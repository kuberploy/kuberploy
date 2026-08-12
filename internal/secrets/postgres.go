package secrets

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgreSQLStore struct{ pool *pgxpool.Pool }

func NewPostgreSQLStore(pool *pgxpool.Pool) (*PostgreSQLStore, error) {
	if pool == nil {
		return nil, ErrInvalid
	}
	return &PostgreSQLStore{pool: pool}, nil
}

func OpenPostgreSQLStore(ctx context.Context, databaseURL string) (*PostgreSQLStore, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, ErrInvalid
	}
	config.ConnConfig.RuntimeParams["application_name"] = "kuberploy-runtime-secrets"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &PostgreSQLStore{pool: pool}, nil
}

func (s *PostgreSQLStore) Close() { s.pool.Close() }

func (s *PostgreSQLStore) BeginCreate(ctx context.Context, command BeginCreate) (Binding, Version, bool, error) {
	if err := validateBeginCreate(command); err != nil {
		return Binding{}, Version{}, false, err
	}
	if binding, version, found, err := s.lookupReplay(ctx, command.Idempotency); found || err != nil {
		return binding, version, found, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Binding{}, Version{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	_, err = tx.Exec(ctx, `INSERT INTO secret_bindings(
		id,organization_id,project_id,environment_id,application_id,target_namespace,name,provider,purpose,state,active_version,created_by,created_at,updated_at)
		VALUES($1,NULLIF($2,'')::uuid,$3,$4,$5,$6,$7,$8,$9,$10,0,$11,$12,$12)`,
		command.Binding.ID, command.Binding.Scope.OrganizationID, command.Binding.Scope.ProjectID, command.Binding.Scope.EnvironmentID,
		command.Binding.Scope.ApplicationID, command.Binding.Scope.Namespace, command.Binding.Name, command.Binding.Provider,
		command.Binding.Purpose, command.Binding.State, command.Binding.CreatedBy, command.Binding.CreatedAt)
	if err == nil {
		err = insertVersion(ctx, tx, command.Version)
	}
	if err == nil {
		err = insertDeliveries(ctx, tx, command.Version)
	}
	if err == nil {
		err = insertIdempotency(ctx, tx, command.Idempotency)
	}
	if err == nil {
		err = insertEvent(ctx, tx, command.Event)
	}
	if err == nil {
		err = invalidateRuntimeSecretProjectionTx(ctx, tx, command.Binding.ID, command.Binding.UpdatedAt)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		if isUniqueViolation(err) {
			if binding, version, found, replayErr := s.lookupReplay(ctx, command.Idempotency); found || replayErr != nil {
				return binding, version, found, replayErr
			}
		}
		return Binding{}, Version{}, false, classifyPostgres(err)
	}
	return command.Binding, cloneVersion(command.Version), false, nil
}

func (s *PostgreSQLStore) BeginRotation(ctx context.Context, command BeginRotation) (Binding, Version, bool, error) {
	if !uuidRE.MatchString(command.BindingID) || command.ExpectedActiveVersion <= 0 || command.Version.Number != 0 ||
		command.Idempotency.validate() != nil || command.Event.Validate() != nil || command.Event.Kind != EventVersionStaging ||
		command.Version.BindingID != command.BindingID || command.Idempotency.BindingID != command.BindingID || command.Idempotency.VersionID != command.Version.ID ||
		command.Event.BindingID != command.BindingID || command.Event.VersionID != command.Version.ID ||
		!sameFingerprint(command.Idempotency.RequestFingerprint, command.Version.RequestFingerprint) {
		return Binding{}, Version{}, false, ErrInvalid
	}
	draft := cloneVersion(command.Version)
	draft.Number = 1
	if draft.Validate() != nil {
		return Binding{}, Version{}, false, ErrInvalid
	}
	if binding, version, found, err := s.lookupReplay(ctx, command.Idempotency); found || err != nil {
		return binding, version, found, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Binding{}, Version{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	binding, err := readBinding(ctx, tx, command.BindingID, true)
	if err != nil {
		return Binding{}, Version{}, false, err
	}
	if binding.State != BindingReady || binding.ActiveVersion != command.ExpectedActiveVersion || binding.Provider != command.Version.Provider ||
		binding.Scope.ApplicationID != command.Idempotency.ApplicationID || command.Version.CreatedAt.Before(binding.UpdatedAt) {
		return Binding{}, Version{}, false, ErrConflict
	}
	if !validPurposeTarget(binding.Purpose, binding.Provider, command.Version.TargetSecretType) {
		return Binding{}, Version{}, false, ErrConflict
	}
	var pending bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM secret_binding_versions WHERE binding_id=$1 AND state IN ('staging','awaiting-readiness'))`, binding.ID).Scan(&pending); err != nil {
		return Binding{}, Version{}, false, classifyPostgres(err)
	}
	if pending {
		return Binding{}, Version{}, false, ErrConflict
	}
	var activeTargetType TargetSecretType
	if err = tx.QueryRow(ctx, `SELECT target_secret_type FROM secret_binding_versions WHERE binding_id=$1 AND state='active' FOR UPDATE`, binding.ID).Scan(&activeTargetType); err != nil {
		return Binding{}, Version{}, false, classifyPostgres(err)
	}
	if activeTargetType != command.Version.TargetSecretType {
		return Binding{}, Version{}, false, ErrConflict
	}
	if err = tx.QueryRow(ctx, `SELECT COALESCE(MAX(version_number),0)+1 FROM secret_binding_versions WHERE binding_id=$1`, binding.ID).Scan(&command.Version.Number); err != nil {
		return Binding{}, Version{}, false, classifyPostgres(err)
	}
	if command.Version.Validate() != nil {
		return Binding{}, Version{}, false, ErrInvalid
	}
	err = insertVersion(ctx, tx, command.Version)
	if err == nil {
		err = insertDeliveries(ctx, tx, command.Version)
	}
	if err == nil {
		err = insertIdempotency(ctx, tx, command.Idempotency)
	}
	if err == nil {
		err = insertEvent(ctx, tx, command.Event)
	}
	if err == nil {
		err = invalidateRuntimeSecretProjectionTx(ctx, tx, binding.ID, command.Version.CreatedAt)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		if isUniqueViolation(err) {
			if replayBinding, version, found, replayErr := s.lookupReplay(ctx, command.Idempotency); found || replayErr != nil {
				return replayBinding, version, found, replayErr
			}
		}
		return Binding{}, Version{}, false, classifyPostgres(err)
	}
	return binding, cloneVersion(command.Version), false, nil
}

func (s *PostgreSQLStore) CompleteStage(ctx context.Context, versionID string, artifact Artifact, event Event, now time.Time) (Version, error) {
	if !uuidRE.MatchString(versionID) || now.IsZero() || event.Validate() != nil || event.Kind != EventVersionAwaitingReadiness || event.VersionID != versionID {
		return Version{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Version{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	version, err := readVersion(ctx, tx, versionID, true)
	if err != nil {
		return Version{}, err
	}
	binding, err := readBinding(ctx, tx, version.BindingID, true)
	if err != nil {
		return Version{}, err
	}
	if event.BindingID != binding.ID {
		return Version{}, ErrInvalid
	}
	if version.State == VersionAwaitingReadiness && version.Artifact != nil && *version.Artifact == artifact {
		return version, tx.Commit(ctx)
	}
	if version.State != VersionStaging || artifact.ValidateFor(binding, version.Number) != nil || event.BindingID != binding.ID {
		return Version{}, ErrConflict
	}
	command, err := tx.Exec(ctx, `UPDATE secret_binding_versions SET state='awaiting-readiness',provider_object_name=$2,target_secret_name=$3,
		provider_revision=$4,manifest_digest=$5,sealed_key_fingerprint=$6,ciphertext_digest=$7,staged_at=$8,updated_at=$8 WHERE id=$1 AND state='staging'`,
		versionID, artifact.ObjectName, artifact.TargetSecretName, artifact.ProviderRevision, artifact.ManifestDigest,
		nullableString(artifact.SealedKeyFingerprint), nullableString(artifact.CiphertextDigest), now.UTC())
	if err != nil {
		return Version{}, classifyPostgres(err)
	}
	if command.RowsAffected() != 1 {
		return Version{}, ErrConflict
	}
	if version.Provider == ProviderSealedSecrets {
		_, err = tx.Exec(ctx, `INSERT INTO secret_binding_runtime_reconciliations(
			version_id,binding_id,next_attempt_at,created_at,updated_at
		) VALUES($1,$2,$3,$3,$3) ON CONFLICT (version_id) DO NOTHING`, version.ID, version.BindingID, now.UTC())
		if err != nil {
			return Version{}, classifyPostgres(err)
		}
	}
	if err = insertEvent(ctx, tx, event); err != nil {
		return Version{}, err
	}
	if err = invalidateRuntimeSecretProjectionTx(ctx, tx, binding.ID, now); err != nil {
		return Version{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Version{}, classifyPostgres(err)
	}
	copyArtifact := artifact
	version.Artifact, version.State, version.StagedAt, version.UpdatedAt = &copyArtifact, VersionAwaitingReadiness, now.UTC(), now.UTC()
	return version, nil
}

func (s *PostgreSQLStore) FailVersion(ctx context.Context, versionID, code string, event Event, now time.Time) (Version, error) {
	if !uuidRE.MatchString(versionID) || !safeCodeRE.MatchString(code) || now.IsZero() || event.Validate() != nil || event.Kind != EventVersionFailed || event.VersionID != versionID {
		return Version{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Version{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	version, err := readVersion(ctx, tx, versionID, true)
	if err != nil {
		return Version{}, err
	}
	if event.BindingID != version.BindingID {
		return Version{}, ErrInvalid
	}
	if version.State == VersionFailed && version.FailureCode == code {
		return version, tx.Commit(ctx)
	}
	if version.State != VersionStaging && version.State != VersionAwaitingReadiness {
		return Version{}, ErrConflict
	}
	command, err := tx.Exec(ctx, `UPDATE secret_binding_versions SET state='failed',failure_code=$2,readiness_observed_at=$3,updated_at=$3 WHERE id=$1 AND state IN ('staging','awaiting-readiness')`, versionID, code, now.UTC())
	if err != nil {
		return Version{}, classifyPostgres(err)
	}
	if command.RowsAffected() != 1 {
		return Version{}, ErrConflict
	}
	_, err = tx.Exec(ctx, `UPDATE secret_bindings SET state='failed',updated_at=$2 WHERE id=$1 AND state='provisioning'`, version.BindingID, now.UTC())
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE secret_binding_runtime_reconciliations SET runtime_state='failed',completed_at=$2,
			lease_owner=NULL,lease_until=NULL,worker_contract=NULL,worker_config_digest=NULL,updated_at=$2
			WHERE version_id=$1 AND runtime_state='awaiting'`, versionID, now.UTC())
	}
	if err == nil {
		err = insertEvent(ctx, tx, event)
	}
	if err == nil {
		err = invalidateRuntimeSecretProjectionTx(ctx, tx, version.BindingID, now)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		return Version{}, classifyPostgres(err)
	}
	version.State, version.FailureCode, version.ReadinessObservedAt, version.UpdatedAt = VersionFailed, code, now.UTC(), now.UTC()
	return version, nil
}

func (s *PostgreSQLStore) ActivateVersion(ctx context.Context, versionID string, now time.Time, event Event) (Binding, Version, error) {
	if !uuidRE.MatchString(versionID) || now.IsZero() || event.Validate() != nil || event.Kind != EventVersionActive || event.VersionID != versionID {
		return Binding{}, Version{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Binding{}, Version{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	version, err := readVersion(ctx, tx, versionID, true)
	if err != nil {
		return Binding{}, Version{}, err
	}
	binding, err := readBinding(ctx, tx, version.BindingID, true)
	if err != nil {
		return Binding{}, Version{}, err
	}
	if event.BindingID != binding.ID {
		return Binding{}, Version{}, ErrInvalid
	}
	if version.State == VersionActive && binding.ActiveVersion == version.Number {
		return binding, version, tx.Commit(ctx)
	}
	if version.State != VersionAwaitingReadiness || (binding.State != BindingProvisioning && binding.State != BindingReady) || event.BindingID != binding.ID {
		return Binding{}, Version{}, ErrConflict
	}
	_, err = tx.Exec(ctx, `UPDATE secret_binding_versions SET state='retained',retained_at=$2,updated_at=$2 WHERE binding_id=$1 AND state='active'`, binding.ID, now.UTC())
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE secret_binding_versions SET state='active',readiness_observed_at=$2,activated_at=$2,updated_at=$2 WHERE id=$1 AND state='awaiting-readiness'`, version.ID, now.UTC())
	}
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE secret_bindings SET state='ready',active_version=$2,updated_at=$3 WHERE id=$1 AND state IN ('provisioning','ready')`, binding.ID, version.Number, now.UTC())
	}
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE secret_binding_runtime_reconciliations SET runtime_state='ready',completed_at=$2,
			lease_owner=NULL,lease_until=NULL,worker_contract=NULL,worker_config_digest=NULL,updated_at=$2
			WHERE version_id=$1 AND runtime_state='awaiting'`, version.ID, now.UTC())
	}
	if err == nil {
		err = insertEvent(ctx, tx, event)
	}
	if err == nil {
		err = invalidateRuntimeSecretProjectionTx(ctx, tx, binding.ID, now)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		return Binding{}, Version{}, classifyPostgres(err)
	}
	version.State, version.ReadinessObservedAt, version.ActivatedAt, version.UpdatedAt = VersionActive, now.UTC(), now.UTC(), now.UTC()
	binding.State, binding.ActiveVersion, binding.UpdatedAt = BindingReady, version.Number, now.UTC()
	return binding, version, nil
}

func (s *PostgreSQLStore) Binding(ctx context.Context, id string) (Binding, error) {
	if !uuidRE.MatchString(id) {
		return Binding{}, ErrInvalid
	}
	return readBinding(ctx, s.pool, id, false)
}

func (s *PostgreSQLStore) ListBindings(ctx context.Context, applicationID, environmentID string) ([]Binding, error) {
	if !uuidRE.MatchString(applicationID) || environmentID != "" && !uuidRE.MatchString(environmentID) {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text,COALESCE(organization_id::text,''),project_id::text,environment_id::text,application_id::text,target_namespace,name,provider,purpose,state,active_version,created_by::text,created_at,updated_at,delete_started_at,deleted_at
		FROM secret_bindings WHERE application_id=$1 AND ($2='' OR environment_id::text=$2) ORDER BY created_at,id`, applicationID, environmentID)
	if err != nil {
		return nil, classifyPostgres(err)
	}
	defer rows.Close()
	result := make([]Binding, 0)
	for rows.Next() {
		binding, scanErr := scanBinding(rows)
		if scanErr != nil {
			return nil, classifyPostgres(scanErr)
		}
		result = append(result, binding)
	}
	return result, classifyPostgres(rows.Err())
}

func (s *PostgreSQLStore) Version(ctx context.Context, id string) (Version, error) {
	if !uuidRE.MatchString(id) {
		return Version{}, ErrInvalid
	}
	return readVersion(ctx, s.pool, id, false)
}

func (s *PostgreSQLStore) Versions(ctx context.Context, bindingID string) ([]Version, error) {
	if !uuidRE.MatchString(bindingID) {
		return nil, ErrInvalid
	}
	if _, err := s.Binding(ctx, bindingID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text FROM secret_binding_versions WHERE binding_id=$1 ORDER BY version_number`, bindingID)
	if err != nil {
		return nil, classifyPostgres(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, classifyPostgres(err)
		}
		ids = append(ids, id)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, classifyPostgres(err)
	}
	rows.Close()
	result := make([]Version, 0, len(ids))
	for _, id := range ids {
		version, readErr := s.Version(ctx, id)
		if readErr != nil {
			return nil, readErr
		}
		result = append(result, version)
	}
	return result, nil
}

func (s *PostgreSQLStore) AddReference(ctx context.Context, reference Reference, event Event) error {
	if reference.Validate() != nil || event.Validate() != nil || event.Kind != EventReferenceAdded || event.BindingID != reference.BindingID || event.VersionID != reference.VersionID {
		return ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var bindingState, versionState string
	if err = tx.QueryRow(ctx, `SELECT b.state,v.state FROM secret_bindings b JOIN secret_binding_versions v ON v.binding_id=b.id WHERE b.id=$1 AND v.id=$2 FOR UPDATE OF b,v`, reference.BindingID, reference.VersionID).Scan(&bindingState, &versionState); err != nil {
		return classifyPostgres(err)
	}
	if bindingState != string(BindingReady) || (versionState != string(VersionActive) && versionState != string(VersionRetained)) {
		return ErrConflict
	}
	command, err := tx.Exec(ctx, `INSERT INTO secret_binding_references(binding_id,version_id,kind,reference_id,revision,created_at) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`,
		reference.BindingID, reference.VersionID, reference.Kind, reference.Reference, reference.Revision, reference.CreatedAt)
	if err != nil {
		return classifyPostgres(err)
	}
	if command.RowsAffected() == 0 {
		var versionID, revision string
		if err = tx.QueryRow(ctx, `SELECT version_id::text,revision FROM secret_binding_references WHERE binding_id=$1 AND kind=$2 AND reference_id=$3`, reference.BindingID, reference.Kind, reference.Reference).Scan(&versionID, &revision); err != nil {
			return classifyPostgres(err)
		}
		if versionID != reference.VersionID || revision != reference.Revision {
			return ErrConflict
		}
		return tx.Commit(ctx)
	}
	if err = insertEvent(ctx, tx, event); err != nil {
		return err
	}
	return classifyPostgres(tx.Commit(ctx))
}

func (s *PostgreSQLStore) RemoveReference(ctx context.Context, bindingID string, kind ReferenceKind, referenceID string, event Event) error {
	if !uuidRE.MatchString(bindingID) || !kind.valid() || !safeOpaque(referenceID, 256) || event.Validate() != nil || event.Kind != EventReferenceRemoved || event.BindingID != bindingID {
		return ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var versionID string
	if err = tx.QueryRow(ctx, `DELETE FROM secret_binding_references WHERE binding_id=$1 AND kind=$2 AND reference_id=$3 RETURNING version_id::text`, bindingID, kind, referenceID).Scan(&versionID); err != nil {
		return classifyPostgres(err)
	}
	if versionID != event.VersionID {
		return ErrInvalid
	}
	if err = insertEvent(ctx, tx, event); err != nil {
		return err
	}
	return classifyPostgres(tx.Commit(ctx))
}

func (s *PostgreSQLStore) References(ctx context.Context, bindingID string) ([]Reference, error) {
	if !uuidRE.MatchString(bindingID) {
		return nil, ErrInvalid
	}
	if _, err := s.Binding(ctx, bindingID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT binding_id::text,version_id::text,kind,reference_id,revision,created_at FROM secret_binding_references WHERE binding_id=$1 ORDER BY kind,reference_id`, bindingID)
	if err != nil {
		return nil, classifyPostgres(err)
	}
	defer rows.Close()
	result := []Reference{}
	for rows.Next() {
		var item Reference
		if err = rows.Scan(&item.BindingID, &item.VersionID, &item.Kind, &item.Reference, &item.Revision, &item.CreatedAt); err != nil {
			return nil, classifyPostgres(err)
		}
		result = append(result, item)
	}
	return result, classifyPostgres(rows.Err())
}

func (s *PostgreSQLStore) PrepareDelete(ctx context.Context, bindingID, actorID string, event Event, now time.Time) (Binding, []Version, error) {
	if !uuidRE.MatchString(bindingID) || !uuidRE.MatchString(actorID) || now.IsZero() || event.Validate() != nil || event.Kind != EventBindingDeleting || event.BindingID != bindingID || event.ActorID != actorID {
		return Binding{}, nil, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Binding{}, nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	binding, err := readBinding(ctx, tx, bindingID, true)
	if err != nil {
		return Binding{}, nil, err
	}
	if binding.State == BindingDeleted {
		return binding, nil, tx.Commit(ctx)
	}
	if binding.State != BindingDeleting {
		var references, pending bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM secret_binding_references WHERE binding_id=$1), EXISTS(SELECT 1 FROM secret_binding_versions WHERE binding_id=$1 AND state IN ('staging','awaiting-readiness'))`, bindingID).Scan(&references, &pending); err != nil {
			return Binding{}, nil, classifyPostgres(err)
		}
		if references {
			return Binding{}, nil, ErrReferenced
		}
		if pending {
			return Binding{}, nil, ErrConflict
		}
		if _, err = tx.Exec(ctx, `UPDATE secret_bindings SET state='deleting',delete_started_at=$2,updated_at=$2 WHERE id=$1`, bindingID, now.UTC()); err != nil {
			return Binding{}, nil, classifyPostgres(err)
		}
		if err = insertEvent(ctx, tx, event); err != nil {
			return Binding{}, nil, err
		}
		binding.State, binding.DeleteStarted, binding.UpdatedAt = BindingDeleting, now.UTC(), now.UTC()
		if err = invalidateRuntimeSecretProjectionTx(ctx, tx, binding.ID, now); err != nil {
			return Binding{}, nil, err
		}
	}
	versions, err := versionsInTx(ctx, tx, bindingID)
	if err != nil {
		return Binding{}, nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Binding{}, nil, classifyPostgres(err)
	}
	return binding, versions, nil
}

func (s *PostgreSQLStore) CompleteDelete(ctx context.Context, bindingID string, event Event, now time.Time) (Binding, error) {
	if !uuidRE.MatchString(bindingID) || now.IsZero() || event.Validate() != nil || event.Kind != EventBindingDeleted || event.BindingID != bindingID {
		return Binding{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Binding{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	binding, err := readBinding(ctx, tx, bindingID, true)
	if err != nil {
		return Binding{}, err
	}
	if binding.State == BindingDeleted {
		return binding, tx.Commit(ctx)
	}
	if binding.State != BindingDeleting {
		return Binding{}, ErrConflict
	}
	var referenced bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM secret_binding_references WHERE binding_id=$1)`, bindingID).Scan(&referenced); err != nil {
		return Binding{}, classifyPostgres(err)
	}
	if referenced {
		return Binding{}, ErrReferenced
	}
	_, err = tx.Exec(ctx, `UPDATE secret_binding_versions SET state='deleted',updated_at=$2 WHERE binding_id=$1 AND state<>'deleted'`, bindingID, now.UTC())
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE secret_bindings SET state='deleted',active_version=0,deleted_at=$2,updated_at=$2 WHERE id=$1 AND state='deleting'`, bindingID, now.UTC())
	}
	if err == nil {
		err = insertEvent(ctx, tx, event)
	}
	if err == nil {
		err = invalidateRuntimeSecretProjectionTx(ctx, tx, binding.ID, now)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		return Binding{}, classifyPostgres(err)
	}
	binding.State, binding.ActiveVersion, binding.DeletedAt, binding.UpdatedAt = BindingDeleted, 0, now.UTC(), now.UTC()
	return binding, nil
}

func (s *PostgreSQLStore) PendingEvents(ctx context.Context, limit int) ([]Event, error) {
	if limit < 1 || limit > 1000 {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text,binding_id::text,COALESCE(version_id::text,''),COALESCE(actor_id::text,''),kind,request_id,occurred_at,published_at FROM secret_binding_events WHERE published_at IS NULL ORDER BY occurred_at,id LIMIT $1`, limit)
	if err != nil {
		return nil, classifyPostgres(err)
	}
	defer rows.Close()
	result := []Event{}
	for rows.Next() {
		var event Event
		var published sql.NullTime
		if err = rows.Scan(&event.ID, &event.BindingID, &event.VersionID, &event.ActorID, &event.Kind, &event.RequestID, &event.OccurredAt, &published); err != nil {
			return nil, classifyPostgres(err)
		}
		if published.Valid {
			event.PublishedAt = published.Time.UTC()
		}
		result = append(result, event)
	}
	return result, classifyPostgres(rows.Err())
}

