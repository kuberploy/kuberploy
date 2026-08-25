package certificates

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func (s *PostgreSQLStore) ClaimCertificateObservation(
	ctx context.Context,
	identity ObservationIdentity,
	owner string,
	namespaces []string,
	namespacePrefixes []string,
	now time.Time,
	duration time.Duration,
) (ObservationWork, error) {
	normalized, err := NormalizeObservationNamespaces(namespaces)
	normalizedPrefixes, prefixErr := NormalizeObservationNamespacePrefixes(namespacePrefixes)
	if s == nil || s.pool == nil || identity.Validate() != nil || !observationWorkerIDRE.MatchString(owner) ||
		err != nil || prefixErr != nil || len(namespaces)+len(namespacePrefixes) == 0 || !slices.Equal(normalized, namespaces) || !slices.Equal(normalizedPrefixes, namespacePrefixes) || now.IsZero() || duration < 20*time.Second || duration > time.Hour {
		return ObservationWork{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return ObservationWork{}, ErrObservationUnavailable
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var bindingID, versionID string
	var versionNumber int64
	var observationVersion sql.NullString
	err = tx.QueryRow(ctx, `SELECT b.id::text,v.id::text,v.version_number,o.version_id::text
		FROM secret_bindings b
		JOIN secret_binding_versions v ON v.binding_id=b.id AND v.version_number=b.active_version
		JOIN tls_certificate_versions c ON c.version_id=v.id AND c.binding_id=b.id
		LEFT JOIN tls_certificate_observations o ON o.version_id=v.id AND o.binding_id=b.id
		WHERE b.purpose='tls-certificate' AND b.provider='sealed-secrets' AND b.state='ready'
		  AND v.provider='sealed-secrets' AND v.target_secret_type='kubernetes.io/tls' AND v.state='active'
		  AND (b.target_namespace=ANY($1::text[]) OR EXISTS (SELECT 1 FROM unnest($2::text[]) prefix WHERE b.target_namespace LIKE prefix || '%' AND length(b.target_namespace)>length(prefix)))
		  AND (o.version_id IS NULL OR (o.next_observation_at<=$3 AND (o.lease_until IS NULL OR o.lease_until<=$3)))
		ORDER BY COALESCE(o.next_observation_at,c.created_at),v.id
		FOR UPDATE OF b,v SKIP LOCKED LIMIT 1`, namespaces, namespacePrefixes, now.UTC()).Scan(
		&bindingID, &versionID, &versionNumber, &observationVersion,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ObservationWork{}, ErrNotFound
	}
	if err != nil {
		return ObservationWork{}, ErrObservationUnavailable
	}
	candidate, err := readActiveReferenceCandidateTx(ctx, tx, bindingID, versionNumber)
	if err != nil || candidate.Version.ID != versionID {
		if err == nil {
			err = ErrConflict
		}
		return ObservationWork{}, err
	}
	targetDigest, err := CertificateObservationTargetDigest(candidate.Binding, candidate.Version, candidate.Certificate)
	if err != nil {
		return ObservationWork{}, ErrConflict
	}
	if !observationVersion.Valid {
		_, err = tx.Exec(ctx, `INSERT INTO tls_certificate_observations(
			version_id,binding_id,target_digest,state,next_observation_at,created_at,updated_at
		) VALUES($1,$2,$3,'awaiting',$4,$4,$4)`, versionID, bindingID, targetDigest, now.UTC())
		if err != nil {
			return ObservationWork{}, ErrObservationUnavailable
		}
	}
	var epoch int64
	var claimedAt, until time.Time
	var failures int
	err = tx.QueryRow(ctx, `UPDATE tls_certificate_observations
		SET lease_owner=$4,lease_epoch=lease_epoch+1,lease_claimed_at=$5,lease_until=$6,
		    lease_contract_version=$7,lease_config_digest=$8,lease_target_digest=$3,updated_at=$5
		WHERE version_id=$1 AND binding_id=$2 AND target_digest=$3
		  AND next_observation_at<=$5 AND (lease_until IS NULL OR lease_until<=$5)
		RETURNING lease_epoch,lease_claimed_at,lease_until,consecutive_failures`,
		versionID, bindingID, targetDigest, owner, now.UTC(), now.UTC().Add(duration),
		identity.ContractVersion, identity.ConfigDigest).Scan(&epoch, &claimedAt, &until, &failures)
	if errors.Is(err, pgx.ErrNoRows) {
		return ObservationWork{}, ErrObservationLeaseLost
	}
	if err != nil {
		return ObservationWork{}, ErrObservationUnavailable
	}
	work := ObservationWork{
		Binding: candidate.Binding, SecretVersion: cloneObservedSecretVersion(candidate.Version),
		Attestation: cloneVersion(candidate.Certificate), ConsecutiveFailures: failures,
		Lease: ObservationLease{BindingID: bindingID, VersionID: versionID, Owner: owner, Epoch: epoch,
			ClaimedAt: claimedAt.UTC(), Until: until.UTC(), Identity: identity, TargetDigest: targetDigest},
	}
	if work.Validate() != nil {
		return ObservationWork{}, ErrConflict
	}
	if err = tx.Commit(ctx); err != nil {
		return ObservationWork{}, ErrObservationUnavailable
	}
	return work, nil
}

func (s *PostgreSQLStore) HeartbeatCertificateObservation(ctx context.Context, lease ObservationLease, now time.Time, duration time.Duration) (ObservationLease, error) {
	if s == nil || s.pool == nil || lease.Validate() != nil || now.IsZero() || duration < 20*time.Second || duration > time.Hour {
		return ObservationLease{}, ErrInvalid
	}
	var until time.Time
	err := s.pool.QueryRow(ctx, `UPDATE tls_certificate_observations SET lease_until=$11,updated_at=$10
		WHERE version_id=$1 AND binding_id=$2 AND target_digest=$3 AND lease_owner=$4 AND lease_epoch=$5
		  AND lease_claimed_at=$6 AND lease_until=$7 AND lease_contract_version=$8 AND lease_config_digest=$9
		  AND lease_target_digest=$3 AND lease_until>$10
		RETURNING lease_until`, lease.VersionID, lease.BindingID, lease.TargetDigest, lease.Owner, lease.Epoch,
		lease.ClaimedAt, lease.Until, lease.Identity.ContractVersion, lease.Identity.ConfigDigest,
		now.UTC(), now.UTC().Add(duration)).Scan(&until)
	if errors.Is(err, pgx.ErrNoRows) {
		return ObservationLease{}, ErrObservationLeaseLost
	}
	if err != nil {
		return ObservationLease{}, ErrObservationUnavailable
	}
	lease.Until = until.UTC()
	return lease, nil
}

func (s *PostgreSQLStore) ApplyCertificateObservationReady(ctx context.Context, lease ObservationLease, outcome ObservationReadyOutcome, now time.Time) error {
	if s == nil || s.pool == nil || lease.Validate() != nil ||
		validateObservationTime(outcome.ObservedAt, outcome.NextAt, now, lease.ClaimedAt) != nil {
		return ErrInvalid
	}
	return s.applyCertificateObservation(ctx, lease, ObservationReady, "", outcome.ObservedAt, outcome.NextAt, now)
}

func (s *PostgreSQLStore) ApplyCertificateObservationDegraded(ctx context.Context, lease ObservationLease, outcome ObservationDegradedOutcome, now time.Time) error {
	if s == nil || s.pool == nil || lease.Validate() != nil || !outcome.FailureCode.valid() ||
		validateObservationTime(outcome.ObservedAt, outcome.NextAt, now, lease.ClaimedAt) != nil {
		return ErrInvalid
	}
	return s.applyCertificateObservation(ctx, lease, ObservationDegraded, outcome.FailureCode, outcome.ObservedAt, outcome.NextAt, now)
}

func (s *PostgreSQLStore) RequeueCertificateObservation(ctx context.Context, lease ObservationLease, outcome ObservationRequeueOutcome, now time.Time) error {
	if s == nil || s.pool == nil || lease.Validate() != nil || !outcome.FailureCode.valid() || now.IsZero() ||
		!outcome.NextAt.After(now) || outcome.NextAt.After(now.Add(24*time.Hour)) {
		return ErrInvalid
	}
	return s.applyCertificateObservation(ctx, lease, ObservationRequeue, outcome.FailureCode, time.Time{}, outcome.NextAt, now)
}

func (s *PostgreSQLStore) applyCertificateObservation(
	ctx context.Context,
	lease ObservationLease,
	state ObservationState,
	failure ObservationFailureCode,
	observedAt, nextAt, now time.Time,
) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return ErrObservationUnavailable
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = lockCertificateObservationLeaseTx(ctx, tx, lease, now); err != nil {
		return err
	}
	var command pgconn.CommandTag
	switch state {
	case ObservationReady:
		command, err = tx.Exec(ctx, `UPDATE tls_certificate_observations
			SET observation_contract_version=$8,observation_config_digest=$9,state='ready',next_observation_at=$10,
			    consecutive_failures=0,failure_code='',last_observed_at=$11,last_ready_at=$11,
			    lease_owner=NULL,lease_claimed_at=NULL,lease_until=NULL,lease_contract_version=NULL,
			    lease_config_digest=NULL,lease_target_digest=NULL,updated_at=$12
			WHERE version_id=$1 AND binding_id=$2 AND target_digest=$3 AND lease_owner=$4 AND lease_epoch=$5
			  AND lease_claimed_at=$6 AND lease_until=$7`, lease.VersionID, lease.BindingID, lease.TargetDigest,
			lease.Owner, lease.Epoch, lease.ClaimedAt, lease.Until, lease.Identity.ContractVersion,
			lease.Identity.ConfigDigest, nextAt.UTC(), observedAt.UTC(), now.UTC())
	case ObservationDegraded:
		command, err = tx.Exec(ctx, `UPDATE tls_certificate_observations
			SET observation_contract_version=$8,observation_config_digest=$9,state='degraded',next_observation_at=$10,
			    consecutive_failures=LEAST(30,consecutive_failures+1),failure_code=$11,last_observed_at=$12,
			    lease_owner=NULL,lease_claimed_at=NULL,lease_until=NULL,lease_contract_version=NULL,
			    lease_config_digest=NULL,lease_target_digest=NULL,updated_at=$13
			WHERE version_id=$1 AND binding_id=$2 AND target_digest=$3 AND lease_owner=$4 AND lease_epoch=$5
			  AND lease_claimed_at=$6 AND lease_until=$7`, lease.VersionID, lease.BindingID, lease.TargetDigest,
			lease.Owner, lease.Epoch, lease.ClaimedAt, lease.Until, lease.Identity.ContractVersion,
			lease.Identity.ConfigDigest, nextAt.UTC(), failure, observedAt.UTC(), now.UTC())
	case ObservationRequeue:
		command, err = tx.Exec(ctx, `UPDATE tls_certificate_observations
			SET observation_contract_version=$8,observation_config_digest=$9,state='requeue',next_observation_at=$10,
			    consecutive_failures=LEAST(30,consecutive_failures+1),failure_code=$11,
			    lease_owner=NULL,lease_claimed_at=NULL,lease_until=NULL,lease_contract_version=NULL,
			    lease_config_digest=NULL,lease_target_digest=NULL,updated_at=$12
			WHERE version_id=$1 AND binding_id=$2 AND target_digest=$3 AND lease_owner=$4 AND lease_epoch=$5
			  AND lease_claimed_at=$6 AND lease_until=$7`, lease.VersionID, lease.BindingID, lease.TargetDigest,
			lease.Owner, lease.Epoch, lease.ClaimedAt, lease.Until, lease.Identity.ContractVersion,
			lease.Identity.ConfigDigest, nextAt.UTC(), failure, now.UTC())
	default:
		return ErrInvalid
	}
	if err != nil {
		return ErrObservationUnavailable
	}
	if command.RowsAffected() != 1 {
		return ErrObservationLeaseLost
	}
	if err = tx.Commit(ctx); err != nil {
		return ErrObservationUnavailable
	}
	return nil
}

func lockCertificateObservationLeaseTx(ctx context.Context, tx pgx.Tx, lease ObservationLease, now time.Time) error {
	var versionNumber int64
	err := tx.QueryRow(ctx, `SELECT v.version_number
		FROM tls_certificate_observations o
		JOIN secret_binding_versions v ON v.id=o.version_id AND v.binding_id=o.binding_id
		JOIN secret_bindings b ON b.id=o.binding_id AND b.active_version=v.version_number
		JOIN tls_certificate_versions c ON c.version_id=v.id AND c.binding_id=b.id
		WHERE o.version_id=$1 AND o.binding_id=$2 AND o.target_digest=$3 AND o.lease_owner=$4 AND o.lease_epoch=$5
		  AND o.lease_claimed_at=$6 AND o.lease_until=$7 AND o.lease_contract_version=$8
		  AND o.lease_config_digest=$9 AND o.lease_target_digest=$3 AND o.lease_until>$10
		  AND b.state='ready' AND b.purpose='tls-certificate' AND b.provider='sealed-secrets'
		  AND v.state='active' AND v.provider='sealed-secrets' AND v.target_secret_type='kubernetes.io/tls'
		FOR UPDATE OF o,b,v,c`, lease.VersionID, lease.BindingID, lease.TargetDigest, lease.Owner, lease.Epoch,
		lease.ClaimedAt, lease.Until, lease.Identity.ContractVersion, lease.Identity.ConfigDigest, now.UTC()).Scan(&versionNumber)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrObservationLeaseLost
	}
	if err != nil {
		return ErrObservationUnavailable
	}
	candidate, err := readActiveReferenceCandidateTx(ctx, tx, lease.BindingID, versionNumber)
	if err != nil {
		return ErrObservationLeaseLost
	}
	digest, err := CertificateObservationTargetDigest(candidate.Binding, candidate.Version, candidate.Certificate)
	if err != nil || digest != lease.TargetDigest {
		return ErrObservationLeaseLost
	}
	return nil
}

