package imagepull

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func desiredArtifact(t *testing.T, revision int64) DesiredArtifact {
	t.Helper()
	config := testRuntimeConfig()
	config.Profiles = append([]Profile(nil), config.Profiles...)
	config.Profiles[0].Revision = revision
	desired, err := Desired(config, testEnvironmentID, "tenant-a-dev", testTargetID)
	if err != nil {
		t.Fatal(err)
	}
	return desired
}

func TestMemoryArtifactRotationIsAtomicAndOldSecretIsRetainedInactive(t *testing.T) {
	store := NewMemoryStore()
	now := time.Date(2026, 8, 9, 2, 0, 0, 0, time.UTC)
	first, err := store.EnsureArtifact(t.Context(), desiredArtifact(t, 3), now)
	if err != nil || !first.Active || first.State != StateAwaiting {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	rotated, err := store.EnsureArtifact(t.Context(), desiredArtifact(t, 4), now.Add(time.Minute))
	if err != nil || !rotated.Active || rotated.SecretName == first.SecretName {
		t.Fatalf("rotated=%#v err=%v", rotated, err)
	}
	old, err := store.Artifact(t.Context(), first.ArtifactKey)
	if err != nil || old.Active {
		t.Fatalf("old artifact was deleted or left active: %#v err=%v", old, err)
	}
	if _, err = store.EnsureArtifact(t.Context(), first.DesiredArtifact, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("safe rollback to retained artifact failed: %v", err)
	}
	old, _ = store.Artifact(t.Context(), first.ArtifactKey)
	latest, _ := store.Artifact(t.Context(), rotated.ArtifactKey)
	if !old.Active || latest.Active {
		t.Fatalf("reactivation mismatch old=%#v latest=%#v", old, latest)
	}

	tampered := old.DesiredArtifact
	tampered.PullCredentialRef = "runtime-pull/other"
	if _, err = store.EnsureArtifact(t.Context(), tampered, now.Add(3*time.Minute)); !errors.Is(err, ErrConflict) {
		t.Fatalf("immutable artifact changed: %v", err)
	}
	oldAfter, _ := store.Artifact(t.Context(), old.ArtifactKey)
	if !oldAfter.Active || oldAfter.PullCredentialRef != old.PullCredentialRef {
		t.Fatalf("conflicting ensure partially mutated active state: %#v", oldAfter)
	}
}

func TestMemoryArtifactLeaseFencesRotationHeartbeatAndResult(t *testing.T) {
	store := NewMemoryStore()
	now := time.Date(2026, 8, 9, 3, 0, 0, 0, time.UTC)
	if _, err := store.EnsureArtifact(t.Context(), desiredArtifact(t, 3), now); err != nil {
		t.Fatal(err)
	}
	digest, _ := testRuntimeConfig().Digest()
	lease, found, err := store.ClaimArtifact(t.Context(), "registry-pull-worker:one", RuntimeContract, digest, now, time.Minute)
	if err != nil || !found {
		t.Fatalf("lease=%#v found=%t err=%v", lease, found, err)
	}
	heartbeat, err := store.HeartbeatArtifact(t.Context(), lease, now.Add(20*time.Second), time.Minute)
	if err != nil || !heartbeat.Until.Equal(now.Add(80*time.Second)) {
		t.Fatalf("heartbeat=%#v err=%v", heartbeat, err)
	}
	if _, err = store.EnsureArtifact(t.Context(), desiredArtifact(t, 4), now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = store.RecordArtifactReady(t.Context(), heartbeat, "44444444-4444-4444-8444-444444444444", "123", now.Add(40*time.Second), now.Add(time.Minute)); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("deactivated lease wrote a result: %v", err)
	}
	lease, found, err = store.ClaimArtifact(t.Context(), "registry-pull-worker:two", RuntimeContract, digest, now.Add(30*time.Second), time.Minute)
	if err != nil || !found || lease.Artifact.ProfileRevision != 4 || lease.Epoch != 1 {
		t.Fatalf("rotated lease=%#v found=%t err=%v", lease, found, err)
	}
}

func TestMemoryArtifactReadyRetryAndHealthAreFailClosed(t *testing.T) {
	store := NewMemoryStore()
	now := time.Date(2026, 8, 9, 4, 0, 0, 0, time.UTC)
	if _, err := store.EnsureArtifact(t.Context(), desiredArtifact(t, 3), now); err != nil {
		t.Fatal(err)
	}
	digest, _ := testRuntimeConfig().Digest()
	lease, _, _ := store.ClaimArtifact(t.Context(), "registry-pull-worker:one", RuntimeContract, digest, now, time.Minute)
	retry, err := store.RecordArtifactRetry(t.Context(), lease, "kubernetes-unavailable", false, now.Add(10*time.Second), now.Add(time.Second))
	if err != nil || retry.State != StateAwaiting || retry.ConsecutiveFailures != 1 || retry.LeaseOwner != "" {
		t.Fatalf("retry=%#v err=%v", retry, err)
	}
	healthy, err := store.ActiveArtifactsHealthy(t.Context(), now)
	if err != nil || healthy {
		t.Fatalf("unobserved artifact healthy=%t err=%v", healthy, err)
	}
	lease, found, err := store.ClaimArtifact(t.Context(), "registry-pull-worker:one", RuntimeContract, digest, now.Add(10*time.Second), time.Minute)
	if err != nil || !found {
		t.Fatalf("retry claim found=%t err=%v", found, err)
	}
	ready, err := store.RecordArtifactReady(t.Context(), lease, "44444444-4444-4444-8444-444444444444", "124", now.Add(11*time.Second), now.Add(time.Minute))
	if err != nil || ready.State != StateReady || ready.ConsecutiveFailures != 0 {
		t.Fatalf("ready=%#v err=%v", ready, err)
	}
	healthy, err = store.ActiveArtifactsHealthy(t.Context(), now.Add(10*time.Second))
	if err != nil || !healthy {
		t.Fatalf("fresh artifact healthy=%t err=%v", healthy, err)
	}
	healthy, err = store.ActiveArtifactsHealthy(t.Context(), now.Add(12*time.Second))
	if err != nil || healthy {
		t.Fatalf("stale artifact healthy=%t err=%v", healthy, err)
	}
}

func TestMemoryArtifactClaimIsSingleWinner(t *testing.T) {
	store := NewMemoryStore()
	now := time.Date(2026, 8, 9, 5, 0, 0, 0, time.UTC)
	if _, err := store.EnsureArtifact(t.Context(), desiredArtifact(t, 3), now); err != nil {
		t.Fatal(err)
	}
	digest, _ := testRuntimeConfig().Digest()
	var group sync.WaitGroup
	winners := make(chan Lease, 16)
	for index := 0; index < 16; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			owner := "registry-pull-worker:" + string(rune('a'+index)) + "-owner"
			lease, found, err := store.ClaimArtifact(context.Background(), owner, RuntimeContract, digest, now, time.Minute)
			if err == nil && found {
				winners <- lease
			}
		}(index)
	}
	group.Wait()
	close(winners)
	if len(winners) != 1 {
		t.Fatalf("claim winners=%d", len(winners))
	}
}

