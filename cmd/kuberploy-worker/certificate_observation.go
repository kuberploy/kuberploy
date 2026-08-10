package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/certificates"
	"github.com/kuberploy/kuberploy/internal/secrets"
)

type certificateObservationRuntime struct {
	controller *certificates.ObservationController
	resolver   *certificates.PostgreSQLReferenceResolver
	close      func()
}

func newCertificateObservationRuntime(
	ctx context.Context,
	databaseURL, host string,
	config certificates.ObservationConfig,
) (*certificateObservationRuntime, error) {
	if !config.Enabled {
		return nil, nil
	}
	if config.Validate() != nil {
		return nil, certificates.ErrObservationUnavailable
	}
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, certificates.ErrObservationUnavailable
	}
	poolConfig.ConnConfig.RuntimeParams["application_name"] = "kuberploy-certificate-observer"
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, err
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	store, err := certificates.NewPostgreSQLStore(pool)
	if err != nil {
		pool.Close()
		return nil, err
	}
	observer, err := secrets.NewInClusterStrictSealedSecretsProvider()
	if err != nil {
		pool.Close()
		return nil, err
	}
	workerID := "certificate-observer-worker:" + host + ":" + strconv.Itoa(os.Getpid())
	runtime, err := buildCertificateObservationRuntime(config, store, observer, workerID, pool.Close)
	if err != nil {
		pool.Close()
		return nil, err
	}
	resolver, err := certificates.NewPostgreSQLReferenceResolver(store, config)
	if err != nil {
		pool.Close()
		return nil, err
	}
	runtime.resolver = resolver
	return runtime, nil
}

func buildCertificateObservationRuntime(
	config certificates.ObservationConfig,
	store certificates.ObservationStore,
	observer certificates.StrictCertificateObserver,
	workerID string,
	close func(),
) (*certificateObservationRuntime, error) {
	identity, err := certificates.ObservationIdentityForConfig(config)
	if err != nil || store == nil || observer == nil || close == nil {
		return nil, certificates.ErrObservationUnavailable
	}
	controller := &certificates.ObservationController{
		Store: store, Observer: observer, Config: config, Identity: identity, WorkerID: workerID,
		ReportError: func(loop string, err error) {
			slog.Warn("certificate observation worker iteration failed", "loop", loop, "error", err)
		},
	}
	if err = controller.ValidateRuntime(); err != nil {
		return nil, err
	}
	return &certificateObservationRuntime{controller: controller, close: close}, nil
}

func (r *certificateObservationRuntime) Run(ctx context.Context) error {
	if r == nil || r.controller == nil || r.close == nil {
		return fmt.Errorf("certificate observation runtime is not configured")
	}
	return r.controller.Run(ctx)
}

func (r *certificateObservationRuntime) Close() {
	if r != nil && r.close != nil {
		r.close()
		r.close = nil
	}
}

var _ interface {
	Run(context.Context) error
	Close()
} = (*certificateObservationRuntime)(nil)
