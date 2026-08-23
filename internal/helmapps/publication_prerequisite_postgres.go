package helmapps

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const publicationPrerequisiteColumns = `release_revision_id::text,project_id::text,
	environment_id::text,application_id::text,platform_binding_id::text,
	environment_binding_id::text,environment_revision,
	environment_generation,foundation_intent_id::text,foundation_revision,
	desired_state_command_id::text,desired_state_revision,planned_base_revision,created_at`

type prerequisiteRow interface{ Scan(...any) error }

func scanPublicationPrerequisite(row prerequisiteRow) (ProtectedPublicationPrerequisiteReceipt, error) {
	var value ProtectedPublicationPrerequisiteReceipt
	err := row.Scan(&value.ReleaseRevisionID, &value.ProjectID, &value.EnvironmentID,
		&value.ApplicationID, &value.PlatformBindingID, &value.EnvironmentBindingID,
		&value.EnvironmentRevision, &value.EnvironmentGeneration,
		&value.FoundationIntentID, &value.FoundationRevision, &value.DesiredStateCommandID,
		&value.DesiredStateRevision, &value.PlannedBaseRevision, &value.CreatedAt)
	if err != nil {
		return ProtectedPublicationPrerequisiteReceipt{}, classifyPostgres(err)
	}
	value.CreatedAt = value.CreatedAt.UTC()
	if value.Validate() != nil {
		return ProtectedPublicationPrerequisiteReceipt{}, ErrConflict
	}
	return value, nil
}