func (s *PostgreSQLStore) ActiveCertificateReady(ctx context.Context, bindingID, versionID string, identity ObservationIdentity, now time.Time, maximumAge time.Duration) error {
	if s == nil || s.pool == nil {
		return ErrObservationUnavailable
	}
	// ActiveCertificateReadyTx takes FOR SHARE locks so the observation and
	// active binding cannot change between readiness validation and target
	// digest verification. PostgreSQL rejects row locks in read-only
	// transactions, even though this path does not mutate application data.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return ErrObservationUnavailable
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = s.ActiveCertificateReadyTx(ctx, tx, bindingID, versionID, identity, now, maximumAge); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return ErrObservationUnavailable
	}
	return nil
}

func (s *PostgreSQLStore) ActiveCertificateReadyTx(ctx context.Context, tx pgx.Tx, bindingID, versionID string, identity ObservationIdentity, now time.Time, maximumAge time.Duration) error {
	if s == nil || s.pool == nil || tx == nil || !uuidRE.MatchString(bindingID) || !uuidRE.MatchString(versionID) ||
		identity.Validate() != nil || now.IsZero() || maximumAge < 10*time.Second || maximumAge > 30*time.Minute {
		return ErrObservationUnavailable
	}
	var versionNumber int64
	var targetDigest string
	err := tx.QueryRow(ctx, `SELECT v.version_number,o.target_digest
		FROM tls_certificate_observations o
		JOIN secret_binding_versions v ON v.id=o.version_id AND v.binding_id=o.binding_id
		JOIN secret_bindings b ON b.id=o.binding_id AND b.active_version=v.version_number
		WHERE o.binding_id=$1 AND o.version_id=$2 AND o.state='ready'
		  AND o.observation_contract_version=$3 AND o.observation_config_digest=$4
		  AND o.last_ready_at>=$5 AND o.last_ready_at<=$6
		  AND b.state='ready' AND b.purpose='tls-certificate' AND b.provider='sealed-secrets'
		  AND v.state='active' AND v.provider='sealed-secrets' AND v.target_secret_type='kubernetes.io/tls'
		FOR SHARE OF o,b,v`, bindingID, versionID, identity.ContractVersion, identity.ConfigDigest,
		now.UTC().Add(-maximumAge), now.UTC().Add(CertificateObservationReadinessSkew)).Scan(&versionNumber, &targetDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrObservationUnavailable
	}
	if err != nil {
		return ErrUnavailable
	}
	candidate, err := readActiveReferenceCandidateTx(ctx, tx, bindingID, versionNumber)
	if err != nil {
		return ErrObservationUnavailable
	}
	digest, err := CertificateObservationTargetDigest(candidate.Binding, candidate.Version, candidate.Certificate)
	if err != nil || digest != targetDigest || now.UTC().Before(candidate.Certificate.NotBefore) || !now.UTC().Before(candidate.Certificate.NotAfter) {
		return ErrObservationUnavailable
	}
	return nil
}

