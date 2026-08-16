package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/certissuers"
	"github.com/kuberploy/kuberploy/internal/githubapp"
)

func workerCertificateIssuerConfig() certissuers.ObserverConfig {
	return certissuers.ObserverConfig{Enabled: true,
		BindingID: "11111111-1111-4111-8111-111111111111", ClusterID: "22222222-2222-4222-8222-222222222222",
		Namespace: "kuberploy-system", ServiceAccount: "kuberploy-worker", PollInterval: 30 * time.Second,
		RequestTimeout: 10 * time.Second, MaximumAge: 2 * time.Minute, ReadinessLease: 3 * time.Minute}
}

func TestCertificateIssuerProviderRetryHonorsRetryAtAndCancellation(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	poll := 30 * time.Second
	for name, testCase := range map[string]struct {
		err  error
		want time.Duration
	}{
		"rate limit":            {err: &githubapp.APIError{Class: githubapp.APIErrorRateLimit, RetryAt: now.Add(2 * time.Minute)}, want: 2 * time.Minute},
		"transient before poll": {err: &githubapp.APIError{Class: githubapp.APIErrorTransient, RetryAt: now.Add(10 * time.Second)}, want: poll},
		"non-retryable":         {err: &githubapp.APIError{Class: githubapp.APIErrorForbidden, RetryAt: now.Add(2 * time.Minute)}, want: poll},
		"bounded provider hint": {err: &githubapp.APIError{Class: githubapp.APIErrorRateLimit, RetryAt: now.Add(8 * 24 * time.Hour)}, want: maximumCertificateIssuerProviderWait},
	} {
		t.Run(name, func(t *testing.T) {
			if got := certificateIssuerProviderDelay(testCase.err, now, poll); got != testCase.want {
				t.Fatalf("delay=%s want=%s", got, testCase.want)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := waitCertificateIssuerCycle(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait error=%v", err)
	}
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
