package certissuers

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *PostgresStore) AcquireObserverReadiness(ctx context.Context, observation ObserverWorkerObservation, duration time.Duration) (ObserverReadinessLease, error) {
	if s == nil || s.pool == nil || observation.Validate() != nil || !validObserverLeaseDuration(duration) {
		return ObserverReadinessLease{}, ErrObservationUnavailable
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ObserverReadinessLease{}, err
	}
	defer tx.Rollback(context.Background())
	until := observation.ObservedAt.Add(duration)
	result, err := tx.Exec(ctx, `INSERT INTO cert_manager_issuer_observer_readiness(
		worker_id,contract_version,config_digest,target_digest,target_count,started_at,observed_at,lease_epoch,lease_until,updated_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,1,$8,$7) ON CONFLICT(config_digest) DO NOTHING`,
		observation.WorkerID, observation.Identity.ContractVersion, observation.Identity.ConfigDigest, observation.TargetDigest,
		observation.TargetCount, observation.StartedAt, observation.ObservedAt, until)
	if err != nil {
		return ObserverReadinessLease{}, err
	}
	lease := ObserverReadinessLease{ObserverWorkerObservation: observation, Epoch: 1, Until: until}
	if result.RowsAffected() == 1 {
		if err = tx.Commit(ctx); err != nil {
			return ObserverReadinessLease{}, err
		}
		return lease, nil
	}
	current, err := loadObserverReadinessForUpdate(ctx, tx, observation.Identity.ConfigDigest)
	if err != nil {
		return ObserverReadinessLease{}, err
	}
	if current.Until.After(observation.ObservedAt) || observation.StartedAt.Before(current.StartedAt) {
		return ObserverReadinessLease{}, ErrObserverLeaseLost
	}
	lease.Epoch = current.Epoch + 1
	result, err = tx.Exec(ctx, `UPDATE cert_manager_issuer_observer_readiness SET
		worker_id=$1,contract_version=$2,target_digest=$4,target_count=$5,started_at=$6,observed_at=$7,
		lease_epoch=$8,lease_until=$9,updated_at=$7
		WHERE config_digest=$3 AND lease_epoch=$10 AND lease_until=$11`,
		observation.WorkerID, observation.Identity.ContractVersion, observation.Identity.ConfigDigest, observation.TargetDigest,
		observation.TargetCount, observation.StartedAt, observation.ObservedAt, lease.Epoch, until, current.Epoch, current.Until)
	if err != nil {
		return ObserverReadinessLease{}, err
	}
	if result.RowsAffected() != 1 {
		return ObserverReadinessLease{}, ErrObserverLeaseLost
	}
	if err = tx.Commit(ctx); err != nil {
		return ObserverReadinessLease{}, err
	}
	return lease, nil
}

func (s *PostgresStore) HeartbeatObserverReadiness(ctx context.Context, lease ObserverReadinessLease, observation ObserverWorkerObservation, duration time.Duration) (ObserverReadinessLease, error) {
	if s == nil || s.pool == nil || lease.Validate() != nil || observation.Validate() != nil || !validObserverLeaseDuration(duration) ||
		observation.WorkerID != lease.WorkerID || !observerIdentityEqual(observation.Identity, lease.Identity) ||
		!observation.StartedAt.Equal(lease.StartedAt) || observation.ObservedAt.Before(lease.ObservedAt) {
		return ObserverReadinessLease{}, ErrObservationUnavailable
	}
	if !observation.ObservedAt.Before(lease.Until) {
		return ObserverReadinessLease{}, ErrObserverLeaseLost
	}
	updated := ObserverReadinessLease{ObserverWorkerObservation: observation, Epoch: lease.Epoch, Until: observation.ObservedAt.Add(duration)}
	var returnedEpoch int64
	var returnedUntil time.Time
	err := s.pool.QueryRow(ctx, `UPDATE cert_manager_issuer_observer_readiness SET
		target_digest=$1,target_count=$2,observed_at=$3,lease_until=$4,updated_at=$3
		WHERE config_digest=$5 AND worker_id=$6 AND contract_version=$7 AND started_at=$8 AND lease_epoch=$9 AND lease_until=$10 AND lease_until>$3
		RETURNING lease_epoch,lease_until`, observation.TargetDigest, observation.TargetCount, observation.ObservedAt, updated.Until,
		observation.Identity.ConfigDigest, observation.WorkerID, observation.Identity.ContractVersion, observation.StartedAt, lease.Epoch, lease.Until).
		Scan(&returnedEpoch, &returnedUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return ObserverReadinessLease{}, ErrObserverLeaseLost
	}
	if err != nil {
		return ObserverReadinessLease{}, err
	}
	if returnedEpoch != updated.Epoch || !returnedUntil.Equal(updated.Until) {
		return ObserverReadinessLease{}, ErrObserverLeaseLost
	}
	updated.Until = returnedUntil.UTC()
	return updated, nil
}

func (s *PostgresStore) ObserverRuntimeReady(ctx context.Context, identity ObserverRuntimeIdentity, targetDigest string, targetCount int, now time.Time, maximumAge time.Duration) error {
	if s == nil || s.pool == nil || identity.Validate() != nil || !digestRE.MatchString(targetDigest) || targetCount < 0 ||
		targetCount > MaximumObservedIssuers || now.IsZero() || now.Location() != time.UTC || maximumAge < 10*time.Second || maximumAge > 15*time.Minute {
		return ErrObservationUnavailable
	}
	var ready bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM cert_manager_issuer_observer_readiness
		WHERE contract_version=$1 AND config_digest=$2 AND target_digest=$3 AND target_count=$4
		  AND observed_at<=$5 AND observed_at>=$6 AND lease_until>$5
	)`, identity.ContractVersion, identity.ConfigDigest, targetDigest, targetCount, now, now.Add(-maximumAge)).Scan(&ready)
	if err != nil {
		return err
	}
	if !ready {
		return ErrObservationUnavailable
	}
	return nil
}

func loadObserverReadinessForUpdate(ctx context.Context, tx pgx.Tx, configDigest string) (ObserverReadinessLease, error) {
	var lease ObserverReadinessLease
	err := tx.QueryRow(ctx, `SELECT worker_id,contract_version,config_digest,target_digest,target_count,started_at,observed_at,lease_epoch,lease_until
		FROM cert_manager_issuer_observer_readiness WHERE config_digest=$1 FOR UPDATE`, configDigest).
		Scan(&lease.WorkerID, &lease.Identity.ContractVersion, &lease.Identity.ConfigDigest, &lease.TargetDigest, &lease.TargetCount,
			&lease.StartedAt, &lease.ObservedAt, &lease.Epoch, &lease.Until)
	if errors.Is(err, pgx.ErrNoRows) {
		return ObserverReadinessLease{}, ErrObserverLeaseLost
	}
	if err != nil {
		return ObserverReadinessLease{}, err
	}
	lease.StartedAt = lease.StartedAt.UTC()
	lease.ObservedAt = lease.ObservedAt.UTC()
	lease.Until = lease.Until.UTC()
	if lease.Validate() != nil {
		return ObserverReadinessLease{}, ErrObservationUnavailable
	}
	return lease, nil
}

var _ ObserverReadinessStore = (*PostgresStore)(nil)
