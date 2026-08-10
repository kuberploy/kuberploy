package memory

import (
	"errors"
	"testing"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/imageresolution"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func TestAuthorizedImageSourcesAreExactDeploymentScopedAndBounded(t *testing.T) {
	store := New()
	projectID := "11111111-1111-4111-8111-111111111111"
	applicationID := "22222222-2222-4222-8222-222222222222"
	environmentID := "33333333-3333-4333-8333-333333333333"
	targetID := "44444444-4444-4444-8444-444444444444"
	store.projects[projectID] = domain.Project{ID: projectID, TeamID: "team-a", Name: "project"}
	store.applications[applicationID] = domain.Application{ID: applicationID, ProjectID: projectID, Name: "service"}
	store.environments[environmentID] = domain.Environment{ID: environmentID, ProjectID: projectID, Namespace: "project-dev", Name: "dev"}
	store.registryTargets[targetID] = domain.RegistryTarget{ID: targetID, Mode: domain.RegistryTargetExternal, Endpoint: "https://registry.example.test", RepositoryPrefix: "tenant"}
	store.registryPolicies[registryScopeKey(targetID, applicationID)] = domain.ServiceRegistryPolicy{RegistryTargetID: targetID, ServiceID: applicationID, Repository: "tenant/service"}
	grantRegistryActor(store, "deployer", domain.RoleDeveloper, domain.ScopeProject, projectID)
	grantRegistryActor(store, "viewer", domain.RoleViewer, domain.ScopeProject, projectID)

	sources, err := store.AuthorizedImageSourcesForActor(t.Context(), "deployer", applicationID, environmentID)
	if err != nil || len(sources) != 1 || sources[0].Target.ID != targetID || sources[0].Policy.Repository != "tenant/service" {
		t.Fatalf("sources=%+v err=%v", sources, err)
	}
	if _, err = store.AuthorizedImageSourcesForActor(t.Context(), "viewer", applicationID, environmentID); !errors.Is(err, base.ErrForbidden) {
		t.Fatalf("viewer err=%v", err)
	}
	otherEnvironment := "55555555-5555-4555-8555-555555555555"
	store.environments[otherEnvironment] = domain.Environment{ID: otherEnvironment, ProjectID: "other-project", Namespace: "other-dev"}
	if _, err = store.AuthorizedImageSourcesForActor(t.Context(), "deployer", applicationID, otherEnvironment); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("cross-project err=%v", err)
	}

	store.mu.Lock()
	for index := 0; index < imageresolution.MaximumAuthorizedSources; index++ {
		id := boundedTargetID(index)
		store.registryTargets[id] = domain.RegistryTarget{ID: id, Mode: domain.RegistryTargetExternal, Endpoint: "https://registry.example.test", RepositoryPrefix: "tenant"}
		store.registryPolicies[registryScopeKey(id, applicationID)] = domain.ServiceRegistryPolicy{RegistryTargetID: id, ServiceID: applicationID, Repository: "tenant/service"}
	}
	store.mu.Unlock()
	if _, err = store.AuthorizedImageSourcesForActor(t.Context(), "deployer", applicationID, environmentID); !errors.Is(err, imageresolution.ErrConflict) {
		t.Fatalf("unbounded catalog err=%v", err)
	}
}

func boundedTargetID(index int) string {
	const digits = "0123456789abcdef"
	return "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaa" + string([]byte{digits[index/16], digits[index%16]})
}
