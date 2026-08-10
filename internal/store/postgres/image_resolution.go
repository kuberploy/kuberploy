package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	accesspolicy "github.com/kuberploy/kuberploy/internal/access"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/imageresolution"
	base "github.com/kuberploy/kuberploy/internal/store"
)

// AuthorizedImageSourcesForActor mirrors deployment admission authorization
// in one repeatable-read snapshot and bounds the registry policy fan-out before
// any provider or credential is reached.
func (s *Store) AuthorizedImageSourcesForActor(ctx context.Context, actor, applicationID, environmentID string) ([]imageresolution.AuthorizedSource, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var projectID string
	err = tx.QueryRow(ctx, `SELECT e.project_id FROM environments e
		JOIN applications a ON a.project_id=e.project_id
		WHERE e.id=$1 AND a.id=$2`, environmentID, applicationID).Scan(&projectID)
	if err != nil {
		return nil, classify(err)
	}
	target, err := resolveAccessTarget(ctx, tx, domain.AccessTarget{Type: "environment", ID: environmentID})
	if err != nil {
		return nil, err
	}
	if target.ProjectID != projectID {
		return nil, imageresolution.ErrConflict
	}
	target.Type, target.ApplicationID = "deployment", applicationID
	bindings, err := effectiveBindings(ctx, tx, actor)
	if err != nil {
		return nil, err
	}
	if !accesspolicy.HasPermission(bindings, target, domain.PermissionResourcesRead) {
		return nil, base.ErrNotFound
	}
	if !accesspolicy.HasPermission(bindings, target, domain.PermissionResourcesWrite) {
		return nil, base.ErrForbidden
	}
	rows, err := tx.Query(ctx, `SELECT
		t.id::text,t.name,t.mode,t.endpoint,t.repository_prefix,t.pull_credential_ref,
		t.push_credential_ref,t.cache_credential_ref,t.created_at,t.updated_at,
		p.registry_target_id::text,p.service_id::text,p.repository,p.keep_last_successful,
		p.minimum_safety_age_seconds,p.cache_keep_generations,p.cache_unused_expiry_seconds,
		p.cache_byte_quota,p.created_at,p.updated_at
		FROM service_registry_policies p
		JOIN registry_targets t ON t.id=p.registry_target_id
		WHERE p.service_id=$1
		ORDER BY p.registry_target_id
		LIMIT $2`, applicationID, imageresolution.MaximumAuthorizedSources+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sources := make([]imageresolution.AuthorizedSource, 0)
	for rows.Next() {
		var registryTarget domain.RegistryTarget
		var policy domain.ServiceRegistryPolicy
		var minimumSafetyAgeSeconds, cacheUnusedExpirySeconds int64
		scanErr := rows.Scan(
			&registryTarget.ID, &registryTarget.Name, &registryTarget.Mode, &registryTarget.Endpoint, &registryTarget.RepositoryPrefix,
			&registryTarget.PullCredentialRef, &registryTarget.PushCredentialRef, &registryTarget.CacheCredentialRef,
			&registryTarget.CreatedAt, &registryTarget.UpdatedAt,
			&policy.RegistryTargetID, &policy.ServiceID, &policy.Repository, &policy.KeepLastSuccessful,
			&minimumSafetyAgeSeconds, &policy.CacheKeepGenerations, &cacheUnusedExpirySeconds,
			&policy.CacheByteQuota, &policy.CreatedAt, &policy.UpdatedAt,
		)
		if scanErr != nil {
			return nil, scanErr
		}
		policy.MinimumSafetyAge = time.Duration(minimumSafetyAgeSeconds) * time.Second
		policy.CacheUnusedExpiry = time.Duration(cacheUnusedExpirySeconds) * time.Second
		sources = append(sources, imageresolution.AuthorizedSource{Target: registryTarget, Policy: policy})
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(sources) > imageresolution.MaximumAuthorizedSources {
		return nil, imageresolution.ErrConflict
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return sources, nil
}
