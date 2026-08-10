package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/kuberploy/kuberploy/internal/certissuers"
)

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
	ticker := time.NewTicker(r.poll)
	defer ticker.Stop()
	for {
		if _, err := r.controller.Reconcile(ctx, certissuers.MaximumObservedIssuers); err != nil {
			slog.Warn("certificate issuer protected publication failed", "error", err)
		} else if err = r.observer.RunOnce(ctx); err != nil {
			slog.Warn("certificate issuer live observation failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (r *certificateIssuerRuntime) Close() {
	if r != nil && r.store != nil {
		r.store.Close()
	}
}
