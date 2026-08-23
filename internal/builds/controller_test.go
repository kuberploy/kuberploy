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

func TestControllerFailsWithoutBuilderCapacityBeforeCredentialsOrKubernetesObjects(t *testing.T) {
	store, _ := seedMemory(t, RegistryManaged)
	clock := testNow
	attempt := createAttempt(t, store, RegistryManaged, &clock)
	provider := &fakeProvider{resolvedCommit: attempt.CommitSHA, now: clock}
	kube := &fakeKubernetes{capacityErr: ErrBuilderCapacityUnavailable}
	controller := &BuildController{Store: store, Provider: provider, Kubernetes: kube, Owner: "build-controller", LeaseDuration: time.Minute, Now: func() time.Time { return clock }}
	result, err := controller.ReconcileNext(context.Background())
	if err != nil || result.State != AttemptFailed {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	stored, getErr := store.Attempt(context.Background(), attempt.ID)
	if getErr != nil || stored.State != AttemptFailed || stored.FailureCode != "builder-capacity-unavailable" || len(kube.workloads) != 0 || len(kube.cancelled) != 1 {
		t.Fatalf("stored=%#v workloads=%d cancels=%d err=%v", stored, len(kube.workloads), len(kube.cancelled), getErr)
	}
	if provider.mintCalls != 0 {
		t.Fatalf("provider credential minted without builder capacity: %d", provider.mintCalls)
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

func TestControllerProviderDeferralsNeverConsumeBuildExecutionBudget(t *testing.T) {
	store, _ := seedMemory(t, RegistryManaged)
	clock := testNow
	attempt := createAttempt(t, store, RegistryManaged, &clock)
	provider := &fakeProvider{resolvedCommit: attempt.CommitSHA, now: clock, transient: 2}
	kube := &fakeKubernetes{state: WorkloadSucceeded, promoted: true}
	controller := &BuildController{Store: store, Provider: provider, Kubernetes: kube, Owner: "build-controller", LeaseDuration: time.Minute, Now: func() time.Time { return clock }}

	for deferral := 0; deferral < 2; deferral++ {
		result, err := controller.ReconcileNext(context.Background())
		if !errors.Is(err, ErrProviderRetry) || result.State != AttemptPreparing || result.RetryAt.IsZero() {
			t.Fatalf("deferral %d result=%#v err=%v", deferral, result, err)
		}
		stored, getErr := store.Attempt(context.Background(), attempt.ID)
		if getErr != nil || stored.State != AttemptPreparing || stored.ExecutionAttempts != 1 || stored.FailureCode != "github-provider-retry" {
			t.Fatalf("deferral %d stored=%#v err=%v", deferral, stored, getErr)
		}
		clock = result.RetryAt.Add(time.Second)
		provider.now = clock
	}

	result, err := controller.ReconcileNext(context.Background())
	if err != nil || result.State != AttemptSucceeded {
		t.Fatalf("success result=%#v err=%v", result, err)
	}
	stored, err := store.Attempt(context.Background(), attempt.ID)
	if err != nil || stored.ExecutionAttempts != 1 || stored.State != AttemptSucceeded || len(kube.workloads) != 1 {
		t.Fatalf("stored=%#v workloads=%d err=%v", stored, len(kube.workloads), err)
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

func TestBuildResultRequiresClosedCacheReuseOutcome(t *testing.T) {
	digest := "sha256:" + strings.Repeat("f", 64)
	result := builder.BuildResult{
		APIVersion: builder.ProtocolVersion, OperationID: "11111111-1111-4111-8111-111111111111", Generation: 1, Status: "Succeeded",
		Image:      builder.Image{Reference: "registry.example.test/team/app@" + digest, Digest: digest, Platforms: []string{"linux/amd64"}},
		CacheReuse: builder.CacheReuseHit, StartedAt: testNow, CompletedAt: testNow.Add(time.Second),
	}
	if err := validateBuildResult(result, "", ""); err != nil {
		t.Fatalf("valid cache result rejected: %v", err)
	}
	result.Warnings = []builder.Warning{builder.WarningSensitiveBuildArg}
	if err := validateBuildResult(result, "", ""); err != nil {
		t.Fatalf("non-blocking build-argument warning rejected: %v", err)
	}
	for _, invalid := range []builder.CacheReuse{"", "raw-output", "Hit"} {
		result.CacheReuse = invalid
		if err := validateBuildResult(result, "", ""); !errors.Is(err, ErrInvalid) {
			t.Fatalf("cache reuse %q accepted: %v", invalid, err)
		}
	}
}

func TestLegacyStoredResultUsesUnknownCacheReuseWithoutFabricatingEvidence(t *testing.T) {
	result := &builder.BuildResult{}
	normalizeLegacyCacheReuse(result)
	if result.CacheReuse != builder.CacheReuseUnknown {
		t.Fatalf("legacy cache reuse = %q", result.CacheReuse)
	}
	result.CacheReuse = builder.CacheReuseHit
	normalizeLegacyCacheReuse(result)
	if result.CacheReuse != builder.CacheReuseHit {
		t.Fatalf("explicit cache reuse changed to %q", result.CacheReuse)
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
	result := builder.BuildResult{APIVersion: builder.ProtocolVersion, OperationID: claimed.ID, Generation: claimed.Generation, Status: "Succeeded", Image: builder.Image{Reference: claimed.PlanRequest.Build.Destination.Repository + "@sha256:" + strings.Repeat("f", 64), Digest: "sha256:" + strings.Repeat("f", 64), Platforms: claimed.PlanRequest.Build.Platforms}, CacheReuse: builder.CacheReuseHit, StartedAt: clock, CompletedAt: clock.Add(time.Second)}
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
	result := builder.BuildResult{APIVersion: builder.ProtocolVersion, OperationID: claimed.ID, Generation: claimed.Generation, Status: "Succeeded", Image: builder.Image{Reference: claimed.PlanRequest.Build.Destination.Repository + "@sha256:" + strings.Repeat("f", 64), Digest: "sha256:" + strings.Repeat("f", 64), Platforms: claimed.PlanRequest.Build.Platforms}, CacheReuse: builder.CacheReuseHit, StartedAt: clock, CompletedAt: clock.Add(time.Second)}
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
