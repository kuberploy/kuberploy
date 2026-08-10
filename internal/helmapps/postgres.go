package helmapps

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore operates only on migration 027's tables. Keeping it here
// avoids widening the platform Store or advertising an API capability before
// the renderer Job and admission boundary exist.
type PostgresStore struct {
	pool                 *pgxpool.Pool
	operatorConfigDigest string
}

func NewPostgresStore(pool *pgxpool.Pool, operatorConfigDigest string) (*PostgresStore, error) {
	if pool == nil || !validDigest(operatorConfigDigest) {
		return nil, ErrInvalid
	}
	return &PostgresStore{pool: pool, operatorConfigDigest: operatorConfigDigest}, nil
}

func (s *PostgresStore) PutApproval(ctx context.Context, approval Approval) (Approval, bool, error) {
	if approval.Validate() != nil {
		return Approval{}, false, ErrInvalid
	}
	identity, _ := approval.IdentityDigest()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Approval{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	stored, queryErr := scanApproval(tx.QueryRow(ctx, approvalSelect+` WHERE created_by=$1 AND idempotency_key=$2 FOR UPDATE`, approval.CreatedBy, approval.IdempotencyKey))
	if queryErr == nil {
		if !stored.replayEqual(approval) {
			return Approval{}, false, ErrConflict
		}
		if err = tx.Commit(ctx); err != nil {
			return Approval{}, false, err
		}
		return stored, true, nil
	}
	if !errors.Is(queryErr, pgx.ErrNoRows) {
		return Approval{}, false, classifyPostgres(queryErr)
	}
	_, err = tx.Exec(ctx, `INSERT INTO helm_chart_approvals(
		approval_id,revision,oci_repository,chart_version,manifest_digest,package_digest,
		values_schema_digest,renderer_image,renderer_version,policy_version,identity_digest,
		created_by,idempotency_key,created_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		approval.ID, approval.Revision, approval.OCIRepository, approval.ChartVersion,
		approval.ManifestDigest, approval.PackageDigest, approval.ValuesSchemaDigest,
		approval.RendererImage, approval.RendererVersion, approval.PolicyVersion, identity,
		approval.CreatedBy, approval.IdempotencyKey, approval.CreatedAt)
	if err != nil {
		return Approval{}, false, classifyPostgres(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return Approval{}, false, classifyPostgres(err)
	}
	return approval, false, nil
}

func (s *PostgresStore) Approval(ctx context.Context, key ApprovalKey) (Approval, error) {
	if key.Validate() != nil {
		return Approval{}, ErrInvalid
	}
	approval, err := scanApproval(s.pool.QueryRow(ctx, approvalSelect+` WHERE approval_id=$1 AND revision=$2`, key.ID, key.Revision))
	return approval, classifyPostgres(err)
}

func (s *PostgresStore) Submit(ctx context.Context, desired DesiredRender, now time.Time) (RenderCommand, bool, error) {
	if desired.Validate() != nil || now.IsZero() {
		return RenderCommand{}, false, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return RenderCommand{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	approval, err := scanApproval(tx.QueryRow(ctx, approvalSelect+` WHERE approval_id=$1 AND revision=$2`, desired.Approval.ID, desired.Approval.Revision))
	if errors.Is(err, pgx.ErrNoRows) {
		return RenderCommand{}, false, ErrNotFound
	}
	if err != nil {
		return RenderCommand{}, false, classifyPostgres(err)
	}
	if !desiredMatchesApproval(desired, approval) {
		return RenderCommand{}, false, ErrConflict
	}
	stored, replayErr := scanCommand(tx.QueryRow(ctx, commandSelect+` WHERE c.idempotency_scope=$1 AND c.idempotency_key=$2 FOR UPDATE`, desired.IdempotencyScope, desired.IdempotencyKey))
	if replayErr == nil {
		if !desiredReplayEqual(stored.DesiredRender, desired) || stored.OperatorConfigDigest != s.operatorConfigDigest {
			return RenderCommand{}, false, ErrConflict
		}
		if err = tx.Commit(ctx); err != nil {
			return RenderCommand{}, false, err
		}
		return stored, true, nil
	}
	if !errors.Is(replayErr, pgx.ErrNoRows) {
		return RenderCommand{}, false, classifyPostgres(replayErr)
	}
	_, err = tx.Exec(ctx, `INSERT INTO helm_render_commands(
		id,idempotency_scope,idempotency_key,approval_id,approval_revision,project_id,
		environment_id,application_id,namespace,release_name,descriptor_yaml,values_yaml,
		descriptor_digest,values_digest,input_digest,operator_config_digest,state,available_at,created_at,updated_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,'queued',$17,$17,$17)`,
		desired.ID, desired.IdempotencyScope, desired.IdempotencyKey, desired.Approval.ID,
		desired.Approval.Revision, desired.Descriptor.Destination.ProjectID,
		desired.Descriptor.Destination.EnvironmentID, desired.Descriptor.Destination.ApplicationID,
		desired.Descriptor.Destination.Namespace, desired.Descriptor.ReleaseName,
		desired.DescriptorYAML, desired.ValuesYAML, desired.DescriptorDigest,
		desired.ValuesDigest, desired.InputDigest, s.operatorConfigDigest, now)
	if err != nil {
		return RenderCommand{}, false, classifyPostgres(err)
	}
	command, err := scanCommand(tx.QueryRow(ctx, commandSelect+` WHERE c.id=$1`, desired.ID))
	if err != nil {
		return RenderCommand{}, false, classifyPostgres(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return RenderCommand{}, false, classifyPostgres(err)
	}
	return command, false, nil
}

func (s *PostgresStore) Command(ctx context.Context, id string) (RenderCommand, error) {
	if !uuidRE.MatchString(id) {
		return RenderCommand{}, ErrInvalid
	}
	command, err := scanCommand(s.pool.QueryRow(ctx, commandSelect+` WHERE c.id=$1`, id))
	return command, classifyPostgres(err)
}

func (s *PostgresStore) Result(ctx context.Context, commandID string) (RenderResult, error) {
	if !uuidRE.MatchString(commandID) {
		return RenderResult{}, ErrInvalid
	}
	result, err := scanResult(s.pool.QueryRow(ctx, resultSelect+` WHERE command_id=$1`, commandID))
	if err != nil {
		return RenderResult{}, classifyPostgres(err)
	}
	command, err := s.Command(ctx, commandID)
	if err != nil {
		return RenderResult{}, err
	}
	validated, validateErr := ValidateRenderedManifests(result.RenderedManifests, command.Descriptor)
	if command.State != StateSucceeded || result.Validate(command) != nil || validateErr != nil ||
		result.InventoryDigest != validated.InventoryDigest || result.ResourceCount != validated.ResourceCount {
		return RenderResult{}, ErrConflict
	}
	return result, nil
}

func (s *PostgresStore) Claim(ctx context.Context, owner string, runtime RenderWorkerIdentity, now time.Time, duration time.Duration) (RenderLease, error) {
	if !validLeaseRequest(owner, runtime, now, duration) || runtime.OperatorConfigDigest != s.operatorConfigDigest {
		return RenderLease{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RenderLease{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	_, err = tx.Exec(ctx, `UPDATE helm_render_commands SET
		state='failed',consecutive_failures=LEAST(consecutive_failures+1,10),
		last_failure_code='renderer-lease-expired',lease_owner=NULL,lease_until=NULL,
		worker_contract=NULL,worker_renderer_image=NULL,worker_renderer_version=NULL,
		worker_policy_version=NULL,worker_limits_digest=NULL,worker_operator_config_digest=NULL,
		completed_at=$1,updated_at=$1
		WHERE state='processing' AND lease_until<=$1 AND attempts>=10 AND operator_config_digest=$2`,
		now, runtime.OperatorConfigDigest)
	if err != nil {
		return RenderLease{}, classifyPostgres(err)
	}
	var id string
	err = tx.QueryRow(ctx, `WITH candidate AS (
		SELECT id,state FROM helm_render_commands
		WHERE operator_config_digest=$9 AND ((state='queued' AND available_at<=$1) OR
		      (state='processing' AND lease_until<=$1))
		ORDER BY available_at,created_at,id FOR UPDATE SKIP LOCKED LIMIT 1
	) UPDATE helm_render_commands AS command SET
		state='processing',attempts=command.attempts+1,
		consecutive_failures=CASE WHEN candidate.state='processing'
			THEN LEAST(command.consecutive_failures+1,10) ELSE command.consecutive_failures END,
		last_failure_code=CASE WHEN candidate.state='processing'
			THEN 'renderer-lease-expired' ELSE command.last_failure_code END,
		lease_owner=$2,lease_epoch=command.lease_epoch+1,lease_until=$3,
		worker_contract=$4,worker_renderer_image=$5,worker_renderer_version=$6,
		worker_policy_version=$7,worker_limits_digest=$8,worker_operator_config_digest=$9,updated_at=$1
		FROM candidate WHERE command.id=candidate.id RETURNING command.id::text`,
		now, owner, now.Add(duration), runtime.Contract, runtime.RendererImage,
		runtime.RendererVersion, runtime.PolicyVersion, runtime.LimitsDigest,
		runtime.OperatorConfigDigest).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return RenderLease{}, ErrNotFound
	}
	if err != nil {
		return RenderLease{}, classifyPostgres(err)
	}
	command, err := scanCommand(tx.QueryRow(ctx, commandSelect+` WHERE c.id=$1`, id))
	if err != nil {
		return RenderLease{}, classifyPostgres(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return RenderLease{}, classifyPostgres(err)
	}
	return RenderLease{Command: command, Owner: owner, Epoch: command.LeaseEpoch, Until: *command.LeaseUntil}, nil
}

func (s *PostgresStore) Heartbeat(ctx context.Context, lease RenderLease, now time.Time, duration time.Duration) (RenderLease, error) {
	if !validLeaseRequest(lease.Owner, lease.Command.WorkerIdentity, now, duration) || lease.Epoch <= 0 {
		return RenderLease{}, ErrInvalid
	}
	var id string
	err := s.pool.QueryRow(ctx, `UPDATE helm_render_commands SET lease_until=$4,updated_at=$3
		WHERE id=$1 AND state='processing' AND lease_owner=$2 AND lease_epoch=$5 AND
		lease_until>$3 AND worker_contract=$6 AND worker_renderer_image=$7 AND
		worker_renderer_version=$8 AND worker_policy_version=$9 AND worker_limits_digest=$10 AND
		operator_config_digest=$11 AND worker_operator_config_digest=$11
		RETURNING id::text`, lease.Command.ID, lease.Owner, now, now.Add(duration), lease.Epoch,
		lease.Command.WorkerIdentity.Contract, lease.Command.WorkerIdentity.RendererImage,
		lease.Command.WorkerIdentity.RendererVersion, lease.Command.WorkerIdentity.PolicyVersion,
		lease.Command.WorkerIdentity.LimitsDigest,
		lease.Command.WorkerIdentity.OperatorConfigDigest).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return RenderLease{}, ErrLeaseLost
	}
	if err != nil {
		return RenderLease{}, classifyPostgres(err)
	}
	command, err := s.Command(ctx, id)
	if err != nil {
		return RenderLease{}, err
	}
	if command.State != StateProcessing || command.LeaseUntil == nil || command.LeaseOwner != lease.Owner || command.LeaseEpoch != lease.Epoch {
		return RenderLease{}, ErrLeaseLost
	}
	return RenderLease{Command: command, Owner: lease.Owner, Epoch: lease.Epoch, Until: *command.LeaseUntil}, nil
}

func (s *PostgresStore) Complete(ctx context.Context, lease RenderLease, manifests ValidatedManifests, now time.Time) (RenderResult, error) {
	if now.IsZero() {
		return RenderResult{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RenderResult{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	command, err := scanCommand(tx.QueryRow(ctx, commandSelect+` WHERE c.id=$1 FOR UPDATE`, lease.Command.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return RenderResult{}, ErrNotFound
	}
	if err != nil {
		return RenderResult{}, classifyPostgres(err)
	}
	if !leaseMatches(command, lease, now) {
		return RenderResult{}, ErrLeaseLost
	}
	verified, verifyErr := ValidateRenderedManifests(manifests.Raw, command.Descriptor)
	if verifyErr != nil || !validatedReplayEqual(verified, manifests) {
		return RenderResult{}, ErrInvalid
	}
	result := RenderResult{CommandID: command.ID, InputDigest: command.InputDigest,
		OperatorConfigDigest: command.OperatorConfigDigest,
		ManifestDigest:       manifests.ManifestDigest, InventoryDigest: manifests.InventoryDigest,
		RenderedManifests: append([]byte(nil), manifests.Raw...), ResourceCount: manifests.ResourceCount,
		OutputBytes: len(manifests.Raw), RendererImage: RendererImage, RendererVersion: HelmVersion,
		PolicyVersion: PolicyVersion, LimitsDigest: LimitsDigest(), CompletedAt: now}
	if result.Validate(command) != nil {
		return RenderResult{}, ErrInvalid
	}
	_, err = tx.Exec(ctx, `INSERT INTO helm_render_results(
		command_id,input_digest,operator_config_digest,manifest_digest,inventory_digest,rendered_manifests,
		resource_count,output_bytes,renderer_image,renderer_version,policy_version,limits_digest,completed_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, result.CommandID, result.InputDigest,
		result.OperatorConfigDigest,
		result.ManifestDigest, result.InventoryDigest, result.RenderedManifests, result.ResourceCount,
		result.OutputBytes, result.RendererImage, result.RendererVersion, result.PolicyVersion,
		result.LimitsDigest, result.CompletedAt)
	if err != nil {
		return RenderResult{}, classifyPostgres(err)
	}
	commandTag, err := tx.Exec(ctx, `UPDATE helm_render_commands SET state='succeeded',lease_owner=NULL,
		lease_until=NULL,worker_contract=NULL,worker_renderer_image=NULL,worker_renderer_version=NULL,
		worker_policy_version=NULL,worker_limits_digest=NULL,worker_operator_config_digest=NULL,
		completed_at=$3,updated_at=$3
		WHERE id=$1 AND lease_owner=$2 AND lease_epoch=$4 AND state='processing' AND lease_until>$3 AND
		operator_config_digest=$5 AND worker_operator_config_digest=$5`,
		command.ID, lease.Owner, now, lease.Epoch, lease.Command.WorkerIdentity.OperatorConfigDigest)
	if err != nil {
		return RenderResult{}, classifyPostgres(err)
	}
	if commandTag.RowsAffected() != 1 {
		return RenderResult{}, ErrLeaseLost
	}
	if err = tx.Commit(ctx); err != nil {
		return RenderResult{}, classifyPostgres(err)
	}
	return result, nil
}

func (s *PostgresStore) Fail(ctx context.Context, lease RenderLease, code string, retryable bool, now time.Time) (RenderCommand, error) {
	if now.IsZero() || !failureCodeRE.MatchString(code) {
		return RenderCommand{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RenderCommand{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	command, err := scanCommand(tx.QueryRow(ctx, commandSelect+` WHERE c.id=$1 FOR UPDATE`, lease.Command.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return RenderCommand{}, ErrNotFound
	}
	if err != nil {
		return RenderCommand{}, classifyPostgres(err)
	}
	if !leaseMatches(command, lease, now) {
		return RenderCommand{}, ErrLeaseLost
	}
	state := StateFailed
	var completedAt *time.Time
	availableAt := now
	if retryable && command.Attempts < MaximumAttempts {
		state = StateQueued
		availableAt = now.Add(RetryDelay(command.Attempts))
	} else {
		completedAt = &now
	}
	var id string
	err = tx.QueryRow(ctx, `UPDATE helm_render_commands SET state=$6,available_at=$7,
		consecutive_failures=consecutive_failures+1,last_failure_code=$5,
		lease_owner=NULL,lease_until=NULL,worker_contract=NULL,worker_renderer_image=NULL,
		worker_renderer_version=NULL,worker_policy_version=NULL,worker_limits_digest=NULL,
		worker_operator_config_digest=NULL,
		completed_at=$8,updated_at=$3
		WHERE id=$1 AND state='processing' AND lease_owner=$2 AND lease_epoch=$4 AND lease_until>$3 AND
		operator_config_digest=$9 AND worker_operator_config_digest=$9
		RETURNING id::text`, lease.Command.ID, lease.Owner, now, lease.Epoch, code, state,
		availableAt, completedAt, lease.Command.WorkerIdentity.OperatorConfigDigest).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return RenderCommand{}, ErrLeaseLost
	}
	if err != nil {
		return RenderCommand{}, classifyPostgres(err)
	}
	command, err = scanCommand(tx.QueryRow(ctx, commandSelect+` WHERE c.id=$1`, id))
	if err != nil {
		return RenderCommand{}, classifyPostgres(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return RenderCommand{}, classifyPostgres(err)
	}
	return command, nil
}

func (s *PostgresStore) PutReadiness(ctx context.Context, readiness Readiness) error {
	if readiness.Validate() != nil || readiness.RenderWorkerIdentity != ExpectedRenderWorkerIdentity(s.operatorConfigDigest) {
		return ErrInvalid
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO helm_renderer_readiness(
		worker_id,worker_epoch,contract_version,renderer_image,renderer_version,
		policy_version,limits_digest,operator_config_digest,started_at,observed_at,lease_until
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
	ON CONFLICT(worker_id) DO UPDATE SET worker_epoch=EXCLUDED.worker_epoch,
		contract_version=EXCLUDED.contract_version,renderer_image=EXCLUDED.renderer_image,
		renderer_version=EXCLUDED.renderer_version,policy_version=EXCLUDED.policy_version,
		limits_digest=EXCLUDED.limits_digest,operator_config_digest=EXCLUDED.operator_config_digest,started_at=EXCLUDED.started_at,
		observed_at=EXCLUDED.observed_at,lease_until=EXCLUDED.lease_until`,
		readiness.WorkerID, readiness.WorkerEpoch, readiness.Contract, readiness.RendererImage,
		readiness.RendererVersion, readiness.PolicyVersion, readiness.LimitsDigest,
		readiness.OperatorConfigDigest,
		readiness.StartedAt, readiness.ObservedAt, readiness.LeaseUntil)
	return classifyPostgres(err)
}

func (s *PostgresStore) RuntimeReady(ctx context.Context, now time.Time) (bool, error) {
	if now.IsZero() {
		return false, ErrInvalid
	}
	runtime := ExpectedRenderWorkerIdentity(s.operatorConfigDigest)
	var ready bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM helm_renderer_readiness
		WHERE contract_version=$1 AND renderer_image=$2 AND renderer_version=$3 AND
		policy_version=$4 AND limits_digest=$5 AND operator_config_digest=$6 AND lease_until>$7)`, runtime.Contract,
		runtime.RendererImage, runtime.RendererVersion, runtime.PolicyVersion, runtime.LimitsDigest,
		runtime.OperatorConfigDigest, now).Scan(&ready)
	return ready, classifyPostgres(err)
}

const approvalSelect = `SELECT approval_id::text,revision,oci_repository,chart_version,
	manifest_digest,package_digest,values_schema_digest,renderer_image,renderer_version,
	policy_version,created_by::text,idempotency_key,created_at,identity_digest FROM helm_chart_approvals`

const commandSelect = `SELECT c.id::text,c.idempotency_scope::text,c.idempotency_key,
	c.approval_id::text,c.approval_revision,c.project_id::text,c.environment_id::text,
	c.application_id::text,c.namespace,c.release_name,c.descriptor_yaml,c.values_yaml,
	c.descriptor_digest,c.values_digest,c.input_digest,c.operator_config_digest,c.state,c.available_at,c.attempts,
	c.consecutive_failures,c.last_failure_code,COALESCE(c.lease_owner,''),c.lease_epoch,
	c.lease_until,COALESCE(c.worker_contract,''),COALESCE(c.worker_renderer_image,''),
	COALESCE(c.worker_renderer_version,''),COALESCE(c.worker_policy_version,''),
	COALESCE(c.worker_limits_digest,''),COALESCE(c.worker_operator_config_digest,''),c.created_at,c.updated_at,c.completed_at,
	a.oci_repository,a.chart_version,a.manifest_digest,a.package_digest,a.values_schema_digest,
	a.renderer_image,a.renderer_version,a.policy_version
	FROM helm_render_commands c JOIN helm_chart_approvals a
	ON a.approval_id=c.approval_id AND a.revision=c.approval_revision`

const resultSelect = `SELECT command_id::text,input_digest,operator_config_digest,manifest_digest,inventory_digest,
	rendered_manifests,resource_count,output_bytes,renderer_image,renderer_version,
	policy_version,limits_digest,completed_at FROM helm_render_results`

type rowScanner interface{ Scan(...any) error }

func scanApproval(row rowScanner) (Approval, error) {
	var approval Approval
	var identityDigest string
	err := row.Scan(&approval.ID, &approval.Revision, &approval.OCIRepository, &approval.ChartVersion,
		&approval.ManifestDigest, &approval.PackageDigest, &approval.ValuesSchemaDigest,
		&approval.RendererImage, &approval.RendererVersion, &approval.PolicyVersion,
		&approval.CreatedBy, &approval.IdempotencyKey, &approval.CreatedAt, &identityDigest)
	if err == nil {
		expected, identityErr := approval.IdentityDigest()
		if identityErr != nil || identityDigest != expected {
			return Approval{}, ErrConflict
		}
	}
	return approval, err
}

func scanCommand(row rowScanner) (RenderCommand, error) {
	var command RenderCommand
	var repository, version, manifestDigest, packageDigest, schemaDigest string
	err := row.Scan(&command.ID, &command.IdempotencyScope, &command.IdempotencyKey,
		&command.Approval.ID, &command.Approval.Revision,
		&command.Descriptor.Destination.ProjectID, &command.Descriptor.Destination.EnvironmentID,
		&command.Descriptor.Destination.ApplicationID, &command.Descriptor.Destination.Namespace,
		&command.Descriptor.ReleaseName, &command.DescriptorYAML, &command.ValuesYAML,
		&command.DescriptorDigest, &command.ValuesDigest, &command.InputDigest,
		&command.OperatorConfigDigest, &command.State,
		&command.AvailableAt, &command.Attempts, &command.ConsecutiveFailures,
		&command.LastFailureCode, &command.LeaseOwner, &command.LeaseEpoch, &command.LeaseUntil,
		&command.WorkerIdentity.Contract, &command.WorkerIdentity.RendererImage,
		&command.WorkerIdentity.RendererVersion, &command.WorkerIdentity.PolicyVersion,
		&command.WorkerIdentity.LimitsDigest, &command.WorkerIdentity.OperatorConfigDigest,
		&command.CreatedAt, &command.UpdatedAt,
		&command.CompletedAt, &repository, &version, &manifestDigest, &packageDigest,
		&schemaDigest, &command.Descriptor.RendererImage, &command.Descriptor.RendererVersion,
		&command.Descriptor.PolicyVersion)
	if err != nil {
		return RenderCommand{}, err
	}
	command.Descriptor.Approval = command.Approval
	command.Descriptor.Repository, command.Descriptor.Version = repository, version
	command.Descriptor.ManifestDigest, command.Descriptor.PackageDigest = manifestDigest, packageDigest
	command.Descriptor.ValuesSchemaDigest = schemaDigest
	command.Descriptor.Destination.ApplicationSlug = command.Descriptor.ReleaseName
	if command.Validate() != nil {
		return RenderCommand{}, ErrConflict
	}
	return command, nil
}

func scanResult(row rowScanner) (RenderResult, error) {
	var result RenderResult
	err := row.Scan(&result.CommandID, &result.InputDigest, &result.OperatorConfigDigest, &result.ManifestDigest,
		&result.InventoryDigest, &result.RenderedManifests, &result.ResourceCount,
		&result.OutputBytes, &result.RendererImage, &result.RendererVersion,
		&result.PolicyVersion, &result.LimitsDigest, &result.CompletedAt)
	return result, err
}

func validatedReplayEqual(left, right ValidatedManifests) bool {
	return left.ManifestDigest == right.ManifestDigest && left.InventoryDigest == right.InventoryDigest &&
		left.ResourceCount == right.ResourceCount && equalBytes(left.Raw, right.Raw)
}

func classifyPostgres(err error) error {
	if err == nil || errors.Is(err, ErrInvalid) || errors.Is(err, ErrConflict) || errors.Is(err, ErrLeaseLost) {
		return err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "23503" || pgErr.Code == "23505" || pgErr.Code == "23514" || pgErr.Code == "40001") {
		return fmt.Errorf("%w: %s", ErrConflict, pgErr.ConstraintName)
	}
	return err
}