func (s *PostgreSQLStore) AcquireCertificateObservationReadiness(ctx context.Context, observation ObservationWorkerObservation, duration time.Duration) (ObservationReadinessLease, error) {
	if s == nil || s.pool == nil || observation.Validate() != nil || duration < 20*time.Second || duration > time.Hour {
		return ObservationReadinessLease{}, ErrInvalid
	}
	lease := ObservationReadinessLease{ObservationWorkerObservation: observation}
	err := s.pool.QueryRow(ctx, `INSERT INTO runtime_readiness(runtime_kind,scope_key,
		worker_id,worker_epoch,contract_version,config_digest,identity,observation,started_at,observed_at,lease_until,updated_at
	) VALUES('tls-certificate-observer','global',$1,1,$2,$3,'{}'::jsonb,'{}'::jsonb,$4,$5,$6,$5)
	ON CONFLICT (runtime_kind,scope_key,worker_id) DO UPDATE SET
		worker_epoch=runtime_readiness.worker_epoch+1,
		contract_version=EXCLUDED.contract_version,config_digest=EXCLUDED.config_digest,
		started_at=EXCLUDED.started_at,observed_at=EXCLUDED.observed_at,
		lease_until=EXCLUDED.lease_until,updated_at=EXCLUDED.updated_at
	RETURNING worker_epoch,started_at,observed_at,lease_until`, observation.WorkerID,
		observation.Identity.ContractVersion, observation.Identity.ConfigDigest, observation.StartedAt.UTC(),
		observation.ObservedAt.UTC(), observation.ObservedAt.UTC().Add(duration)).Scan(
		&lease.Epoch, &lease.StartedAt, &lease.ObservedAt, &lease.Until,
	)
	if err != nil {
		return ObservationReadinessLease{}, ErrObservationUnavailable
	}
	lease.StartedAt, lease.ObservedAt, lease.Until = lease.StartedAt.UTC(), lease.ObservedAt.UTC(), lease.Until.UTC()
	return lease, nil
}

