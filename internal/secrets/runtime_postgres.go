package secrets

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *PostgreSQLStore) ClaimRuntimeSecret(ctx context.Context, identity RuntimeIdentity, owner string, namespaces []string, now time.Time, duration time.Duration) (RuntimeWork, error) {
	if identity.Validate() != nil || !runtimeSecretWorkerIDRE.MatchString(owner) || !exactRuntimeNamespaces(namespaces) ||
		now.IsZero() || duration < 20*time.Second || duration > time.Hour {
		return RuntimeWork{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return RuntimeWork{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var versionID, bindingID string
	var failures int
	var epoch int64
	err = tx.QueryRow(ctx, `SELECT r.version_id::text,r.binding_id::text,r.consecutive_failures,r.lease_epoch
		FROM secret_binding_runtime_reconciliations r
		JOIN secret_binding_versions v ON v.id=r.version_id AND v.binding_id=r.binding_id
		JOIN secret_bindings b ON b.id=r.binding_id
		WHERE r.runtime_state='awaiting' AND v.state='awaiting-readiness' AND v.provider='sealed-secrets'
		  AND b.provider='sealed-secrets' AND b.target_namespace=ANY($1::text[])
		  AND r.next_attempt_at<=$2 AND (r.lease_until IS NULL OR r.lease_until<=$2)
		ORDER BY r.next_attempt_at,r.version_id
		FOR UPDATE OF r,v,b SKIP LOCKED LIMIT 1`, namespaces, now.UTC()).Scan(&versionID, &bindingID, &failures, &epoch)
	if err != nil {
		return RuntimeWork{}, classifyPostgres(err)
	}
	var until time.Time
	err = tx.QueryRow(ctx, `UPDATE secret_binding_runtime_reconciliations
		SET lease_owner=$2,lease_epoch=lease_epoch+1,lease_until=$3,worker_contract=$4,
		    worker_config_digest=$5,updated_at=$6
		WHERE version_id=$1 AND runtime_state='awaiting'
		RETURNING lease_epoch,lease_until`, versionID, owner, now.UTC().Add(duration), identity.ContractVersion,
		identity.ConfigDigest, now.UTC()).Scan(&epoch, &until)
	if err != nil {
		return RuntimeWork{}, classifyPostgres(err)
	}
	binding, err := readBinding(ctx, tx, bindingID, false)
	if err != nil {
		return RuntimeWork{}, err
	}
	version, err := readVersion(ctx, tx, versionID, false)
	if err != nil {
		return RuntimeWork{}, err
	}
	work := RuntimeWork{Binding: binding, Version: version, ConsecutiveFailures: failures,
		Lease: RuntimeLease{VersionID: versionID, BindingID: bindingID, Owner: owner, Epoch: epoch, Until: until.UTC(), Identity: identity}}
	if work.Validate() != nil {
		return RuntimeWork{}, ErrConflict
	}
	if err = tx.Commit(ctx); err != nil {
		return RuntimeWork{}, classifyPostgres(err)
	}
	return work, nil
}

func (s *PostgreSQLStore) HeartbeatRuntimeSecret(ctx context.Context, lease RuntimeLease, now time.Time, duration time.Duration) (RuntimeLease, error) {
	if lease.Validate() != nil || now.IsZero() || duration < 20*time.Second || duration > time.Hour {
		return RuntimeLease{}, ErrInvalid
	}
	var until time.Time
	err := s.pool.QueryRow(ctx, `UPDATE secret_binding_runtime_reconciliations
		SET lease_until=$9,updated_at=$8
		WHERE version_id=$1 AND binding_id=$2 AND runtime_state='awaiting' AND lease_owner=$3 AND lease_epoch=$4
		  AND worker_contract=$5 AND worker_config_digest=$6 AND lease_until=$7 AND lease_until>$8
		RETURNING lease_until`, lease.VersionID, lease.BindingID, lease.Owner, lease.Epoch, lease.Identity.ContractVersion,
		lease.Identity.ConfigDigest, lease.Until, now.UTC(), now.UTC().Add(duration)).Scan(&until)
	if errors.Is(err, pgx.ErrNoRows) {
		return RuntimeLease{}, ErrRuntimeLeaseLost
	}
	if err != nil {
		return RuntimeLease{}, classifyPostgres(err)
	}
	lease.Until = until.UTC()
	return lease, nil
}

func (s *PostgreSQLStore) ApplyRuntimeSecretPending(ctx context.Context, lease RuntimeLease, outcome RuntimePendingOutcome, now time.Time) error {
	if lease.Validate() != nil || outcome.validate(now) != nil {
		return ErrInvalid
	}
	command, err := s.pool.Exec(ctx, `UPDATE secret_binding_runtime_reconciliations r
		SET next_attempt_at=$8,
		    consecutive_failures=CASE WHEN $9='' THEN 0 ELSE LEAST(30,r.consecutive_failures+1) END,
		    last_failure_code=$9,lease_owner=NULL,lease_until=NULL,worker_contract=NULL,
		    worker_config_digest=NULL,updated_at=$10
		FROM secret_binding_versions v
		WHERE r.version_id=$1 AND r.binding_id=$2 AND r.runtime_state='awaiting'
		  AND r.lease_owner=$3 AND r.lease_epoch=$4 AND r.worker_contract=$5
		  AND r.worker_config_digest=$6 AND r.lease_until=$7 AND r.lease_until>$10
		  AND v.id=r.version_id AND v.binding_id=r.binding_id AND v.state='awaiting-readiness'
		  AND v.provider='sealed-secrets'`, lease.VersionID, lease.BindingID, lease.Owner, lease.Epoch,
		lease.Identity.ContractVersion, lease.Identity.ConfigDigest, lease.Until, outcome.NextAt.UTC(), outcome.FailureCode, now.UTC())
	if err != nil {
		return classifyPostgres(err)
	}
	if command.RowsAffected() != 1 {
		return ErrRuntimeLeaseLost
	}
	return nil
}

func (s *PostgreSQLStore) ApplyRuntimeSecretReady(ctx context.Context, lease RuntimeLease, event Event, now time.Time) (Binding, Version, error) {
	if lease.Validate() != nil || now.IsZero() || event.Validate() != nil || event.Kind != EventVersionActive ||
		event.VersionID != lease.VersionID || event.BindingID != lease.BindingID || event.ActorID != "" || !event.OccurredAt.Equal(now) {
		return Binding{}, Version{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Binding{}, Version{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = lockPostgresRuntimeLease(ctx, tx, lease, now); err != nil {
		return Binding{}, Version{}, err
	}
	version, err := readVersion(ctx, tx, lease.VersionID, true)
	if err != nil {
		return Binding{}, Version{}, err
	}
	binding, err := readBinding(ctx, tx, lease.BindingID, true)
	if err != nil {
		return Binding{}, Version{}, err
	}
	if version.BindingID != binding.ID || version.State != VersionAwaitingReadiness || version.Provider != ProviderSealedSecrets ||
		version.Artifact == nil || (binding.State != BindingProvisioning && binding.State != BindingReady) ||
		now.Before(version.UpdatedAt) || now.Before(binding.UpdatedAt) {
		return Binding{}, Version{}, ErrConflict
	}
	command, err := tx.Exec(ctx, `UPDATE secret_binding_versions SET state='retained',retained_at=$2,updated_at=$2
		WHERE binding_id=$1 AND state='active'`, binding.ID, now.UTC())
	_ = command
	if err == nil {
		command, err = tx.Exec(ctx, `UPDATE secret_binding_versions SET state='active',readiness_observed_at=$2,
			activated_at=$2,updated_at=$2 WHERE id=$1 AND state='awaiting-readiness'`, version.ID, now.UTC())
		if err == nil && command.RowsAffected() != 1 {
			err = ErrRuntimeLeaseLost
		}
	}
	if err == nil {
		command, err = tx.Exec(ctx, `UPDATE secret_bindings SET state='ready',active_version=$2,updated_at=$3
			WHERE id=$1 AND state IN ('provisioning','ready')`, binding.ID, version.Number, now.UTC())
		if err == nil && command.RowsAffected() != 1 {
			err = ErrConflict
		}
	}
	if err == nil {
		command, err = tx.Exec(ctx, `UPDATE secret_binding_runtime_reconciliations SET runtime_state='ready',completed_at=$2,
			lease_owner=NULL,lease_until=NULL,worker_contract=NULL,worker_config_digest=NULL,updated_at=$2
			WHERE version_id=$1 AND runtime_state='awaiting'`, version.ID, now.UTC())
		if err == nil && command.RowsAffected() != 1 {
			err = ErrRuntimeLeaseLost
		}
	}
	if err == nil {
		err = insertEvent(ctx, tx, event)
	}
	if err == nil {
		err = invalidateRuntimeSecretProjectionTx(ctx, tx, binding.ID, now)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		return Binding{}, Version{}, runtimeLeaseError(classifyPostgres(err))
	}
	version.State, version.ReadinessObservedAt, version.ActivatedAt, version.UpdatedAt = VersionActive, now.UTC(), now.UTC(), now.UTC()
	binding.State, binding.ActiveVersion, binding.UpdatedAt = BindingReady, version.Number, now.UTC()
	return binding, version, nil
}

func (s *PostgreSQLStore) ApplyRuntimeSecretFailed(ctx context.Context, lease RuntimeLease, code string, event Event, now time.Time) (Version, error) {
	if lease.Validate() != nil || !safeCodeRE.MatchString(code) || now.IsZero() || event.Validate() != nil ||
		event.Kind != EventVersionFailed || event.VersionID != lease.VersionID || event.BindingID != lease.BindingID ||
		event.ActorID != "" || !event.OccurredAt.Equal(now) {
		return Version{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Version{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = lockPostgresRuntimeLease(ctx, tx, lease, now); err != nil {
		return Version{}, err
	}
	version, err := readVersion(ctx, tx, lease.VersionID, true)
	if err != nil {
		return Version{}, err
	}
	binding, err := readBinding(ctx, tx, lease.BindingID, true)
	if err != nil {
		return Version{}, err
	}
	if version.BindingID != binding.ID || version.State != VersionAwaitingReadiness || version.Provider != ProviderSealedSecrets ||
		version.Artifact == nil || now.Before(version.UpdatedAt) || now.Before(binding.UpdatedAt) {
		return Version{}, ErrConflict
	}
	command, err := tx.Exec(ctx, `UPDATE secret_binding_versions SET state='failed',failure_code=$2,
		readiness_observed_at=$3,updated_at=$3 WHERE id=$1 AND state='awaiting-readiness'`, version.ID, code, now.UTC())
	if err == nil && command.RowsAffected() != 1 {
		err = ErrRuntimeLeaseLost
	}
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE secret_bindings SET state='failed',updated_at=$2 WHERE id=$1 AND state='provisioning'`, binding.ID, now.UTC())
	}
	if err == nil {
		command, err = tx.Exec(ctx, `UPDATE secret_binding_runtime_reconciliations SET runtime_state='failed',completed_at=$2,
			lease_owner=NULL,lease_until=NULL,worker_contract=NULL,worker_config_digest=NULL,updated_at=$2
			WHERE version_id=$1 AND runtime_state='awaiting'`, version.ID, now.UTC())
		if err == nil && command.RowsAffected() != 1 {
			err = ErrRuntimeLeaseLost
		}
	}
	if err == nil {
		err = insertEvent(ctx, tx, event)
	}
	if err == nil {
		err = invalidateRuntimeSecretProjectionTx(ctx, tx, binding.ID, now)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		return Version{}, runtimeLeaseError(classifyPostgres(err))
	}
	version.State, version.FailureCode, version.ReadinessObservedAt, version.UpdatedAt = VersionFailed, code, now.UTC(), now.UTC()
	return version, nil
}

func lockPostgresRuntimeLease(ctx context.Context, tx pgx.Tx, lease RuntimeLease, now time.Time) error {
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM secret_binding_runtime_reconciliations
		WHERE version_id=$1 AND binding_id=$2 AND runtime_state='awaiting' AND lease_owner=$3 AND lease_epoch=$4
		  AND worker_contract=$5 AND worker_config_digest=$6 AND lease_until=$7 AND lease_until>$8
		FOR UPDATE`, lease.VersionID, lease.BindingID, lease.Owner, lease.Epoch, lease.Identity.ContractVersion,
		lease.Identity.ConfigDigest, lease.Until, now.UTC()).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRuntimeLeaseLost
	}
	return classifyPostgres(err)
}

func (s *PostgreSQLStore) AcquireRuntimeSecretReadiness(ctx context.Context, observation RuntimeWorkerObservation, duration time.Duration) (RuntimeReadinessLease, error) {
	if observation.Validate() != nil || duration < 20*time.Second || duration > time.Hour {
		return RuntimeReadinessLease{}, ErrInvalid
	}
	lease := RuntimeReadinessLease{RuntimeWorkerObservation: observation}
	err := s.pool.QueryRow(ctx, `INSERT INTO runtime_readiness(runtime_kind,scope_key,worker_id,worker_epoch,
		contract_version,config_digest,identity,observation,started_at,observed_at,lease_until,updated_at
	) VALUES('runtime-secret','global',$1,1,$2,$3,
		jsonb_build_object('fingerprintKeyId',$4::text,'sealingKeyFingerprint',$5::text),'{}'::jsonb,$6,$7,$8,$7)
	ON CONFLICT (runtime_kind,scope_key,worker_id) DO UPDATE SET
		worker_epoch=runtime_readiness.worker_epoch+1,
		contract_version=EXCLUDED.contract_version,config_digest=EXCLUDED.config_digest,
		identity=EXCLUDED.identity,started_at=EXCLUDED.started_at,observed_at=EXCLUDED.observed_at,
		lease_until=EXCLUDED.lease_until,updated_at=EXCLUDED.updated_at
	RETURNING worker_epoch,started_at,observed_at,lease_until`, observation.WorkerID, observation.Identity.ContractVersion,
		observation.Identity.ConfigDigest, observation.Identity.FingerprintKeyID, observation.Identity.SealingKeyFingerprint,
		observation.StartedAt.UTC(), observation.ObservedAt.UTC(), observation.ObservedAt.UTC().Add(duration)).
		Scan(&lease.Epoch, &lease.StartedAt, &lease.ObservedAt, &lease.Until)
	if err != nil {
		return RuntimeReadinessLease{}, classifyPostgres(err)
	}
	lease.StartedAt, lease.ObservedAt, lease.Until = lease.StartedAt.UTC(), lease.ObservedAt.UTC(), lease.Until.UTC()
	return lease, nil
}

func (s *PostgreSQLStore) HeartbeatRuntimeSecretReadiness(ctx context.Context, lease RuntimeReadinessLease, observedAt time.Time, duration time.Duration) (RuntimeReadinessLease, error) {
	if lease.Validate() != nil || observedAt.IsZero() || observedAt.Before(lease.ObservedAt) ||
		duration < 20*time.Second || duration > time.Hour {
		return RuntimeReadinessLease{}, ErrInvalid
	}
	var storedObservedAt, until time.Time
	err := s.pool.QueryRow(ctx, `UPDATE runtime_readiness SET observed_at=$9,lease_until=$10,updated_at=$9
		WHERE runtime_kind='runtime-secret' AND scope_key='global' AND worker_id=$1 AND worker_epoch=$2
		  AND contract_version=$3 AND config_digest=$4 AND identity->>'fingerprintKeyId'=$5
		  AND identity->>'sealingKeyFingerprint'=$6 AND started_at=$7
		  AND lease_until=$8 AND lease_until>$9
		RETURNING observed_at,lease_until`, lease.WorkerID, lease.Epoch, lease.Identity.ContractVersion,
		lease.Identity.ConfigDigest, lease.Identity.FingerprintKeyID, lease.Identity.SealingKeyFingerprint,
		lease.StartedAt, lease.Until, observedAt.UTC(), observedAt.UTC().Add(duration)).Scan(&storedObservedAt, &until)
	if errors.Is(err, pgx.ErrNoRows) {
		return RuntimeReadinessLease{}, ErrRuntimeLeaseLost
	}
	if err != nil {
		return RuntimeReadinessLease{}, classifyPostgres(err)
	}
	lease.ObservedAt, lease.Until = storedObservedAt.UTC(), until.UTC()
	return lease, nil
}

func (s *PostgreSQLStore) RuntimeSecretReady(ctx context.Context, identity RuntimeIdentity, now time.Time, maximumAge time.Duration) error {
	if identity.Validate() != nil || now.IsZero() || maximumAge < 2*RuntimeSecretHeartbeatInterval || maximumAge > 5*time.Minute {
		return ErrRuntimeUnavailable
	}
	var ready bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM runtime_readiness WHERE runtime_kind='runtime-secret' AND scope_key='global'
		AND contract_version=$1 AND config_digest=$2 AND identity->>'fingerprintKeyId'=$3
		AND identity->>'sealingKeyFingerprint'=$4
		  AND lease_until>$5 AND observed_at>=$6 AND observed_at<=$7
	)`, identity.ContractVersion, identity.ConfigDigest, identity.FingerprintKeyID, identity.SealingKeyFingerprint,
		now.UTC(), now.UTC().Add(-maximumAge), now.UTC().Add(RuntimeSecretReadinessSkew)).Scan(&ready)
	if err != nil {
		return classifyPostgres(err)
	}
	if !ready {
		return ErrRuntimeUnavailable
	}
	return nil
}

var _ RuntimeStore = (*PostgreSQLStore)(nil)
