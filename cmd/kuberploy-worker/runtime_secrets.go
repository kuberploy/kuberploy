package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/kuberploy/kuberploy/internal/secrets"
)

type runtimeSecretStore interface {
	secrets.RuntimeStore
	Close()
}

type runtimeSecretRuntime struct {
	controller *secrets.RuntimeController
	store      runtimeSecretStore
}

func newRuntimeSecretRuntime(ctx context.Context, databaseURL, host string, config secrets.RuntimeConfig) (*runtimeSecretRuntime, error) {
	if !config.Enabled {
		return nil, nil
	}
	if config.Validate() != nil {
		return nil, secrets.ErrRuntimeUnavailable
	}
	store, err := secrets.OpenPostgreSQLStore(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	observer, err := secrets.NewInClusterStrictSealedSecretsProvider()
	if err != nil {
		store.Close()
		return nil, err
	}
	runtime, err := buildRuntimeSecretRuntime(ctx, host, config, store, observer, secrets.WorkerRuntimeIdentity, time.Now().UTC())
	if err != nil {
		store.Close()
		return nil, err
	}
	return runtime, nil
}

func buildRuntimeSecretRuntime(
	ctx context.Context,
	host string,
	config secrets.RuntimeConfig,
	store runtimeSecretStore,
	observer secrets.StrictSealedSecretsObserver,
	resolveIdentity func(context.Context, secrets.RuntimeConfig, time.Time) (secrets.RuntimeIdentity, error),
	now time.Time,
) (*runtimeSecretRuntime, error) {
	if config.Validate() != nil || store == nil || observer == nil || resolveIdentity == nil || now.IsZero() {
		return nil, secrets.ErrRuntimeUnavailable
	}
	identity, err := resolveIdentity(ctx, config, now.UTC())
	if err != nil {
		return nil, secrets.ErrRuntimeUnavailable
	}
	controller := &secrets.RuntimeController{
		Store: store, Observer: observer, Config: config, Identity: identity,
		WorkerID: "runtime-secrets-worker:" + host + ":" + strconv.Itoa(os.Getpid()), ResolveIdentity: resolveIdentity,
		ReportError: func(loop string, err error) {
			slog.Warn("runtime-secret worker iteration failed", "loop", loop, "error", err)
		},
	}
	if err = controller.ValidateRuntime(); err != nil {
		return nil, err
	}
	return &runtimeSecretRuntime{controller: controller, store: store}, nil
}

func (r *runtimeSecretRuntime) Run(ctx context.Context) error {
	if r == nil || r.controller == nil || r.store == nil {
		return fmt.Errorf("runtime-secret runtime is not configured")
	}
	return r.controller.Run(ctx)
}

func (r *runtimeSecretRuntime) Close() {
	if r != nil && r.store != nil {
		r.store.Close()
	}
}
