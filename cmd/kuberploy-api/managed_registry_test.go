package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/registry"
	base "github.com/kuberploy/kuberploy/internal/store"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

type managedRegistryAPIStoreStub struct{ *memory.Store }

func (managedRegistryAPIStoreStub) RefreshRegistryProtection(context.Context, string, string, time.Time, bool) error {
	return nil
}

func apiRegistryConfig() registry.RuntimeConfig {
	return registry.RuntimeConfig{
		Enabled: true, TargetID: "11111111-1111-4111-8111-111111111111",
		TargetName: "Managed registry", PullCredentialRef: "registry-pull",
		PushCredentialRef: "registry-push", CacheCredentialRef: "registry-cache",
		Endpoint: "http://kuberploy-registry.kuberploy-registry.svc.cluster.local:5000", RepositoryPrefix: "kuberploy",
		CredentialRef: "operator/managed-registry", AllowPlainHTTP: true, Namespace: "kuberploy-registry",
		Deployment: "kuberploy-registry", PersistentVolumeClaim: "kuberploy-registry", RegistryConfigMap: "kuberploy-registry-config-abc123",
		HelperServiceAccount: "kuberploy-registry-maintenance",
		HelperImage:          "ghcr.io/kuberploy/kuberploy-worker@sha256:" + strings.Repeat("a", 64),
		ObservationInterval:  5 * time.Minute,
	}
}

func TestManagedRegistryAPIConstructsLocalManagementAndExactProbe(t *testing.T) {
	store := managedRegistryAPIStoreStub{memory.New()}
	admin := domain.User{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Email: "admin@registry.test", Role: "platform-admin", GrantRevision: 1, CreatedAt: time.Now().UTC()}
	if err := store.BootstrapAdmin(context.Background(), admin, "hash", make([]byte, 32), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	disabled, err := newManagedRegistryAPI(registry.RuntimeConfig{}, store)
	if err != nil || disabled.management == nil || disabled.readiness != nil {
		t.Fatalf("disabled API=%+v err=%v", disabled, err)
	}
	config := apiRegistryConfig()
	configured, err := newManagedRegistryAPI(config, store)
	if err != nil || configured.management == nil || configured.readiness == nil {
		t.Fatalf("configured API=%+v err=%v", configured, err)
	}
	targets, err := configured.management.Targets(context.Background(), admin.ID)
	if err != nil || len(targets) != 1 || targets[0].ID != config.TargetID || targets[0].Mode != domain.RegistryTargetManaged || targets[0].Name != config.TargetName {
		t.Fatalf("managed targets=%+v err=%v", targets, err)
	}
	_, err = configured.management.UpdateTarget(context.Background(), admin.ID, "managed-target", "fingerprint", "request", config.TargetID, registry.RegistryTargetInput{
		Name: "External", Mode: domain.RegistryTargetExternal, Endpoint: config.Endpoint, RepositoryPrefix: config.RepositoryPrefix,
	})
	if !errors.Is(err, base.ErrConflict) {
		t.Fatalf("operator managed target redefinition error=%v", err)
	}
	if err = configured.readiness.Probe(context.Background()); !errors.Is(err, registry.ErrRegistryRuntimeNotReady) {
		t.Fatalf("unobserved probe=%v", err)
	}
	identity, _ := registry.RuntimeIdentityForConfig(config)
	now := time.Now().UTC()
	if _, err = store.AcquireManagedRegistryReadiness(context.Background(), registry.RuntimeWorkerObservation{
		WorkerID: "worker-registry-ready-01234567", RuntimeIdentity: identity, StartedAt: now, ObservedAt: now,
	}, registry.ManagedRegistryReadinessLease); err != nil {
		t.Fatal(err)
	}
	if err = configured.readiness.Probe(context.Background()); err != nil {
		t.Fatalf("fresh exact probe=%v", err)
	}
}

func TestManagedRegistryAPIRejectsPartialConfiguration(t *testing.T) {
	_, err := newManagedRegistryAPI(registry.RuntimeConfig{Enabled: true}, managedRegistryAPIStoreStub{memory.New()})
	if err == nil {
		t.Fatal("partial managed registry configuration was accepted")
	}
}
