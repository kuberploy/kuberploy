package httpapi

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strconv"

	"github.com/kuberploy/kuberploy/internal/builder"
	"github.com/kuberploy/kuberploy/internal/builds"
	"github.com/kuberploy/kuberploy/internal/domain"
	registrycore "github.com/kuberploy/kuberploy/internal/registry"
	"github.com/kuberploy/kuberploy/internal/store"
)

var (
	buildResolverUUIDRE     = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	buildResolverKubeNameRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?$`)
	buildRepositoryPrefixRE = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`)
	buildRegistryServerRE   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]*(?::[0-9]{1,5})?$`)
)

type BuildDefinitionCatalog interface {
	GetApplication(context.Context, string) (domain.Application, error)
	GetProject(context.Context, string) (domain.Project, error)
	Authorize(context.Context, string, domain.Permission, domain.AccessTarget) error
	RegistryTarget(context.Context, string) (domain.RegistryTarget, error)
	ServiceRegistryPolicy(context.Context, string, string) (domain.ServiceRegistryPolicy, error)
}

type ServerBuildDefinitionResolver struct {
	Catalog  BuildDefinitionCatalog
	Runtime  builds.WorkerRuntimeConfig
	Settings builds.BuilderPlatformSettingsReader
}

func (r *ServerBuildDefinitionResolver) SecretProfileCatalog(applicationID string) (builds.BuildSecretProfileCatalog, error) {
	if r == nil {
		return builds.BuildSecretProfileCatalog{}, builds.ErrInvalid
	}
	return r.Runtime.SecretProfileCatalog(applicationID)
}

func (r *ServerBuildDefinitionResolver) ResolveSecretProfiles(applicationID string, buildIDs, sshIDs []string) (builds.BuildSecretSelection, error) {
	if r == nil {
		return builds.BuildSecretSelection{}, builds.ErrInvalid
	}
	return r.Runtime.ResolveSecretProfiles(applicationID, buildIDs, sshIDs)
}

func (r *ServerBuildDefinitionResolver) ResolveSecretFiles(applicationID string, secretFiles, sshFiles []builder.FileReference) (builds.BuildSecretSelection, error) {
	if r == nil {
		return builds.BuildSecretSelection{}, builds.ErrInvalid
	}
	return r.Runtime.ResolveSecretFiles(applicationID, secretFiles, sshFiles)
}

func (r *ServerBuildDefinitionResolver) ResolveBuildDefinition(ctx context.Context, actorID, projectID, applicationID, registryTargetID string) (BuildDefinitionResolution, error) {
	if r == nil || r.Catalog == nil || !buildResolverUUIDRE.MatchString(actorID) || !buildResolverUUIDRE.MatchString(projectID) ||
		!buildResolverUUIDRE.MatchString(applicationID) || !buildResolverUUIDRE.MatchString(registryTargetID) {
		return BuildDefinitionResolution{}, builds.ErrInvalid
	}
	application, err := r.Catalog.GetApplication(ctx, applicationID)
	if err != nil {
		return BuildDefinitionResolution{}, buildResolverStoreError(err)
	}
	project, err := r.Catalog.GetProject(ctx, projectID)
	if err != nil {
		return BuildDefinitionResolution{}, buildResolverStoreError(err)
	}
	if application.ID != applicationID || application.ProjectID != project.ID || project.ID != projectID {
		return BuildDefinitionResolution{}, builds.ErrUnauthorized
	}
	if err = r.Catalog.Authorize(ctx, actorID, domain.PermissionBuildsManage, domain.AccessTarget{Type: "application", ID: application.ID}); err != nil {
		return BuildDefinitionResolution{}, buildResolverStoreError(err)
	}
	target, err := r.Catalog.RegistryTarget(ctx, registryTargetID)
	if err != nil {
		return BuildDefinitionResolution{}, buildResolverStoreError(err)
	}
	if target.ID != registryTargetID {
		return BuildDefinitionResolution{}, builds.ErrInfrastructure
	}
	policy, err := r.Catalog.ServiceRegistryPolicy(ctx, target.ID, application.ID)
	if err != nil {
		return BuildDefinitionResolution{}, buildResolverStoreError(err)
	}
	expectedRepository := target.RepositoryPrefix + "/projects/" + project.ID + "/services/" + application.ID + "/image"
	if registrycore.ValidatePolicyForTarget(target, policy) != nil || policy.RegistryTargetID != target.ID ||
		policy.ServiceID != application.ID || policy.Repository != expectedRepository {
		return BuildDefinitionResolution{}, builds.ErrInfrastructure
	}
	registry, port, err := strictBuildRegistryBinding(target)
	if err != nil {
		return BuildDefinitionResolution{}, err
	}
	platform := builds.DefaultBuilderPlatformSettings(r.Runtime)
	if r.Settings != nil {
		platform, err = r.Settings.Current(ctx)
		if err != nil {
			return BuildDefinitionResolution{}, builds.ErrInfrastructure
		}
	}
	execution, err := r.Runtime.ExecutionSettingsForPlatform(port, platform)
	if err != nil {
		return BuildDefinitionResolution{}, builds.ErrInfrastructure
	}
	return BuildDefinitionResolution{Registry: registry, Execution: execution}, nil
}

func strictBuildRegistryBinding(target domain.RegistryTarget) (builds.RegistryBinding, int, error) {
	if !buildResolverUUIDRE.MatchString(target.ID) || len(target.RepositoryPrefix) > 160 || !buildRepositoryPrefixRE.MatchString(target.RepositoryPrefix) ||
		!buildResolverKubeNameRE.MatchString(target.PushCredentialRef) ||
		!buildResolverKubeNameRE.MatchString(target.CacheCredentialRef) ||
		target.PushCredentialRef == target.CacheCredentialRef {
		return builds.RegistryBinding{}, 0, builds.ErrInfrastructure
	}
	server, err := builds.RegistryServer(target.Endpoint)
	if err != nil || !buildRegistryServerRE.MatchString(server) {
		return builds.RegistryBinding{}, 0, builds.ErrInfrastructure
	}
	u, err := url.Parse("https://" + server)
	if err != nil || u.Host != server || u.Hostname() == "" {
		return builds.RegistryBinding{}, 0, builds.ErrInfrastructure
	}
	port := 443
	if rawPort := u.Port(); rawPort != "" {
		parsed, parseErr := strconv.Atoi(rawPort)
		if parseErr != nil || parsed < 1 || parsed > 65535 {
			return builds.RegistryBinding{}, 0, builds.ErrInfrastructure
		}
		port = parsed
	}
	mode := builds.RegistryMode(target.Mode)
	if mode != builds.RegistryManaged && mode != builds.RegistryExternal {
		return builds.RegistryBinding{}, 0, builds.ErrInfrastructure
	}
	return builds.RegistryBinding{TargetID: target.ID, Mode: mode, Server: server,
		RepositoryPrefix: target.RepositoryPrefix, PushCredentialSecret: target.PushCredentialRef,
		CacheCredentialSecret: target.CacheCredentialRef}, port, nil
}

func buildResolverStoreError(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return builds.ErrNotFound
	case errors.Is(err, store.ErrForbidden):
		return builds.ErrUnauthorized
	default:
		return builds.ErrInfrastructure
	}
}

var _ BuildDefinitionResolver = (*ServerBuildDefinitionResolver)(nil)
