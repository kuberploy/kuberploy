package postgres

import (
	"context"
	"time"

	"github.com/kuberploy/kuberploy/internal/deploymentrollback"
	"github.com/kuberploy/kuberploy/internal/domain"
)

// VerifyRetainedDeploymentImage is a metadata-only retained-root check. It
// does not contact a registry or accept registry coordinates from the caller.
func (s *Store) VerifyRetainedDeploymentImage(ctx context.Context, applicationID, image string) (bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT t.endpoint,r.repository,r.root_digest,r.availability,r.availability_observed_at
		FROM registry_releases r JOIN registry_targets t ON t.id=r.registry_target_id
		WHERE r.service_id=$1 ORDER BY r.id`, applicationID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	managed := false
	for rows.Next() {
		var endpoint, repository, digest string
		var availability domain.RegistryArtifactAvailability
		var observedAt *time.Time
		if err = rows.Scan(&endpoint, &repository, &digest, &availability, &observedAt); err != nil {
			return false, err
		}
		if !deploymentrollback.MatchesRegistryArtifact(image, endpoint, repository, digest) {
			continue
		}
		managed = true
		if availability == domain.RegistryArtifactPresent && observedAt == nil {
			return true, nil
		}
	}
	if err = rows.Err(); err != nil {
		return false, err
	}
	if managed {
		return true, deploymentrollback.ErrArtifactUnavailable
	}
	return false, nil
}
