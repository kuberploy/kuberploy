package httpapi

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/builder"
	"github.com/kuberploy/kuberploy/internal/builds"
	platformconfig "github.com/kuberploy/kuberploy/internal/config"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/githubapp"
	"github.com/kuberploy/kuberploy/internal/store"
)

type resolverCatalog struct {
	application domain.Application
	project     domain.Project
	target      domain.RegistryTarget
	policy      domain.ServiceRegistryPolicy
	policyErr   error
	authorize   error
}

type unusedDefinitionResolver struct{}

type staticBuilderSettings struct{ value builds.BuilderPlatformSettings }

func (s staticBuilderSettings) Current(context.Context) (builds.BuilderPlatformSettings, error) {
	return s.value, nil
}

func (unusedDefinitionResolver) ResolveBuildDefinition(context.Context, string, string, string, string) (BuildDefinitionResolution, error) {
	return BuildDefinitionResolution{}, builds.ErrInfrastructure
}

func (c *resolverCatalog) GetApplication(context.Context, string) (domain.Application, error) {
	return c.application, nil
}
func (c *resolverCatalog) GetProject(context.Context, string) (domain.Project, error) {
	return c.project, nil
}
func (c *resolverCatalog) Authorize(_ context.Context, _ string, permission domain.Permission, target domain.AccessTarget) error {
	if permission != domain.PermissionBuildsManage || target.Type != "application" || target.ID != c.application.ID {
		return store.ErrForbidden
	}
	return c.authorize
}
func (c *resolverCatalog) RegistryTarget(context.Context, string) (domain.RegistryTarget, error) {
	return c.target, nil
}
func (c *resolverCatalog) ServiceRegistryPolicy(context.Context, string, string) (domain.ServiceRegistryPolicy, error) {
	return c.policy, c.policyErr
}

