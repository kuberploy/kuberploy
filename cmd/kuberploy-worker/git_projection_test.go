package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/certificates"
	"github.com/kuberploy/kuberploy/internal/certissuers"
	"github.com/kuberploy/kuberploy/internal/edge"
	"github.com/kuberploy/kuberploy/internal/externaldns"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/imagepull"
	"github.com/kuberploy/kuberploy/internal/secrets"
)

func TestPublicationObservationDoesNotInheritSlowProjectionSafetyPoll(t *testing.T) {
	if got := publicationObservationInterval(5 * time.Minute); got != 5*time.Second {
		t.Fatalf("publication observation interval=%s", got)
	}
	if got := publicationObservationInterval(2 * time.Second); got != 2*time.Second {
		t.Fatalf("short publication observation interval=%s", got)
	}
}

func TestNewGitProjectionRuntimeDisabledDoesNotOpenDependencies(t *testing.T) {
	runtime, err := newGitProjectionRuntime(context.Background(), "not-a-database-url", "", gitprojection.RuntimeConfig{}, secrets.RuntimeConfig{}, certificates.ObservationConfig{}, certissuers.ObserverConfig{}, imagepull.RuntimeConfig{}, edge.RuntimeConfig{}, externaldns.OperationalConfig{}, nil, nil)
	if err != nil || runtime != nil {
		t.Fatalf("disabled runtime=%#v err=%v", runtime, err)
	}
}

func TestNewGitProjectionRuntimeRejectsInvalidEnabledConfigBeforeOpeningDatabase(t *testing.T) {
	runtime, err := newGitProjectionRuntime(context.Background(), "not-a-database-url", "worker", gitprojection.RuntimeConfig{Enabled: true}, secrets.RuntimeConfig{}, certificates.ObservationConfig{}, certissuers.ObserverConfig{}, imagepull.RuntimeConfig{}, edge.RuntimeConfig{}, externaldns.OperationalConfig{}, nil, nil)
	if err == nil || runtime != nil {
		t.Fatalf("invalid runtime=%#v err=%v", runtime, err)
	}
}

func TestNewGitProjectionRuntimeRejectsCertificatePolicyWithoutExactResolver(t *testing.T) {
	certificateConfig := workerCertificateConfig()
	runtime, err := newGitProjectionRuntime(context.Background(), "not-a-database-url", "worker", gitprojection.RuntimeConfig{}, secrets.RuntimeConfig{}, certificateConfig, certissuers.ObserverConfig{}, imagepull.RuntimeConfig{}, edge.RuntimeConfig{}, externaldns.OperationalConfig{}, nil, nil)
	if runtime != nil || !errors.Is(err, certificates.ErrObservationUnavailable) {
		t.Fatalf("certificate runtime=%#v err=%v", runtime, err)
	}
	runtime, err = newGitProjectionRuntime(context.Background(), "not-a-database-url", "worker", gitprojection.RuntimeConfig{}, secrets.RuntimeConfig{}, certificates.ObservationConfig{}, certissuers.ObserverConfig{}, imagepull.RuntimeConfig{}, edge.RuntimeConfig{}, externaldns.OperationalConfig{}, &certificates.PostgreSQLReferenceResolver{}, nil)
	if runtime != nil || !errors.Is(err, certificates.ErrObservationUnavailable) {
		t.Fatalf("dormant resolver runtime=%#v err=%v", runtime, err)
	}
}

func TestEdgeRoutePolicyKeepsDisabledCertificateResolverNil(t *testing.T) {
	policy := newEdgeRouteReferencePolicy(edge.RuntimeConfig{}, externaldns.OperationalConfig{}, nil, certissuers.ObserverConfig{}, nil)
	if policy.Certificates != nil {
		t.Fatal("disabled certificate resolver became a non-nil interface")
	}
	resolver := &certificates.PostgreSQLReferenceResolver{}
	policy = newEdgeRouteReferencePolicy(edge.RuntimeConfig{}, externaldns.OperationalConfig{}, resolver, certissuers.ObserverConfig{}, nil)
	if policy.Certificates != resolver {
		t.Fatal("enabled certificate resolver was not installed")
	}
}
