package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/registry"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func scanProjectRegistryPullCredential(row pgx.Row) (domain.ProjectRegistryPullCredential, error) {
	var item domain.ProjectRegistryPullCredential
	err := row.Scan(&item.ID, &item.ProjectID, &item.RegistryTargetID, &item.Name,
		&item.RegistryName, &item.RegistryServer, &item.RepositoryPrefix, &item.CreatedAt, &item.UpdatedAt)
	return item, classify(err)
}

const projectRegistryPullCredentialSelect = `SELECT c.id::text,c.project_id::text,c.registry_target_id::text,c.name,
	t.name,t.endpoint,t.repository_prefix,c.created_at,c.updated_at
	FROM project_registry_pull_credentials c JOIN registry_targets t ON t.id=c.registry_target_id`

func (s *Store) ListRegistryPullTargetsForActor(ctx context.Context, actor, projectID string) ([]domain.RegistryTarget, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = authorizeWith(ctx, tx, actor, domain.PermissionRegistryRead, domain.AccessTarget{Type: "project", ID: projectID}); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT id::text,name,mode,endpoint,repository_prefix,pull_credential_ref,push_credential_ref,cache_credential_ref,created_at,updated_at FROM registry_targets WHERE pull_credential_ref<>'' ORDER BY name,id LIMIT 65`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.RegistryTarget, 0)
	for rows.Next() {
		var target domain.RegistryTarget
		if err = rows.Scan(&target.ID, &target.Name, &target.Mode, &target.Endpoint, &target.RepositoryPrefix, &target.PullCredentialRef, &target.PushCredentialRef, &target.CacheCredentialRef, &target.CreatedAt, &target.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, target)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(items) > 64 {
		return nil, base.ErrConflict
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Store) ListProjectRegistryPullCredentialsForActor(ctx context.Context, actor, projectID string) ([]domain.ProjectRegistryPullCredential, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = authorizeWith(ctx, tx, actor, domain.PermissionRegistryRead, domain.AccessTarget{Type: "project", ID: projectID}); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, projectRegistryPullCredentialSelect+` WHERE c.project_id=$1 ORDER BY c.name,c.id LIMIT 65`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.ProjectRegistryPullCredential, 0)
	for rows.Next() {
		var item domain.ProjectRegistryPullCredential
		if err = rows.Scan(&item.ID, &item.ProjectID, &item.RegistryTargetID, &item.Name, &item.RegistryName, &item.RegistryServer, &item.RepositoryPrefix, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(items) > 64 {
		return nil, base.ErrConflict
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Store) CreateProjectRegistryPullCredentialForActor(ctx context.Context, actor, key, fingerprint, requestID string, item domain.ProjectRegistryPullCredential) (base.Result[domain.ProjectRegistryPullCredential], error) {
	if err := registry.ValidateProjectPullCredential(item); err != nil {
		return base.Result[domain.ProjectRegistryPullCredential]{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return base.Result[domain.ProjectRegistryPullCredential]{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = authorizeWith(ctx, tx, actor, domain.PermissionRegistryPolicyWrite, domain.AccessTarget{Type: "project", ID: item.ProjectID}); err != nil {
		return base.Result[domain.ProjectRegistryPullCredential]{}, err
	}
	scope := "project-registry-pull-credentials.create:" + item.ProjectID
	if old, ok, findErr := findIdem(ctx, tx, actor, scope, key); findErr != nil {
		return base.Result[domain.ProjectRegistryPullCredential]{}, findErr
	} else if ok {
		if old.fingerprint != fingerprint {
			return base.Result[domain.ProjectRegistryPullCredential]{}, base.ErrIdempotencyConflict
		}
		current, loadErr := scanProjectRegistryPullCredential(tx.QueryRow(ctx, projectRegistryPullCredentialSelect+` WHERE c.id=$1 AND c.project_id=$2`, old.resourceID, item.ProjectID))
		if loadErr != nil {
			return base.Result[domain.ProjectRegistryPullCredential]{}, loadErr
		}
		if err = tx.Commit(ctx); err != nil {
			return base.Result[domain.ProjectRegistryPullCredential]{}, err
		}
		return base.Result[domain.ProjectRegistryPullCredential]{Value: current, Replay: true}, nil
	}
	now := databaseTime(time.Now())
	item.CreatedAt, item.UpdatedAt = now, now
	item, err = scanProjectRegistryPullCredential(tx.QueryRow(ctx, `INSERT INTO project_registry_pull_credentials(id,project_id,registry_target_id,name,created_by,created_at,updated_at)
		SELECT $1,$2,t.id,$4,$5,$6,$6 FROM registry_targets t WHERE t.id=$3 AND t.pull_credential_ref<>''
		RETURNING id::text,project_id::text,registry_target_id::text,name,
		(SELECT name FROM registry_targets WHERE id=registry_target_id),
		(SELECT endpoint FROM registry_targets WHERE id=registry_target_id),
		(SELECT repository_prefix FROM registry_targets WHERE id=registry_target_id),created_at,updated_at`,
		item.ID, item.ProjectID, item.RegistryTargetID, item.Name, actor, now))
	if err != nil {
		return base.Result[domain.ProjectRegistryPullCredential]{}, err
	}
	if err = putIdem(ctx, tx, actor, scope, key, fingerprint, "project-registry-pull-credential", item.ID, nil); err != nil {
		return base.Result[domain.ProjectRegistryPullCredential]{}, classify(err)
	}
	if err = audit(ctx, tx, actor, "project-registry-pull-credential.create", "project", item.ProjectID, requestID, map[string]any{"credentialId": item.ID, "registryTargetId": item.RegistryTargetID, "name": item.Name}); err != nil {
		return base.Result[domain.ProjectRegistryPullCredential]{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return base.Result[domain.ProjectRegistryPullCredential]{}, classify(err)
	}
	return base.Result[domain.ProjectRegistryPullCredential]{Value: item}, nil
}

func (s *Store) DeleteProjectRegistryPullCredentialForActor(ctx context.Context, actor, projectID, credentialID, key, fingerprint, requestID string) (bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = authorizeWith(ctx, tx, actor, domain.PermissionRegistryPolicyWrite, domain.AccessTarget{Type: "project", ID: projectID}); err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, advisoryIdentity(actor, "project-registry-pull-credentials.delete:"+projectID+":"+credentialID, key)); err != nil {
		return false, err
	}
	idemScope := "project-registry-pull-credentials.delete:" + projectID + ":" + credentialID
	if old, ok, findErr := findIdem(ctx, tx, actor, idemScope, key); findErr != nil {
		return false, findErr
	} else if ok {
		if old.fingerprint != fingerprint {
			return false, base.ErrIdempotencyConflict
		}
		return true, tx.Commit(ctx)
	}
	var storedProjectID string
	if err = tx.QueryRow(ctx, `SELECT project_id::text FROM project_registry_pull_credentials WHERE id=$1 FOR UPDATE`, credentialID).Scan(&storedProjectID); err != nil {
		return false, classify(err)
	}
	if storedProjectID != projectID {
		return false, base.ErrNotFound
	}
	var selected bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM applications WHERE registry_pull_project_credential_id=$1)`, credentialID).Scan(&selected); err != nil {
		return false, err
	}
	if selected {
		return false, base.ErrConflict
	}
	result, err := tx.Exec(ctx, `DELETE FROM project_registry_pull_credentials WHERE id=$1 AND project_id=$2`, credentialID, projectID)
	if err != nil {
		return false, classify(err)
	}
	if result.RowsAffected() == 0 {
		return false, base.ErrNotFound
	}
	if err = audit(ctx, tx, actor, "project-registry-pull-credential.delete", "project", projectID, requestID, map[string]any{"credentialId": credentialID}); err != nil {
		return false, err
	}
	if err = putIdem(ctx, tx, actor, idemScope, key, fingerprint, "project-registry-pull-credential", credentialID, nil); err != nil {
		return false, classify(err)
	}
	return false, classify(tx.Commit(ctx))
}

