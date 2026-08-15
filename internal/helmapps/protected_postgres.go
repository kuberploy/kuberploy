package helmapps

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/argo"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/id"
)

const (
	protectedPayloadTable     = "public.helm_protected_payload_intents"
	protectedApplicationTable = "public.helm_protected_application_intents"
	minimumProtectedLease     = 15 * time.Second
	maximumProtectedLease     = 5 * time.Minute
	maximumProtectedRetry     = 24 * time.Hour
)

type PostgresProtectedPublicationStore struct {
	pool                   *pgxpool.Pool
	authority              ArgoMaterializationAuthority
	cascadeIdentity        *argo.DesiredStateRuntimeIdentity
	cascadeArgoObservation *argo.DesiredStateRuntimeWorkerObservation
}

func NewPostgresProtectedPublicationStoreWithCascade(pool *pgxpool.Pool,
	authority ArgoMaterializationAuthority,
	observation argo.DesiredStateRuntimeWorkerObservation) (*PostgresProtectedPublicationStore, error) {
	if authority.Validate() != nil || observation.Validate() != nil {
		return nil, ErrInvalid
	}
	result, err := NewPostgresProtectedPublicationStore(pool, authority)
	if err != nil {
		return nil, err
	}
	copyObservation := observation
	copyIdentity := observation.DesiredStateRuntimeIdentity
	result.cascadeIdentity = &copyIdentity
	result.cascadeArgoObservation = &copyObservation
	return result, nil
}

func scanCascadePlatformBinding(row rowScanner) (gitprojection.Binding, error) {
	var value gitprojection.Binding
	var target, indexed *string
	var targetAt, indexedAt *time.Time
	err := row.Scan(&value.ID, &value.Kind, &value.ScopeID, &value.ProjectID, &value.EnvironmentID,
		&value.ClusterID, &value.Repository.Provider, &value.Repository.InstallationID,
		&value.Repository.RepositoryID, &value.Repository.Owner, &value.Repository.Name,
		&value.TargetRef, &value.Prefix, &value.CredentialMode, &value.CredentialSecretName,
		&value.State, &target, &indexed, &value.ProjectionGeneration, &value.ParserVersion,
		&targetAt, &indexedAt, &value.CreatedAt, &value.UpdatedAt)
	if err != nil {
		return gitprojection.Binding{}, classifyPostgres(err)
	}
	if target != nil {
		value.TargetHeadRevision = *target
	}
	if indexed != nil {
		value.IndexedRevision = *indexed
	}
	if targetAt != nil {
		value.TargetHeadObservedAt = *targetAt
	}
	if indexedAt != nil {
		value.IndexedAt = *indexedAt
	}
	if value.Validate() != nil {
		return gitprojection.Binding{}, ErrConflict
	}
	return value, nil
}

func NewPostgresProtectedPublicationStore(pool *pgxpool.Pool,
	authority ...ArgoMaterializationAuthority) (*PostgresProtectedPublicationStore, error) {
	if pool == nil {
		return nil, ErrInvalid
	}
	if len(authority) > 1 || len(authority) == 1 && authority[0].Validate() != nil {
		return nil, ErrInvalid
	}
	result := &PostgresProtectedPublicationStore{pool: pool}
	if len(authority) == 1 {
		result.authority = authority[0]
	}
	return result, nil
}

