package builds

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/builder"
)

func TestControllerCreatesExactWorkloadPromotesCacheAndStoresSafeResult(t *testing.T) {
	for _, mode := range []RegistryMode{RegistryManaged, RegistryExternal} {
		t.Run(string(mode), func(t *testing.T) {
			store, _ := seedMemory(t, mode)
			clock := testNow
			attempt := createAttempt(t, store, mode, &clock)
			provider := &fakeProvider{resolvedCommit: attempt.CommitSHA, now: clock}
			kube := &fakeKubernetes{state: WorkloadSucceeded, promoted: true}
			controller := &BuildController{Store: store, Provider: provider, Kubernetes: kube, Owner: "build-controller", LeaseDuration: time.Minute, Now: func() time.Time { return clock }}
			result, err := controller.ReconcileNext(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if result.AttemptID != attempt.ID || result.State != AttemptSucceeded {
				t.Fatalf("result=%#v", result)
			}
			stored, err := store.Attempt(context.Background(), attempt.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Result == nil || stored.CacheReference != cacheReference(stored) || stored.LogReference == "" {
				t.Fatalf("stored=%#v", stored)
			}
			if len(kube.workloads) != 1 || !builder.CanAdoptJob(kube.workloads[0].Plan.Job, attempt.PlanRequest) || !builder.CanAdoptNetworkPolicy(kube.workloads[0].Plan.NetworkPolicy, attempt.PlanRequest) {
				t.Fatal("controller changed exact plan")
			}
			encoded := string(mustJSON(t, stored))
			for _, forbidden := range []string{"authorization", "ghs_", "password-value", "private-key"} {
				if strings.Contains(strings.ToLower(encoded), forbidden) {
					t.Fatalf("secret-like data persisted: %s", forbidden)
				}
			}
		})
	}
}

func TestControllerRetriesSameImmutableAttemptAndAdopts(t *testing.T) {
	store, _ := seedMemory(t, RegistryManaged)
	clock := testNow
	attempt := createAttempt(t, store, RegistryManaged, &clock)
	provider := &fakeProvider{resolvedCommit: attempt.CommitSHA, now: clock}
	kube := &fakeKubernetes{state: WorkloadSucceeded, promoted: true, errorCount: 1}
	controller := &BuildController{Store: store, Provider: provider, Kubernetes: kube, Owner: "build-controller", LeaseDuration: time.Minute, Now: func() time.Time { return clock }}
	first, err := controller.ReconcileNext(context.Background())
	if !errors.Is(err, ErrInfrastructure) || first.State != AttemptQueued || first.RetryAt.IsZero() {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	clock = first.RetryAt.Add(time.Second)
	provider.now = clock
	second, err := controller.ReconcileNext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.State != AttemptSucceeded || second.AttemptID != attempt.ID {
		t.Fatalf("second=%#v", second)
	}
	stored, _ := store.Attempt(context.Background(), attempt.ID)
	if stored.Generation != attempt.Generation || stored.ExecutionAttempts != 2 || stored.PlanRequest.Build.Destination != attempt.PlanRequest.Build.Destination || stored.CacheCandidate != attempt.CacheCandidate {
		t.Fatalf("retry mutated identity: %#v", stored)
	}
}

func TestControllerRejectsAdoptionMismatchPermanently(t *testing.T) {
	store, _ := seedMemory(t, RegistryManaged)
	clock := testNow
	attempt := createAttempt(t, store, RegistryManaged, &clock)
	controller := &BuildController{Store: store, Provider: &fakeProvider{resolvedCommit: attempt.CommitSHA, now: clock}, Kubernetes: &fakeKubernetes{state: WorkloadRunning, mismatch: true}, Owner: "build-controller", LeaseDuration: time.Minute, Now: func() time.Time { return clock }}
	if _, err := controller.ReconcileNext(context.Background()); !errors.Is(err, ErrInfrastructure) {
		t.Fatalf("mismatch accepted: %v", err)
	}
	stored, _ := store.Attempt(context.Background(), attempt.ID)
	if stored.State != AttemptFailed || stored.FailureCode != "kubernetes-adoption-mismatch" {
		t.Fatalf("stored=%#v", stored)
	}
}

func TestCancellationIsIdempotentBeforeAndDuringExecution(t *testing.T) {
	store, _ := seedMemory(t, RegistryManaged)
	clock := testNow
	attempt := createAttempt(t, store, RegistryManaged, &clock)
	cancelled, err := store.RequestCancel(context.Background(), attempt.ID, clock)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.State != AttemptCancelled {
		t.Fatalf("queued cancellation=%#v", cancelled)
	}
	again, err := store.RequestCancel(context.Background(), attempt.ID, clock.Add(time.Second))
	if err != nil || again.State != AttemptCancelled {
		t.Fatalf("repeat=%#v err=%v", again, err)
	}

	store2, _ := seedMemory(t, RegistryManaged)
	attempt2 := createAttempt(t, store2, RegistryManaged, &clock)
	kube := &fakeKubernetes{state: WorkloadRunning}
	controller := &BuildController{Store: store2, Provider: &fakeProvider{resolvedCommit: attempt2.CommitSHA, now: clock}, Kubernetes: kube, Owner: "build-controller", LeaseDuration: time.Minute, Now: func() time.Time { return clock }}
	if result, runErr := controller.ReconcileNext(context.Background()); runErr != nil || result.State != AttemptRunning {
		t.Fatalf("run=%#v err=%v", result, runErr)
	}
	if _, err = store2.RequestCancel(context.Background(), attempt2.ID, clock.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Minute)
	if result, cancelErr := controller.ReconcileNext(context.Background()); cancelErr != nil || result.State != AttemptCancelled {
		t.Fatalf("cancel=%#v err=%v", result, cancelErr)
	}
	if len(kube.cancelled) != 1 || kube.cancelled[0] != attempt2.ID {
		t.Fatalf("cancel calls=%v", kube.cancelled)
	}
}

func TestCancellationFailureRetriesAsCancellationWithBackoff(t *testing.T) {
	store, _ := seedMemory(t, RegistryManaged)
	clock := testNow
	attempt := createAttempt(t, store, RegistryManaged, &clock)
	kube := &fakeKubernetes{state: WorkloadRunning, cancelErrors: 1}
	controller := &BuildController{Store: store, Provider: &fakeProvider{resolvedCommit: attempt.CommitSHA, now: clock}, Kubernetes: kube, Owner: "build-controller", LeaseDuration: time.Minute, Now: func() time.Time { return clock }}
	if result, err := controller.ReconcileNext(context.Background()); err != nil || result.State != AttemptRunning {
		t.Fatalf("run=%#v err=%v", result, err)
	}
	if _, err := store.RequestCancel(context.Background(), attempt.ID, clock.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Minute)
	first, err := controller.ReconcileNext(context.Background())
	if !errors.Is(err, ErrInfrastructure) || first.State != AttemptCancelling || first.RetryAt.IsZero() {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	clock = first.RetryAt.Add(time.Second)
	second, err := controller.ReconcileNext(context.Background())
	if err != nil || second.State != AttemptCancelled {
		t.Fatalf("second=%#v err=%v", second, err)
	}
	stored, err := store.Attempt(context.Background(), attempt.ID)
	if err != nil || stored.State != AttemptCancelled || stored.FailureCode != "" || len(kube.cancelled) != 2 {
		t.Fatalf("stored=%#v cancels=%v err=%v", stored, kube.cancelled, err)
	}
}

func TestCachePromotionFailureKeepsImageSuccessButNeverAdvertisesImport(t *testing.T) {
	store, _ := seedMemory(t, RegistryManaged)
	clock := testNow
	attempt := createAttempt(t, store, RegistryManaged, &clock)
	controller := &BuildController{Store: store, Provider: &fakeProvider{resolvedCommit: attempt.CommitSHA, now: clock}, Kubernetes: &fakeKubernetes{state: WorkloadSucceeded, promoted: false}, Owner: "build-controller", LeaseDuration: time.Minute, Now: func() time.Time { return clock }}
	if result, err := controller.ReconcileNext(context.Background()); err != nil || result.State != AttemptSucceeded {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	stored, _ := store.Attempt(context.Background(), attempt.ID)
	if stored.CacheReference != "" || stored.Result == nil || !containsWarning(stored.Result.Warnings, builder.WarningCacheDegraded) {
		t.Fatalf("stored=%#v", stored)
	}
}

func TestCancellationWinsConcurrentFailureAndSuccess(t *testing.T) {
	store, _ := seedMemory(t, RegistryManaged)
	clock := testNow
	createAttempt(t, store, RegistryManaged, &clock)
	claimed, err := store.ClaimNextAttempt(context.Background(), "builder-race", clock, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.MarkAttemptRunning(context.Background(), claimed.ID, "builder-race", clock); err != nil {
		t.Fatal(err)
	}
	if _, err = store.RequestCancel(context.Background(), claimed.ID, clock.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	result := builder.BuildResult{APIVersion: builder.ProtocolVersion, OperationID: claimed.ID, Generation: claimed.Generation, Status: "Succeeded", Image: builder.Image{Reference: claimed.PlanRequest.Build.Destination.Repository + "@sha256:" + strings.Repeat("f", 64), Digest: "sha256:" + strings.Repeat("f", 64), Platforms: claimed.PlanRequest.Build.Platforms}, StartedAt: clock, CompletedAt: clock.Add(time.Second)}
	if err = store.CompleteAttempt(context.Background(), claimed.ID, "builder-race", BuildCompletion{Result: result, LogReference: "k8s://kuberploy-build-dind/pods/build-pod/containers/agent"}, clock.Add(2*time.Second)); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("success overrode cancellation: %v", err)
	}
	retryAt := clock.Add(20 * time.Second)
	if retry, retryErr := store.ScheduleAttemptRetry(context.Background(), claimed.ID, "builder-race", "builder-job-failed", clock.Add(2*time.Second), retryAt); retryErr != nil || !retry {
		t.Fatalf("cancellation retry scheduled=%v err=%v", retry, retryErr)
	}
	persisted, _ := store.Attempt(context.Background(), claimed.ID)
	if persisted.State != AttemptCancelling || persisted.LeaseOwner != "" || !persisted.AvailableAt.Equal(retryAt) {
		t.Fatalf("persisted=%#v", persisted)
	}
}

func TestExpiredLeaseCannotPublishResult(t *testing.T) {
	store, _ := seedMemory(t, RegistryManaged)
	clock := testNow
	attempt := createAttempt(t, store, RegistryManaged, &clock)
	claimed, err := store.ClaimNextAttempt(context.Background(), "stale-worker", clock, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.MarkAttemptRunning(context.Background(), claimed.ID, "stale-worker", clock); err != nil {
		t.Fatal(err)
	}
	result := builder.BuildResult{APIVersion: builder.ProtocolVersion, OperationID: claimed.ID, Generation: claimed.Generation, Status: "Succeeded", Image: builder.Image{Reference: claimed.PlanRequest.Build.Destination.Repository + "@sha256:" + strings.Repeat("f", 64), Digest: "sha256:" + strings.Repeat("f", 64), Platforms: claimed.PlanRequest.Build.Platforms}, StartedAt: clock, CompletedAt: clock.Add(time.Second)}
	if err = store.CompleteAttempt(context.Background(), attempt.ID, "stale-worker", BuildCompletion{Result: result, LogReference: "k8s://kuberploy-build-dind/pods/build-pod/containers/agent"}, clock.Add(6*time.Second)); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("expired lease published: %v", err)
	}
}

func containsWarning(warnings []builder.Warning, want builder.Warning) bool {
	for _, warning := range warnings {
		if warning == want {
			return true
		}
	}
	return false
}