func scanApplicationRegistryPullSelection(row pgx.Row) (domain.ApplicationRegistryPullSelection, error) {
	var value domain.ApplicationRegistryPullSelection
	err := row.Scan(&value.ApplicationID, &value.Mode, &value.ProjectCredentialID, &value.UpdatedAt)
	return value, classify(err)
}

func (s *Store) ApplicationRegistryPullSelectionForActor(ctx context.Context, actor, applicationID string) (domain.ApplicationRegistryPullSelection, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return domain.ApplicationRegistryPullSelection{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = authorizeWith(ctx, tx, actor, domain.PermissionRegistryRead, domain.AccessTarget{Type: "application", ID: applicationID}); err != nil {
		return domain.ApplicationRegistryPullSelection{}, err
	}
	value, err := scanApplicationRegistryPullSelection(tx.QueryRow(ctx, `SELECT id::text,registry_pull_mode,
		COALESCE(registry_pull_project_credential_id::text,''),COALESCE(registry_pull_updated_at,created_at)
		FROM applications WHERE id=$1 AND registry_pull_mode IS NOT NULL`, applicationID))
	if errors.Is(err, base.ErrNotFound) {
		value, err = domain.ApplicationRegistryPullSelection{ApplicationID: applicationID, Mode: domain.ApplicationRegistryPullPublic}, nil
	}
	if err != nil {
		return domain.ApplicationRegistryPullSelection{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.ApplicationRegistryPullSelection{}, err
	}
	return value, nil
}

func (s *Store) PutApplicationRegistryPullSelectionForActor(ctx context.Context, actor, key, fingerprint, requestID string, value domain.ApplicationRegistryPullSelection) (base.Result[domain.ApplicationRegistryPullSelection], error) {
	if err := registry.ValidateApplicationPullSelection(value); err != nil {
		return base.Result[domain.ApplicationRegistryPullSelection]{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return base.Result[domain.ApplicationRegistryPullSelection]{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = authorizeWith(ctx, tx, actor, domain.PermissionRegistryPolicyWrite, domain.AccessTarget{Type: "application", ID: value.ApplicationID}); err != nil {
		return base.Result[domain.ApplicationRegistryPullSelection]{}, err
	}
	scope := "application-registry-pull-selection.put:" + value.ApplicationID
	if old, ok, findErr := findIdem(ctx, tx, actor, scope, key); findErr != nil {
		return base.Result[domain.ApplicationRegistryPullSelection]{}, findErr
	} else if ok {
		if old.fingerprint != fingerprint {
			return base.Result[domain.ApplicationRegistryPullSelection]{}, base.ErrIdempotencyConflict
		}
		current, loadErr := scanApplicationRegistryPullSelection(tx.QueryRow(ctx, `SELECT id::text,registry_pull_mode,
			COALESCE(registry_pull_project_credential_id::text,''),COALESCE(registry_pull_updated_at,created_at)
			FROM applications WHERE id=$1`, value.ApplicationID))
		if loadErr != nil {
			return base.Result[domain.ApplicationRegistryPullSelection]{}, loadErr
		}
		if err = tx.Commit(ctx); err != nil {
			return base.Result[domain.ApplicationRegistryPullSelection]{}, err
		}
		return base.Result[domain.ApplicationRegistryPullSelection]{Value: current, Replay: true}, nil
	}
	now := databaseTime(time.Now())
	var credential any
	if value.ProjectCredentialID != "" {
		credential = value.ProjectCredentialID
	}
	value, err = scanApplicationRegistryPullSelection(tx.QueryRow(ctx, `UPDATE applications SET
		registry_pull_mode=$2,registry_pull_project_credential_id=$3,
		registry_pull_updated_by=$4,registry_pull_updated_at=$5 WHERE id=$1
		RETURNING id::text,registry_pull_mode,COALESCE(registry_pull_project_credential_id::text,''),registry_pull_updated_at`,
		value.ApplicationID, value.Mode, credential, actor, now))
	if err != nil {
		return base.Result[domain.ApplicationRegistryPullSelection]{}, err
	}
	if err = putIdem(ctx, tx, actor, scope, key, fingerprint, "application-registry-pull-selection", value.ApplicationID, nil); err != nil {
		return base.Result[domain.ApplicationRegistryPullSelection]{}, classify(err)
	}
	if err = audit(ctx, tx, actor, "application-registry-pull-selection.update", "application", value.ApplicationID, requestID, map[string]any{"mode": value.Mode, "projectCredentialId": value.ProjectCredentialID}); err != nil {
		return base.Result[domain.ApplicationRegistryPullSelection]{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return base.Result[domain.ApplicationRegistryPullSelection]{}, classify(err)
	}
	return base.Result[domain.ApplicationRegistryPullSelection]{Value: value}, nil
}
