package autodeploy

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *PostgreSQLStore) AcquireRuntimeReadiness(ctx context.Context, observation RuntimeObservation, duration time.Duration) (RuntimeLease, error) {
	if s == nil || s.pool == nil || observation.validate() != nil || duration < 15*time.Second || duration > 5*time.Minute {
		return RuntimeLease{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return RuntimeLease{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var epoch int64
	err = tx.QueryRow(ctx, `SELECT lease_epoch FROM auto_deploy_runtime_readiness WHERE worker_id=$1 FOR UPDATE`, observation.WorkerID).Scan(&epoch)
	lease := RuntimeLease{RuntimeObservation: observation, Epoch: 1, Until: observation.ObservedAt.UTC().Add(duration)}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		_, err = tx.Exec(ctx, `INSERT INTO auto_deploy_runtime_readiness(worker_id,contract_version,operator_config_digest,started_at,observed_at,lease_epoch,lease_until)
			VALUES($1,$2,$3,$4,$5,1,$6)`, observation.WorkerID, observation.ContractVersion, observation.OperatorConfigDigest,
			observation.StartedAt.UTC(), observation.ObservedAt.UTC(), lease.Until)
	case err != nil:
		return RuntimeLease{}, classifyPostgres(err)
	default:
		lease.Epoch = epoch + 1
		_, err = tx.Exec(ctx, `UPDATE auto_deploy_runtime_readiness SET contract_version=$2,operator_config_digest=$3,started_at=$4,
			observed_at=$5,lease_epoch=$6,lease_until=$7 WHERE worker_id=$1`, observation.WorkerID, observation.ContractVersion,
			observation.OperatorConfigDigest, observation.StartedAt.UTC(), observation.ObservedAt.UTC(), lease.Epoch, lease.Until)
	}
	if err != nil {
		return RuntimeLease{}, classifyPostgres(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return RuntimeLease{}, classifyPostgres(err)
	}
	return lease, nil
}

func (s *PostgreSQLStore) HeartbeatRuntimeReadiness(ctx context.Context, lease RuntimeLease, observedAt time.Time, duration time.Duration) (RuntimeLease, error) {
	if s == nil || s.pool == nil || lease.RuntimeObservation.validate() != nil || lease.Epoch < 1 || lease.Until.IsZero() ||
		observedAt.IsZero() || duration < 15*time.Second || duration > 5*time.Minute {
		return RuntimeLease{}, ErrInvalid
	}
	until := observedAt.UTC().Add(duration)
	result, err := s.pool.Exec(ctx, `UPDATE auto_deploy_runtime_readiness SET observed_at=$7,lease_until=$8
		WHERE worker_id=$1 AND contract_version=$2 AND operator_config_digest=$3 AND started_at=$4 AND lease_epoch=$5 AND lease_until>$6`,
		lease.WorkerID, lease.ContractVersion, lease.OperatorConfigDigest, lease.StartedAt.UTC(), lease.Epoch, observedAt.UTC(), observedAt.UTC(), until)
	if err != nil {
		return RuntimeLease{}, classifyPostgres(err)
	}
	if result.RowsAffected() != 1 {
		return RuntimeLease{}, ErrLeaseLost
	}
	lease.ObservedAt, lease.Until = observedAt.UTC(), until
	return lease, nil
}

func (s *PostgreSQLStore) RuntimeReady(ctx context.Context, identity RuntimeIdentity, now time.Time, maximumAge time.Duration) error {
	if s == nil || s.pool == nil || identity.Validate() != nil || now.IsZero() || maximumAge < 2*RuntimeHeartbeatPeriod || maximumAge > 5*time.Minute {
		return ErrInvalid
	}
	var ready bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM auto_deploy_runtime_readiness
		WHERE contract_version=$1 AND operator_config_digest=$2 AND observed_at<=$4 AND observed_at>=$5 AND lease_until>$3)`,
		identity.ContractVersion, identity.OperatorConfigDigest, now.UTC(), now.UTC().Add(maximumRuntimeClockSkew), now.UTC().Add(-maximumAge)).Scan(&ready)
	if err != nil {
		return classifyPostgres(err)
	}
	if !ready {
		return ErrRuntimeNotReady
	}
	return nil
}

var _ RuntimeReadinessStore = (*PostgreSQLStore)(nil)
