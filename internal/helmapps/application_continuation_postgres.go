package helmapps

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/argo"
)

const applicationContinuationColumns = `application_intent_id::text,release_revision_id::text,payload_intent_id::text,
	project_id::text,environment_id::text,application_id::text,platform_binding_id::text,
	environment_binding_id::text,source_environment_revision,
	source_environment_generation,source_foundation_intent_id::text,source_foundation_revision,
	source_desired_state_command_id::text,source_desired_state_revision,
	source_desired_state_content_digest,current_environment_revision,current_environment_generation,
	current_foundation_intent_id::text,current_foundation_revision,
	current_materialization_receipt_id::text,current_desired_state_command_id::text,
	current_desired_state_revision,current_desired_state_content_digest,planned_base_revision,current_policy_digest,
	current_chart_repository,current_chart_name,current_chart_version,current_chart_digest,
	current_renderer_image,current_chart_digest_enforcement,current_app_project_content,
	application_content_digest,application_intent_digest,created_at`

func scanApplicationContinuation(row rowScanner) (ProtectedApplicationContinuationReceipt, error) {
	var value ProtectedApplicationContinuationReceipt
	err := row.Scan(&value.ApplicationIntentID, &value.ReleaseRevisionID, &value.PayloadIntentID, &value.ProjectID,
		&value.EnvironmentID, &value.ApplicationID, &value.PlatformBindingID,
		&value.EnvironmentBindingID, &value.SourceEnvironmentRevision,
		&value.SourceEnvironmentGeneration, &value.SourceFoundationIntentID,
		&value.SourceFoundationRevision, &value.SourceDesiredStateCommandID,
		&value.SourceDesiredStateRevision, &value.SourceDesiredStateContentDigest,
		&value.CurrentEnvironmentRevision, &value.CurrentEnvironmentGeneration,
		&value.CurrentFoundationIntentID, &value.CurrentFoundationRevision,
		&value.CurrentMaterializationReceiptID, &value.CurrentDesiredStateCommandID,
		&value.CurrentDesiredStateRevision, &value.CurrentDesiredStateContentDigest,
		&value.PlannedBaseRevision, &value.CurrentPolicyDigest,
		&value.CurrentRuntime.ChartRepository, &value.CurrentRuntime.ChartName,
		&value.CurrentRuntime.ChartVersion, &value.CurrentRuntime.ChartDigest,
		&value.CurrentRuntime.RendererImage, &value.CurrentChartDigestEnforcement,
		&value.CurrentAppProjectContent,
		&value.ApplicationContentDigest,
		&value.ApplicationIntentDigest, &value.CreatedAt)
	if err != nil {
		return ProtectedApplicationContinuationReceipt{}, classifyPostgres(err)
	}
	value.CreatedAt = value.CreatedAt.UTC()
	if value.Validate() != nil {
		return ProtectedApplicationContinuationReceipt{}, ErrConflict
	}
	return value, nil
}

func (s *PostgresProtectedPublicationStore) ApplicationContinuation(ctx context.Context,
	intentID string) (ProtectedApplicationContinuationReceipt, error) {
	if !uuidRE.MatchString(intentID) {
		return ProtectedApplicationContinuationReceipt{}, ErrInvalid
	}
	return scanApplicationContinuation(s.pool.QueryRow(ctx, `SELECT `+applicationContinuationColumns+`
		FROM public.helm_application_continuation_receipts WHERE application_intent_id=$1`, intentID))
}

