package main

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/registrycredentials"
	"github.com/kuberploy/kuberploy/internal/secrets"
)

type registryCredentialAPI struct {
	backend httpapi.RegistryCredentialManagementBackend
	pool    *pgxpool.Pool
}

func newRegistryCredentialAPI(ctx context.Context, databaseURL string, config secrets.RuntimeConfig) (*registryCredentialAPI, error) {
	if !config.Enabled {
		return nil, nil
	}
	if config.Validate() != nil {
		return nil, secrets.ErrRuntimeUnavailable
	}
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, registrycredentials.ErrUnavailable
	}
	poolConfig.ConnConfig.RuntimeParams["application_name"] = "kuberploy-registry-credential-api"
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, err
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	secretStore, err := secrets.NewPostgreSQLStore(pool)
	if err != nil {
		pool.Close()
		return nil, err
	}
	credentialStore, err := registrycredentials.NewPostgreSQLStore(pool)
	if err != nil {
		pool.Close()
		return nil, err
	}
	provider, err := secrets.NewInClusterStrictSealedSecretsProvider()
	if err != nil {
		pool.Close()
		return nil, err
	}
	keys := secrets.NewDefaultProjectedFingerprintKeyProvider()
	// Fail before exposing the plaintext-ingesting registry-credential
	// lifecycle if the fixed API projection cannot establish the strict
	// runtime identity, mirroring the certificate and runtime-secret APIs.
	if _, err = secrets.DefaultRuntimeIdentity(ctx, config, time.Now().UTC()); err != nil {
		pool.Close()
		return nil, secrets.ErrRuntimeUnavailable
	}
	secretService := secrets.Service{Store: secretStore, Keys: keys, SealedSecrets: provider}
	service := registrycredentials.Service{Secrets: secretService, Catalog: secretStore, Store: credentialStore}
	backend, err := httpapi.NewRegistryCredentialManagementBackend(service, secretStore)
	if err != nil {
		pool.Close()
		return nil, err
	}
	return &registryCredentialAPI{backend: backend, pool: pool}, nil
}

func (a *registryCredentialAPI) Close() {
	if a != nil && a.pool != nil {
		a.pool.Close()
		a.pool = nil
	}
}
