package gitprojection

import (
	"testing"
	"time"
)

func TestGitHubPushWakeMatchesExactAppInstallationRepositoryAndRef(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	store := NewMemoryGitHubPushWakeStore()
	exact := wakeBinding(t, "11111111-1111-4111-8111-111111111111", 10, 20, "refs/heads/main", now)
	otherInstallation := wakeBinding(t, "22222222-2222-4222-8222-222222222222", 11, 20, "refs/heads/main", now)
	otherRepository := wakeBinding(t, "33333333-3333-4333-8333-333333333333", 10, 21, "refs/heads/main", now)
	otherRef := wakeBinding(t, "44444444-4444-4444-8444-444444444444", 10, 20, "refs/heads/release", now)
	otherApp := wakeBinding(t, "55555555-5555-4555-8555-555555555555", 10, 20, "refs/heads/main", now)
	for _, candidate := range []struct {
		binding Binding
		appID   int64
	}{{exact, 99}, {otherInstallation, 99}, {otherRepository, 99}, {otherRef, 99}, {otherApp, 100}} {
		if err := store.PutGitHubBinding(candidate.binding, candidate.appID, true); err != nil {
			t.Fatal(err)
		}
	}
	wake := GitHubPushWake{GitHubAppID: 99, InstallationID: 10, RepositoryID: 20, TargetRef: "refs/heads/main",
		AfterCommit: repeatWake("a", 40), DeliveryHash: "sha256:" + repeatWake("b", 64), ReceivedAt: now}
	result, err := (GitHubPushWaker{Store: store}).Wake(t.Context(), wake)
	if err != nil || result.Replay || len(result.Bindings) != 1 || result.Bindings[0].BindingID != exact.ID || result.Bindings[0].WakeGeneration != 1 {
		t.Fatalf("unsafe wake result=%#v err=%v", result, err)
	}
	for _, untouched := range []Binding{otherInstallation, otherRepository, otherRef, otherApp} {
		if _, err = store.Snapshot(untouched.ID); err != ErrNotFound {
			t.Fatalf("cross-binding wake for %s: %v", untouched.ID, err)
		}
	}
	replay, err := (GitHubPushWaker{Store: store}).Wake(t.Context(), wake)
	if err != nil || !replay.Replay || len(replay.Bindings) != 0 {
		t.Fatalf("delivery replay was not collapsed: %#v err=%v", replay, err)
	}
}

func TestGitHubPushAfterSHAIsWakeOnlyAndConcurrentWakeCannotBeLost(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	store := NewMemoryGitHubPushWakeStore()
	binding := wakeBinding(t, "11111111-1111-4111-8111-111111111111", 10, 20, "refs/heads/main", now)
	originalHead := binding.TargetHeadRevision
	if err := store.PutGitHubBinding(binding, 99, true); err != nil {
		t.Fatal(err)
	}
	first := GitHubPushWake{GitHubAppID: 99, InstallationID: 10, RepositoryID: 20, TargetRef: binding.TargetRef,
		AfterCommit: repeatWake("b", 40), DeliveryHash: "sha256:" + repeatWake("c", 64), ReceivedAt: now.Add(time.Second)}
	if _, err := store.WakeGitHubPush(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.Snapshot(binding.ID)
	if err != nil || claimed.WakeGeneration != 1 {
		t.Fatalf("claim snapshot=%#v err=%v", claimed, err)
	}
	second := first
	second.AfterCommit = repeatWake("d", 40)
	second.DeliveryHash = "sha256:" + repeatWake("e", 64)
	second.ReceivedAt = now.Add(2 * time.Second)
	if _, err = store.WakeGitHubPush(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	if err = store.FinishWakeSnapshot(binding.ID, claimed.WakeGeneration, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	current, err := store.Snapshot(binding.ID)
	if err != nil || current.WakeGeneration != 2 || current.ReconciledGeneration != 1 || !current.NextPollAt.Equal(first.ReceivedAt) {
		t.Fatalf("newer wake was lost: %#v err=%v", current, err)
	}
	stored := store.bindings[binding.ID].Binding
	if stored.TargetHeadRevision != originalHead || stored.TargetHeadRevision == first.AfterCommit || stored.TargetHeadRevision == second.AfterCommit {
		t.Fatalf("webhook SHA became authority: %#v", stored)
	}
}

func wakeBinding(t *testing.T, id string, installation, repository int64, ref string, now time.Time) Binding {
	t.Helper()
	binding, err := NewGitHubEnvironmentBinding(id, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		RepositoryIdentity{Provider: "github", InstallationID: installation, RepositoryID: repository, Owner: "kuberploy", Name: "desired-state"}, ref, now)
	if err != nil {
		t.Fatal(err)
	}
	binding.TargetHeadRevision = repeatWake("a", 40)
	binding.TargetHeadObservedAt = now
	binding.State = BindingIndexing
	return binding
}

func repeatWake(value string, count int) string {
	result := ""
	for range count {
		result += value
	}
	return result
}
