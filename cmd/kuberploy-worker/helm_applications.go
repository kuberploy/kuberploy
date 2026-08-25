package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/argo"
	"github.com/kuberploy/kuberploy/internal/helmapps"
)

type helmApplicationsRuntime struct {
	pool    *pgxpool.Pool
	runtime *helmapps.Runtime
}

func newHelmApplicationsRuntime(ctx context.Context, databaseURL, host string,
	projection *gitProjectionRuntime, argoRuntime *argoDesiredStateRuntime) (*helmApplicationsRuntime, error) {
	return newHelmApplicationsRuntimeFromLookup(ctx, databaseURL, host, projection, argoRuntime, os.LookupEnv)
}

func newHelmApplicationsRuntimeFromLookup(ctx context.Context, databaseURL, host string,
	projection *gitProjectionRuntime, argoRuntime *argoDesiredStateRuntime,
	lookup func(string) (string, bool)) (*helmApplicationsRuntime, error) {
	config, err := protectedHelmRuntimeConfigForWorker(projection, lookup)
	if err != nil {
		return nil, err
	}
	if !config.Enabled {
		return nil, nil
	}
	identity, err := validateHelmWorkerAuthorities(config, projection, argoRuntime)
	if err != nil {
		return nil, err
	}
	credentials, err := helmapps.RuntimeOCICredentialProvider(ctx, config)
	if err != nil {
		return nil, err
	}
	renderer, err := helmapps.NewInClusterRendererKubernetesAPI(config.Renderer.Namespace)
	if err != nil {
		return nil, err
	}
	pool, err := openHelmWorkerPool(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	bindings, err := helmapps.NewPostgresProtectedBindingResolver(pool, helmapps.ProtectedBindingResolverConfig{
		PlatformBindingID: identity.PlatformBindingID,
	})
	if err != nil {
		pool.Close()
		return nil, err
	}
	startedAt := time.Now().UTC()
	// A container restart in the same Pod reuses both hostname and PID. Include
	// the process start instant so readiness upserts cannot collide with the
	// prior process row while its lease ages out.
	processIdentity := workerProcessIdentity(host, os.Getpid(), startedAt)
	runtime, err := helmapps.NewRuntime(config, helmapps.RuntimeDependencies{
		Pool: pool, OCIClient: &http.Client{}, Credentials: credentials, RendererAPI: renderer, Bindings: bindings,
		ArgoMaterialization: helmapps.ArgoMaterializationAuthority{PolicyDigest: projection.policyDigest,
			Runtime: identity.Runtime, DigestEnforcement: identity.DigestEnforcement},
		GitBindings: projection.store, GitProvider: projection.headVerifier, GitManager: projection.writeManager,
		RootRefresher: &helmapps.ProductionProtectedRootRefresher{Identity: identity,
			Refresher: argoRuntime.kubernetes},
		ArgoObservation: argoRuntime.observation, CascadeRoots: argoRuntime.kubernetes,
		CascadeApplications: argoRuntime.kubernetes,
		WorkerID:            workerLeaseOwner(processIdentity, "helm-applications"), WorkerEpoch: 1,
		StartedAt: startedAt, Now: func() time.Time { return time.Now().UTC() },
		ReportError: func(loop string, runtimeErr error) {
			slog.Warn("protected Helm application runtime iteration failed", "loop", loop, "error", runtimeErr)
		},
	})
	if err != nil {
		pool.Close()
		return nil, err
	}
	return &helmApplicationsRuntime{pool: pool, runtime: runtime}, nil
}

func workerProcessIdentity(host string, pid int, startedAt time.Time) string {
	return host + "/" + strconv.Itoa(pid) + "/" + startedAt.UTC().Format(time.RFC3339Nano)
}

func protectedHelmRuntimeConfigForWorker(projection *gitProjectionRuntime,
	lookup func(string) (string, bool)) (helmapps.RuntimeConfig, error) {
	baseDigest := ""
	if projection == nil || projection.identity.Validate() != nil {
		config, err := helmapps.RuntimeConfigFromLookup(lookup, helmapps.ProtectedPublisherIdentity{})
		return config, err
	}
	baseDigest = projection.identity.ConfigDigest
	seed := helmapps.ProtectedPublisherIdentity{Contract: helmapps.ProtectedPublisherContract,
		PolicyVersion: helmapps.ProtectedGitPolicy, ConfigDigest: baseDigest}
	config, err := helmapps.RuntimeConfigFromLookup(lookup, seed)
	if err != nil || !config.Enabled {
		return config, err
	}
	publisher, err := helmapps.ProtectedPublisherIdentityForRuntime(projection.identity, config)
	if err != nil {
		return helmapps.RuntimeConfig{}, err
	}
	config.Publisher = publisher
	return config, config.Validate()
}

func validateHelmWorkerAuthorities(config helmapps.RuntimeConfig, projection *gitProjectionRuntime,
	argoRuntime *argoDesiredStateRuntime) (argo.DesiredStateRuntimeIdentity, error) {
	if config.Validate() != nil || !config.Enabled || projection == nil || projection.store == nil ||
		projection.identity.Validate() != nil || projection.headVerifier.AppID != projection.identity.GitHubAppID ||
		projection.headVerifier.Authorizations != projection.store || projection.headVerifier.Client == nil ||
		projection.writeManager == nil || projection.writeManager.CredentialProvider == nil ||
		argoRuntime == nil || argoRuntime.store == nil || argoRuntime.runtime == nil ||
		argoRuntime.runtime.Worker == nil || argoRuntime.runtime.Prerequisites == nil ||
		argoRuntime.runtime.Materializer == nil {
		return argo.DesiredStateRuntimeIdentity{}, helmapps.ErrInvalid
	}
	expectedPublisher, err := helmapps.ProtectedPublisherIdentityForRuntime(projection.identity, config)
	if err != nil || config.Publisher != expectedPublisher {
		return argo.DesiredStateRuntimeIdentity{}, helmapps.ErrInvalid
	}
	identity := argoRuntime.runtime.Worker.Observation.DesiredStateRuntimeIdentity
	if identity.Validate() != nil || identity.GitHubAppID != projection.identity.GitHubAppID ||
		identity.ArgoNamespace != config.Application.ArgoNamespace ||
		identity.RootApplicationName != argo.PlatformRootApplicationName ||
		identity.Runtime.ChartName != argo.RuntimeChartName ||
		identity.DigestEnforcement != argo.ChartDigestNativeOCI {
		return argo.DesiredStateRuntimeIdentity{}, helmapps.ErrInvalid
	}
	return identity, nil
}

func openHelmWorkerPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, helmapps.ErrInvalid
	}
	config.ConnConfig.RuntimeParams["application_name"] = "kuberploy-helm-applications-worker"
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

func (r *helmApplicationsRuntime) Run(ctx context.Context) error {
	if r == nil || r.pool == nil || r.runtime == nil || !r.runtime.Enabled {
		return fmt.Errorf("protected Helm application runtime is not configured")
	}
	return r.runtime.Run(ctx)
}

func (r *helmApplicationsRuntime) Close() {
	if r != nil && r.pool != nil {
		r.pool.Close()
	}
}
