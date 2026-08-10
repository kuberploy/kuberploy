package builds

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *PostgreSQLStore) ClaimNextReleaseProjection(ctx context.Context, owner string, now time.Time, leaseDuration time.Duration) (ReleaseProjectionWork, error) {
	if s == nil || s.pool == nil || !validOwnerLease(owner, leaseDuration) || now.IsZero() {
		return ReleaseProjectionWork{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return ReleaseProjectionWork{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	current := now.UTC()
	_, err = tx.Exec(ctx, `UPDATE build_release_projections
		SET state='failed',failure_code='projection-attempts-exhausted',lease_owner=NULL,lease_until=NULL,completed_at=$1,updated_at=$1
		WHERE attempts>=20 AND available_at<=$1 AND (state='pending' OR (state='processing' AND lease_until<=$1))`, current)
	if err != nil {
		return ReleaseProjectionWork{}, classifyPostgres(err)
	}
	var attemptID string
	err = tx.QueryRow(ctx, `SELECT p.attempt_id::text
		FROM build_release_projections p
		JOIN build_attempts a ON a.id=p.attempt_id AND a.state='succeeded'
		WHERE p.attempts<20 AND p.available_at<=$1
		  AND (p.state='pending' OR (p.state='processing' AND p.lease_until<=$1))
		ORDER BY p.available_at,p.created_at,p.attempt_id
		FOR UPDATE OF p SKIP LOCKED LIMIT 1`, current).Scan(&attemptID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReleaseProjectionWork{}, ErrNotFound
	}
	if err != nil {
		return ReleaseProjectionWork{}, classifyPostgres(err)
	}
	lease := ReleaseProjectionLease{AttemptID: attemptID, Owner: owner, Until: current.Add(leaseDuration)}
	var attempts int
	err = tx.QueryRow(ctx, `UPDATE build_release_projections SET
		state='processing',attempts=attempts+1,lease_owner=$2,lease_until=$3,lease_epoch=lease_epoch+1,
		failure_code='',updated_at=$4
		WHERE attempt_id=$1 RETURNING lease_epoch,attempts`, attemptID, owner, lease.Until, current).Scan(&lease.Epoch, &attempts)
	if err != nil {
		return ReleaseProjectionWork{}, classifyPostgres(err)
	}
	attempt, err := attemptByIDQuery(ctx, tx, attemptID, false)
	if err != nil {
		return ReleaseProjectionWork{}, err
	}
	definition, err := definitionByIDQuery(ctx, tx, attempt.DefinitionID, false)
	if err != nil {
		return ReleaseProjectionWork{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ReleaseProjectionWork{}, err
	}
	return ReleaseProjectionWork{Attempt: attempt, Definition: definition, Lease: lease, Attempts: attempts}, nil
}

func (s *PostgreSQLStore) HeartbeatReleaseProjection(ctx context.Context, lease ReleaseProjectionLease, now time.Time, duration time.Duration) (ReleaseProjectionLease, error) {
	if s == nil || s.pool == nil || !validProjectionLease(lease) || !validOwnerLease(lease.Owner, duration) || now.IsZero() {
		return ReleaseProjectionLease{}, ErrInvalid
	}
	current := now.UTC()
	until := current.Add(duration)
	command, err := s.pool.Exec(ctx, `UPDATE build_release_projections SET lease_until=$5,updated_at=$4
		WHERE attempt_id=$1 AND state='processing' AND lease_owner=$2 AND lease_epoch=$3 AND lease_until=$6 AND lease_until>$4`,
		lease.AttemptID, lease.Owner, lease.Epoch, current, until, lease.Until.UTC())
	if err != nil {
		return ReleaseProjectionLease{}, classifyPostgres(err)
	}
	if command.RowsAffected() != 1 {
		return ReleaseProjectionLease{}, projectionLeaseError(lease)
	}
	lease.Until = until
	return lease, nil
}

func (s *PostgreSQLStore) RetryReleaseProjection(ctx context.Context, lease ReleaseProjectionLease, code string, now, availableAt time.Time) (bool, error) {
	if s == nil || s.pool == nil || !validProjectionLease(lease) || validateFailureCode(code) != nil || now.IsZero() || availableAt.Before(now.UTC()) {
		return false, ErrInvalid
	}
	current := now.UTC()
	var state ReleaseProjectionState
	err := s.pool.QueryRow(ctx, `UPDATE build_release_projections SET
		state=CASE WHEN attempts>=20 THEN 'failed' ELSE 'pending' END,
		available_at=CASE WHEN attempts>=20 THEN available_at ELSE $6 END,
		failure_code=$5,lease_owner=NULL,lease_until=NULL,updated_at=$4,
		completed_at=CASE WHEN attempts>=20 THEN $4 ELSE NULL END
		WHERE attempt_id=$1 AND state='processing' AND lease_owner=$2 AND lease_epoch=$3 AND lease_until=$7 AND lease_until>$4
		RETURNING state`, lease.AttemptID, lease.Owner, lease.Epoch, current, code, availableAt.UTC(), lease.Until.UTC()).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, projectionLeaseError(lease)
	}
	return state == ReleaseProjectionPending, classifyPostgres(err)
}

func (s *PostgreSQLStore) FailReleaseProjection(ctx context.Context, lease ReleaseProjectionLease, code string, now time.Time) error {
	if s == nil || s.pool == nil || !validProjectionLease(lease) || validateFailureCode(code) != nil || now.IsZero() {
		return ErrInvalid
	}
	current := now.UTC()
	command, err := s.pool.Exec(ctx, `UPDATE build_release_projections SET
		state='failed',failure_code=$5,lease_owner=NULL,lease_until=NULL,completed_at=$4,updated_at=$4
		WHERE attempt_id=$1 AND state='processing' AND lease_owner=$2 AND lease_epoch=$3 AND lease_until=$6 AND lease_until>$4`,
		lease.AttemptID, lease.Owner, lease.Epoch, current, code, lease.Until.UTC())
	if err != nil {
		return classifyPostgres(err)
	}
	if command.RowsAffected() != 1 {
		return projectionLeaseError(lease)
	}
	return nil
}

func (s *PostgreSQLStore) CompleteReleaseProjection(ctx context.Context, lease ReleaseProjectionLease, releaseID, cacheGenerationID string, now time.Time) error {
	if s == nil || s.pool == nil || !validProjectionLease(lease) || !uuidRE.MatchString(releaseID) || cacheGenerationID != "" && !uuidRE.MatchString(cacheGenerationID) || now.IsZero() {
		return ErrInvalid
	}
	current := now.UTC()
	var cache any
	if cacheGenerationID != "" {
		cache = cacheGenerationID
	}
	command, err := s.pool.Exec(ctx, `UPDATE build_release_projections SET
		state='succeeded',failure_code='',release_id=$5,cache_generation_id=$6,
		lease_owner=NULL,lease_until=NULL,completed_at=$4,updated_at=$4
		WHERE attempt_id=$1 AND state='processing' AND lease_owner=$2 AND lease_epoch=$3 AND lease_until=$7 AND lease_until>$4`,
		lease.AttemptID, lease.Owner, lease.Epoch, current, releaseID, cache, lease.Until.UTC())
	if err != nil {
		return classifyPostgres(err)
	}
	if command.RowsAffected() != 1 {
		return projectionLeaseError(lease)
	}
	return nil
}