func (s *PostgreSQLStore) MarkEventPublished(ctx context.Context, id string, at time.Time) error {
	if !uuidRE.MatchString(id) || at.IsZero() {
		return ErrInvalid
	}
	var occurredAt time.Time
	if err := s.pool.QueryRow(ctx, `SELECT occurred_at FROM secret_binding_events WHERE id=$1`, id).Scan(&occurredAt); err != nil {
		return classifyPostgres(err)
	}
	if at.Before(occurredAt) {
		return ErrInvalid
	}
	command, err := s.pool.Exec(ctx, `UPDATE secret_binding_events SET published_at=COALESCE(published_at,$2) WHERE id=$1`, id, at.UTC())
	if err != nil {
		return classifyPostgres(err)
	}
	if command.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

type rowScanner interface {
	Scan(...any) error
}

func scanBinding(row rowScanner) (Binding, error) {
	var binding Binding
	var deleteStarted, deleted sql.NullTime
	err := row.Scan(&binding.ID, &binding.Scope.OrganizationID, &binding.Scope.ProjectID, &binding.Scope.EnvironmentID,
		&binding.Scope.ApplicationID, &binding.Scope.Namespace, &binding.Name, &binding.Provider, &binding.Purpose, &binding.State, &binding.ActiveVersion,
		&binding.CreatedBy, &binding.CreatedAt, &binding.UpdatedAt, &deleteStarted, &deleted)
	if err != nil {
		return Binding{}, err
	}
	if deleteStarted.Valid {
		binding.DeleteStarted = deleteStarted.Time.UTC()
	}
	if deleted.Valid {
		binding.DeletedAt = deleted.Time.UTC()
	}
	return binding, nil
}

func readBinding(ctx context.Context, query rowQuerier, id string, lock bool) (Binding, error) {
	statement := `SELECT id::text,COALESCE(organization_id::text,''),project_id::text,environment_id::text,application_id::text,target_namespace,name,provider,purpose,state,active_version,created_by::text,created_at,updated_at,delete_started_at,deleted_at FROM secret_bindings WHERE id=$1`
	if lock {
		statement += ` FOR UPDATE`
	}
	binding, err := scanBinding(query.QueryRow(ctx, statement, id))
	if err != nil {
		return Binding{}, classifyPostgres(err)
	}
	return binding, nil
}

func readVersion(ctx context.Context, query rowQuerier, id string, lock bool) (Version, error) {
	statement := `SELECT id::text,binding_id::text,version_number,provider,state,target_secret_type,fingerprint_key_id,content_fingerprint,
		provider_object_name,target_secret_name,provider_revision,manifest_digest,sealed_key_fingerprint,ciphertext_digest,failure_code,
		staged_at,readiness_observed_at,activated_at,retained_at,created_at,updated_at FROM secret_binding_versions WHERE id=$1`
	if lock {
		statement += ` FOR UPDATE`
	}
	var version Version
	var fingerprint []byte
	var objectName, targetName, providerRevision, manifestDigest, keyFingerprint, ciphertextDigest sql.NullString
	var staged, readiness, activated, retained sql.NullTime
	err := query.QueryRow(ctx, statement, id).Scan(&version.ID, &version.BindingID, &version.Number, &version.Provider, &version.State,
		&version.TargetSecretType, &version.FingerprintKeyID, &fingerprint, &objectName, &targetName, &providerRevision, &manifestDigest, &keyFingerprint,
		&ciphertextDigest, &version.FailureCode, &staged, &readiness, &activated, &retained, &version.CreatedAt, &version.UpdatedAt)
	if err != nil {
		return Version{}, classifyPostgres(err)
	}
	if len(fingerprint) != len(version.ContentFingerprint) {
		return Version{}, ErrConflict
	}
	copy(version.ContentFingerprint[:], fingerprint)
	if objectName.Valid {
		version.Artifact = &Artifact{Provider: version.Provider, ObjectName: objectName.String, TargetSecretName: targetName.String,
			TargetSecretType: version.TargetSecretType,
			ProviderRevision: providerRevision.String, ManifestDigest: manifestDigest.String, SealedKeyFingerprint: keyFingerprint.String, CiphertextDigest: ciphertextDigest.String}
		var namespace string
		if err = query.QueryRow(ctx, `SELECT target_namespace FROM secret_bindings WHERE id=$1`, version.BindingID).Scan(&namespace); err != nil {
			return Version{}, classifyPostgres(err)
		}
		version.Artifact.Namespace = namespace
	}
	if staged.Valid {
		version.StagedAt = staged.Time.UTC()
	}
	if readiness.Valid {
		version.ReadinessObservedAt = readiness.Time.UTC()
	}
	if activated.Valid {
		version.ActivatedAt = activated.Time.UTC()
	}
	if retained.Valid {
		version.RetainedAt = retained.Time.UTC()
	}
	rows, err := query.Query(ctx, `SELECT source_key,kind,COALESCE(environment_name,''),COALESCE(file_path,''),COALESCE(file_mode,0) FROM secret_binding_deliveries WHERE version_id=$1 ORDER BY ordinal`, id)
	if err != nil {
		return Version{}, classifyPostgres(err)
	}
	defer rows.Close()
	for rows.Next() {
		var delivery Delivery
		if err = rows.Scan(&delivery.SourceKey, &delivery.Kind, &delivery.EnvironmentName, &delivery.FilePath, &delivery.FileMode); err != nil {
			return Version{}, classifyPostgres(err)
		}
		version.Deliveries = append(version.Deliveries, delivery)
	}
	if err = rows.Err(); err != nil {
		return Version{}, classifyPostgres(err)
	}
	return version, nil
}

func versionsInTx(ctx context.Context, tx pgx.Tx, bindingID string) ([]Version, error) {
	rows, err := tx.Query(ctx, `SELECT id::text FROM secret_binding_versions WHERE binding_id=$1 ORDER BY version_number FOR UPDATE`, bindingID)
	if err != nil {
		return nil, classifyPostgres(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, classifyPostgres(err)
		}
		ids = append(ids, id)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, classifyPostgres(err)
	}
	rows.Close()
	result := make([]Version, 0, len(ids))
	for _, id := range ids {
		version, readErr := readVersion(ctx, tx, id, false)
		if readErr != nil {
			return nil, readErr
		}
		result = append(result, version)
	}
	return result, nil
}

func insertVersion(ctx context.Context, tx pgx.Tx, version Version) error {
	_, err := tx.Exec(ctx, `INSERT INTO secret_binding_versions(id,binding_id,version_number,provider,state,target_secret_type,fingerprint_key_id,content_fingerprint,failure_code,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,'',$9,$9)`, version.ID, version.BindingID, version.Number, version.Provider, version.State,
		version.TargetSecretType, version.FingerprintKeyID, version.ContentFingerprint[:], version.CreatedAt)
	return classifyPostgres(err)
}

func insertDeliveries(ctx context.Context, tx pgx.Tx, version Version) error {
	for ordinal, delivery := range version.Deliveries {
		_, err := tx.Exec(ctx, `INSERT INTO secret_binding_deliveries(version_id,binding_id,ordinal,source_key,kind,environment_name,file_path,file_mode) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
			version.ID, version.BindingID, ordinal, delivery.SourceKey, delivery.Kind, nullableString(delivery.EnvironmentName), nullableString(delivery.FilePath), nullableMode(delivery.FileMode))
		if err != nil {
			return classifyPostgres(err)
		}
	}
	return nil
}

func insertIdempotency(ctx context.Context, tx pgx.Tx, value Idempotency) error {
	_, err := tx.Exec(ctx, `INSERT INTO mutation_receipts(actor_id,receipt_kind,namespace,scope_key,idempotency_key,request_fingerprint,secret_binding_id,secret_version_id,created_at) VALUES($1,'secret-binding',$2,$3::text,$4,$5,$6,$7,$8)`,
		value.ActorID, value.Operation, value.ApplicationID, value.Key, value.RequestFingerprint[:], value.BindingID, value.VersionID, value.CreatedAt)
	return classifyPostgres(err)
}

func insertEvent(ctx context.Context, tx pgx.Tx, event Event) error {
	_, err := tx.Exec(ctx, `INSERT INTO secret_binding_events(id,binding_id,version_id,actor_id,kind,request_id,occurred_at) VALUES($1,$2,$3,$4,$5,$6,$7)`,
		event.ID, event.BindingID, nullableString(event.VersionID), nullableString(event.ActorID), event.Kind, event.RequestID, event.OccurredAt)
	return classifyPostgres(err)
}

func (s *PostgreSQLStore) lookupReplay(ctx context.Context, input Idempotency) (Binding, Version, bool, error) {
	if input.validate() != nil {
		return Binding{}, Version{}, false, ErrInvalid
	}
	var fingerprint []byte
	var bindingID, versionID string
	err := s.pool.QueryRow(ctx, `SELECT request_fingerprint,secret_binding_id::text,secret_version_id::text FROM mutation_receipts WHERE actor_id=$1 AND receipt_kind='secret-binding' AND namespace=$2 AND scope_key=$3::text AND idempotency_key=$4`,
		input.ActorID, input.Operation, input.ApplicationID, input.Key).Scan(&fingerprint, &bindingID, &versionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Binding{}, Version{}, false, nil
	}
	if err != nil {
		return Binding{}, Version{}, false, classifyPostgres(err)
	}
	if len(fingerprint) != 32 || subtleBytes(fingerprint, input.RequestFingerprint[:]) == false {
		return Binding{}, Version{}, false, ErrConflict
	}
	binding, err := s.Binding(ctx, bindingID)
	if err != nil {
		return Binding{}, Version{}, false, err
	}
	version, err := s.Version(ctx, versionID)
	if err != nil {
		return Binding{}, Version{}, false, err
	}
	return binding, version, true, nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableMode(value uint32) any {
	if value == 0 {
		return nil
	}
	return int64(value)
}

func subtleBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var difference byte
	for i := range left {
		difference |= left[i] ^ right[i]
	}
	return difference == 0
}

func isUniqueViolation(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "23505"
}

func classifyPostgres(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		switch postgresError.Code {
		case "23505", "40001", "40P01":
			return ErrConflict
		case "23503":
			return ErrNotFound
		case "23502", "23514", "22P02", "22001":
			return ErrInvalid
		}
	}
	return err
}

var _ Store = (*PostgreSQLStore)(nil)
