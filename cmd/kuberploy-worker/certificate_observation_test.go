package main

import (
	"context"
	"errors"
	"testing"

	"github.com/kuberploy/kuberploy/internal/certificates"
	"github.com/kuberploy/kuberploy/internal/secrets"
)

type workerCertificateObserver struct{}

func (workerCertificateObserver) ObserveStrictSealedSecret(context.Context, secrets.Artifact) (secrets.ReadinessObservation, error) {
	return secrets.ReadinessObservation{}, secrets.ErrProviderOperation
}

func workerCertificateConfig() certificates.ObservationConfig {
	config := certificates.DefaultObservationConfig()
	config.Enabled = true
	config.Namespaces = []string{"payments-production"}
	return config
}

func TestCertificateObservationRuntimeIsDefaultOffAndValidatesBeforeOpeningPostgreSQL(t *testing.T) {
	runtime, err := newCertificateObservationRuntime(t.Context(), "not-a-database-url", "worker", certificates.ObservationConfig{})
	if err != nil || runtime != nil {
		t.Fatalf("runtime=%#v err=%v", runtime, err)
	}
	if _, err = newCertificateObservationRuntime(t.Context(), "not-a-database-url", "worker", certificates.ObservationConfig{Enabled: true}); !errors.Is(err, certificates.ErrObservationUnavailable) {
		t.Fatalf("invalid enabled config error=%v", err)
	}
}

func TestBuildCertificateObservationRuntimeUsesExactIdentityAndReadOnlyObserver(t *testing.T) {
	store := certificates.NewObservationMemoryStore()
	closed := 0
	runtime, err := buildCertificateObservationRuntime(workerCertificateConfig(), store, workerCertificateObserver{}, "certificate-observer-worker:test:123", func() { closed++ })
	if err != nil {
		t.Fatal(err)
	}
	want, err := certificates.ObservationIdentityForConfig(workerCertificateConfig())
	if err != nil || runtime.controller.Identity != want || runtime.controller.Observer == nil {
		t.Fatalf("controller=%#v wantIdentity=%#v err=%v", runtime.controller, want, err)
	}
	runtime.Close()
	runtime.Close()
	if closed != 1 {
		t.Fatalf("close count=%d", closed)
	}
}