func (s *PostgreSQLStore) HeartbeatCertificateObservationReadiness(ctx context.Context, lease ObservationReadinessLease, observedAt time.Time, duration time.Duration) (ObservationReadinessLease, error) {
	if s == nil || s.pool == nil || lease.Validate() != nil || observedAt.IsZero() || observedAt.Before(lease.ObservedAt) ||
		duration < 20*time.Second || duration > time.Hour {
		return ObservationReadinessLease{}, ErrInvalid
	}
	var storedObservedAt, until time.Time
	err := s.pool.QueryRow(ctx, `UPDATE runtime_readiness
		SET observed_at=$9,lease_until=$10,updated_at=$9
		WHERE runtime_kind='tls-certificate-observer' AND scope_key='global'
		  AND worker_id=$1 AND worker_epoch=$2 AND contract_version=$3 AND config_digest=$4
		  AND started_at=$5 AND observed_at=$6 AND lease_until=$7 AND updated_at=$6
		  AND lease_until>$8
		RETURNING observed_at,lease_until`, lease.WorkerID, lease.Epoch, lease.Identity.ContractVersion,
		lease.Identity.ConfigDigest, lease.StartedAt, lease.ObservedAt, lease.Until, observedAt.UTC(),
		observedAt.UTC(), observedAt.UTC().Add(duration)).Scan(&storedObservedAt, &until)
	if errors.Is(err, pgx.ErrNoRows) {
		return ObservationReadinessLease{}, ErrObservationLeaseLost
	}
	if err != nil {
		return ObservationReadinessLease{}, ErrObservationUnavailable
	}
	lease.ObservedAt, lease.Until = storedObservedAt.UTC(), until.UTC()
	return lease, nil
}

