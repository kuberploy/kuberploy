package builds

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *PostgreSQLStore) SourceDeploymentReceipt(ctx context.Context, actorID, deploymentID, key string) (SourceDeploymentAcceptance, error) {
	if s == nil || s.pool == nil || !uuidRE.MatchString(actorID) || !uuidRE.MatchString(deploymentID) || !setupIdempotencyRE.MatchString(key) {
		return SourceDeploymentAcceptance{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return SourceDeploymentAcceptance{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var attemptID string
	err = tx.QueryRow(ctx, `SELECT resource_id::text FROM mutation_receipts
		WHERE actor_id=$1 AND receipt_kind='build-api' AND namespace=$2 AND scope_key=$3::text AND idempotency_key=$4`,
		actorID, APICommandSourceDeployment, deploymentID, key).Scan(&attemptID)
	if err != nil {
		return SourceDeploymentAcceptance{}, classifyPostgres(err)
	}
	attempt, err := attemptByIDQuery(ctx, tx, attemptID, false)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			err = ErrConflict
		}
		return SourceDeploymentAcceptance{}, err
	}
	intent, err := sourceDeploymentIntentByAttemptQuery(ctx, tx, attemptID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			err = ErrConflict
		}
		return SourceDeploymentAcceptance{}, err
	}
	if intent.ActorID != actorID || intent.DeploymentID != deploymentID ||
		intent.AttemptID != attempt.ID || intent.ProjectID != attempt.ProjectID || intent.ApplicationID != attempt.ServiceID {
		return SourceDeploymentAcceptance{}, ErrConflict
	}
	if err = tx.Commit(ctx); err != nil {
		return SourceDeploymentAcceptance{}, classifyPostgres(err)
	}
	return SourceDeploymentAcceptance{Attempt: attempt, Intent: intent, Replay: true}, nil
}

