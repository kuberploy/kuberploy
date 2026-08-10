package edge

import (
	"context"
	"errors"
	"testing"
	"time"
)

const testWorkerID = "edge-worker-test-0001"

func TestMemoryStoreLeaseIdentityAndStaleReadiness(t *testing.T) {
	ctx := context.Background()
	config := testRuntimeConfig()
	config.Profiles.CertManager = nil
	config.Profiles.ExternalDNS = nil
	digest, _ := config.Digest()
	targets, _ := config.DesiredTargets()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	if err := store.SynchronizeTargets(ctx, digest, targets, now); err != nil {
		t.Fatal(err)
	}
	lease, found, err := store.ClaimTarget(ctx, testWorkerID, RuntimeContract, digest, now, config.WorkLease)
	if err != nil || !found {
		t.Fatalf("claim failed: %#v %v", lease, err)
	}
	// A concurrent worker bootstrapping the same exact config must not clear or
	// steal the live lease.
	if err = store.SynchronizeTargets(ctx, digest, targets, now); err != nil {
		t.Fatal(err)
	}
	current, err := store.Target(ctx, lease.Target.Key, lease.Target.Revision)
	if err != nil || current.LeaseOwner != testWorkerID || current.LeaseEpoch != lease.Epoch {
		t.Fatalf("idempotent synchronization cleared a lease: %#v %v", current, err)
	}
	updatedLease, err := store.HeartbeatTarget(ctx, lease, now.Add(time.Second), config.WorkLease)
	if err != nil {
		t.Fatalf("live lease lost: %v", err)
	}
	if _, err = store.HeartbeatTarget(ctx, lease, now.Add(2*time.Second), config.WorkLease); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("superseded lease was accepted: %v", err)
	}
	if _, err = store.RecordTargetRetry(ctx, updatedLease, "test-requeue", false, now.Add(2*time.Second), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	// Finish all targets with exact receipts.
	for {
		lease, found, err = store.ClaimTarget(ctx, testWorkerID, RuntimeContract, digest, now.Add(2*time.Second), config.WorkLease)
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			break
		}
		receipt := ObservationReceipt{TargetKey: lease.Target.Key, DesiredDigest: lease.Target.DesiredDigest,
			IdentityDigest: testDigest("identity/" + lease.Target.Key), ResourceVersionDigest: testDigest("version/" + lease.Target.Key)}
		if _, err = store.RecordTargetReady(ctx, lease, receipt, now.Add(3*time.Second), now.Add(config.PollInterval)); err != nil {
			t.Fatal(err)
		}
	}
	readiness := Readiness{WorkerID: testWorkerID, WorkerEpoch: 1, Contract: RuntimeContract, ConfigDigest: digest,
		TargetCount: len(targets), StartedAt: now, ObservedAt: now.Add(3 * time.Second), LeaseUntil: now.Add(config.ReadinessMaxAge)}
	if err = store.RecordReadiness(ctx, readiness); err != nil {
		t.Fatal(err)
	}
	if err = store.RuntimeReady(ctx, RuntimeContract, digest, len(targets), now.Add(4*time.Second), config.ReadinessMaxAge); err != nil {
		t.Fatalf("fresh exact runtime not ready: %v", err)
	}
	if err = store.RuntimeReady(ctx, RuntimeContract, digest, len(targets), now.Add(config.ReadinessMaxAge+time.Second), config.ReadinessMaxAge); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("stale readiness accepted: %v", err)
	}
}

func TestMemoryStoreConfigChangeRequiresFreshObservationAndPinsUIDDigest(t *testing.T) {
	ctx := context.Background()
	config := testRuntimeConfig()
	config.Profiles.CertManager = nil
	config.Profiles.ExternalDNS = nil
	digest, _ := config.Digest()
	targets, _ := config.DesiredTargets()
	now := time.Date(2026, 8, 9, 13, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	if err := store.SynchronizeTargets(ctx, digest, targets, now); err != nil {
		t.Fatal(err)
	}
	lease, found, err := store.ClaimTarget(ctx, testWorkerID, RuntimeContract, digest, now, config.WorkLease)
	if err != nil || !found {
		t.Fatal("target was not claimable")
	}
	receipt := ObservationReceipt{TargetKey: lease.Target.Key, DesiredDigest: lease.Target.DesiredDigest,
		IdentityDigest: testDigest("stable-uid-spec"), ResourceVersionDigest: testDigest("rv-1")}
	if _, err = store.RecordTargetReady(ctx, lease, receipt, now.Add(time.Second), now.Add(config.PollInterval)); err != nil {
		t.Fatal(err)
	}
	lease, found, err = store.ClaimTarget(ctx, testWorkerID, RuntimeContract, digest, now.Add(config.PollInterval), config.WorkLease)
	if err != nil || !found {
		t.Fatal("ready target was not scheduled for re-observation")
	}
	changed := receipt
	changed.ResourceVersionDigest = testDigest("rv-2")
	changed.IdentityDigest = testDigest("replacement-uid")
	if _, err = store.RecordTargetReady(ctx, lease, changed, now.Add(config.PollInterval+time.Second), now.Add(2*config.PollInterval)); !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("replacement UID/spec identity accepted: %v", err)
	}
	if _, err = store.RecordTargetRetry(ctx, lease, "resource-identity-changed", true, now.Add(2*config.PollInterval), now.Add(config.PollInterval+time.Second)); err != nil {
		t.Fatalf("identity mismatch did not retain its fenced lease for terminal recording: %v", err)
	}

	changedConfig := config
	changedConfig.MinimumBackoff = 6 * time.Second
	changedDigest, _ := changedConfig.Digest()
	changedTargets, _ := changedConfig.DesiredTargets()
	if err = store.SynchronizeTargets(ctx, changedDigest, changedTargets, now.Add(config.PollInterval+2*time.Second)); err != nil {
		t.Fatal(err)
	}
	current, err := store.Target(ctx, lease.Target.Key, lease.Target.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != StateAwaiting || current.RuntimeConfigDigest != changedDigest || current.LeaseOwner != "" || current.ObservedIdentityDigest != receipt.IdentityDigest {
		t.Fatalf("config fence did not require a fresh observation while preserving pinned identity: %#v", current)
	}
}

func TestMemoryReadinessEpochFence(t *testing.T) {
	store := NewMemoryStore()
	config := testRuntimeConfig()
	digest, _ := config.Digest()
	now := time.Date(2026, 8, 9, 14, 0, 0, 0, time.UTC)
	base := Readiness{WorkerID: testWorkerID, WorkerEpoch: 2, Contract: RuntimeContract, ConfigDigest: digest,
		TargetCount: config.TargetCount(), StartedAt: now, ObservedAt: now, LeaseUntil: now.Add(time.Minute)}
	if err := store.RecordReadiness(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	stale := base
	stale.WorkerEpoch = 1
	if err := store.RecordReadiness(context.Background(), stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale worker epoch accepted: %v", err)
	}
	jump := base
	jump.WorkerEpoch = 4
	if err := store.RecordReadiness(context.Background(), jump); !errors.Is(err, ErrConflict) {
		t.Fatalf("epoch jump accepted: %v", err)
	}
}
