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

func apiCertificateRuntimeSecrets() secrets.RuntimeConfig {
	config := secrets.DefaultRuntimeConfig()
	config.Enabled = true
	config.Namespaces = []string{"payments-production"}
	config.FingerprintSecretRef = "runtime-secret-fingerprint"
	return config
}

func apiCertificateObservation() certificates.ObservationConfig {
	config := certificates.DefaultObservationConfig()
	config.Enabled = true
	config.Namespaces = []string{"payments-production"}
	return config
}

func TestCertificateAPIIsDefaultOffAndValidatesCouplingBeforePostgreSQL(t *testing.T) {
	api, err := newCertificateAPI(t.Context(), "not-a-database-url", secrets.RuntimeConfig{}, certificates.ObservationConfig{})
	if err != nil || api != nil {
		t.Fatalf("api=%#v err=%v", api, err)
	}
	if _, err = newCertificateAPI(t.Context(), "not-a-database-url", secrets.RuntimeConfig{}, apiCertificateObservation()); !errors.Is(err, certificates.ErrObservationUnavailable) {
		t.Fatalf("missing runtime-secrets error=%v", err)
	}
	runtimeSecrets := apiCertificateRuntimeSecrets()
	observation := apiCertificateObservation()
	observation.Namespaces = []string{"other-production"}
	if _, err = newCertificateAPI(t.Context(), "not-a-database-url", runtimeSecrets, observation); !errors.Is(err, certificates.ErrObservationUnavailable) {
		t.Fatalf("namespace drift error=%v", err)
	}
	observation = apiCertificateObservation()
	runtimeSecrets.FingerprintSecretRef = ""
	if _, err = newCertificateAPI(t.Context(), "not-a-database-url", runtimeSecrets, observation); !errors.Is(err, certificates.ErrObservationUnavailable) {
		t.Fatalf("invalid runtime-secret contract error=%v", err)
	}
}

type testReadinessProbe struct{ err error }

func (p testReadinessProbe) Probe(_ context.Context) error { return p.err }

func TestCertificateAPIReadinessRequiresRuntimeSecretsAndObservation(t *testing.T) {
	ready := certificateAPIReadiness{runtimeSecrets: testReadinessProbe{}, observations: testReadinessProbe{}}
	if err := ready.Probe(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, probe := range []certificateAPIReadiness{
		{},
		{runtimeSecrets: testReadinessProbe{}, observations: testReadinessProbe{err: errors.New("stale")}},
		{runtimeSecrets: testReadinessProbe{err: errors.New("stale")}, observations: testReadinessProbe{}},
	} {
		if err := probe.Probe(t.Context()); !errors.Is(err, certificates.ErrObservationUnavailable) {
			t.Fatalf("probe error=%v", err)
		}
	}
}

func TestGitProjectionAPIRejectsCertificateObservationWhenProjectionIsDisabled(t *testing.T) {
	api, err := newGitProjectionAPI(t.Context(), "not-a-database-url", gitprojection.RuntimeConfig{}, apiCertificateRuntimeSecrets(), apiCertificateObservation(), certissuers.ObserverConfig{}, imagepull.RuntimeConfig{}, edge.RuntimeConfig{}, externaldns.OperationalConfig{}, nil)
	if api != nil || !errors.Is(err, gitprojection.ErrInvalid) {
		t.Fatalf("api=%#v err=%v", api, err)
	}
	dormant := certificates.DefaultObservationConfig()
	api, err = newGitProjectionAPI(t.Context(), "not-a-database-url", gitprojection.RuntimeConfig{}, secrets.RuntimeConfig{}, dormant, certissuers.ObserverConfig{}, imagepull.RuntimeConfig{}, edge.RuntimeConfig{}, externaldns.OperationalConfig{}, nil)
	if api != nil || !errors.Is(err, certificates.ErrObservationUnavailable) {
		t.Fatalf("dormant api=%#v err=%v", api, err)
	}
}
