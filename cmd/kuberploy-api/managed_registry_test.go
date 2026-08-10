package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/registry"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

func apiRegistryConfig() registry.RuntimeConfig {
	return registry.RuntimeConfig{
		Enabled: true, TargetID: "11111111-1111-4111-8111-111111111111",
		Endpoint: "http://kuberploy-registry.kuberploy-registry.svc.cluster.local:5000", RepositoryPrefix: "kuberploy",
		CredentialRef: "operator/managed-registry", AllowPlainHTTP: true, Namespace: "kuberploy-registry",
		Deployment: "kuberploy-registry", PersistentVolumeClaim: "kuberploy-registry", RegistryConfigMap: "kuberploy-registry-config-abc123",
		HelperServiceAccount: "kuberploy-registry-maintenance",
		HelperImage:          "ghcr.io/kuberploy/kuberploy-worker@sha256:" + strings.Repeat("a", 64),
		ObservationInterval:  5 * time.Minute,
	}
}

func TestManagedRegistryAPIConstructsLocalManagementAndExactProbe(t *testing.T) {
	store := memory.New()
	admin := domain.User{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Role: "platform-admin", GrantRevision: 1, CreatedAt: time.Now().UTC()}
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
	created, err := configured.management.CreateTarget(context.Background(), admin.ID, "managed-target", "fingerprint", "request", registry.RegistryTargetInput{
		Name: "managed", Mode: domain.RegistryTargetManaged, Endpoint: config.Endpoint, RepositoryPrefix: config.RepositoryPrefix,
		PullCredentialRef: "registry-pull", PushCredentialRef: "registry-push", CacheCredentialRef: "registry-cache",
	})
	if err != nil || created.Value.ID != config.TargetID {
		t.Fatalf("managed target=%+v err=%v", created.Value, err)
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
	_, err := newManagedRegistryAPI(registry.RuntimeConfig{Enabled: true}, memory.New())
	if err == nil {
		t.Fatal("partial managed registry configuration was accepted")
	}
}