const protectedPayloadColumns = `id::text,release_revision_id::text,release_generation,
	project_id::text,environment_id::text,application_id::text,action,
	platform_binding_id::text,environment_binding_id::text,cluster_id::text,
	platform_target_ref,environment_target_ref,environment_revision,environment_generation,
	catalog_digest,planned_base_revision,path,content,content_digest,
	COALESCE(manifest_inventory_digest,''),COALESCE(manifest_resource_count,0),
	intent_digest,commit_trailer,publisher_contract,publisher_config_digest,
	original_publisher_config_digest,publisher_adoption_epoch,message,state,
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
		&value.Publisher.ConfigDigest, &value.OriginalPublisherConfigDigest,
		&value.PublisherAdoptionEpoch, &value.Message, &value.State, &value.NextAttemptAt,
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
	intent_digest,commit_trailer,publisher_contract,publisher_config_digest,
	original_publisher_config_digest,publisher_adoption_epoch,continuation_required,
	COALESCE(continuation_receipt_id::text,''),continuation_contract,cascade_required,
	COALESCE(cascade_receipt_id::text,''),cascade_contract,message,state,
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
		&value.OriginalPublisherConfigDigest, &value.PublisherAdoptionEpoch,
		&value.ContinuationRequired, &value.ContinuationReceiptID,
		&value.ContinuationContract, &value.CascadeRequired, &value.CascadeReceiptID,
		&value.CascadeContract, &value.Message, &value.State, &value.NextAttemptAt, &value.Attempts,
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
	type result struct {
		intent ProtectedPayloadIntent
		replay bool
	}
	value, err := retryProtectedTransaction(ctx, func() (result, error) {
		intent, replay, createErr := s.createPayloadForHeadOnce(ctx, intentID, target, binding,
			publisher, now)
		return result{intent: intent, replay: replay}, createErr
	})
	return value.intent, value.replay, err
}

func (s *PostgresProtectedPublicationStore) createPayloadForHeadOnce(ctx context.Context, intentID string,
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
	if _, err = ensurePublicationPrerequisite(ctx, tx, release, binding, s.authority, now); err != nil {
		return ProtectedPayloadIntent{}, false, err
	}
	value := ProtectedPayloadIntent{
		ID: intentID, ReleaseRevisionID: release.ID, ReleaseGeneration: release.Generation,
		Target: target, Binding: binding, Publisher: publisher,
		OriginalPublisherConfigDigest: publisher.ConfigDigest, State: ProtectedPending,
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
	result, err := tx.Exec(ctx, `INSERT INTO public.helm_protected_payload_intents(
		id,release_revision_id,release_generation,project_id,environment_id,application_id,
		action,platform_binding_id,environment_binding_id,cluster_id,platform_target_ref,
		environment_target_ref,environment_revision,environment_generation,catalog_digest,
		planned_base_revision,path,precondition,expected_etag,content,content_digest,
		manifest_inventory_digest,manifest_resource_count,intent_digest,commit_trailer,
		publisher_contract,publisher_config_digest,original_publisher_config_digest,
		publisher_adoption_epoch,message,state,next_attempt_at,attempts,
		consecutive_failures,last_failure_code,lease_epoch,prerequisite_receipt_id,
		prerequisite_contract,prerequisite_epoch,created_at,updated_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,
		'create-if-absent','',$18,$19,$20,$21,$22,$23,$24,$25,$25,0,$26,'pending',$27,0,0,'',0,
		$2,$28,0,$27,$27)
		ON CONFLICT DO NOTHING`, value.ID, value.ReleaseRevisionID, value.ReleaseGeneration,
		value.Target.ProjectID, value.Target.EnvironmentID, value.Target.ApplicationID,
		value.Action, value.Binding.PlatformBindingID, value.Binding.EnvironmentBindingID,
		value.Binding.ClusterID, value.Binding.PlatformTargetRef,
		value.Binding.EnvironmentTargetRef, value.Binding.EnvironmentRevision,
		value.Binding.EnvironmentGeneration, value.Binding.CatalogDigest,
		value.Binding.PlannedBaseRevision, value.Path, value.Content, value.ContentDigest,
		nullableProtectedDigest(value.InventoryDigest), nullableProtectedCount(value.ResourceCount),
		value.IntentDigest, value.CommitTrailer, value.Publisher.Contract,
		value.Publisher.ConfigDigest, value.Message, value.CreatedAt, protectedPrerequisiteContract)
	if err != nil {
		return ProtectedPayloadIntent{}, false, classifyPostgres(err)
	}
	created := result.RowsAffected() == 1
	if !created {
		existing, getErr := scanProtectedPayload(tx.QueryRow(ctx, `SELECT `+protectedPayloadColumns+`
			FROM public.helm_protected_payload_intents WHERE release_revision_id=$1`, release.ID))
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
	type result struct {
		intent ProtectedApplicationIntent
		replay bool
	}
	value, err := retryProtectedTransaction(ctx, func() (result, error) {
		intent, replay, createErr := s.createApplicationForPayloadOnce(ctx, intentID, payloadID,
			runtime, publisher, now)
		return result{intent: intent, replay: replay}, createErr
	})
	return value.intent, value.replay, err
}

func (s *PostgresProtectedPublicationStore) createApplicationForPayloadOnce(ctx context.Context,
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
		FROM public.helm_protected_payload_intents WHERE id=$1 FOR KEY SHARE`, payloadID))
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
		FROM public.git_repository_bindings platform CROSS JOIN public.environments environment
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
	// Legacy installations can already have a verified phase-one payload when
	// the prerequisite receipt migration lands. Create/replay the exact receipt
	// here as well so phase two recovers without granting imperative authority.
	if _, err = ensurePublicationPrerequisite(ctx, tx, release, binding, s.authority, now); err != nil {
		return ProtectedApplicationIntent{}, false, err
	}
	value := ProtectedApplicationIntent{
		ID: intentID, ReleaseRevisionID: release.ID, PayloadIntentID: payload.ID,
		ReleaseGeneration: release.Generation, Target: release.Target, Binding: binding,
		PayloadRevision: payload.CommittedRevision, PayloadPath: payload.Path,
		ApplicationPath: protectedApplicationPath(binding.ClusterID, release.Target.EnvironmentID,
			release.Target.ApplicationID), Publisher: publisher,
		OriginalPublisherConfigDigest: publisher.ConfigDigest, State: ProtectedPending,
		ContinuationRequired: true, ContinuationReceiptID: intentID,
		ContinuationContract: protectedContinuationContract,
		NextAttemptAt:        now.UTC(), CreatedAt: now.UTC(), UpdatedAt: now.UTC(),
		CommitTrailer: "Kuberploy-Helm-Application-Intent: " + intentID,
		Message:       "Publish protected Helm Application " + release.ID,
	}
	if release.DesiredEnabled {
		value.Action = ProtectedApplicationPublish
		value.SourceDirectory = protectedSourceDirectory(binding.ClusterID,
			release.Target.EnvironmentID, release.Target.ApplicationID, release.ID)
		recoveryCreate := false
		if release.BaseApplicationIntentID != "" {
			err = tx.QueryRow(ctx, `SELECT public.helm_application_cascade_recovery_create_is_authorized(
				$1,$2,$3)`, release.ID, release.BaseApplicationIntentID, value.ApplicationPath).
				Scan(&recoveryCreate)
			if err != nil {
				return ProtectedApplicationIntent{}, false, classifyPostgres(err)
			}
		}
		if release.BaseApplicationIntentID == "" || recoveryCreate {
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
		var adoptedDigest string
		err = tx.QueryRow(ctx, `SELECT cascade.id::text,cascade.adopted_content_digest
			FROM public.helm_application_cascade_preflights cascade
			WHERE cascade.payload_intent_id=$1
			  AND cascade.state='verified' AND public.helm_application_cascade_observation_is_exact(
			    cascade.id,$2,pg_catalog.clock_timestamp())
			LIMIT 1 FOR KEY SHARE OF cascade`,
			payload.ID, publisher.ConfigDigest).Scan(&value.CascadeReceiptID, &adoptedDigest)
		if err != nil || !validDigest(adoptedDigest) {
			if err != nil {
				return ProtectedApplicationIntent{}, false, classifyPostgres(err)
			}
			return ProtectedApplicationIntent{}, false, ErrConflict
		}
		value.CascadeRequired = true
		value.CascadeContract = protectedCascadeContract
		// pgx encodes a nil []byte as SQL NULL. Delete intents deliberately carry
		// zero bytes, but the protected intent contract stores content as NOT NULL.
		value.Content = []byte{}
		value.Message = "Delete protected Helm Application " + release.ID
		value.ExpectedETag = `"` + adoptedDigest + `"`
	}
	value.IntentDigest, err = applicationIntentDigest(value)
	if err != nil || value.Validate() != nil {
		return ProtectedApplicationIntent{}, false, ErrInvalid
	}
	continuation, err := ensureApplicationContinuation(ctx, tx, release, payload, value, s.authority, now)
	if err != nil {
		return ProtectedApplicationIntent{}, false, err
	}
	if continuation.PlannedBaseRevision != value.Binding.PlannedBaseRevision {
		return ProtectedApplicationIntent{}, false, ErrConflict
	}
	result, err := tx.Exec(ctx, `INSERT INTO public.helm_protected_application_intents(
		id,release_revision_id,payload_intent_id,release_generation,project_id,environment_id,
		application_id,action,platform_binding_id,environment_binding_id,cluster_id,
		platform_target_ref,environment_target_ref,environment_revision,environment_generation,
		catalog_digest,planned_base_revision,payload_revision,payload_path,source_directory,
		application_path,operation,precondition,expected_etag,content,content_digest,intent_digest,
		commit_trailer,publisher_contract,publisher_config_digest,original_publisher_config_digest,
		publisher_adoption_epoch,continuation_required,continuation_receipt_id,
		continuation_contract,cascade_required,cascade_receipt_id,cascade_contract,message,state,next_attempt_at,
		attempts,consecutive_failures,last_failure_code,lease_epoch,prerequisite_receipt_id,
		prerequisite_contract,prerequisite_epoch,created_at,updated_at
		) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,
			$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$30,0,TRUE,$1,$31,$32,$33,$34,$35,'pending',$36,0,0,'',0,
			$2,$37,0,$36,$36)
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
		value.Publisher.ConfigDigest, value.ContinuationContract, value.CascadeRequired,
		nullableCascadeReceipt(value.CascadeReceiptID), value.CascadeContract, value.Message, value.CreatedAt,
		protectedPrerequisiteContract)
	if err != nil {
		return ProtectedApplicationIntent{}, false, classifyPostgres(err)
	}
	created := result.RowsAffected() == 1
	if !created {
		existing, getErr := scanProtectedApplication(tx.QueryRow(ctx, `SELECT `+protectedApplicationColumns+`
			FROM public.helm_protected_application_intents
			WHERE payload_intent_id=$1 AND state<>'superseded'`, payload.ID))
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
	err := tx.QueryRow(ctx, `SELECT content_digest FROM public.helm_protected_application_intents
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
		FROM public.helm_protected_payload_intents WHERE id=$1`, intentID))
}

func (s *PostgresProtectedPublicationStore) Application(ctx context.Context, intentID string) (ProtectedApplicationIntent, error) {
	if !uuidRE.MatchString(intentID) {
		return ProtectedApplicationIntent{}, ErrInvalid
	}
	return scanProtectedApplication(s.pool.QueryRow(ctx, `SELECT `+protectedApplicationColumns+`
		FROM public.helm_protected_application_intents WHERE id=$1`, intentID))
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

func (s *PostgresProtectedPublicationStore) AdoptPayload(ctx context.Context, owner string,
	workerEpoch int64, publisher ProtectedPublisherIdentity, duration time.Duration) (ProtectedPayloadIntent, ProtectedIntentLease, error) {
	id, err := s.adoptProtected(ctx, protectedPayloadTable, owner, workerEpoch, publisher, duration)
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

func (s *PostgresProtectedPublicationStore) AdoptApplication(ctx context.Context, owner string,
	workerEpoch int64, publisher ProtectedPublisherIdentity, duration time.Duration) (ProtectedApplicationIntent, ProtectedIntentLease, error) {
	id, err := s.adoptProtected(ctx, protectedApplicationTable, owner, workerEpoch, publisher, duration)
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

func (s *PostgresProtectedPublicationStore) adoptProtected(ctx context.Context, table, owner string,
	workerEpoch int64, publisher ProtectedPublisherIdentity, duration time.Duration) (string, error) {
	return retryProtectedTransaction(ctx, func() (string, error) {
		return s.adoptProtectedOnce(ctx, table, owner, workerEpoch, publisher, duration)
	})
}

func (s *PostgresProtectedPublicationStore) adoptProtectedOnce(ctx context.Context, table, owner string,
	workerEpoch int64, publisher ProtectedPublisherIdentity, duration time.Duration) (string, error) {
	if !validProtectedTable(table) || !workerIDRE.MatchString(owner) || publisher.Validate() != nil ||
		workerEpoch < 1 || !validProtectedLeaseDuration(duration) {
		return "", ErrInvalid
	}
	function := "public.adopt_helm_protected_payload_intent"
	if table == protectedApplicationTable {
		function = "public.adopt_helm_protected_application_intent"
	} else if table == protectedCascadeTable {
		function = "public.adopt_helm_application_cascade_preflight"
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var intentID *string
	err = tx.QueryRow(ctx, `SELECT `+function+`($1,$2,$3,$4,$5,$6,$7)::text`,
		id.New(), owner, workerEpoch, publisher.Contract, publisher.PolicyVersion,
		publisher.ConfigDigest, duration.Milliseconds()).Scan(&intentID)
	if err != nil {
		return "", classifyPostgres(err)
	}
	if intentID == nil {
		if err = tx.Commit(ctx); err != nil {
			return "", classifyPostgres(err)
		}
		return "", ErrNotFound
	}
	if err = tx.Commit(ctx); err != nil {
		return "", classifyPostgres(err)
	}
	return *intentID, nil
}

func (s *PostgresProtectedPublicationStore) claimProtected(ctx context.Context, table, owner string,
	publisher ProtectedPublisherIdentity, now time.Time, duration time.Duration) (string, error) {
	return retryProtectedTransaction(ctx, func() (string, error) {
		return s.claimProtectedOnce(ctx, table, owner, publisher, now, duration)
	})
}

func (s *PostgresProtectedPublicationStore) claimProtectedOnce(ctx context.Context, table, owner string,
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
	staleEligibility := `NOT (` + freshProtectedProjectionSQL("candidate") + `)`
	stalePublisherEligibility := `candidate.publisher_config_digest=$3`
	staleFailureCode := "projection-superseded"
	if table == protectedApplicationTable {
		staleEligibility = `(NOT (CASE WHEN candidate.continuation_required THEN ` +
			applicationContinuationIsExactSQL("candidate") + ` ELSE ` + freshProtectedProjectionSQL("candidate") + ` END)
			OR (candidate.cascade_required AND NOT public.helm_application_cascade_is_exact(candidate.id,$3,$1)))`
		stalePublisherEligibility = `(candidate.continuation_required OR candidate.cascade_required OR candidate.publisher_config_digest=$3)`
	} else if table == protectedCascadeTable {
		staleEligibility = `NOT (` + cascadePreflightIsFreshSQL("candidate") + `)`
		// Keep the shared statement's third parameter typed without coupling
		// cascade retirement to the immutable preflight publisher identity.
		stalePublisherEligibility = `($3::text <> '')`
		staleFailureCode = "cascade-projection-superseded"
	}
	_, err = tx.Exec(ctx, `UPDATE `+table+` candidate SET state='superseded',
		completed_at=$1,updated_at=$1,consecutive_failures=1,
		last_failure_code='`+staleFailureCode+`',prerequisite_epoch=prerequisite_epoch+1
		WHERE candidate.state='pending' AND candidate.lease_epoch=0 AND candidate.attempts=0
		AND candidate.lease_owner IS NULL AND candidate.lease_until IS NULL
		AND candidate.write_base_revision='' AND candidate.write_base_observed_at IS NULL
		AND candidate.committed_revision='' AND candidate.committed_parent_revision=''
		AND candidate.committed_at IS NULL AND candidate.verified_at IS NULL
		AND candidate.verified_path_digest='' AND candidate.provider_request=''
		AND candidate.completed_at IS NULL
		AND candidate.publisher_contract=$2 AND (`+stalePublisherEligibility+`)
		AND (`+staleEligibility+`)
		AND NOT EXISTS(SELECT 1 FROM public.helm_protected_payload_intents held
			WHERE held.platform_binding_id=candidate.platform_binding_id
			AND (`+protectedLaneExclusion(table, protectedPayloadTable)+`)
			AND held.lease_owner IS NOT NULL AND held.lease_until>$1)
		AND NOT EXISTS(SELECT 1 FROM public.helm_protected_application_intents held
			WHERE held.platform_binding_id=candidate.platform_binding_id
			AND (`+protectedLaneExclusion(table, protectedApplicationTable)+`)
			AND held.lease_owner IS NOT NULL AND held.lease_until>$1)
		AND NOT EXISTS(SELECT 1 FROM public.helm_application_cascade_preflights held
			WHERE held.platform_binding_id=candidate.platform_binding_id
			AND (`+protectedLaneExclusion(table, protectedCascadeTable)+`)
			AND held.lease_owner IS NOT NULL AND held.lease_until>$1)`, now.UTC(),
		publisher.Contract, publisher.ConfigDigest)
	if err != nil {
		return "", classifyPostgres(err)
	}
	var intentID string
	initialAuthority := freshProtectedProjectionSQL("candidate")
	if table == protectedApplicationTable {
		initialAuthority = `(CASE WHEN candidate.continuation_required THEN ` +
			applicationContinuationIsExactSQL("candidate") + ` ELSE ` + freshProtectedProjectionSQL("candidate") + ` END)
			AND (NOT candidate.cascade_required OR public.helm_application_cascade_is_exact(candidate.id,$3,$1))`
	} else if table == protectedCascadeTable {
		initialAuthority = cascadePreflightIsFreshSQL("candidate")
	}
	err = tx.QueryRow(ctx, `SELECT candidate.id::text FROM `+table+` candidate
		WHERE candidate.state IN ('pending','claimed','git-committed')
		AND candidate.next_attempt_at<=$1
		AND (candidate.lease_owner IS NULL OR candidate.lease_until<=$1)
		AND candidate.publisher_contract=$2 AND candidate.publisher_config_digest=$3
		AND (candidate.lease_epoch>0 OR (`+initialAuthority+`))
		AND NOT EXISTS(SELECT 1 FROM public.helm_protected_payload_intents held
			WHERE held.platform_binding_id=candidate.platform_binding_id
			AND (`+protectedLaneExclusion(table, protectedPayloadTable)+`)
			AND held.lease_owner IS NOT NULL AND held.lease_until>$1)
		AND NOT EXISTS(SELECT 1 FROM public.helm_protected_application_intents held
			WHERE held.platform_binding_id=candidate.platform_binding_id
			AND (`+protectedLaneExclusion(table, protectedApplicationTable)+`)
			AND held.lease_owner IS NOT NULL AND held.lease_until>$1)
		AND NOT EXISTS(SELECT 1 FROM public.helm_application_cascade_preflights held
			WHERE held.platform_binding_id=candidate.platform_binding_id
			AND (`+protectedLaneExclusion(table, protectedCascadeTable)+`)
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
		attempts=LEAST(attempts+1,30),updated_at=$1,
		prerequisite_epoch=prerequisite_epoch+1
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

func cascadePreflightIsFreshSQL(alias string) string {
	if alias != "candidate" && alias != "intent" && alias != "preflight" {
		return "FALSE"
	}
	return `EXISTS(SELECT 1
		FROM public.helm_release_heads head
		JOIN public.helm_release_revisions release ON release.id=head.revision_id
		JOIN public.helm_protected_payload_intents payload ON payload.id=` + alias + `.payload_intent_id
		JOIN public.helm_protected_application_intents base ON base.id=` + alias + `.base_application_intent_id
		JOIN public.git_repository_bindings platform ON platform.id=` + alias + `.platform_binding_id
		WHERE head.environment_id=` + alias + `.environment_id
		  AND head.application_id=` + alias + `.application_id
		  AND head.revision_id=` + alias + `.release_revision_id
		  AND head.generation=` + alias + `.release_generation
		  AND release.project_id=` + alias + `.project_id
		  AND release.action='disable' AND NOT release.desired_enabled
		  AND release.base_intent_id=` + alias + `.base_application_intent_id
		  AND payload.release_revision_id=` + alias + `.release_revision_id
		  AND payload.state='verified' AND payload.action='disable-receipt'
		  AND payload.committed_revision=` + alias + `.payload_revision
		  AND base.state='verified' AND base.action='publish'
		  AND base.application_path=` + alias + `.application_path
		  AND base.content=` + alias + `.source_content
		  AND base.content_digest=` + alias + `.source_content_digest
		  AND platform.kind='platform' AND platform.credential_mode='github-app'
		  AND platform.cluster_id=` + alias + `.cluster_id
		  AND platform.target_ref=` + alias + `.platform_target_ref
		  AND platform.target_head_revision IS NOT NULL)`
}

func freshProtectedProjectionSQL(alias string) string {
	if alias != "candidate" {
		panic("closed SQL alias")
	}
	return `EXISTS(SELECT 1 FROM public.git_repository_bindings platform
		JOIN public.git_repository_bindings environment ON environment.id=` + alias + `.environment_binding_id
		JOIN public.git_projection_generations generation ON generation.binding_id=environment.id
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
			SELECT 1 FROM public.git_projected_documents invalid
			WHERE invalid.binding_id=environment.id
			AND invalid.generation=` + alias + `.environment_generation AND NOT invalid.valid))`
}

func applicationContinuationIsExactSQL(alias string) string {
	if alias != "candidate" {
		panic("closed SQL alias")
	}
	return `public.helm_application_continuation_is_exact(` + alias + `.id)`
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
	result, err := s.pool.Exec(ctx, `UPDATE `+table+` SET lease_until=$6,updated_at=$5,
		prerequisite_epoch=prerequisite_epoch+1
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

func (s *PostgresProtectedPublicationStore) RebindPayloadWriteBase(ctx context.Context,
	lease ProtectedIntentLease, previous, revision string, observedAt, now time.Time) (ProtectedPayloadIntent, error) {
	if err := s.rebindProtectedWriteBase(ctx, protectedPayloadTable, lease, previous, revision, observedAt, now); err != nil {
		return ProtectedPayloadIntent{}, err
	}
	return s.Payload(ctx, lease.IntentID)
}

func (s *PostgresProtectedPublicationStore) RebindApplicationWriteBase(ctx context.Context,
	lease ProtectedIntentLease, previous, revision string, observedAt, now time.Time) (ProtectedApplicationIntent, error) {
	if err := s.rebindProtectedWriteBase(ctx, protectedApplicationTable, lease, previous, revision, observedAt, now); err != nil {
		return ProtectedApplicationIntent{}, err
	}
	return s.Application(ctx, lease.IntentID)
}

// rebindProtectedWriteBase repairs only a claimed operation whose previous
// CAS base lost to an unrelated protected-lane commit. The publisher calls it
// only after a fresh provider read proves the operation trailer is absent and
// the protected path precondition still holds at revision.
func (s *PostgresProtectedPublicationStore) rebindProtectedWriteBase(ctx context.Context, table string,
	lease ProtectedIntentLease, previous, revision string, observedAt, now time.Time) error {
	if !validProtectedTable(table) || lease.Validate() != nil || !gitCommitRE.MatchString(previous) ||
		!gitCommitRE.MatchString(revision) || previous == revision || observedAt.IsZero() || now.IsZero() ||
		observedAt.After(now) {
		return ErrInvalid
	}
	result, err := s.pool.Exec(ctx, `UPDATE `+table+` SET write_base_revision=$7,
		write_base_observed_at=$8,updated_at=$9,prerequisite_epoch=prerequisite_epoch+1
		WHERE id=$1 AND lease_owner=$2 AND lease_epoch=$3 AND publisher_contract=$4
		AND publisher_config_digest=$5 AND lease_until>$9 AND state='claimed'
		AND write_base_revision=$6 AND write_base_observed_at IS NOT NULL
		AND committed_revision='' AND committed_parent_revision='' AND committed_at IS NULL
		AND updated_at<=$9`, lease.IntentID, lease.Owner, lease.Epoch,
		lease.Publisher.Contract, lease.Publisher.ConfigDigest, previous, revision,
		observedAt.UTC(), now.UTC())
	if err != nil {
		return classifyPostgres(err)
	}
	if result.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

func (s *PostgresProtectedPublicationStore) bindProtectedWriteBase(ctx context.Context, table string,
	lease ProtectedIntentLease, revision string, observedAt, now time.Time) error {
	if !validProtectedTable(table) || lease.Validate() != nil || !gitCommitRE.MatchString(revision) ||
		observedAt.IsZero() || now.IsZero() || observedAt.After(now) {
		return ErrInvalid
	}
	result, err := s.pool.Exec(ctx, `UPDATE `+table+` SET write_base_revision=$6,
		write_base_observed_at=$7,updated_at=$8,prerequisite_epoch=prerequisite_epoch+1
		WHERE id=$1 AND lease_owner=$2
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
		committed_revision=$6,committed_parent_revision=$7,committed_at=$8,updated_at=$8,
		prerequisite_epoch=prerequisite_epoch+1
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
		lease_until=NULL,updated_at=$8,prerequisite_epoch=prerequisite_epoch+1
		WHERE id=$1 AND lease_owner=$2 AND lease_epoch=$3
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
		updated_at=$8,prerequisite_epoch=prerequisite_epoch+1
		WHERE id=$1 AND lease_owner=$2 AND lease_epoch=$3
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
		lease_owner=NULL,lease_until=NULL,completed_at=$7,updated_at=$7,
		prerequisite_epoch=prerequisite_epoch+1
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return classifyPostgres(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = pruneExpiredHelmReadinessTx(ctx, tx, "helm-protected-publisher", readiness.WorkerID); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO public.runtime_readiness(runtime_kind,scope_key,worker_id,worker_epoch,
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
	if err != nil {
		return classifyPostgres(err)
	}
	return classifyPostgres(tx.Commit(ctx))
}

func (s *PostgresProtectedPublicationStore) PublisherReady(ctx context.Context,
	publisher ProtectedPublisherIdentity, now time.Time) (bool, error) {
	if publisher.Validate() != nil || now.IsZero() {
		return false, ErrInvalid
	}
	var ready bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.runtime_readiness
		WHERE runtime_kind='helm-protected-publisher' AND scope_key='global' AND contract_version=$1
		AND identity->>'policyVersion'=$2 AND config_digest=$3
		AND updated_at=observed_at AND observed_at<=$4
		AND observed_at>=$5 AND lease_until>$4
		AND lease_until<=observed_at+interval '5 minutes'
		AND lease_until<=$6)`, publisher.Contract, publisher.PolicyVersion,
		publisher.ConfigDigest, now.UTC(), now.UTC().Add(-maximumPublisherReadinessAge),
		now.UTC().Add(maximumPublisherReadinessLease)).Scan(&ready)
	return ready, classifyPostgres(err)
}

func payloadIntentDigest(value ProtectedPayloadIntent) (string, error) {
	publisher := value.Publisher
	if value.OriginalPublisherConfigDigest != "" {
		publisher.ConfigDigest = value.OriginalPublisherConfigDigest
	}
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
		value.ContentDigest, value.InventoryDigest, value.ResourceCount, publisher})
}

func applicationIntentDigest(value ProtectedApplicationIntent) (string, error) {
	publisher := value.Publisher
	if value.OriginalPublisherConfigDigest != "" {
		publisher.ConfigDigest = value.OriginalPublisherConfigDigest
	}
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
		value.ContentDigest, publisher})
}

func equalProtectedPayloadIdentity(left, right ProtectedPayloadIntent) bool {
	return left.ID == right.ID && left.ReleaseRevisionID == right.ReleaseRevisionID &&
		left.ReleaseGeneration == right.ReleaseGeneration && left.Target == right.Target &&
		left.Action == right.Action && left.Binding == right.Binding && left.Path == right.Path &&
		equalBytes(left.Content, right.Content) && left.ContentDigest == right.ContentDigest &&
		left.InventoryDigest == right.InventoryDigest && left.ResourceCount == right.ResourceCount &&
		left.IntentDigest == right.IntentDigest && left.CommitTrailer == right.CommitTrailer &&
		left.Publisher.Contract == right.Publisher.Contract &&
		left.Publisher.PolicyVersion == right.Publisher.PolicyVersion &&
		left.OriginalPublisherConfigDigest == right.OriginalPublisherConfigDigest &&
		left.Message == right.Message
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
		left.CommitTrailer == right.CommitTrailer &&
		left.Publisher.Contract == right.Publisher.Contract &&
		left.Publisher.PolicyVersion == right.Publisher.PolicyVersion &&
		left.OriginalPublisherConfigDigest == right.OriginalPublisherConfigDigest &&
		left.ContinuationRequired == right.ContinuationRequired &&
		left.ContinuationReceiptID == right.ContinuationReceiptID &&
		left.ContinuationContract == right.ContinuationContract &&
		left.CascadeRequired == right.CascadeRequired && left.CascadeReceiptID == right.CascadeReceiptID &&
		left.CascadeContract == right.CascadeContract &&
		left.Message == right.Message
}

func nullableCascadeReceipt(value string) any {
	if value == "" {
		return nil
	}
	return value
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
		value.Publisher == lease.Publisher &&
		value.LeaseUntil != nil && value.LeaseUntil.After(now)
}

func activeApplicationLease(value ProtectedApplicationIntent, lease ProtectedIntentLease, now time.Time) bool {
	return value.LeaseOwner == lease.Owner && value.LeaseEpoch == lease.Epoch &&
		value.Publisher == lease.Publisher &&
		value.LeaseUntil != nil && value.LeaseUntil.After(now)
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
	return table == protectedPayloadTable || table == protectedApplicationTable || table == protectedCascadeTable
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
var _ ProtectedCascadeStore = (*PostgresProtectedPublicationStore)(nil)
var _ ProtectedCascadeObservationStore = (*PostgresProtectedPublicationStore)(nil)
