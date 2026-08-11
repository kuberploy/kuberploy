package environmentfoundation

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresStore struct{ pool *pgxpool.Pool }

func NewPostgresStore(pool *pgxpool.Pool) (*PostgresStore, error) {
	if pool == nil {
		return nil, ErrInvalid
	}
	return &PostgresStore{pool: pool}, nil
}

func OpenPostgresStore(ctx context.Context, databaseURL string) (*PostgresStore, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, ErrInvalid
	}
	config.ConnConfig.RuntimeParams["application_name"] = "kuberploy-environment-foundation"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &PostgresStore{pool: pool}, nil
}

func (s *PostgresStore) Close() {
	if s != nil && s.pool != nil {
		s.pool.Close()
	}
}

const intentColumns = `id::text,environment_id::text,project_id::text,namespace,argo_project,
	platform_binding_id::text,cluster_id::text,target_ref,planned_head_revision,binding_generation,
	profile_digest,publisher_config_digest,publisher_contract,publisher_policy,
	manifest_path,manifest,manifest_digest,intent_digest,
	commit_trailer,state,active,next_attempt_at,attempts,consecutive_failures,last_failure_code,
	COALESCE(lease_owner,''),lease_epoch,lease_until,write_base_revision,write_base_observed_at,committed_revision,
	committed_parent_revision,provider_request,published_at,completed_at,created_at,updated_at`

type rowScanner interface{ Scan(...any) error }

func scanIntent(row rowScanner) (Intent, error) {
	var v Intent
	err := row.Scan(&v.ID, &v.EnvironmentID, &v.ProjectID, &v.Namespace, &v.ArgoProject,
		&v.Authority.BindingID, &v.Authority.ClusterID, &v.Authority.TargetRef, &v.Authority.PlannedHead, &v.Authority.Generation,
		&v.ProfileDigest, &v.PublisherConfigDigest, &v.PublisherContractVersion, &v.PublisherPolicy,
		&v.Path, &v.Manifest, &v.ManifestDigest, &v.IntentDigest, &v.CommitTrailer,
		&v.State, &v.Active, &v.NextAttemptAt, &v.Attempts, &v.ConsecutiveFailures, &v.LastFailureCode, &v.LeaseOwner,
		&v.LeaseEpoch, &v.LeaseUntil, &v.WriteBaseRevision, &v.WriteBaseObservedAt, &v.CommittedRevision, &v.CommittedParentRevision, &v.ProviderRequest,
		&v.PublishedAt, &v.CompletedAt, &v.CreatedAt, &v.UpdatedAt)
	if err != nil {
		return Intent{}, mapPG(err)
	}
	v.NextAttemptAt = v.NextAttemptAt.UTC()
	v.CreatedAt = v.CreatedAt.UTC()
	v.UpdatedAt = v.UpdatedAt.UTC()
	if v.LeaseUntil != nil {
		x := v.LeaseUntil.UTC()
		v.LeaseUntil = &x
	}
	if v.WriteBaseObservedAt != nil {
		x := v.WriteBaseObservedAt.UTC()
		v.WriteBaseObservedAt = &x
	}
	if v.PublishedAt != nil {
		x := v.PublishedAt.UTC()
		v.PublishedAt = &x
	}
	if v.CompletedAt != nil {
		x := v.CompletedAt.UTC()
		v.CompletedAt = &x
	}
	if v.Validate() != nil {
		return Intent{}, ErrConflict
	}
	return v, nil
}

