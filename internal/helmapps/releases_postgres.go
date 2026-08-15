package helmapps

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/id"
)

type PostgresReleaseService struct {
	pool                 *pgxpool.Pool
	operatorConfigDigest string
}

func NewPostgresReleaseService(pool *pgxpool.Pool, operatorConfigDigest string) (*PostgresReleaseService, error) {
	if pool == nil || !validDigest(operatorConfigDigest) {
		return nil, ErrInvalid
	}
	return &PostgresReleaseService{pool: pool, operatorConfigDigest: operatorConfigDigest}, nil
}

func (s *PostgresReleaseService) ApprovalCatalog(ctx context.Context, limit int) ([]ApprovalDocument, error) {
	if limit < 1 || limit > 100 {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT
		a.approval_id::text,a.revision,a.oci_repository,a.chart_version,
		a.manifest_digest,a.package_digest,a.values_schema_digest,a.renderer_image,
		a.renderer_version,a.policy_version,a.created_by::text,a.idempotency_key,
		a.created_at,a.identity_digest,d.values_schema_json,d.default_values_yaml,
		d.documents_digest,d.created_at
		FROM helm_chart_approvals a
		JOIN helm_chart_approval_documents d
		  ON d.approval_id=a.approval_id AND d.approval_revision=a.revision
		ORDER BY a.created_at DESC,a.approval_id,a.revision DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]ApprovalDocument, 0, limit)
	for rows.Next() {
		document, scanErr := scanApprovalDocument(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, cloneApprovalDocument(document))
	}
	return result, rows.Err()
}

func (s *PostgresReleaseService) ApprovalDocument(ctx context.Context, key ApprovalKey) (ApprovalDocument, error) {
	if key.Validate() != nil {
		return ApprovalDocument{}, ErrInvalid
	}
	document, err := scanApprovalDocument(s.pool.QueryRow(ctx, approvalDocumentSelect+`
		WHERE a.approval_id=$1 AND a.revision=$2`, key.ID, key.Revision))
	return cloneApprovalDocument(document), classifyPostgres(err)
}

func (s *PostgresReleaseService) PreviewValues(ctx context.Context, target ReleaseTarget,
	approval ApprovalKey, values []byte) (ValuesPreview, error) {
	if target.Validate() != nil || approval.Validate() != nil {
		return ValuesPreview{}, ErrInvalid
	}
	document, err := s.ApprovalDocument(ctx, approval)
	if err != nil {
		return ValuesPreview{}, err
	}
	var current []byte
	err = s.pool.QueryRow(ctx, `SELECT release.values_yaml FROM helm_release_heads head
		JOIN helm_release_revisions release ON release.id=head.revision_id
		WHERE head.project_id=$1 AND head.environment_id=$2 AND head.application_id=$3`,
		target.ProjectID, target.EnvironmentID, target.ApplicationID).Scan(&current)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return ValuesPreview{}, classifyPostgres(err)
	}
	return PreviewApprovalValues(document, current, values)
}

func (s *PostgresReleaseService) Upsert(ctx context.Context, request UpsertReleaseRequest, now time.Time) (ReleaseRevision, bool, error) {
	if request.Target.Validate() != nil || request.Actor.Validate() != nil ||
		request.Approval.Validate() != nil || now.IsZero() {
		return ReleaseRevision{}, false, ErrInvalid
	}
	document, err := s.ApprovalDocument(ctx, request.Approval)
	if err != nil {
		return ReleaseRevision{}, false, err
	}
	preview, err := PreviewApprovalValues(document, nil, request.ValuesYAML)
	if err != nil {
		return ReleaseRevision{}, false, err
	}
	values, err := ParseValues([]byte(preview.NormalizedValuesYAML))
	if err != nil {
		return ReleaseRevision{}, false, err
	}
	digest, err := releaseRequestDigest("upsert", request.Target, request.Approval,
		digestBytes(values.Raw), "")
	if err != nil {
		return ReleaseRevision{}, false, err
	}
	return s.mutate(ctx, releaseMutation{
		kind: "upsert", target: request.Target, actor: request.Actor,
		approval: request.Approval, values: values.Raw, requestDigest: digest,
	}, now)
}

