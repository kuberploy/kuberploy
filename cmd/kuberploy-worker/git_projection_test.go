package main

import (
	"context"
	"errors"
	"testing"

	"github.com/kuberploy/kuberploy/internal/certificates"
	"github.com/kuberploy/kuberploy/internal/certissuers"
	"github.com/kuberploy/kuberploy/internal/edge"
	"github.com/kuberploy/kuberploy/internal/externaldns"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/imagepull"
	"github.com/kuberploy/kuberploy/internal/secrets"
)

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
