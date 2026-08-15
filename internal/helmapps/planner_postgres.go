package helmapps

import (
	"context"
)

func (s *PostgresProtectedPublicationStore) NextPayloadCandidate(ctx context.Context) (PublicationCandidate, error) {
	if s == nil || s.pool == nil || ctx == nil {
		return PublicationCandidate{}, ErrInvalid
	}
	var candidate PublicationCandidate
	candidate.Kind = PublicationPayload
	err := s.pool.QueryRow(ctx, `SELECT release.id::text,release.project_id::text,
		release.environment_id::text,release.application_id::text
		FROM helm_release_heads head
		JOIN helm_release_revisions release ON release.id=head.revision_id
		LEFT JOIN helm_protected_payload_intents payload
		  ON payload.release_revision_id=release.id
		WHERE payload.id IS NULL AND (
		  release.desired_enabled=FALSE OR EXISTS(
		    SELECT 1 FROM helm_render_commands command
		    JOIN helm_render_results result ON result.command_id=command.id
		    WHERE command.id=release.render_command_id AND command.state='succeeded'
		      AND result.input_digest=command.input_digest
		  )
		)
		ORDER BY release.created_at,release.id LIMIT 1`).Scan(&candidate.ReleaseRevisionID,
		&candidate.Target.ProjectID, &candidate.Target.EnvironmentID,
		&candidate.Target.ApplicationID)
	if err != nil {
		return PublicationCandidate{}, classifyPostgres(err)
	}
	if candidate.Validate() != nil {
		return PublicationCandidate{}, ErrConflict
	}
	return candidate, nil
}

func (s *PostgresProtectedPublicationStore) NextApplicationCandidate(ctx context.Context,
	publisher ProtectedPublisherIdentity) (PublicationCandidate, error) {
	if s == nil || s.pool == nil || ctx == nil || publisher.Validate() != nil {
		return PublicationCandidate{}, ErrInvalid
	}
	var candidate PublicationCandidate
	candidate.Kind = PublicationApplication
	err := s.pool.QueryRow(ctx, `SELECT payload.release_revision_id::text,payload.id::text,
		COALESCE((SELECT CASE WHEN NOT EXISTS(
		    SELECT 1 FROM public.helm_protected_application_intents history
		    WHERE history.payload_intent_id=payload.id
		  ) THEN cascade.delete_intent_id::text ELSE '' END
		  FROM public.helm_application_cascade_preflights cascade
		  WHERE cascade.payload_intent_id=payload.id AND cascade.state='verified'
		    AND public.helm_application_cascade_observation_is_exact(
		      cascade.id,$1,pg_catalog.clock_timestamp()) LIMIT 1),''),
		payload.project_id::text,payload.environment_id::text,payload.application_id::text
		FROM helm_protected_payload_intents payload
		JOIN helm_release_heads head ON head.revision_id=payload.release_revision_id
		LEFT JOIN helm_protected_application_intents application
		  ON application.payload_intent_id=payload.id AND application.state<>'superseded'
		WHERE payload.state='verified' AND application.id IS NULL
		AND (payload.action='publish' OR EXISTS(
		  SELECT 1 FROM public.helm_application_cascade_preflights cascade
		  WHERE cascade.payload_intent_id=payload.id AND cascade.state='verified'
		    AND public.helm_application_cascade_observation_is_exact(
		      cascade.id,$1,pg_catalog.clock_timestamp())
		)) AND (
		  NOT EXISTS(SELECT 1 FROM helm_protected_application_intents prior
		    WHERE prior.payload_intent_id=payload.id) OR
		  EXISTS(SELECT 1 FROM helm_protected_application_intents prior
		    WHERE prior.payload_intent_id=payload.id AND prior.state='superseded'
		      AND prior.last_failure_code IN ('projection-superseded','cascade-migration-replan-required')
		      AND prior.lease_epoch=0 AND prior.attempts=0 AND prior.lease_owner IS NULL
		      AND prior.lease_until IS NULL AND prior.write_base_revision=''
		      AND prior.write_base_observed_at IS NULL AND prior.committed_revision=''
		      AND prior.committed_parent_revision='' AND prior.committed_at IS NULL
		      AND prior.verified_at IS NULL AND prior.verified_path_digest=''
		      AND prior.provider_request='' AND prior.completed_at IS NOT NULL
		      AND NOT EXISTS(SELECT 1 FROM helm_protected_application_intents newer
		        WHERE newer.payload_intent_id=prior.payload_intent_id AND
		          (newer.created_at,newer.id)>(prior.created_at,prior.id)))
		)
		ORDER BY payload.verified_at,payload.id LIMIT 1`, publisher.ConfigDigest).Scan(&candidate.ReleaseRevisionID,
		&candidate.PayloadIntentID, &candidate.ReservedIntentID, &candidate.Target.ProjectID,
		&candidate.Target.EnvironmentID, &candidate.Target.ApplicationID)
	if err != nil {
		return PublicationCandidate{}, classifyPostgres(err)
	}
	if candidate.Validate() != nil {
		return PublicationCandidate{}, ErrConflict
	}
	return candidate, nil
}

func (s *PostgresProtectedPublicationStore) NextCascadeCandidate(ctx context.Context) (PublicationCandidate, error) {
	if s == nil || s.pool == nil || ctx == nil {
		return PublicationCandidate{}, ErrInvalid
	}
	var candidate PublicationCandidate
	candidate.Kind = PublicationCascade
	err := s.pool.QueryRow(ctx, `SELECT payload.release_revision_id::text,payload.id::text,
		payload.project_id::text,payload.environment_id::text,payload.application_id::text
		FROM public.helm_protected_payload_intents payload
		JOIN public.helm_release_heads head ON head.revision_id=payload.release_revision_id
		LEFT JOIN public.helm_application_cascade_preflights cascade
		  ON cascade.payload_intent_id=payload.id AND cascade.state<>'superseded'
		WHERE payload.state='verified' AND payload.action='disable-receipt' AND cascade.id IS NULL
		AND (NOT EXISTS(SELECT 1 FROM public.helm_application_cascade_preflights prior
		  WHERE prior.payload_intent_id=payload.id) OR EXISTS(
		  SELECT 1 FROM public.helm_application_cascade_preflights prior
		  WHERE prior.payload_intent_id=payload.id AND prior.state='superseded'
		    AND prior.last_failure_code='cascade-projection-superseded'
		    AND prior.lease_epoch=0 AND prior.attempts=0 AND prior.lease_owner IS NULL
		    AND prior.lease_until IS NULL AND prior.write_base_revision=''
		    AND prior.write_base_observed_at IS NULL AND prior.committed_revision=''
		    AND prior.committed_parent_revision='' AND prior.committed_at IS NULL
		    AND prior.verified_at IS NULL AND prior.verified_path_digest=''
		    AND prior.provider_request='' AND prior.completed_at IS NOT NULL
		    AND NOT EXISTS(SELECT 1 FROM public.helm_application_cascade_preflights newer
		      WHERE newer.payload_intent_id=prior.payload_intent_id
		        AND (newer.created_at,newer.id)>(prior.created_at,prior.id))))
		ORDER BY payload.verified_at,payload.id LIMIT 1`).Scan(&candidate.ReleaseRevisionID,
		&candidate.PayloadIntentID, &candidate.Target.ProjectID,
		&candidate.Target.EnvironmentID, &candidate.Target.ApplicationID)
	if err != nil {
		return PublicationCandidate{}, classifyPostgres(err)
	}
	if candidate.Validate() != nil {
		return PublicationCandidate{}, ErrConflict
	}
	return candidate, nil
}
