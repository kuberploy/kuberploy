package helmapps

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/argo"
	"github.com/kuberploy/kuberploy/internal/id"
)

func protectedArgoReadinessFixture(t *testing.T, now time.Time) (*argo.MemoryDesiredStateStore,
	*argo.ProductionDesiredStateReadinessProbe, ProductionProtectedArgoReadinessConfig,
	argo.DesiredStateRuntimeWorkerObservation) {
	t.Helper()
	platformBindingID := id.New()
	repositoryCredential, err := argo.RepositoryCredentialName(platformBindingID)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := argo.DesiredStateRuntimeIdentityForConfig(argo.DesiredStateRuntimeConfig{
		Enabled: true, GitHubAppID: 1001, PlatformBindingID: platformBindingID,
		ArgoNamespace: "argocd", RootApplicationName: argo.PlatformRootApplicationName,
		RepositorySecretName: repositoryCredential,
		Runtime: argo.RuntimeLock{ChartRepository: "oci://ghcr.io/kuberploy/charts",
			ChartName: argo.RuntimeChartName, ChartVersion: "1.2.3",
			ChartDigest:   "sha256:" + strings.Repeat("c", 64),
			RendererImage: "ghcr.io/kuberploy/renderer@sha256:" + strings.Repeat("d", 64)},
		DigestEnforcement: argo.ChartDigestNativeOCI,
	})
	if err != nil {
		t.Fatal(err)
	}
	store := argo.NewMemoryDesiredStateStore()
	probe := &argo.ProductionDesiredStateReadinessProbe{Store: store, Identity: identity,
		MaxAge: argo.DesiredStateHeartbeatMaxAge,
		Now:    func() time.Time { return now.Add(24 * time.Hour) }}
	publisher := ProtectedPublisherIdentity{Contract: ProtectedPublisherContract, PolicyVersion: ProtectedGitPolicy,
		ConfigDigest: digestBytes([]byte("protected-argo-publisher"))}
	config := ProductionProtectedArgoReadinessConfig{PlatformBindingID: platformBindingID,
		Application: ProtectedApplicationRuntime{ArgoNamespace: identity.ArgoNamespace}, Publisher: publisher}
	observation := argo.DesiredStateRuntimeWorkerObservation{WorkerID: "helm-argo-readiness-worker", DesiredStateRuntimeIdentity: identity,
		StartedAt: now, ObservedAt: now}
	return store, probe, config, observation
}

func TestProductionProtectedArgoReadinessRequiresExactFreshProductionLease(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	store, probe, config, observation := protectedArgoReadinessFixture(t, now)
	readiness, err := NewProductionProtectedArgoReadiness(probe, config)
	if err != nil {
		t.Fatal(err)
	}
	if ready, readyErr := readiness.ProtectedHelmApplicationsReady(t.Context(), config.Publisher, now); readyErr != nil || ready {
		t.Fatalf("missing Argo lease ready=%v err=%v", ready, readyErr)
	}
	if _, err = store.AcquireDesiredStateReadiness(t.Context(), observation, argo.DesiredStateReadinessLease); err != nil {
		t.Fatal(err)
	}
	// The adapter overrides the caller-owned probe clock with the exact
	// capability evaluation time.
	if ready, readyErr := readiness.ProtectedHelmApplicationsReady(t.Context(), config.Publisher,
		now.Add(20*time.Second)); readyErr != nil || !ready {
		t.Fatalf("fresh exact Argo lease ready=%v err=%v", ready, readyErr)
	}
	if ready, readyErr := readiness.ProtectedHelmApplicationsReady(t.Context(), config.Publisher,
		now.Add(time.Minute)); readyErr != nil || ready {
		t.Fatalf("stale Argo lease ready=%v err=%v", ready, readyErr)
	}
}

func TestProductionProtectedArgoReadinessRejectsPublisherAndIdentitySubstitution(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	store, probe, config, observation := protectedArgoReadinessFixture(t, now)
	readiness, err := NewProductionProtectedArgoReadiness(probe, config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.AcquireDesiredStateReadiness(t.Context(), observation, argo.DesiredStateReadinessLease); err != nil {
		t.Fatal(err)
	}
	wrongPublisher := config.Publisher
	wrongPublisher.ConfigDigest = digestBytes([]byte("substituted-publisher"))
	if ready, readyErr := readiness.ProtectedHelmApplicationsReady(t.Context(), wrongPublisher,
		now.Add(time.Second)); !errors.Is(readyErr, ErrInvalid) || ready {
		t.Fatalf("publisher substitution ready=%v err=%v", ready, readyErr)
	}
	// Constructor snapshots the concrete production probe; mutating the caller's
	// pointer after construction cannot redirect the adapter to another runtime.
	probe.Identity.PlatformBindingID = id.New()
	if ready, readyErr := readiness.ProtectedHelmApplicationsReady(t.Context(), config.Publisher,
		now.Add(time.Second)); readyErr != nil || !ready {
		t.Fatalf("caller probe mutation changed readiness ready=%v err=%v", ready, readyErr)
	}
}

func TestNewProductionProtectedArgoReadinessRejectsEveryRootMismatch(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	_, validProbe, validConfig, _ := protectedArgoReadinessFixture(t, now)
	tests := []struct {
		name   string
		mutate func(**argo.ProductionDesiredStateReadinessProbe, *ProductionProtectedArgoReadinessConfig)
	}{
		{name: "nil probe", mutate: func(probe **argo.ProductionDesiredStateReadinessProbe, _ *ProductionProtectedArgoReadinessConfig) {
			*probe = nil
		}},
		{name: "nil readiness store", mutate: func(probe **argo.ProductionDesiredStateReadinessProbe, _ *ProductionProtectedArgoReadinessConfig) {
			(*probe).Store = nil
		}},
		{name: "platform binding mismatch", mutate: func(_ **argo.ProductionDesiredStateReadinessProbe, config *ProductionProtectedArgoReadinessConfig) {
			config.PlatformBindingID = id.New()
		}},
		{name: "Argo namespace mismatch", mutate: func(_ **argo.ProductionDesiredStateReadinessProbe, config *ProductionProtectedArgoReadinessConfig) {
			config.Application.ArgoNamespace = "other-argocd"
		}},
		{name: "root name mismatch", mutate: func(probe **argo.ProductionDesiredStateReadinessProbe, _ *ProductionProtectedArgoReadinessConfig) {
			identity := (*probe).Identity
			identity.RootApplicationName = "other-root"
			(*probe).Identity = identity
		}},
		{name: "repository credential mismatch", mutate: func(probe **argo.ProductionDesiredStateReadinessProbe, _ *ProductionProtectedArgoReadinessConfig) {
			identity := (*probe).Identity
			identity.RepositorySecretName = "other-repository"
			(*probe).Identity = identity
		}},
		{name: "invalid maximum age", mutate: func(probe **argo.ProductionDesiredStateReadinessProbe, _ *ProductionProtectedArgoReadinessConfig) {
			(*probe).MaxAge = time.Second
		}},
		{name: "publisher policy mismatch", mutate: func(_ **argo.ProductionDesiredStateReadinessProbe, config *ProductionProtectedArgoReadinessConfig) {
			config.Publisher.PolicyVersion = "other-policy"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			probeCopy := *validProbe
			probe, config := &probeCopy, validConfig
			test.mutate(&probe, &config)
			if value, err := NewProductionProtectedArgoReadiness(probe, config); !errors.Is(err, ErrInvalid) || value != nil {
				t.Fatalf("mismatched adapter=%#v err=%v", value, err)
			}
		})
	}
}
