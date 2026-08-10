package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/kuberploy/kuberploy/internal/certificates"
	"github.com/kuberploy/kuberploy/internal/certissuers"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/edge"
	"github.com/kuberploy/kuberploy/internal/githubapp"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/gitpublication"
	"github.com/kuberploy/kuberploy/internal/imagepull"
	"github.com/kuberploy/kuberploy/internal/middlewareprofiles"
	"github.com/kuberploy/kuberploy/internal/projectionpolicy"
	"github.com/kuberploy/kuberploy/internal/scheduling"
	"github.com/kuberploy/kuberploy/internal/secrets"
)

type gitProjectionRuntime struct {
	store        *gitprojection.PostgreSQLStore
	coordinator  *gitprojection.Coordinator
	publications *gitpublication.Reconciler
	writer       *projectionOperationWriter
	policy       *projectionpolicy.Validator
	policyDigest string
	headVerifier gitprojection.GitHubHeadVerifier
	writeManager *gitprojection.MirrorManager
	identity     gitprojection.RuntimeIdentity
	workerID     string
	startedAt    time.Time
	sslip        *edge.PostgreSQLSSLIPResolver
	scheduling   *scheduling.PostgresStore
	middleware   *middlewareprofiles.PostgresStore
}

type projectionOperationWriter struct {
	projection *gitprojection.ProjectionWriter
}

func (w *projectionOperationWriter) Write(ctx context.Context, operation domain.Operation, project domain.Project, environment domain.Environment, application domain.Application, deployment domain.Deployment) (domain.GitPublicationResult, error) {
	if w == nil || w.projection == nil || w.projection.Commands == nil {
		return domain.GitPublicationResult{}, gitprojection.ErrInvalid
	}
	command, err := w.projection.Commands.WriteCommand(ctx, operation.ID)
	if err != nil {
		return domain.GitPublicationResult{}, err
	}
	if command.OperationID != operation.ID || command.DeploymentID != deployment.ID || command.Plan.ProjectID != project.ID ||
		command.Plan.EnvironmentID != environment.ID || command.Plan.ApplicationID != application.ID ||
		!bytes.Equal(command.Content, deployment.ConfigRaw) {
		return domain.GitPublicationResult{}, gitprojection.ErrInvalid
	}
	result, err := w.projection.PublishOperation(ctx, operation.ID)
	if err != nil {
		return domain.GitPublicationResult{}, err
	}
	publication := domain.GitPublicationResult{Mode: string(result.Mode), Revision: result.Revision}
	if result.Publication != nil {
		publication.CandidateRevision = result.Publication.CandidateRevision
		publication.PullRequestNumber = result.Publication.PullRequestNumber
		publication.PullRequestURL = result.Publication.PullRequestURL
		publication.PullRequestState = string(result.Publication.PullRequestState)
	}
	return publication, nil
}

func (w *projectionOperationWriter) WriteVariable(ctx context.Context, operation domain.Operation) (domain.GitPublicationResult, error) {
	if w == nil || w.projection == nil || w.projection.Commands == nil || operation.Kind != "variable-set.git-write" {
		return domain.GitPublicationResult{}, gitprojection.ErrInvalid
	}
	command, err := w.projection.Commands.WriteCommand(ctx, operation.ID)
	if err != nil {
		return domain.GitPublicationResult{}, err
	}
	if command.OperationID != operation.ID || command.DeploymentID != "" || command.Plan.VariableScope == "" ||
		command.Path != command.Plan.VariablePath || operation.TargetType != command.Plan.VariableScope {
		return domain.GitPublicationResult{}, gitprojection.ErrInvalid
	}
	targetID := command.Plan.EnvironmentID
	if command.Plan.VariableScope == "project" {
		targetID = command.Plan.ProjectID
	}
	if operation.TargetID != targetID {
		return domain.GitPublicationResult{}, gitprojection.ErrInvalid
	}
	result, err := w.projection.PublishOperation(ctx, operation.ID)
	if err != nil {
		return domain.GitPublicationResult{}, err
	}
	publication := domain.GitPublicationResult{Mode: string(result.Mode), Revision: result.Revision}
	if result.Publication != nil {
		publication.CandidateRevision = result.Publication.CandidateRevision
		publication.PullRequestNumber = result.Publication.PullRequestNumber
		publication.PullRequestURL = result.Publication.PullRequestURL
		publication.PullRequestState = string(result.Publication.PullRequestState)
	}
	return publication, nil
}

