package argo

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/githubapp"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

type productionRuntimeClaimGateStub struct{}

func (productionRuntimeClaimGateStub) ValidateDesiredStateClaim(context.Context, DesiredStateCommand, DesiredStateClaimMode) error {
	return nil
}

func (productionRuntimeClaimGateStub) productionDesiredStateClaimGate() {}

type productionRuntimeBindingStoreStub struct{}

func (productionRuntimeBindingStoreStub) Binding(context.Context, string) (gitprojection.Binding, error) {
	return gitprojection.Binding{}, ErrNotFound
}

type productionRuntimeHeadVerifierStub struct{}

func (productionRuntimeHeadVerifierStub) VerifyTargetHead(context.Context, gitprojection.Binding, gitprojection.ObservationSource) (gitprojection.VerifiedHead, error) {
	return gitprojection.VerifiedHead{}, ErrNotFound
}

type productionRuntimePrerequisiteStub struct {
	proof ProductionPrerequisiteProof
	err   error
}

func (s productionRuntimePrerequisiteStub) ObserveProductionPrerequisites(context.Context, time.Time) (ProductionPrerequisiteProof, error) {
	return s.proof, s.err
}

type signalingNotReadyPrerequisite struct{ called chan struct{} }

func (s signalingNotReadyPrerequisite) ObserveProductionPrerequisites(context.Context, time.Time) (ProductionPrerequisiteProof, error) {
	select {
	case s.called <- struct{}{}:
	default:
	}
	return ProductionPrerequisiteProof{}, ErrArgoRuntimePrerequisiteNotReady
}

type rateLimitedPrerequisite struct {
	called  chan struct{}
	retryAt time.Time
	calls   atomic.Int64
}

func (s *rateLimitedPrerequisite) ObserveProductionPrerequisites(context.Context, time.Time) (ProductionPrerequisiteProof, error) {
	s.calls.Add(1)
	select {
	case s.called <- struct{}{}:
	default:
	}
	return ProductionPrerequisiteProof{}, errors.Join(ErrArgoRuntimePrerequisiteNotReady, &githubapp.APIError{
		Class: githubapp.APIErrorRateLimit, RetryAt: s.retryAt,
	})
}

type productionRuntimeMaterializerStub struct {
	called chan struct{}
	err    error
}

type productionRuntimeRefresherStub struct{}

func (productionRuntimeRefresherStub) RefreshPlatformRootApplication(context.Context, PlatformRootApplicationExpectation, time.Time) error {
	return nil
}

type transientPrerequisiteMaterializer struct {
	calls     atomic.Int64
	recovered chan struct{}
}

func (s *transientPrerequisiteMaterializer) MaterializeDesiredStateOnce(context.Context, time.Time) (bool, error) {
	if s.calls.Add(1) == 1 {
		return false, ErrArgoRuntimePrerequisiteNotReady
	}
	select {
	case s.recovered <- struct{}{}:
	default:
	}
	return false, nil
}

func (s productionRuntimeMaterializerStub) MaterializeDesiredStateOnce(context.Context, time.Time) (bool, error) {
	select {
	case s.called <- struct{}{}:
	default:
	}
	return false, s.err
}

func productionRuntimeFixture(t *testing.T, now time.Time) (*ProductionDesiredStateRuntime, *MemoryDesiredStateStore) {
	t.Helper()
	platform, _ := productionBindings(t, now)
	identity := productionIdentity(t, platform)
	store := NewMemoryDesiredStateStore()
	writer := &DesiredStateWriter{Store: store, Bindings: productionRuntimeBindingStoreStub{}, ClaimGate: productionRuntimeClaimGateStub{},
		Provider: productionRuntimeHeadVerifierStub{}, Manager: &gitprojection.MirrorManager{}, RootRefresher: productionRuntimeRefresherStub{},
		ObservationWaker: NewMemoryObservationStore(), Identity: identity}
	worker := &DesiredStateRuntimeWorker{Store: store, Writer: writer, PollInterval: 250 * time.Millisecond,
		Observation: DesiredStateRuntimeWorkerObservation{WorkerID: "argo-production-runtime-test", DesiredStateRuntimeIdentity: identity,
			StartedAt: now, ObservedAt: now}, Now: func() time.Time { return now }}
	proof := ProductionPrerequisiteProof{PlatformBindingID: identity.PlatformBindingID, PlatformHead: platform.TargetHeadRevision,
		ProtectionDigest: "sha256:" + strings.Repeat("9", 64), CredentialCount: 1,
		RootUID: "7a111111-1111-4111-8111-111111111111", RootSpecDigest: "sha256:" + strings.Repeat("8", 64), ObservedAt: now}
	runtime := &ProductionDesiredStateRuntime{Worker: worker, Prerequisites: productionRuntimePrerequisiteStub{proof: proof},
		Materializer: productionRuntimeMaterializerStub{called: make(chan struct{}, 1)}, PollInterval: 250 * time.Millisecond,
		Now: func() time.Time { return now }}
	return runtime, store
}

