package main

import (
	"context"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/argo"
	"github.com/kuberploy/kuberploy/internal/helmapps"
	"github.com/kuberploy/kuberploy/internal/id"
)

type helmApplicationsAPI struct {
	pool      *pgxpool.Pool
	runtime   *helmapps.APIRuntime
	approvals *helmapps.ApprovalAdmissionService
	previews  *helmapps.PostgresRenderedManifestPreviewService
}

func newHelmApplicationsAPI(ctx context.Context, databaseURL string, projection *gitProjectionAPI,
	argoRuntime *argoDesiredStateAPI) (*helmApplicationsAPI, error) {
	return newHelmApplicationsAPIFromLookup(ctx, databaseURL, projection, argoRuntime, os.LookupEnv)
}

func newHelmApplicationsAPIFromLookup(ctx context.Context, databaseURL string, projection *gitProjectionAPI,
	argoRuntime *argoDesiredStateAPI, lookup func(string) (string, bool)) (*helmApplicationsAPI, error) {
	config, err := protectedHelmRuntimeConfigForAPI(projection, lookup)
	if err != nil {
		return nil, err
	}
	if !config.Enabled {
		return nil, nil
	}
	identity, err := validateHelmAPIAuthorities(config, projection, argoRuntime)
	if err != nil {
		return nil, err
	}
	credentials, err := helmapps.RuntimeOCICredentialProvider(ctx, config)
	if err != nil {
		return nil, err
	}
	readiness, err := helmapps.NewProductionProtectedArgoReadiness(argoRuntime.readiness,
		helmapps.ProductionProtectedArgoReadinessConfig{
			PlatformBindingID: identity.PlatformBindingID, ClusterID: identity.ClusterID,
			Application: config.Application, Publisher: config.Publisher,
		})
	if err != nil {
		return nil, err
	}
	pool, err := openHelmAPIPool(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	runtime, err := helmapps.NewAPIRuntime(config, helmapps.APIRuntimeDependencies{
		Pool: pool, Argo: readiness, Now: func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		pool.Close()
		return nil, err
	}
	admissionStore, err := helmapps.NewPostgresApprovalAdmissionStore(pool)
	if err != nil {
		pool.Close()
		return nil, err
	}
	client := &http.Client{Timeout: config.OCIRequestTimeout}
	packages := &helmapps.CachedChartPackageSource{Upstream: helmapps.OCIHTTPPackageSource{
		Client: client, AllowedRegistryHosts: append([]string(nil), config.OCIRegistryHosts...),
		AllowedAuthHosts:     append([]string(nil), config.OCIAuthHosts...),
		AllowedRedirectHosts: append([]string(nil), config.OCIRedirectHosts...), Credentials: credentials}, MaxBytes: config.PackageCacheBytes}
	approvals := &helmapps.ApprovalAdmissionService{Store: admissionStore, Packages: packages,
		Now: func() time.Time { return time.Now().UTC() }, NewID: id.New}
	if approvals.Validate() != nil {
		pool.Close()
		return nil, helmapps.ErrInvalid
	}
	previews, err := helmapps.NewPostgresRenderedManifestPreviewService(pool)
	if err != nil {
		pool.Close()
		return nil, err
	}
	return &helmApplicationsAPI{pool: pool, runtime: runtime, approvals: approvals, previews: previews}, nil
}

func protectedHelmRuntimeConfigForAPI(projection *gitProjectionAPI,
	lookup func(string) (string, bool)) (helmapps.RuntimeConfig, error) {
	if projection == nil || projection.readiness == nil || projection.readiness.Identity.Validate() != nil {
		config, err := helmapps.RuntimeConfigFromLookup(lookup, helmapps.ProtectedPublisherIdentity{})
		return config, err
	}
	baseDigest := projection.readiness.Identity.ConfigDigest
	seed := helmapps.ProtectedPublisherIdentity{Contract: helmapps.ProtectedPublisherContract,
		PolicyVersion: helmapps.ProtectedGitPolicy, ConfigDigest: baseDigest}
	config, err := helmapps.RuntimeConfigFromLookup(lookup, seed)
	if err != nil || !config.Enabled {
		return config, err
	}
	publisher, err := helmapps.ProtectedPublisherIdentityForRuntime(projection.readiness.Identity, config)
	if err != nil {
		return helmapps.RuntimeConfig{}, err
	}
	config.Publisher = publisher
	return config, config.Validate()
}

func validateHelmAPIAuthorities(config helmapps.RuntimeConfig, projection *gitProjectionAPI,
	argoRuntime *argoDesiredStateAPI) (argo.DesiredStateRuntimeIdentity, error) {
	if config.Validate() != nil || !config.Enabled || projection == nil || projection.store == nil ||
		projection.backend == nil || projection.readiness == nil || projection.readiness.Store == nil ||
		projection.readiness.Identity.Validate() != nil ||
		argoRuntime == nil || argoRuntime.store == nil || argoRuntime.readiness == nil ||
		argoRuntime.readiness.Store == nil {
		return argo.DesiredStateRuntimeIdentity{}, helmapps.ErrInvalid
	}
	expectedPublisher, err := helmapps.ProtectedPublisherIdentityForRuntime(
		projection.readiness.Identity, config)
	if err != nil || config.Publisher != expectedPublisher {
		return argo.DesiredStateRuntimeIdentity{}, helmapps.ErrInvalid
	}
	identity := argoRuntime.readiness.Identity
	if identity.Validate() != nil || identity.GitHubAppID != projection.readiness.Identity.GitHubAppID ||
		identity.ArgoNamespace != config.Application.ArgoNamespace ||
		identity.RootApplicationName != argo.PlatformRootApplicationName ||
		identity.Runtime.ChartName != argo.RuntimeChartName ||
		identity.DigestEnforcement != argo.ChartDigestNativeOCI {
		return argo.DesiredStateRuntimeIdentity{}, helmapps.ErrInvalid
	}
	return identity, nil
}

func openHelmAPIPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, helmapps.ErrInvalid
	}
	config.ConnConfig.RuntimeParams["application_name"] = "kuberploy-helm-applications-api"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

func (a *helmApplicationsAPI) Close() {
	if a != nil && a.pool != nil {
		a.pool.Close()
	}
}
