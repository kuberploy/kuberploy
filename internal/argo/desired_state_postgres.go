package argo

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const desiredStateColumns = `id::text,generation,project_id::text,environment_id::text,
platform_binding_id::text,environment_binding_id::text,cluster_id::text,
	platform_target_ref,environment_target_ref,environment_revision,environment_generation,path,argo_namespace,destination_namespace,argo_project,
base_revision,write_base_revision,write_base_observed_at,precondition,expected_etag,catalog_digest,chart_repository,chart_name,chart_version,
chart_digest,renderer_image,chart_digest_enforcement,content,content_sha256,message,state,
committed_revision,committed_at,verified_at,next_attempt_at,consecutive_failures,last_failure_code,
lease_owner,lease_epoch,lease_until,worker_contract,worker_config_digest,created_at,updated_at,completed_at`

func scanDesiredState(row pgx.Row) (DesiredStateCommand, error) {
	var command DesiredStateCommand
	var leaseOwner, workerContract, workerConfigDigest *string
	var leaseUntil *time.Time
	err := row.Scan(
		&command.ID, &command.Generation, &command.ProjectID, &command.EnvironmentID,
		&command.PlatformBindingID, &command.EnvironmentBindingID, &command.ClusterID,
		&command.PlatformTargetRef, &command.EnvironmentTargetRef, &command.EnvironmentRevision, &command.EnvironmentGeneration,
		&command.Path, &command.ArgoNamespace,
		&command.DestinationNamespace, &command.ArgoProject, &command.BaseRevision, &command.WriteBaseRevision, &command.WriteBaseObservedAt, &command.Precondition,
		&command.ExpectedETag, &command.CatalogDigest, &command.Runtime.ChartRepository, &command.Runtime.ChartName,
		&command.Runtime.ChartVersion, &command.Runtime.ChartDigest, &command.Runtime.RendererImage,
		&command.DigestEnforcement, &command.Content, &command.ContentSHA256, &command.Message, &command.State,
		&command.CommittedRevision, &command.CommittedAt, &command.VerifiedAt, &command.NextAttemptAt,
		&command.ConsecutiveFailures, &command.LastFailureCode, &leaseOwner, &command.LeaseEpoch, &leaseUntil,
		&workerContract, &workerConfigDigest, &command.CreatedAt, &command.UpdatedAt, &command.CompletedAt,
	)
	if err != nil {
		return DesiredStateCommand{}, classifyPostgres(err)
	}
	if leaseOwner != nil || leaseUntil != nil || workerContract != nil || workerConfigDigest != nil {
		if leaseOwner == nil || leaseUntil == nil || workerContract == nil || workerConfigDigest == nil {
			return DesiredStateCommand{}, ErrInvalid
		}
		command.Lease = &DesiredStateLease{CommandID: command.ID, Owner: *leaseOwner, Epoch: command.LeaseEpoch,
			Until: leaseUntil.UTC(), Contract: *workerContract, ConfigDigest: *workerConfigDigest}
	}
	return command, command.Validate()
}

