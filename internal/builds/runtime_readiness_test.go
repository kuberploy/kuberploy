package builds

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/builder"
)

func runtimeConfigFixture(t *testing.T) WorkerRuntimeConfig {
	t.Helper()
	values := map[string]string{
		GitHubBuildsEnabledEnv: "true", GitHubAppIDEnv: "12345", GitHubAppClientIDEnv: "Iv1_KuberployClient",
		BuilderNamespaceEnv: "kuberploy-build-dind", BuilderPodServiceAccountEnv: "kuberploy-build-pod",
		BuilderAgentImageEnv:        "ghcr.io/kuberploy/builder@sha256:" + strings.Repeat("a", 64),
		BuilderBuildKitImageEnv:     builder.DefaultBuildKitImage,
		BuilderSourceEgressCIDRsEnv: "192.0.2.10/32", BuilderRegistryEgressCIDRsEnv: "192.0.2.20/32",
		"KUBERPLOY_KUBE_API_SERVER_CIDRS": "10.43.0.1/32",
	}
	config, err := WorkerRuntimeConfigFromLookup(mapLookup(values))
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func TestRuntimeReadinessRequiresFreshExactIdentity(t *testing.T) {
	store := NewMemoryStore()
	identity, err := RuntimeIdentity(runtimeConfigFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)
	observation := SourceBuildWorkerObservation{WorkerID: "worker-runtime-0123456789", SourceBuildRuntimeIdentity: identity,
		StartedAt: now.Add(-time.Minute), ObservedAt: now}
	if err = store.ObserveSourceBuildWorker(context.Background(), observation); err != nil {
		t.Fatal(err)
	}
	probe := &RuntimeReadinessProbe{Store: store, Identity: identity, Now: func() time.Time { return now }}
	if err = probe.Probe(context.Background()); err != nil {
		t.Fatalf("matching observation was not ready: %v", err)
	}
	for name, mutation := range map[string]func(*SourceBuildRuntimeIdentity){
		"digest":    func(i *SourceBuildRuntimeIdentity) { i.ConfigDigest = "sha256:" + strings.Repeat("b", 64) },
		"app":       func(i *SourceBuildRuntimeIdentity) { i.GitHubAppID++ },
		"namespace": func(i *SourceBuildRuntimeIdentity) { i.BuilderNamespace = "other-builder" },
		"agent": func(i *SourceBuildRuntimeIdentity) {
			i.BuilderAgentImage = "ghcr.io/kuberploy/builder@sha256:" + strings.Repeat("c", 64)
		},
	} {
		t.Run(name, func(t *testing.T) {
			mismatch := identity
			mutation(&mismatch)
			if err := store.SourceBuildRuntimeReady(context.Background(), mismatch, now, SourceBuildHeartbeatMaxAge); !errors.Is(err, ErrRuntimeNotReady) {
				t.Fatalf("mismatched identity ready: %v", err)
			}
		})
	}
	if err = store.SourceBuildRuntimeReady(context.Background(), identity, now.Add(SourceBuildHeartbeatMaxAge+time.Second), SourceBuildHeartbeatMaxAge); !errors.Is(err, ErrRuntimeNotReady) {
		t.Fatalf("stale observation ready: %v", err)
	}
	if err = store.SourceBuildRuntimeReady(context.Background(), identity, now.Add(-10*time.Second), SourceBuildHeartbeatMaxAge); !errors.Is(err, ErrRuntimeNotReady) {
		t.Fatalf("future observation ready: %v", err)
	}
}

func TestRuntimeReadinessConcurrentHeartbeatsNeverRegress(t *testing.T) {
	store := NewMemoryStore()
	identity, _ := RuntimeIdentity(runtimeConfigFixture(t))
	start := time.Now().UTC().Add(-time.Minute)
	var workers sync.WaitGroup
	for i := 0; i < 32; i++ {
		workers.Add(1)
		go func(offset int) {
			defer workers.Done()
			_ = store.ObserveSourceBuildWorker(context.Background(), SourceBuildWorkerObservation{WorkerID: "worker-runtime-concurrent", SourceBuildRuntimeIdentity: identity,
				StartedAt: start, ObservedAt: start.Add(time.Duration(offset) * time.Second)})
		}(i)
	}
	workers.Wait()
	if err := store.SourceBuildRuntimeReady(context.Background(), identity, start.Add(31*time.Second), time.Minute); err != nil {
		t.Fatal(err)
	}
}