func TestMemoryReadinessIsExactFreshAndEpochFenced(t *testing.T) {
	store := NewMemoryStore()
	now := time.Date(2026, 8, 9, 6, 0, 0, 0, time.UTC)
	digest, _ := testRuntimeConfig().Digest()
	readiness := Readiness{WorkerID: "registry-pull-worker:one", WorkerEpoch: 1, Contract: RuntimeContract,
		ConfigDigest: digest, ProfileCount: 1, StartedAt: now, ObservedAt: now, LeaseUntil: now.Add(time.Minute)}
	if err := store.RecordReadiness(t.Context(), readiness); err != nil {
		t.Fatal(err)
	}
	if err := store.RuntimeReady(t.Context(), RuntimeContract, digest, 1, now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.RuntimeReady(t.Context(), RuntimeContract, "sha256:"+stringsOf('a', 64), 1, now.Add(30*time.Second)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("mismatched digest readiness=%v", err)
	}
	if err := store.RuntimeReady(t.Context(), RuntimeContract, digest, 1, now.Add(time.Minute)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expired readiness=%v", err)
	}
	advanced := readiness
	advanced.ObservedAt = now.Add(10 * time.Second)
	advanced.LeaseUntil = now.Add(70 * time.Second)
	if err := store.RecordReadiness(t.Context(), advanced); err != nil {
		t.Fatal(err)
	}
	regressed := advanced
	regressed.ObservedAt = now.Add(5 * time.Second)
	if err := store.RecordReadiness(t.Context(), regressed); !errors.Is(err, ErrConflict) {
		t.Fatalf("regressed readiness accepted: %v", err)
	}
	skipped := readiness
	skipped.WorkerEpoch = 3
	if err := store.RecordReadiness(t.Context(), skipped); !errors.Is(err, ErrConflict) {
		t.Fatalf("skipped epoch accepted: %v", err)
	}
}

func stringsOf(value byte, count int) string {
	result := make([]byte, count)
	for index := range result {
		result[index] = value
	}
	return string(result)
}