func newGitProjectionRuntime(ctx context.Context, databaseURL, host string, config gitprojection.RuntimeConfig, secretConfig secrets.RuntimeConfig, certificateConfig certificates.ObservationConfig, issuerConfig certissuers.ObserverConfig, registryPullConfig imagepull.RuntimeConfig, edgeConfig edge.RuntimeConfig, certificateResolver *certificates.PostgreSQLReferenceResolver, issuerStore *certissuers.PostgresStore) (*gitProjectionRuntime, error) {
	if _, err := certificates.ObservationPolicyDigest(certificateConfig); err != nil {
		return nil, certificates.ErrObservationUnavailable
	}
	if certificateConfig.Enabled && (!config.Enabled || certificateResolver == nil) {
		return nil, certificates.ErrObservationUnavailable
	}
	if !certificateConfig.Enabled && certificateResolver != nil {
		return nil, certificates.ErrObservationUnavailable
	}
	if issuerConfig.Validate() != nil || issuerConfig.Enabled && (!config.Enabled || issuerStore == nil) || !issuerConfig.Enabled && issuerStore != nil {
		return nil, certissuers.ErrObservationUnavailable
	}
	if !config.Enabled {
		return nil, nil
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if secretConfig.Enabled && secretConfig.Validate() != nil {
		return nil, secrets.ErrRuntimeUnavailable
	}
	if err := githubapp.ProbeProjectedWorkerRuntime(ctx, config.GitHub); err != nil {
		return nil, err
	}
	provider, err := githubapp.NewProjectedClient(config.GitHub)
	if err != nil {
		return nil, err
	}
	store, err := gitprojection.OpenPostgreSQLStore(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	policyDigest, err := projectionpolicy.RuntimePolicyDigest(secretConfig, certificateConfig, issuerConfig, registryPullConfig, edgeConfig)
	if err != nil {
		store.Close()
		return nil, err
	}
	identity, err := gitprojection.RuntimeIdentityForConfig(config, policyDigest)
	if err != nil {
		store.Close()
		return nil, err
	}
	readManager := config.MirrorManager()
	readManager.CredentialProvider = gitprojection.GitHubGitCredentialProvider{
		AppID: config.GitHub.AppID, Authorizations: store,
		Client: gitprojection.GitHubGitClientAdapter{Client: provider},
	}
	writeManager := config.MirrorManager()
	writeManager.CredentialProvider = gitprojection.GitHubGitCredentialProvider{
		AppID: config.GitHub.AppID, Authorizations: store,
		Client: gitprojection.GitHubGitClientAdapter{Client: provider}, Write: true,
	}
	indexer := config.Indexer(store)
	edgePolicy := &projectionpolicy.EdgeRouteReferencePolicy{Config: edgeConfig, Certificates: certificateResolver}
	if issuerConfig.Enabled {
		edgePolicy.ManagedIssuers = issuerStore
		edgePolicy.ManagedIssuerMaxAge = issuerConfig.MaximumAge
	}
	var sslipResolver *edge.PostgreSQLSSLIPResolver
	if edgeConfig.Enabled && edgeConfig.Profiles.Traefik != nil && edgeConfig.Profiles.Traefik.SSLIP != nil {
		sslipResolver, err = edge.OpenPostgreSQLSSLIPResolver(ctx, databaseURL, edgeConfig)
		if err != nil {
			store.Close()
			return nil, err
		}
		edgePolicy.SSLIP = sslipResolver
	}
	schedulingStore, err := scheduling.OpenPostgresStore(ctx, databaseURL, "kuberploy-git-scheduling-policy")
	if err != nil {
		if sslipResolver != nil {
			sslipResolver.Close()
		}
		store.Close()
		return nil, err
	}
	middlewareStore, err := middlewareprofiles.OpenPostgresStore(ctx, databaseURL, "kuberploy-git-middleware-policy")
	if err != nil {
		schedulingStore.Close()
		if sslipResolver != nil {
			sslipResolver.Close()
		}
		store.Close()
		return nil, err
	}
	policy := &projectionpolicy.Validator{Edge: edgePolicy, ExternalDNSRuntime: edgePolicy, Scheduling: schedulingStore, Middleware: middlewareStore}
	if secretConfig.Enabled {
		policy.Secrets = &projectionpolicy.RuntimeSecretReferencePolicy{Config: secretConfig}
	}
	policy.Registry = &projectionpolicy.RegistryPullReferencePolicy{Config: registryPullConfig}
	indexer.Policy = policy
	processIdentity := host + "/" + strconv.Itoa(os.Getpid())
	verifier := gitprojection.GitHubHeadVerifier{AppID: config.GitHub.AppID, Authorizations: store, Client: provider}
	coordinator := &gitprojection.Coordinator{
		Store:             store,
		Provider:          verifier,
		Projector:         gitprojection.ShadowProjector{Manager: readManager, Indexer: indexer},
		Owner:             workerLeaseOwner(processIdentity, "git-projection"),
		LeaseDuration:     time.Minute,
		HeartbeatInterval: 15 * time.Second,
		WorkTimeout:       10 * time.Minute,
		PollInterval:      config.PollInterval,
		MinimumBackoff:    5 * time.Second,
		MaximumBackoff:    5 * time.Minute,
		IdleDelay:         time.Second,
		JitterFraction:    0.2,
		ReportError: func(err error) {
			slog.Warn("Git projection reconciliation iteration failed", "error", err)
		},
	}
	projectionWriter := &gitprojection.ProjectionWriter{Store: store, Commands: store, Provider: verifier, Manager: writeManager,
		Protection: gitprojection.GitHubRepositoryProtectionVerifier{AppID: config.GitHub.AppID, Authorizations: store, Client: provider},
		Owner:      workerLeaseOwner(processIdentity, "git-writes")}
	publicationProvider := gitpublication.GitHubProvider{AppID: config.GitHub.AppID, Authorizations: store, Client: provider}
	publicationService := &gitpublication.Service{Store: store, Provider: publicationProvider}
	publicationReconciler := &gitpublication.Reconciler{Store: store, Service: *publicationService, Batch: 50,
		PollInterval: config.PollInterval, ReportError: func(err error) {
			slog.Warn("protected Git pull request observation failed", "error", err)
		}}
	projectionWriter.Publications, projectionWriter.PullRequests = store, publicationService
	return &gitProjectionRuntime{store: store, coordinator: coordinator, writer: &projectionOperationWriter{projection: projectionWriter},
		publications: publicationReconciler, policy: policy, policyDigest: policyDigest, headVerifier: verifier, writeManager: writeManager,
		identity: identity, workerID: workerLeaseOwner(processIdentity, "git-runtime"), startedAt: time.Now().UTC(), sslip: sslipResolver, scheduling: schedulingStore, middleware: middlewareStore}, nil
}

func (r *gitProjectionRuntime) Run(ctx context.Context) error {
	if r == nil || r.coordinator == nil || r.publications == nil || r.store == nil || r.writer == nil || r.writer.projection == nil || r.policy == nil ||
		r.headVerifier.Client == nil || r.writeManager == nil ||
		r.identity.Validate() != nil || r.workerID == "" || r.startedAt.IsZero() {
		return fmt.Errorf("Git projection runtime is not configured")
	}
	observedAt := time.Now().UTC()
	lease, err := r.store.AcquireRuntimeReadiness(ctx, gitprojection.RuntimeWorkerObservation{WorkerID: r.workerID,
		RuntimeIdentity: r.identity, StartedAt: r.startedAt, ObservedAt: observedAt}, gitprojection.RuntimeReadinessLease)
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errors := make(chan error, 3)
	go func() { errors <- r.coordinator.Run(runCtx) }()
	go func() { errors <- r.publications.Run(runCtx) }()
	go func() { errors <- r.heartbeat(runCtx, lease) }()
	first := <-errors
	cancel()
	second := <-errors
	third := <-errors
	if first != nil && first != context.Canceled {
		return first
	}
	if second != nil && second != context.Canceled {
		return second
	}
	if third != nil && third != context.Canceled {
		return third
	}
	return ctx.Err()
}

func (r *gitProjectionRuntime) heartbeat(ctx context.Context, lease gitprojection.RuntimeLease) error {
	ticker := time.NewTicker(gitprojection.RuntimeHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case observedAt := <-ticker.C:
			updated, err := r.store.HeartbeatRuntimeReadiness(ctx, lease, observedAt.UTC(), gitprojection.RuntimeReadinessLease)
			if err != nil {
				return fmt.Errorf("Git projection readiness heartbeat: %w", err)
			}
			lease = updated
		}
	}
}

func (r *gitProjectionRuntime) Close() {
	if r != nil {
		if r.sslip != nil {
			r.sslip.Close()
		}
		if r.scheduling != nil {
			r.scheduling.Close()
		}
		if r.middleware != nil {
			r.middleware.Close()
		}
		if r.store != nil {
			r.store.Close()
		}
	}
}
