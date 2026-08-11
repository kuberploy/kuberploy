package helmapps

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	protectedPayloadTable     = "helm_protected_payload_intents"
	protectedApplicationTable = "helm_protected_application_intents"
	minimumProtectedLease     = 15 * time.Second
	maximumProtectedLease     = 5 * time.Minute
	maximumProtectedRetry     = 24 * time.Hour
)

type PostgresProtectedPublicationStore struct{ pool *pgxpool.Pool }

func NewPostgresProtectedPublicationStore(pool *pgxpool.Pool) (*PostgresProtectedPublicationStore, error) {
	if pool == nil {
		return nil, ErrInvalid
	}
	return &PostgresProtectedPublicationStore{pool: pool}, nil
}

const protectedPayloadColumns = `id::text,release_revision_id::text,release_generation,
	project_id::text,environment_id::text,application_id::text,action,
	platform_binding_id::text,environment_binding_id::text,cluster_id::text,
	platform_target_ref,environment_target_ref,environment_revision,environment_generation,
	catalog_digest,planned_base_revision,path,content,content_digest,
	COALESCE(manifest_inventory_digest,''),COALESCE(manifest_resource_count,0),
	intent_digest,commit_trailer,publisher_contract,publisher_config_digest,message,state,
	next_attempt_at,attempts,consecutive_failures,last_failure_code,COALESCE(lease_owner,''),
	lease_epoch,lease_until,write_base_revision,write_base_observed_at,committed_revision,
	committed_parent_revision,committed_at,verified_at,verified_path_digest,provider_request,
	created_at,updated_at,completed_at`

func scanProtectedPayload(row rowScanner) (ProtectedPayloadIntent, error) {
	var value ProtectedPayloadIntent
	err := row.Scan(&value.ID, &value.ReleaseRevisionID, &value.ReleaseGeneration,
		&value.Target.ProjectID, &value.Target.EnvironmentID, &value.Target.ApplicationID,
		&value.Action, &value.Binding.PlatformBindingID, &value.Binding.EnvironmentBindingID,
		&value.Binding.ClusterID, &value.Binding.PlatformTargetRef,
		&value.Binding.EnvironmentTargetRef, &value.Binding.EnvironmentRevision,
		&value.Binding.EnvironmentGeneration, &value.Binding.CatalogDigest,
		&value.Binding.PlannedBaseRevision, &value.Path, &value.Content,
		&value.ContentDigest, &value.InventoryDigest, &value.ResourceCount,
		&value.IntentDigest, &value.CommitTrailer, &value.Publisher.Contract,
		&value.Publisher.ConfigDigest, &value.Message, &value.State, &value.NextAttemptAt,
		&value.Attempts, &value.ConsecutiveFailures, &value.LastFailureCode,
		&value.LeaseOwner, &value.LeaseEpoch, &value.LeaseUntil, &value.WriteBaseRevision,
		&value.WriteBaseObservedAt, &value.CommittedRevision,
		&value.CommittedParentRevision, &value.CommittedAt, &value.VerifiedAt,
		&value.VerifiedPathDigest, &value.ProviderRequest, &value.CreatedAt,
		&value.UpdatedAt, &value.CompletedAt)
	value.Publisher.PolicyVersion = ProtectedGitPolicy
	if err != nil {
		return ProtectedPayloadIntent{}, classifyPostgres(err)
	}
	if value.Validate() != nil {
		return ProtectedPayloadIntent{}, ErrConflict
	}
	value.Content = append([]byte(nil), value.Content...)
	return value, nil
}

const protectedApplicationColumns = `id::text,release_revision_id::text,payload_intent_id::text,
	release_generation,project_id::text,environment_id::text,application_id::text,action,
	platform_binding_id::text,environment_binding_id::text,cluster_id::text,
	platform_target_ref,environment_target_ref,environment_revision,environment_generation,
	catalog_digest,planned_base_revision,payload_revision,payload_path,source_directory,
	application_path,operation,precondition,expected_etag,content,content_digest,
	intent_digest,commit_trailer,publisher_contract,publisher_config_digest,message,state,
	next_attempt_at,attempts,consecutive_failures,last_failure_code,COALESCE(lease_owner,''),
	lease_epoch,lease_until,write_base_revision,write_base_observed_at,committed_revision,
	committed_parent_revision,committed_at,verified_at,verified_path_digest,provider_request,
	created_at,updated_at,completed_at`

func scanProtectedApplication(row rowScanner) (ProtectedApplicationIntent, error) {
	var value ProtectedApplicationIntent
	err := row.Scan(&value.ID, &value.ReleaseRevisionID, &value.PayloadIntentID,
		&value.ReleaseGeneration, &value.Target.ProjectID, &value.Target.EnvironmentID,
		&value.Target.ApplicationID, &value.Action, &value.Binding.PlatformBindingID,
		&value.Binding.EnvironmentBindingID, &value.Binding.ClusterID,
		&value.Binding.PlatformTargetRef, &value.Binding.EnvironmentTargetRef,
		&value.Binding.EnvironmentRevision, &value.Binding.EnvironmentGeneration,
		&value.Binding.CatalogDigest, &value.Binding.PlannedBaseRevision,
		&value.PayloadRevision, &value.PayloadPath, &value.SourceDirectory,
		&value.ApplicationPath, &value.Operation, &value.Precondition,
		&value.ExpectedETag, &value.Content, &value.ContentDigest, &value.IntentDigest,
		&value.CommitTrailer, &value.Publisher.Contract, &value.Publisher.ConfigDigest,
		&value.Message, &value.State, &value.NextAttemptAt, &value.Attempts,
		&value.ConsecutiveFailures, &value.LastFailureCode, &value.LeaseOwner,
		&value.LeaseEpoch, &value.LeaseUntil, &value.WriteBaseRevision,
		&value.WriteBaseObservedAt, &value.CommittedRevision,
		&value.CommittedParentRevision, &value.CommittedAt, &value.VerifiedAt,
		&value.VerifiedPathDigest, &value.ProviderRequest, &value.CreatedAt,
		&value.UpdatedAt, &value.CompletedAt)
	value.Publisher.PolicyVersion = ProtectedGitPolicy
	if err != nil {
		return ProtectedApplicationIntent{}, classifyPostgres(err)
	}
	if value.Validate() != nil {
		return ProtectedApplicationIntent{}, ErrConflict
	}
	value.Content = append([]byte(nil), value.Content...)
	return value, nil
}

