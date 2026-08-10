package builds

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
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
	rows, err := s.pool.Query(ctx, `SELECT id::text,project_id::text,service_id::text,installation_id::text,repository_id::text,trigger_ref,spec,definition_digest,generation,enabled,created_at,updated_at
		FROM build_definitions WHERE service_id=$1 ORDER BY updated_at DESC,id`, serviceID)
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
	rows, err := s.pool.Query(ctx, `SELECT id::text,definition_id::text,delivery_claim_key,project_id::text,service_id::text,commit_sha,git_ref,generation,definition_digest,plan_request,checkout_request,input_digest,registry_mode,state,execution_attempts,max_attempts,available_at,lease_owner,lease_until,job_namespace,job_name,cache_candidate,cache_reference,result,log_reference,failure_code,cancel_requested_at,started_at,completed_at,created_at,updated_at
		FROM build_attempts WHERE service_id=$1 ORDER BY created_at DESC,id DESC LIMIT $2`, serviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]BuildAttempt, 0)
	for rows.Next() {
		attempt, scanErr := scanAttempt(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, attempt)
	}
	return result, rows.Err()
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
	command, err := tx.Exec(ctx, `INSERT INTO build_api_idempotency(actor_id,operation,scope_id,idempotency_key,request_fingerprint,resource_id,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(actor_id,operation,scope_id,idempotency_key) DO NOTHING`, actorID, operation, scopeID, key, fingerprint, resourceID, now.UTC())
	if err != nil {
		return "", false, classifyPostgres(err)
	}
	if command.RowsAffected() == 1 {
		return resourceID, false, tx.Commit(ctx)
	}
	var storedFingerprint, storedResource string
	err = tx.QueryRow(ctx, `SELECT request_fingerprint,resource_id::text FROM build_api_idempotency
		WHERE actor_id=$1 AND operation=$2 AND scope_id=$3 AND idempotency_key=$4 FOR UPDATE`, actorID, operation, scopeID, key).
		Scan(&storedFingerprint, &storedResource)
	if err != nil {
		return "", false, classifyPostgres(err)
	}
	if storedFingerprint != fingerprint {
		return "", false, ErrConflict
	}
	return storedResource, true, tx.Commit(ctx)
}

func (s *PostgreSQLStore) RetryAttempt(ctx context.Context, sourceAttemptID, retryAttemptID, claimKey string, now time.Time) (BuildAttempt, bool, error) {
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
		if existing.DeliveryClaimKey != claimKey {
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
	definition, err := definitionByIDQuery(ctx, tx, source.DefinitionID, true)
	if err != nil {
		return BuildAttempt{}, false, err
	}
	if !definition.Enabled || definition.DefinitionDigest != source.DefinitionDigest {
		return BuildAttempt{}, false, ErrUnauthorized
	}
	installation, err := installationByIDQuery(ctx, tx, definition.InstallationID)
	if err != nil {
		return BuildAttempt{}, false, err
	}
	repository, err := repositoryByIDQuery(ctx, tx, definition.RepositoryID)
	if err != nil {
		return BuildAttempt{}, false, err
	}
	if installation.Lifecycle != InstallationActive || repository.Lifecycle != RepositoryActive || repository.InstallationID != installation.ID {
		return BuildAttempt{}, false, ErrUnauthorized
	}
	var generation int64
	err = tx.QueryRow(ctx, `INSERT INTO build_service_generations(project_id,service_id,last_generation) VALUES($1,$2,1)
		ON CONFLICT(project_id,service_id) DO UPDATE SET last_generation=build_service_generations.last_generation+1 RETURNING last_generation`,
		definition.ProjectID, definition.ServiceID).Scan(&generation)
	if err != nil {
		return BuildAttempt{}, false, classifyPostgres(err)
	}
	imports, err := cacheImportsQuery(ctx, tx, definition, generation)
	if err != nil {
		return BuildAttempt{}, false, err
	}
	attempt, err := newAttempt(definition, repository, EnqueuePush{ClaimKey: claimKey, CommitSHA: source.CommitSHA, GitRef: source.GitRef, ResolvedAt: now.UTC()}, generation, imports, now)
	if err != nil || attempt.ID != retryAttemptID {
		return BuildAttempt{}, false, ErrInvalid
	}
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
	planJSON, _ := json.Marshal(attempt.PlanRequest)
	checkoutJSON, _ := json.Marshal(attempt.CheckoutRequest)
	_, err = tx.Exec(ctx, `INSERT INTO build_attempts(id,definition_id,delivery_claim_key,project_id,service_id,commit_sha,git_ref,generation,definition_digest,plan_request,checkout_request,input_digest,registry_mode,state,execution_attempts,max_attempts,available_at,job_namespace,job_name,cache_candidate,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,'queued',0,$14,$15,$16,$17,$18,$19,$19)`,
		attempt.ID, attempt.DefinitionID, attempt.DeliveryClaimKey, attempt.ProjectID, attempt.ServiceID, attempt.CommitSHA, attempt.GitRef,
		attempt.Generation, attempt.DefinitionDigest, planJSON, checkoutJSON, attempt.InputDigest, attempt.RegistryMode,
		attempt.MaxAttempts, attempt.AvailableAt, attempt.JobNamespace, attempt.JobName, attempt.CacheCandidate, attempt.CreatedAt)
	if err != nil {
		return BuildAttempt{}, false, classifyPostgres(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO build_outbox(attempt_id,kind,trace_id,available_at,created_at) VALUES($1,'source-build',$2,$3,$3)`, attempt.ID, claimKey, now.UTC())
	if err != nil {
		return BuildAttempt{}, false, classifyPostgres(err)
	}
	return attempt, false, tx.Commit(ctx)
}
