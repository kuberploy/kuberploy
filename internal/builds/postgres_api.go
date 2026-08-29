package builds

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/builder"
)

func (s *PostgreSQLStore) Installation(ctx context.Context, installationID string) (Installation, error) {
	if !uuidRE.MatchString(installationID) {
		return Installation{}, ErrInvalid
	}
	return installationByIDQuery(ctx, s.pool, installationID)
}

func (s *PostgreSQLStore) Repository(ctx context.Context, repositoryID string) (Repository, error) {
	if !uuidRE.MatchString(repositoryID) {
		return Repository{}, ErrInvalid
	}
	return repositoryByIDQuery(ctx, s.pool, repositoryID)
}

func (s *PostgreSQLStore) Definition(ctx context.Context, definitionID string) (BuildDefinition, error) {
	if !uuidRE.MatchString(definitionID) {
		return BuildDefinition{}, ErrInvalid
	}
	return definitionByIDQuery(ctx, s.pool, definitionID, false)
}

func (s *PostgreSQLStore) ListRepositories(ctx context.Context, installationID string) ([]Repository, error) {
	if !uuidRE.MatchString(installationID) {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text,installation_id::text,github_repository_id,github_owner_id,owner_login,name,lifecycle,last_verified_at,removed_at,created_at,updated_at
		FROM github_repositories WHERE installation_id=$1 ORDER BY github_repository_id`, installationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Repository, 0)
	for rows.Next() {
		repository, scanErr := scanRepository(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, repository)
	}
	return result, rows.Err()
}

func (s *PostgreSQLStore) DefinitionsForService(ctx context.Context, serviceID string) ([]BuildDefinition, error) {
	if !uuidRE.MatchString(serviceID) {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT build_source_id::text,project_id::text,id::text,build_source_kind,
		COALESCE(build_source_installation_id::text,''),COALESCE(build_source_repository_id::text,''),build_source_git_ssh,
		build_source_trigger_ref,build_source_spec,build_source_digest,build_source_revision,true,
		build_source_created_at,build_source_updated_at
		FROM applications WHERE id=$1 AND build_source_id IS NOT NULL`, serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]BuildDefinition, 0)
	for rows.Next() {
		definition, scanErr := scanDefinition(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, definition)
	}
	return result, rows.Err()
}

