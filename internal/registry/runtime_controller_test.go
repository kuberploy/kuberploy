package registry

import (
	"context"
	"errors"
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
	calls int
	plan  string
	owner string
	err   error
}

func (s *cleanupExecutorStub) Execute(_ context.Context, plan, owner string) error {
	s.calls++
	s.plan, s.owner = plan, owner
	return s.err
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
	exact := domain.RegistryTarget{ID: config.TargetID, Name: "managed", Mode: domain.RegistryTargetManaged,
		Endpoint: config.Endpoint, RepositoryPrefix: config.RepositoryPrefix, PushCredentialRef: "builder-push-secret"}
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
	if err != nil || !didWork || runtimeStore.nextCalls != 1 || executor.calls != 1 {
		t.Fatalf("separate build credential affected lifecycle runtime: didWork=%v next=%d cleanup=%d err=%v", didWork, runtimeStore.nextCalls, executor.calls, err)
	}
}