func ensurePublicationPrerequisite(ctx context.Context, tx pgx.Tx, release ReleaseRevision,
	binding ProtectedBindingSnapshot, authority ArgoMaterializationAuthority,
	now time.Time) (ProtectedPublicationPrerequisiteReceipt, error) {
	if ctx == nil || tx == nil || release.Validate() != nil || binding.Validate() != nil ||
		authority.Validate() != nil || now.IsZero() {
		return ProtectedPublicationPrerequisiteReceipt{}, ErrInvalid
	}
	existing, existingErr := scanPublicationPrerequisite(tx.QueryRow(ctx, `SELECT `+
		publicationPrerequisiteColumns+` FROM helm_publication_prerequisite_receipts
		WHERE release_revision_id=$1 FOR KEY SHARE`, release.ID))
	if existingErr == nil {
		if existing.ValidateFor(release.ID, release.Target, binding) != nil {
			return ProtectedPublicationPrerequisiteReceipt{}, ErrConflict
		}
		// A receipt is exact-current when inserted. Ordinary later foundation,
		// AppProject, environment, and platform-head progress must not strand
		// either publication phase; recheck only its immutable terminal identity.
		return publicationPrerequisite(ctx, tx, release.ID)
	}
	if !errors.Is(existingErr, ErrNotFound) {
		return ProtectedPublicationPrerequisiteReceipt{}, existingErr
	}
	var foundationID, foundationRevision string
	err := tx.QueryRow(ctx, `SELECT intent.id::text,intent.committed_revision
		FROM environment_foundation_intents intent
		WHERE intent.environment_id=$1 AND intent.project_id=$2 AND intent.active
		  AND intent.state='ready' AND intent.platform_binding_id=$3
		  AND intent.target_ref=$4
		  AND intent.committed_revision<>'' AND intent.published_at IS NOT NULL
		  AND intent.completed_at=intent.published_at
		ORDER BY intent.created_at DESC,intent.id DESC LIMIT 1 FOR KEY SHARE`,
		release.Target.EnvironmentID, release.Target.ProjectID, binding.PlatformBindingID,
		binding.PlatformTargetRef).Scan(&foundationID, &foundationRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProtectedPublicationPrerequisiteReceipt{}, ErrFoundationNotReady
	}
	if err != nil {
		return ProtectedPublicationPrerequisiteReceipt{}, classifyPostgres(err)
	}
	var desiredStateID, desiredStateRevision string
	err = tx.QueryRow(ctx, `SELECT command.id::text,command.committed_revision
		FROM argo_desired_state_materialization_receipts materialization
		JOIN argo_desired_state_commands command
		  ON command.id=materialization.desired_state_command_id
		JOIN environments environment ON environment.id=command.environment_id
		WHERE materialization.project_id=$1 AND materialization.environment_id=$2
		  AND materialization.platform_binding_id=$3
		  AND materialization.environment_binding_id=$4
		  AND materialization.platform_target_ref=$5
		  AND materialization.environment_target_ref=$6
		  AND materialization.environment_revision=$7
		  AND materialization.environment_generation=$8
		  AND materialization.policy_digest=$9
		  AND materialization.chart_repository=$10
		  AND materialization.chart_name=$11
		  AND materialization.chart_version=$12
		  AND materialization.chart_digest=$13
		  AND materialization.renderer_image=$14
		  AND materialization.chart_digest_enforcement=$15
		  AND command.project_id=$1 AND command.environment_id=$2
		  AND command.platform_binding_id=$3 AND command.environment_binding_id=$4
		  AND command.platform_target_ref=$5
		  AND command.environment_target_ref=$6 AND command.state='verified'
		  AND command.generation=materialization.desired_state_generation
		  AND command.committed_revision<>'' AND command.write_base_revision<>''
		  AND command.committed_revision=materialization.desired_state_revision
		  AND command.content_sha256=materialization.desired_state_content_sha256
		  AND command.verified_at IS NOT NULL AND command.completed_at=command.verified_at
		  AND command.argo_project=environment.argo_project
		  AND command.destination_namespace=environment.namespace
		  AND NOT EXISTS (
		    SELECT 1 FROM argo_desired_state_commands later
		    WHERE later.project_id=command.project_id
		      AND later.environment_id=command.environment_id
		      AND later.generation>command.generation
		      AND (later.state NOT IN ('failed','superseded') OR
		           later.completed_at IS NULL OR
		           later.completed_at>=materialization.created_at)
		  )
		ORDER BY materialization.created_at DESC,materialization.id DESC
		LIMIT 1 FOR KEY SHARE OF materialization,command`,
		release.Target.ProjectID, release.Target.EnvironmentID, binding.PlatformBindingID,
		binding.EnvironmentBindingID, binding.PlatformTargetRef,
		binding.EnvironmentTargetRef, binding.EnvironmentRevision,
		binding.EnvironmentGeneration, authority.PolicyDigest, authority.Runtime.ChartRepository,
		authority.Runtime.ChartName, authority.Runtime.ChartVersion, authority.Runtime.ChartDigest,
		authority.Runtime.RendererImage, authority.DigestEnforcement).Scan(&desiredStateID,
		&desiredStateRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProtectedPublicationPrerequisiteReceipt{}, ErrArgoProjectNotReady
	}
	if err != nil {
		return ProtectedPublicationPrerequisiteReceipt{}, classifyPostgres(err)
	}
	value := ProtectedPublicationPrerequisiteReceipt{
		ReleaseRevisionID: release.ID, ProjectID: release.Target.ProjectID,
		EnvironmentID: release.Target.EnvironmentID, ApplicationID: release.Target.ApplicationID,
		PlatformBindingID: binding.PlatformBindingID, EnvironmentBindingID: binding.EnvironmentBindingID,
		EnvironmentRevision:   binding.EnvironmentRevision,
		EnvironmentGeneration: binding.EnvironmentGeneration, FoundationIntentID: foundationID,
		FoundationRevision: foundationRevision, DesiredStateCommandID: desiredStateID,
		DesiredStateRevision: desiredStateRevision, PlannedBaseRevision: binding.PlannedBaseRevision,
		CreatedAt: now.UTC(),
	}
	if value.ValidateFor(release.ID, release.Target, binding) != nil {
		return ProtectedPublicationPrerequisiteReceipt{}, ErrConflict
	}
	result, err := tx.Exec(ctx, `INSERT INTO helm_publication_prerequisite_receipts(
		release_revision_id,project_id,environment_id,application_id,platform_binding_id,
		environment_binding_id,environment_revision,environment_generation,
		foundation_intent_id,foundation_revision,desired_state_command_id,
		desired_state_revision,planned_base_revision,created_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
	ON CONFLICT(release_revision_id) DO NOTHING`, value.ReleaseRevisionID, value.ProjectID,
		value.EnvironmentID, value.ApplicationID, value.PlatformBindingID,
		value.EnvironmentBindingID, value.EnvironmentRevision,
		value.EnvironmentGeneration, value.FoundationIntentID, value.FoundationRevision,
		value.DesiredStateCommandID, value.DesiredStateRevision, value.PlannedBaseRevision,
		value.CreatedAt)
	if err != nil {
		return ProtectedPublicationPrerequisiteReceipt{}, classifyPostgres(err)
	}
	if result.RowsAffected() == 1 {
		return value, nil
	}
	existing, err = scanPublicationPrerequisite(tx.QueryRow(ctx, `SELECT `+
		publicationPrerequisiteColumns+` FROM helm_publication_prerequisite_receipts
		WHERE release_revision_id=$1 FOR KEY SHARE`, release.ID))
	if err != nil {
		return ProtectedPublicationPrerequisiteReceipt{}, err
	}
	if existing.ValidateFor(release.ID, release.Target, binding) != nil ||
		existing.FoundationIntentID != value.FoundationIntentID ||
		existing.FoundationRevision != value.FoundationRevision ||
		existing.DesiredStateCommandID != value.DesiredStateCommandID ||
		existing.DesiredStateRevision != value.DesiredStateRevision {
		return ProtectedPublicationPrerequisiteReceipt{}, ErrConflict
	}
	return existing, nil
}

type prerequisiteQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func publicationPrerequisite(ctx context.Context, query prerequisiteQuerier,
	releaseID string) (ProtectedPublicationPrerequisiteReceipt, error) {
	if query == nil || ctx == nil || !uuidRE.MatchString(releaseID) {
		return ProtectedPublicationPrerequisiteReceipt{}, ErrInvalid
	}
	value, err := scanPublicationPrerequisite(query.QueryRow(ctx, `SELECT `+
		publicationPrerequisiteColumns+` FROM helm_publication_prerequisite_receipts
		WHERE release_revision_id=$1`, releaseID))
	if err != nil {
		return ProtectedPublicationPrerequisiteReceipt{}, err
	}
	// Currentness is an insertion-time invariant of the immutable receipt.
	// Rechecking mutable heads/generations here would strand already-authorized
	// work whenever ordinary environment projection advances. The publisher
	// instead proves both terminal receipt revisions are ancestors of its fresh
	// provider head immediately before every write and recovery.
	var foundationCurrent, desiredStateCurrent bool
	err = query.QueryRow(ctx, `SELECT
		EXISTS(SELECT 1 FROM environment_foundation_intents foundation
		  WHERE foundation.id=$1 AND foundation.environment_id=$2
		    AND foundation.project_id=$3 AND foundation.state IN ('ready','superseded')
		    AND foundation.platform_binding_id=$4
		    AND foundation.committed_revision=$5 AND foundation.published_at IS NOT NULL
		    AND foundation.completed_at=foundation.published_at),
		EXISTS(SELECT 1 FROM argo_desired_state_materialization_receipts materialization
		  JOIN argo_desired_state_commands command
		    ON command.id=materialization.desired_state_command_id
		  JOIN environments environment ON environment.id=command.environment_id
		  JOIN git_repository_bindings environment_binding
		    ON environment_binding.id=command.environment_binding_id
		  JOIN git_repository_bindings platform_binding
		    ON platform_binding.id=command.platform_binding_id
		  WHERE materialization.environment_binding_id=$7
		    AND materialization.environment_revision=$8
		    AND materialization.environment_generation=$9
		    AND materialization.project_id=$3 AND materialization.environment_id=$2
		    AND materialization.platform_binding_id=$4
		    AND materialization.desired_state_command_id=$6
		    AND materialization.desired_state_revision=$10
		    AND materialization.created_at<=$11
		    AND command.id=$6 AND command.project_id=$3 AND command.environment_id=$2
		    AND command.platform_binding_id=$4 AND command.environment_binding_id=$7
		    AND command.state='verified'
		    AND command.generation=materialization.desired_state_generation
		    AND command.committed_revision=$10 AND command.verified_at IS NOT NULL
		    AND command.content_sha256=materialization.desired_state_content_sha256
		    AND command.completed_at=command.verified_at
		    AND command.argo_project=environment.argo_project
		    AND command.destination_namespace=environment.namespace
		    AND environment_binding.kind='environment'
		    AND environment_binding.credential_mode='github-app'
		    AND environment_binding.project_id=command.project_id
		    AND environment_binding.environment_id=command.environment_id
		    AND environment_binding.target_ref=command.environment_target_ref
		    AND platform_binding.kind='platform'
		    AND platform_binding.credential_mode='github-app'
		    AND platform_binding.target_ref=command.platform_target_ref)`,
		value.FoundationIntentID, value.EnvironmentID, value.ProjectID,
		value.PlatformBindingID, value.FoundationRevision,
		value.DesiredStateCommandID, value.EnvironmentBindingID, value.EnvironmentRevision,
		value.EnvironmentGeneration, value.DesiredStateRevision, value.CreatedAt).Scan(&foundationCurrent,
		&desiredStateCurrent)
	if err != nil {
		return ProtectedPublicationPrerequisiteReceipt{}, classifyPostgres(err)
	}
	if !foundationCurrent {
		return ProtectedPublicationPrerequisiteReceipt{}, ErrFoundationNotReady
	}
	if !desiredStateCurrent {
		return ProtectedPublicationPrerequisiteReceipt{}, ErrArgoProjectNotReady
	}
	return value, nil
}

func (s *PostgresProtectedPublicationStore) PublicationPrerequisite(ctx context.Context,
	releaseID string) (ProtectedPublicationPrerequisiteReceipt, error) {
	if s == nil || s.pool == nil {
		return ProtectedPublicationPrerequisiteReceipt{}, ErrInvalid
	}
	return publicationPrerequisite(ctx, s.pool, releaseID)
}
