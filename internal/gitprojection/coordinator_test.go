package gitprojection

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/githubapp"
)

type projectionClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *projectionClock) Next() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(time.Second)
	return c.now
}

func (c *projectionClock) Set(value time.Time) {
	c.mu.Lock()
	c.now = value
	c.mu.Unlock()
}

type headVerifierStub struct {
	verify func(context.Context, Binding, ObservationSource) (VerifiedHead, error)
	calls  int
}

func (v *headVerifierStub) VerifyTargetHead(ctx context.Context, binding Binding, source ObservationSource) (VerifiedHead, error) {
	v.calls++
	return v.verify(ctx, binding, source)
}

type projectionProjectorStub struct {
	store    Store
	err      error
	complete bool
	calls    int
}

type blockingProjectionProjector struct{}

func (blockingProjectionProjector) Project(ctx context.Context, _ ReconciliationLease, _ Binding, _ VerifiedHead, _ time.Time) error {
	<-ctx.Done()
	return ctx.Err()
}

type failingHeartbeatStore struct{ Store }

func (s failingHeartbeatStore) HeartbeatReconciliation(context.Context, ReconciliationLease, time.Time, time.Duration) (ReconciliationLease, error) {
	return ReconciliationLease{}, ErrLeaseLost
}

func (p *projectionProjectorStub) Project(ctx context.Context, lease ReconciliationLease, binding Binding, head VerifiedHead, now time.Time) error {
	p.calls++
	if p.err != nil {
		return p.err
	}
	if !p.complete {
		return nil
	}
	generation, err := p.store.BeginGeneration(ctx, lease, head.Commit, binding.ParserVersion, now)
	if err != nil {
		return err
	}
	if err = p.store.PutDocuments(ctx, generation, nil); err != nil {
		return err
	}
	_, err = p.store.ActivateGeneration(ctx, lease, generation, SchemaOnlyAppConfigPolicyValidator{}, now)
	return err
}