func (s *PostgreSQLStore) CreateDesiredState(ctx context.Context, command DesiredStateCommand) (bool, error) {
	if command.Validate() != nil || command.Lease != nil || command.WriteBaseRevision != "" || command.WriteBaseObservedAt != nil ||
		command.State != DesiredStatePending && command.State != DesiredStateBlockedPrerequisite {
		return false, ErrInvalid
	}
	result, err := s.pool.Exec(ctx, `INSERT INTO argo_desired_state_commands(
		id,generation,project_id,environment_id,platform_binding_id,environment_binding_id,cluster_id,
		platform_target_ref,environment_target_ref,environment_revision,environment_generation,path,argo_namespace,destination_namespace,argo_project,
		base_revision,precondition,expected_etag,catalog_digest,chart_repository,chart_name,chart_version,
		chart_digest,renderer_image,chart_digest_enforcement,content,content_sha256,message,state,
		next_attempt_at,consecutive_failures,last_failure_code,lease_epoch,created_at,updated_at,completed_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35,$36)
	ON CONFLICT DO NOTHING`,
		command.ID, command.Generation, command.ProjectID, command.EnvironmentID, command.PlatformBindingID,
		command.EnvironmentBindingID, command.ClusterID, command.PlatformTargetRef, command.EnvironmentTargetRef,
		command.EnvironmentRevision, command.EnvironmentGeneration, command.Path, command.ArgoNamespace, command.DestinationNamespace, command.ArgoProject, command.BaseRevision,
		command.Precondition, command.ExpectedETag, command.CatalogDigest, command.Runtime.ChartRepository,
		command.Runtime.ChartName, command.Runtime.ChartVersion, command.Runtime.ChartDigest, command.Runtime.RendererImage,
		command.DigestEnforcement, command.Content, command.ContentSHA256, command.Message, command.State,
		command.NextAttemptAt.UTC(), command.ConsecutiveFailures, command.LastFailureCode, command.LeaseEpoch,
		command.CreatedAt.UTC(), command.UpdatedAt.UTC(), command.CompletedAt)
	if err != nil {
		return false, classifyPostgres(err)
	}
	if result.RowsAffected() == 1 {
		return true, nil
	}
	current, getErr := s.DesiredStateCommand(ctx, command.ID)
	if getErr == nil && equalDesiredStateCommand(current, command) {
		return false, nil
	}
	return false, ErrConflict
}

func (s *PostgreSQLStore) DesiredStateCommand(ctx context.Context, commandID string) (DesiredStateCommand, error) {
	if !uuidRE.MatchString(commandID) {
		return DesiredStateCommand{}, ErrInvalid
	}
	return scanDesiredState(s.pool.QueryRow(ctx, `SELECT `+desiredStateColumns+` FROM argo_desired_state_commands WHERE id=$1`, commandID))
}

func (s *PostgreSQLStore) LatestDesiredState(ctx context.Context, projectID, environmentID string) (DesiredStateStatus, error) {
	if !uuidRE.MatchString(projectID) || !uuidRE.MatchString(environmentID) {
		return DesiredStateStatus{}, ErrInvalid
	}
	command, err := scanDesiredState(s.pool.QueryRow(ctx, `SELECT `+desiredStateColumns+` FROM argo_desired_state_commands
		WHERE project_id=$1 AND environment_id=$2 ORDER BY generation DESC LIMIT 1`, projectID, environmentID))
	if err != nil {
		return DesiredStateStatus{}, err
	}
	return command.Status(), nil
}

