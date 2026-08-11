package postgres

import (
	"context"
	"time"

	"github.com/kuberploy/kuberploy/internal/registry"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func (s *Store) AcquireManagedRegistryReadiness(ctx context.Context, observation registry.RuntimeWorkerObservation, duration time.Duration) (registry.RuntimeReadinessLease, error) {
	if s == nil || s.pool == nil || observation.Validate() != nil || !validRegistryRuntimeDuration(duration) {
		return registry.RuntimeReadinessLease{}, registry.ErrRegistryRuntimeNotReady
	}
	observation.StartedAt = databaseTime(observation.StartedAt)
	observation.ObservedAt = databaseTime(observation.ObservedAt)
	lease := registry.RuntimeReadinessLease{RuntimeWorkerObservation: observation, Until: databaseTime(observation.ObservedAt.Add(duration))}
	err := s.pool.QueryRow(ctx, `INSERT INTO runtime_readiness(runtime_kind,scope_key,registry_target_id,
		worker_id,worker_epoch,contract_version,config_digest,identity,observation,started_at,observed_at,lease_until,updated_at
	) VALUES('managed-registry',$1::text,$1::uuid,$2,1,$3,$4,'{}'::jsonb,'{}'::jsonb,$5,$6,$7,$6)
	ON CONFLICT(runtime_kind,scope_key,worker_id) DO UPDATE SET
		worker_epoch=runtime_readiness.worker_epoch+1,
		contract_version=EXCLUDED.contract_version,config_digest=EXCLUDED.config_digest,
		started_at=EXCLUDED.started_at,observed_at=EXCLUDED.observed_at,lease_until=EXCLUDED.lease_until,updated_at=EXCLUDED.updated_at
	RETURNING worker_epoch`, observation.TargetID, observation.WorkerID, observation.ContractVersion,
		observation.ConfigDigest, observation.StartedAt, observation.ObservedAt, lease.Until).Scan(&lease.Epoch)
	if err != nil {
		return registry.RuntimeReadinessLease{}, classify(err)
	}
	return lease, nil
}

func (s *Store) HeartbeatManagedRegistryReadiness(ctx context.Context, lease registry.RuntimeReadinessLease, observedAt time.Time, duration time.Duration) (registry.RuntimeReadinessLease, error) {
	if s == nil || s.pool == nil || lease.Validate() != nil || observedAt.IsZero() || observedAt.Before(lease.ObservedAt) || !validRegistryRuntimeDuration(duration) {
		return registry.RuntimeReadinessLease{}, registry.ErrRegistryRuntimeNotReady
	}
	observedAt = databaseTime(observedAt)
	until := databaseTime(observedAt.Add(duration))
	command, err := s.pool.Exec(ctx, `UPDATE runtime_readiness SET observed_at=$6,lease_until=$7,updated_at=$6
		WHERE runtime_kind='managed-registry' AND scope_key=$1::text AND registry_target_id=$1::uuid AND worker_id=$2 AND worker_epoch=$3
		AND contract_version=$4 AND config_digest=$5 AND started_at=$8
		AND observed_at<=$6 AND lease_until>$6`, lease.TargetID, lease.WorkerID, lease.Epoch,
		lease.ContractVersion, lease.ConfigDigest, observedAt, until, databaseTime(lease.StartedAt))
	if err != nil {
		return registry.RuntimeReadinessLease{}, classify(err)
	}
	if command.RowsAffected() != 1 {
		return registry.RuntimeReadinessLease{}, base.ErrRegistryLeaseLost
	}
	lease.ObservedAt = observedAt
	lease.Until = until
	return lease, nil
}

func (s *Store) ManagedRegistryRuntimeReady(ctx context.Context, identity registry.RuntimeIdentity, now time.Time, maximumAge time.Duration) error {
	if s == nil || s.pool == nil || identity.Validate() != nil || now.IsZero() || maximumAge < time.Second || maximumAge > 5*time.Minute {
		return registry.ErrRegistryRuntimeNotReady
	}
	now = databaseTime(now)
	var ready bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM runtime_readiness r
		JOIN registry_targets t ON t.id=r.registry_target_id AND t.mode='managed'
		WHERE r.runtime_kind='managed-registry' AND r.scope_key=$1::text AND r.registry_target_id=$1::uuid AND r.contract_version=$2 AND r.config_digest=$3
		AND t.endpoint=$4 AND t.repository_prefix=$5
		AND t.pull_credential_ref<>$6 AND t.push_credential_ref<>$6 AND t.cache_credential_ref<>$6
		AND r.observed_at >= $7 AND r.observed_at <= $8 AND r.lease_until > $9
	)`, identity.TargetID, identity.ContractVersion, identity.ConfigDigest, identity.TargetEndpoint,
		identity.TargetRepositoryPrefix, identity.LifecycleCredentialRef, now.Add(-maximumAge),
		now.Add(registry.ManagedRegistryReadinessClockSkew), now).Scan(&ready)
	if err != nil {
		return classify(err)
	}
	if !ready {
		return registry.ErrRegistryRuntimeNotReady
	}
	return nil
}