func TestServerBuildDefinitionResolverDerivesClosedOperatorSettings(t *testing.T) {
	actorID := "11111111-1111-4111-8111-111111111111"
	projectID := "22222222-2222-4222-8222-222222222222"
	applicationID := "33333333-3333-4333-8333-333333333333"
	targetID := "44444444-4444-4444-8444-444444444444"
	runtime, err := builds.WorkerRuntimeConfigFromLookup(func(name string) (string, bool) {
		values := map[string]string{
			builds.GitHubBuildsEnabledEnv: "true", builds.GitHubAppIDEnv: "12345", builds.GitHubAppClientIDEnv: "Iv1_KuberployClient",
			builds.BuilderNamespaceEnv: "kuberploy-build-dind", builds.BuilderPodServiceAccountEnv: "kuberploy-build-pod",
			builds.BuilderAgentImageEnv:          "ghcr.io/kuberploy/builder@sha256:" + strings.Repeat("a", 64),
			builds.BuilderBuildKitImageEnv:       builder.DefaultBuildKitImage,
			platformconfig.KubeAPIServerCIDRsEnv: "10.43.0.1/32",
			builds.BuilderSourceEgressCIDRsEnv:   "192.0.2.10/32", builds.BuilderRegistryEgressCIDRsEnv: "192.0.2.20/32",
		}
		value, ok := values[name]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	target := domain.RegistryTarget{ID: targetID, Name: "managed", Mode: domain.RegistryTargetManaged, Endpoint: "https://registry.example.test:5000", RepositoryPrefix: "tenant/builds", PullCredentialRef: "registry-pull", PushCredentialRef: "registry-push", CacheCredentialRef: "registry-cache"}
	policy := domain.ServiceRegistryPolicy{RegistryTargetID: targetID, ServiceID: applicationID,
		Repository:         "tenant/builds/projects/" + projectID + "/services/" + applicationID + "/image",
		KeepLastSuccessful: 2, MinimumSafetyAge: time.Hour, CacheKeepGenerations: 2, CacheUnusedExpiry: 24 * time.Hour, CacheByteQuota: 1 << 30, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	catalog := &resolverCatalog{application: domain.Application{ID: applicationID, ProjectID: projectID}, project: domain.Project{ID: projectID}, target: target, policy: policy}
	platform := builds.DefaultBuilderPlatformSettings(runtime)
	platform.NodeIsolation = true
	platform.DinDResources.CPULimit = "6"
	resolver := &ServerBuildDefinitionResolver{Catalog: catalog, Runtime: runtime, Settings: staticBuilderSettings{value: platform}}
	resolution, err := resolver.ResolveBuildDefinition(context.Background(), actorID, projectID, applicationID, targetID)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Registry.Server != "registry.example.test:5000" || resolution.Registry.PushCredentialSecret != "registry-push" || resolution.Registry.CacheCredentialSecret != "registry-cache" || resolution.Execution.Namespace != "kuberploy-build-dind" ||
		len(resolution.Execution.Egress) != 2 || resolution.Execution.Egress[1].Ports[0] != 5000 ||
		resolution.Execution.NodeSelector["kuberploy.io/node-class"] != "dind-builder" || resolution.Execution.DinDResources.CPULimit != "6" {
		t.Fatalf("resolution=%#v", resolution)
	}
	catalog.application.ProjectID = "55555555-5555-4555-8555-555555555555"
	if _, err = resolver.ResolveBuildDefinition(context.Background(), actorID, projectID, applicationID, targetID); !errors.Is(err, builds.ErrUnauthorized) {
		t.Fatalf("cross-project app accepted: %v", err)
	}
}

func TestServerBuildDefinitionResolverRequiresExactApplicationPolicy(t *testing.T) {
	actorID := "11111111-1111-4111-8111-111111111111"
	projectID := "22222222-2222-4222-8222-222222222222"
	applicationID := "33333333-3333-4333-8333-333333333333"
	targetID := "44444444-4444-4444-8444-444444444444"
	runtime, err := builds.WorkerRuntimeConfigFromLookup(func(name string) (string, bool) {
		values := map[string]string{
			builds.GitHubBuildsEnabledEnv: "true", builds.GitHubAppIDEnv: "12345", builds.GitHubAppClientIDEnv: "Iv1_KuberployClient",
			builds.BuilderNamespaceEnv: "kuberploy-build-dind", builds.BuilderPodServiceAccountEnv: "kuberploy-build-pod",
			builds.BuilderAgentImageEnv: "ghcr.io/kuberploy/builder@sha256:" + strings.Repeat("a", 64), builds.BuilderBuildKitImageEnv: builder.DefaultBuildKitImage,
			platformconfig.KubeAPIServerCIDRsEnv: "10.43.0.1/32",
			builds.BuilderSourceEgressCIDRsEnv:   "192.0.2.10/32", builds.BuilderRegistryEgressCIDRsEnv: "192.0.2.20/32",
		}
		value, ok := values[name]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	target := domain.RegistryTarget{ID: targetID, Name: "managed", Mode: domain.RegistryTargetManaged, Endpoint: "https://registry.example.test", RepositoryPrefix: "tenant", PullCredentialRef: "pull", PushCredentialRef: "push", CacheCredentialRef: "cache"}
	catalog := &resolverCatalog{application: domain.Application{ID: applicationID, ProjectID: projectID}, project: domain.Project{ID: projectID}, target: target,
		policy: domain.ServiceRegistryPolicy{RegistryTargetID: targetID, ServiceID: applicationID, Repository: "tenant/attacker/image", KeepLastSuccessful: 2, MinimumSafetyAge: time.Hour, CacheKeepGenerations: 2, CacheUnusedExpiry: 24 * time.Hour, CacheByteQuota: 1 << 30, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}}
	resolver := &ServerBuildDefinitionResolver{Catalog: catalog, Runtime: runtime}
	if _, err = resolver.ResolveBuildDefinition(context.Background(), actorID, projectID, applicationID, targetID); !errors.Is(err, builds.ErrInfrastructure) {
		t.Fatalf("mismatched application policy accepted: %v", err)
	}
	catalog.policyErr = store.ErrNotFound
	if _, err = resolver.ResolveBuildDefinition(context.Background(), actorID, projectID, applicationID, targetID); !errors.Is(err, builds.ErrNotFound) {
		t.Fatalf("missing application policy accepted: %v", err)
	}
}

func TestServerBuildDefinitionResolverScopesSecretProfilesByApplication(t *testing.T) {
	applicationID := "33333333-3333-4333-8333-333333333333"
	otherApplicationID := "55555555-5555-4555-8555-555555555555"
	resolver := &ServerBuildDefinitionResolver{Runtime: builds.WorkerRuntimeConfig{
		BuildSecretProfiles: []builds.BuildSecretProfile{{ID: "npmrc", Label: "Private npm registry", ApplicationIDs: []string{applicationID}, File: builder.FileReference{ID: "npmrc", Path: builder.BuildSecretRoot + "/npmrc"}}},
		SSHSecretProfiles:   []builds.BuildSecretProfile{{ID: "github", Label: "GitHub deploy key", ApplicationIDs: []string{otherApplicationID}, File: builder.FileReference{ID: "github", Path: builder.SSHSecretRoot + "/id_ed25519"}}},
	}}
	catalog, err := resolver.SecretProfileCatalog(applicationID)
	if err != nil || len(catalog.Build) != 1 || len(catalog.SSH) != 0 {
		t.Fatalf("catalog=%#v err=%v", catalog, err)
	}
	if _, err = resolver.ResolveSecretProfiles(otherApplicationID, []string{"npmrc"}, nil); !errors.Is(err, builds.ErrInvalid) {
		t.Fatalf("cross-application profile accepted: %v", err)
	}
}

func TestServerBuildDefinitionResolverFailsClosedOnSharedRegistryCredentialAuthority(t *testing.T) {
	target := domain.RegistryTarget{ID: "44444444-4444-4444-8444-444444444444", Mode: domain.RegistryTargetExternal, Endpoint: "registry.example.test",
		RepositoryPrefix: "tenant", PushCredentialRef: "push-secret", CacheCredentialRef: "push-secret"}
	if _, _, err := strictBuildRegistryBinding(target); !errors.Is(err, builds.ErrInfrastructure) {
		t.Fatalf("shared credentials accepted: %v", err)
	}
	target.CacheCredentialRef = "cache-secret"
	target.Endpoint = "http://registry.example.test"
	if _, _, err := strictBuildRegistryBinding(target); !errors.Is(err, builds.ErrInfrastructure) {
		t.Fatalf("HTTP registry accepted: %v", err)
	}
}

func TestProductionBuildBackendResolvesOnlyActiveVerifiedRepositoryIdentity(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	store := builds.NewMemoryStore()
	installation := builds.Installation{
		ID: "11111111-1111-4111-8111-111111111111", AppID: 123, GitHubInstallationID: 456,
		Account:             githubapp.AccountIdentity{ID: 789, Login: "example", Type: "Organization"},
		RepositorySelection: "selected", Permissions: githubapp.Permissions{"metadata": githubapp.PermissionRead, "contents": githubapp.PermissionRead},
		Lifecycle: builds.InstallationActive, LastVerifiedAt: now, UpdatedAt: now,
	}
	repository := builds.Repository{
		ID: "22222222-2222-4222-8222-222222222222", InstallationID: installation.ID,
		Identity:  githubapp.RepositoryIdentity{ID: 9001, OwnerID: installation.Account.ID, OwnerLogin: "example", Name: "platform-config"},
		Lifecycle: builds.RepositoryActive, LastVerifiedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.PutInstallation(ctx, installation); err != nil {
		t.Fatal(err)
	}
	if err := store.PutRepository(ctx, repository); err != nil {
		t.Fatal(err)
	}
	backend, err := NewBuildBackend(store, unusedDefinitionResolver{})
	if err != nil {
		t.Fatal(err)
	}
	resolver, ok := backend.(GitBindingRepositoryResolver)
	if !ok {
		t.Fatal("production build backend does not expose the narrow Git binding catalog")
	}
	resolved, err := resolver.ResolveGitBindingRepository(ctx, installation.ID, repository.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.GitHubAppID != installation.AppID || resolved.Repository.InstallationID != installation.GitHubInstallationID ||
		resolved.Repository.RepositoryID != repository.Identity.ID || resolved.Repository.Owner != repository.Identity.OwnerLogin ||
		resolved.Repository.Name != repository.Identity.Name {
		t.Fatalf("resolved identity=%#v", resolved)
	}
	if _, err = resolver.ResolveGitBindingRepository(ctx, installation.ID, "33333333-3333-4333-8333-333333333333"); !errors.Is(err, builds.ErrNotFound) {
		t.Fatalf("unknown repository error=%v", err)
	}
	suspendedAt := now.Add(time.Second)
	installation.Lifecycle = builds.InstallationSuspended
	installation.SuspendedAt = &suspendedAt
	installation.UpdatedAt = suspendedAt
	if err = store.PutInstallation(ctx, installation); err != nil {
		t.Fatal(err)
	}
	if _, err = resolver.ResolveGitBindingRepository(ctx, installation.ID, repository.ID); !errors.Is(err, builds.ErrUnauthorized) {
		t.Fatalf("suspended installation resolved: %v", err)
	}
}
