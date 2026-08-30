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
	controller certificateIssuerController
	observer   certificateIssuerObserver
	poll       time.Duration
}

type certificateIssuerController interface {
	Reconcile(context.Context, int) (certissuers.ProtectedControllerResult, error)
}

type certificateIssuerObserver interface {
	Validate() error
	RunOnce(context.Context) error
	RefreshPreviouslyReadyOnce(context.Context) error
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

// Run publishes before normal observation. If publication conflicts, only
// previously ready exact revisions may refresh; pending or revised issuers
// still wait for successful protected Git publication.
func (r *certificateIssuerRuntime) Run(ctx context.Context) error {
	if r == nil || r.store == nil || r.controller == nil || r.observer == nil || r.observer.Validate() != nil || r.poll <= 0 {
		return certissuers.ErrObservationUnavailable
	}
	for {
		next := r.runCycle(ctx, time.Now().UTC())
		if err := waitCertificateIssuerCycle(ctx, next); err != nil {
			return err
		}
	}
}

func (r *certificateIssuerRuntime) runCycle(ctx context.Context, now time.Time) time.Duration {
	next := r.poll
	if _, err := r.controller.Reconcile(ctx, certissuers.MaximumObservedIssuers); err != nil {
		slog.Warn("certificate issuer protected publication failed", "error", err)
		next = certificateIssuerProviderDelay(err, now, r.poll)
		if observeErr := r.observer.RefreshPreviouslyReadyOnce(ctx); observeErr != nil {
			slog.Warn("certificate issuer retained observation refresh failed", "error", observeErr)
		}
		return next
	}
	if err := r.observer.RunOnce(ctx); err != nil {
		slog.Warn("certificate issuer live observation failed", "error", err)
	}
	return next
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
