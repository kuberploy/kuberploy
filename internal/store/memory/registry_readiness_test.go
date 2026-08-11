package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/registry"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func memoryRegistryRuntimeConfig() registry.RuntimeConfig {
	return registry.RuntimeConfig{
		Enabled: true, TargetID: "11111111-1111-4111-8111-111111111111",
		TargetName: "Managed registry", PullCredentialRef: "registry-pull",
		PushCredentialRef: "builder-push-secret", CacheCredentialRef: "builder-cache-secret",
		Endpoint: "http://kuberploy-registry.kuberploy-registry.svc.cluster.local:5000", RepositoryPrefix: "kuberploy",
		CredentialRef: "operator/managed-registry", AllowPlainHTTP: true, Namespace: "kuberploy-registry",
		Deployment: "kuberploy-registry", PersistentVolumeClaim: "kuberploy-registry", RegistryConfigMap: "kuberploy-registry-config-abc123",
		HelperServiceAccount: "kuberploy-registry-maintenance",
		HelperImage:          "ghcr.io/kuberploy/kuberploy-worker@sha256:" + strings.Repeat("a", 64),
		ObservationInterval:  5 * time.Minute,
	}
}

func TestManagedRegistryRuntimeReadinessIsFreshExactAndEpochFenced(t *testing.T) {
	ctx := context.Background()
	store := New()
	config := memoryRegistryRuntimeConfig()
	now := time.Date(2026, 8, 9, 9, 0, 0, 0, time.UTC)
	target, err := config.ManagedTarget()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutRegistryTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	identity, err := registry.RuntimeIdentityForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	observation := registry.RuntimeWorkerObservation{WorkerID: "worker-registry-ready-01234567", RuntimeIdentity: identity, StartedAt: now, ObservedAt: now}
	first, err := store.AcquireManagedRegistryReadiness(ctx, observation, registry.ManagedRegistryReadinessLease)
	if err != nil || first.Epoch != 1 {
		t.Fatalf("first lease=%+v err=%v", first, err)
	}
	probe := &registry.RuntimeReadinessProbe{Store: store, Targets: store, Config: config, Now: func() time.Time { return now }}
	if err = probe.Probe(ctx); err != nil {
		t.Fatalf("fresh probe: %v", err)
	}
	second, err := store.AcquireManagedRegistryReadiness(ctx, observation, registry.ManagedRegistryReadinessLease)
	if err != nil || second.Epoch != 2 {
		t.Fatalf("replacement lease=%+v err=%v", second, err)
	}
	if _, err = store.HeartbeatManagedRegistryReadiness(ctx, first, now.Add(time.Second), registry.ManagedRegistryReadinessLease); !errors.Is(err, base.ErrRegistryLeaseLost) {
		t.Fatalf("stale worker heartbeat err=%v", err)
	}
	if _, err = store.HeartbeatManagedRegistryReadiness(ctx, second, now.Add(10*time.Second), registry.ManagedRegistryReadinessLease); err != nil {
		t.Fatal(err)
	}
	probe.Now = func() time.Time { return now.Add(10 * time.Second) }
	if err = probe.Probe(ctx); err != nil {
		t.Fatalf("heartbeat probe: %v", err)
	}
	rotatedBuildCredential := target
	rotatedBuildCredential.PushCredentialRef = "rotated-builder-push-secret"
	if _, err = store.PutRegistryTarget(ctx, rotatedBuildCredential); err != nil {
		t.Fatal(err)
	}
	if err = probe.Probe(ctx); !errors.Is(err, registry.ErrRegistryRuntimeNotReady) {
		t.Fatalf("operator-owned build-push credential mutation was accepted: %v", err)
	}
	reusedLifecycleCredential := target
	reusedLifecycleCredential.PushCredentialRef = config.CredentialRef
	if _, err = store.PutRegistryTarget(ctx, reusedLifecycleCredential); err != nil {
		t.Fatal(err)
	}
	if err = probe.Probe(ctx); !errors.Is(err, registry.ErrRegistryRuntimeNotReady) {
		t.Fatalf("lifecycle credential reuse as build-push credential was accepted: %v", err)
	}
	if _, err = store.PutRegistryTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	mutatedTarget := target
	mutatedTarget.Endpoint = "http://other-registry.kuberploy-registry.svc.cluster.local:5000"
	if _, err = store.PutRegistryTarget(ctx, mutatedTarget); err != nil {
		t.Fatal(err)
	}
	if err = probe.Probe(ctx); !errors.Is(err, registry.ErrRegistryRuntimeNotReady) {
		t.Fatalf("mutated target probe=%v", err)
	}
	if _, err = store.PutRegistryTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	changed := config
	changed.ObservationInterval++
	probe.Config = changed
	if err = probe.Probe(ctx); !errors.Is(err, registry.ErrRegistryRuntimeNotReady) {
		t.Fatalf("mismatched config probe=%v", err)
	}
	probe.Config = config
	probe.Now = func() time.Time { return now.Add(registry.ManagedRegistryHeartbeatMaxAge + 11*time.Second) }
	if err = probe.Probe(ctx); !errors.Is(err, registry.ErrRegistryRuntimeNotReady) {
		t.Fatalf("stale probe=%v", err)
	}
}

func TestManagedRegistryReadinessRejectsExternalTarget(t *testing.T) {
	store := New()
	config := memoryRegistryRuntimeConfig()
	target, targetErr := config.ManagedTarget()
	if targetErr != nil {
		t.Fatal(targetErr)
	}
	target.Mode = domain.RegistryTargetExternal
	if _, err := store.PutRegistryTarget(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	identity, _ := registry.RuntimeIdentityForConfig(config)
	now := time.Now().UTC()
	_, err := store.AcquireManagedRegistryReadiness(context.Background(), registry.RuntimeWorkerObservation{
		WorkerID: "worker-registry-ready-01234567", RuntimeIdentity: identity, StartedAt: now, ObservedAt: now,
	}, registry.ManagedRegistryReadinessLease)
	if !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("external target readiness err=%v", err)
	}
}