func (s *PostgreSQLStore) ClaimDesiredState(ctx context.Context, owner string, identity DesiredStateWorkerIdentity, now time.Time, leaseDuration time.Duration) (DesiredStateWork, error) {
	if !desiredStateOwnerRE.MatchString(owner) || identity.Validate() != nil || now.IsZero() || !validDesiredStateLeaseDuration(leaseDuration) {
		return DesiredStateWork{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return DesiredStateWork{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// Clear expired ownership before selecting work. lease_epoch is retained and
	// incremented by the next claimant, fencing the crashed worker permanently.
	if _, err = tx.Exec(ctx, `UPDATE argo_desired_state_commands SET
		state=CASE WHEN state='claimed' THEN 'pending' ELSE state END,
		lease_owner=NULL,lease_until=NULL,worker_contract=NULL,worker_config_digest=NULL,updated_at=$1
		WHERE lease_owner IS NOT NULL AND lease_until<=$1 AND state IN ('claimed','git-committed')`, now.UTC()); err != nil {
		return DesiredStateWork{}, classifyPostgres(err)
	}
	// A never-attempted command cannot have reached Git. Retire it when its
	// approved generation is no longer the exact active projection so it cannot
	// block a replacement forever. Commands with a prior lease may represent an
	// unacknowledged push and must remain claimable for trailer recovery.
	if _, err = tx.Exec(ctx, `UPDATE argo_desired_state_commands candidate SET
		state='superseded',completed_at=$1,updated_at=$1,
		consecutive_failures=LEAST(candidate.consecutive_failures+1,30),last_failure_code='projection-superseded'
		WHERE candidate.state='pending' AND candidate.lease_owner IS NULL AND candidate.lease_epoch=0
		AND NOT EXISTS(
			SELECT 1 FROM git_repository_bindings environment_binding
			JOIN git_projection_generations active_generation
			  ON active_generation.binding_id=environment_binding.id
			 AND active_generation.generation=environment_binding.projection_generation
			WHERE environment_binding.id=candidate.environment_binding_id
			  AND environment_binding.state='ready'
			  AND environment_binding.target_head_revision=candidate.environment_revision
			  AND environment_binding.indexed_revision=candidate.environment_revision
			  AND environment_binding.projection_generation=candidate.environment_generation
			  AND active_generation.state='active'
			  AND active_generation.head_revision=candidate.environment_revision
			  AND NOT EXISTS(
				SELECT 1 FROM git_projected_documents invalid_document
				WHERE invalid_document.binding_id=candidate.environment_binding_id
				  AND invalid_document.generation=candidate.environment_generation
				  AND NOT invalid_document.valid
			  )
		)`, now.UTC()); err != nil {
		return DesiredStateWork{}, classifyPostgres(err)
	}
	var commandID string
	err = tx.QueryRow(ctx, `SELECT candidate.id::text FROM argo_desired_state_commands candidate
		WHERE candidate.state IN ('pending','git-committed') AND candidate.next_attempt_at<=$1
		AND candidate.lease_owner IS NULL
		AND (candidate.state='git-committed' OR candidate.lease_epoch>0 OR EXISTS(
			SELECT 1 FROM git_repository_bindings environment_binding
			JOIN git_projection_generations active_generation
			  ON active_generation.binding_id=environment_binding.id
			 AND active_generation.generation=environment_binding.projection_generation
			WHERE environment_binding.id=candidate.environment_binding_id
			  AND environment_binding.state='ready'
			  AND environment_binding.target_head_revision=candidate.environment_revision
			  AND environment_binding.indexed_revision=candidate.environment_revision
			  AND environment_binding.projection_generation=candidate.environment_generation
			  AND active_generation.state='active'
			  AND active_generation.head_revision=candidate.environment_revision
			  AND NOT EXISTS(
				SELECT 1 FROM git_projected_documents invalid_document
				WHERE invalid_document.binding_id=candidate.environment_binding_id
				  AND invalid_document.generation=candidate.environment_generation
				  AND NOT invalid_document.valid
			  )
		))
		AND NOT EXISTS(
			SELECT 1 FROM argo_desired_state_commands held
			WHERE held.platform_binding_id=candidate.platform_binding_id
			AND held.lease_owner IS NOT NULL AND held.lease_until>$1
		)
		ORDER BY candidate.next_attempt_at,candidate.created_at,candidate.id
		FOR UPDATE OF candidate SKIP LOCKED LIMIT 1`, now.UTC()).Scan(&commandID)
	if err != nil {
		classified := classifyPostgres(err)
		if errors.Is(classified, ErrNotFound) {
			// Persist expired-lease cleanup and retirement of never-attempted
			// stale commands even when no work remains in this transaction.
			if commitErr := tx.Commit(ctx); commitErr != nil {
				return DesiredStateWork{}, classifyPostgres(commitErr)
			}
		}
		return DesiredStateWork{}, classified
	}
	command, err := scanDesiredState(tx.QueryRow(ctx, `UPDATE argo_desired_state_commands SET
		state=CASE WHEN state='pending' THEN 'claimed' ELSE state END,
		lease_owner=$2,lease_epoch=lease_epoch+1,lease_until=$3,worker_contract=$4,
		worker_config_digest=$5,updated_at=$1 WHERE id=$6
		RETURNING `+desiredStateColumns, now.UTC(), owner, now.UTC().Add(leaseDuration), identity.ContractVersion, identity.ConfigDigest, commandID))
	if err != nil {
		return DesiredStateWork{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return DesiredStateWork{}, classifyPostgres(err)
	}
	return DesiredStateWork{Command: command, Lease: *command.Lease}, nil
}

func (s *PostgreSQLStore) HeartbeatDesiredState(ctx context.Context, lease DesiredStateLease, now time.Time, leaseDuration time.Duration) (DesiredStateLease, error) {
	if lease.Validate() != nil || now.IsZero() || !validDesiredStateLeaseDuration(leaseDuration) {
		return DesiredStateLease{}, ErrInvalid
	}
	var updated DesiredStateLease
	err := s.pool.QueryRow(ctx, `UPDATE argo_desired_state_commands SET lease_until=$7,updated_at=$6
		WHERE id=$1 AND lease_owner=$2 AND lease_epoch=$3 AND worker_contract=$4 AND worker_config_digest=$5
		AND lease_until>$6 AND state IN ('claimed','git-committed')
		RETURNING id::text,lease_owner,lease_epoch,lease_until,worker_contract,worker_config_digest`,
		lease.CommandID, lease.Owner, lease.Epoch, lease.Contract, lease.ConfigDigest, now.UTC(), now.UTC().Add(leaseDuration)).Scan(
		&updated.CommandID, &updated.Owner, &updated.Epoch, &updated.Until, &updated.Contract, &updated.ConfigDigest)
	if err != nil {
		if errors.Is(classifyPostgres(err), ErrNotFound) {
			return DesiredStateLease{}, ErrLeaseLost
		}
		return DesiredStateLease{}, classifyPostgres(err)
	}
	return updated, updated.Validate()
}

func (s *PostgreSQLStore) BindDesiredStateWriteBase(ctx context.Context, lease DesiredStateLease, revision string, observedAt, now time.Time) (DesiredStateCommand, error) {
	if lease.Validate() != nil || !commitRE.MatchString(revision) || observedAt.IsZero() || now.IsZero() || observedAt.After(now) {
		return DesiredStateCommand{}, ErrInvalid
	}
	command, err := scanDesiredState(s.pool.QueryRow(ctx, `UPDATE argo_desired_state_commands SET
		write_base_revision=$6,write_base_observed_at=$7,updated_at=$8
		WHERE id=$1 AND lease_owner=$2 AND lease_epoch=$3 AND worker_contract=$4 AND worker_config_digest=$5
		AND lease_until>$8 AND state='claimed' AND write_base_revision=''
		AND created_at<=$7 AND updated_at<=$8
		RETURNING `+desiredStateColumns,
		lease.CommandID, lease.Owner, lease.Epoch, lease.Contract, lease.ConfigDigest,
		revision, observedAt.UTC(), now.UTC()))
	if err == nil {
		return command, nil
	}
	current, currentErr := s.DesiredStateCommand(ctx, lease.CommandID)
	if currentErr == nil && activeDesiredStateLease(current, lease, now) && current.State == DesiredStateClaimed &&
		current.WriteBaseRevision == revision && current.WriteBaseObservedAt != nil && current.WriteBaseObservedAt.Equal(observedAt) {
		return current, nil
	}
	return DesiredStateCommand{}, desiredStateWriteMiss(current, currentErr, lease, now)
}

func (s *PostgreSQLStore) MarkDesiredStateGitCommitted(ctx context.Context, lease DesiredStateLease, revision string, now time.Time) (DesiredStateCommand, error) {
	if lease.Validate() != nil || !commitRE.MatchString(revision) || now.IsZero() {
		return DesiredStateCommand{}, ErrInvalid
	}
	command, err := scanDesiredState(s.pool.QueryRow(ctx, `UPDATE argo_desired_state_commands SET
		state='git-committed',committed_revision=$6,committed_at=$7,updated_at=$7
		WHERE id=$1 AND lease_owner=$2 AND lease_epoch=$3 AND worker_contract=$4 AND worker_config_digest=$5
		AND lease_until>$7 AND state='claimed' RETURNING `+desiredStateColumns,
		lease.CommandID, lease.Owner, lease.Epoch, lease.Contract, lease.ConfigDigest, revision, now.UTC()))
	if err == nil {
		return command, nil
	}
	current, currentErr := s.DesiredStateCommand(ctx, lease.CommandID)
	if currentErr == nil && activeDesiredStateLease(current, lease, now) && current.State == DesiredStateGitCommitted && current.CommittedRevision == revision {
		return current, nil
	}
	return DesiredStateCommand{}, desiredStateWriteMiss(current, currentErr, lease, now)
}

func (s *PostgreSQLStore) CompleteDesiredStateVerified(ctx context.Context, lease DesiredStateLease, revision string, now time.Time) (DesiredStateCommand, error) {
	if lease.Validate() != nil || !commitRE.MatchString(revision) || now.IsZero() {
		return DesiredStateCommand{}, ErrInvalid
	}
	command, err := scanDesiredState(s.pool.QueryRow(ctx, `UPDATE argo_desired_state_commands SET
		state='verified',verified_at=$7,completed_at=$7,lease_owner=NULL,lease_until=NULL,
		worker_contract=NULL,worker_config_digest=NULL,updated_at=$7
		WHERE id=$1 AND lease_owner=$2 AND lease_epoch=$3 AND worker_contract=$4 AND worker_config_digest=$5
		AND lease_until>$7 AND state='git-committed' AND committed_revision=$6 AND committed_at<=$7
		RETURNING `+desiredStateColumns,
		lease.CommandID, lease.Owner, lease.Epoch, lease.Contract, lease.ConfigDigest, revision, now.UTC()))
	if err == nil {
		return command, nil
	}
	current, currentErr := s.DesiredStateCommand(ctx, lease.CommandID)
	return DesiredStateCommand{}, desiredStateWriteMiss(current, currentErr, lease, now)
}

func (s *PostgreSQLStore) RetryDesiredState(ctx context.Context, lease DesiredStateLease, retry DesiredStateRetry, now time.Time) (DesiredStateCommand, error) {
	if lease.Validate() != nil || retry.Validate(now) != nil {
		return DesiredStateCommand{}, ErrInvalid
	}
	command, err := scanDesiredState(s.pool.QueryRow(ctx, `UPDATE argo_desired_state_commands SET
		state=CASE WHEN state='claimed' THEN 'pending' ELSE state END,next_attempt_at=$6,
		consecutive_failures=LEAST(consecutive_failures+1,30),last_failure_code=$7,
		lease_owner=NULL,lease_until=NULL,worker_contract=NULL,worker_config_digest=NULL,updated_at=$8
		WHERE id=$1 AND lease_owner=$2 AND lease_epoch=$3 AND worker_contract=$4 AND worker_config_digest=$5
		AND lease_until>$8 AND state IN ('claimed','git-committed')
		RETURNING `+desiredStateColumns,
		lease.CommandID, lease.Owner, lease.Epoch, lease.Contract, lease.ConfigDigest,
		retry.NextAttemptAt.UTC(), retry.FailureCode, now.UTC()))
	if err == nil {
		return command, nil
	}
	current, currentErr := s.DesiredStateCommand(ctx, lease.CommandID)
	return DesiredStateCommand{}, desiredStateWriteMiss(current, currentErr, lease, now)
}

func (s *PostgreSQLStore) FailDesiredState(ctx context.Context, lease DesiredStateLease, failureCode string, now time.Time) (DesiredStateCommand, error) {
	if lease.Validate() != nil || !failureCodeRE.MatchString(failureCode) || now.IsZero() {
		return DesiredStateCommand{}, ErrInvalid
	}
	command, err := scanDesiredState(s.pool.QueryRow(ctx, `UPDATE argo_desired_state_commands SET
		state='failed',consecutive_failures=LEAST(consecutive_failures+1,30),last_failure_code=$6,
		completed_at=$7,lease_owner=NULL,lease_until=NULL,worker_contract=NULL,worker_config_digest=NULL,updated_at=$7
		WHERE id=$1 AND lease_owner=$2 AND lease_epoch=$3 AND worker_contract=$4 AND worker_config_digest=$5
		AND lease_until>$7 AND state='claimed'
		RETURNING `+desiredStateColumns,
		lease.CommandID, lease.Owner, lease.Epoch, lease.Contract, lease.ConfigDigest, failureCode, now.UTC()))
	if err == nil {
		return command, nil
	}
	current, currentErr := s.DesiredStateCommand(ctx, lease.CommandID)
	return DesiredStateCommand{}, desiredStateWriteMiss(current, currentErr, lease, now)
}

func desiredStateWriteMiss(current DesiredStateCommand, err error, lease DesiredStateLease, now time.Time) error {
	if err != nil {
		return err
	}
	if !activeDesiredStateLease(current, lease, now) {
		return ErrLeaseLost
	}
	return ErrConflict
}

func (s *PostgreSQLStore) AcquireDesiredStateReadiness(ctx context.Context, observation DesiredStateRuntimeWorkerObservation, duration time.Duration) (DesiredStateRuntimeLease, error) {
	if observation.Validate() != nil || !validDesiredStateReadinessLease(duration) {
		return DesiredStateRuntimeLease{}, ErrInvalid
	}
	var lease DesiredStateRuntimeLease
	err := s.pool.QueryRow(ctx, `INSERT INTO argo_desired_state_runtime_readiness(
		worker_id,worker_epoch,contract_version,config_digest,github_app_id,platform_binding_id,cluster_id,
		argo_namespace,root_application_name,repository_secret_name,chart_repository,chart_name,chart_version,
		chart_digest,renderer_image,chart_digest_enforcement,started_at,observed_at,lease_until
	) VALUES($1,1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
	ON CONFLICT(worker_id) DO UPDATE SET worker_epoch=argo_desired_state_runtime_readiness.worker_epoch+1,
		contract_version=excluded.contract_version,config_digest=excluded.config_digest,github_app_id=excluded.github_app_id,
		platform_binding_id=excluded.platform_binding_id,cluster_id=excluded.cluster_id,argo_namespace=excluded.argo_namespace,
		root_application_name=excluded.root_application_name,repository_secret_name=excluded.repository_secret_name,
		chart_repository=excluded.chart_repository,chart_name=excluded.chart_name,chart_version=excluded.chart_version,
		chart_digest=excluded.chart_digest,renderer_image=excluded.renderer_image,
		chart_digest_enforcement=excluded.chart_digest_enforcement,started_at=excluded.started_at,
		observed_at=excluded.observed_at,lease_until=excluded.lease_until
	RETURNING worker_id,worker_epoch,contract_version,config_digest,github_app_id,platform_binding_id::text,cluster_id::text,
		argo_namespace,root_application_name,repository_secret_name,chart_repository,chart_name,chart_version,
		chart_digest,renderer_image,chart_digest_enforcement,started_at,observed_at,lease_until`,
		observation.WorkerID, observation.ContractVersion, observation.ConfigDigest, observation.GitHubAppID,
		observation.PlatformBindingID, observation.ClusterID, observation.ArgoNamespace, observation.RootApplicationName,
		observation.RepositorySecretName, observation.Runtime.ChartRepository, observation.Runtime.ChartName,
		observation.Runtime.ChartVersion, observation.Runtime.ChartDigest, observation.Runtime.RendererImage,
		observation.DigestEnforcement, observation.StartedAt.UTC(), observation.ObservedAt.UTC(), observation.ObservedAt.UTC().Add(duration)).Scan(
		&lease.WorkerID, &lease.Epoch, &lease.ContractVersion, &lease.ConfigDigest, &lease.GitHubAppID,
		&lease.PlatformBindingID, &lease.ClusterID, &lease.ArgoNamespace, &lease.RootApplicationName,
		&lease.RepositorySecretName, &lease.Runtime.ChartRepository, &lease.Runtime.ChartName, &lease.Runtime.ChartVersion,
		&lease.Runtime.ChartDigest, &lease.Runtime.RendererImage, &lease.DigestEnforcement, &lease.StartedAt,
		&lease.ObservedAt, &lease.Until)
	if err != nil {
		return DesiredStateRuntimeLease{}, classifyPostgres(err)
	}
	return lease, lease.Validate()
}

func (s *PostgreSQLStore) HeartbeatDesiredStateReadiness(ctx context.Context, lease DesiredStateRuntimeLease, observedAt time.Time, duration time.Duration) (DesiredStateRuntimeLease, error) {
	if lease.Validate() != nil || observedAt.IsZero() || observedAt.Before(lease.ObservedAt) || !validDesiredStateReadinessLease(duration) {
		return DesiredStateRuntimeLease{}, ErrInvalid
	}
	var updated DesiredStateRuntimeLease
	err := s.pool.QueryRow(ctx, `UPDATE argo_desired_state_runtime_readiness SET observed_at=$19,lease_until=$20
		WHERE worker_id=$1 AND worker_epoch=$2 AND contract_version=$3 AND config_digest=$4 AND github_app_id=$5
		AND platform_binding_id=$6 AND cluster_id=$7 AND argo_namespace=$8 AND root_application_name=$9
		AND repository_secret_name=$10 AND chart_repository=$11 AND chart_name=$12 AND chart_version=$13
		AND chart_digest=$14 AND renderer_image=$15 AND chart_digest_enforcement=$16 AND started_at=$17
		AND observed_at=$18 AND lease_until>$19
	RETURNING worker_id,worker_epoch,contract_version,config_digest,github_app_id,platform_binding_id::text,cluster_id::text,
		argo_namespace,root_application_name,repository_secret_name,chart_repository,chart_name,chart_version,
		chart_digest,renderer_image,chart_digest_enforcement,started_at,observed_at,lease_until`,
		lease.WorkerID, lease.Epoch, lease.ContractVersion, lease.ConfigDigest, lease.GitHubAppID,
		lease.PlatformBindingID, lease.ClusterID, lease.ArgoNamespace, lease.RootApplicationName, lease.RepositorySecretName,
		lease.Runtime.ChartRepository, lease.Runtime.ChartName, lease.Runtime.ChartVersion, lease.Runtime.ChartDigest,
		lease.Runtime.RendererImage, lease.DigestEnforcement, lease.StartedAt, lease.ObservedAt,
		observedAt.UTC(), observedAt.UTC().Add(duration)).Scan(
		&updated.WorkerID, &updated.Epoch, &updated.ContractVersion, &updated.ConfigDigest, &updated.GitHubAppID,
		&updated.PlatformBindingID, &updated.ClusterID, &updated.ArgoNamespace, &updated.RootApplicationName,
		&updated.RepositorySecretName, &updated.Runtime.ChartRepository, &updated.Runtime.ChartName, &updated.Runtime.ChartVersion,
		&updated.Runtime.ChartDigest, &updated.Runtime.RendererImage, &updated.DigestEnforcement, &updated.StartedAt,
		&updated.ObservedAt, &updated.Until)
	if err != nil {
		if errors.Is(classifyPostgres(err), ErrNotFound) {
			return DesiredStateRuntimeLease{}, ErrLeaseLost
		}
		return DesiredStateRuntimeLease{}, classifyPostgres(err)
	}
	return updated, updated.Validate()
}

func (s *PostgreSQLStore) DesiredStateRuntimeReady(ctx context.Context, identity DesiredStateRuntimeIdentity, now time.Time, maximumAge time.Duration) error {
	if identity.Validate() != nil || now.IsZero() || maximumAge < 2*DesiredStateHeartbeatInterval || maximumAge > 5*time.Minute {
		return ErrDesiredStateNotReady
	}
	var ready bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM argo_desired_state_runtime_readiness
		WHERE contract_version=$1 AND config_digest=$2 AND github_app_id=$3 AND platform_binding_id=$4
		AND cluster_id=$5 AND argo_namespace=$6 AND root_application_name=$7 AND repository_secret_name=$8
		AND chart_repository=$9 AND chart_name=$10 AND chart_version=$11 AND chart_digest=$12
		AND renderer_image=$13 AND chart_digest_enforcement=$14 AND observed_at>=$15 AND observed_at<=$16
		AND lease_until>$16)`, identity.ContractVersion, identity.ConfigDigest, identity.GitHubAppID,
		identity.PlatformBindingID, identity.ClusterID, identity.ArgoNamespace, identity.RootApplicationName,
		identity.RepositorySecretName, identity.Runtime.ChartRepository, identity.Runtime.ChartName,
		identity.Runtime.ChartVersion, identity.Runtime.ChartDigest, identity.Runtime.RendererImage,
		identity.DigestEnforcement, now.UTC().Add(-maximumAge), now.UTC()).Scan(&ready)
	if err != nil || !ready {
		return ErrDesiredStateNotReady
	}
	return nil
}

var _ DesiredStateStore = (*PostgreSQLStore)(nil)
var _ DesiredStateReadinessStore = (*PostgreSQLStore)(nil)
