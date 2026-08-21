package argo

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type observationRuntimeRow struct {
	namespace           string
	owner               string
	epoch               int64
	until               *time.Time
	snapshotVersion     string
	consecutiveFailures int
	failureCode         string
	nextPollAt          time.Time
	lastCompletedAt     *time.Time
	updatedAt           time.Time
}

const observationRuntimeSelect = `SELECT argo_namespace,lease_owner,lease_epoch,lease_until,snapshot_resource_version,consecutive_failures,last_failure_code,next_poll_at,last_completed_at,updated_at FROM argo_observation_runtime`

func scanObservationRuntime(row pgx.Row) (observationRuntimeRow, error) {
	var value observationRuntimeRow
	if err := row.Scan(&value.namespace, &value.owner, &value.epoch, &value.until, &value.snapshotVersion, &value.consecutiveFailures,
		&value.failureCode, &value.nextPollAt, &value.lastCompletedAt, &value.updatedAt); err != nil {
		return observationRuntimeRow{}, classifyPostgres(err)
	}
	return value, nil
}

func (s *PostgreSQLStore) ClaimObservation(ctx context.Context, namespace, owner string, now time.Time, leaseDuration time.Duration) (ObservationWork, error) {
	if !kubeRE.MatchString(namespace) || !observationOwnerRE.MatchString(owner) || now.IsZero() || leaseDuration < minimumObservationLease || leaseDuration > maximumObservationLease {
		return ObservationWork{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ObservationWork{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `INSERT INTO argo_observation_runtime(argo_namespace,next_poll_at,updated_at) VALUES($1,$2,$2) ON CONFLICT(argo_namespace) DO NOTHING`, namespace, now.UTC()); err != nil {
		return ObservationWork{}, classifyPostgres(err)
	}
	state, err := scanObservationRuntime(tx.QueryRow(ctx, observationRuntimeSelect+` WHERE argo_namespace=$1 FOR UPDATE`, namespace))
	if err != nil {
		return ObservationWork{}, err
	}
	if state.owner != "" && state.until != nil && state.until.After(now) {
		return ObservationWork{}, ErrLeaseHeld
	}
	if state.nextPollAt.After(now) {
		return ObservationWork{}, ErrNotFound
	}
	lease := ObservationLease{Namespace: namespace, Owner: owner, Epoch: state.epoch + 1, Until: now.UTC().Add(leaseDuration)}
	if _, err = tx.Exec(ctx, `UPDATE argo_observation_runtime SET lease_owner=$2,lease_epoch=$3,lease_until=$4,next_poll_at=$4,updated_at=$5 WHERE argo_namespace=$1`,
		namespace, owner, lease.Epoch, lease.Until, now.UTC()); err != nil {
		return ObservationWork{}, classifyPostgres(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return ObservationWork{}, classifyPostgres(err)
	}
	return ObservationWork{Lease: lease, ConsecutiveFailures: state.consecutiveFailures}, nil
}

func (s *PostgreSQLStore) HeartbeatObservation(ctx context.Context, lease ObservationLease, now time.Time, leaseDuration time.Duration) (ObservationLease, error) {
	if lease.Validate() != nil || now.IsZero() || leaseDuration < minimumObservationLease || leaseDuration > maximumObservationLease {
		return ObservationLease{}, ErrInvalid
	}
	until := now.UTC().Add(leaseDuration)
	result, err := s.pool.Exec(ctx, `UPDATE argo_observation_runtime SET next_poll_at=CASE WHEN next_poll_at=lease_until THEN $4 ELSE next_poll_at END,lease_until=$4,updated_at=$3
		WHERE argo_namespace=$1 AND lease_owner=$2 AND lease_epoch=$5 AND lease_until>$3`, lease.Namespace, lease.Owner, now.UTC(), until, lease.Epoch)
	if err != nil {
		return ObservationLease{}, classifyPostgres(err)
	}
	if result.RowsAffected() != 1 {
		return ObservationLease{}, ErrLeaseLost
	}
	lease.Until = until
	return lease, nil
}

func (s *PostgreSQLStore) PutObservationFenced(ctx context.Context, lease ObservationLease, value Observation, now time.Time) error {
	if lease.Validate() != nil || value.Validate() != nil || now.IsZero() || value.ArgoNamespace != lease.Namespace {
		return ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	state, err := scanObservationRuntime(tx.QueryRow(ctx, observationRuntimeSelect+` WHERE argo_namespace=$1 FOR UPDATE`, lease.Namespace))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrLeaseLost
		}
		return err
	}
	if state.owner != lease.Owner || state.epoch != lease.Epoch || state.until == nil || !state.until.After(now) {
		return ErrLeaseLost
	}
	if err = putObservationTx(ctx, tx, value); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return classifyPostgres(err)
	}
	return nil
}

func (s *PostgreSQLStore) FinishObservation(ctx context.Context, lease ObservationLease, outcome ObservationOutcome, now time.Time) error {
	if lease.Validate() != nil || outcome.Validate() != nil || now.IsZero() || outcome.NextPollAt.Before(now) {
		return ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	state, err := scanObservationRuntime(tx.QueryRow(ctx, observationRuntimeSelect+` WHERE argo_namespace=$1 FOR UPDATE`, lease.Namespace))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrLeaseLost
		}
		return err
	}
	if state.owner != lease.Owner || state.epoch != lease.Epoch || state.until == nil || !state.until.After(now) {
		return ErrLeaseLost
	}
	nextPollAt := outcome.NextPollAt.UTC()
	if !state.nextPollAt.After(now) {
		nextPollAt = now.UTC()
	}
	snapshot := state.snapshotVersion
	lastCompleted := state.lastCompletedAt
	if outcome.ConsecutiveFailures == 0 {
		snapshot = outcome.SnapshotVersion
		completed := now.UTC()
		lastCompleted = &completed
	}
	if _, err = tx.Exec(ctx, `UPDATE argo_observation_runtime SET lease_owner='',lease_until=NULL,snapshot_resource_version=$2,
		consecutive_failures=$3,last_failure_code=$4,next_poll_at=$5,last_completed_at=$6,updated_at=$7 WHERE argo_namespace=$1`,
		lease.Namespace, snapshot, outcome.ConsecutiveFailures, outcome.FailureCode, nextPollAt, lastCompleted, now.UTC()); err != nil {
		return classifyPostgres(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return classifyPostgres(err)
	}
	return nil
}

func (s *PostgreSQLStore) WakeObservation(ctx context.Context, namespace string, now time.Time) error {
	if s == nil || s.pool == nil || !kubeRE.MatchString(namespace) || now.IsZero() {
		return ErrInvalid
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO argo_observation_runtime(argo_namespace,next_poll_at,updated_at) VALUES($1,$2,$2)
		ON CONFLICT(argo_namespace) DO UPDATE SET next_poll_at=LEAST(argo_observation_runtime.next_poll_at,excluded.next_poll_at),updated_at=excluded.updated_at`, namespace, now.UTC())
	if err != nil {
		return classifyPostgres(err)
	}
	return nil
}

var _ ObservationRuntimeStore = (*PostgreSQLStore)(nil)
