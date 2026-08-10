package main

import (
	"context"
	"time"

	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/secrets"
)

type runtimeSecretAPIStore interface {
	secrets.Store
	secrets.RuntimeStore
	Close()
}

type runtimeSecretAPI struct {
	backend   httpapi.RuntimeSecretBackend
	readiness httpapi.ReadinessProbe
	store     runtimeSecretAPIStore
}

func newRuntimeSecretAPI(ctx context.Context, databaseURL string, config secrets.RuntimeConfig) (*runtimeSecretAPI, error) {
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
	provider, err := secrets.NewInClusterStrictSealedSecretsProvider()
	if err != nil {
		store.Close()
		return nil, err
	}
	api, err := buildRuntimeSecretAPI(ctx, config, store, provider, secrets.NewDefaultProjectedFingerprintKeyProvider(), secrets.DefaultRuntimeIdentity, time.Now().UTC())
	if err != nil {
		store.Close()
		return nil, err
	}
	return api, nil
}

func buildRuntimeSecretAPI(
	ctx context.Context,
	config secrets.RuntimeConfig,
	store runtimeSecretAPIStore,
	provider secrets.SealedSecretsProvider,
	keys secrets.FingerprintKeyProvider,
	resolveIdentity func(context.Context, secrets.RuntimeConfig, time.Time) (secrets.RuntimeIdentity, error),
	now time.Time,
) (*runtimeSecretAPI, error) {
	if config.Validate() != nil || store == nil || provider == nil || keys == nil || resolveIdentity == nil || now.IsZero() {
		return nil, secrets.ErrRuntimeUnavailable
	}
	// Validate both fixed projections before making a plaintext-ingesting API
	// handler reachable. The readiness probe repeats this check on every call.
	if _, err := resolveIdentity(ctx, config, now.UTC()); err != nil {
		return nil, secrets.ErrRuntimeUnavailable
	}
	backend, err := httpapi.NewRuntimeSecretBackend(secrets.Service{Store: store, Keys: keys, SealedSecrets: provider}, config)
	if err != nil {
		return nil, err
	}
	probe := &secrets.RuntimeReadinessProbe{Store: store, Config: config, MaxAge: secrets.RuntimeSecretHeartbeatMaxAge, ResolveIdentity: resolveIdentity}
	return &runtimeSecretAPI{backend: backend, readiness: probe, store: store}, nil
}

func (a *runtimeSecretAPI) Close() {
	if a != nil && a.store != nil {
		a.store.Close()
	}
}
