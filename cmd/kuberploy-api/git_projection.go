package main

import (
	"context"
	"errors"
	"time"

	"github.com/kuberploy/kuberploy/internal/certificates"
	"github.com/kuberploy/kuberploy/internal/certissuers"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/edge"
	"github.com/kuberploy/kuberploy/internal/externaldns"
	"github.com/kuberploy/kuberploy/internal/githubapp"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/imagepull"
	"github.com/kuberploy/kuberploy/internal/projectionpolicy"
	"github.com/kuberploy/kuberploy/internal/secrets"
)

type gitProjectionAPI struct {
	store      *gitprojection.PostgreSQLStore
	backend    *gitprojection.ControlPlane
	readiness  *gitprojection.RuntimeReadinessProbe
	catalog    gitprojection.DeploymentCatalog
	provider   gitprojection.HeadVerifier
	protection gitprojection.RepositoryProtectionVerifier
}

func newGitProjectionAPI(ctx context.Context, databaseURL string, config gitprojection.RuntimeConfig, secretConfig secrets.RuntimeConfig, certificateConfig certificates.ObservationConfig, issuerConfig certissuers.ObserverConfig, registryPullConfig imagepull.RuntimeConfig, edgeConfig edge.RuntimeConfig, externalDNSConfig externaldns.OperationalConfig, catalog gitprojection.DeploymentCatalog) (*gitProjectionAPI, error) {
	if _, err := certificates.ObservationPolicyDigest(certificateConfig); err != nil {
		return nil, certificates.ErrObservationUnavailable
	}
	if certificateConfig.Enabled && !config.Enabled {
		return nil, gitprojection.ErrInvalid
	}
	if issuerConfig.Validate() != nil || issuerConfig.Enabled && !config.Enabled {
		return nil, certissuers.ErrObservationUnavailable
	}
	if !config.Enabled {
		return nil, nil
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	policyDigest, err := projectionpolicy.RuntimePolicyDigest(secretConfig, certificateConfig, issuerConfig, registryPullConfig, edgeConfig, externalDNSConfig)
	if err != nil {
		return nil, err
	}
	identity, err := gitprojection.RuntimeIdentityForConfig(config, policyDigest)
	if err != nil {
		return nil, err
	}
	store, err := gitprojection.OpenPostgreSQLStore(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	readiness := &gitprojection.RuntimeReadinessProbe{Store: store, Identity: identity}
	providerClient, err := githubapp.NewProjectedClient(config.GitHub)
	if err != nil {
		store.Close()
		return nil, err
	}
	headVerifier := gitprojection.GitHubHeadVerifier{AppID: config.GitHub.AppID, Authorizations: store, Client: providerClient}
	protection := gitprojection.GitHubRepositoryProtectionVerifier{AppID: config.GitHub.AppID, Authorizations: store, Client: providerClient}
	backend := &gitprojection.ControlPlane{Catalog: catalog, Store: store, ChartDigest: config.ChartDigest, PolicyVersion: config.PolicyVersion}
	return &gitProjectionAPI{store: store, backend: backend, readiness: readiness, catalog: catalog, provider: headVerifier, protection: protection}, nil
}

func (a *gitProjectionAPI) PlanMutation(ctx context.Context, actor, environmentID, applicationID, expectedETag string) (gitprojection.WritePlan, error) {
	plan, err := a.backend.PlanMutation(ctx, actor, environmentID, applicationID, expectedETag)
	if err != nil {
		return gitprojection.WritePlan{}, err
	}
	environment, err := a.catalog.GetEnvironmentForActor(ctx, actor, environmentID)
	if err != nil {
		return gitprojection.WritePlan{}, err
	}
	if environment.ProtectionPolicy == domain.EnvironmentDevelopment {
		return plan, nil
	}
	if environment.ProtectionPolicy != domain.EnvironmentProtected || a.provider == nil || a.protection == nil {
		return gitprojection.WritePlan{}, gitprojection.ErrProtectionUnavailable
	}
	binding, err := a.store.Binding(ctx, plan.BindingID)
	if err != nil {
		return gitprojection.WritePlan{}, err
	}
	startedAt := time.Now().UTC()
	head, err := a.provider.VerifyTargetHead(ctx, binding, gitprojection.ObservationWrite)
	if err != nil {
		return gitprojection.WritePlan{}, errors.Join(gitprojection.ErrProtectionUnavailable, err)
	}
	if _, err = a.protection.VerifyRepositoryProtection(ctx, binding, head, startedAt); err != nil {
		return gitprojection.WritePlan{}, errors.Join(gitprojection.ErrProtectionUnavailable, err)
	}
	return plan, nil
}

func (a *gitProjectionAPI) Bundle(ctx context.Context, actor string, deployment domain.Deployment, atLeastRevision string, wait time.Duration) (gitprojection.Bundle, error) {
	return a.backend.Bundle(ctx, actor, deployment, atLeastRevision, wait)
}

func (a *gitProjectionAPI) Close() {
	if a != nil && a.store != nil {
		a.store.Close()
	}
}
