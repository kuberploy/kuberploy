package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/edge"
)

type workerEdgeStore struct {
	*edge.MemoryStore
	closed bool
}

func (s *workerEdgeStore) Close() { s.closed = true }

type workerEdgeObserver struct{}

func (workerEdgeObserver) ObserveTraefik(context.Context, edge.TraefikProfile) (edge.ObservationReceipt, error) {
	return edge.ObservationReceipt{}, edge.ErrUnavailable
}
func (workerEdgeObserver) ObserveCertManager(context.Context, edge.CertManagerProfile) (edge.ObservationReceipt, error) {
	return edge.ObservationReceipt{}, edge.ErrUnavailable
}
func (workerEdgeObserver) ObserveExternalDNS(context.Context, edge.ExternalDNSProfile) (edge.ObservationReceipt, error) {
	return edge.ObservationReceipt{}, edge.ErrUnavailable
}

func workerEdgeConfig() edge.RuntimeConfig {
	digest := "sha256:" + strings.Repeat("a", 64)
	return edge.RuntimeConfig{
		Enabled: true,
		Profiles: edge.RuntimeProfiles{ExternalDNS: []edge.ExternalDNSProfile{{
			IntegrationID: "66666666-6666-4666-8666-666666666666", Revision: 1, Mode: edge.ModeAdopted,
			Namespace: "external-dns", Version: "v0.18.0",
			Deployment: edge.DeploymentExpectation{Name: "external-dns-primary", ContainerName: "external-dns",
				Image: "registry.k8s.io/external-dns/external-dns:v0.18.0", SpecDigest: digest},
			ProfileConfigMap: "external-dns-profile", LabelFilter: "kuberploy.io/dns-integration=primary",
			ProviderKind: "cloudflare", TXTOwnerID: "kuberploy.primary", Policy: "upsert-only", DomainFilters: []string{"example.com"},
		}}},
		PollInterval: 30 * time.Second, WorkLease: 2 * time.Minute, HeartbeatInterval: 20 * time.Second,
		ReadinessMaxAge: 90 * time.Second, MinimumBackoff: 5 * time.Second, MaximumBackoff: 5 * time.Minute,
	}
}

func TestEdgeWorkerDefaultsOffAndRejectsInvalidIdentityBeforeDependencies(t *testing.T) {
	runtime, err := newEdgeRuntime(t.Context(), "not-a-database-url", "bad host", edge.RuntimeConfig{})
	if err != nil || runtime != nil {
		t.Fatalf("default-off edge runtime=%#v err=%v", runtime, err)
	}
	if _, err = newEdgeRuntime(t.Context(), "not-a-database-url", "bad host", edge.RuntimeConfig{Enabled: true}); !errors.Is(err, edge.ErrUnavailable) {
		t.Fatalf("partial config reached dependencies: %v", err)
	}
	if _, err = newEdgeRuntime(t.Context(), "not-a-database-url", "bad host", workerEdgeConfig()); !errors.Is(err, edge.ErrUnavailable) {
		t.Fatalf("invalid worker identity reached PostgreSQL: %v", err)
	}
}

func TestEdgeWorkerBuildsExactControllerAndClosesStore(t *testing.T) {
	config := workerEdgeConfig()
	store := &workerEdgeStore{MemoryStore: edge.NewMemoryStore()}
	workerID, err := edgeWorkerIdentity("worker-pod-0", 42, time.Date(2026, 8, 9, 1, 2, 3, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := buildEdgeRuntime(config, store, workerEdgeObserver{}, workerID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.controller.WorkerID != workerID || runtime.controller.WorkerEpoch != 1 ||
		runtime.controller.Config.TargetCount() != 1 || runtime.controller.Validate() != nil {
		t.Fatalf("edge controller identity drifted: %#v", runtime.controller)
	}
	if _, err = buildEdgeRuntime(config, store, nil, workerID, 1); !errors.Is(err, edge.ErrUnavailable) {
		t.Fatalf("nil observer accepted: %v", err)
	}
	runtime.Close()
	if !store.closed {
		t.Fatal("edge runtime store was not closed")
	}
}

func TestEdgeWorkerIdentityIsBoundedAndCanonical(t *testing.T) {
	now := time.Date(2026, 8, 9, 1, 2, 3, 0, time.UTC)
	identity, err := edgeWorkerIdentity("worker-pod-0", 42, now)
	if err != nil || !strings.HasPrefix(identity, "edge-worker:worker-pod-0:42:") || len(identity) > 128 {
		t.Fatalf("identity=%q err=%v", identity, err)
	}
	for _, host := range []string{"", "UPPERCASE", "bad host", strings.Repeat("a", 64)} {
		if _, err = edgeWorkerIdentity(host, 42, now); !errors.Is(err, edge.ErrUnavailable) {
			t.Fatalf("invalid host %q accepted: %v", host, err)
		}
	}
}