func TestCoordinatorProjectsExactVerifiedHeadAndSchedulesSafetyPoll(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	clock := &projectionClock{now: base}
	store := NewMemoryStore()
	binding := coordinatorBinding(t, base)
	if err := store.PutBinding(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("a", 40)
	provider := &headVerifierStub{verify: func(_ context.Context, candidate Binding, source ObservationSource) (VerifiedHead, error) {
		if candidate.ID != binding.ID || candidate.Repository != binding.Repository || candidate.TargetRef != binding.TargetRef || source != ObservationPoll {
			t.Fatalf("binding=%#v source=%s", candidate, source)
		}
		return VerifiedHead{BindingID: candidate.ID, Repository: candidate.Repository, TargetRef: candidate.TargetRef, Commit: commit, Source: source, ProviderRequest: "poll-request-1", ObservedAt: clock.Next()}, nil
	}}
	projector := &projectionProjectorStub{store: store, complete: true}
	coordinator := coordinatorForTest(store, provider, projector, clock)
	worked, err := coordinator.ReconcileNext(t.Context())
	if err != nil || !worked {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	current, err := store.Binding(t.Context(), binding.ID)
	if err != nil || current.State != BindingReady || current.TargetHeadRevision != commit || current.IndexedRevision != commit || current.ProjectionGeneration != 1 {
		t.Fatalf("binding=%#v err=%v", current, err)
	}
	cursor, err := store.PollCursor(t.Context(), binding.ID)
	if err != nil || cursor.LastCommit != commit || cursor.ConsecutiveFail != 0 || !cursor.NextPollAt.After(cursor.UpdatedAt) {
		t.Fatalf("cursor=%#v err=%v", cursor, err)
	}
	if provider.calls != 1 || projector.calls != 1 {
		t.Fatalf("provider=%d projector=%d", provider.calls, projector.calls)
	}
	if worked, err = coordinator.ReconcileNext(t.Context()); err != nil || worked {
		t.Fatalf("not-due worked=%v err=%v", worked, err)
	}
	changedAt := cursor.UpdatedAt.Add(time.Second)
	if err = store.SetBindingState(t.Context(), binding.ID, commit, BindingWaiting, changedAt); err != nil {
		t.Fatal(err)
	}
	clock.Set(changedAt)
	if worked, err = coordinator.ReconcileNext(t.Context()); err != nil || !worked {
		t.Fatalf("binding-change worked=%v err=%v", worked, err)
	}
	current, _ = store.Binding(t.Context(), binding.ID)
	if current.State != BindingReady || current.ProjectionGeneration != 2 || projector.calls != 2 {
		t.Fatalf("changed binding was not reindexed: binding=%#v calls=%d", current, projector.calls)
	}
}

func TestCoordinatorDoesNotAdvertiseIncompleteProjection(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	clock := &projectionClock{now: base}
	store := NewMemoryStore()
	binding := coordinatorBinding(t, base)
	if err := store.PutBinding(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("b", 40)
	provider := &headVerifierStub{verify: func(_ context.Context, candidate Binding, source ObservationSource) (VerifiedHead, error) {
		return VerifiedHead{BindingID: candidate.ID, Repository: candidate.Repository, TargetRef: candidate.TargetRef, Commit: commit, Source: source, ProviderRequest: "poll-request-2", ObservedAt: clock.Next()}, nil
	}}
	projector := &projectionProjectorStub{store: store, complete: false}
	coordinator := coordinatorForTest(store, provider, projector, clock)
	reported := []error{}
	coordinator.ReportError = func(err error) { reported = append(reported, err) }
	worked, err := coordinator.ReconcileNext(t.Context())
	if err != nil || !worked {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	cursor, err := store.PollCursor(t.Context(), binding.ID)
	if err != nil || cursor.LastCommit != commit || cursor.ConsecutiveFail != 1 {
		t.Fatalf("cursor=%#v err=%v", cursor, err)
	}
	current, _ := store.Binding(t.Context(), binding.ID)
	if current.State == BindingReady || current.IndexedRevision != "" || current.ProjectionGeneration != 0 {
		t.Fatalf("incomplete projection advertised: %#v", current)
	}
	if len(reported) != 1 || !errors.Is(reported[0], ErrConflict) {
		t.Fatalf("durably handled projection failure was not reported: %#v", reported)
	}
}

func TestCoordinatorFailureBackoffRecoversAndMissingRefRetainsProjection(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	clock := &projectionClock{now: base}
	store := NewMemoryStore()
	binding := coordinatorBinding(t, base)
	if err := store.PutBinding(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	providerErr := errors.New("provider unavailable")
	provider := &headVerifierStub{verify: func(context.Context, Binding, ObservationSource) (VerifiedHead, error) {
		return VerifiedHead{}, providerErr
	}}
	projector := &projectionProjectorStub{store: store, complete: true}
	coordinator := coordinatorForTest(store, provider, projector, clock)
	if worked, err := coordinator.ReconcileNext(t.Context()); err != nil || !worked {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	cursor, err := store.PollCursor(t.Context(), binding.ID)
	if err != nil || cursor.ConsecutiveFail != 1 || cursor.LastCommit != "" || cursor.NextPollAt.Sub(cursor.UpdatedAt) != 5*time.Second {
		t.Fatalf("cursor=%#v err=%v", cursor, err)
	}

	clock.Set(cursor.NextPollAt)
	commit := strings.Repeat("c", 40)
	provider.verify = func(_ context.Context, candidate Binding, source ObservationSource) (VerifiedHead, error) {
		return VerifiedHead{BindingID: candidate.ID, Repository: candidate.Repository, TargetRef: candidate.TargetRef, Commit: commit, Source: source, ProviderRequest: "poll-recovery", ObservedAt: clock.Next()}, nil
	}
	if worked, err := coordinator.ReconcileNext(t.Context()); err != nil || !worked {
		t.Fatalf("recovery worked=%v err=%v", worked, err)
	}
	cursor, _ = store.PollCursor(t.Context(), binding.ID)
	if cursor.ConsecutiveFail != 0 || cursor.LastCommit != commit || cursor.NextPollAt.Sub(cursor.UpdatedAt) != time.Minute {
		t.Fatalf("recovery cursor=%#v", cursor)
	}

	clock.Set(cursor.NextPollAt)
	provider.verify = func(context.Context, Binding, ObservationSource) (VerifiedHead, error) {
		return VerifiedHead{}, ErrMissingRef
	}
	if worked, err := coordinator.ReconcileNext(t.Context()); err != nil || !worked {
		t.Fatalf("missing-ref worked=%v err=%v", worked, err)
	}
	current, _ := store.Binding(t.Context(), binding.ID)
	if current.State != BindingMissingRef || current.IndexedRevision != commit || current.ProjectionGeneration != 1 {
		t.Fatalf("missing ref destroyed last projection: %#v", current)
	}
	cursor, _ = store.PollCursor(t.Context(), binding.ID)
	if cursor.ConsecutiveFail != 1 || cursor.LastCommit != commit {
		t.Fatalf("missing ref cursor=%#v", cursor)
	}
}

func TestCoordinatorHonorsBoundedGitHubRetryTime(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	clock := &projectionClock{now: base}
	store := NewMemoryStore()
	binding := coordinatorBinding(t, base)
	if err := store.PutBinding(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	retryAt := base.Add(30 * time.Minute)
	provider := &headVerifierStub{verify: func(context.Context, Binding, ObservationSource) (VerifiedHead, error) {
		return VerifiedHead{}, &githubapp.APIError{StatusCode: 429, Class: githubapp.APIErrorRateLimit, RetryAt: retryAt}
	}}
	coordinator := coordinatorForTest(store, provider, &projectionProjectorStub{store: store, complete: true}, clock)
	if worked, err := coordinator.ReconcileNext(t.Context()); err != nil || !worked {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	cursor, err := store.PollCursor(t.Context(), binding.ID)
	if err != nil || !cursor.NextPollAt.Equal(retryAt) || cursor.ConsecutiveFail != 1 {
		t.Fatalf("cursor=%#v err=%v", cursor, err)
	}
}

func TestCoordinatorCancellationPromptlyReleasesLease(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	clock := &projectionClock{now: base}
	store := NewMemoryStore()
	binding := coordinatorBinding(t, base)
	if err := store.PutBinding(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	provider := &headVerifierStub{verify: func(callContext context.Context, _ Binding, _ ObservationSource) (VerifiedHead, error) {
		cancel()
		<-callContext.Done()
		return VerifiedHead{}, callContext.Err()
	}}
	coordinator := coordinatorForTest(store, provider, &projectionProjectorStub{store: store, complete: true}, clock)
	worked, err := coordinator.ReconcileNext(ctx)
	if !worked || !errors.Is(err, context.Canceled) {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	reclaimed, err := store.ClaimReconciliation(t.Context(), "projection-after-cancel", clock.Next(), time.Minute)
	if err != nil || reclaimed.Reclaimed || reclaimed.Lease.Epoch != 2 {
		t.Fatalf("reclaimed=%#v err=%v", reclaimed, err)
	}
}

func TestCoordinatorHeartbeatLossCancelsWorkAndNeverFinalizes(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	clock := &projectionClock{now: base}
	memory := NewMemoryStore()
	binding := coordinatorBinding(t, base)
	if err := memory.PutBinding(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("e", 40)
	provider := &headVerifierStub{verify: func(_ context.Context, candidate Binding, source ObservationSource) (VerifiedHead, error) {
		return VerifiedHead{BindingID: candidate.ID, Repository: candidate.Repository, TargetRef: candidate.TargetRef, Commit: commit, Source: source, ProviderRequest: "heartbeat-provider", ObservedAt: clock.Next()}, nil
	}}
	store := failingHeartbeatStore{Store: memory}
	coordinator := &Coordinator{
		Store: store, Provider: provider, Projector: blockingProjectionProjector{}, Owner: "projection-heartbeat-test", Now: clock.Next,
		LeaseDuration: 15 * time.Second, HeartbeatInterval: time.Second, WorkTimeout: 15 * time.Second,
		PollInterval: time.Minute, MinimumBackoff: time.Second, MaximumBackoff: time.Minute,
		IdleDelay: 10 * time.Millisecond, JitterFraction: 0.2, Random: func() float64 { return 0.5 },
	}
	worked, err := coordinator.ReconcileNext(t.Context())
	if !worked || !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	current, _ := memory.Binding(t.Context(), binding.ID)
	if current.State == BindingReady || current.IndexedRevision != "" || current.ProjectionGeneration != 0 {
		t.Fatalf("heartbeat-lost work finalized: %#v", current)
	}
	clock.Set(base.Add(16 * time.Second))
	reclaimed, err := memory.ClaimReconciliation(t.Context(), "projection-recovery-owner", clock.Next(), time.Minute)
	if err != nil || !reclaimed.Reclaimed || reclaimed.Lease.Epoch != 2 {
		t.Fatalf("reclaimed=%#v err=%v", reclaimed, err)
	}
}

func TestReconciliationLeaseFencesSameOwnerAcrossExpiryAndRecoversStaging(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	store := NewMemoryStore()
	binding := coordinatorBinding(t, base)
	if err := store.PutBinding(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	first, err := store.ClaimReconciliation(t.Context(), "projection-same-owner", base.Add(time.Second), 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.ClaimReconciliation(t.Context(), "projection-other-owner", base.Add(2*time.Second), 15*time.Second); !errors.Is(err, ErrNotFound) {
		t.Fatalf("active work was claimed twice: %v", err)
	}
	head := VerifiedHead{BindingID: binding.ID, Repository: binding.Repository, TargetRef: binding.TargetRef, Commit: strings.Repeat("d", 40), Source: ObservationPoll, ProviderRequest: "lease-provider-1", ObservedAt: base.Add(3 * time.Second)}
	binding, _, err = store.RecordVerifiedHead(t.Context(), head)
	if err != nil {
		t.Fatal(err)
	}
	staging, err := store.BeginGeneration(t.Context(), first.Lease, head.Commit, binding.ParserVersion, base.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}

	reclaimedAt := first.Lease.Until
	second, err := store.ClaimReconciliation(t.Context(), first.Lease.Owner, reclaimedAt, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Reclaimed || second.Lease.Epoch != first.Lease.Epoch+1 || second.Lease.Owner != first.Lease.Owner {
		t.Fatalf("second=%#v", second)
	}
	outcome := ReconciliationOutcome{ConsecutiveFailure: 1, NextPollAt: reclaimedAt.Add(time.Minute), FailureCode: "stale-worker"}
	if _, err = store.HeartbeatReconciliation(t.Context(), first.Lease, reclaimedAt, 15*time.Second); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale heartbeat=%v", err)
	}
	if err = store.FinishReconciliation(t.Context(), first.Lease, outcome, reclaimedAt); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale finish=%v", err)
	}
	if _, err = store.ActivateGeneration(t.Context(), first.Lease, staging, SchemaOnlyAppConfigPolicyValidator{}, reclaimedAt); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale activate=%v", err)
	}
	if err = store.FailGeneration(t.Context(), first.Lease, staging, reclaimedAt); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale failure=%v", err)
	}
	extended, err := store.HeartbeatReconciliation(t.Context(), second.Lease, reclaimedAt.Add(time.Second), 15*time.Second)
	if err != nil || !extended.Until.After(second.Lease.Until) || extended.Epoch != second.Lease.Epoch {
		t.Fatalf("extended=%#v err=%v", extended, err)
	}
	second.Lease = extended
	newGeneration, err := store.BeginGeneration(t.Context(), second.Lease, head.Commit, binding.ParserVersion, reclaimedAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if newGeneration.Number <= staging.Number {
		t.Fatalf("staging generation was not recovered: old=%d new=%d", staging.Number, newGeneration.Number)
	}
	if err = store.PutDocuments(t.Context(), newGeneration, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ActivateGeneration(t.Context(), second.Lease, newGeneration, SchemaOnlyAppConfigPolicyValidator{}, reclaimedAt.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = store.FinishReconciliation(t.Context(), second.Lease, ReconciliationOutcome{LastCommit: head.Commit, NextPollAt: reclaimedAt.Add(time.Minute)}, reclaimedAt.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestReconciliationClaimIsExclusiveUnderConcurrency(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	store := NewMemoryStore()
	if err := store.PutBinding(t.Context(), coordinatorBinding(t, base)); err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := store.ClaimReconciliation(context.Background(), "projection-racing-owner", base.Add(time.Second), time.Minute); err == nil {
				successes.Add(1)
			} else if !errors.Is(err, ErrNotFound) {
				t.Errorf("claim error=%v", err)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("successful claims=%d", successes.Load())
	}
}

func coordinatorForTest(store Store, provider HeadVerifier, projector ProjectionProjector, clock *projectionClock) *Coordinator {
	return &Coordinator{
		Store: store, Provider: provider, Projector: projector, Owner: "projection-worker-test", Now: clock.Next,
		LeaseDuration: time.Minute, HeartbeatInterval: 10 * time.Second, WorkTimeout: time.Minute,
		PollInterval: time.Minute, MinimumBackoff: 5 * time.Second, MaximumBackoff: time.Minute,
		IdleDelay: 10 * time.Millisecond, JitterFraction: 0.2, Random: func() float64 { return 0.5 },
	}
}

func coordinatorBinding(t *testing.T, now time.Time) Binding {
	t.Helper()
	binding, err := NewGitHubEnvironmentBinding(
		"11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333",
		RepositoryIdentity{Provider: "github", InstallationID: 77, RepositoryID: 88, Owner: "kuberploy", Name: "environment"},
		"refs/heads/main", now,
	)
	if err != nil {
		t.Fatal(err)
	}
	return binding
}
