package main

import (
	"errors"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/certissuers"
)

func workerCertificateIssuerConfig() certissuers.ObserverConfig {
	return certissuers.ObserverConfig{Enabled: true,
		BindingID: "11111111-1111-4111-8111-111111111111", ClusterID: "22222222-2222-4222-8222-222222222222",
		Namespace: "kuberploy-system", ServiceAccount: "kuberploy-worker", PollInterval: 30 * time.Second,
		RequestTimeout: 10 * time.Second, MaximumAge: 2 * time.Minute, ReadinessLease: 3 * time.Minute}
}

func TestCertificateIssuerWorkerIsStrictlyDefaultOff(t *testing.T) {
	store, err := openCertificateIssuerWorkerStore(t.Context(), "not-a-database-url", certissuers.ObserverConfig{})
	if err != nil || store != nil {
		t.Fatalf("store=%#v err=%v", store, err)
	}
	runtime, err := newCertificateIssuerRuntime(certissuers.ObserverConfig{}, "", nil, nil)
	if err != nil || runtime != nil {
		t.Fatalf("runtime=%#v err=%v", runtime, err)
	}
	if runtime, err = newCertificateIssuerRuntime(certissuers.ObserverConfig{}, "", &certissuers.PostgresStore{}, nil); runtime != nil || !errors.Is(err, certissuers.ErrObservationUnavailable) {
		t.Fatalf("dormant store runtime=%#v err=%v", runtime, err)
	}
}

func TestCertificateIssuerWorkerRequiresExactGitRuntime(t *testing.T) {
	config := workerCertificateIssuerConfig()
	for _, test := range []struct {
		name string
		host string
		git  *gitProjectionRuntime
	}{
		{name: "missing host"},
		{name: "missing git", host: "worker"},
		{name: "incomplete git", host: "worker", git: &gitProjectionRuntime{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, err := newCertificateIssuerRuntime(config, test.host, &certissuers.PostgresStore{}, test.git)
			if runtime != nil || !errors.Is(err, certissuers.ErrObservationUnavailable) {
				t.Fatalf("runtime=%#v err=%v", runtime, err)
			}
		})
	}
}
