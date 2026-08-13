package registry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/store"
)

type runtimeStoreStub struct {
	store.RegistryRuntimeStore
	planID    string
	nextCalls int
}

func (s *runtimeStoreStub) NextAcceptedRegistryCleanup(context.Context, string, time.Time) (string, error) {
	s.nextCalls++
	if s.planID == "" {
		return "", store.ErrNotFound
	}
	return s.planID, nil
}

type targetReaderStub struct {
	target domain.RegistryTarget
	err    error
}

func (s targetReaderStub) RegistryTarget(context.Context, string) (domain.RegistryTarget, error) {
	return s.target, s.err
}

type cleanupExecutorStub struct {
	calls   int
	plan    string
	owner   string
	err     error
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *cleanupExecutorStub) Execute(_ context.Context, plan, owner string) error {
	s.calls++
	s.plan, s.owner = plan, owner
	if s.entered != nil {
		s.once.Do(func() { close(s.entered) })
		<-s.release
	}
	return s.err
}

func TestRuntimeControllerSerializesObservationAgainstCleanup(t *testing.T) {
	config := testManagedRuntimeConfig(t)
	target, err := config.ManagedTarget()
	if err != nil {
		t.Fatal(err)
	}
	controller, _, executor := runtimeControllerForCleanup(t, target)
	executor.entered, executor.release = make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() { _, runErr := controller.ReconcileCleanup(t.Context()); done <- runErr }()
	<-executor.entered
	locked := make(chan struct{})
	go func() { controller.runtimeMu.RLock(); close(locked); controller.runtimeMu.RUnlock() }()
	select {
	case <-locked:
		t.Fatal("observation lock entered during cleanup")
	case <-time.After(20 * time.Millisecond):
	}
	close(executor.release)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-locked:
	case <-time.After(time.Second):
		t.Fatal("observation lock remained blocked after cleanup")
	}
}

func runtimeControllerForCleanup(t *testing.T, target domain.RegistryTarget) (*RuntimeController, *runtimeStoreStub, *cleanupExecutorStub) {
	t.Helper()
	config := testManagedRuntimeConfig(t)
	runtimeStore := &runtimeStoreStub{planID: "22222222-2222-4222-8222-222222222222"}
	executor := &cleanupExecutorStub{}
	credentials := distributionCredentialSourceFunc(func(context.Context, string) (DistributionAuthorization, error) {
		return DistributionAuthorization{}, ErrDistributionCredentialUnavailable
	})
	controller := &RuntimeController{
		Store: runtimeStore, Targets: targetReaderStub{target: target}, Credentials: credentials, Cleanup: executor,
		Config: config, Owner: "worker-managed-registry-test", LeaseDuration: time.Minute, HeartbeatInterval: 10 * time.Second,
		IdleDelay: time.Second, MinimumBackoff: time.Second, MaximumBackoff: time.Minute,
	}
	return controller, runtimeStore, executor
}

func TestRuntimeControllerCleanupRequiresExactManagedTarget(t *testing.T) {
	config := testManagedRuntimeConfig(t)
	exact, err := config.ManagedTarget()
	if err != nil {
		t.Fatal(err)
	}
	controller, runtimeStore, executor := runtimeControllerForCleanup(t, exact)
	didWork, err := controller.ReconcileCleanup(t.Context())
	if err != nil || !didWork || runtimeStore.nextCalls != 1 || executor.calls != 1 ||
		executor.plan != runtimeStore.planID || executor.owner != controller.Owner+"-cleanup" {
		t.Fatalf("didWork=%v next=%d executor=%+v err=%v", didWork, runtimeStore.nextCalls, executor, err)
	}

	for name, mutate := range map[string]func(*domain.RegistryTarget){
		"external":        func(target *domain.RegistryTarget) { target.Mode = domain.RegistryTargetExternal },
		"origin":          func(target *domain.RegistryTarget) { target.Endpoint = "https://attacker.invalid" },
		"prefix":          func(target *domain.RegistryTarget) { target.RepositoryPrefix = "other" },
		"lifecycle-reuse": func(target *domain.RegistryTarget) { target.PushCredentialRef = config.CredentialRef },
	} {
		t.Run(name, func(t *testing.T) {
			changed := exact
			mutate(&changed)
			controller, runtimeStore, executor := runtimeControllerForCleanup(t, changed)
			didWork, err := controller.ReconcileCleanup(t.Context())
			if didWork || !errors.Is(err, ErrDistributionScopeMismatch) || runtimeStore.nextCalls != 0 || executor.calls != 0 {
				t.Fatalf("didWork=%v next=%d cleanup=%d err=%v", didWork, runtimeStore.nextCalls, executor.calls, err)
			}
		})
	}
	separateBuildCredential := exact
	separateBuildCredential.PushCredentialRef = "rotated-builder-push-secret"
	controller, runtimeStore, executor = runtimeControllerForCleanup(t, separateBuildCredential)
	didWork, err = controller.ReconcileCleanup(t.Context())
	if didWork || !errors.Is(err, ErrDistributionScopeMismatch) || runtimeStore.nextCalls != 0 || executor.calls != 0 {
		t.Fatalf("operator-owned build credential mutation was accepted: didWork=%v next=%d cleanup=%d err=%v", didWork, runtimeStore.nextCalls, executor.calls, err)
	}
}