func (s *PostgreSQLStore) CertificateObservationRuntimeReady(ctx context.Context, identity ObservationIdentity, now time.Time, maximumAge time.Duration) error {
	if s == nil || s.pool == nil {
		return ErrObservationUnavailable
	}
	return s.certificateObservationRuntimeReadyQuery(ctx, s.pool, identity, now, maximumAge)
}

func (s *PostgreSQLStore) CertificateObservationRuntimeReadyTx(ctx context.Context, tx pgx.Tx, identity ObservationIdentity, now time.Time, maximumAge time.Duration) error {
	if s == nil || s.pool == nil || tx == nil {
		return ErrObservationUnavailable
	}
	return s.certificateObservationRuntimeReadyQuery(ctx, tx, identity, now, maximumAge)
}

type certificateReadinessQueryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (s *PostgreSQLStore) certificateObservationRuntimeReadyQuery(ctx context.Context, query certificateReadinessQueryer, identity ObservationIdentity, now time.Time, maximumAge time.Duration) error {
	if identity.Validate() != nil || now.IsZero() || maximumAge < 2*time.Second || maximumAge > 5*time.Minute {
		return ErrObservationUnavailable
	}
	var ready bool
	err := query.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM runtime_readiness
		WHERE runtime_kind='tls-certificate-observer' AND scope_key='global'
		  AND contract_version=$1 AND config_digest=$2 AND lease_until>$3
		  AND observed_at>=$4 AND observed_at<=$5
	)`, identity.ContractVersion, identity.ConfigDigest, now.UTC(), now.UTC().Add(-maximumAge),
		now.UTC().Add(CertificateObservationReadinessSkew)).Scan(&ready)
	if err != nil {
		return ErrUnavailable
	}
	if !ready {
		return ErrObservationUnavailable
	}
	return nil
}

var _ ObservationStore = (*PostgreSQLStore)(nil)
