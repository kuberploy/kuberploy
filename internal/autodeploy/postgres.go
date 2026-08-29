package autodeploy

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

func OpenPostgreSQLStore(ctx context.Context, databaseURL string) (*PostgreSQLStore, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, ErrInvalid
	}
	config.ConnConfig.RuntimeParams["application_name"] = "kuberploy-auto-deploy"
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

func (s *PostgreSQLStore) ClaimNextRun(ctx context.Context, owner string, now time.Time, duration time.Duration) (Work, error) {
	if s == nil || s.pool == nil || owner == "" || len(owner) > 128 || now.IsZero() || duration < 15*time.Second || duration > 5*time.Minute {
		return Work{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Work{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var attemptID, policyID string
	err = tx.QueryRow(ctx, `SELECT attempt_id::text,policy_id::text FROM auto_deploy_runs
		WHERE attempts<20 AND available_at<=$1 AND (state='pending' OR (state='processing' AND lease_until<=$1))
		ORDER BY available_at,created_at,attempt_id,policy_id FOR UPDATE SKIP LOCKED LIMIT 1`, now.UTC()).Scan(&attemptID, &policyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Work{}, ErrNotFound
	}
	if err != nil {
		return Work{}, classifyPostgres(err)
	}
	lease := Lease{AttemptID: attemptID, PolicyID: policyID, Owner: owner, Until: now.UTC().Add(duration)}
	err = tx.QueryRow(ctx, `UPDATE auto_deploy_runs SET state='processing',attempts=attempts+1,lease_owner=$3,
		lease_until=$4,lease_epoch=lease_epoch+1,failure_code='',updated_at=$5 WHERE attempt_id=$1 AND policy_id=$2 RETURNING lease_epoch`,
		attemptID, policyID, owner, lease.Until, now.UTC()).Scan(&lease.Epoch)
	if err != nil {
		return Work{}, classifyPostgres(err)
	}
	work, err := scanWork(ctx, tx, attemptID, policyID, lease)
	if err != nil {
		return Work{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Work{}, classifyPostgres(err)
	}
	return work, nil
}

type queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func scanWork(ctx context.Context, q queryer, attemptID, policyID string, lease Lease) (Work, error) {
	var w Work
	w.Lease = lease
	var completed *time.Time
	err := q.QueryRow(ctx, `SELECT p.id::text,p.project_id::text,p.application_id::text,p.environment_id::text,p.current_revision,p.created_by::text,p.created_at,
		r.policy_id::text,r.revision,r.enabled,r.source_deployment_id::text,r.source_deployment_generation,r.source_config_etag,r.config_intent,r.template_digest,r.service_actor_id::text,r.created_by::text,r.created_at,
		x.attempt_id::text,x.policy_id::text,x.policy_revision,x.definition_id::text,x.definition_digest,x.release_id::text,x.template_digest,
		x.source_deployment_id::text,x.source_deployment_generation,x.source_config_etag,x.idempotency_key,x.state,x.attempts,x.available_at,COALESCE(x.operation_id::text,''),COALESCE(x.deployment_id::text,''),x.failure_code,x.created_at,x.updated_at,x.completed_at
		FROM auto_deploy_runs x JOIN auto_deploy_policies p ON p.id=x.policy_id JOIN auto_deploy_policy_revisions r ON r.policy_id=x.policy_id AND r.revision=x.policy_revision
		WHERE x.attempt_id=$1 AND x.policy_id=$2`, attemptID, policyID).Scan(
		&w.Policy.ID, &w.Policy.ProjectID, &w.Policy.ApplicationID, &w.Policy.EnvironmentID, &w.Policy.CurrentRevision, &w.Policy.CreatedBy, &w.Policy.CreatedAt,
		&w.Revision.PolicyID, &w.Revision.Revision, &w.Revision.Enabled, &w.Revision.Template.SourceDeploymentID, &w.Revision.Template.SourceDeploymentGeneration, &w.Revision.Template.SourceConfigETag, &w.Revision.Template.ConfigIntent, &w.Revision.TemplateDigest, &w.Revision.ServiceActorID, &w.Revision.CreatedBy, &w.Revision.CreatedAt,
		&w.Run.AttemptID, &w.Run.PolicyID, &w.Run.PolicyRevision, &w.Run.DefinitionID, &w.Run.DefinitionDigest, &w.Run.ReleaseID, &w.Run.TemplateDigest,
		&w.Run.SourceDeploymentID, &w.Run.SourceDeploymentGeneration, &w.Run.SourceConfigETag, &w.Run.IdempotencyKey, &w.Run.State, &w.Run.Attempts, &w.Run.AvailableAt, &w.Run.OperationID, &w.Run.DeploymentID, &w.Run.FailureCode, &w.Run.CreatedAt, &w.Run.UpdatedAt, &completed)
	if err != nil {
		return Work{}, classifyPostgres(err)
	}
	w.Run.CompletedAt = completed
	if validateWork(w) != nil {
		return Work{}, ErrConflict
	}
	return w, nil
}

func (s *PostgreSQLStore) HeartbeatRun(ctx context.Context, lease Lease, now time.Time, duration time.Duration) (Lease, error) {
	if !validLease(lease) || now.IsZero() || duration < 15*time.Second || duration > 5*time.Minute {
		return Lease{}, ErrInvalid
	}
	until := now.UTC().Add(duration)
	if !until.After(lease.Until) {
		until = lease.Until.Add(time.Microsecond)
	}
	result, err := s.pool.Exec(ctx, `UPDATE auto_deploy_runs SET lease_until=$6,updated_at=$5 WHERE attempt_id=$1 AND policy_id=$2 AND state='processing' AND lease_owner=$3 AND lease_epoch=$4 AND lease_until>$5`, lease.AttemptID, lease.PolicyID, lease.Owner, lease.Epoch, now.UTC(), until)
	if err != nil {
		return Lease{}, classifyPostgres(err)
	}
	if result.RowsAffected() != 1 {
		return Lease{}, ErrLeaseLost
	}
	lease.Until = until
	return lease, nil
}

func (s *PostgreSQLStore) RetryRun(ctx context.Context, lease Lease, code string, now, available time.Time) error {
	if !validLease(lease) || !validFailure(code) || now.IsZero() || available.Before(now.UTC()) {
		return ErrInvalid
	}
	result, err := s.pool.Exec(ctx, `UPDATE auto_deploy_runs SET state=CASE WHEN attempts>=20 THEN 'failed' ELSE 'pending' END,
		available_at=CASE WHEN attempts>=20 THEN available_at ELSE $7 END,lease_owner=NULL,lease_until=NULL,failure_code=$6,
		completed_at=CASE WHEN attempts>=20 THEN $5 ELSE NULL END,updated_at=$5 WHERE attempt_id=$1 AND policy_id=$2 AND state='processing' AND lease_owner=$3 AND lease_epoch=$4 AND lease_until>$5`,
		lease.AttemptID, lease.PolicyID, lease.Owner, lease.Epoch, now.UTC(), code, available.UTC())
	if err != nil {
		return classifyPostgres(err)
	}
	if result.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (s *PostgreSQLStore) FailRun(ctx context.Context, lease Lease, code string, now time.Time) error {
	if !validLease(lease) || !validFailure(code) || now.IsZero() {
		return ErrInvalid
	}
	result, err := s.pool.Exec(ctx, `UPDATE auto_deploy_runs SET state='failed',lease_owner=NULL,lease_until=NULL,failure_code=$6,completed_at=$5,updated_at=$5 WHERE attempt_id=$1 AND policy_id=$2 AND state='processing' AND lease_owner=$3 AND lease_epoch=$4 AND lease_until>$5`, lease.AttemptID, lease.PolicyID, lease.Owner, lease.Epoch, now.UTC(), code)
	if err != nil {
		return classifyPostgres(err)
	}
	if result.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (s *PostgreSQLStore) CompleteRun(ctx context.Context, lease Lease, receipt SubmissionReceipt, now time.Time) error {
	if !validLease(lease) || !uuidRE.MatchString(receipt.OperationID) || !uuidRE.MatchString(receipt.DeploymentID) || now.IsZero() {
		return ErrInvalid
	}
	result, err := s.pool.Exec(ctx, `UPDATE auto_deploy_runs SET state='submitted',lease_owner=NULL,lease_until=NULL,failure_code='',operation_id=$6,deployment_id=$7,completed_at=$5,updated_at=$5 WHERE attempt_id=$1 AND policy_id=$2 AND state='processing' AND lease_owner=$3 AND lease_epoch=$4 AND lease_until>$5`, lease.AttemptID, lease.PolicyID, lease.Owner, lease.Epoch, now.UTC(), receipt.OperationID, receipt.DeploymentID)
	if err != nil {
		return classifyPostgres(err)
	}
	if result.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}

func validLease(l Lease) bool {
	return uuidRE.MatchString(l.AttemptID) && uuidRE.MatchString(l.PolicyID) && l.Owner != "" && len(l.Owner) <= 128 && l.Epoch > 0 && !l.Until.IsZero()
}
func validFailure(v string) bool {
	if len(v) < 1 || len(v) > 63 {
		return false
	}
	for i, c := range v {
		if i == 0 && (c < 'a' || c > 'z') || i > 0 && !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.' || c == '-') {
			return false
		}
	}
	return true
}
func classifyPostgres(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23503":
			return ErrNotFound
		case "23505", "23514", "40001":
			return ErrConflict
		}
	}
	return err
}

var _ RunStore = (*PostgreSQLStore)(nil)