func (s *PostgresReleaseService) Retry(ctx context.Context, request RetryReleaseRequest, now time.Time) (ReleaseRevision, bool, error) {
	return s.simpleMutation(ctx, "retry", request.Target, request.Actor, "", now)
}

func (s *PostgresReleaseService) Disable(ctx context.Context, request DisableReleaseRequest, now time.Time) (ReleaseRevision, bool, error) {
	return s.simpleMutation(ctx, "disable", request.Target, request.Actor, "", now)
}

func (s *PostgresReleaseService) Rollback(ctx context.Context, request RollbackReleaseRequest, now time.Time) (ReleaseRevision, bool, error) {
	if !uuidRE.MatchString(request.SourceRevisionID) {
		return ReleaseRevision{}, false, ErrInvalid
	}
	return s.simpleMutation(ctx, "rollback", request.Target, request.Actor,
		request.SourceRevisionID, now)
}

func (s *PostgresReleaseService) simpleMutation(ctx context.Context, kind string, target ReleaseTarget, actor ReleaseActor, source string, now time.Time) (ReleaseRevision, bool, error) {
	if target.Validate() != nil || actor.Validate() != nil || now.IsZero() {
		return ReleaseRevision{}, false, ErrInvalid
	}
	digest, err := releaseRequestDigest(kind, target, ApprovalKey{}, "", source)
	if err != nil {
		return ReleaseRevision{}, false, err
	}
	return s.mutate(ctx, releaseMutation{kind: kind, target: target, actor: actor,
		sourceRevisionID: source, requestDigest: digest}, now)
}

type releaseMutation struct {
	kind, sourceRevisionID, requestDigest string
	target                                ReleaseTarget
	actor                                 ReleaseActor
	approval                              ApprovalKey
	values                                []byte
}

