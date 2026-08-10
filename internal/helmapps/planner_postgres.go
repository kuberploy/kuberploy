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

func (s *PostgresProtectedPublicationStore) NextApplicationCandidate(ctx context.Context) (PublicationCandidate, error) {
	if s == nil || s.pool == nil || ctx == nil {
		return PublicationCandidate{}, ErrInvalid
	}
	var candidate PublicationCandidate
	candidate.Kind = PublicationApplication
	err := s.pool.QueryRow(ctx, `SELECT payload.release_revision_id::text,payload.id::text,
		payload.project_id::text,payload.environment_id::text,payload.application_id::text
		FROM helm_protected_payload_intents payload
		JOIN helm_release_heads head ON head.revision_id=payload.release_revision_id
		LEFT JOIN helm_protected_application_intents application
		  ON application.payload_intent_id=payload.id
		WHERE payload.state='verified' AND application.id IS NULL
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
