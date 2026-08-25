package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/kuberploy/kuberploy/internal/argo"
	"github.com/kuberploy/kuberploy/internal/githubapp"
	"github.com/kuberploy/kuberploy/internal/imagepull"
)

type argoDesiredStateRuntime struct {
	store       *argo.PostgreSQLStore
	runtime     *argo.ProductionDesiredStateRuntime
	kubernetes  *argo.InClusterProductionClient
	observation argo.DesiredStateRuntimeWorkerObservation
}

func newArgoDesiredStateRuntime(
	ctx context.Context,
	databaseURL string,
	host string,
	config argo.ProductionRuntimeConfig,
	registryPullConfig imagepull.RuntimeConfig,
	projection *gitProjectionRuntime,
	foundation argo.FoundationReadinessProbe,
	observation *argoObservationRuntime,
) (*argoDesiredStateRuntime, error) {
	if !config.Enabled {
		return nil, nil
	}
	if config.Validate() != nil || projection == nil || projection.store == nil || projection.policy == nil || foundation == nil ||
		projection.writeManager == nil || projection.headVerifier.Client == nil || projection.policyDigest == "" ||
		projection.headVerifier.AppID != config.DesiredState.GitHubAppID || observation == nil || observation.store == nil ||
		observation.coordinator == nil || observation.coordinator.Namespace != config.DesiredState.ArgoNamespace {
		return nil, argo.ErrInvalid
	}
	if err := githubapp.ProbeProjectedWorkerRuntime(ctx, config.GitHub); err != nil {
		return nil, err
	}
	protectionClient, err := githubapp.NewProjectedClient(config.GitHub)
	if err != nil {
		return nil, err
	}
	identity, err := argo.DesiredStateRuntimeIdentityForConfig(config.DesiredState)
	if err != nil {
		return nil, err
	}
	store, err := argo.OpenPostgreSQLStore(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	artifactAge := registryPullConfig.ReadinessMaxAge
	if artifactAge == 0 {
		artifactAge = 90 * time.Second
	}
	components, err := argo.NewPostgreSQLProductionComponents(
		store,
		projection.store,
		projection.policy,
		projection.policyDigest,
		artifactAge,
		identity,
	)
	if err != nil {
		store.Close()
		return nil, err
	}
	kubernetes, err := argo.NewInClusterProductionClient()
	if err != nil {
		store.Close()
		return nil, err
	}
	keys, err := argo.NewProjectedGitHubAppPrivateKeySource(config.GitHub, githubapp.NewProjectedSecretReader())
	if err != nil {
		store.Close()
		return nil, err
	}
	credentials := &argo.RepositoryCredentialController{
		Namespace: config.DesiredState.ArgoNamespace, GitHubAppID: config.DesiredState.GitHubAppID,
		Keys: keys, Kubernetes: kubernetes,
	}
	protection := argo.GitHubPlatformRepositoryProtectionVerifier{
		AppID: config.DesiredState.GitHubAppID, Authorizations: projection.store, Client: protectionClient,
	}
	prerequisites := &argo.ProductionPrerequisites{
		Identity: identity, Catalog: components.Catalog, Credentials: credentials,
		Provider: projection.headVerifier, Protection: protection, RootApplications: kubernetes, RootRefresher: kubernetes,
		Foundation:        foundation,
		MaximumCatalogAge: config.MaximumCatalogAge,
	}
	startedAt := time.Now().UTC()
	workerID := argoDesiredStateWorkerID(host, os.Getpid(), startedAt)
	writer := &argo.DesiredStateWriter{
		Store: store, Bindings: projection.store, ClaimGate: components.ProjectionGate,
		Provider: projection.headVerifier, Manager: projection.writeManager, RootRefresher: kubernetes,
		ApplicationSets: kubernetes, ObservationWaker: observation.store, Identity: identity,
		LeaseDuration: 2 * time.Minute, HeartbeatInterval: 30 * time.Second,
	}
	worker := &argo.DesiredStateRuntimeWorker{
		Store: store, Writer: writer,
		Observation: argo.DesiredStateRuntimeWorkerObservation{
			WorkerID: workerID, DesiredStateRuntimeIdentity: identity,
			StartedAt: startedAt, ObservedAt: startedAt,
		},
		LeaseDuration: 2 * time.Minute, PollInterval: config.PollInterval,
		ReportError: func(commandID, failureCode string, err error) {
			slog.Warn("Argo desired-state command will retry", "command_id", commandID, "failure_code", failureCode, "error", err)
		},
	}
	runtime := &argo.ProductionDesiredStateRuntime{
		Worker: worker, Prerequisites: prerequisites, Materializer: components.Materializer,
		PollInterval: config.PollInterval,
		ReportPrerequisiteError: func(err error) {
			slog.Warn("Argo desired-state prerequisites not ready", "error", err)
		},
	}
	return &argoDesiredStateRuntime{store: store, runtime: runtime, kubernetes: kubernetes,
		observation: worker.Observation}, nil
}

func argoDesiredStateWorkerID(host string, pid int, startedAt time.Time) string {
	return workerLeaseOwner(workerProcessIdentity(host, pid, startedAt), "argo-desired-state")
}

func workerProcessIdentity(host string, pid int, startedAt time.Time) string {
	return host + "/" + strconv.Itoa(pid) + "/" + startedAt.UTC().Format(time.RFC3339Nano)
}

func (r *argoDesiredStateRuntime) Run(ctx context.Context) error {
	if r == nil || r.store == nil || r.runtime == nil {
		return fmt.Errorf("protected Argo desired-state runtime is not configured")
	}
	return r.runtime.Run(ctx)
}

func (r *argoDesiredStateRuntime) Close() {
	if r != nil && r.store != nil {
		r.store.Close()
	}
}
