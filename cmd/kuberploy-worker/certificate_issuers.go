package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/kuberploy/kuberploy/internal/certissuers"
	"github.com/kuberploy/kuberploy/internal/githubapp"
)

const maximumCertificateIssuerProviderWait = 7 * 24 * time.Hour

type certificateIssuerRuntime struct {
	store      *certissuers.PostgresStore
	controller *certissuers.ProtectedController
	observer   *certissuers.ObserverRuntime
	poll       time.Duration
}

func openCertificateIssuerWorkerStore(ctx context.Context, databaseURL string, config certissuers.ObserverConfig) (*certissuers.PostgresStore, error) {
	if config.Validate() != nil {
		return nil, certissuers.ErrObservationUnavailable
	}
	if !config.Enabled {
		return nil, nil
	}
	return certissuers.OpenPostgresStore(ctx, databaseURL)
}

func newCertificateIssuerRuntime(config certissuers.ObserverConfig, host string, store *certissuers.PostgresStore, git *gitProjectionRuntime) (*certificateIssuerRuntime, error) {
	if config.Validate() != nil {
		return nil, certissuers.ErrObservationUnavailable
	}
	if !config.Enabled {
		if store != nil {
			return nil, certissuers.ErrObservationUnavailable
		}
		return nil, nil
	}
	if store == nil || git == nil || git.store == nil || git.headVerifier.Client == nil || git.writeManager == nil || host == "" {
		return nil, certissuers.ErrObservationUnavailable
	}
	identity, err := certissuers.ObserverIdentityForConfig(config)
	if err != nil {
		return nil, err
	}
	reader, err := certissuers.NewInClusterClusterIssuerReader(config)
	if err != nil {
		return nil, err
	}
	owner := workerLeaseOwner(host, "certificate-issuers")
	protectedConfig, err := certissuers.ProtectedGitConfigForObserver(owner, config)
	if err != nil {
		return nil, err
	}
	publisher, err := certissuers.NewProtectedGitPublisher(git.store, git.headVerifier, git.writeManager, protectedConfig, func() time.Time { return time.Now().UTC() })
	if err != nil {
		return nil, err
	}
	startedAt := time.Now().UTC()
	observer := &certissuers.ObserverRuntime{Store: store, ReadinessStore: store, Reader: reader, Config: config,
		Identity: identity, WorkerID: owner, StartedAt: startedAt, Now: func() time.Time { return time.Now().UTC() }}
	if observer.Validate() != nil {
		return nil, certissuers.ErrObservationUnavailable
	}
	return &certificateIssuerRuntime{store: store, controller: &certissuers.ProtectedController{Store: store, Publisher: publisher},
		observer: observer, poll: config.PollInterval}, nil
}

// Run sequences publication before live observation. Therefore the durable
// observer lease is refreshed only after both protected Git publication and
// exact live ClusterIssuer verification succeed for the current target set.
func (r *certificateIssuerRuntime) Run(ctx context.Context) error {
	if r == nil || r.store == nil || r.controller == nil || r.observer == nil || r.observer.Validate() != nil || r.poll <= 0 {
		return certissuers.ErrObservationUnavailable
	}
	for {
		next := r.poll
		if _, err := r.controller.Reconcile(ctx, certissuers.MaximumObservedIssuers); err != nil {
			slog.Warn("certificate issuer protected publication failed", "error", err)
			next = certificateIssuerProviderDelay(err, time.Now().UTC(), r.poll)
		} else if err = r.observer.RunOnce(ctx); err != nil {
			slog.Warn("certificate issuer live observation failed", "error", err)
		}
		if err := waitCertificateIssuerCycle(ctx, next); err != nil {
			return err
		}
	}
}

func certificateIssuerProviderDelay(err error, now time.Time, poll time.Duration) time.Duration {
	if poll <= 0 {
		return 0
	}
	delay := poll
	var providerError *githubapp.APIError
	if !errors.As(err, &providerError) || !providerError.Retryable() || !providerError.RetryAt.After(now.Add(delay)) {
		return delay
	}
	delay = providerError.RetryAt.Sub(now)
	if delay > maximumCertificateIssuerProviderWait {
		return maximumCertificateIssuerProviderWait
	}
	return delay
}

func waitCertificateIssuerCycle(ctx context.Context, delay time.Duration) error {
	if ctx == nil || delay <= 0 {
		return certissuers.ErrObservationUnavailable
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (r *certificateIssuerRuntime) Close() {
	if r != nil && r.store != nil {
		r.store.Close()
	}
}
