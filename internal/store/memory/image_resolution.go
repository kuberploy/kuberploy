package memory

import (
	"context"
	"sort"

	accesspolicy "github.com/kuberploy/kuberploy/internal/access"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/imageresolution"
	base "github.com/kuberploy/kuberploy/internal/store"
)

// AuthorizedImageSourcesForActor returns a bounded deployment-authorized
// registry policy catalog. It intentionally returns no global target list and
// no credential coordinates chosen by the caller.
func (s *Store) AuthorizedImageSourcesForActor(_ context.Context, actor, applicationID, environmentID string) ([]imageresolution.AuthorizedSource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	environment, environmentOK := s.environments[environmentID]
	application, applicationOK := s.applications[applicationID]
	project, projectOK := s.projects[environment.ProjectID]
	if !environmentOK || !applicationOK || !projectOK || environment.ProjectID != application.ProjectID {
		return nil, base.ErrNotFound
	}
	target := domain.AccessTarget{Type: "deployment", TeamID: project.TeamID, ProjectID: project.ID, EnvironmentID: environment.ID,
		Namespace: environment.Namespace, ApplicationID: application.ID}
	bindings := s.bindingsLocked(actor)
	if !accesspolicy.HasPermission(bindings, target, domain.PermissionResourcesRead) {
		return nil, base.ErrNotFound
	}
	if !accesspolicy.HasPermission(bindings, target, domain.PermissionResourcesWrite) {
		return nil, base.ErrForbidden
	}
	policies := make([]domain.ServiceRegistryPolicy, 0)
	for _, policy := range s.registryPolicies {
		if policy.ServiceID == applicationID {
			policies = append(policies, policy)
		}
	}
	sort.Slice(policies, func(i, j int) bool { return policies[i].RegistryTargetID < policies[j].RegistryTargetID })
	if len(policies) > imageresolution.MaximumAuthorizedSources {
		return nil, imageresolution.ErrConflict
	}
	sources := make([]imageresolution.AuthorizedSource, 0, len(policies))
	for _, policy := range policies {
		target, ok := s.registryTargets[policy.RegistryTargetID]
		if !ok {
			return nil, imageresolution.ErrConflict
		}
		sources = append(sources, imageresolution.AuthorizedSource{Target: target, Policy: policy})
	}
	return sources, nil
}