func ensureApplicationContinuation(ctx context.Context, tx pgx.Tx, release ReleaseRevision,
	payload ProtectedPayloadIntent, application ProtectedApplicationIntent, authority ArgoMaterializationAuthority,
	now time.Time) (ProtectedApplicationContinuationReceipt, error) {
	if ctx == nil || tx == nil || release.Validate() != nil || payload.Validate() != nil ||
		payload.State != ProtectedVerified || application.Validate() != nil || authority.Validate() != nil || now.IsZero() ||
		application.ID == "" || application.ReleaseRevisionID != release.ID || application.PayloadIntentID != payload.ID {
		return ProtectedApplicationContinuationReceipt{}, ErrInvalid
	}
	value, err := scanApplicationContinuation(tx.QueryRow(ctx, `SELECT `+applicationContinuationColumns+`
		FROM public.helm_application_continuation_receipts WHERE application_intent_id=$1 FOR KEY SHARE`, application.ID))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return ProtectedApplicationContinuationReceipt{}, err
	}
	if errors.Is(err, ErrNotFound) {
		_, err = tx.Exec(ctx, `INSERT INTO public.helm_application_continuation_receipts(
		application_intent_id,release_revision_id,payload_intent_id,project_id,environment_id,application_id,
		platform_binding_id,environment_binding_id,source_environment_revision,
		source_environment_generation,source_foundation_intent_id,source_foundation_revision,
		source_desired_state_command_id,source_desired_state_revision,source_desired_state_content_digest,
		current_environment_revision,current_environment_generation,current_foundation_intent_id,
		current_foundation_revision,current_materialization_receipt_id,current_desired_state_command_id,
		current_desired_state_revision,current_desired_state_content_digest,planned_base_revision,
		current_policy_digest,current_chart_repository,current_chart_name,current_chart_version,
		current_chart_digest,current_renderer_image,current_chart_digest_enforcement,current_app_project_content,
		application_content_digest,application_intent_digest,created_at)
		SELECT $11,release.id,payload.id,release.project_id,release.environment_id,release.application_id,
			payload.platform_binding_id,payload.environment_binding_id,
			prerequisite.environment_revision,prerequisite.environment_generation,
			prerequisite.foundation_intent_id,prerequisite.foundation_revision,
			prerequisite.desired_state_command_id,prerequisite.desired_state_revision,
			source_command.content_sha256,environment.indexed_revision,environment.projection_generation,
			foundation.id,foundation.committed_revision,materialization.id,
			materialization.desired_state_command_id,materialization.desired_state_revision,
			materialization.desired_state_content_sha256,platform.target_head_revision,
			materialization.policy_digest,materialization.chart_repository,materialization.chart_name,
			materialization.chart_version,materialization.chart_digest,materialization.renderer_image,
			materialization.chart_digest_enforcement,materialization.app_project_content,$12,$13,$4
		FROM public.helm_release_revisions release
		JOIN public.helm_release_heads head ON head.revision_id=release.id
			AND head.generation=release.generation
		JOIN public.helm_protected_payload_intents payload ON payload.id=$2
			AND payload.release_revision_id=release.id AND payload.state='verified'
		JOIN public.helm_publication_prerequisite_receipts prerequisite
			ON prerequisite.release_revision_id=release.id
		JOIN public.argo_desired_state_commands source_command
			ON source_command.id=prerequisite.desired_state_command_id
			AND source_command.state='verified'
			AND source_command.committed_revision=prerequisite.desired_state_revision
		JOIN public.git_repository_bindings platform ON platform.id=payload.platform_binding_id
			AND platform.kind='platform' AND platform.credential_mode='github-app'
			AND platform.state IN ('ready','indexing') AND platform.target_head_revision IS NOT NULL
		JOIN public.git_repository_bindings environment ON environment.id=payload.environment_binding_id
			AND environment.kind='environment' AND environment.credential_mode='github-app'
			AND environment.state='ready' AND environment.target_head_revision=environment.indexed_revision
			AND environment.indexed_revision IS NOT NULL AND environment.projection_generation>0
		JOIN public.git_projection_generations generation ON generation.binding_id=environment.id
			AND generation.generation=environment.projection_generation
			AND generation.head_revision=environment.indexed_revision AND generation.state='active'
		JOIN public.environments desired_environment ON desired_environment.id=release.environment_id
			AND desired_environment.project_id=release.project_id
		JOIN public.environment_foundation_intents foundation
			ON foundation.environment_id=release.environment_id AND foundation.project_id=release.project_id
			AND foundation.active AND foundation.state='ready'
			AND foundation.platform_binding_id=payload.platform_binding_id
			AND foundation.target_ref=payload.platform_target_ref
			AND foundation.namespace=desired_environment.namespace
			AND foundation.argo_project=desired_environment.argo_project
			AND foundation.committed_revision<>'' AND foundation.published_at IS NOT NULL
		JOIN public.argo_desired_state_materialization_receipts materialization
			ON materialization.environment_binding_id=environment.id
			AND materialization.environment_revision=environment.indexed_revision
			AND materialization.environment_generation=environment.projection_generation
			AND materialization.policy_digest=$3
			AND materialization.chart_repository=$5 AND materialization.chart_name=$6
			AND materialization.chart_version=$7 AND materialization.chart_digest=$8
			AND materialization.renderer_image=$9 AND materialization.chart_digest_enforcement=$10
			AND materialization.app_project_content IS NOT NULL
		JOIN public.argo_desired_state_commands current_command
			ON current_command.id=materialization.desired_state_command_id
			AND current_command.project_id=release.project_id
			AND current_command.environment_id=release.environment_id
			AND current_command.platform_binding_id=payload.platform_binding_id
			AND current_command.environment_binding_id=payload.environment_binding_id
			AND current_command.platform_target_ref=payload.platform_target_ref
			AND current_command.environment_target_ref=payload.environment_target_ref
			AND current_command.state='verified'
			AND current_command.committed_revision=materialization.desired_state_revision
			AND current_command.content_sha256=materialization.desired_state_content_sha256
			AND current_command.chart_repository=materialization.chart_repository
			AND current_command.chart_name=materialization.chart_name
			AND current_command.chart_version=materialization.chart_version
			AND current_command.chart_digest=materialization.chart_digest
			AND current_command.renderer_image=materialization.renderer_image
			AND current_command.chart_digest_enforcement=materialization.chart_digest_enforcement
			AND current_command.app_project_content=materialization.app_project_content
		WHERE release.id=$1 AND release.project_id=payload.project_id
			AND release.environment_id=payload.environment_id AND release.application_id=payload.application_id
			AND prerequisite.project_id=release.project_id
			AND prerequisite.environment_id=release.environment_id
			AND prerequisite.application_id=release.application_id
			AND prerequisite.platform_binding_id=payload.platform_binding_id
			AND prerequisite.environment_binding_id=payload.environment_binding_id
			AND prerequisite.environment_revision=payload.environment_revision
			AND prerequisite.environment_generation=payload.environment_generation
			AND source_command.project_id=release.project_id
			AND source_command.environment_id=release.environment_id
			AND current_command.argo_project=desired_environment.argo_project
			AND current_command.destination_namespace=desired_environment.namespace
			AND platform.target_ref=payload.platform_target_ref
			AND environment.project_id=release.project_id
			AND environment.environment_id=release.environment_id
			AND environment.target_ref=payload.environment_target_ref
			AND NOT EXISTS(SELECT 1 FROM public.git_projected_documents invalid
				WHERE invalid.binding_id=environment.id AND invalid.generation=environment.projection_generation
				AND NOT invalid.valid)
			AND NOT EXISTS(
				SELECT 1
				FROM public.argo_desired_state_materialization_receipts newer_materialization
				JOIN public.argo_desired_state_commands newer_command
				  ON newer_command.id=newer_materialization.desired_state_command_id
				 AND newer_command.state='verified'
				 AND newer_command.committed_revision=newer_materialization.desired_state_revision
				 AND newer_command.content_sha256=newer_materialization.desired_state_content_sha256
				WHERE newer_materialization.environment_binding_id=environment.id
				  AND newer_materialization.environment_revision=environment.indexed_revision
				  AND newer_materialization.environment_generation=environment.projection_generation
				  AND (newer_materialization.desired_state_generation,
				       newer_materialization.created_at,newer_materialization.id)>
				      (materialization.desired_state_generation,
				       materialization.created_at,materialization.id)
			)
		ORDER BY materialization.created_at DESC,materialization.id DESC LIMIT 1
		ON CONFLICT(application_intent_id) DO NOTHING`, release.ID, payload.ID,
			authority.PolicyDigest, now.UTC(), authority.Runtime.ChartRepository,
			authority.Runtime.ChartName, authority.Runtime.ChartVersion, authority.Runtime.ChartDigest,
			authority.Runtime.RendererImage, authority.DigestEnforcement, application.ID,
			application.ContentDigest, application.IntentDigest)
		if err != nil {
			return ProtectedApplicationContinuationReceipt{}, classifyPostgres(err)
		}
		value, err = scanApplicationContinuation(tx.QueryRow(ctx, `SELECT `+applicationContinuationColumns+`
			FROM public.helm_application_continuation_receipts WHERE application_intent_id=$1 FOR KEY SHARE`, application.ID))
		if err != nil {
			return ProtectedApplicationContinuationReceipt{}, err
		}
	}
	canonicalAuthority := argo.AppProjectAuthority{ProjectID: value.ProjectID,
		EnvironmentID: value.EnvironmentID, EnvironmentBindingID: value.EnvironmentBindingID,
		Runtime: value.CurrentRuntime}
	err = tx.QueryRow(ctx, `SELECT environment.namespace,environment.argo_project,
		command.argo_namespace,environment_binding.provider,environment_binding.installation_id,
		environment_binding.repository_id,environment_binding.repository_owner,
		environment_binding.repository_name,platform_binding.provider,platform_binding.installation_id,
		platform_binding.repository_id,platform_binding.repository_owner,platform_binding.repository_name
		FROM public.argo_desired_state_commands command
		JOIN public.environments environment ON environment.id=command.environment_id
		JOIN public.git_repository_bindings environment_binding ON environment_binding.id=command.environment_binding_id
		JOIN public.git_repository_bindings platform_binding ON platform_binding.id=command.platform_binding_id
		WHERE command.id=$1 AND environment.id=$2 AND environment.project_id=$3
		  AND environment_binding.id=$4 AND platform_binding.id=$5
		FOR KEY SHARE OF command,environment,environment_binding,platform_binding`,
		value.CurrentDesiredStateCommandID, value.EnvironmentID, value.ProjectID,
		value.EnvironmentBindingID, value.PlatformBindingID).Scan(&canonicalAuthority.Namespace,
		&canonicalAuthority.ArgoProject, &canonicalAuthority.ArgoNamespace,
		&canonicalAuthority.EnvironmentRepository.Provider,
		&canonicalAuthority.EnvironmentRepository.InstallationID,
		&canonicalAuthority.EnvironmentRepository.RepositoryID,
		&canonicalAuthority.EnvironmentRepository.Owner, &canonicalAuthority.EnvironmentRepository.Name,
		&canonicalAuthority.PlatformRepository.Provider, &canonicalAuthority.PlatformRepository.InstallationID,
		&canonicalAuthority.PlatformRepository.RepositoryID,
		&canonicalAuthority.PlatformRepository.Owner, &canonicalAuthority.PlatformRepository.Name)
	if err != nil {
		return ProtectedApplicationContinuationReceipt{}, classifyPostgres(err)
	}
	canonicalAppProject, err := argo.RenderAppProjectAuthority(canonicalAuthority)
	if err != nil || !bytes.Equal(canonicalAppProject, value.CurrentAppProjectContent) {
		return ProtectedApplicationContinuationReceipt{}, ErrConflict
	}
	if value.ApplicationIntentID != application.ID || value.PayloadIntentID != payload.ID ||
		value.ReleaseRevisionID != release.ID || value.ProjectID != release.Target.ProjectID ||
		value.EnvironmentID != release.Target.EnvironmentID || value.ApplicationID != release.Target.ApplicationID ||
		value.PlatformBindingID != payload.Binding.PlatformBindingID ||
		value.EnvironmentBindingID != payload.Binding.EnvironmentBindingID ||
		value.SourceEnvironmentRevision != payload.Binding.EnvironmentRevision ||
		value.SourceEnvironmentGeneration != payload.Binding.EnvironmentGeneration ||
		value.ApplicationContentDigest != application.ContentDigest || value.ApplicationIntentDigest != application.IntentDigest ||
		value.PlannedBaseRevision != application.Binding.PlannedBaseRevision ||
		value.CurrentPolicyDigest != authority.PolicyDigest || value.CurrentRuntime != authority.Runtime ||
		value.CurrentChartDigestEnforcement != authority.DigestEnforcement {
		return ProtectedApplicationContinuationReceipt{}, ErrConflict
	}
	return value, nil
}
