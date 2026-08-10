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
	disabled, err := newManagedRegistryAPI(registry.RuntimeConfig{}, store)
	if err != nil || disabled.management == nil || disabled.readiness != nil {
		t.Fatalf("disabled API=%+v err=%v", disabled, err)
	}
	config := apiRegistryConfig()
	configured, err := newManagedRegistryAPI(config, store)
	if err != nil || configured.management == nil || configured.readiness == nil {
		t.Fatalf("configured API=%+v err=%v", configured, err)
	}
	if err = configured.readiness.Probe(context.Background()); !errors.Is(err, registry.ErrRegistryRuntimeNotReady) {
		t.Fatalf("unobserved probe=%v", err)
	}
	if _, err = store.PutRegistryTarget(context.Background(), domain.RegistryTarget{ID: config.TargetID, Name: "managed",
		Mode: domain.RegistryTargetManaged, Endpoint: config.Endpoint, RepositoryPrefix: config.RepositoryPrefix,
		PushCredentialRef: "builder-push-secret"}); err != nil {
		t.Fatal(err)
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