func (s *PostgresReleaseService) mutate(ctx context.Context, mutation releaseMutation, now time.Time) (ReleaseRevision, bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ReleaseRevision{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	revision, replay, err := s.mutateTx(ctx, tx, mutation, now)
	if err != nil {
		return ReleaseRevision{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ReleaseRevision{}, false, classifyPostgres(err)
	}
	return revision, replay, nil
}

func (s *PostgresReleaseService) mutateTx(ctx context.Context, tx pgx.Tx, mutation releaseMutation, now time.Time) (ReleaseRevision, bool, error) {
	var err error
	existing, replayErr := scanReleaseRevision(tx.QueryRow(ctx, releaseRevisionSelect+
		` WHERE actor_id=$1 AND idempotency_key=$2`, mutation.actor.ID, mutation.actor.IdempotencyKey))
	if replayErr == nil {
		if existing.IntentDigest != mutation.requestDigest || existing.Target != mutation.target {
			return ReleaseRevision{}, false, ErrConflict
		}
		return existing, true, nil
	}
	if !errors.Is(replayErr, pgx.ErrNoRows) {
		return ReleaseRevision{}, false, classifyPostgres(replayErr)
	}

	var namespace, releaseName string
	if err = tx.QueryRow(ctx, `SELECT environment.namespace,application.slug
		FROM environments environment
		JOIN applications application ON application.id=$3 AND application.project_id=$1
		WHERE environment.id=$2 AND environment.project_id=$1`,
		mutation.target.ProjectID, mutation.target.EnvironmentID,
		mutation.target.ApplicationID).Scan(&namespace, &releaseName); err != nil {
		return ReleaseRevision{}, false, classifyPostgres(err)
	}

	var headID string
	var headGeneration int64
	var headUpdatedAt time.Time
	headErr := tx.QueryRow(ctx, `SELECT revision_id::text,generation,updated_at
		FROM helm_release_heads WHERE environment_id=$1 AND application_id=$2 FOR UPDATE`,
		mutation.target.EnvironmentID, mutation.target.ApplicationID).
		Scan(&headID, &headGeneration, &headUpdatedAt)
	if headErr != nil && !errors.Is(headErr, pgx.ErrNoRows) {
		return ReleaseRevision{}, false, classifyPostgres(headErr)
	}
	if !headUpdatedAt.IsZero() && !now.After(headUpdatedAt) {
		now = headUpdatedAt.Add(time.Microsecond)
	}
	now = now.UTC()

	var parent ReleaseRevision
	if headID != "" {
		parent, err = scanReleaseRevision(tx.QueryRow(ctx, releaseRevisionSelect+` WHERE id=$1`, headID))
		if err != nil {
			return ReleaseRevision{}, false, classifyPostgres(err)
		}
	}
	baseIntentID, err := latestLiveApplicationIntent(ctx, tx, mutation.target)
	if err != nil {
		return ReleaseRevision{}, false, err
	}

	revision := ReleaseRevision{
		ID: id.New(), Generation: headGeneration + 1, Target: mutation.target,
		ReleaseName: releaseName, DesiredEnabled: mutation.kind != "disable",
		ParentRevisionID: headID, BaseApplicationIntentID: baseIntentID,
		IntentDigest: mutation.requestDigest, ActorID: mutation.actor.ID,
		IdempotencyKey: mutation.actor.IdempotencyKey, RequestID: mutation.actor.RequestID,
		CreatedAt: now,
	}
	switch mutation.kind {
	case "upsert":
		if headID == "" {
			revision.Action = ReleaseInitial
		} else {
			revision.Action = ReleaseUpdate
		}
		revision.Approval, revision.ValuesYAML = mutation.approval, append([]byte(nil), mutation.values...)
	case "retry":
		if headID == "" || !parent.DesiredEnabled {
			return ReleaseRevision{}, false, ErrConflict
		}
		revision.Action, revision.Approval = ReleaseRetry, parent.Approval
		revision.ValuesYAML = append([]byte(nil), parent.ValuesYAML...)
	case "disable":
		if headID == "" || !parent.DesiredEnabled || baseIntentID == "" {
			return ReleaseRevision{}, false, ErrConflict
		}
		revision.Action, revision.Approval = ReleaseDisable, parent.Approval
		revision.ValuesYAML = append([]byte(nil), parent.ValuesYAML...)
	case "rollback":
		if headID == "" || mutation.sourceRevisionID == headID {
			return ReleaseRevision{}, false, ErrConflict
		}
		source, sourceErr := scanReleaseRevision(tx.QueryRow(ctx, releaseRevisionSelect+
			` WHERE id=$1 AND project_id=$2 AND environment_id=$3 AND application_id=$4`,
			mutation.sourceRevisionID, mutation.target.ProjectID,
			mutation.target.EnvironmentID, mutation.target.ApplicationID))
		if sourceErr != nil || !source.DesiredEnabled || source.Generation >= revision.Generation {
			if sourceErr != nil {
				return ReleaseRevision{}, false, classifyPostgres(sourceErr)
			}
			return ReleaseRevision{}, false, ErrConflict
		}
		revision.Action, revision.RollbackSourceRevisionID = ReleaseRollback, source.ID
		revision.Approval, revision.ValuesYAML = source.Approval, append([]byte(nil), source.ValuesYAML...)
	default:
		return ReleaseRevision{}, false, ErrInvalid
	}
	revision.ValuesDigest = digestBytes(revision.ValuesYAML)

	if revision.DesiredEnabled {
		approval, approvalErr := scanApproval(tx.QueryRow(ctx, approvalSelect+
			` WHERE approval_id=$1 AND revision=$2 FOR KEY SHARE`,
			revision.Approval.ID, revision.Approval.Revision))
		if approvalErr != nil {
			return ReleaseRevision{}, false, classifyPostgres(approvalErr)
		}
		commandID := id.New()
		desired, desiredErr := NewDesiredRender(commandID, mutation.actor.ID,
			"release-render-"+revision.ID, approval,
			DestinationIdentity{ProjectID: mutation.target.ProjectID,
				EnvironmentID: mutation.target.EnvironmentID, ApplicationID: mutation.target.ApplicationID,
				ApplicationSlug: releaseName, Namespace: namespace}, revision.ValuesYAML)
		if desiredErr != nil {
			return ReleaseRevision{}, false, desiredErr
		}
		if _, err = tx.Exec(ctx, `INSERT INTO helm_render_commands(
			id,idempotency_scope,idempotency_key,approval_id,approval_revision,project_id,
			environment_id,application_id,namespace,release_name,descriptor_yaml,values_yaml,
			descriptor_digest,values_digest,input_digest,operator_config_digest,state,available_at,created_at,updated_at
		) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,
			$16,'queued',$17,$17,$17)`, desired.ID, desired.IdempotencyScope,
			desired.IdempotencyKey, desired.Approval.ID, desired.Approval.Revision,
			desired.Descriptor.Destination.ProjectID, desired.Descriptor.Destination.EnvironmentID,
			desired.Descriptor.Destination.ApplicationID, desired.Descriptor.Destination.Namespace,
			desired.Descriptor.ReleaseName, desired.DescriptorYAML, desired.ValuesYAML,
			desired.DescriptorDigest, desired.ValuesDigest, desired.InputDigest,
			s.operatorConfigDigest, now); err != nil {
			return ReleaseRevision{}, false, classifyPostgres(err)
		}
		revision.RenderCommandID = desired.ID
	}

	if _, err = tx.Exec(ctx, `INSERT INTO helm_release_revisions(
		id,generation,project_id,environment_id,application_id,release_name,action,
		desired_enabled,parent_revision_id,rollback_source_revision_id,base_intent_id,
		approval_id,approval_revision,render_command_id,values_yaml,values_digest,
		intent_digest,actor_id,idempotency_key,request_id,created_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)`,
		revision.ID, revision.Generation, revision.Target.ProjectID, revision.Target.EnvironmentID,
		revision.Target.ApplicationID, revision.ReleaseName, revision.Action,
		revision.DesiredEnabled, nullableReleaseID(revision.ParentRevisionID),
		nullableReleaseID(revision.RollbackSourceRevisionID),
		nullableReleaseID(revision.BaseApplicationIntentID), revision.Approval.ID,
		revision.Approval.Revision, nullableReleaseID(revision.RenderCommandID),
		revision.ValuesYAML, revision.ValuesDigest, revision.IntentDigest, revision.ActorID,
		revision.IdempotencyKey, revision.RequestID, revision.CreatedAt); err != nil {
		return ReleaseRevision{}, false, classifyPostgres(err)
	}
	if headID == "" {
		_, err = tx.Exec(ctx, `INSERT INTO helm_release_heads(
			project_id,environment_id,application_id,revision_id,generation,updated_at
		) VALUES($1,$2,$3,$4,$5,$6)`, revision.Target.ProjectID,
			revision.Target.EnvironmentID, revision.Target.ApplicationID, revision.ID,
			revision.Generation, revision.CreatedAt)
	} else {
		var updated string
		err = tx.QueryRow(ctx, `UPDATE helm_release_heads SET revision_id=$3,generation=$4,updated_at=$5
			WHERE environment_id=$1 AND application_id=$2 AND revision_id=$6 AND generation=$7
			RETURNING revision_id::text`, revision.Target.EnvironmentID,
			revision.Target.ApplicationID, revision.ID, revision.Generation,
			revision.CreatedAt, headID, headGeneration).Scan(&updated)
	}
	if err != nil {
		return ReleaseRevision{}, false, classifyPostgres(err)
	}
	if revision.Validate() != nil {
		return ReleaseRevision{}, false, ErrConflict
	}
	return cloneReleaseRevision(revision), false, nil
}

func (s *PostgresReleaseService) Head(ctx context.Context, target ReleaseTarget) (ReleaseStatus, error) {
	if target.Validate() != nil {
		return ReleaseStatus{}, ErrInvalid
	}
	status, err := scanReleaseStatus(s.pool.QueryRow(ctx, releaseStatusSelect+`
		JOIN helm_release_heads head ON head.revision_id=release.id
		WHERE head.environment_id=$1 AND head.application_id=$2 AND release.project_id=$3`,
		target.EnvironmentID, target.ApplicationID, target.ProjectID))
	return status, classifyPostgres(err)
}

func (s *PostgresReleaseService) History(ctx context.Context, target ReleaseTarget, limit int) ([]ReleaseStatus, error) {
	if target.Validate() != nil || limit < 1 || limit > 100 {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, releaseStatusSelect+`
		WHERE release.environment_id=$1 AND release.application_id=$2 AND release.project_id=$3
		ORDER BY release.generation DESC LIMIT $4`, target.EnvironmentID,
		target.ApplicationID, target.ProjectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]ReleaseStatus, 0, limit)
	for rows.Next() {
		status, scanErr := scanReleaseStatus(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, status)
	}
	return result, rows.Err()
}

const releaseRevisionSelect = `SELECT id::text,generation,project_id::text,
	environment_id::text,application_id::text,release_name,action,desired_enabled,
	COALESCE(parent_revision_id::text,''),COALESCE(rollback_source_revision_id::text,''),
	COALESCE(base_intent_id::text,''),approval_id::text,approval_revision,
	COALESCE(render_command_id::text,''),values_yaml,values_digest,intent_digest,
	actor_id::text,idempotency_key,request_id,created_at FROM helm_release_revisions`

const approvalDocumentSelect = `SELECT
		a.approval_id::text,a.revision,a.oci_repository,a.chart_version,
		a.manifest_digest,a.package_digest,a.values_schema_digest,a.renderer_image,
		a.renderer_version,a.policy_version,a.created_by::text,a.idempotency_key,
		a.created_at,a.identity_digest,d.values_schema_json,d.default_values_yaml,
		d.documents_digest,d.created_at
		FROM helm_chart_approvals a
		JOIN helm_chart_approval_documents d
		  ON d.approval_id=a.approval_id AND d.approval_revision=a.revision `

func scanApprovalDocument(row rowScanner) (ApprovalDocument, error) {
	var document ApprovalDocument
	var identityDigest string
	err := row.Scan(&document.Approval.ID, &document.Approval.Revision,
		&document.Approval.OCIRepository, &document.Approval.ChartVersion,
		&document.Approval.ManifestDigest, &document.Approval.PackageDigest,
		&document.Approval.ValuesSchemaDigest, &document.Approval.RendererImage,
		&document.Approval.RendererVersion, &document.Approval.PolicyVersion,
		&document.Approval.CreatedBy, &document.Approval.IdempotencyKey,
		&document.Approval.CreatedAt, &identityDigest, &document.ValuesSchemaJSON,
		&document.DefaultValuesYAML, &document.DocumentsDigest, &document.CreatedAt)
	if err != nil {
		return ApprovalDocument{}, err
	}
	expectedIdentity, identityErr := document.Approval.IdentityDigest()
	if identityErr != nil || expectedIdentity != identityDigest || document.Validate() != nil {
		return ApprovalDocument{}, ErrConflict
	}
	return document, nil
}

func scanReleaseRevision(row rowScanner) (ReleaseRevision, error) {
	var revision ReleaseRevision
	err := row.Scan(&revision.ID, &revision.Generation, &revision.Target.ProjectID,
		&revision.Target.EnvironmentID, &revision.Target.ApplicationID, &revision.ReleaseName,
		&revision.Action, &revision.DesiredEnabled, &revision.ParentRevisionID,
		&revision.RollbackSourceRevisionID, &revision.BaseApplicationIntentID,
		&revision.Approval.ID, &revision.Approval.Revision, &revision.RenderCommandID,
		&revision.ValuesYAML, &revision.ValuesDigest, &revision.IntentDigest,
		&revision.ActorID, &revision.IdempotencyKey, &revision.RequestID, &revision.CreatedAt)
	if err == nil && revision.Validate() != nil {
		return ReleaseRevision{}, ErrConflict
	}
	return revision, err
}

const releaseStatusSelect = `SELECT release.id::text,release.generation,release.project_id::text,
	release.environment_id::text,release.application_id::text,release.release_name,release.action,release.desired_enabled,
	COALESCE(release.parent_revision_id::text,''),COALESCE(release.rollback_source_revision_id::text,''),
	COALESCE(release.base_intent_id::text,''),release.approval_id::text,release.approval_revision,
	COALESCE(release.render_command_id::text,''),release.values_yaml,release.values_digest,release.intent_digest,
	release.actor_id::text,release.idempotency_key,release.request_id,release.created_at,
	COALESCE(render.state,''),COALESCE(render.last_failure_code,''),
	COALESCE(payload.id::text,''),COALESCE(payload.state,''),
	COALESCE(payload.committed_revision,''),COALESCE(payload.last_failure_code,''),
	COALESCE(cascade.state,''),COALESCE(cascade.last_failure_code,''),
	COALESCE(cascade_observation.state,''),COALESCE(cascade_observation.last_failure_code,''),
	COALESCE(application.id::text,''),COALESCE(application.state,''),
	COALESCE(application.committed_revision,''),COALESCE(application.last_failure_code,'')
	FROM helm_release_revisions release
	LEFT JOIN helm_render_commands render ON render.id=release.render_command_id
	LEFT JOIN helm_protected_payload_intents payload ON payload.release_revision_id=release.id
	LEFT JOIN LATERAL (
		SELECT candidate.* FROM helm_application_cascade_preflights candidate
		WHERE candidate.release_revision_id=release.id
		ORDER BY (candidate.state<>'superseded') DESC,candidate.created_at DESC,candidate.id DESC
		LIMIT 1
	) cascade ON true
	LEFT JOIN LATERAL (
		SELECT job.* FROM helm_application_cascade_observation_jobs job
		JOIN helm_application_cascade_observer_activations activation
		  ON activation.platform_binding_id=job.platform_binding_id
		 AND activation.activation_epoch=job.activation_epoch
		WHERE job.cascade_preflight_id=cascade.id
		  AND activation.activation_epoch=(
		    SELECT MAX(current.activation_epoch)
		    FROM helm_application_cascade_observer_activations current
		    WHERE current.platform_binding_id=cascade.platform_binding_id)
		ORDER BY job.activation_epoch DESC
		LIMIT 1
	) cascade_observation ON true
	LEFT JOIN LATERAL (
		SELECT candidate.* FROM helm_protected_application_intents candidate
		WHERE candidate.release_revision_id=release.id
		ORDER BY (candidate.state<>'superseded') DESC,candidate.created_at DESC,candidate.id DESC
		LIMIT 1
	) application ON true `

func scanReleaseStatus(row rowScanner) (ReleaseStatus, error) {
	var status ReleaseStatus
	var renderFailure, payloadFailure, cascadeFailure string
	var cascadeObservationFailure, applicationFailure string
	err := row.Scan(&status.Revision.ID, &status.Revision.Generation,
		&status.Revision.Target.ProjectID, &status.Revision.Target.EnvironmentID,
		&status.Revision.Target.ApplicationID, &status.Revision.ReleaseName,
		&status.Revision.Action, &status.Revision.DesiredEnabled,
		&status.Revision.ParentRevisionID, &status.Revision.RollbackSourceRevisionID,
		&status.Revision.BaseApplicationIntentID, &status.Revision.Approval.ID,
		&status.Revision.Approval.Revision, &status.Revision.RenderCommandID,
		&status.Revision.ValuesYAML, &status.Revision.ValuesDigest,
		&status.Revision.IntentDigest, &status.Revision.ActorID,
		&status.Revision.IdempotencyKey, &status.Revision.RequestID,
		&status.Revision.CreatedAt, &status.RenderState, &renderFailure,
		&status.PayloadIntentID, &status.PayloadState, &status.PayloadRevision,
		&payloadFailure, &status.CascadeState, &cascadeFailure, &status.CascadeObservationState,
		&cascadeObservationFailure, &status.ApplicationIntentID, &status.ApplicationState,
		&status.ApplicationRevision, &applicationFailure)
	if err != nil {
		return ReleaseStatus{}, err
	}
	if status.Revision.Validate() != nil {
		return ReleaseStatus{}, ErrConflict
	}
	status.Phase, status.FailureCode = deriveReleasePhase(status, renderFailure,
		payloadFailure, status.CascadeState, cascadeFailure, status.CascadeObservationState,
		cascadeObservationFailure, applicationFailure)
	return status, nil
}

func deriveReleasePhase(status ReleaseStatus, renderFailure, payloadFailure, cascadeState,
	cascadeFailure, cascadeObservationState, cascadeObservationFailure,
	applicationFailure string) (ReleasePhase, string) {
	if status.RenderState == "failed" {
		return ReleasePhaseRenderFailed, renderFailure
	}
	if status.Revision.DesiredEnabled && status.RenderState != "succeeded" {
		return ReleasePhaseRendering, ""
	}
	switch status.PayloadState {
	case "":
		return ReleasePhasePayloadPending, ""
	case "pending", "claimed":
		return ReleasePhasePayloadPending, ""
	case "git-committed":
		return ReleasePhasePayloadCommitted, ""
	case "failed", "superseded":
		return ReleasePhaseFailed, payloadFailure
	case "verified":
		// A verified Application is terminal durable publication history. A
		// later observer-authority rotation can temporarily leave no current
		// observation job, but it must not regress an already-published release.
		if status.ApplicationState == "verified" && (cascadeState == "" || cascadeState == "verified") {
			return ReleasePhasePublished, ""
		}
		switch cascadeState {
		case "pending", "claimed":
			return ReleasePhaseApplicationPending, ""
		case "git-committed":
			return ReleasePhaseApplicationCommitted, ""
		case "failed", "superseded":
			return ReleasePhaseFailed, cascadeFailure
		case "verified":
			switch cascadeObservationState {
			case "", "pending", "claimed":
				return ReleasePhaseApplicationPending, ""
			case "failed", "superseded":
				return ReleasePhaseFailed, cascadeObservationFailure
			case "verified":
				// The planner may not have materialized the final delete intent yet.
				if status.ApplicationState == "" {
					return ReleasePhaseApplicationPending, ""
				}
			default:
				return ReleasePhaseFailed, "invalid-durable-state"
			}
		case "":
			if !status.Revision.DesiredEnabled {
				return ReleasePhaseApplicationPending, ""
			}
			// Publish releases have no cascade preflight.
		default:
			return ReleasePhaseFailed, "invalid-durable-state"
		}
		if status.ApplicationState == "" {
			return ReleasePhasePayloadVerified, ""
		}
	}
	switch status.ApplicationState {
	case "pending", "claimed":
		return ReleasePhaseApplicationPending, ""
	case "git-committed":
		return ReleasePhaseApplicationCommitted, ""
	case "verified":
		return ReleasePhasePublished, ""
	case "failed", "superseded":
		return ReleasePhaseFailed, applicationFailure
	default:
		return ReleasePhaseFailed, "invalid-durable-state"
	}
}

func latestLiveApplicationIntent(ctx context.Context, tx pgx.Tx, target ReleaseTarget) (string, error) {
	var intentID, action string
	err := tx.QueryRow(ctx, `SELECT id::text,action
		FROM helm_protected_application_intents
		WHERE project_id=$1 AND environment_id=$2 AND application_id=$3 AND state='verified'
		ORDER BY release_generation DESC LIMIT 1 FOR KEY SHARE`, target.ProjectID,
		target.EnvironmentID, target.ApplicationID).Scan(&intentID, &action)
	if errors.Is(err, pgx.ErrNoRows) || action == "delete" {
		return "", nil
	}
	if err != nil {
		return "", classifyPostgres(err)
	}
	return intentID, nil
}

func releaseRequestDigest(kind string, target ReleaseTarget, approval ApprovalKey, valuesDigest, source string) (string, error) {
	if target.Validate() != nil || (kind == "upsert" && (approval.Validate() != nil || !validDigest(valuesDigest))) ||
		(kind == "rollback" && !uuidRE.MatchString(source)) {
		return "", ErrInvalid
	}
	return digestJSON(struct {
		Contract     string        `json:"contract"`
		Kind         string        `json:"kind"`
		Target       ReleaseTarget `json:"target"`
		Approval     ApprovalKey   `json:"approval,omitempty"`
		ValuesDigest string        `json:"valuesDigest,omitempty"`
		Source       string        `json:"sourceRevisionId,omitempty"`
	}{"helm-release-request.v1", kind, target, approval, valuesDigest, source})
}

func nullableReleaseID(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func cloneReleaseRevision(value ReleaseRevision) ReleaseRevision {
	value.ValuesYAML = append([]byte(nil), value.ValuesYAML...)
	return value
}

func cloneApprovalDocument(value ApprovalDocument) ApprovalDocument {
	value.ValuesSchemaJSON = append([]byte(nil), value.ValuesSchemaJSON...)
	value.DefaultValuesYAML = append([]byte(nil), value.DefaultValuesYAML...)
	return value
}

var _ ReleaseService = (*PostgresReleaseService)(nil)
var _ ReleaseValuesService = (*PostgresReleaseService)(nil)

func releaseStoreError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s Helm desired release: %w", action, err)
}
