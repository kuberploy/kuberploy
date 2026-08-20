package imagepull

import (
	"context"
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

// RetireUnconfiguredArtifacts runs at worker startup after the projected
// credential preflight. It stops claiming artifacts from an old operator
// profile or namespace while retaining their immutable rows for rollback.
func (s *PostgreSQLStore) RetireUnconfiguredArtifacts(ctx context.Context, config RuntimeConfig, now time.Time) (int, error) {
	if s == nil || s.pool == nil || config.Validate() != nil || now.IsZero() {
		return 0, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	rows, err := tx.Query(ctx, `SELECT environment_id::text,namespace,registry_target_id::text,
		pull_credential_ref,profile_name,profile_revision,secret_name
		FROM runtime_registry_pull_artifacts WHERE active FOR UPDATE`)
	if err != nil {
		return 0, classifyPostgres(err)
	}
	type artifactIdentity struct {
		environmentID, namespace, targetID, pullCredentialRef, profileName, secretName string
		profileRevision                                                                int64
	}
	active := make([]artifactIdentity, 0)
	for rows.Next() {
		var identity artifactIdentity
		if err = rows.Scan(&identity.environmentID, &identity.namespace, &identity.targetID,
			&identity.pullCredentialRef, &identity.profileName, &identity.profileRevision, &identity.secretName); err != nil {
			rows.Close()
			return 0, classifyPostgres(err)
		}
		active = append(active, identity)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, classifyPostgres(err)
	}
	rows.Close()
	retired := 0
	for _, identity := range active {
		profile, found := config.ProfileForTarget(identity.targetID)
		if found && config.AllowsNamespace(identity.namespace) && identity.pullCredentialRef == profile.CredentialRef &&
			identity.profileName == profile.Name && identity.profileRevision == profile.Revision &&
			identity.secretName == SecretName(identity.namespace, identity.targetID, profile.Revision) {
			continue
		}
		if _, err = tx.Exec(ctx, `UPDATE runtime_registry_pull_artifacts
			SET active=false,lease_owner=NULL,lease_until=NULL,worker_contract=NULL,
				worker_config_digest=NULL,updated_at=$2
			WHERE environment_id=$1 AND registry_target_id=$3 AND profile_revision=$4 AND active`,
			identity.environmentID, now.UTC(), identity.targetID, identity.profileRevision); err != nil {
			return 0, classifyPostgres(err)
		}
		retired++
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, classifyPostgres(err)
	}
	return retired, nil
}

func OpenPostgreSQLStore(ctx context.Context, databaseURL string) (*PostgreSQLStore, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, ErrInvalid
	}
	config.ConnConfig.RuntimeParams["application_name"] = "kuberploy-runtime-registry-pulls"
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

func (s *PostgreSQLStore) Close() {
	if s != nil && s.pool != nil {
		s.pool.Close()
	}
}

const artifactColumns = `environment_id::text,namespace,registry_target_id::text,pull_credential_ref,
	profile_name,profile_revision,secret_name,active,runtime_state,next_observation_at,last_observed_at,
	consecutive_failures,last_failure_code,observed_uid,observed_resource_version,COALESCE(lease_owner,''),
	lease_epoch,lease_until,COALESCE(worker_contract,''),COALESCE(worker_config_digest,''),created_at,updated_at`

type rowScanner interface{ Scan(...any) error }

func scanArtifact(row rowScanner) (Artifact, error) {
	var artifact Artifact
	err := row.Scan(&artifact.EnvironmentID, &artifact.Namespace, &artifact.RegistryTargetID, &artifact.PullCredentialRef,
		&artifact.ProfileName, &artifact.ProfileRevision, &artifact.SecretName, &artifact.Active, &artifact.State,
		&artifact.NextObservationAt, &artifact.LastObservedAt, &artifact.ConsecutiveFailures, &artifact.LastFailureCode,
		&artifact.ObservedUID, &artifact.ObservedResourceVersion, &artifact.LeaseOwner, &artifact.LeaseEpoch,
		&artifact.LeaseUntil, &artifact.WorkerContract, &artifact.WorkerConfigDigest, &artifact.CreatedAt, &artifact.UpdatedAt)
	if err != nil {
		return Artifact{}, classifyPostgres(err)
	}
	artifact.NextObservationAt = artifact.NextObservationAt.UTC()
	artifact.CreatedAt = artifact.CreatedAt.UTC()
	artifact.UpdatedAt = artifact.UpdatedAt.UTC()
	if artifact.LastObservedAt != nil {
		value := artifact.LastObservedAt.UTC()
		artifact.LastObservedAt = &value
	}
	if artifact.LeaseUntil != nil {
		value := artifact.LeaseUntil.UTC()
		artifact.LeaseUntil = &value
	}
	if artifact.Validate() != nil {
		return Artifact{}, ErrConflict
	}
	return artifact, nil
}

func (s *PostgreSQLStore) EnsureArtifact(ctx context.Context, desired DesiredArtifact, now time.Time) (Artifact, error) {
	if desired.Validate() != nil || now.IsZero() {
		return Artifact{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Artifact{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	artifact, err := EnsureArtifactTx(ctx, tx, desired, now.UTC())
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		return Artifact{}, classifyPostgres(err)
	}
	return artifact, nil
}

// EnsureArtifactTx lets Git projection policy stage an exact pull artifact in
// the same serializable transaction as generation validation and activation.
func EnsureArtifactTx(ctx context.Context, tx pgx.Tx, desired DesiredArtifact, now time.Time) (Artifact, error) {
	if tx == nil || desired.Validate() != nil || now.IsZero() {
		return Artifact{}, ErrInvalid
	}
	var namespace, pullRef string
	err := tx.QueryRow(ctx, `SELECT e.namespace,t.pull_credential_ref
		FROM environments e CROSS JOIN registry_targets t
		WHERE e.id=$1 AND t.id=$2 FOR KEY SHARE OF e,t`, desired.EnvironmentID, desired.RegistryTargetID).
		Scan(&namespace, &pullRef)
	if err != nil {
		return Artifact{}, classifyPostgres(err)
	}
	if namespace != desired.Namespace || pullRef == "" || pullRef != desired.PullCredentialRef {
		return Artifact{}, ErrConflict
	}
	existing, err := scanArtifact(tx.QueryRow(ctx, `SELECT `+artifactColumns+` FROM runtime_registry_pull_artifacts
		WHERE environment_id=$1 AND registry_target_id=$2 AND profile_revision=$3 FOR UPDATE`,
		desired.EnvironmentID, desired.RegistryTargetID, desired.ProfileRevision))
	found := err == nil
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Artifact{}, err
	}
	if found && (existing.DesiredArtifact != desired || existing.State == StateFailed) {
		return Artifact{}, ErrConflict
	}
	if found && existing.Active {
		return existing, nil
	}
	_, err = tx.Exec(ctx, `UPDATE runtime_registry_pull_artifacts
		SET active=false,lease_owner=NULL,lease_until=NULL,worker_contract=NULL,worker_config_digest=NULL,updated_at=$3
		WHERE environment_id=$1 AND registry_target_id=$2 AND active
		  AND profile_revision<>$4`, desired.EnvironmentID, desired.RegistryTargetID, now.UTC(), desired.ProfileRevision)
	if err != nil {
		return Artifact{}, classifyPostgres(err)
	}
	if found {
		return scanArtifact(tx.QueryRow(ctx, `UPDATE runtime_registry_pull_artifacts
			SET active=true,next_observation_at=$4,lease_owner=NULL,lease_until=NULL,
			    worker_contract=NULL,worker_config_digest=NULL,updated_at=$4
			WHERE environment_id=$1 AND registry_target_id=$2 AND profile_revision=$3
			RETURNING `+artifactColumns, desired.EnvironmentID, desired.RegistryTargetID, desired.ProfileRevision, now.UTC()))
	}
	return scanArtifact(tx.QueryRow(ctx, `INSERT INTO runtime_registry_pull_artifacts(
		environment_id,namespace,registry_target_id,pull_credential_ref,profile_name,profile_revision,
		secret_name,active,runtime_state,next_observation_at,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,true,'awaiting',$8,$8,$8)
		RETURNING `+artifactColumns, desired.EnvironmentID, desired.Namespace, desired.RegistryTargetID,
		desired.PullCredentialRef, desired.ProfileName, desired.ProfileRevision, desired.SecretName, now.UTC()))
}

func (s *PostgreSQLStore) Artifact(ctx context.Context, key ArtifactKey) (Artifact, error) {
	if !uuidPattern.MatchString(key.EnvironmentID) || !uuidPattern.MatchString(key.RegistryTargetID) || key.ProfileRevision <= 0 {
		return Artifact{}, ErrInvalid
	}
	return scanArtifact(s.pool.QueryRow(ctx, `SELECT `+artifactColumns+` FROM runtime_registry_pull_artifacts
		WHERE environment_id=$1 AND registry_target_id=$2 AND profile_revision=$3`,
		key.EnvironmentID, key.RegistryTargetID, key.ProfileRevision))
}

func (s *PostgreSQLStore) ClaimArtifact(ctx context.Context, owner, contract, configDigest string, now time.Time, duration time.Duration) (Lease, bool, error) {
	if !workerIDPattern.MatchString(owner) || contract != RuntimeContract || !digestPattern(configDigest) || now.IsZero() || duration < time.Second {
		return Lease{}, false, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Lease{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var key ArtifactKey
	err = tx.QueryRow(ctx, `SELECT environment_id::text,registry_target_id::text,profile_revision
		FROM runtime_registry_pull_artifacts
		WHERE active AND (runtime_state<>'failed' OR last_failure_code='profile-mismatch') AND next_observation_at<=$1
		  AND (lease_until IS NULL OR lease_until<=$1)
		ORDER BY next_observation_at,environment_id,registry_target_id,profile_revision
		FOR UPDATE SKIP LOCKED LIMIT 1`, now.UTC()).Scan(&key.EnvironmentID, &key.RegistryTargetID, &key.ProfileRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, false, nil
	}
	if err != nil {
		return Lease{}, false, classifyPostgres(err)
	}
	artifact, err := scanArtifact(tx.QueryRow(ctx, `UPDATE runtime_registry_pull_artifacts
		SET lease_owner=$4,lease_epoch=lease_epoch+1,lease_until=$5,worker_contract=$6,
		    worker_config_digest=$7,updated_at=$8
		WHERE environment_id=$1 AND registry_target_id=$2 AND profile_revision=$3 AND active
		RETURNING `+artifactColumns, key.EnvironmentID, key.RegistryTargetID, key.ProfileRevision, owner,
		now.UTC().Add(duration), contract, configDigest, now.UTC()))
	if err != nil {
		return Lease{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Lease{}, false, classifyPostgres(err)
	}
	lease := Lease{Artifact: artifact, Owner: owner, Epoch: artifact.LeaseEpoch, Until: *artifact.LeaseUntil}
	if lease.Validate(now.UTC()) != nil {
		return Lease{}, false, ErrConflict
	}
	return lease, true, nil
}

func (s *PostgreSQLStore) HeartbeatArtifact(ctx context.Context, lease Lease, now time.Time, duration time.Duration) (Lease, error) {
	if duration < time.Second || now.IsZero() || lease.Validate(now.UTC().Add(-time.Nanosecond)) != nil {
		return Lease{}, ErrInvalid
	}
	artifact, err := scanArtifact(s.pool.QueryRow(ctx, `UPDATE runtime_registry_pull_artifacts
		SET lease_until=$9,updated_at=$8
		WHERE environment_id=$1 AND registry_target_id=$2 AND profile_revision=$3 AND active
		  AND lease_owner=$4 AND lease_epoch=$5 AND lease_until=$6 AND lease_until>$8
		  AND worker_contract=$7 AND worker_config_digest=$10
		RETURNING `+artifactColumns, lease.Artifact.EnvironmentID, lease.Artifact.RegistryTargetID,
		lease.Artifact.ProfileRevision, lease.Owner, lease.Epoch, lease.Until, RuntimeContract, now.UTC(),
		now.UTC().Add(duration), lease.Artifact.WorkerConfigDigest))
	if errors.Is(err, ErrNotFound) {
		return Lease{}, ErrLeaseLost
	}
	if err != nil {
		return Lease{}, err
	}
	return Lease{Artifact: artifact, Owner: lease.Owner, Epoch: lease.Epoch, Until: *artifact.LeaseUntil}, nil
}

func (s *PostgreSQLStore) RecordArtifactReady(ctx context.Context, lease Lease, uid, resourceVersion string, observedAt, next time.Time) (Artifact, error) {
	if !uuidPattern.MatchString(uid) || !resourceVersionPattern.MatchString(resourceVersion) || observedAt.IsZero() || next.Before(observedAt) {
		return Artifact{}, ErrInvalid
	}
	if lease.Validate(observedAt.UTC().Add(-time.Nanosecond)) != nil {
		return Artifact{}, ErrInvalid
	}
	artifact, err := scanArtifact(s.pool.QueryRow(ctx, `UPDATE runtime_registry_pull_artifacts
		SET runtime_state='ready',next_observation_at=$9,last_observed_at=$8,consecutive_failures=0,
		    last_failure_code='',observed_uid=$10,observed_resource_version=$11,
		    lease_owner=NULL,lease_until=NULL,worker_contract=NULL,worker_config_digest=NULL,updated_at=$8
		WHERE environment_id=$1 AND registry_target_id=$2 AND profile_revision=$3 AND active
		  AND lease_owner=$4 AND lease_epoch=$5 AND lease_until=$6 AND lease_until>$8
		  AND worker_contract=$7 AND worker_config_digest=$12
		RETURNING `+artifactColumns, lease.Artifact.EnvironmentID, lease.Artifact.RegistryTargetID,
		lease.Artifact.ProfileRevision, lease.Owner, lease.Epoch, lease.Until, RuntimeContract, observedAt.UTC(),
		next.UTC(), uid, resourceVersion, lease.Artifact.WorkerConfigDigest))
	if errors.Is(err, ErrNotFound) {
		return Artifact{}, ErrLeaseLost
	}
	return artifact, err
}

func (s *PostgreSQLStore) RecordArtifactRetry(ctx context.Context, lease Lease, code string, permanent bool, next, now time.Time) (Artifact, error) {
	if !failureCodePattern.MatchString(code) || now.IsZero() || next.Before(now) {
		return Artifact{}, ErrInvalid
	}
	if lease.Validate(now.UTC().Add(-time.Nanosecond)) != nil {
		return Artifact{}, ErrInvalid
	}
	artifact, err := scanArtifact(s.pool.QueryRow(ctx, `UPDATE runtime_registry_pull_artifacts
		SET runtime_state=CASE WHEN $10 OR consecutive_failures>=29 THEN 'failed' ELSE runtime_state END,
		    next_observation_at=$9,consecutive_failures=LEAST(30,consecutive_failures+1),last_failure_code=$11,
		    lease_owner=NULL,lease_until=NULL,worker_contract=NULL,worker_config_digest=NULL,updated_at=$8
		WHERE environment_id=$1 AND registry_target_id=$2 AND profile_revision=$3 AND active
		  AND lease_owner=$4 AND lease_epoch=$5 AND lease_until=$6 AND lease_until>$8
		  AND worker_contract=$7 AND worker_config_digest=$12
		RETURNING `+artifactColumns, lease.Artifact.EnvironmentID, lease.Artifact.RegistryTargetID,
		lease.Artifact.ProfileRevision, lease.Owner, lease.Epoch, lease.Until, RuntimeContract, now.UTC(), next.UTC(),
		permanent, code, lease.Artifact.WorkerConfigDigest))
	if errors.Is(err, ErrNotFound) {
		return Artifact{}, ErrLeaseLost
	}
	return artifact, err
}

func (s *PostgreSQLStore) ActiveArtifactsHealthy(ctx context.Context, staleBefore time.Time) (bool, error) {
	if staleBefore.IsZero() {
		return false, ErrInvalid
	}
	var healthy bool
	err := s.pool.QueryRow(ctx, `SELECT NOT EXISTS(
		SELECT 1 FROM runtime_registry_pull_artifacts
		WHERE active AND (runtime_state<>'ready' OR last_observed_at IS NULL OR last_observed_at<$1)
	)`, staleBefore.UTC()).Scan(&healthy)
	if err != nil {
		return false, classifyPostgres(err)
	}
	return healthy, nil
}

func (s *PostgreSQLStore) RecordReadiness(ctx context.Context, readiness Readiness) error {
	if readiness.Validate() != nil {
		return ErrInvalid
	}
	command, err := s.pool.Exec(ctx, `INSERT INTO runtime_readiness(runtime_kind,scope_key,worker_id,worker_epoch,
		contract_version,config_digest,identity,observation,started_at,observed_at,lease_until,updated_at)
		VALUES('runtime-registry-pull','global',$1,$2,$3,$4,jsonb_build_object('profileCount',$5::integer),'{}'::jsonb,$6,$7,$8,$7)
		ON CONFLICT(runtime_kind,scope_key,worker_id) DO UPDATE SET worker_epoch=EXCLUDED.worker_epoch,
		 contract_version=EXCLUDED.contract_version,config_digest=EXCLUDED.config_digest,
		 identity=EXCLUDED.identity,started_at=EXCLUDED.started_at,observed_at=EXCLUDED.observed_at,
		 lease_until=EXCLUDED.lease_until,updated_at=EXCLUDED.updated_at
		WHERE EXCLUDED.worker_epoch BETWEEN runtime_readiness.worker_epoch AND runtime_readiness.worker_epoch+1`,
		readiness.WorkerID, readiness.WorkerEpoch, readiness.Contract, readiness.ConfigDigest, readiness.ProfileCount,
		readiness.StartedAt.UTC(), readiness.ObservedAt.UTC(), readiness.LeaseUntil.UTC())
	if err != nil {
		return classifyPostgres(err)
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

func (s *PostgreSQLStore) RuntimeReady(ctx context.Context, contract, configDigest string, profileCount int, now time.Time) error {
	if contract != RuntimeContract || !digestPattern(configDigest) || profileCount < 1 || profileCount > MaximumProfiles || now.IsZero() {
		return ErrInvalid
	}
	var ready bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM runtime_readiness WHERE runtime_kind='runtime-registry-pull' AND scope_key='global'
		AND contract_version=$1 AND config_digest=$2 AND (identity->>'profileCount')::integer=$3
		  AND lease_until>$4 AND observed_at<=$5
	)`, contract, configDigest, profileCount, now.UTC(), now.UTC().Add(5*time.Second)).Scan(&ready)
	if err != nil {
		return classifyPostgres(err)
	}
	if !ready {
		return ErrUnavailable
	}
	return nil
}

func classifyPostgres(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var postgres *pgconn.PgError
	if errors.As(err, &postgres) {
		switch postgres.Code {
		case "23505", "23514", "23503", "40001", "40P01":
			return ErrConflict
		}
	}
	return err
}

var _ Store = (*PostgreSQLStore)(nil)