func (s *PostgresProtectedPublicationStore) CreatePayloadForHead(ctx context.Context, intentID string,
	target ReleaseTarget, binding ProtectedBindingSnapshot, publisher ProtectedPublisherIdentity,
	now time.Time) (ProtectedPayloadIntent, bool, error) {
	if !uuidRE.MatchString(intentID) || target.Validate() != nil || binding.Validate() != nil ||
		publisher.Validate() != nil || now.IsZero() {
		return ProtectedPayloadIntent{}, false, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ProtectedPayloadIntent{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var headRevisionID string
	err = tx.QueryRow(ctx, `SELECT revision_id::text FROM helm_release_heads
		WHERE project_id=$1 AND environment_id=$2 AND application_id=$3 FOR KEY SHARE`,
		target.ProjectID, target.EnvironmentID, target.ApplicationID).Scan(&headRevisionID)
	if err != nil {
		return ProtectedPayloadIntent{}, false, classifyPostgres(err)
	}
	release, err := scanReleaseRevision(tx.QueryRow(ctx, releaseRevisionSelect+`
		WHERE id=$1 FOR KEY SHARE`, headRevisionID))
	if err != nil {
		return ProtectedPayloadIntent{}, false, classifyPostgres(err)
	}
	if release.Target != target || now.Before(release.CreatedAt) {
		return ProtectedPayloadIntent{}, false, ErrConflict
	}
	value := ProtectedPayloadIntent{
		ID: intentID, ReleaseRevisionID: release.ID, ReleaseGeneration: release.Generation,
		Target: target, Binding: binding, Publisher: publisher, State: ProtectedPending,
		NextAttemptAt: now.UTC(), CreatedAt: now.UTC(), UpdatedAt: now.UTC(),
		CommitTrailer: "Kuberploy-Helm-Payload-Intent: " + intentID,
		Message:       "Publish protected Helm payload " + release.ID,
	}
	if release.DesiredEnabled {
		value.Action, value.Path = ProtectedPayloadPublish, protectedPayloadPath(binding.ClusterID,
			target.EnvironmentID, target.ApplicationID, release.ID, false)
		var commandState string
		err = tx.QueryRow(ctx, `SELECT result.rendered_manifests,result.manifest_digest,
			result.inventory_digest,result.resource_count,command.state
			FROM helm_render_results result JOIN helm_render_commands command
			ON command.id=result.command_id WHERE result.command_id=$1 FOR KEY SHARE OF result,command`,
			release.RenderCommandID).Scan(&value.Content, &value.ContentDigest,
			&value.InventoryDigest, &value.ResourceCount, &commandState)
		if err != nil || commandState != string(StateSucceeded) {
			if err != nil {
				return ProtectedPayloadIntent{}, false, classifyPostgres(err)
			}
			return ProtectedPayloadIntent{}, false, ErrConflict
		}
	} else {
		value.Action, value.Path = ProtectedPayloadDisable, protectedPayloadPath(binding.ClusterID,
			target.EnvironmentID, target.ApplicationID, release.ID, true)
		value.Content, err = json.Marshal(struct {
			APIVersion        string `json:"apiVersion"`
			Kind              string `json:"kind"`
			ReleaseRevisionID string `json:"releaseRevisionId"`
			Generation        int64  `json:"generation"`
			ProjectID         string `json:"projectId"`
			EnvironmentID     string `json:"environmentId"`
			ApplicationID     string `json:"applicationId"`
		}{"kuberploy.io/v1alpha1", "HelmReleaseDisabledReceipt", release.ID,
			release.Generation, target.ProjectID, target.EnvironmentID, target.ApplicationID})
		if err != nil {
			return ProtectedPayloadIntent{}, false, ErrInvalid
		}
		value.ContentDigest = digestBytes(value.Content)
	}
	value.IntentDigest, err = payloadIntentDigest(value)
	if err != nil || value.Validate() != nil {
		return ProtectedPayloadIntent{}, false, ErrInvalid
	}
	result, err := tx.Exec(ctx, `INSERT INTO helm_protected_payload_intents(
		id,release_revision_id,release_generation,project_id,environment_id,application_id,
		action,platform_binding_id,environment_binding_id,cluster_id,platform_target_ref,
		environment_target_ref,environment_revision,environment_generation,catalog_digest,
		planned_base_revision,path,precondition,expected_etag,content,content_digest,
		manifest_inventory_digest,manifest_resource_count,intent_digest,commit_trailer,
		publisher_contract,publisher_config_digest,message,state,next_attempt_at,attempts,
		consecutive_failures,last_failure_code,lease_epoch,created_at,updated_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,
		'create-if-absent','',$18,$19,$20,$21,$22,$23,$24,$25,$26,'pending',$27,0,0,'',0,$27,$27)
		ON CONFLICT DO NOTHING`, value.ID, value.ReleaseRevisionID, value.ReleaseGeneration,
		value.Target.ProjectID, value.Target.EnvironmentID, value.Target.ApplicationID,
		value.Action, value.Binding.PlatformBindingID, value.Binding.EnvironmentBindingID,
		value.Binding.ClusterID, value.Binding.PlatformTargetRef,
		value.Binding.EnvironmentTargetRef, value.Binding.EnvironmentRevision,
		value.Binding.EnvironmentGeneration, value.Binding.CatalogDigest,
		value.Binding.PlannedBaseRevision, value.Path, value.Content, value.ContentDigest,
		nullableProtectedDigest(value.InventoryDigest), nullableProtectedCount(value.ResourceCount),
		value.IntentDigest, value.CommitTrailer, value.Publisher.Contract,
		value.Publisher.ConfigDigest, value.Message, value.CreatedAt)
	if err != nil {
		return ProtectedPayloadIntent{}, false, classifyPostgres(err)
	}
	created := result.RowsAffected() == 1
	if !created {
		existing, getErr := scanProtectedPayload(tx.QueryRow(ctx, `SELECT `+protectedPayloadColumns+`
			FROM helm_protected_payload_intents WHERE release_revision_id=$1`, release.ID))
		if getErr != nil || !equalProtectedPayloadIdentity(existing, value) {
			if getErr != nil {
				return ProtectedPayloadIntent{}, false, getErr
			}
			return ProtectedPayloadIntent{}, false, ErrConflict
		}
		value = existing
	}
	if err = tx.Commit(ctx); err != nil {
		return ProtectedPayloadIntent{}, false, classifyPostgres(err)
	}
	return value, !created, nil
}

func (s *PostgresProtectedPublicationStore) CreateApplicationForPayload(ctx context.Context,
	intentID, payloadID string, runtime ProtectedApplicationRuntime,
	publisher ProtectedPublisherIdentity, now time.Time) (ProtectedApplicationIntent, bool, error) {
	if !uuidRE.MatchString(intentID) || !uuidRE.MatchString(payloadID) || runtime.Validate() != nil ||
		publisher.Validate() != nil || now.IsZero() {
		return ProtectedApplicationIntent{}, false, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ProtectedApplicationIntent{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	payload, err := scanProtectedPayload(tx.QueryRow(ctx, `SELECT `+protectedPayloadColumns+`
		FROM helm_protected_payload_intents WHERE id=$1 FOR KEY SHARE`, payloadID))
	if err != nil || payload.State != ProtectedVerified {
		if err != nil {
			return ProtectedApplicationIntent{}, false, err
		}
		return ProtectedApplicationIntent{}, false, ErrConflict
	}
	release, err := scanReleaseRevision(tx.QueryRow(ctx, releaseRevisionSelect+`
		WHERE id=$1 FOR KEY SHARE`, payload.ReleaseRevisionID))
	if err != nil || release.Target != payload.Target || now.Before(payload.UpdatedAt) {
		if err != nil {
			return ProtectedApplicationIntent{}, false, classifyPostgres(err)
		}
		return ProtectedApplicationIntent{}, false, ErrConflict
	}
	var headRevisionID string
	err = tx.QueryRow(ctx, `SELECT revision_id::text FROM helm_release_heads
		WHERE project_id=$1 AND environment_id=$2 AND application_id=$3 FOR KEY SHARE`,
		release.Target.ProjectID, release.Target.EnvironmentID,
		release.Target.ApplicationID).Scan(&headRevisionID)
	if err != nil || headRevisionID != release.ID {
		if err != nil {
			return ProtectedApplicationIntent{}, false, classifyPostgres(err)
		}
		return ProtectedApplicationIntent{}, false, ErrConflict
	}
	var repositoryOwner, repositoryName, platformHead, destinationNamespace, argoProject string
	err = tx.QueryRow(ctx, `SELECT platform.repository_owner,platform.repository_name,
		platform.target_head_revision,environment.namespace,environment.argo_project
		FROM git_repository_bindings platform CROSS JOIN environments environment
		WHERE platform.id=$1 AND environment.id=$2 AND environment.project_id=$3
		FOR KEY SHARE OF platform,environment`, payload.Binding.PlatformBindingID,
		payload.Target.EnvironmentID, payload.Target.ProjectID).Scan(&repositoryOwner,
		&repositoryName, &platformHead, &destinationNamespace, &argoProject)
	if err != nil || !gitCommitRE.MatchString(platformHead) {
		if err != nil {
			return ProtectedApplicationIntent{}, false, classifyPostgres(err)
		}
		return ProtectedApplicationIntent{}, false, ErrConflict
	}
	binding := payload.Binding
	binding.PlannedBaseRevision = platformHead
	value := ProtectedApplicationIntent{
		ID: intentID, ReleaseRevisionID: release.ID, PayloadIntentID: payload.ID,
		ReleaseGeneration: release.Generation, Target: release.Target, Binding: binding,
		PayloadRevision: payload.CommittedRevision, PayloadPath: payload.Path,
		ApplicationPath: protectedApplicationPath(binding.ClusterID, release.Target.EnvironmentID,
			release.Target.ApplicationID), Publisher: publisher, State: ProtectedPending,
		NextAttemptAt: now.UTC(), CreatedAt: now.UTC(), UpdatedAt: now.UTC(),
		CommitTrailer: "Kuberploy-Helm-Application-Intent: " + intentID,
		Message:       "Publish protected Helm Application " + release.ID,
	}
	if release.DesiredEnabled {
		value.Action = ProtectedApplicationPublish
		value.SourceDirectory = protectedSourceDirectory(binding.ClusterID,
			release.Target.EnvironmentID, release.Target.ApplicationID, release.ID)
		if release.BaseApplicationIntentID == "" {
			value.Operation, value.Precondition = "create", "create-if-absent"
		} else {
			value.Operation, value.Precondition = "update", "match-etag"
			value.ExpectedETag, err = baseApplicationETag(ctx, tx,
				release.BaseApplicationIntentID, value.ApplicationPath)
			if err != nil {
				return ProtectedApplicationIntent{}, false, err
			}
		}
		value.Content, err = renderProtectedArgoApplication(value.ID, release, payload, runtime,
			repositoryOwner, repositoryName, destinationNamespace, argoProject)
		if err != nil {
			return ProtectedApplicationIntent{}, false, err
		}
		value.ContentDigest = digestBytes(value.Content)
	} else {
		value.Action, value.Operation, value.Precondition = ProtectedApplicationDelete, "delete", "match-etag"
		value.Message = "Delete protected Helm Application " + release.ID
		value.ExpectedETag, err = baseApplicationETag(ctx, tx,
			release.BaseApplicationIntentID, value.ApplicationPath)
		if err != nil {
			return ProtectedApplicationIntent{}, false, err
		}
	}
	value.IntentDigest, err = applicationIntentDigest(value)
	if err != nil || value.Validate() != nil {
		return ProtectedApplicationIntent{}, false, ErrInvalid
	}
	result, err := tx.Exec(ctx, `INSERT INTO helm_protected_application_intents(
		id,release_revision_id,payload_intent_id,release_generation,project_id,environment_id,
		application_id,action,platform_binding_id,environment_binding_id,cluster_id,
		platform_target_ref,environment_target_ref,environment_revision,environment_generation,
		catalog_digest,planned_base_revision,payload_revision,payload_path,source_directory,
		application_path,operation,precondition,expected_etag,content,content_digest,intent_digest,
		commit_trailer,publisher_contract,publisher_config_digest,message,state,next_attempt_at,
		attempts,consecutive_failures,last_failure_code,lease_epoch,created_at,updated_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,
		$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,'pending',$32,0,0,'',0,$32,$32)
		ON CONFLICT DO NOTHING`, value.ID, value.ReleaseRevisionID, value.PayloadIntentID,
		value.ReleaseGeneration, value.Target.ProjectID, value.Target.EnvironmentID,
		value.Target.ApplicationID, value.Action, value.Binding.PlatformBindingID,
		value.Binding.EnvironmentBindingID, value.Binding.ClusterID,
		value.Binding.PlatformTargetRef, value.Binding.EnvironmentTargetRef,
		value.Binding.EnvironmentRevision, value.Binding.EnvironmentGeneration,
		value.Binding.CatalogDigest, value.Binding.PlannedBaseRevision, value.PayloadRevision,
		value.PayloadPath, value.SourceDirectory, value.ApplicationPath, value.Operation,
		value.Precondition, value.ExpectedETag, value.Content, value.ContentDigest,
		value.IntentDigest, value.CommitTrailer, value.Publisher.Contract,
		value.Publisher.ConfigDigest, value.Message, value.CreatedAt)
	if err != nil {
		return ProtectedApplicationIntent{}, false, classifyPostgres(err)
	}
	created := result.RowsAffected() == 1
	if !created {
		existing, getErr := scanProtectedApplication(tx.QueryRow(ctx, `SELECT `+protectedApplicationColumns+`
			FROM helm_protected_application_intents WHERE payload_intent_id=$1`, payload.ID))
		if getErr != nil || !equalProtectedApplicationIdentity(existing, value) {
			if getErr != nil {
				return ProtectedApplicationIntent{}, false, getErr
			}
			return ProtectedApplicationIntent{}, false, ErrConflict
		}
		value = existing
	}
	if err = tx.Commit(ctx); err != nil {
		return ProtectedApplicationIntent{}, false, classifyPostgres(err)
	}
	return value, !created, nil
}

func baseApplicationETag(ctx context.Context, tx pgx.Tx, intentID, applicationPath string) (string, error) {
	if !uuidRE.MatchString(intentID) || !validProtectedApplicationPath(applicationPath) {
		return "", ErrInvalid
	}
	var digest string
	err := tx.QueryRow(ctx, `SELECT content_digest FROM helm_protected_application_intents
		WHERE id=$1 AND state='verified' AND action='publish' AND application_path=$2
		FOR KEY SHARE`, intentID, applicationPath).Scan(&digest)
	if err != nil {
		return "", classifyPostgres(err)
	}
	etag := `"` + digest + `"`
	if !validProtectedETag(etag) {
		return "", ErrConflict
	}
	return etag, nil
}

func (s *PostgresProtectedPublicationStore) Payload(ctx context.Context, intentID string) (ProtectedPayloadIntent, error) {
	if !uuidRE.MatchString(intentID) {
		return ProtectedPayloadIntent{}, ErrInvalid
	}
	return scanProtectedPayload(s.pool.QueryRow(ctx, `SELECT `+protectedPayloadColumns+`
		FROM helm_protected_payload_intents WHERE id=$1`, intentID))
}

func (s *PostgresProtectedPublicationStore) Application(ctx context.Context, intentID string) (ProtectedApplicationIntent, error) {
	if !uuidRE.MatchString(intentID) {
		return ProtectedApplicationIntent{}, ErrInvalid
	}
	return scanProtectedApplication(s.pool.QueryRow(ctx, `SELECT `+protectedApplicationColumns+`
		FROM helm_protected_application_intents WHERE id=$1`, intentID))
}

func (s *PostgresProtectedPublicationStore) ClaimPayload(ctx context.Context, owner string,
	publisher ProtectedPublisherIdentity, now time.Time, duration time.Duration) (ProtectedPayloadIntent, ProtectedIntentLease, error) {
	id, err := s.claimProtected(ctx, protectedPayloadTable, owner, publisher, now, duration)
	if err != nil {
		return ProtectedPayloadIntent{}, ProtectedIntentLease{}, err
	}
	value, err := s.Payload(ctx, id)
	if err != nil {
		return ProtectedPayloadIntent{}, ProtectedIntentLease{}, err
	}
	lease := payloadLease(value)
	return value, lease, lease.Validate()
}

func (s *PostgresProtectedPublicationStore) ClaimApplication(ctx context.Context, owner string,
	publisher ProtectedPublisherIdentity, now time.Time, duration time.Duration) (ProtectedApplicationIntent, ProtectedIntentLease, error) {
	id, err := s.claimProtected(ctx, protectedApplicationTable, owner, publisher, now, duration)
	if err != nil {
		return ProtectedApplicationIntent{}, ProtectedIntentLease{}, err
	}
	value, err := s.Application(ctx, id)
	if err != nil {
		return ProtectedApplicationIntent{}, ProtectedIntentLease{}, err
	}
	lease := applicationLease(value)
	return value, lease, lease.Validate()
}

func (s *PostgresProtectedPublicationStore) claimProtected(ctx context.Context, table, owner string,
	publisher ProtectedPublisherIdentity, now time.Time, duration time.Duration) (string, error) {
	if !validProtectedTable(table) || !workerIDRE.MatchString(owner) || publisher.Validate() != nil ||
		now.IsZero() || !validProtectedLeaseDuration(duration) {
		return "", ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// Only pristine, never-attempted work is retired when its projection snapshot
	// is stale. Previously leased work may represent an unacknowledged Git push
	// and remains recoverable by its own trailer and path digest.
	_, err = tx.Exec(ctx, `UPDATE `+table+` candidate SET state='superseded',
		completed_at=$1,updated_at=$1,consecutive_failures=1,
		last_failure_code='projection-superseded'
		WHERE candidate.state='pending' AND candidate.lease_epoch=0
		AND candidate.publisher_contract=$2 AND candidate.publisher_config_digest=$3
		AND NOT (`+freshProtectedProjectionSQL("candidate")+`)`, now.UTC(),
		publisher.Contract, publisher.ConfigDigest)
	if err != nil {
		return "", classifyPostgres(err)
	}
	var intentID string
	err = tx.QueryRow(ctx, `SELECT candidate.id::text FROM `+table+` candidate
		WHERE candidate.state IN ('pending','claimed','git-committed')
		AND candidate.next_attempt_at<=$1
		AND (candidate.lease_owner IS NULL OR candidate.lease_until<=$1)
		AND candidate.publisher_contract=$2 AND candidate.publisher_config_digest=$3
		AND (candidate.lease_epoch>0 OR (`+freshProtectedProjectionSQL("candidate")+`))
		AND NOT EXISTS(SELECT 1 FROM helm_protected_payload_intents held
			WHERE held.platform_binding_id=candidate.platform_binding_id
			AND (`+protectedLaneExclusion(table, protectedPayloadTable)+`)
			AND held.lease_owner IS NOT NULL AND held.lease_until>$1)
		AND NOT EXISTS(SELECT 1 FROM helm_protected_application_intents held
			WHERE held.platform_binding_id=candidate.platform_binding_id
			AND (`+protectedLaneExclusion(table, protectedApplicationTable)+`)
			AND held.lease_owner IS NOT NULL AND held.lease_until>$1)
		ORDER BY candidate.next_attempt_at,candidate.created_at,candidate.id
		FOR UPDATE OF candidate SKIP LOCKED LIMIT 1`, now.UTC(), publisher.Contract,
		publisher.ConfigDigest).Scan(&intentID)
	if err != nil {
		classified := classifyPostgres(err)
		if errors.Is(classified, ErrNotFound) {
			if commitErr := tx.Commit(ctx); commitErr != nil {
				return "", classifyPostgres(commitErr)
			}
		}
		return "", classified
	}
	result, err := tx.Exec(ctx, `UPDATE `+table+` SET
		state=CASE WHEN state='pending' THEN 'claimed' ELSE state END,
		lease_owner=$2,lease_epoch=lease_epoch+1,lease_until=$3,
		attempts=LEAST(attempts+1,30),updated_at=$1
		WHERE id=$4 AND (lease_owner IS NULL OR lease_until<=$1)
		AND state IN ('pending','claimed','git-committed')`, now.UTC(), owner,
		now.UTC().Add(duration), intentID)
	if err != nil || result.RowsAffected() != 1 {
		if err != nil {
			return "", classifyPostgres(err)
		}
		return "", ErrConflict
	}
	if err = tx.Commit(ctx); err != nil {
		return "", classifyPostgres(err)
	}
	return intentID, nil
}

func freshProtectedProjectionSQL(alias string) string {
	if alias != "candidate" {
		panic("closed SQL alias")
	}
	return `EXISTS(SELECT 1 FROM git_repository_bindings platform
		JOIN git_repository_bindings environment ON environment.id=` + alias + `.environment_binding_id
		JOIN git_projection_generations generation ON generation.binding_id=environment.id
			AND generation.generation=` + alias + `.environment_generation
		WHERE platform.id=` + alias + `.platform_binding_id AND platform.kind='platform'
		AND platform.credential_mode='github-app' AND platform.cluster_id=` + alias + `.cluster_id
		AND platform.target_ref=` + alias + `.platform_target_ref
		AND platform.target_head_revision IS NOT NULL
		AND platform.state IN ('ready','indexing') AND environment.kind='environment'
		AND environment.project_id=` + alias + `.project_id
		AND environment.environment_id=` + alias + `.environment_id
		AND environment.target_ref=` + alias + `.environment_target_ref
		AND environment.target_head_revision=` + alias + `.environment_revision
		AND environment.indexed_revision=` + alias + `.environment_revision
		AND environment.projection_generation=` + alias + `.environment_generation
		AND environment.state='ready' AND generation.head_revision=` + alias + `.environment_revision
		AND generation.state='active' AND NOT EXISTS(
			SELECT 1 FROM git_projected_documents invalid
			WHERE invalid.binding_id=environment.id
			AND invalid.generation=` + alias + `.environment_generation AND NOT invalid.valid))`
}

func protectedLaneExclusion(candidateTable, heldTable string) string {
	if !validProtectedTable(candidateTable) || !validProtectedTable(heldTable) {
		panic("closed protected publication table")
	}
	if candidateTable == heldTable {
		return "held.id<>candidate.id"
	}
	return "TRUE"
}

func (s *PostgresProtectedPublicationStore) HeartbeatPayload(ctx context.Context, lease ProtectedIntentLease,
	now time.Time, duration time.Duration) (ProtectedIntentLease, error) {
	if err := s.heartbeatProtected(ctx, protectedPayloadTable, lease, now, duration); err != nil {
		return ProtectedIntentLease{}, err
	}
	value, err := s.Payload(ctx, lease.IntentID)
	if err != nil {
		return ProtectedIntentLease{}, err
	}
	return payloadLease(value), nil
}

func (s *PostgresProtectedPublicationStore) HeartbeatApplication(ctx context.Context, lease ProtectedIntentLease,
	now time.Time, duration time.Duration) (ProtectedIntentLease, error) {
	if err := s.heartbeatProtected(ctx, protectedApplicationTable, lease, now, duration); err != nil {
		return ProtectedIntentLease{}, err
	}
	value, err := s.Application(ctx, lease.IntentID)
	if err != nil {
		return ProtectedIntentLease{}, err
	}
	return applicationLease(value), nil
}

func (s *PostgresProtectedPublicationStore) heartbeatProtected(ctx context.Context, table string,
	lease ProtectedIntentLease, now time.Time, duration time.Duration) error {
	if !validProtectedTable(table) || lease.Validate() != nil || now.IsZero() ||
		!validProtectedLeaseDuration(duration) {
		return ErrInvalid
	}
	result, err := s.pool.Exec(ctx, `UPDATE `+table+` SET lease_until=$6,updated_at=$5
		WHERE id=$1 AND lease_owner=$2 AND lease_epoch=$3 AND publisher_contract=$4
		AND publisher_config_digest=$7 AND lease_until>$5
		AND state IN ('claimed','git-committed')`, lease.IntentID, lease.Owner, lease.Epoch,
		lease.Publisher.Contract, now.UTC(), now.UTC().Add(duration), lease.Publisher.ConfigDigest)
	if err != nil {
		return classifyPostgres(err)
	}
	if result.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (s *PostgresProtectedPublicationStore) BindPayloadWriteBase(ctx context.Context,
	lease ProtectedIntentLease, revision string, observedAt, now time.Time) (ProtectedPayloadIntent, error) {
	if err := s.bindProtectedWriteBase(ctx, protectedPayloadTable, lease, revision, observedAt, now); err != nil {
		current, getErr := s.Payload(ctx, lease.IntentID)
		if getErr == nil && activePayloadLease(current, lease, now) && current.State == ProtectedClaimed &&
			current.WriteBaseRevision == revision && current.WriteBaseObservedAt != nil && current.WriteBaseObservedAt.Equal(observedAt) {
			return current, nil
		}
		return ProtectedPayloadIntent{}, protectedPayloadWriteMiss(current, getErr, lease, now, err)
	}
	return s.Payload(ctx, lease.IntentID)
}

func (s *PostgresProtectedPublicationStore) BindApplicationWriteBase(ctx context.Context,
	lease ProtectedIntentLease, revision string, observedAt, now time.Time) (ProtectedApplicationIntent, error) {
	if err := s.bindProtectedWriteBase(ctx, protectedApplicationTable, lease, revision, observedAt, now); err != nil {
		current, getErr := s.Application(ctx, lease.IntentID)
		if getErr == nil && activeApplicationLease(current, lease, now) && current.State == ProtectedClaimed &&
			current.WriteBaseRevision == revision && current.WriteBaseObservedAt != nil && current.WriteBaseObservedAt.Equal(observedAt) {
			return current, nil
		}
		return ProtectedApplicationIntent{}, protectedApplicationWriteMiss(current, getErr, lease, now, err)
	}
	return s.Application(ctx, lease.IntentID)
}

func (s *PostgresProtectedPublicationStore) bindProtectedWriteBase(ctx context.Context, table string,
	lease ProtectedIntentLease, revision string, observedAt, now time.Time) error {
	if !validProtectedTable(table) || lease.Validate() != nil || !gitCommitRE.MatchString(revision) ||
		observedAt.IsZero() || now.IsZero() || observedAt.After(now) {
		return ErrInvalid
	}
	result, err := s.pool.Exec(ctx, `UPDATE `+table+` SET write_base_revision=$6,
		write_base_observed_at=$7,updated_at=$8 WHERE id=$1 AND lease_owner=$2
		AND lease_epoch=$3 AND publisher_contract=$4 AND publisher_config_digest=$5
		AND lease_until>$8 AND state='claimed' AND write_base_revision=''
		AND created_at<=$7 AND updated_at<=$8`, lease.IntentID, lease.Owner, lease.Epoch,
		lease.Publisher.Contract, lease.Publisher.ConfigDigest, revision, observedAt.UTC(), now.UTC())
	if err != nil {
		return classifyPostgres(err)
	}
	if result.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

func (s *PostgresProtectedPublicationStore) MarkPayloadCommitted(ctx context.Context,
	lease ProtectedIntentLease, revision, parent string, now time.Time) (ProtectedPayloadIntent, error) {
	if err := s.markProtectedCommitted(ctx, protectedPayloadTable, lease, revision, parent, now); err != nil {
		current, getErr := s.Payload(ctx, lease.IntentID)
		if getErr == nil && activePayloadLease(current, lease, now) && current.State == ProtectedGitCommitted &&
			current.CommittedRevision == revision && current.CommittedParentRevision == parent {
			return current, nil
		}
		return ProtectedPayloadIntent{}, protectedPayloadWriteMiss(current, getErr, lease, now, err)
	}
	return s.Payload(ctx, lease.IntentID)
}

func (s *PostgresProtectedPublicationStore) MarkApplicationCommitted(ctx context.Context,
	lease ProtectedIntentLease, revision, parent string, now time.Time) (ProtectedApplicationIntent, error) {
	if err := s.markProtectedCommitted(ctx, protectedApplicationTable, lease, revision, parent, now); err != nil {
		current, getErr := s.Application(ctx, lease.IntentID)
		if getErr == nil && activeApplicationLease(current, lease, now) && current.State == ProtectedGitCommitted &&
			current.CommittedRevision == revision && current.CommittedParentRevision == parent {
			return current, nil
		}
		return ProtectedApplicationIntent{}, protectedApplicationWriteMiss(current, getErr, lease, now, err)
	}
	return s.Application(ctx, lease.IntentID)
}

func (s *PostgresProtectedPublicationStore) markProtectedCommitted(ctx context.Context, table string,
	lease ProtectedIntentLease, revision, parent string, now time.Time) error {
	if !validProtectedTable(table) || lease.Validate() != nil || !gitCommitRE.MatchString(revision) ||
		!gitCommitRE.MatchString(parent) || revision == parent || now.IsZero() {
		return ErrInvalid
	}
	result, err := s.pool.Exec(ctx, `UPDATE `+table+` SET state='git-committed',
		committed_revision=$6,committed_parent_revision=$7,committed_at=$8,updated_at=$8
		WHERE id=$1 AND lease_owner=$2 AND lease_epoch=$3 AND publisher_contract=$4
		AND publisher_config_digest=$5 AND lease_until>$8 AND state='claimed'
		AND write_base_revision=$7 AND write_base_observed_at IS NOT NULL
		AND updated_at<=$8`, lease.IntentID, lease.Owner, lease.Epoch,
		lease.Publisher.Contract, lease.Publisher.ConfigDigest, revision, parent, now.UTC())
	if err != nil {
		return classifyPostgres(err)
	}
	if result.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

func (s *PostgresProtectedPublicationStore) VerifyPayload(ctx context.Context,
	lease ProtectedIntentLease, revision, pathDigest, providerRequest string,
	now time.Time) (ProtectedPayloadIntent, error) {
	if err := s.verifyProtected(ctx, protectedPayloadTable, lease, revision, pathDigest,
		providerRequest, now); err != nil {
		current, getErr := s.Payload(ctx, lease.IntentID)
		if getErr == nil && current.State == ProtectedVerified && current.CommittedRevision == revision &&
			current.VerifiedPathDigest == pathDigest && current.ProviderRequest == providerRequest {
			return current, nil
		}
		return ProtectedPayloadIntent{}, protectedPayloadWriteMiss(current, getErr, lease, now, err)
	}
	return s.Payload(ctx, lease.IntentID)
}

func (s *PostgresProtectedPublicationStore) VerifyApplication(ctx context.Context,
	lease ProtectedIntentLease, revision, pathDigest, providerRequest string,
	now time.Time) (ProtectedApplicationIntent, error) {
	if err := s.verifyProtected(ctx, protectedApplicationTable, lease, revision, pathDigest,
		providerRequest, now); err != nil {
		current, getErr := s.Application(ctx, lease.IntentID)
		if getErr == nil && current.State == ProtectedVerified && current.CommittedRevision == revision &&
			current.VerifiedPathDigest == pathDigest && current.ProviderRequest == providerRequest {
			return current, nil
		}
		return ProtectedApplicationIntent{}, protectedApplicationWriteMiss(current, getErr, lease, now, err)
	}
	return s.Application(ctx, lease.IntentID)
}

func (s *PostgresProtectedPublicationStore) verifyProtected(ctx context.Context, table string,
	lease ProtectedIntentLease, revision, pathDigest, providerRequest string, now time.Time) error {
	if !validProtectedTable(table) || lease.Validate() != nil || !gitCommitRE.MatchString(revision) ||
		len(providerRequest) < 1 || len(providerRequest) > 256 || containsControl(providerRequest) || now.IsZero() ||
		(pathDigest != "" && !validDigest(pathDigest)) {
		return ErrInvalid
	}
	result, err := s.pool.Exec(ctx, `UPDATE `+table+` SET state='verified',verified_at=$8,
		verified_path_digest=$7,provider_request=$6,completed_at=$8,lease_owner=NULL,
		lease_until=NULL,updated_at=$8 WHERE id=$1 AND lease_owner=$2 AND lease_epoch=$3
		AND publisher_contract=$4 AND publisher_config_digest=$5 AND lease_until>$8
		AND state='git-committed' AND committed_revision=$9 AND committed_at<=$8
		AND content_digest=$7`, lease.IntentID, lease.Owner, lease.Epoch,
		lease.Publisher.Contract, lease.Publisher.ConfigDigest, providerRequest,
		pathDigest, now.UTC(), revision)
	if err != nil {
		return classifyPostgres(err)
	}
	if result.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

func (s *PostgresProtectedPublicationStore) RetryPayload(ctx context.Context,
	lease ProtectedIntentLease, code string, nextAttemptAt, now time.Time) (ProtectedPayloadIntent, error) {
	if err := s.retryProtected(ctx, protectedPayloadTable, lease, code, nextAttemptAt, now); err != nil {
		return ProtectedPayloadIntent{}, err
	}
	return s.Payload(ctx, lease.IntentID)
}

func (s *PostgresProtectedPublicationStore) RetryApplication(ctx context.Context,
	lease ProtectedIntentLease, code string, nextAttemptAt, now time.Time) (ProtectedApplicationIntent, error) {
	if err := s.retryProtected(ctx, protectedApplicationTable, lease, code, nextAttemptAt, now); err != nil {
		return ProtectedApplicationIntent{}, err
	}
	return s.Application(ctx, lease.IntentID)
}

func (s *PostgresProtectedPublicationStore) retryProtected(ctx context.Context, table string,
	lease ProtectedIntentLease, code string, nextAttemptAt, now time.Time) error {
	if !validProtectedTable(table) || lease.Validate() != nil || !failureCodeRE.MatchString(code) ||
		now.IsZero() || nextAttemptAt.Before(now) || nextAttemptAt.After(now.Add(maximumProtectedRetry)) {
		return ErrInvalid
	}
	result, err := s.pool.Exec(ctx, `UPDATE `+table+` SET
		state=CASE WHEN state='claimed' THEN 'pending' ELSE state END,next_attempt_at=$6,
		consecutive_failures=LEAST(consecutive_failures+1,30),last_failure_code=$7,
		lease_owner=CASE WHEN state='git-committed' THEN lease_owner ELSE NULL END,
		lease_until=CASE WHEN state='git-committed' THEN $8+interval '1 microsecond' ELSE NULL END,
		updated_at=$8 WHERE id=$1 AND lease_owner=$2 AND lease_epoch=$3
		AND publisher_contract=$4 AND publisher_config_digest=$5 AND lease_until>$8
		AND state IN ('claimed','git-committed')`, lease.IntentID, lease.Owner, lease.Epoch,
		lease.Publisher.Contract, lease.Publisher.ConfigDigest, nextAttemptAt.UTC(), code, now.UTC())
	if err != nil {
		return classifyPostgres(err)
	}
	if result.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (s *PostgresProtectedPublicationStore) FailPayload(ctx context.Context,
	lease ProtectedIntentLease, code string, now time.Time) (ProtectedPayloadIntent, error) {
	if err := s.failProtected(ctx, protectedPayloadTable, lease, code, now); err != nil {
		return ProtectedPayloadIntent{}, err
	}
	return s.Payload(ctx, lease.IntentID)
}

func (s *PostgresProtectedPublicationStore) FailApplication(ctx context.Context,
	lease ProtectedIntentLease, code string, now time.Time) (ProtectedApplicationIntent, error) {
	if err := s.failProtected(ctx, protectedApplicationTable, lease, code, now); err != nil {
		return ProtectedApplicationIntent{}, err
	}
	return s.Application(ctx, lease.IntentID)
}

func (s *PostgresProtectedPublicationStore) failProtected(ctx context.Context, table string,
	lease ProtectedIntentLease, code string, now time.Time) error {
	if !validProtectedTable(table) || lease.Validate() != nil || !failureCodeRE.MatchString(code) || now.IsZero() {
		return ErrInvalid
	}
	result, err := s.pool.Exec(ctx, `UPDATE `+table+` SET state='failed',
		consecutive_failures=LEAST(consecutive_failures+1,30),last_failure_code=$6,
		lease_owner=NULL,lease_until=NULL,completed_at=$7,updated_at=$7
		WHERE id=$1 AND lease_owner=$2 AND lease_epoch=$3 AND publisher_contract=$4
		AND publisher_config_digest=$5 AND lease_until>$7 AND state='claimed'`,
		lease.IntentID, lease.Owner, lease.Epoch, lease.Publisher.Contract,
		lease.Publisher.ConfigDigest, code, now.UTC())
	if err != nil {
		return classifyPostgres(err)
	}
	if result.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (s *PostgresProtectedPublicationStore) PutPublisherReadiness(ctx context.Context,
	readiness ProtectedPublisherReadiness) error {
	if readiness.Validate() != nil {
		return ErrInvalid
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO runtime_readiness(runtime_kind,scope_key,worker_id,worker_epoch,
		contract_version,config_digest,identity,observation,started_at,observed_at,lease_until,updated_at)
		VALUES('helm-protected-publisher','global',$1,$2,$3,$5,jsonb_build_object('policyVersion',$4::text),
		'{}'::jsonb,$6,$7,$8,$7)
		ON CONFLICT(runtime_kind,scope_key,worker_id) DO UPDATE SET worker_epoch=EXCLUDED.worker_epoch,
		contract_version=EXCLUDED.contract_version,identity=EXCLUDED.identity,
		config_digest=EXCLUDED.config_digest,started_at=EXCLUDED.started_at,
		observed_at=EXCLUDED.observed_at,lease_until=EXCLUDED.lease_until,updated_at=EXCLUDED.updated_at`, readiness.WorkerID,
		readiness.WorkerEpoch, readiness.Publisher.Contract, readiness.Publisher.PolicyVersion,
		readiness.Publisher.ConfigDigest, readiness.StartedAt.UTC(), readiness.ObservedAt.UTC(),
		readiness.LeaseUntil.UTC())
	return classifyPostgres(err)
}

func (s *PostgresProtectedPublicationStore) PublisherReady(ctx context.Context,
	publisher ProtectedPublisherIdentity, now time.Time) (bool, error) {
	if publisher.Validate() != nil || now.IsZero() {
		return false, ErrInvalid
	}
	var ready bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM runtime_readiness
		WHERE runtime_kind='helm-protected-publisher' AND scope_key='global' AND contract_version=$1
		AND identity->>'policyVersion'=$2 AND config_digest=$3 AND lease_until>$4)`, publisher.Contract,
		publisher.PolicyVersion, publisher.ConfigDigest, now.UTC()).Scan(&ready)
	return ready, classifyPostgres(err)
}

func payloadIntentDigest(value ProtectedPayloadIntent) (string, error) {
	return protectedIntentDigest(struct {
		Contract          string                     `json:"contract"`
		ID                string                     `json:"id"`
		ReleaseRevisionID string                     `json:"releaseRevisionId"`
		Generation        int64                      `json:"generation"`
		Target            ReleaseTarget              `json:"target"`
		Action            ProtectedPayloadAction     `json:"action"`
		Binding           ProtectedBindingSnapshot   `json:"binding"`
		Path              string                     `json:"path"`
		ContentDigest     string                     `json:"contentDigest"`
		InventoryDigest   string                     `json:"inventoryDigest,omitempty"`
		ResourceCount     int                        `json:"resourceCount,omitempty"`
		Publisher         ProtectedPublisherIdentity `json:"publisher"`
	}{"helm-protected-payload-intent.v1", value.ID, value.ReleaseRevisionID,
		value.ReleaseGeneration, value.Target, value.Action, value.Binding, value.Path,
		value.ContentDigest, value.InventoryDigest, value.ResourceCount, value.Publisher})
}

func applicationIntentDigest(value ProtectedApplicationIntent) (string, error) {
	return protectedIntentDigest(struct {
		Contract          string                     `json:"contract"`
		ID                string                     `json:"id"`
		ReleaseRevisionID string                     `json:"releaseRevisionId"`
		PayloadIntentID   string                     `json:"payloadIntentId"`
		Generation        int64                      `json:"generation"`
		Target            ReleaseTarget              `json:"target"`
		Action            ProtectedApplicationAction `json:"action"`
		Binding           ProtectedBindingSnapshot   `json:"binding"`
		PayloadRevision   string                     `json:"payloadRevision"`
		PayloadPath       string                     `json:"payloadPath"`
		SourceDirectory   string                     `json:"sourceDirectory,omitempty"`
		ApplicationPath   string                     `json:"applicationPath"`
		Operation         string                     `json:"operation"`
		Precondition      string                     `json:"precondition"`
		ExpectedETag      string                     `json:"expectedETag,omitempty"`
		ContentDigest     string                     `json:"contentDigest,omitempty"`
		Publisher         ProtectedPublisherIdentity `json:"publisher"`
	}{"helm-protected-application-intent.v1", value.ID, value.ReleaseRevisionID,
		value.PayloadIntentID, value.ReleaseGeneration, value.Target, value.Action,
		value.Binding, value.PayloadRevision, value.PayloadPath, value.SourceDirectory,
		value.ApplicationPath, value.Operation, value.Precondition, value.ExpectedETag,
		value.ContentDigest, value.Publisher})
}

func equalProtectedPayloadIdentity(left, right ProtectedPayloadIntent) bool {
	return left.ID == right.ID && left.ReleaseRevisionID == right.ReleaseRevisionID &&
		left.ReleaseGeneration == right.ReleaseGeneration && left.Target == right.Target &&
		left.Action == right.Action && left.Binding == right.Binding && left.Path == right.Path &&
		equalBytes(left.Content, right.Content) && left.ContentDigest == right.ContentDigest &&
		left.InventoryDigest == right.InventoryDigest && left.ResourceCount == right.ResourceCount &&
		left.IntentDigest == right.IntentDigest && left.CommitTrailer == right.CommitTrailer &&
		left.Publisher == right.Publisher && left.Message == right.Message
}

func equalProtectedApplicationIdentity(left, right ProtectedApplicationIntent) bool {
	return left.ID == right.ID && left.ReleaseRevisionID == right.ReleaseRevisionID &&
		left.PayloadIntentID == right.PayloadIntentID && left.ReleaseGeneration == right.ReleaseGeneration &&
		left.Target == right.Target && left.Action == right.Action && left.Binding == right.Binding &&
		left.PayloadRevision == right.PayloadRevision && left.PayloadPath == right.PayloadPath &&
		left.SourceDirectory == right.SourceDirectory && left.ApplicationPath == right.ApplicationPath &&
		left.Operation == right.Operation && left.Precondition == right.Precondition &&
		left.ExpectedETag == right.ExpectedETag && equalBytes(left.Content, right.Content) &&
		left.ContentDigest == right.ContentDigest && left.IntentDigest == right.IntentDigest &&
		left.CommitTrailer == right.CommitTrailer && left.Publisher == right.Publisher &&
		left.Message == right.Message
}

func payloadLease(value ProtectedPayloadIntent) ProtectedIntentLease {
	lease := ProtectedIntentLease{IntentID: value.ID, Owner: value.LeaseOwner,
		Epoch: value.LeaseEpoch, Publisher: value.Publisher}
	if value.LeaseUntil != nil {
		lease.Until = value.LeaseUntil.UTC()
	}
	return lease
}

func applicationLease(value ProtectedApplicationIntent) ProtectedIntentLease {
	lease := ProtectedIntentLease{IntentID: value.ID, Owner: value.LeaseOwner,
		Epoch: value.LeaseEpoch, Publisher: value.Publisher}
	if value.LeaseUntil != nil {
		lease.Until = value.LeaseUntil.UTC()
	}
	return lease
}

func activePayloadLease(value ProtectedPayloadIntent, lease ProtectedIntentLease, now time.Time) bool {
	return value.LeaseOwner == lease.Owner && value.LeaseEpoch == lease.Epoch &&
		value.Publisher == lease.Publisher && value.LeaseUntil != nil && value.LeaseUntil.After(now)
}

func activeApplicationLease(value ProtectedApplicationIntent, lease ProtectedIntentLease, now time.Time) bool {
	return value.LeaseOwner == lease.Owner && value.LeaseEpoch == lease.Epoch &&
		value.Publisher == lease.Publisher && value.LeaseUntil != nil && value.LeaseUntil.After(now)
}

func protectedPayloadWriteMiss(value ProtectedPayloadIntent, getErr error,
	lease ProtectedIntentLease, now time.Time, operationErr error) error {
	if getErr != nil {
		return getErr
	}
	if !activePayloadLease(value, lease, now) {
		return ErrLeaseLost
	}
	return operationErr
}

func protectedApplicationWriteMiss(value ProtectedApplicationIntent, getErr error,
	lease ProtectedIntentLease, now time.Time, operationErr error) error {
	if getErr != nil {
		return getErr
	}
	if !activeApplicationLease(value, lease, now) {
		return ErrLeaseLost
	}
	return operationErr
}

func validProtectedTable(table string) bool {
	return table == protectedPayloadTable || table == protectedApplicationTable
}

func validProtectedLeaseDuration(duration time.Duration) bool {
	return duration >= minimumProtectedLease && duration <= maximumProtectedLease
}

func nullableProtectedDigest(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableProtectedCount(value int) any {
	if value == 0 {
		return nil
	}
	return value
}

var _ ProtectedPublicationStore = (*PostgresProtectedPublicationStore)(nil)