func (s *PostgreSQLStore) AttemptsForService(ctx context.Context, serviceID string, limit int) ([]BuildAttempt, error) {
	if !uuidRE.MatchString(serviceID) || limit < 1 || limit > 100 {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text,definition_id::text,project_id::text,service_id::text,commit_sha,git_ref,generation,state,execution_attempts,max_attempts,cache_reference,result,failure_code,cancel_requested_at,started_at,completed_at,created_at,updated_at
		FROM build_attempts WHERE service_id=$1 ORDER BY created_at DESC,id DESC LIMIT $2`, serviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]BuildAttempt, 0)
	for rows.Next() {
		attempt, scanErr := scanAttemptHistory(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, attempt)
	}
	return result, rows.Err()
}

// scanAttemptHistory reads only the bounded, credential-free projection used
// by the history API. Historical terminal attempts remain readable when a
// later release strengthens the private immutable execution protocol. Direct
// attempt reads and every mutation continue to use scanAttempt and its exact
// current protocol validation.
func scanAttemptHistory(row scanner) (BuildAttempt, error) {
	var attempt BuildAttempt
	var resultJSON []byte
	if err := row.Scan(&attempt.ID, &attempt.DefinitionID, &attempt.ProjectID, &attempt.ServiceID, &attempt.CommitSHA, &attempt.GitRef,
		&attempt.Generation, &attempt.State, &attempt.ExecutionAttempts, &attempt.MaxAttempts, &attempt.CacheReference, &resultJSON,
		&attempt.FailureCode, &attempt.CancelRequestedAt, &attempt.StartedAt, &attempt.CompletedAt, &attempt.CreatedAt, &attempt.UpdatedAt); err != nil {
		return BuildAttempt{}, classifyPostgres(err)
	}
	if len(resultJSON) > 0 {
		var result builder.BuildResult
		if err := decodeClosedJSON(resultJSON, &result); err != nil {
			return BuildAttempt{}, ErrInvalid
		}
		normalizeLegacyCacheReuse(&result)
		attempt.Result = &result
	}
	if err := validateStoredAttemptHistory(attempt); err != nil {
		return BuildAttempt{}, err
	}
	return attempt, nil
}

func validateStoredAttemptHistory(attempt BuildAttempt) error {
	if !uuidRE.MatchString(attempt.ID) || !uuidRE.MatchString(attempt.DefinitionID) || !uuidRE.MatchString(attempt.ProjectID) ||
		!uuidRE.MatchString(attempt.ServiceID) || !commitRE.MatchString(attempt.CommitSHA) || !validGitRef(attempt.GitRef) ||
		attempt.Generation < 1 || attempt.ExecutionAttempts < 0 || attempt.MaxAttempts < 1 || attempt.MaxAttempts > 5 ||
		(attempt.FailureCode != "" && !failureRE.MatchString(attempt.FailureCode)) || attempt.CreatedAt.IsZero() ||
		attempt.UpdatedAt.IsZero() || terminalAttempt(attempt.State) != (attempt.CompletedAt != nil) {
		return ErrInvalid
	}
	switch attempt.State {
	case AttemptQueued, AttemptPreparing, AttemptRunning, AttemptCancelling, AttemptSucceeded, AttemptFailed, AttemptCancelled:
	default:
		return ErrInvalid
	}
	if attempt.State == AttemptSucceeded {
		if attempt.Result == nil || attempt.Result.OperationID != attempt.ID || attempt.Result.Generation != attempt.Generation ||
			validateBuildResult(*attempt.Result, attempt.CacheReference, "") != nil {
			return ErrInvalid
		}
	}
	return nil
}

func (s *PostgreSQLStore) ClaimAPICommand(ctx context.Context, actorID, operation, scopeID, key, fingerprint, resourceID string, now time.Time) (string, bool, error) {
	if !validAPICommand(operation, actorID, scopeID, key, fingerprint, resourceID, now) {
		return "", false, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "build-api-command|"+actorID+"|"+operation+"|"+scopeID+"|"+key); err != nil {
		return "", false, err
	}
	command, err := tx.Exec(ctx, `INSERT INTO mutation_receipts(actor_id,receipt_kind,namespace,scope_key,idempotency_key,request_digest,resource_id,created_at)
		VALUES($1,'build-api',$2,$3::text,$4,$5,$6,$7) ON CONFLICT(actor_id,receipt_kind,namespace,scope_key,idempotency_key) DO NOTHING`, actorID, operation, scopeID, key, fingerprint, resourceID, now.UTC())
	if err != nil {
		return "", false, classifyPostgres(err)
	}
	if command.RowsAffected() == 1 {
		return resourceID, false, tx.Commit(ctx)
	}
	var storedFingerprint, storedResource string
	err = tx.QueryRow(ctx, `SELECT request_digest,resource_id::text FROM mutation_receipts
		WHERE actor_id=$1 AND receipt_kind='build-api' AND namespace=$2 AND scope_key=$3::text AND idempotency_key=$4 FOR UPDATE`, actorID, operation, scopeID, key).
		Scan(&storedFingerprint, &storedResource)
	if err != nil {
		return "", false, classifyPostgres(err)
	}
	if storedFingerprint != fingerprint {
		return "", false, ErrConflict
	}
	return storedResource, true, tx.Commit(ctx)
}

func (s *PostgreSQLStore) RetryAttempt(ctx context.Context, sourceAttemptID, retryAttemptID, claimKey string, execution ExecutionSettings, now time.Time) (BuildAttempt, bool, error) {
	if !uuidRE.MatchString(sourceAttemptID) || !uuidRE.MatchString(retryAttemptID) || !regexpHex64(claimKey) || now.IsZero() {
		return BuildAttempt{}, false, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return BuildAttempt{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "build-api-retry|"+retryAttemptID); err != nil {
		return BuildAttempt{}, false, err
	}
	if existing, getErr := attemptByIDQuery(ctx, tx, retryAttemptID, false); getErr == nil {
		if existing.TriggerKey != claimKey {
			return BuildAttempt{}, false, ErrConflict
		}
		return existing, true, tx.Commit(ctx)
	} else if !errors.Is(getErr, ErrNotFound) {
		return BuildAttempt{}, false, getErr
	}
	source, err := attemptByIDQuery(ctx, tx, sourceAttemptID, true)
	if err != nil {
		return BuildAttempt{}, false, err
	}
	if source.State != AttemptFailed && source.State != AttemptCancelled {
		return BuildAttempt{}, false, ErrConflict
	}
	definition := source.SourceSnapshot
	if !definition.Enabled || definition.DefinitionDigest != source.DefinitionDigest || definition.validate() != nil {
		return BuildAttempt{}, false, ErrUnauthorized
	}
	var installation Installation
	var repository Repository
	if definition.SourceKind == SourceGitHub {
		installation, err = installationByIDQuery(ctx, tx, definition.InstallationID)
		if err != nil {
			return BuildAttempt{}, false, err
		}
		repository, err = repositoryByIDQuery(ctx, tx, definition.RepositoryID)
		if err != nil {
			return BuildAttempt{}, false, err
		}
		if installation.Lifecycle != InstallationActive || repository.Lifecycle != RepositoryActive || repository.InstallationID != installation.ID {
			return BuildAttempt{}, false, ErrUnauthorized
		}
	} else {
		var keyStatus string
		if definition.GitSSH == nil {
			return BuildAttempt{}, false, ErrUnauthorized
		}
		if err = tx.QueryRow(ctx, `SELECT status FROM git_ssh_key_revisions WHERE scope=$1 AND owner_id=$2 AND revision=$3 FOR SHARE`,
			definition.GitSSH.KeyScope, definition.GitSSH.KeyOwnerID, definition.GitSSH.KeyRevision).Scan(&keyStatus); err != nil {
			return BuildAttempt{}, false, classifyPostgres(err)
		}
		if keyStatus != "active" {
			return BuildAttempt{}, false, ErrUnauthorized
		}
	}
	var generation int64
	err = tx.QueryRow(ctx, `UPDATE applications SET build_generation=build_generation+1 WHERE project_id=$1 AND id=$2 RETURNING build_generation`,
		definition.ProjectID, definition.ServiceID).Scan(&generation)
	if err != nil {
		return BuildAttempt{}, false, classifyPostgres(err)
	}
	imports, err := cacheImportsQuery(ctx, tx, definition, generation)
	if err != nil {
		return BuildAttempt{}, false, err
	}
	attempt, err := newAttemptWithExecution(definition, execution, repository, EnqueuePush{ClaimKey: claimKey, CommitSHA: source.CommitSHA, GitRef: source.GitRef, ResolvedAt: now.UTC()}, generation, imports, now)
	if err != nil || attempt.ID != retryAttemptID {
		return BuildAttempt{}, false, ErrInvalid
	}
	if definition.SourceKind == SourceGitHub {
		claimRetainUntil := now.UTC().Add(24 * time.Hour)
		_, err = tx.Exec(ctx, `INSERT INTO github_one_time_claims(kind,claim_key,retain_until,permanent) VALUES('github-delivery',$1,$2,true)`, claimKey, claimRetainUntil)
		if err != nil {
			return BuildAttempt{}, false, classifyPostgres(err)
		}
		bodyDigest := sha256.Sum256([]byte("kuberploy-manual-build-retry-v1\x00" + sourceAttemptID))
		deliveryID := deterministicUUID("build-retry-delivery-v1", claimKey)
		_, err = tx.Exec(ctx, `INSERT INTO github_webhook_receipts(claim_key,github_app_id,github_installation_id,delivery_id,event,body_sha256,typed_event,repository_id,git_ref,state,available_at,received_at,completed_at,updated_at)
			VALUES($1,$2,$3,$4,'push',$5,NULL,$6,$7,'enqueued',$8,$8,$8,$8)`, claimKey, installation.AppID, installation.GitHubInstallationID,
			deliveryID, "sha256:"+hex.EncodeToString(bodyDigest[:]), repository.Identity.ID, source.GitRef, now.UTC())
		if err != nil {
			return BuildAttempt{}, false, classifyPostgres(err)
		}
	} else {
		attempt.DeliveryClaimKey = ""
		attempt.TriggerKind, attempt.TriggerKey = "retry", claimKey
		if err = validateStoredAttempt(attempt); err != nil {
			return BuildAttempt{}, false, err
		}
	}
	planJSON, _ := json.Marshal(attempt.PlanRequest)
	checkoutJSON, _ := json.Marshal(attempt.CheckoutRequest)
	sourceJSON, _ := json.Marshal(attempt.SourceSnapshot)
	_, err = tx.Exec(ctx, `INSERT INTO build_attempts(id,definition_id,delivery_claim_key,trigger_kind,trigger_key,project_id,service_id,commit_sha,git_ref,generation,definition_digest,source_snapshot,plan_request,checkout_request,input_digest,registry_mode,state,execution_attempts,max_attempts,available_at,job_namespace,job_name,cache_candidate,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,'queued',0,$17,$18,$19,$20,$21,$22,$22)`,
		attempt.ID, attempt.DefinitionID, nullableUUID(attempt.DeliveryClaimKey), attempt.TriggerKind, attempt.TriggerKey, attempt.ProjectID, attempt.ServiceID,
		attempt.CommitSHA, attempt.GitRef, attempt.Generation, attempt.DefinitionDigest, sourceJSON, planJSON, checkoutJSON, attempt.InputDigest, attempt.RegistryMode,
		attempt.MaxAttempts, attempt.AvailableAt, attempt.JobNamespace, attempt.JobName, attempt.CacheCandidate, attempt.CreatedAt)
	if err != nil {
		return BuildAttempt{}, false, classifyPostgres(err)
	}
	return attempt, false, tx.Commit(ctx)
}

func (s *PostgreSQLStore) EnqueueManualAttempt(ctx context.Context, definitionID, commitSHA, claimKey string, execution ExecutionSettings, now time.Time) (BuildAttempt, bool, error) {
	if !uuidRE.MatchString(definitionID) || !commitRE.MatchString(commitSHA) || !regexpHex64(claimKey) || now.IsZero() {
		return BuildAttempt{}, false, ErrInvalid
	}
	attemptID := ManualAttemptID(claimKey, definitionID)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return BuildAttempt{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "build-manual|"+attemptID); err != nil {
		return BuildAttempt{}, false, err
	}
	if existing, getErr := attemptByIDQuery(ctx, tx, attemptID, false); getErr == nil {
		if existing.TriggerKind != "manual" || existing.TriggerKey != claimKey || existing.DefinitionID != definitionID || existing.CommitSHA != commitSHA {
			return BuildAttempt{}, false, ErrConflict
		}
		return existing, true, tx.Commit(ctx)
	} else if !errors.Is(getErr, ErrNotFound) {
		return BuildAttempt{}, false, getErr
	}
	definition, err := definitionByIDQuery(ctx, tx, definitionID, true)
	if err != nil {
		return BuildAttempt{}, false, err
	}
	if definition.SourceKind != SourceGitSSH || definition.GitSSH == nil || !definition.Enabled {
		return BuildAttempt{}, false, ErrUnauthorized
	}
	var keyStatus string
	if err = tx.QueryRow(ctx, `SELECT status FROM git_ssh_key_revisions WHERE scope=$1 AND owner_id=$2 AND revision=$3 FOR SHARE`,
		definition.GitSSH.KeyScope, definition.GitSSH.KeyOwnerID, definition.GitSSH.KeyRevision).Scan(&keyStatus); err != nil {
		return BuildAttempt{}, false, classifyPostgres(err)
	}
	if keyStatus != "active" {
		return BuildAttempt{}, false, ErrGitSSHKeyInactive
	}
	var generation int64
	if err = tx.QueryRow(ctx, `UPDATE applications SET build_generation=build_generation+1 WHERE project_id=$1 AND id=$2 RETURNING build_generation`,
		definition.ProjectID, definition.ServiceID).Scan(&generation); err != nil {
		return BuildAttempt{}, false, classifyPostgres(err)
	}
	imports, err := cacheImportsQuery(ctx, tx, definition, generation)
	if err != nil {
		return BuildAttempt{}, false, err
	}
	attempt, err := newAttemptWithExecution(definition, execution, Repository{}, EnqueuePush{
		ClaimKey: claimKey, CommitSHA: commitSHA, GitRef: definition.TriggerRef, ResolvedAt: now.UTC(),
	}, generation, imports, now)
	if err != nil || attempt.ID != attemptID {
		return BuildAttempt{}, false, ErrInvalid
	}
	attempt.DeliveryClaimKey = ""
	attempt.TriggerKind, attempt.TriggerKey = "manual", claimKey
	if err = validateStoredAttempt(attempt); err != nil {
		return BuildAttempt{}, false, err
	}
	planJSON, _ := json.Marshal(attempt.PlanRequest)
	checkoutJSON, _ := json.Marshal(attempt.CheckoutRequest)
	sourceJSON, _ := json.Marshal(attempt.SourceSnapshot)
	_, err = tx.Exec(ctx, `INSERT INTO build_attempts(id,definition_id,delivery_claim_key,trigger_kind,trigger_key,project_id,service_id,commit_sha,git_ref,generation,definition_digest,source_snapshot,plan_request,checkout_request,input_digest,registry_mode,state,execution_attempts,max_attempts,available_at,job_namespace,job_name,cache_candidate,created_at,updated_at)
		VALUES($1,$2,NULL,'manual',$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'queued',0,$15,$16,$17,$18,$19,$20,$20)`,
		attempt.ID, attempt.DefinitionID, attempt.TriggerKey, attempt.ProjectID, attempt.ServiceID, attempt.CommitSHA, attempt.GitRef,
		attempt.Generation, attempt.DefinitionDigest, sourceJSON, planJSON, checkoutJSON, attempt.InputDigest, attempt.RegistryMode,
		attempt.MaxAttempts, attempt.AvailableAt, attempt.JobNamespace, attempt.JobName, attempt.CacheCandidate, attempt.CreatedAt)
	if err != nil {
		return BuildAttempt{}, false, classifyPostgres(err)
	}
	return attempt, false, tx.Commit(ctx)
}
