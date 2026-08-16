package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kuberploy/kuberploy/internal/appconfigpreview"
	"github.com/kuberploy/kuberploy/internal/argo"
	"github.com/kuberploy/kuberploy/internal/autodeploy"
	"github.com/kuberploy/kuberploy/internal/buildlogs"
	"github.com/kuberploy/kuberploy/internal/buildpromotion"
	"github.com/kuberploy/kuberploy/internal/builds"
	"github.com/kuberploy/kuberploy/internal/certificates"
	"github.com/kuberploy/kuberploy/internal/certissuers"
	"github.com/kuberploy/kuberploy/internal/config"
	"github.com/kuberploy/kuberploy/internal/deploymentrollback"
	"github.com/kuberploy/kuberploy/internal/edge"
	"github.com/kuberploy/kuberploy/internal/environmentfoundation"
	"github.com/kuberploy/kuberploy/internal/externaldns"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/helmapps"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/imagepull"
	"github.com/kuberploy/kuberploy/internal/imageresolution"
	"github.com/kuberploy/kuberploy/internal/middlewareprofiles"
	"github.com/kuberploy/kuberploy/internal/observability"
	"github.com/kuberploy/kuberploy/internal/operationcache"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	"github.com/kuberploy/kuberploy/internal/registry"
	"github.com/kuberploy/kuberploy/internal/releases"
	"github.com/kuberploy/kuberploy/internal/runtimeview"
	"github.com/kuberploy/kuberploy/internal/secrets"
	"github.com/kuberploy/kuberploy/internal/store/postgres"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("api stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := config.ValidateControlPlaneEgressFromEnvironment(); err != nil {
		return err
	}
	databaseURL, err := config.Required("KUBERPLOY_DATABASE_URL")
	if err != nil {
		return err
	}
	db, err := postgres.Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	middlewareProfiles, err := middlewareprofiles.OpenPostgresStore(ctx, databaseURL, "kuberploy-api-middleware")
	if err != nil {
		return err
	}
	defer middlewareProfiles.Close()
	publicURL := config.Get("KUBERPLOY_PUBLIC_URL", "")
	secure := false
	if u, parseErr := url.Parse(publicURL); parseErr == nil {
		secure = u.Scheme == "https"
	}
	valkeyAddresses := config.List("KUBERPLOY_VALKEY_ADDRESSES", "127.0.0.1:6379")
	valkeyUsername := os.Getenv("KUBERPLOY_VALKEY_USERNAME")
	valkeyPassword := os.Getenv("KUBERPLOY_VALKEY_PASSWORD")
	cacheUsername := config.Get("KUBERPLOY_VALKEY_CACHE_USERNAME", valkeyUsername)
	cachePassword := valkeyCredential("KUBERPLOY_VALKEY_CACHE_PASSWORD", valkeyPassword)
	limiterUsername := config.Get("KUBERPLOY_VALKEY_LIMITER_USERNAME", valkeyUsername)
	limiterPassword := valkeyCredential("KUBERPLOY_VALKEY_LIMITER_PASSWORD", valkeyPassword)
	releaseCache, err := releases.NewValkeyCache(releases.ValkeyCacheOptions{
		Addresses: valkeyAddresses,
		Username:  cacheUsername,
		Password:  cachePassword,
	})
	if err != nil {
		return err
	}
	defer releaseCache.Close()
	operationCache, err := operationcache.NewValkeyCache(operationcache.Options{Addresses: valkeyAddresses, Username: cacheUsername, Password: cachePassword, TTL: 30 * time.Second})
	if err != nil {
		return err
	}
	defer operationCache.Close()
	highRiskLimiter, err := ratelimit.NewValkeyLimiter(ratelimit.ValkeyOptions{Addresses: valkeyAddresses, Username: limiterUsername, Password: limiterPassword})
	if err != nil {
		return err
	}
	defer highRiskLimiter.Close()
	releaseService := releases.NewService(releases.NewGitHubChecker(nil), releaseCache, 30*time.Second)
	monitoringMode := config.Get("KUBERPLOY_MONITORING_MODE", "disabled")
	metrics, err := monitoringClient(monitoringMode, version)
	if err != nil {
		return err
	}
	runtime, runtimeReadiness, err := runtimeViewService(db)
	if err != nil {
		return err
	}
	sourceBuildConfig, err := builds.WorkerRuntimeConfigFromEnvironment()
	if err != nil {
		return err
	}
	gitProjectionConfig, err := gitprojection.RuntimeConfigFromEnvironment()
	if err != nil {
		return err
	}
	autoDeployConfig, err := autodeploy.RuntimeConfigFromEnvironment()
	if err != nil {
		return err
	}
	argoDesiredStateConfig, err := argo.ProductionRuntimeConfigFromEnvironment()
	if err != nil {
		return err
	}
	foundationConfig, err := environmentfoundation.RuntimeConfigFromEnvironment()
	if err != nil {
		return err
	}
	if argoDesiredStateConfig.Enabled && (!foundationConfig.Enabled || !gitProjectionConfig.Enabled) {
		return argo.ErrInvalid
	}
	if foundationConfig.Enabled && (!argoDesiredStateConfig.Enabled || !gitProjectionConfig.Enabled ||
		foundationConfig.PlatformBindingID != argoDesiredStateConfig.DesiredState.PlatformBindingID ||
		foundationConfig.Profile.ClusterID != argoDesiredStateConfig.DesiredState.ClusterID) {
		return environmentfoundation.ErrInvalid
	}
	platformGitBindingConfig, err := platformGitBindingConfigFromEnvironment(gitProjectionConfig)
	if err != nil {
		return err
	}
	runtimeSecretConfig, err := secrets.RuntimeConfigFromEnvironment()
	if err != nil {
		return err
	}
	certificateObservationConfig, err := certificates.ObservationConfigFromEnvironment(runtimeSecretConfig)
	if err != nil {
		return err
	}
	if certificateObservationConfig.Enabled && !gitProjectionConfig.Enabled {
		return gitprojection.ErrInvalid
	}
	if autoDeployConfig.Enabled && (!sourceBuildConfig.Enabled || !gitProjectionConfig.Enabled || !argoDesiredStateConfig.Enabled || !foundationConfig.Enabled) {
		return autodeploy.ErrInvalid
	}
	if autoDeployConfig.Enabled {
		sourceBuildDigest, digestErr := sourceBuildConfig.RuntimeDigest()
		if digestErr != nil {
			return digestErr
		}
		gitProjectionDigest, digestErr := gitProjectionConfig.RuntimeDigest()
		if digestErr != nil {
			return digestErr
		}
		argoIdentity, identityErr := argo.DesiredStateRuntimeIdentityForConfig(argoDesiredStateConfig.DesiredState)
		if identityErr != nil {
			return identityErr
		}
		autoDeployConfig.Identity, err = autodeploy.RuntimeIdentityForAuthorities(autodeploy.RuntimeAuthorities{
			SourceBuildConfigDigest: sourceBuildDigest, SourceBuildGitHubAppID: sourceBuildConfig.GitHub.AppID,
			GitProjectionConfigDigest: gitProjectionDigest, GitProjectionGitHubAppID: gitProjectionConfig.GitHub.AppID,
			FoundationConfigDigest: foundationConfig.Publisher.ConfigDigest, FoundationPollNanos: int64(foundationConfig.PollInterval),
			FoundationPlatformBindingID: foundationConfig.PlatformBindingID, FoundationClusterID: foundationConfig.Profile.ClusterID,
			ArgoConfigDigest: argoIdentity.ConfigDigest, ArgoGitHubAppID: argoIdentity.GitHubAppID,
			ArgoPlatformBindingID: argoIdentity.PlatformBindingID, ArgoClusterID: argoIdentity.ClusterID,
		})
		if err != nil {
			return err
		}
	}
	runtimeRegistryPullConfig, err := imagepull.RuntimeConfigFromEnvironment()
	if err != nil {
		return err
	}
	imageResolutionConfig, err := imageresolution.RuntimeConfigFromEnvironment(runtimeRegistryPullConfig)
	if err != nil {
		return err
	}
	edgeRuntimeConfig, err := edge.RuntimeConfigFromEnvironment()
	if err != nil {
		return err
	}
	externalDNSOperationalConfig, err := externaldns.OperationalConfigFromEnvironment()
	if err != nil {
		return err
	}
	certificateIssuerConfig, err := certissuers.ObserverConfigFromEnvironment()
	if err != nil {
		return err
	}
	if certificateIssuerConfig.Enabled && (!gitProjectionConfig.Enabled || !argoDesiredStateConfig.Enabled || !foundationConfig.Enabled ||
		!edgeRuntimeConfig.Enabled || edgeRuntimeConfig.Profiles.CertManager == nil ||
		certificateIssuerConfig.BindingID != argoDesiredStateConfig.DesiredState.PlatformBindingID ||
		certificateIssuerConfig.ClusterID != argoDesiredStateConfig.DesiredState.ClusterID ||
		certificateIssuerConfig.BindingID != foundationConfig.PlatformBindingID ||
		certificateIssuerConfig.ClusterID != foundationConfig.Profile.ClusterID) {
		return certissuers.ErrObservationUnavailable
	}
	if externalDNSOperationalConfig.Enabled && (!gitProjectionConfig.Enabled || !argoDesiredStateConfig.Enabled || !foundationConfig.Enabled || !edgeRuntimeConfig.Enabled || externalDNSOperationalConfig.BindingID != argoDesiredStateConfig.DesiredState.PlatformBindingID || externalDNSOperationalConfig.ClusterID != argoDesiredStateConfig.DesiredState.ClusterID || externalDNSOperationalConfig.BindingID != foundationConfig.PlatformBindingID || externalDNSOperationalConfig.ClusterID != foundationConfig.Profile.ClusterID) {
		return externaldns.ErrRuntimeUnavailable
	}
	sourceBuilds, err := newSourceBuildAPI(ctx, databaseURL, publicURL, os.Getenv(githubAppSlugEnv), sourceBuildConfig, db)
	if err != nil {
		return err
	}
	defer sourceBuilds.Close()
	buildLogService, buildLogReadiness, err := sourceBuildLogService(db, sourceBuilds)
	if err != nil {
		return err
	}
	runtimeSecrets, err := newRuntimeSecretAPI(ctx, databaseURL, runtimeSecretConfig)
	if err != nil {
		return err
	}
	if runtimeSecrets != nil {
		defer runtimeSecrets.Close()
	}
	certificateRuntime, err := newCertificateAPI(ctx, databaseURL, runtimeSecretConfig, certificateObservationConfig)
	if err != nil {
		return err
	}
	if certificateRuntime != nil {
		defer certificateRuntime.Close()
	}
	gitProjection, err := newGitProjectionAPI(ctx, databaseURL, gitProjectionConfig, runtimeSecretConfig, certificateObservationConfig, certificateIssuerConfig, runtimeRegistryPullConfig, edgeRuntimeConfig, db)
	if err != nil {
		return err
	}
	if gitProjection != nil {
		defer gitProjection.Close()
		if sourceBuilds != nil && gitProjectionConfig.WebhookWake {
			sourceBuilds.setGitProjectionWaker(gitProjection.store)
		}
	}
	foundationAPI, err := newEnvironmentFoundationAPI(ctx, databaseURL, foundationConfig)
	if err != nil {
		return err
	}
	if foundationAPI != nil {
		defer foundationAPI.Close()
		if runtime != nil {
			// A Kubernetes discovery response alone does not prove the API service
			// account can read this installation's dynamically created workload
			// namespaces. Keep the public runtime-view capability closed until the
			// exact protected foundation profile, including its RoleBinding subject,
			// is durably ready.
			runtimeReadiness = combinedReadiness{foundationAPI.readiness, runtimeReadiness}
		}
	}
	argoDesiredState, err := newArgoDesiredStateAPI(ctx, databaseURL, argoDesiredStateConfig)
	if err != nil {
		return err
	}
	if argoDesiredState != nil {
		defer argoDesiredState.Close()
	}
	helmApplications, err := newHelmApplicationsAPI(ctx, databaseURL, gitProjection, argoDesiredState)
	if err != nil {
		return err
	}
	if helmApplications != nil {
		defer helmApplications.Close()
	}
	runtimeRegistryPulls, err := newRuntimeRegistryPullAPI(ctx, databaseURL, runtimeRegistryPullConfig)
	if err != nil {
		return err
	}
	if runtimeRegistryPulls != nil {
		defer runtimeRegistryPulls.Close()
	}
	edgeAPI, err := newEdgeAPI(ctx, databaseURL, edgeRuntimeConfig)
	if err != nil {
		return err
	}
	if edgeAPI != nil {
		defer edgeAPI.Close()
	}
	if err = enableDynamicExternalDNSAPI(edgeAPI, db, edgeRuntimeConfig, externalDNSOperationalConfig); err != nil {
		return err
	}
	certificateIssuers, err := newCertificateIssuerAPI(ctx, databaseURL, certificateIssuerConfig, edgeHTTPCertificateIssuers(edgeAPI))
	if err != nil {
		return err
	}
	if certificateIssuers != nil {
		defer certificateIssuers.Close()
	}
	var autoDeployStore *autodeploy.PostgreSQLStore
	var autoDeployService *autodeploy.PolicyService
	var autoDeployReadiness httpapi.ReadinessProbe
	if autoDeployConfig.Enabled {
		autoDeployStore, err = autodeploy.OpenPostgreSQLStore(ctx, databaseURL)
		if err != nil {
			return err
		}
		defer autoDeployStore.Close()
		autoDeployService = &autodeploy.PolicyService{Catalog: db, Projection: gitProjection, Store: db,
			NewID: func() (string, error) { return id.New(), nil }}
		autoDeployReadiness = &autodeploy.RuntimeReadinessProbe{Store: autoDeployStore, Identity: autoDeployConfig.Identity}
	}
	var githubSetup httpapi.GitHubSetupBackend
	var githubWebhook httpapi.GitHubWebhookBackend
	var buildBackend httpapi.BuildBackend
	var buildPromotions *buildpromotion.Resolver
	var gitBindingRepositories httpapi.GitBindingRepositoryResolver
	var buildReadiness httpapi.ReadinessProbe
	var gitProjectionBackend httpapi.GitProjectionBackend
	var gitProjectionReadiness httpapi.ReadinessProbe
	var argoReadiness httpapi.ReadinessProbe
	var runtimeSecretBackend httpapi.RuntimeSecretBackend
	var runtimeSecretReadiness httpapi.ReadinessProbe
	var certificateBackend httpapi.CertificateManagementBackend
	var certificateReadiness httpapi.ReadinessProbe
	var certificateReferences httpapi.CertificateReferenceBackend
	var helmApplicationBackend httpapi.HelmApplicationBackend
	var helmApprovalBackend httpapi.HelmApprovalAdmissionBackend
	var helmPreviewBackend httpapi.HelmRenderedManifestPreviewBackend
	if sourceBuilds != nil {
		githubSetup, githubWebhook, buildBackend, buildReadiness = sourceBuilds.setup, sourceBuilds.webhook, sourceBuilds.backend, sourceBuilds.readiness
		buildPromotions = &buildpromotion.Resolver{Projections: sourceBuilds.store, Releases: db, Resources: db, Access: db}
		var ok bool
		gitBindingRepositories, ok = sourceBuilds.backend.(httpapi.GitBindingRepositoryResolver)
		if !ok {
			return errors.New("verified GitHub repository resolver is unavailable")
		}
	}
	if gitProjection != nil {
		gitProjectionBackend, gitProjectionReadiness = gitProjection.backend, gitProjection.readiness
	}
	if argoDesiredState != nil {
		if foundationAPI == nil {
			return environmentfoundation.ErrUnavailable
		}
		argoReadiness = combinedReadiness{foundationAPI.readiness, argoDesiredState.readiness}
	}
	if runtimeSecrets != nil {
		runtimeSecretBackend, runtimeSecretReadiness = runtimeSecrets.backend, runtimeSecrets.readiness
	}
	if certificateRuntime != nil {
		certificateBackend, certificateReadiness, certificateReferences = certificateRuntime.backend, certificateRuntime.readiness, certificateRuntime.resolver
		if err = db.ConfigureCertificateReferences(certificateRuntime.resolver); err != nil {
			return err
		}
	}
	if helmApplications != nil {
		helmApplicationBackend = helmApplications.runtime
		helmApprovalBackend = helmApplications.approvals
		helmPreviewBackend = helmApplications.previews
	}
	managedRegistryConfig, err := registry.RuntimeConfigFromEnvironment()
	if err != nil {
		return err
	}
	managedRegistry, err := newManagedRegistryAPI(managedRegistryConfig, db)
	if err != nil {
		return err
	}
	externalDNSManagement := externalDNSManagementForConfig(db, externalDNSOperationalConfig)
	deploymentRollbacks := &deploymentrollback.Resolver{History: db, Artifacts: db, Publications: db}
	imageResolution := &imageresolution.Resolver{Catalog: db, Config: imageResolutionConfig,
		Provider: &imageresolution.HTTPProvider{Credentials: imageresolution.NewProjectedCredentialSource(), Config: imageresolution.DefaultProviderConfig()}}
	edgeReadiness, edgeFeatures := edgeHTTPRuntime(edgeAPI)
	var certificateIssuerCatalog httpapi.CertificateIssuerCatalog = edgeHTTPCertificateIssuers(edgeAPI)
	var certificateIssuerAdmin httpapi.CertificateIssuerAdminBackend
	var certificateIssuerReadiness httpapi.ReadinessProbe
	if certificateIssuers != nil {
		certificateIssuerCatalog = certificateIssuers.catalog
		certificateIssuerAdmin = certificateIssuers.admin
		certificateIssuerReadiness = certificateIssuers.readiness
	}
	var appConfigRenderedPreviews httpapi.AppConfigRenderedPreviewBackend
	if argoDesiredStateConfig.Enabled {
		lock := argoDesiredStateConfig.DesiredState.Runtime
		appConfigRenderedPreviews, err = appconfigpreview.NewProduction(appconfigpreview.Identity{
			Contract: appconfigpreview.Contract, ChartName: lock.ChartName, ChartVersion: lock.ChartVersion,
			ChartDigest: lock.ChartDigest, RendererImage: appconfigpreview.RendererImage,
			RendererVersion: appconfigpreview.RendererVersion, PolicyVersion: helmapps.PolicyVersion,
		})
		if err != nil {
			return err
		}
	}
	handler := httpapi.New(httpapi.Options{Store: db, BootstrapToken: os.Getenv("KUBERPLOY_BOOTSTRAP_TOKEN"), Version: version, PublicURL: publicURL, MonitoringMode: monitoringMode, SecureCookie: secure, Releases: releaseService, Metrics: metrics, Runtime: runtime, RuntimeReadiness: runtimeReadiness,
		GitHubSetup: githubSetup, GitHubWebhook: githubWebhook, Builds: buildBackend, BuildPromotions: buildPromotions, BuildLogs: buildLogService, GitBindingRepositories: gitBindingRepositories, PlatformGitBinding: platformGitBindingConfig, BuildReadiness: buildReadiness, BuildLogReadiness: buildLogReadiness, ValkeyReadiness: valkeyReadinessProbe{pinger: releaseCache}, OperationCache: operationCache, AppConfigRenderedPreviews: appConfigRenderedPreviews,
		GitProjection: gitProjectionBackend, GitProjectionReadiness: gitProjectionReadiness, ArgoReadiness: argoReadiness,
		RuntimeSecrets: runtimeSecretBackend, RuntimeSecretReadiness: runtimeSecretReadiness,
		Certificates: certificateBackend, CertificateReadiness: certificateReadiness, CertificateReferences: certificateReferences, CertificateIssuers: certificateIssuerCatalog,
		CertificateIssuerAdmin: certificateIssuerAdmin, CertificateIssuerRuntimeReadiness: certificateIssuerReadiness,
		RegistryPullReadiness: runtimeRegistryPullReadiness(runtimeRegistryPulls), RegistryPulls: db, RegistryPullConfig: runtimeRegistryPullConfig,
		ImageResolution: imageResolution,
		EdgeReadiness:   edgeReadiness, EdgeFeatures: edgeFeatures, SSLIP: edgeHTTPSSLIP(edgeAPI),
		Registry: managedRegistry.management, RegistryReadiness: managedRegistry.readiness,
		ExternalDNS: externalDNSManagement, HelmApplications: helmApplicationBackend,
		HelmApprovals: helmApprovalBackend, HelmRenderedPreviews: helmPreviewBackend,
		MiddlewareProfiles:  middlewareProfiles,
		DeploymentRollbacks: deploymentRollbacks,
		AutoDeployService:   autoDeployService,
		AutoDeployPolicies:  db,
		AutoDeployReadiness: autoDeployReadiness,
		HighRiskLimiter:     highRiskLimiter})
	var autoDeployRuntimeDone <-chan error
	if autoDeployConfig.Enabled {
		runtimeDone := make(chan error, 1)
		autoDeployRuntimeDone = runtimeDone
		runtime := &autoDeployRuntime{readiness: autoDeployStore, identity: autoDeployConfig.Identity, workerID: "api-auto-deploy-" + id.New(),
			controller: &autodeploy.Controller{Store: autoDeployStore, Releases: db, Authorization: db, Deployments: handler,
				Owner: "api-auto-deploy-" + id.New(), LeaseDuration: autodeploy.RuntimeRunLease}}
		go func() { runtimeDone <- runtime.Run(ctx) }()
	}
	srv := &http.Server{Addr: config.Get("KUBERPLOY_LISTEN_ADDR", ":8080"), Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	done := make(chan error, 1)
	go func() {
		slog.Info("Kuberploy API listening", "address", srv.Addr, "version", version)
		done <- srv.ListenAndServe()
	}()
	select {
	case err = <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case err = <-autoDeployRuntimeDone:
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

func valkeyCredential(name, fallback string) string {
	if value, present := os.LookupEnv(name); present && value != "" {
		return value
	}
	return fallback
}

func sourceBuildLogService(db *postgres.Store, sourceBuilds *sourceBuildAPI) (httpapi.BuildLogService, httpapi.ReadinessProbe, error) {
	enabled := config.Get("KUBERPLOY_BUILD_LOGS_ENABLED", "false")
	if enabled != "true" && enabled != "false" {
		return nil, nil, errors.New("KUBERPLOY_BUILD_LOGS_ENABLED must be true or false")
	}
	if enabled == "false" {
		return nil, nil, nil
	}
	if db == nil || sourceBuilds == nil || sourceBuilds.backend == nil {
		return nil, nil, errors.New("build logs require the source-build API runtime")
	}
	client, err := buildlogs.NewInClusterClient()
	if err != nil {
		return nil, nil, err
	}
	service, err := httpapi.NewBuildLogService(db, sourceBuilds.backend, client)
	if err != nil {
		return nil, nil, err
	}
	return service, client, nil
}

func runtimeViewService(db *postgres.Store) (httpapi.RuntimeViewService, httpapi.ReadinessProbe, error) {
	enabled := config.Get("KUBERPLOY_RUNTIME_VIEW_ENABLED", "false")
	if enabled != "true" && enabled != "false" {
		return nil, nil, errors.New("KUBERPLOY_RUNTIME_VIEW_ENABLED must be true or false")
	}
	if enabled == "false" {
		return nil, nil, nil
	}
	client, err := runtimeview.NewInClusterClient()
	if err != nil {
		return nil, nil, err
	}
	service, err := httpapi.NewRuntimeViewService(db, client)
	if err != nil {
		return nil, nil, err
	}
	return service, client, nil
}

func monitoringClient(mode, runtimeVersion string) (httpapi.MetricsService, error) {
	endpoint := config.Get("KUBERPLOY_PROMETHEUS_URL", "")
	tokenSetting := config.Get("KUBERPLOY_PROMETHEUS_BEARER_TOKEN_ENABLED", "false")
	if tokenSetting != "true" && tokenSetting != "false" {
		return nil, errors.New("KUBERPLOY_PROMETHEUS_BEARER_TOKEN_ENABLED must be true or false")
	}
	switch mode {
	case "disabled":
		return nil, nil
	case "managed", "existing":
		if endpoint == "" {
			return nil, errors.New("KUBERPLOY_PROMETHEUS_URL is required when monitoring is enabled")
		}
	default:
		return nil, errors.New("KUBERPLOY_MONITORING_MODE must be disabled, managed, or existing")
	}
	var token observability.BearerTokenSource
	if tokenSetting == "true" {
		token = observability.NewProjectedBearerToken()
	}
	client, err := observability.NewClient(observability.Options{BaseURL: endpoint, TokenSource: token, AllowHTTPForClusterService: mode == "managed"})
	if err != nil {
		return nil, err
	}
	if mode != "managed" {
		return client, nil
	}
	if endpoint != observability.ManagedMonitoringQueryURL || token != nil {
		return nil, errors.New("managed monitoring requires the exact credential-free protected service endpoint")
	}
	observer, err := observability.NewInClusterManagedMonitoringObserver()
	if err != nil {
		return nil, err
	}
	return observability.NewManagedService(client, observer, runtimeVersion)
}
