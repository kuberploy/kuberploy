package memory

import (
	"context"

	"github.com/kuberploy/kuberploy/internal/deploymentrollback"
	"github.com/kuberploy/kuberploy/internal/domain"
)

// VerifyRetainedDeploymentImage treats images outside the Kuberploy release
// catalog as externally managed. Once an exact catalog release exists, at
// least one matching retained root must still be observed as present.
func (s *Store) VerifyRetainedDeploymentImage(_ context.Context, applicationID, image string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	managed := false
	for _, release := range s.registryReleases {
		if release.ServiceID != applicationID {
			continue
		}
		target, ok := s.registryTargets[release.RegistryTargetID]
		if !ok || !deploymentrollback.MatchesRegistryArtifact(image, target.Endpoint, release.Repository, release.RootDigest) {
			continue
		}
		managed = true
		if release.Availability == domain.RegistryArtifactPresent && release.AvailabilityObservedAt == nil {
			return true, nil
		}
	}
	if managed {
		return true, deploymentrollback.ErrArtifactUnavailable
	}
	return false, nil
}
