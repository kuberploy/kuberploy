package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/edge"
)

type apiEdgeStore struct {
	*edge.MemoryStore
	closed bool
}

func (s *apiEdgeStore) Close() { s.closed = true }

func apiEdgeConfig() edge.RuntimeConfig {
	digest := "sha256:" + strings.Repeat("a", 64)
	return edge.RuntimeConfig{
		Enabled: true,
		Profiles: edge.RuntimeProfiles{ExternalDNS: []edge.ExternalDNSProfile{{
			IntegrationID: "55555555-5555-4555-8555-555555555555", Revision: 1, Mode: edge.ModeAdopted,
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

func TestEdgeAPIDefaultsOffWithoutOpeningDependencies(t *testing.T) {
	api, err := newEdgeAPI(t.Context(), "not-a-database-url", edge.RuntimeConfig{})
	if err != nil || api != nil {
		t.Fatalf("default-off edge API=%#v err=%v", api, err)
	}
	if _, err = newEdgeAPI(t.Context(), "not-a-database-url", edge.RuntimeConfig{Enabled: true}); !errors.Is(err, edge.ErrUnavailable) {
		t.Fatalf("partial enabled config reached PostgreSQL: %v", err)
	}
}

func TestEdgeAPIBuildsExactProbeAndFeatures(t *testing.T) {
	config := apiEdgeConfig()
	store := &apiEdgeStore{MemoryStore: edge.NewMemoryStore()}
	api, err := buildEdgeAPI(config, store)
	if err != nil {
		t.Fatal(err)
	}
	if api.features.Traefik || api.features.CertManager || !api.features.ExternalDNS {
		t.Fatalf("edge feature identity drifted: %#v", api.features)
	}
	if err = api.readiness.Probe(t.Context()); !errors.Is(err, edge.ErrUnavailable) {
		t.Fatalf("unobserved edge worker was ready: %v", err)
	}
	now := time.Now().UTC()
	digest, _ := config.Digest()
	targets, _ := config.DesiredTargets()
	if err = store.SynchronizeTargets(t.Context(), digest, targets, now); err != nil {
		t.Fatal(err)
	}
	lease, found, err := store.ClaimTarget(t.Context(), "edge-api-test-worker-0001", edge.RuntimeContract, digest, now, config.WorkLease)
	if err != nil || !found {
		t.Fatalf("claim=%#v found=%v err=%v", lease, found, err)
	}
	receipt := edge.ObservationReceipt{TargetKey: lease.Target.Key, DesiredDigest: lease.Target.DesiredDigest,
		IdentityDigest: "sha256:" + strings.Repeat("b", 64), ResourceVersionDigest: "sha256:" + strings.Repeat("c", 64)}
	if _, err = store.RecordTargetReady(t.Context(), lease, receipt, now.Add(time.Second), now.Add(config.PollInterval)); err != nil {
		t.Fatal(err)
	}
	if err = store.RecordReadiness(context.Background(), edge.Readiness{WorkerID: "edge-api-test-worker-0001", WorkerEpoch: 1,
		Contract: edge.RuntimeContract, ConfigDigest: digest, TargetCount: 1, StartedAt: now, ObservedAt: now.Add(time.Second),
		LeaseUntil: now.Add(config.ReadinessMaxAge)}); err != nil {
		t.Fatal(err)
	}
	if err = api.readiness.Probe(t.Context()); err != nil {
		t.Fatalf("fresh exact edge API probe failed: %v", err)
	}
	readiness, features := edgeHTTPRuntime(api)
	if readiness == nil || features != api.features {
		t.Fatalf("HTTP runtime seam drifted: readiness=%#v features=%#v", readiness, features)
	}
	api.Close()
	if !store.closed {
		t.Fatal("edge API store was not closed")
	}
}