func TestProductionDesiredStateRuntimeRejectsInvalidPrerequisiteProofBeforeReadiness(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	runtime, store := productionRuntimeFixture(t, now)
	runtime.Prerequisites = productionRuntimePrerequisiteStub{proof: ProductionPrerequisiteProof{}}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if err := runtime.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("invalid proof did not remain safely pre-ready: %v", err)
	}
	if err := store.DesiredStateRuntimeReady(t.Context(), runtime.Worker.Observation.DesiredStateRuntimeIdentity,
		now, DesiredStateHeartbeatMaxAge); !errors.Is(err, ErrDesiredStateNotReady) {
		t.Fatalf("invalid proof left a readiness receipt: %v", err)
	}
	probe := &ProductionDesiredStateReadinessProbe{Store: store, Identity: runtime.Worker.Observation.DesiredStateRuntimeIdentity,
		Now: func() time.Time { return now }}
	if err := probe.Probe(t.Context()); !errors.Is(err, ErrDesiredStateNotReady) {
		t.Fatalf("production probe accepted invalid prerequisite startup: %v", err)
	}
}

func TestProductionDesiredStateRuntimeCancellationInterruptsPrerequisiteWait(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	runtime, store := productionRuntimeFixture(t, now)
	called := make(chan struct{}, 1)
	runtime.Prerequisites = signalingNotReadyPrerequisite{called: called}
	runtime.PollInterval = time.Minute
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	select {
	case <-called:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("runtime did not enter prerequisite wait")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runtime cancellation error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runtime cancellation blocked draining the prerequisite timer")
	}
	if err := store.DesiredStateRuntimeReady(t.Context(), runtime.Worker.Observation.DesiredStateRuntimeIdentity,
		now, DesiredStateHeartbeatMaxAge); !errors.Is(err, ErrDesiredStateNotReady) {
		t.Fatalf("canceled pre-ready runtime left a readiness receipt: %v", err)
	}
}

func TestProductionDesiredStateRuntimeHonorsProviderRetryAt(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	runtime, _ := productionRuntimeFixture(t, now)
	prerequisite := &rateLimitedPrerequisite{called: make(chan struct{}, 1), retryAt: now.Add(500 * time.Millisecond)}
	runtime.Prerequisites = prerequisite
	runtime.PollInterval = 250 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	select {
	case <-prerequisite.called:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("runtime did not enter rate-limit wait")
	}
	time.Sleep(350 * time.Millisecond)
	if got := prerequisite.calls.Load(); got != 1 {
		cancel()
		t.Fatalf("rate-limited prerequisite retried before RetryAt: calls=%d", got)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("rate-limited runtime did not stop with its context: %v", err)
	}
}

func TestProductionDesiredStateRuntimeHeartbeatsOnlyCompositeReadyWorker(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	runtime, store := productionRuntimeFixture(t, now)
	materializer := runtime.Materializer.(productionRuntimeMaterializerStub)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	select {
	case <-materializer.called:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("production materializer was not polled")
	}
	if err := store.DesiredStateRuntimeReady(t.Context(), runtime.Worker.Observation.DesiredStateRuntimeIdentity,
		now, DesiredStateHeartbeatMaxAge); err != nil {
		cancel()
		t.Fatalf("composite-ready runtime was not observable: %v", err)
	}
	probe := &ProductionDesiredStateReadinessProbe{Store: store, Identity: runtime.Worker.Observation.DesiredStateRuntimeIdentity,
		Now: func() time.Time { return now }}
	if err := probe.Probe(t.Context()); err != nil {
		cancel()
		t.Fatalf("single production readiness probe rejected the exact lease: %v", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("runtime did not stop with its context: %v", err)
	}
}

func TestProductionDesiredStateRuntimeReacquiresAfterTransientPrerequisiteLoss(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	runtime, _ := productionRuntimeFixture(t, now)
	materializer := &transientPrerequisiteMaterializer{recovered: make(chan struct{}, 1)}
	runtime.Materializer = materializer
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	select {
	case <-materializer.recovered:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("runtime terminated instead of reacquiring after a transient prerequisite loss")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("recovered runtime did not stop with its context: %v", err)
	}
	if materializer.calls.Load() < 2 {
		t.Fatal("runtime did not enter a fresh readiness cycle")
	}
}
