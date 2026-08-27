package postgres

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/id"
	base "github.com/kuberploy/kuberploy/internal/store"
)

const centralGitBindingColumns = `id::text,kind,scope_id::text,COALESCE(project_id::text,''),COALESCE(environment_id::text,''),provider,installation_id,repository_id,repository_owner,repository_name,target_ref,path_prefix,credential_mode,credential_secret_name,state,target_head_revision,indexed_revision,projection_generation,parser_version,target_head_observed_at,indexed_at,created_at,updated_at`

func scanCentralGitBinding(row pgx.Row) (gitprojection.Binding, error) {
	var binding gitprojection.Binding
	var targetHead, indexedRevision *string
	var targetHeadAt, indexedAt *time.Time
	if err := row.Scan(
		&binding.ID, &binding.Kind, &binding.ScopeID, &binding.ProjectID, &binding.EnvironmentID,
		&binding.Repository.Provider, &binding.Repository.InstallationID, &binding.Repository.RepositoryID,
		&binding.Repository.Owner, &binding.Repository.Name, &binding.TargetRef, &binding.Prefix,
		&binding.CredentialMode, &binding.CredentialSecretName, &binding.State, &targetHead, &indexedRevision,
		&binding.ProjectionGeneration, &binding.ParserVersion, &targetHeadAt, &indexedAt,
		&binding.CreatedAt, &binding.UpdatedAt,
	); err != nil {
		return gitprojection.Binding{}, classify(err)
	}
	if targetHead != nil {
		binding.TargetHeadRevision = *targetHead
	}
	if indexedRevision != nil {
		binding.IndexedRevision = *indexedRevision
	}
	if targetHeadAt != nil {
		binding.TargetHeadObservedAt = targetHeadAt.UTC()
	}
	if indexedAt != nil {
		binding.IndexedAt = indexedAt.UTC()
	}
	if err := binding.Validate(); err != nil {
		return gitprojection.Binding{}, base.ErrConflict
	}
	return binding, nil
}

func centralGitBindingByID(ctx context.Context, query rowQuerier, bindingID string) (gitprojection.Binding, error) {
	return scanCentralGitBinding(query.QueryRow(ctx, `SELECT `+centralGitBindingColumns+` FROM git_repository_bindings WHERE id=$1`, bindingID))
}

func centralGitBindingByEnvironment(ctx context.Context, query rowQuerier, environmentID string) (gitprojection.Binding, error) {
	return scanCentralGitBinding(query.QueryRow(ctx, `SELECT `+centralGitBindingColumns+` FROM git_repository_bindings WHERE kind='environment' AND environment_id=$1`, environmentID))
}

func centralPlatformGitBinding(ctx context.Context, query rowQuerier) (gitprojection.Binding, error) {
	return scanCentralGitBinding(query.QueryRow(ctx, `SELECT `+centralGitBindingColumns+` FROM git_repository_bindings WHERE kind='platform' LIMIT 1`))
}

