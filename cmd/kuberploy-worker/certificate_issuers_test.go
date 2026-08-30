package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/certissuers"
	"github.com/kuberploy/kuberploy/internal/githubapp"
)

type failingCertificateIssuerController struct{ err error }

func (c failingCertificateIssuerController) Reconcile(context.Context, int) (certissuers.ProtectedControllerResult, error) {
	return certissuers.ProtectedControllerResult{}, c.err
}

type recordingCertificateIssuerObserver struct {
	runCalls     int
	refreshCalls int
	refreshErr   error
}

func (o *recordingCertificateIssuerObserver) Validate() error { return nil }
func (o *recordingCertificateIssuerObserver) RunOnce(context.Context) error {
	o.runCalls++
	return nil
}
func (o *recordingCertificateIssuerObserver) RefreshPreviouslyReadyOnce(context.Context) error {
	o.refreshCalls++
	return o.refreshErr
}

func workerCertificateIssuerConfig() certissuers.ObserverConfig {
	return certissuers.ObserverConfig{Enabled: true,
		BindingID: "11111111-1111-4111-8111-111111111111", Namespace: "kuberploy-system", ServiceAccount: "kuberploy-worker", PollInterval: 30 * time.Second,
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

func TestCertificateIssuerPublicationConflictRefreshesRetainedObservations(t *testing.T) {
	observer := &recordingCertificateIssuerObserver{}
	runtime := &certificateIssuerRuntime{controller: failingCertificateIssuerController{err: certissuers.ErrConflict},
		observer: observer, poll: 30 * time.Second}
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	if delay := runtime.runCycle(t.Context(), now); delay != runtime.poll {
		t.Fatalf("delay=%s want=%s", delay, runtime.poll)
	}
	if observer.refreshCalls != 1 || observer.runCalls != 0 {
		t.Fatalf("observer calls: refresh=%d normal=%d", observer.refreshCalls, observer.runCalls)
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
