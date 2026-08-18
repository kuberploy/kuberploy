package argo

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

type PostgreSQLRuntimeBindingCatalog struct{ pool *pgxpool.Pool }

func NewPostgreSQLRuntimeBindingCatalog(pool *pgxpool.Pool) (*PostgreSQLRuntimeBindingCatalog, error) {
	if pool == nil {
		return nil, ErrInvalid
	}
	return &PostgreSQLRuntimeBindingCatalog{pool: pool}, nil
}

func (c *PostgreSQLRuntimeBindingCatalog) ArgoRepositoryBindings(
	ctx context.Context,
	appID int64,
	platformBindingID string,
	clusterID string,
	now time.Time,
	maximumAge time.Duration,
) ([]RepositoryBindingAuthority, error) {
	if c == nil || c.pool == nil || appID <= 0 || !uuidRE.MatchString(platformBindingID) || !uuidRE.MatchString(clusterID) ||
		now.IsZero() || maximumAge <= 0 || maximumAge > time.Hour {
		return nil, ErrInvalid
	}
	rows, err := c.pool.Query(ctx, `SELECT
		b.id::text,b.kind,b.scope_id::text,COALESCE(b.project_id::text,''),COALESCE(b.environment_id::text,''),COALESCE(b.cluster_id::text,''),
		b.provider,b.installation_id,b.repository_id,b.repository_owner,b.repository_name,b.target_ref,b.path_prefix,
		b.credential_mode,b.credential_secret_name,b.state,b.target_head_revision,b.indexed_revision,b.projection_generation,b.parser_version,
		b.target_head_observed_at,b.indexed_at,b.created_at,b.updated_at,
		CASE WHEN i.id IS NOT NULL AND r.id IS NOT NULL AND i.lifecycle='active' AND r.lifecycle='active'
			AND i.github_app_id=$1 AND i.github_installation_id=b.installation_id
			AND r.github_repository_id=b.repository_id AND r.github_owner_id=i.github_account_id
			AND lower(i.account_login)=lower(b.repository_owner) AND lower(r.owner_login)=lower(b.repository_owner)
			AND r.name=b.repository_name AND i.permissions->>'metadata' IN ('read','write')
			AND i.permissions->>'contents' IN ('read','write')
			AND i.last_verified_at IS NOT NULL AND r.last_verified_at IS NOT NULL
			AND i.last_verified_at BETWEEN $4 AND $3 AND r.last_verified_at BETWEEN $4 AND $3
		THEN true ELSE false END AS authorized,
		CASE WHEN i.id IS NOT NULL AND r.id IS NOT NULL AND i.lifecycle='active' AND r.lifecycle='active'
			AND i.github_app_id=$1 AND i.github_installation_id=b.installation_id
			AND r.github_repository_id=b.repository_id AND r.github_owner_id=i.github_account_id
			AND lower(i.account_login)=lower(b.repository_owner) AND lower(r.owner_login)=lower(b.repository_owner)
			AND r.name=b.repository_name AND i.permissions->>'metadata' IN ('read','write')
			AND i.permissions->>'contents' IN ('read','write')
		THEN false ELSE true END AS revocation_required,
		CASE WHEN i.id IS NOT NULL AND r.id IS NOT NULL AND i.lifecycle='active' AND r.lifecycle='active'
			AND i.github_app_id=$1 AND i.github_installation_id=b.installation_id
			AND r.github_repository_id=b.repository_id AND r.github_owner_id=i.github_account_id
			AND lower(i.account_login)=lower(b.repository_owner) AND lower(r.owner_login)=lower(b.repository_owner)
			AND r.name=b.repository_name AND i.permissions->>'metadata' IN ('read','write')
			AND i.permissions->>'contents' IN ('read','write')
			AND i.last_verified_at IS NOT NULL AND r.last_verified_at IS NOT NULL
		THEN LEAST(i.last_verified_at,r.last_verified_at) ELSE NULL END AS catalog_observed_at
	FROM git_repository_bindings b
	LEFT JOIN github_installations i ON i.github_installation_id=b.installation_id
	LEFT JOIN github_repositories r ON r.installation_id=i.id AND r.github_repository_id=b.repository_id
	WHERE b.credential_mode='github-app'
	  AND ((b.kind='platform' AND b.id=$2 AND b.cluster_id=$5) OR b.kind='environment')
	ORDER BY CASE WHEN b.kind='platform' THEN 0 ELSE 1 END,b.id
	LIMIT $6`, appID, platformBindingID, now.UTC(), now.UTC().Add(-maximumAge), clusterID, MaximumArgoRepositoryBindings+1)
	if err != nil {
		return nil, classifyPostgres(err)
	}
	defer rows.Close()
	authorities := make([]RepositoryBindingAuthority, 0)
	for rows.Next() {
		if len(authorities) >= MaximumArgoRepositoryBindings {
			return nil, ErrArgoRuntimePrerequisiteNotReady
		}
		binding, authority, scanErr := scanRuntimeBindingAuthority(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		authority.Binding = binding
		authorities = append(authorities, authority)
	}
	if err = rows.Err(); err != nil {
		return nil, classifyPostgres(err)
	}
	if len(authorities) == 0 {
		return nil, ErrArgoRuntimePrerequisiteNotReady
	}
	platforms := 0
	for _, authority := range authorities {
		if authority.Binding.Kind == gitprojection.BindingPlatform {
			if authority.Binding.ID != platformBindingID || authority.Binding.ClusterID != clusterID {
				return nil, ErrConflict
			}
			platforms++
		}
	}
	if platforms != 1 {
		return nil, ErrArgoRuntimePrerequisiteNotReady
	}
	return authorities, nil
}

// MarkArgoRepositoryBindingsVerified renews only identities that just passed
// VerifyInstallation and ResolveRemoteRef. It does not mint credentials or
// alter binding authority; it prevents a healthy runtime from repeating the
// same expensive provider proof on every readiness heartbeat.
func (c *PostgreSQLRuntimeBindingCatalog) MarkArgoRepositoryBindingsVerified(
	ctx context.Context, appID int64, bindings []gitprojection.Binding, now time.Time,
) error {
	if c == nil || c.pool == nil || appID <= 0 || len(bindings) == 0 || now.IsZero() {
		return ErrInvalid
	}
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return classifyPostgres(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, binding := range bindings {
		if binding.Validate() != nil || binding.CredentialMode != gitprojection.CredentialGitHubApp {
			return ErrInvalid
		}
		var installationID string
		err = tx.QueryRow(ctx, `UPDATE github_installations
			SET last_verified_at=$4,updated_at=GREATEST(updated_at,$4)
			WHERE github_app_id=$1 AND github_installation_id=$2 AND lifecycle='active'
			  AND EXISTS (
				SELECT 1 FROM github_repositories r
				WHERE r.installation_id=github_installations.id AND r.github_repository_id=$3
				  AND r.lifecycle='active' AND r.github_owner_id=github_installations.github_account_id
			  )
			RETURNING id::text`, appID, binding.Repository.InstallationID, binding.Repository.RepositoryID, now.UTC()).Scan(&installationID)
		if err != nil {
			return classifyPostgres(err)
		}
		var repositoryID string
		err = tx.QueryRow(ctx, `UPDATE github_repositories
			SET last_verified_at=$3,updated_at=GREATEST(updated_at,$3)
			WHERE installation_id=$1 AND github_repository_id=$2 AND github_owner_id=(
				SELECT github_account_id FROM github_installations WHERE id=$1
			) AND lifecycle='active'
			RETURNING id::text`, installationID, binding.Repository.RepositoryID, now.UTC()).Scan(&repositoryID)
		if err != nil {
			return classifyPostgres(err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return classifyPostgres(err)
	}
	return nil
}

func scanRuntimeBindingAuthority(row pgx.Row) (gitprojection.Binding, RepositoryBindingAuthority, error) {
	var binding gitprojection.Binding
	var targetHead, indexedRevision *string
	var targetObservedAt, indexedAt, catalogObservedAt *time.Time
	var authority RepositoryBindingAuthority
	err := row.Scan(
		&binding.ID, &binding.Kind, &binding.ScopeID, &binding.ProjectID, &binding.EnvironmentID, &binding.ClusterID,
		&binding.Repository.Provider, &binding.Repository.InstallationID, &binding.Repository.RepositoryID,
		&binding.Repository.Owner, &binding.Repository.Name, &binding.TargetRef, &binding.Prefix,
		&binding.CredentialMode, &binding.CredentialSecretName, &binding.State, &targetHead, &indexedRevision,
		&binding.ProjectionGeneration, &binding.ParserVersion, &targetObservedAt, &indexedAt, &binding.CreatedAt, &binding.UpdatedAt,
		&authority.Authorized, &authority.RevocationRequired, &catalogObservedAt,
	)
	if err != nil {
		return gitprojection.Binding{}, RepositoryBindingAuthority{}, classifyPostgres(err)
	}
	if targetHead != nil {
		binding.TargetHeadRevision = *targetHead
	}
	if indexedRevision != nil {
		binding.IndexedRevision = *indexedRevision
	}
	if targetObservedAt != nil {
		binding.TargetHeadObservedAt = targetObservedAt.UTC()
	}
	if indexedAt != nil {
		binding.IndexedAt = indexedAt.UTC()
	}
	if catalogObservedAt != nil {
		authority.CatalogObservedAt = catalogObservedAt.UTC()
	}
	if binding.Validate() != nil || authority.Authorized && (authority.RevocationRequired || catalogObservedAt == nil) ||
		authority.RevocationRequired && catalogObservedAt != nil {
		return gitprojection.Binding{}, RepositoryBindingAuthority{}, ErrConflict
	}
	return binding, authority, nil
}

var _ RuntimeBindingCatalog = (*PostgreSQLRuntimeBindingCatalog)(nil)
