package builds

import (
	"context"
	"time"
)

func (s *PostgreSQLStore) ObserveSourceBuildWorker(ctx context.Context, observation SourceBuildWorkerObservation) error {
	if s == nil || s.pool == nil || observation.validate() != nil {
		return ErrInvalid
	}
	command, err := s.pool.Exec(ctx, `INSERT INTO source_build_runtime_readiness(worker_id,config_digest,github_app_id,builder_namespace,builder_agent_image,started_at,observed_at)
		VALUES($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT(worker_id) DO UPDATE SET config_digest=EXCLUDED.config_digest,github_app_id=EXCLUDED.github_app_id,
		builder_namespace=EXCLUDED.builder_namespace,builder_agent_image=EXCLUDED.builder_agent_image,
		started_at=EXCLUDED.started_at,observed_at=EXCLUDED.observed_at
		WHERE source_build_runtime_readiness.observed_at <= EXCLUDED.observed_at`,
		observation.WorkerID, observation.ConfigDigest, observation.GitHubAppID, observation.BuilderNamespace,
		observation.BuilderAgentImage, observation.StartedAt.UTC(), observation.ObservedAt.UTC())
	if err != nil {
		return classifyPostgres(err)
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

func (s *PostgreSQLStore) SourceBuildRuntimeReady(ctx context.Context, identity SourceBuildRuntimeIdentity, now time.Time, maximumAge time.Duration) error {
	if s == nil || s.pool == nil || identity.validate() != nil || now.IsZero() || maximumAge < time.Second || maximumAge > 5*time.Minute {
		return ErrInvalid
	}
	var exists bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM source_build_runtime_readiness
		WHERE config_digest=$1 AND github_app_id=$2 AND builder_namespace=$3 AND builder_agent_image=$4
		AND observed_at >= $5 AND observed_at <= $6
	)`, identity.ConfigDigest, identity.GitHubAppID, identity.BuilderNamespace, identity.BuilderAgentImage,
		now.UTC().Add(-maximumAge), now.UTC().Add(5*time.Second)).Scan(&exists)
	if err != nil {
		return classifyPostgres(err)
	}
	if !exists {
		return ErrRuntimeNotReady
	}
	return nil
}
