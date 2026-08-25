package main

import (
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/certificates"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/secrets"
)

type certificateAPI struct {
	backend   httpapi.CertificateManagementBackend
	readiness httpapi.ReadinessProbe
	resolver  *certificates.PostgreSQLReferenceResolver
	pool      *pgxpool.Pool
}

type certificateAPIReadiness struct {
	runtimeSecrets httpapi.ReadinessProbe
	observations   httpapi.ReadinessProbe
}

func (p certificateAPIReadiness) Probe(ctx context.Context) error {
	if p.runtimeSecrets == nil || p.observations == nil {
		return certificates.ErrObservationUnavailable
	}
	if err := p.runtimeSecrets.Probe(ctx); err != nil {
		return certificates.ErrObservationUnavailable
	}
	if err := p.observations.Probe(ctx); err != nil {
		return certificates.ErrObservationUnavailable
	}
	return nil
}

func newCertificateAPI(
	ctx context.Context,
	databaseURL string,
	runtimeSecrets secrets.RuntimeConfig,
	observations certificates.ObservationConfig,
) (*certificateAPI, error) {
	if !observations.Enabled {
		return nil, nil
	}
	if err := validateCertificateRuntimeCoupling(runtimeSecrets, observations); err != nil {
		return nil, err
	}
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, certificates.ErrObservationUnavailable
	}
	poolConfig.ConnConfig.RuntimeParams["application_name"] = "kuberploy-certificate-api"
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
	certificateStore, err := certificates.NewPostgreSQLStore(pool)
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
	// Fail before exposing the plaintext-ingesting certificate lifecycle if
	// either fixed API projection cannot establish the strict runtime identity.
	if _, err = secrets.DefaultRuntimeIdentity(ctx, runtimeSecrets, time.Now().UTC()); err != nil {
		pool.Close()
		return nil, secrets.ErrRuntimeUnavailable
	}
	secretService := secrets.Service{Store: secretStore, Keys: keys, SealedSecrets: provider}
	service := certificates.Service{Secrets: secretService, Catalog: secretStore, Store: certificateStore}
	backend, err := httpapi.NewCertificateManagementBackend(service, secretStore)
	if err != nil {
		pool.Close()
		return nil, err
	}
	resolver, err := certificates.NewPostgreSQLReferenceResolver(certificateStore, observations)
	if err != nil {
		pool.Close()
		return nil, err
	}
	runtimeReadiness := &secrets.RuntimeReadinessProbe{
		Store: secretStore, Config: runtimeSecrets, MaxAge: secrets.RuntimeSecretHeartbeatMaxAge,
		ResolveIdentity: secrets.DefaultRuntimeIdentity,
	}
	observationReadiness := &certificates.ObservationReadinessProbe{
		Store: certificateStore, Config: observations, MaxAge: certificates.CertificateObservationHeartbeatMaxAge,
	}
	return &certificateAPI{
		backend: backend,
		readiness: certificateAPIReadiness{
			runtimeSecrets: runtimeReadiness,
			observations:   observationReadiness,
		},
		resolver: resolver,
		pool:     pool,
	}, nil
}

func validateCertificateRuntimeCoupling(runtimeSecrets secrets.RuntimeConfig, observations certificates.ObservationConfig) error {
	if runtimeSecrets.Validate() != nil || observations.Validate() != nil ||
		!slices.Equal(runtimeSecrets.Namespaces, observations.Namespaces) ||
		!slices.Equal(runtimeSecrets.NamespacePrefixes, observations.NamespacePrefixes) ||
		runtimeSecrets.SealingCertificateSecretRef == "" || runtimeSecrets.SealingCertificateSecretKey == "" {
		return certificates.ErrObservationUnavailable
	}
	if _, err := secrets.RuntimePolicyDigest(runtimeSecrets); err != nil {
		return certificates.ErrObservationUnavailable
	}
	if _, err := certificates.ObservationPolicyDigest(observations); err != nil {
		return certificates.ErrObservationUnavailable
	}
	return nil
}

func (a *certificateAPI) Close() {
	if a != nil && a.pool != nil {
		a.pool.Close()
		a.pool = nil
	}
}

var _ httpapi.ReadinessProbe = certificateAPIReadiness{}