func (s *PostgreSQLStore) AcceptSourceDeployment(ctx context.Context, command SourceDeploymentCommand) (SourceDeploymentAcceptance, error) {
	if s == nil || s.pool == nil || command.validate() != nil {
		return SourceDeploymentAcceptance{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return SourceDeploymentAcceptance{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	lockKey := "source-deployment|" + command.ActorID + "|" + command.DeploymentID + "|" + command.IdempotencyKey
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return SourceDeploymentAcceptance{}, err
	}
	var storedFingerprint, storedAttemptID string
	err = tx.QueryRow(ctx, `SELECT request_digest,resource_id::text FROM mutation_receipts
		WHERE actor_id=$1 AND receipt_kind='build-api' AND namespace=$2 AND scope_key=$3::text AND idempotency_key=$4 FOR UPDATE`,
		command.ActorID, APICommandSourceDeployment, command.DeploymentID, command.IdempotencyKey).Scan(&storedFingerprint, &storedAttemptID)
	if err == nil {
		if storedFingerprint != command.Fingerprint {
			return SourceDeploymentAcceptance{}, ErrConflict
		}
		attempt, getErr := attemptByIDQuery(ctx, tx, storedAttemptID, false)
		if getErr != nil {
			return SourceDeploymentAcceptance{}, getErr
		}
		intent, getErr := sourceDeploymentIntentByAttemptQuery(ctx, tx, storedAttemptID)
		if getErr != nil || intent.DeploymentID != command.DeploymentID {
			if getErr == nil {
				getErr = ErrConflict
			}
			return SourceDeploymentAcceptance{}, getErr
		}
		if err = tx.Commit(ctx); err != nil {
			return SourceDeploymentAcceptance{}, classifyPostgres(err)
		}
		return SourceDeploymentAcceptance{Attempt: attempt, Intent: intent, Replay: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return SourceDeploymentAcceptance{}, classifyPostgres(err)
	}

	var projectID, applicationID, environmentID, configETag, state string
	var deploymentGeneration int64
	err = tx.QueryRow(ctx, `SELECT e.project_id::text,d.application_id::text,d.environment_id::text,d.generation,d.config_etag,d.state
		FROM deployments d JOIN environments e ON e.id=d.environment_id WHERE d.id=$1 FOR UPDATE OF d`, command.DeploymentID).
		Scan(&projectID, &applicationID, &environmentID, &deploymentGeneration, &configETag, &state)
	if err != nil {
		return SourceDeploymentAcceptance{}, classifyPostgres(err)
	}
	if projectID != command.ProjectID || applicationID != command.ApplicationID || environmentID != command.EnvironmentID ||
		deploymentGeneration != command.SourceDeploymentGeneration || configETag != command.SourceProjectionETag ||
		(command.StartDraft != (state == "stopped")) {
		return SourceDeploymentAcceptance{}, ErrConflict
	}

	definition, repository, err := sourceDeploymentDefinitionQuery(ctx, tx, command)
	if err != nil {
		return SourceDeploymentAcceptance{}, err
	}
	claimKey := APICommandClaimKey(command.ActorID, APICommandSourceDeployment, command.DeploymentID, command.IdempotencyKey)
	attemptID := ManualAttemptID(claimKey, definition.ID)
	var generation int64
	if err = tx.QueryRow(ctx, `UPDATE applications SET build_generation=build_generation+1 WHERE project_id=$1 AND id=$2 RETURNING build_generation`,
		definition.ProjectID, definition.ServiceID).Scan(&generation); err != nil {
		return SourceDeploymentAcceptance{}, classifyPostgres(err)
	}
	imports, err := cacheImportsQuery(ctx, tx, definition, generation, command.AcceptedAt)
	if err != nil {
		return SourceDeploymentAcceptance{}, err
	}
	attempt, err := newAttemptWithExecution(definition, command.Execution, repository, EnqueuePush{
		ClaimKey: claimKey, CommitSHA: command.CommitSHA, GitRef: definition.TriggerRef, ResolvedAt: command.AcceptedAt.UTC(),
	}, generation, imports, command.AcceptedAt)
	if err != nil || attempt.ID != attemptID {
		return SourceDeploymentAcceptance{}, ErrInvalid
	}
	attempt.DeliveryClaimKey, attempt.TriggerKey = "", claimKey
	if command.Mode == SourceDeploymentRebuild {
		attempt.TriggerKind = "retry"
	} else {
		attempt.TriggerKind = "manual"
	}
	if validateStoredAttempt(attempt) != nil {
		return SourceDeploymentAcceptance{}, ErrInvalid
	}
	if err = insertSourceDeploymentAttempt(ctx, tx, attempt); err != nil {
		return SourceDeploymentAcceptance{}, err
	}

	now := command.AcceptedAt.UTC()
	var sequence int64
	if err = tx.QueryRow(ctx, `SELECT COALESCE(max(sequence),0)+1 FROM source_deployment_intents WHERE deployment_id=$1`, command.DeploymentID).Scan(&sequence); err != nil {
		return SourceDeploymentAcceptance{}, classifyPostgres(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE source_deployment_intents SET state='superseded',failure_code='newer-source-deploy',
		lease_owner=NULL,lease_until=NULL,completed_at=$2,updated_at=$2
		WHERE deployment_id=$1 AND state IN ('pending','processing')`, command.DeploymentID, now); err != nil {
		return SourceDeploymentAcceptance{}, classifyPostgres(err)
	}
	intent := SourceDeploymentIntent{ID: SourceDeploymentIntentID(attempt.ID, command.DeploymentID), AttemptID: attempt.ID,
		ActorID: command.ActorID, ProjectID: command.ProjectID, ApplicationID: command.ApplicationID,
		EnvironmentID: command.EnvironmentID, DeploymentID: command.DeploymentID, Mode: command.Mode,
		SourceAttemptID: command.SourceAttemptID, Sequence: sequence, DefinitionID: definition.ID,
		DefinitionDigest: definition.DefinitionDigest, SourceDeploymentGeneration: command.SourceDeploymentGeneration,
		SourceConfigETag: command.SourceConfigETag, ConfigIntent: append([]byte{}, command.ConfigIntent...),
		TemplateDigest: command.TemplateDigest, RequestID: command.RequestID, State: SourceDeploymentPending,
		AvailableAt: now, CreatedAt: now, UpdatedAt: now, StartDraft: command.StartDraft}
	if intent.validate() != nil {
		return SourceDeploymentAcceptance{}, ErrInvalid
	}
	var sourceAttempt any
	if intent.SourceAttemptID != "" {
		sourceAttempt = intent.SourceAttemptID
	}
	_, err = tx.Exec(ctx, `INSERT INTO source_deployment_intents(id,attempt_id,actor_id,project_id,application_id,environment_id,deployment_id,
		mode,source_attempt_id,sequence,definition_id,definition_digest,source_deployment_generation,source_config_etag,config_intent,
		template_digest,request_id,start_draft,state,attempts,available_at,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,'pending',0,$19,$19,$19)`,
		intent.ID, intent.AttemptID, intent.ActorID, intent.ProjectID, intent.ApplicationID, intent.EnvironmentID, intent.DeploymentID,
		intent.Mode, sourceAttempt, intent.Sequence, intent.DefinitionID, intent.DefinitionDigest, intent.SourceDeploymentGeneration,
		intent.SourceConfigETag, intent.ConfigIntent, intent.TemplateDigest, intent.RequestID, intent.StartDraft, now)
	if err != nil {
		return SourceDeploymentAcceptance{}, classifyPostgres(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO mutation_receipts(actor_id,receipt_kind,namespace,scope_key,idempotency_key,request_digest,resource_id,created_at)
		VALUES($1,'build-api',$2,$3::text,$4,$5,$6,$7)`, command.ActorID, APICommandSourceDeployment,
		command.DeploymentID, command.IdempotencyKey, command.Fingerprint, attempt.ID, now)
	if err != nil {
		return SourceDeploymentAcceptance{}, classifyPostgres(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return SourceDeploymentAcceptance{}, classifyPostgres(err)
	}
	return SourceDeploymentAcceptance{Attempt: attempt, Intent: intent}, nil
}

func sourceDeploymentDefinitionQuery(ctx context.Context, tx pgx.Tx, command SourceDeploymentCommand) (BuildDefinition, Repository, error) {
	definition, err := definitionByIDQuery(ctx, tx, command.DefinitionID, command.Mode == SourceDeploymentDeploy)
	if command.Mode == SourceDeploymentRebuild {
		source, sourceErr := attemptByIDQuery(ctx, tx, command.SourceAttemptID, true)
		if sourceErr != nil {
			return BuildDefinition{}, Repository{}, sourceErr
		}
		if source.State != AttemptSucceeded || source.ProjectID != command.ProjectID || source.ServiceID != command.ApplicationID ||
			source.DefinitionID != command.DefinitionID || source.DefinitionDigest != command.ExpectedDefinitionDigest || source.CommitSHA != command.CommitSHA {
			return BuildDefinition{}, Repository{}, ErrConflict
		}
		definition, err = source.SourceSnapshot, nil
	}
	if err != nil {
		return BuildDefinition{}, Repository{}, err
	}
	if !definition.Enabled || definition.validate() != nil || definition.ProjectID != command.ProjectID || definition.ServiceID != command.ApplicationID ||
		definition.DefinitionDigest != command.ExpectedDefinitionDigest {
		return BuildDefinition{}, Repository{}, ErrUnauthorized
	}
	var repository Repository
	switch definition.SourceKind {
	case SourceGitHub:
		installation, installationErr := installationByIDQuery(ctx, tx, definition.InstallationID)
		if installationErr != nil {
			return BuildDefinition{}, Repository{}, installationErr
		}
		repository, err = repositoryByIDQuery(ctx, tx, definition.RepositoryID)
		if err != nil {
			return BuildDefinition{}, Repository{}, err
		}
		if installation.Lifecycle != InstallationActive || repository.Lifecycle != RepositoryActive || repository.InstallationID != installation.ID ||
			repository.Identity.OwnerID != installation.Account.ID || !strings.EqualFold(repository.Identity.OwnerLogin, installation.Account.Login) {
			return BuildDefinition{}, Repository{}, ErrUnauthorized
		}
	case SourceGitSSH:
		if definition.GitSSH == nil {
			return BuildDefinition{}, Repository{}, ErrUnauthorized
		}
		var keyStatus string
		if err = tx.QueryRow(ctx, `SELECT status FROM git_ssh_key_revisions WHERE scope=$1 AND owner_id=$2 AND revision=$3 FOR SHARE`,
			definition.GitSSH.KeyScope, definition.GitSSH.KeyOwnerID, definition.GitSSH.KeyRevision).Scan(&keyStatus); err != nil {
			return BuildDefinition{}, Repository{}, classifyPostgres(err)
		}
		if keyStatus != "active" {
			return BuildDefinition{}, Repository{}, ErrGitSSHKeyInactive
		}
	default:
		return BuildDefinition{}, Repository{}, ErrUnauthorized
	}
	return definition, repository, nil
}

func insertSourceDeploymentAttempt(ctx context.Context, tx pgx.Tx, attempt BuildAttempt) error {
	planJSON, err := json.Marshal(attempt.PlanRequest)
	if err != nil {
		return ErrInvalid
	}
	checkoutJSON, err := json.Marshal(attempt.CheckoutRequest)
	if err != nil {
		return ErrInvalid
	}
	sourceJSON, err := json.Marshal(attempt.SourceSnapshot)
	if err != nil {
		return ErrInvalid
	}
	_, err = tx.Exec(ctx, `INSERT INTO build_attempts(id,definition_id,delivery_claim_key,trigger_kind,trigger_key,project_id,service_id,
		commit_sha,git_ref,generation,definition_digest,source_snapshot,plan_request,checkout_request,input_digest,registry_mode,state,
		execution_attempts,max_attempts,available_at,job_namespace,job_name,cache_candidate,created_at,updated_at)
		VALUES($1,$2,NULL,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,'queued',0,$16,$17,$18,$19,$20,$21,$21)`,
		attempt.ID, attempt.DefinitionID, attempt.TriggerKind, attempt.TriggerKey, attempt.ProjectID, attempt.ServiceID,
		attempt.CommitSHA, attempt.GitRef, attempt.Generation, attempt.DefinitionDigest, sourceJSON, planJSON, checkoutJSON,
		attempt.InputDigest, attempt.RegistryMode, attempt.MaxAttempts, attempt.AvailableAt, attempt.JobNamespace, attempt.JobName,
		attempt.CacheCandidate, attempt.CreatedAt)
	return classifyPostgres(err)
}

func (s *PostgreSQLStore) ClaimNextSourceDeployment(ctx context.Context, owner string, now time.Time, duration time.Duration) (SourceDeploymentWork, error) {
	if s == nil || s.pool == nil || !validOwnerLease(owner, duration) || now.IsZero() {
		return SourceDeploymentWork{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return SourceDeploymentWork{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	current := now.UTC()
	if _, err = tx.Exec(ctx, `UPDATE source_deployment_intents i SET state='superseded',failure_code='newer-source-deploy',
		lease_owner=NULL,lease_until=NULL,completed_at=$1,updated_at=$1
		WHERE state IN ('pending','processing') AND EXISTS(
			SELECT 1 FROM source_deployment_intents newer WHERE newer.deployment_id=i.deployment_id AND newer.sequence>i.sequence)`, current); err != nil {
		return SourceDeploymentWork{}, classifyPostgres(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE source_deployment_intents i SET state='failed',failure_code='source-build-failed',
		lease_owner=NULL,lease_until=NULL,completed_at=$1,updated_at=$1
		FROM build_attempts a LEFT JOIN build_release_projections p ON p.attempt_id=a.id
		WHERE i.attempt_id=a.id AND i.state IN ('pending','processing') AND
			(a.state IN ('failed','cancelled') OR p.state='failed' OR i.attempts>=20)`, current); err != nil {
		return SourceDeploymentWork{}, classifyPostgres(err)
	}
	var intentID string
	err = tx.QueryRow(ctx, `SELECT i.id::text FROM source_deployment_intents i
		JOIN build_attempts a ON a.id=i.attempt_id AND a.state='succeeded'
		JOIN build_release_projections p ON p.attempt_id=i.attempt_id AND p.state='succeeded' AND p.release_id=i.attempt_id
		WHERE i.attempts<20 AND i.available_at<=$1 AND (i.state='pending' OR (i.state='processing' AND i.lease_until<=$1))
			AND NOT EXISTS(SELECT 1 FROM source_deployment_intents newer WHERE newer.deployment_id=i.deployment_id AND newer.sequence>i.sequence)
		ORDER BY i.available_at,i.created_at,i.id FOR UPDATE OF i SKIP LOCKED LIMIT 1`, current).Scan(&intentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return SourceDeploymentWork{}, ErrNotFound
	}
	if err != nil {
		return SourceDeploymentWork{}, classifyPostgres(err)
	}
	lease := SourceDeploymentLeaseToken{IntentID: intentID, Owner: owner, Until: current.Add(duration)}
	err = tx.QueryRow(ctx, `UPDATE source_deployment_intents SET state='processing',attempts=attempts+1,lease_owner=$2,
		lease_until=$3,lease_epoch=lease_epoch+1,failure_code='',updated_at=$4 WHERE id=$1 RETURNING lease_epoch`,
		intentID, owner, lease.Until, current).Scan(&lease.Epoch)
	if err != nil {
		return SourceDeploymentWork{}, classifyPostgres(err)
	}
	intent, err := sourceDeploymentIntentByIDQuery(ctx, tx, intentID)
	if err != nil {
		return SourceDeploymentWork{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return SourceDeploymentWork{}, classifyPostgres(err)
	}
	return SourceDeploymentWork{Intent: intent, Lease: lease}, nil
}

func (s *PostgreSQLStore) HeartbeatSourceDeployment(ctx context.Context, lease SourceDeploymentLeaseToken, now time.Time, duration time.Duration) (SourceDeploymentLeaseToken, error) {
	if !validSourceDeploymentLease(lease) || !validOwnerLease(lease.Owner, duration) || now.IsZero() {
		return SourceDeploymentLeaseToken{}, ErrInvalid
	}
	current, until := now.UTC(), now.UTC().Add(duration)
	result, err := s.pool.Exec(ctx, `UPDATE source_deployment_intents i SET lease_until=$5,updated_at=$4
		WHERE i.id=$1 AND i.state='processing' AND i.lease_owner=$2 AND i.lease_epoch=$3 AND i.lease_until>$4
			AND NOT EXISTS(SELECT 1 FROM source_deployment_intents newer WHERE newer.deployment_id=i.deployment_id AND newer.sequence>i.sequence)`,
		lease.IntentID, lease.Owner, lease.Epoch, current, until)
	if err != nil {
		return SourceDeploymentLeaseToken{}, classifyPostgres(err)
	}
	if result.RowsAffected() != 1 {
		return SourceDeploymentLeaseToken{}, ErrLeaseLost
	}
	lease.Until = until
	return lease, nil
}

func (s *PostgreSQLStore) RetrySourceDeployment(ctx context.Context, lease SourceDeploymentLeaseToken, code string, now, availableAt time.Time) error {
	if !validSourceDeploymentLease(lease) || validateFailureCode(code) != nil || now.IsZero() || availableAt.Before(now.UTC()) {
		return ErrInvalid
	}
	current := now.UTC()
	result, err := s.pool.Exec(ctx, `UPDATE source_deployment_intents i SET
		state=CASE WHEN EXISTS(SELECT 1 FROM source_deployment_intents newer WHERE newer.deployment_id=i.deployment_id AND newer.sequence>i.sequence) THEN 'superseded' WHEN attempts>=20 THEN 'failed' ELSE 'pending' END,
		failure_code=CASE WHEN EXISTS(SELECT 1 FROM source_deployment_intents newer WHERE newer.deployment_id=i.deployment_id AND newer.sequence>i.sequence) THEN 'newer-source-deploy' ELSE $5 END,
		available_at=CASE WHEN attempts>=20 THEN available_at ELSE $6 END,lease_owner=NULL,lease_until=NULL,
		completed_at=CASE WHEN attempts>=20 OR EXISTS(SELECT 1 FROM source_deployment_intents newer WHERE newer.deployment_id=i.deployment_id AND newer.sequence>i.sequence) THEN $4 ELSE NULL END,updated_at=$4
		WHERE i.id=$1 AND i.state='processing' AND i.lease_owner=$2 AND i.lease_epoch=$3 AND i.lease_until>$4`,
		lease.IntentID, lease.Owner, lease.Epoch, current, code, availableAt.UTC())
	if err != nil {
		return classifyPostgres(err)
	}
	if result.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (s *PostgreSQLStore) FailSourceDeployment(ctx context.Context, lease SourceDeploymentLeaseToken, code string, now time.Time) error {
	if !validSourceDeploymentLease(lease) || validateFailureCode(code) != nil || now.IsZero() {
		return ErrInvalid
	}
	current := now.UTC()
	result, err := s.pool.Exec(ctx, `UPDATE source_deployment_intents SET state='failed',failure_code=$5,lease_owner=NULL,
		lease_until=NULL,completed_at=$4,updated_at=$4 WHERE id=$1 AND state='processing' AND lease_owner=$2 AND lease_epoch=$3 AND lease_until>$4`,
		lease.IntentID, lease.Owner, lease.Epoch, current, code)
	if err != nil {
		return classifyPostgres(err)
	}
	if result.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (s *PostgreSQLStore) CompleteSourceDeployment(ctx context.Context, lease SourceDeploymentLeaseToken, receipt SourceDeploymentSubmissionReceipt, now time.Time) error {
	if !validSourceDeploymentLease(lease) || !uuidRE.MatchString(receipt.OperationID) || !uuidRE.MatchString(receipt.DeploymentID) || now.IsZero() {
		return ErrInvalid
	}
	current := now.UTC()
	result, err := s.pool.Exec(ctx, `UPDATE source_deployment_intents i SET state='submitted',operation_id=$5,failure_code='',
		lease_owner=NULL,lease_until=NULL,completed_at=$4,updated_at=$4
		WHERE i.id=$1 AND i.deployment_id=$6 AND i.state='processing' AND i.lease_owner=$2 AND i.lease_epoch=$3 AND i.lease_until>$4
			AND NOT EXISTS(SELECT 1 FROM source_deployment_intents newer WHERE newer.deployment_id=i.deployment_id AND newer.sequence>i.sequence)`,
		lease.IntentID, lease.Owner, lease.Epoch, current, receipt.OperationID, receipt.DeploymentID)
	if err != nil {
		return classifyPostgres(err)
	}
	if result.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}

type sourceDeploymentScanner interface {
	Scan(...any) error
}

const sourceDeploymentIntentSelect = `SELECT id::text,attempt_id::text,actor_id::text,project_id::text,application_id::text,
	environment_id::text,deployment_id::text,mode,COALESCE(source_attempt_id::text,''),sequence,definition_id::text,definition_digest,
	source_deployment_generation,source_config_etag,config_intent,template_digest,request_id,start_draft,state,attempts,available_at,
	COALESCE(lease_owner,''),lease_until,lease_epoch,COALESCE(operation_id::text,''),failure_code,
	created_at,updated_at,completed_at FROM source_deployment_intents`

func sourceDeploymentIntentByIDQuery(ctx context.Context, q rowQuery, intentID string) (SourceDeploymentIntent, error) {
	return scanSourceDeploymentIntent(q.QueryRow(ctx, sourceDeploymentIntentSelect+` WHERE id=$1`, intentID))
}

func sourceDeploymentIntentByAttemptQuery(ctx context.Context, q rowQuery, attemptID string) (SourceDeploymentIntent, error) {
	return scanSourceDeploymentIntent(q.QueryRow(ctx, sourceDeploymentIntentSelect+` WHERE attempt_id=$1`, attemptID))
}

func scanSourceDeploymentIntent(row sourceDeploymentScanner) (SourceDeploymentIntent, error) {
	var intent SourceDeploymentIntent
	var leaseUntil *time.Time
	err := row.Scan(&intent.ID, &intent.AttemptID, &intent.ActorID, &intent.ProjectID, &intent.ApplicationID, &intent.EnvironmentID,
		&intent.DeploymentID, &intent.Mode, &intent.SourceAttemptID, &intent.Sequence, &intent.DefinitionID, &intent.DefinitionDigest,
		&intent.SourceDeploymentGeneration, &intent.SourceConfigETag, &intent.ConfigIntent, &intent.TemplateDigest, &intent.RequestID, &intent.StartDraft,
		&intent.State, &intent.Attempts, &intent.AvailableAt, &intent.LeaseOwner, &leaseUntil, &intent.LeaseEpoch,
		&intent.OperationID, &intent.FailureCode, &intent.CreatedAt, &intent.UpdatedAt, &intent.CompletedAt)
	if err != nil {
		return SourceDeploymentIntent{}, classifyPostgres(err)
	}
	if leaseUntil != nil {
		intent.LeaseUntil = *leaseUntil
	}
	if intent.validate() != nil {
		return SourceDeploymentIntent{}, ErrConflict
	}
	return intent, nil
}

var _ SourceDeploymentStore = (*PostgreSQLStore)(nil)