func cloneEnvironmentGitBindingTx(ctx context.Context, tx pgx.Tx, source, clone domain.Environment, now time.Time) error {
	sourceBinding, err := centralGitBindingByEnvironment(ctx, tx, source.ID)
	if errors.Is(err, base.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if sourceBinding.ProjectID != source.ProjectID || clone.ProjectID != source.ProjectID {
		return base.ErrConflict
	}
	var binding gitprojection.Binding
	switch sourceBinding.CredentialMode {
	case gitprojection.CredentialGitHubApp:
		binding, err = gitprojection.NewGitHubEnvironmentBinding(id.New(), clone.ProjectID, clone.ID,
			sourceBinding.Repository, sourceBinding.TargetRef, now)
	case gitprojection.CredentialLegacySecret:
		binding, err = gitprojection.NewEnvironmentBinding(id.New(), clone.ProjectID, clone.ID,
			sourceBinding.Repository, sourceBinding.TargetRef, sourceBinding.CredentialSecretName, now)
	default:
		return gitprojection.ErrInvalid
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO git_repository_bindings(
		id,kind,scope_id,project_id,environment_id,provider,installation_id,repository_id,repository_owner,repository_name,
		target_ref,path_prefix,credential_mode,credential_secret_name,state,target_head_revision,indexed_revision,
		projection_generation,parser_version,target_head_observed_at,indexed_at,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,NULL,NULL,0,$16,NULL,NULL,$17,$17)`,
		binding.ID, binding.Kind, binding.ScopeID, binding.ProjectID, binding.EnvironmentID, binding.Repository.Provider,
		binding.Repository.InstallationID, binding.Repository.RepositoryID, binding.Repository.Owner, binding.Repository.Name,
		binding.TargetRef, binding.Prefix, binding.CredentialMode, binding.CredentialSecretName, binding.State,
		binding.ParserVersion, binding.CreatedAt)
	return classify(err)
}

func classifyGitBindingTransaction(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "40001" {
		return base.ErrConflict
	}
	return classify(err)
}

func (s *Store) CreateEnvironmentGitBinding(ctx context.Context, actor, key, fingerprint, requestID string, in gitprojection.CreateEnvironmentBindingInput) (base.Result[gitprojection.Binding], error) {
	if err := in.Validate(); err != nil || actor == "" || key == "" || fingerprint == "" || requestID == "" {
		return base.Result[gitprojection.Binding]{}, gitprojection.ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return base.Result[gitprojection.Binding]{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err = authorizeWith(ctx, tx, actor, domain.PermissionConfigWrite, domain.AccessTarget{Type: "environment", ID: in.EnvironmentID}); err != nil {
		return base.Result[gitprojection.Binding]{}, err
	}
	if err = authorizeWith(ctx, tx, actor, domain.PermissionBuildsManage, domain.AccessTarget{Type: "environment", ID: in.EnvironmentID}); err != nil {
		return base.Result[gitprojection.Binding]{}, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, advisoryIdentity("environment-git-binding", in.EnvironmentID)); err != nil {
		return base.Result[gitprojection.Binding]{}, err
	}
	idemScope := "environment-git-bindings.create:" + in.EnvironmentID
	if old, replay, findErr := findIdem(ctx, tx, actor, idemScope, key); findErr != nil {
		return base.Result[gitprojection.Binding]{}, findErr
	} else if replay {
		if old.fingerprint != fingerprint || old.resourceType != "git-binding" {
			return base.Result[gitprojection.Binding]{}, base.ErrIdempotencyConflict
		}
		binding, getErr := centralGitBindingByID(ctx, tx, old.resourceID)
		if getErr != nil || binding.EnvironmentID != in.EnvironmentID {
			return base.Result[gitprojection.Binding]{}, getErr
		}
		if err = tx.Commit(ctx); err != nil {
			return base.Result[gitprojection.Binding]{}, classifyGitBindingTransaction(err)
		}
		return base.Result[gitprojection.Binding]{Value: binding, Replay: true}, nil
	}

	var projectID, projectTeamID string
	var installationAppID, installationProviderID, installationAccountID, repositoryProviderID, repositoryOwnerID int64
	var installationLogin, installationOwnerID, installationVisibility, installationTeamID, repositoryOwner, repositoryName string
	err = tx.QueryRow(ctx, `SELECT e.project_id::text,COALESCE(p.team_id::text,''),
		i.github_app_id,i.github_installation_id,i.github_account_id,i.account_login,i.owner_user_id::text,i.visibility,COALESCE(i.team_id::text,''),
		r.github_repository_id,r.github_owner_id,r.owner_login,r.name
		FROM environments e
		JOIN projects p ON p.id=e.project_id
		JOIN github_installations i ON i.id=$2 AND i.lifecycle='active'
		JOIN github_repositories r ON r.id=$3 AND r.installation_id=i.id AND r.lifecycle='active'
		WHERE e.id=$1
		AND i.permissions->>'metadata' IN ('read','write')
		AND i.permissions->>'contents' IN ('read','write')
		FOR UPDATE OF e,i,r`, in.EnvironmentID, in.LinkedInstallationID, in.LinkedRepositoryID).Scan(
		&projectID, &projectTeamID, &installationAppID, &installationProviderID, &installationAccountID,
		&installationLogin, &installationOwnerID, &installationVisibility, &installationTeamID,
		&repositoryProviderID, &repositoryOwnerID, &repositoryOwner, &repositoryName,
	)
	if err != nil {
		return base.Result[gitprojection.Binding]{}, classify(err)
	}
	admin, err := actorIsAdmin(ctx, tx, actor)
	if err != nil {
		return base.Result[gitprojection.Binding]{}, err
	}
	if !admin && installationOwnerID != actor && !(installationVisibility == "team" && installationTeamID != "" && installationTeamID == projectTeamID) {
		return base.Result[gitprojection.Binding]{}, base.ErrNotFound
	}
	if installationAppID != in.GitHubAppID || installationAccountID != repositoryOwnerID ||
		!strings.EqualFold(installationLogin, repositoryOwner) ||
		in.Repository.Provider != "github" || in.Repository.InstallationID != installationProviderID ||
		in.Repository.RepositoryID != repositoryProviderID || !strings.EqualFold(in.Repository.Owner, repositoryOwner) ||
		in.Repository.Name != repositoryName {
		return base.Result[gitprojection.Binding]{}, base.ErrNotFound
	}

	repository := gitprojection.RepositoryIdentity{Provider: "github", InstallationID: installationProviderID,
		RepositoryID: repositoryProviderID, Owner: repositoryOwner, Name: repositoryName}
	now := time.Now().UTC()
	binding, err := gitprojection.NewGitHubEnvironmentBinding(id.New(), projectID, in.EnvironmentID, repository, in.TargetRef, now)
	if err != nil {
		return base.Result[gitprojection.Binding]{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO git_repository_bindings(
		id,kind,scope_id,project_id,environment_id,provider,installation_id,repository_id,repository_owner,repository_name,
		target_ref,path_prefix,credential_mode,credential_secret_name,state,target_head_revision,indexed_revision,projection_generation,
		parser_version,target_head_observed_at,indexed_at,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,'',$14,NULL,NULL,$15,$16,NULL,NULL,$17,$17)`,
		binding.ID, binding.Kind, binding.ScopeID, binding.ProjectID, binding.EnvironmentID, binding.Repository.Provider,
		binding.Repository.InstallationID, binding.Repository.RepositoryID, binding.Repository.Owner, binding.Repository.Name,
		binding.TargetRef, binding.Prefix, binding.CredentialMode, binding.State, binding.ProjectionGeneration,
		binding.ParserVersion, binding.CreatedAt)
	if err != nil {
		return base.Result[gitprojection.Binding]{}, classify(err)
	}
	if err = audit(ctx, tx, actor, "git-binding.create", "environment", in.EnvironmentID, requestID, map[string]any{
		"bindingId": binding.ID, "installationId": in.LinkedInstallationID, "repositoryId": in.LinkedRepositoryID,
		"providerRepositoryId": binding.Repository.RepositoryID, "targetRef": binding.TargetRef,
	}); err != nil {
		return base.Result[gitprojection.Binding]{}, err
	}
	if err = putIdem(ctx, tx, actor, idemScope, key, fingerprint, "git-binding", binding.ID, nil); err != nil {
		return base.Result[gitprojection.Binding]{}, classify(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return base.Result[gitprojection.Binding]{}, classifyGitBindingTransaction(err)
	}
	return base.Result[gitprojection.Binding]{Value: binding}, nil
}

func (s *Store) GetEnvironmentGitBindingForActor(ctx context.Context, actor, environmentID string) (gitprojection.Binding, error) {
	if err := authorizeWith(ctx, s.pool, actor, domain.PermissionConfigRead, domain.AccessTarget{Type: "environment", ID: environmentID}); err != nil {
		return gitprojection.Binding{}, err
	}
	return centralGitBindingByEnvironment(ctx, s.pool, environmentID)
}

func (s *Store) CreatePlatformGitBinding(ctx context.Context, actor, key, fingerprint, requestID string, in gitprojection.CreatePlatformBindingInput) (base.Result[gitprojection.Binding], error) {
	if err := in.Validate(); err != nil || actor == "" || key == "" || fingerprint == "" || requestID == "" {
		return base.Result[gitprojection.Binding]{}, gitprojection.ErrInvalid
	}
	// The installation-wide advisory lock is the serialization boundary. ReadCommitted is
	// intentional: after waiting for an earlier creator, this transaction must see
	// the authority that creator committed before it checks for an existing binding.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return base.Result[gitprojection.Binding]{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err = authorizeWith(ctx, tx, actor, domain.PermissionPlatformAdmin, domain.AccessTarget{Type: "platform", ID: "platform"}); err != nil {
		return base.Result[gitprojection.Binding]{}, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, advisoryIdentity("argo-platform-git-binding", "singleton")); err != nil {
		return base.Result[gitprojection.Binding]{}, err
	}
	idemScope := "argo-platform-git-bindings.create"
	if old, replay, findErr := findIdem(ctx, tx, actor, idemScope, key); findErr != nil {
		return base.Result[gitprojection.Binding]{}, findErr
	} else if replay {
		if old.fingerprint != fingerprint || old.resourceType != "argo-platform-git-binding" {
			return base.Result[gitprojection.Binding]{}, base.ErrIdempotencyConflict
		}
		binding, getErr := centralGitBindingByID(ctx, tx, old.resourceID)
		if getErr != nil || binding.Kind != gitprojection.BindingPlatform {
			return base.Result[gitprojection.Binding]{}, getErr
		}
		if err = tx.Commit(ctx); err != nil {
			return base.Result[gitprojection.Binding]{}, classifyGitBindingTransaction(err)
		}
		return base.Result[gitprojection.Binding]{Value: binding, Replay: true}, nil
	}

	if _, getErr := centralPlatformGitBinding(ctx, tx); getErr == nil {
		return base.Result[gitprojection.Binding]{}, base.ErrConflict
	} else if !errors.Is(getErr, base.ErrNotFound) {
		return base.Result[gitprojection.Binding]{}, getErr
	}

	var installationAppID, installationProviderID, installationAccountID int64
	var repositoryProviderID, repositoryOwnerID int64
	var installationLogin, repositoryOwner, repositoryName string
	err = tx.QueryRow(ctx, `SELECT i.github_app_id,i.github_installation_id,i.github_account_id,i.account_login,
		r.github_repository_id,r.github_owner_id,r.owner_login,r.name
		FROM github_installations i
		JOIN github_repositories r ON r.id=$2 AND r.installation_id=i.id
		WHERE i.id=$1 AND i.lifecycle='active' AND r.lifecycle='active'
		  AND i.last_verified_at IS NOT NULL AND r.last_verified_at IS NOT NULL
		  AND i.permissions->>'metadata' IN ('read','write')
		  AND i.permissions->>'contents' IN ('read','write')
		FOR UPDATE OF i,r`, in.LinkedInstallationID, in.LinkedRepositoryID).Scan(
		&installationAppID, &installationProviderID, &installationAccountID, &installationLogin,
		&repositoryProviderID, &repositoryOwnerID, &repositoryOwner, &repositoryName,
	)
	if err != nil {
		return base.Result[gitprojection.Binding]{}, classify(err)
	}
	if installationAppID != in.GitHubAppID || installationAccountID != repositoryOwnerID ||
		!strings.EqualFold(installationLogin, repositoryOwner) || in.Repository.Provider != "github" ||
		in.Repository.InstallationID != installationProviderID || in.Repository.RepositoryID != repositoryProviderID ||
		!strings.EqualFold(in.Repository.Owner, repositoryOwner) || in.Repository.Name != repositoryName {
		return base.Result[gitprojection.Binding]{}, base.ErrNotFound
	}
	repository := gitprojection.RepositoryIdentity{Provider: "github", InstallationID: installationProviderID,
		RepositoryID: repositoryProviderID, Owner: repositoryOwner, Name: repositoryName}
	if _, err = repository.CanonicalRemote(); err != nil {
		return base.Result[gitprojection.Binding]{}, gitprojection.ErrInvalid
	}
	now := time.Now().UTC()
	binding, err := gitprojection.NewGitHubPlatformBinding(in.BindingID, repository, in.TargetRef, now)
	if err != nil {
		return base.Result[gitprojection.Binding]{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO git_repository_bindings(
		id,kind,scope_id,project_id,environment_id,provider,installation_id,repository_id,repository_owner,repository_name,
		target_ref,path_prefix,credential_mode,credential_secret_name,state,target_head_revision,indexed_revision,projection_generation,
		parser_version,target_head_observed_at,indexed_at,created_at,updated_at)
		VALUES($1,$2,$3,NULL,NULL,$4,$5,$6,$7,$8,$9,$10,$11,'',$12,NULL,NULL,$13,$14,NULL,NULL,$15,$15)`,
		binding.ID, binding.Kind, binding.ScopeID, binding.Repository.Provider,
		binding.Repository.InstallationID, binding.Repository.RepositoryID, binding.Repository.Owner, binding.Repository.Name,
		binding.TargetRef, binding.Prefix, binding.CredentialMode, binding.State, binding.ProjectionGeneration,
		binding.ParserVersion, binding.CreatedAt)
	if err != nil {
		return base.Result[gitprojection.Binding]{}, classify(err)
	}
	if err = audit(ctx, tx, actor, "argo-platform-git-binding.create", "platform", binding.ID, requestID, map[string]any{
		"bindingId": binding.ID, "installationId": in.LinkedInstallationID, "repositoryId": in.LinkedRepositoryID,
		"providerRepositoryId": binding.Repository.RepositoryID, "targetRef": binding.TargetRef,
	}); err != nil {
		return base.Result[gitprojection.Binding]{}, err
	}
	if err = putIdem(ctx, tx, actor, idemScope, key, fingerprint, "argo-platform-git-binding", binding.ID, nil); err != nil {
		return base.Result[gitprojection.Binding]{}, classify(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return base.Result[gitprojection.Binding]{}, classifyGitBindingTransaction(err)
	}
	return base.Result[gitprojection.Binding]{Value: binding}, nil
}

func (s *Store) GetPlatformGitBindingForActor(ctx context.Context, actor string) (gitprojection.Binding, error) {
	if err := authorizeWith(ctx, s.pool, actor, domain.PermissionPlatformAdmin, domain.AccessTarget{Type: "platform", ID: "platform"}); err != nil {
		return gitprojection.Binding{}, err
	}
	return centralPlatformGitBinding(ctx, s.pool)
}
