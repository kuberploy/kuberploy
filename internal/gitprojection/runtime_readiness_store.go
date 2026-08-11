package gitprojection

import (
	"context"
	"time"
)

func validRuntimeLeaseDuration(duration time.Duration) bool {
	return duration >= 2*RuntimeHeartbeatInterval && duration <= 5*time.Minute
}

func (s *MemoryStore) AcquireRuntimeReadiness(_ context.Context, observation RuntimeWorkerObservation, duration time.Duration) (RuntimeLease, error) {
	if observation.Validate() != nil || !validRuntimeLeaseDuration(duration) {
		return RuntimeLease{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	epoch := int64(1)
	if current, exists := s.runtimeLeases[observation.WorkerID]; exists {
		epoch = current.Epoch + 1
	}
	lease := RuntimeLease{RuntimeWorkerObservation: observation, Epoch: epoch, Until: observation.ObservedAt.Add(duration)}
	s.runtimeLeases[observation.WorkerID] = lease
	return lease, nil
}

func (s *MemoryStore) HeartbeatRuntimeReadiness(_ context.Context, lease RuntimeLease, observedAt time.Time, duration time.Duration) (RuntimeLease, error) {
	if lease.Validate() != nil || observedAt.IsZero() || observedAt.Before(lease.ObservedAt) || !validRuntimeLeaseDuration(duration) {
		return RuntimeLease{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.runtimeLeases[lease.WorkerID]
	if !exists || current != lease || !current.Until.After(observedAt) {
		return RuntimeLease{}, ErrLeaseLost
	}
	current.ObservedAt, current.Until = observedAt.UTC(), observedAt.UTC().Add(duration)
	s.runtimeLeases[lease.WorkerID] = current
	return current, nil
}

func (s *MemoryStore) RuntimeReady(_ context.Context, identity RuntimeIdentity, now time.Time, maximumAge time.Duration) error {
	if identity.Validate() != nil || now.IsZero() || maximumAge < 2*RuntimeHeartbeatInterval || maximumAge > 5*time.Minute {
		return ErrRuntimeNotReady
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, lease := range s.runtimeLeases {
		if lease.RuntimeIdentity == identity && lease.Until.After(now) && !lease.ObservedAt.Before(now.Add(-maximumAge)) {
			return nil
		}
	}
	return ErrRuntimeNotReady
}

func (s *PostgreSQLStore) AcquireRuntimeReadiness(ctx context.Context, observation RuntimeWorkerObservation, duration time.Duration) (RuntimeLease, error) {
	if observation.Validate() != nil || !validRuntimeLeaseDuration(duration) {
		return RuntimeLease{}, ErrInvalid
	}
	var lease RuntimeLease
	err := s.pool.QueryRow(ctx, `INSERT INTO runtime_readiness(runtime_kind,scope_key,worker_id,worker_epoch,
		contract_version,config_digest,identity,observation,started_at,observed_at,lease_until,updated_at
	) VALUES('git-projection','global',$1,1,$2,$3,jsonb_build_object('githubAppId',$4::bigint),'{}'::jsonb,$5,$6,$7,$6)
	ON CONFLICT(runtime_kind,scope_key,worker_id) DO UPDATE SET worker_epoch=runtime_readiness.worker_epoch+1,
		contract_version=excluded.contract_version,config_digest=excluded.config_digest,identity=excluded.identity,
		started_at=excluded.started_at,observed_at=excluded.observed_at,lease_until=excluded.lease_until,updated_at=excluded.updated_at
	RETURNING worker_id,worker_epoch,contract_version,config_digest,(identity->>'githubAppId')::bigint,started_at,observed_at,lease_until`,
		observation.WorkerID, observation.ContractVersion, observation.ConfigDigest, observation.GitHubAppID,
		observation.StartedAt.UTC(), observation.ObservedAt.UTC(), observation.ObservedAt.UTC().Add(duration)).Scan(
		&lease.WorkerID, &lease.Epoch, &lease.ContractVersion, &lease.ConfigDigest, &lease.GitHubAppID,
		&lease.StartedAt, &lease.ObservedAt, &lease.Until)
	if err != nil {
		return RuntimeLease{}, classifyPostgres(err)
	}
	return lease, lease.Validate()
}

func (s *PostgreSQLStore) HeartbeatRuntimeReadiness(ctx context.Context, lease RuntimeLease, observedAt time.Time, duration time.Duration) (RuntimeLease, error) {
	if lease.Validate() != nil || observedAt.IsZero() || observedAt.Before(lease.ObservedAt) || !validRuntimeLeaseDuration(duration) {
		return RuntimeLease{}, ErrInvalid
	}
	var updated RuntimeLease
	err := s.pool.QueryRow(ctx, `UPDATE runtime_readiness SET observed_at=$8,lease_until=$9,updated_at=$8
		WHERE runtime_kind='git-projection' AND scope_key='global' AND worker_id=$1 AND worker_epoch=$2
		AND contract_version=$3 AND config_digest=$4 AND (identity->>'githubAppId')::bigint=$5
		AND started_at=$6 AND observed_at=$7 AND lease_until>$8
		RETURNING worker_id,worker_epoch,contract_version,config_digest,(identity->>'githubAppId')::bigint,started_at,observed_at,lease_until`,
		lease.WorkerID, lease.Epoch, lease.ContractVersion, lease.ConfigDigest, lease.GitHubAppID, lease.StartedAt,
		lease.ObservedAt, observedAt.UTC(), observedAt.UTC().Add(duration)).Scan(
		&updated.WorkerID, &updated.Epoch, &updated.ContractVersion, &updated.ConfigDigest, &updated.GitHubAppID,
		&updated.StartedAt, &updated.ObservedAt, &updated.Until)
	if err != nil {
		if classified := classifyPostgres(err); classified == ErrNotFound {
			return RuntimeLease{}, ErrLeaseLost
		} else {
			return RuntimeLease{}, classified
		}
	}
	return updated, updated.Validate()
}

func (s *PostgreSQLStore) RuntimeReady(ctx context.Context, identity RuntimeIdentity, now time.Time, maximumAge time.Duration) error {
	if identity.Validate() != nil || now.IsZero() || maximumAge < 2*RuntimeHeartbeatInterval || maximumAge > 5*time.Minute {
		return ErrRuntimeNotReady
	}
	var ready bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM runtime_readiness WHERE runtime_kind='git-projection' AND scope_key='global'
		AND contract_version=$1 AND config_digest=$2 AND (identity->>'githubAppId')::bigint=$3
		AND observed_at>=$4 AND observed_at<=$5 AND lease_until>$5
	)`, identity.ContractVersion, identity.ConfigDigest, identity.GitHubAppID, now.UTC().Add(-maximumAge), now.UTC()).Scan(&ready)
	if err != nil || !ready {
		return ErrRuntimeNotReady
	}
	return nil
}

var _ RuntimeReadinessStore = (*MemoryStore)(nil)
var _ RuntimeReadinessStore = (*PostgreSQLStore)(nil)
