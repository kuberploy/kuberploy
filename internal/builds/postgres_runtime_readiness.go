package builds

import (
	"context"
	"time"
)

func (s *PostgreSQLStore) ObserveSourceBuildWorker(ctx context.Context, observation SourceBuildWorkerObservation) error {
	if s == nil || s.pool == nil || observation.validate() != nil {
		return ErrInvalid
	}
	command, err := s.pool.Exec(ctx, `INSERT INTO runtime_readiness(runtime_kind,scope_key,worker_id,worker_epoch,
		contract_version,config_digest,identity,observation,started_at,observed_at,lease_until,updated_at)
		VALUES('source-build','global',$1,1,'source-build.v1',$2,
		jsonb_build_object('githubAppId',$3::bigint,'builderNamespace',$4::text,'builderAgentImage',$5::text),
		jsonb_build_object('builderCapacityReady',$6::boolean),$7,$8,$8::timestamptz+interval '5 minutes',$8)
		ON CONFLICT(runtime_kind,scope_key,worker_id) DO UPDATE SET worker_epoch=runtime_readiness.worker_epoch+1,
		config_digest=EXCLUDED.config_digest,identity=EXCLUDED.identity,observation=EXCLUDED.observation,started_at=EXCLUDED.started_at,
		observed_at=EXCLUDED.observed_at,lease_until=EXCLUDED.lease_until,updated_at=EXCLUDED.updated_at
		WHERE runtime_readiness.observed_at <= EXCLUDED.observed_at`,
		observation.WorkerID, observation.ConfigDigest, observation.GitHubAppID, observation.BuilderNamespace,
		observation.BuilderAgentImage, observation.BuilderCapacityReady, observation.StartedAt.UTC(), observation.ObservedAt.UTC())
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
		SELECT 1 FROM runtime_readiness
		WHERE runtime_kind='source-build' AND scope_key='global' AND config_digest=$1
		AND (identity->>'githubAppId')::bigint=$2 AND identity->>'builderNamespace'=$3 AND identity->>'builderAgentImage'=$4
		AND COALESCE((observation->>'builderCapacityReady')::boolean,false)
		AND observed_at >= $5 AND observed_at <= $6 AND lease_until>$6
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