func (s *PostgresStore) EnsureIntent(ctx context.Context, request EnsureRequest) (Intent, error) {
	request.Now = request.Now.UTC()
	if !uuidRE.MatchString(request.IntentID) || !uuidRE.MatchString(request.EnvironmentID) || request.Profile.Validate() != nil || request.Now.IsZero() {
		return Intent{}, ErrInvalid
	}
	profileDigest, err := request.Profile.Digest()
	if err != nil {
		return Intent{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Intent{}, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	existing, err := scanIntent(tx.QueryRow(ctx, `SELECT `+intentColumns+` FROM environment_foundation_intents WHERE id=$1`, request.IntentID))
	if err == nil {
		if existing.EnvironmentID != request.EnvironmentID || existing.ProfileDigest != profileDigest ||
			existing.PublisherConfigDigest != request.Profile.PublisherConfigDigest || existing.Authority.ClusterID != request.Profile.ClusterID ||
			existing.Authority.BindingID != request.Profile.PlatformBindingID {
			return Intent{}, ErrConflict
		}
		if err = tx.Commit(ctx); err != nil {
			return Intent{}, mapPG(err)
		}
		return existing, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Intent{}, err
	}
	var identity EnvironmentIdentity
	var authority GitAuthority
	err = tx.QueryRow(ctx, `SELECT e.id::text,e.project_id::text,e.namespace,e.argo_project,
		b.id::text,b.cluster_id::text,b.target_ref,b.target_head_revision,b.projection_generation
		FROM environments e JOIN git_repository_bindings b
		  ON b.kind='platform' AND b.id=$3 AND b.cluster_id=$2
		WHERE e.id=$1 AND b.state='ready' AND b.target_head_revision IS NOT NULL
		  AND b.indexed_revision=b.target_head_revision AND b.projection_generation>0
		FOR SHARE OF e,b`, request.EnvironmentID, request.Profile.ClusterID, request.Profile.PlatformBindingID).Scan(
		&identity.EnvironmentID, &identity.ProjectID, &identity.Namespace, &identity.ArgoProject,
		&authority.BindingID, &authority.ClusterID, &authority.TargetRef, &authority.PlannedHead, &authority.Generation)
	if err != nil {
		return Intent{}, mapPG(err)
	}
	value, err := buildIntent(request.IntentID, identity, authority, request.Profile, request.Now)
	if err != nil {
		return Intent{}, err
	}
	active, err := scanIntent(tx.QueryRow(ctx, `SELECT `+intentColumns+` FROM environment_foundation_intents WHERE environment_id=$1 AND active FOR UPDATE`, request.EnvironmentID))
	if err == nil {
		if active.IntentDigest == value.IntentDigest {
			if err = tx.Commit(ctx); err != nil {
				return Intent{}, mapPG(err)
			}
			return active, nil
		}
		_, err = tx.Exec(ctx, `UPDATE environment_foundation_intents SET state='superseded',active=false,
			lease_owner=NULL,lease_until=NULL,completed_at=COALESCE(completed_at,$2),updated_at=$2 WHERE id=$1`, active.ID, request.Now)
		if err != nil {
			return Intent{}, mapPG(err)
		}
	} else if !errors.Is(err, ErrNotFound) {
		return Intent{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO environment_foundation_intents(
		id,environment_id,project_id,namespace,argo_project,platform_binding_id,cluster_id,target_ref,
		planned_head_revision,binding_generation,profile_digest,publisher_config_digest,publisher_contract,publisher_policy,manifest_path,
		manifest,manifest_digest,intent_digest,commit_trailer,state,active,next_attempt_at,attempts,
		consecutive_failures,last_failure_code,lease_epoch,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,'pending',true,$20,0,0,'',0,$20,$20)`,
		value.ID, value.EnvironmentID, value.ProjectID, value.Namespace, value.ArgoProject, value.Authority.BindingID,
		value.Authority.ClusterID, value.Authority.TargetRef, value.Authority.PlannedHead, value.Authority.Generation,
		profileDigest, value.PublisherConfigDigest, value.PublisherContractVersion, value.PublisherPolicy,
		value.Path, value.Manifest, value.ManifestDigest, value.IntentDigest, value.CommitTrailer, request.Now)
	if err != nil {
		return Intent{}, mapPG(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return Intent{}, mapPG(err)
	}
	return value, nil
}

func (s *PostgresStore) Intent(ctx context.Context, id string) (Intent, error) {
	if !uuidRE.MatchString(id) {
		return Intent{}, ErrInvalid
	}
	return scanIntent(s.pool.QueryRow(ctx, `SELECT `+intentColumns+` FROM environment_foundation_intents WHERE id=$1`, id))
}

func (s *PostgresStore) EnvironmentIDs(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT id::text FROM environments ORDER BY id`)
	if err != nil {
		return nil, mapPG(err)
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil || !uuidRE.MatchString(id) {
			return nil, ErrConflict
		}
		ids = append(ids, id)
		if len(ids) > 10_000 {
			return nil, ErrConflict
		}
	}
	if err = rows.Err(); err != nil {
		return nil, mapPG(err)
	}
	return ids, nil
}

func (s *PostgresStore) ClaimIntent(ctx context.Context, owner, profile, publisher string, now time.Time, duration time.Duration) (Lease, bool, error) {
	now = now.UTC()
	if !workerIDRE.MatchString(owner) || !digestRE.MatchString(profile) || !digestRE.MatchString(publisher) || now.IsZero() || duration < MinimumLease || duration > MaximumLease {
		return Lease{}, false, ErrInvalid
	}
	until := now.Add(duration)
	row := s.pool.QueryRow(ctx, `WITH candidate AS (
		SELECT i.id AS candidate_id FROM environment_foundation_intents i
		 JOIN git_repository_bindings b ON b.id=i.platform_binding_id
		 WHERE i.active AND i.profile_digest=$1 AND i.publisher_config_digest=$2 AND i.attempts<30
		   AND i.state IN ('pending','claimed') AND i.next_attempt_at<=$3
		   AND (i.lease_until IS NULL OR i.lease_until<=$3)
		   AND NOT EXISTS (SELECT 1 FROM environment_foundation_intents held
		       WHERE held.id<>i.id AND held.platform_binding_id=i.platform_binding_id
		         AND held.active AND held.state='claimed' AND held.lease_until>$3)
		 ORDER BY i.next_attempt_at,i.id FOR UPDATE OF i,b SKIP LOCKED LIMIT 1)
		UPDATE environment_foundation_intents i SET state='claimed',lease_owner=$4,
		 lease_epoch=i.lease_epoch+1,lease_until=$5,attempts=i.attempts+1,updated_at=$3
		FROM candidate WHERE i.id=candidate.candidate_id RETURNING `+intentColumns, profile, publisher, now, owner, until)
	v, err := scanIntent(row)
	if errors.Is(err, ErrNotFound) {
		return Lease{}, false, nil
	}
	if err != nil {
		return Lease{}, false, err
	}
	// PostgreSQL stores timestamptz values at microsecond precision. Build the
	// lease from the authoritative value read back from the row instead of the
	// nanosecond-precision input used to calculate it.
	lease := Lease{Intent: v, Owner: owner, Epoch: v.LeaseEpoch, Until: *v.LeaseUntil}
	if lease.Validate(now) != nil {
		return Lease{}, false, ErrConflict
	}
	return lease, true, nil
}

func (s *PostgresStore) HeartbeatIntent(ctx context.Context, lease Lease, now time.Time, duration time.Duration) (Lease, error) {
	now = now.UTC()
	if duration < MinimumLease || duration > MaximumLease || lease.Validate(now.Add(-time.Nanosecond)) != nil {
		return Lease{}, ErrInvalid
	}
	until := now.Add(duration)
	v, err := scanIntent(s.pool.QueryRow(ctx, `UPDATE environment_foundation_intents SET lease_until=$6,updated_at=$5
		WHERE id=$1 AND active AND state='claimed' AND lease_owner=$2 AND lease_epoch=$3
		  AND lease_until=$4 AND lease_until>$5 RETURNING `+intentColumns, lease.Intent.ID, lease.Owner, lease.Epoch, lease.Until, now, until))
	if errors.Is(err, ErrNotFound) {
		return Lease{}, ErrLeaseLost
	}
	if err != nil {
		return Lease{}, err
	}
	return Lease{Intent: v, Owner: lease.Owner, Epoch: lease.Epoch, Until: *v.LeaseUntil}, nil
}

func (s *PostgresStore) BindWriteBase(ctx context.Context, lease Lease, revision string, observedAt, now time.Time) (Intent, error) {
	now, observedAt = now.UTC(), observedAt.UTC()
	if lease.Validate(now.Add(-time.Nanosecond)) != nil || !gitCommitRE.MatchString(revision) || observedAt.IsZero() || observedAt.After(now) {
		return Intent{}, ErrInvalid
	}
	v, err := scanIntent(s.pool.QueryRow(ctx, `UPDATE environment_foundation_intents SET
		write_base_revision=$6,write_base_observed_at=$7,updated_at=$8
		WHERE id=$1 AND active AND state='claimed' AND lease_owner=$2 AND lease_epoch=$3
		  AND publisher_config_digest=$4 AND publisher_contract=$5 AND lease_until>$8
		  AND write_base_revision='' AND created_at<=$7 RETURNING `+intentColumns,
		lease.Intent.ID, lease.Owner, lease.Epoch, lease.Intent.PublisherConfigDigest,
		PublisherContract, revision, observedAt, now))
	if err == nil {
		return v, nil
	}
	current, getErr := s.Intent(ctx, lease.Intent.ID)
	if getErr == nil && current.Active && current.State == StateClaimed && current.LeaseOwner == lease.Owner &&
		current.LeaseEpoch == lease.Epoch && current.LeaseUntil != nil && current.LeaseUntil.After(now) &&
		current.WriteBaseRevision == revision && current.WriteBaseObservedAt != nil && current.WriteBaseObservedAt.Equal(observedAt) {
		return current, nil
	}
	if errors.Is(err, ErrNotFound) {
		return Intent{}, ErrConflict
	}
	return Intent{}, err
}

func (s *PostgresStore) RecordReady(ctx context.Context, lease Lease, receipt PublicationReceipt, now time.Time) (Intent, error) {
	now = now.UTC()
	if receipt.Validate(lease.Intent) != nil || receipt.ObservedAt.After(now) {
		return Intent{}, ErrInvalid
	}
	v, err := scanIntent(s.pool.QueryRow(ctx, `UPDATE environment_foundation_intents SET state='ready',
		lease_owner=NULL,lease_until=NULL,consecutive_failures=0,last_failure_code='',
		committed_revision=$6,committed_parent_revision=$7,provider_request=$8,
		published_at=$9,completed_at=$9,updated_at=$5
		WHERE id=$1 AND active AND state='claimed' AND lease_owner=$2 AND lease_epoch=$3
		  AND lease_until=$4 AND lease_until>$5 AND platform_binding_id=$10
		  AND target_ref=$11 AND manifest_path=$12 AND manifest_digest=$13 RETURNING `+intentColumns,
		lease.Intent.ID, lease.Owner, lease.Epoch, lease.Until, now, receipt.CommittedRevision,
		receipt.ParentRevision, receipt.ProviderRequest, receipt.ObservedAt.UTC(), receipt.BindingID,
		receipt.TargetRef, receipt.Path, receipt.ContentDigest))
	if errors.Is(err, ErrNotFound) {
		return Intent{}, ErrLeaseLost
	}
	return v, err
}

func (s *PostgresStore) RecordRetry(ctx context.Context, lease Lease, code string, permanent bool, next, now time.Time) (Intent, error) {
	now, next = now.UTC(), next.UTC()
	if !failureRE.MatchString(code) || next.Before(now) {
		return Intent{}, ErrInvalid
	}
	v, err := scanIntent(s.pool.QueryRow(ctx, `UPDATE environment_foundation_intents SET
		consecutive_failures=LEAST(30,consecutive_failures+1),last_failure_code=$6,
		next_attempt_at=$7,state=CASE WHEN $8 OR attempts>=30 THEN 'failed' ELSE 'pending' END,
		active=NOT($8 OR attempts>=30),completed_at=CASE WHEN $8 OR attempts>=30 THEN $5 ELSE NULL END,
		lease_owner=NULL,lease_until=NULL,updated_at=$5
		WHERE id=$1 AND active AND state='claimed' AND lease_owner=$2 AND lease_epoch=$3
		  AND lease_until=$4 AND lease_until>$5 RETURNING `+intentColumns,
		lease.Intent.ID, lease.Owner, lease.Epoch, lease.Until, now, code, next, permanent))
	if errors.Is(err, ErrNotFound) {
		return Intent{}, ErrLeaseLost
	}
	return v, err
}

func (s *PostgresStore) RecordReadiness(ctx context.Context, r Readiness) error {
	if r.Validate() != nil {
		return ErrInvalid
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO environment_foundation_readiness(
	worker_id,worker_epoch,contract_version,profile_digest,publisher_config_digest,active_intent_count,
	started_at,observed_at,lease_until) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
	ON CONFLICT(worker_id) DO UPDATE SET worker_epoch=EXCLUDED.worker_epoch,
	contract_version=EXCLUDED.contract_version,profile_digest=EXCLUDED.profile_digest,
	publisher_config_digest=EXCLUDED.publisher_config_digest,active_intent_count=EXCLUDED.active_intent_count,
	started_at=EXCLUDED.started_at,observed_at=EXCLUDED.observed_at,lease_until=EXCLUDED.lease_until`,
		r.WorkerID, r.WorkerEpoch, r.Contract, r.ProfileDigest, r.PublisherConfigDigest, r.ActiveIntentCount, r.StartedAt, r.ObservedAt, r.LeaseUntil)
	return mapPGExec(err)
}

func (s *PostgresStore) ExactReady(ctx context.Context, profile, publisher string, count int, now time.Time) error {
	now = now.UTC()
	if !digestRE.MatchString(profile) || !digestRE.MatchString(publisher) || count < 0 || count > 10000 || now.IsZero() {
		return ErrInvalid
	}
	var environments, active, ready int
	var found bool
	err := s.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM environments),
		count(*),count(*) FILTER(WHERE state='ready'),
		EXISTS(SELECT 1 FROM environment_foundation_readiness WHERE contract_version=$1
		  AND profile_digest=$2 AND publisher_config_digest=$3 AND active_intent_count=$4
		  AND observed_at<=$5 AND lease_until>$5)
		FROM environment_foundation_intents
		WHERE active AND profile_digest=$2 AND publisher_config_digest=$3`,
		Contract, profile, publisher, count, now).Scan(&environments, &active, &ready, &found)
	if err != nil {
		return mapPG(err)
	}
	if environments != count || active != count || ready != count || !found {
		return ErrUnavailable
	}
	return nil
}

func mapPG(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505", "23514", "40001":
			return ErrConflict
		}
	}
	return err
}
func mapPGExec(err error) error {
	if err == nil {
		return nil
	}
	return mapPG(err)
}

var _ Store = (*PostgresStore)(nil)
var _ EnvironmentCatalog = (*PostgresStore)(nil)
