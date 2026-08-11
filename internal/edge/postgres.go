package edge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgreSQLStore struct{ pool *pgxpool.Pool }

func NewPostgreSQLStore(pool *pgxpool.Pool) (*PostgreSQLStore, error) {
	if pool == nil {
		return nil, ErrInvalid
	}
	return &PostgreSQLStore{pool: pool}, nil
}

func OpenPostgreSQLStore(ctx context.Context, databaseURL string) (*PostgreSQLStore, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, ErrInvalid
	}
	config.ConnConfig.RuntimeParams["application_name"] = "kuberploy-edge-observer"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &PostgreSQLStore{pool: pool}, nil
}

func (s *PostgreSQLStore) Close() {
	if s != nil && s.pool != nil {
		s.pool.Close()
	}
}

func (s *PostgreSQLStore) SynchronizeTargets(ctx context.Context, configDigest string, desired []DesiredTarget, now time.Time) error {
	if s == nil || s.pool == nil || !validDigest(configDigest) || now.IsZero() || len(desired) < 1 || len(desired) > MaximumTargets {
		return ErrInvalid
	}
	for index, target := range desired {
		if target.Validate() != nil || target.RuntimeConfigDigest != configDigest || index > 0 && desired[index-1].Key >= target.Key {
			return ErrInvalid
		}
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	current := map[string]Target{}
	rows, err := tx.Query(ctx, targetSelect+` WHERE active FOR UPDATE`)
	if err != nil {
		return classifyPostgreSQL(err)
	}
	for rows.Next() {
		target, scanErr := scanTarget(rows)
		if scanErr != nil {
			rows.Close()
			return classifyPostgreSQL(scanErr)
		}
		current[targetMapKey(target.Key, target.Revision)] = target
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return classifyPostgreSQL(err)
	}
	rows.Close()

	wanted := make(map[string]DesiredTarget, len(desired))
	policyChanged := false
	for _, target := range desired {
		if target.Kind == KindExternalDNS {
			if err = validateExternalDNSMetadataTx(ctx, tx, target); err != nil {
				return err
			}
		}
		wanted[targetMapKey(target.Key, target.Revision)] = target
	}
	for mapKey, target := range current {
		if _, keep := wanted[mapKey]; keep {
			continue
		}
		result, updateErr := tx.Exec(ctx, `UPDATE edge_runtime_targets SET active=false,lease_owner=NULL,lease_until=NULL,
			worker_contract=NULL,worker_config_digest=NULL,updated_at=$3
			WHERE target_key=$1 AND profile_revision=$2 AND active AND updated_at<=$3`, target.Key, target.Revision, now.UTC())
		if updateErr != nil {
			return classifyPostgreSQL(updateErr)
		}
		if result.RowsAffected() != 1 {
			return ErrConflict
		}
		policyChanged = true
	}

	for _, target := range desired {
		existing, getErr := scanTarget(tx.QueryRow(ctx, targetSelect+` WHERE target_key=$1 AND profile_revision=$2 FOR UPDATE`, target.Key, target.Revision))
		if errors.Is(getErr, pgx.ErrNoRows) {
			_, err = tx.Exec(ctx, `INSERT INTO edge_runtime_targets(
				target_key,profile_revision,kind,integration_id,management_mode,namespace,profile_config_map,
				external_txt_owner_id,external_policy,external_domains,external_provider_kind,external_credential_secret_ref,
				external_provider_config_ref,external_egress_config_ref,desired_digest,runtime_config_digest,
				active,runtime_state,next_observation_at,created_at,updated_at
			) VALUES($1,$2,$3,NULLIF($4,'')::uuid,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,true,'awaiting',$17,$17,$17)`,
				target.Key, target.Revision, target.Kind, target.IntegrationID, target.Mode, target.Namespace, target.ProfileConfigMap,
				target.ExternalTXTOwnerID, target.ExternalPolicy, target.ExternalDomains, target.ExternalProviderKind,
				target.ExternalCredentialSecretRef, target.ExternalProviderConfigRef, target.ExternalEgressConfigRef,
				target.DesiredDigest, target.RuntimeConfigDigest, now.UTC())
			if err != nil {
				return classifyPostgreSQL(err)
			}
			policyChanged = true
			continue
		}
		if getErr != nil {
			return classifyPostgreSQL(getErr)
		}
		identity := existing.DesiredTarget
		identity.RuntimeConfigDigest = target.RuntimeConfigDigest
		if identity != target || now.Before(existing.UpdatedAt) {
			return ErrConflict
		}
		if existing.Active && existing.RuntimeConfigDigest == target.RuntimeConfigDigest {
			continue
		}
		result, updateErr := tx.Exec(ctx, `UPDATE edge_runtime_targets SET runtime_config_digest=$3,active=true,
			runtime_state='awaiting',next_observation_at=$4,consecutive_failures=0,last_failure_code='',
			lease_owner=NULL,lease_until=NULL,worker_contract=NULL,worker_config_digest=NULL,updated_at=$4
			WHERE target_key=$1 AND profile_revision=$2 AND updated_at<=$4`, target.Key, target.Revision, target.RuntimeConfigDigest, now.UTC())
		if updateErr != nil {
			return classifyPostgreSQL(updateErr)
		}
		if result.RowsAffected() != 1 {
			return ErrConflict
		}
		policyChanged = true
	}
	if policyChanged {
		if err = invalidateEdgeProjectionBindings(ctx, tx, now.UTC()); err != nil {
			return err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return classifyPostgreSQL(err)
	}
	return nil
}

func validateExternalDNSMetadataTx(ctx context.Context, tx pgx.Tx, target DesiredTarget) error {
	if target.Kind != KindExternalDNS || target.Validate() != nil {
		return ErrInvalid
	}
	var mode, providerKind, txtOwner, policy, credentialRef, providerRef, egressRef, profile, lifecycle string
	var runtimeRevision int64
	var rawDomains []byte
	err := tx.QueryRow(ctx, `SELECT mode,provider_kind,txt_owner_id,sync_policy,COALESCE(credential_secret_ref,''),
		COALESCE(provider_config_ref,''),COALESCE(egress_config_ref,''),COALESCE(operator_profile_ref,''),allowed_domain_suffixes,runtime_revision,lifecycle
		FROM external_dns_integrations WHERE id=$1 FOR KEY SHARE`, target.IntegrationID).
		Scan(&mode, &providerKind, &txtOwner, &policy, &credentialRef, &providerRef, &egressRef, &profile, &rawDomains, &runtimeRevision, &lifecycle)
	if err != nil {
		return classifyPostgreSQL(err)
	}
	var domains []string
	if json.Unmarshal(rawDomains, &domains) != nil || len(domains) < 1 || len(domains) > 64 {
		return ErrConflict
	}
	slices.Sort(domains)
	if lifecycle != "active" || runtimeRevision != target.Revision || mode != string(target.Mode) || providerKind != target.ExternalProviderKind || txtOwner != target.ExternalTXTOwnerID || policy != target.ExternalPolicy ||
		strings.Join(domains, ",") != target.ExternalDomains || target.Mode == ModeAdopted && profile != target.ProfileConfigMap ||
		target.Mode == ModeManaged && (profile != "" || credentialRef != target.ExternalCredentialSecretRef ||
			providerRef != target.ExternalProviderConfigRef || egressRef != target.ExternalEgressConfigRef) ||
		target.Mode == ModeAdopted && (credentialRef != "" || providerRef != "" || egressRef != "") {
		return ErrConflict
	}
	return nil
}

func (s *PostgreSQLStore) Target(ctx context.Context, key string, revision int64) (Target, error) {
	if s == nil || s.pool == nil || key == "" || revision <= 0 {
		return Target{}, ErrInvalid
	}
	target, err := scanTarget(s.pool.QueryRow(ctx, targetSelect+` WHERE target_key=$1 AND profile_revision=$2`, key, revision))
	if err != nil {
		return Target{}, classifyPostgreSQL(err)
	}
	if target.Validate() != nil {
		return Target{}, ErrConflict
	}
	return target, nil
}

func (s *PostgreSQLStore) ClaimTarget(ctx context.Context, owner, contract, configDigest string, now time.Time, duration time.Duration) (Lease, bool, error) {
	if s == nil || s.pool == nil || !workerIDPattern.MatchString(owner) || contract != RuntimeContract || !validDigest(configDigest) || now.IsZero() ||
		duration < 30*time.Second || duration > 15*time.Minute {
		return Lease{}, false, ErrInvalid
	}
	until := now.UTC().Add(duration)
	target, err := scanTarget(s.pool.QueryRow(ctx, `WITH candidate AS (
		SELECT target_key,profile_revision FROM edge_runtime_targets
		WHERE active AND runtime_state<>'failed' AND runtime_config_digest=$3 AND next_observation_at<=$4
		  AND (lease_until IS NULL OR lease_until<=$4)
		ORDER BY next_observation_at,target_key,profile_revision
		FOR UPDATE SKIP LOCKED LIMIT 1
	), claimed AS (
		UPDATE edge_runtime_targets target SET lease_owner=$1,lease_epoch=target.lease_epoch+1,
			lease_until=$5,worker_contract=$2,worker_config_digest=$3,updated_at=$4
		FROM candidate WHERE target.target_key=candidate.target_key AND target.profile_revision=candidate.profile_revision
		RETURNING target.*
	) `+targetSelectFromClaimed, owner, contract, configDigest, now.UTC(), until))
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, false, nil
	}
	if err != nil {
		return Lease{}, false, classifyPostgreSQL(err)
	}
	// PostgreSQL stores timestamptz at microsecond precision. Use the value
	// returned by PostgreSQL so the lease and its embedded target have exactly
	// the same authority timestamp even when the caller's clock has nanoseconds.
	lease, err := authoritativePostgreSQLLease(target, owner, target.LeaseEpoch, now.UTC())
	if err != nil {
		return Lease{}, false, err
	}
	return lease, true, nil
}

func (s *PostgreSQLStore) HeartbeatTarget(ctx context.Context, lease Lease, now time.Time, duration time.Duration) (Lease, error) {
	if s == nil || s.pool == nil || now.IsZero() || duration < 30*time.Second || duration > 15*time.Minute || lease.Validate(now) != nil {
		return Lease{}, ErrInvalid
	}
	until := now.UTC().Add(duration)
	target, err := scanTarget(s.pool.QueryRow(ctx, `UPDATE edge_runtime_targets SET lease_until=$8,updated_at=$7
		WHERE target_key=$1 AND profile_revision=$2 AND active AND desired_digest=$3 AND runtime_config_digest=$4
		  AND lease_owner=$5 AND lease_epoch=$6 AND lease_until=$9 AND lease_until>$7
		  AND worker_contract=$10 AND worker_config_digest=$4
		RETURNING `+targetColumns, lease.Target.Key, lease.Target.Revision, lease.Target.DesiredDigest, lease.Target.RuntimeConfigDigest,
		lease.Owner, lease.Epoch, now.UTC(), until, lease.Until, RuntimeContract))
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, ErrLeaseLost
	}
	if err != nil {
		return Lease{}, classifyPostgreSQL(err)
	}
	return authoritativePostgreSQLLease(target, lease.Owner, lease.Epoch, now.UTC())
}

func authoritativePostgreSQLLease(target Target, owner string, epoch int64, now time.Time) (Lease, error) {
	if target.LeaseUntil == nil {
		return Lease{}, ErrConflict
	}
	lease := Lease{Target: target, Owner: owner, Epoch: epoch, Until: *target.LeaseUntil}
	if lease.Validate(now) != nil {
		return Lease{}, ErrConflict
	}
	return lease, nil
}

func (s *PostgreSQLStore) RecordTargetReady(ctx context.Context, lease Lease, receipt ObservationReceipt, observedAt, next time.Time) (Target, error) {
	if s == nil || s.pool == nil || observedAt.IsZero() || next.Before(observedAt) || lease.Validate(observedAt) != nil ||
		receipt.Validate(lease.Target.DesiredTarget) != nil {
		return Target{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Target{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	current, err := scanTarget(tx.QueryRow(ctx, targetSelect+` WHERE target_key=$1 AND profile_revision=$2 FOR UPDATE`, lease.Target.Key, lease.Target.Revision))
	if errors.Is(err, pgx.ErrNoRows) {
		return Target{}, ErrLeaseLost
	}
	if err != nil {
		return Target{}, classifyPostgreSQL(err)
	}
	if !sameLease(current, lease, observedAt) {
		return Target{}, ErrLeaseLost
	}
	if current.ObservedIdentityDigest != "" && current.ObservedIdentityDigest != receipt.IdentityDigest {
		return Target{}, ErrIdentityChanged
	}
	if receipt.SSLIP != nil {
		observation := SSLIPIngressObservation{TargetKey: current.Key, ProfileRevision: current.Revision,
			DesiredDigest: current.DesiredDigest, RuntimeConfigDigest: current.RuntimeConfigDigest,
			Endpoint: *receipt.SSLIP, ObservedAt: observedAt.UTC()}
		if observation.Validate(current.DesiredTarget) != nil {
			return Target{}, ErrInvalid
		}
		_, err = tx.Exec(ctx, `INSERT INTO edge_sslip_ingress_observations(
			target_key,profile_revision,desired_digest,runtime_config_digest,public_ipv4,source_kind,
			service_uid,service_resource_version,worker_id,lease_epoch,observed_at,created_at,updated_at
		) VALUES($1,$2,$3,$4,$5::inet,$6,$7::uuid,$8,$9,$10,$11,$11,$11)
		ON CONFLICT(target_key,profile_revision) DO UPDATE SET
			runtime_config_digest=excluded.runtime_config_digest,
			service_resource_version=excluded.service_resource_version,
			worker_id=excluded.worker_id,lease_epoch=excluded.lease_epoch,
			observed_at=excluded.observed_at,updated_at=excluded.updated_at`,
			observation.TargetKey, observation.ProfileRevision, observation.DesiredDigest, observation.RuntimeConfigDigest,
			observation.Endpoint.PublicIPv4, observation.Endpoint.Source, observation.Endpoint.ServiceUID,
			observation.Endpoint.ServiceResourceVersion, lease.Owner, lease.Epoch, observation.ObservedAt)
		if err != nil {
			return Target{}, classifyPostgreSQL(err)
		}
	}
	result, err := scanTarget(tx.QueryRow(ctx, `UPDATE edge_runtime_targets SET runtime_state='ready',next_observation_at=$3,
		last_observed_at=$4,observed_identity_digest=$5,observed_resource_versions=$6,
		consecutive_failures=0,last_failure_code='',lease_owner=NULL,lease_until=NULL,
		worker_contract=NULL,worker_config_digest=NULL,updated_at=$4
		WHERE target_key=$1 AND profile_revision=$2 RETURNING `+targetColumns,
		lease.Target.Key, lease.Target.Revision, next.UTC(), observedAt.UTC(), receipt.IdentityDigest, receipt.ResourceVersionDigest))
	if err != nil {
		return Target{}, classifyPostgreSQL(err)
	}
	recoveredFromStaleGap := false
	if current.LastObservedAt != nil {
		expectedInterval := current.NextObservationAt.Sub(*current.LastObservedAt)
		recoveredFromStaleGap = expectedInterval > 0 && !observedAt.Before(current.LastObservedAt.Add(2*expectedInterval))
	}
	if current.State != StateReady || recoveredFromStaleGap {
		if err = invalidateEdgeProjectionBindings(ctx, tx, observedAt.UTC()); err != nil {
			return Target{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return Target{}, classifyPostgreSQL(err)
	}
	if result.Validate() != nil {
		return Target{}, ErrConflict
	}
	return result, nil
}

func (s *PostgreSQLStore) SSLIPIngressObservation(ctx context.Context, key string, revision int64) (SSLIPIngressObservation, error) {
	if s == nil || s.pool == nil || key != "traefik" || revision <= 0 {
		return SSLIPIngressObservation{}, ErrInvalid
	}
	var observation SSLIPIngressObservation
	var publicIPv4 string
	err := s.pool.QueryRow(ctx, `SELECT o.target_key,o.profile_revision,o.desired_digest,o.runtime_config_digest,
		host(o.public_ipv4),o.source_kind,o.service_uid::text,o.service_resource_version,o.observed_at
		FROM edge_sslip_ingress_observations o
		WHERE o.target_key=$1 AND o.profile_revision=$2`, key, revision).Scan(
		&observation.TargetKey, &observation.ProfileRevision, &observation.DesiredDigest, &observation.RuntimeConfigDigest,
		&publicIPv4, &observation.Endpoint.Source, &observation.Endpoint.ServiceUID,
		&observation.Endpoint.ServiceResourceVersion, &observation.ObservedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SSLIPIngressObservation{}, ErrNotFound
	}
	if err != nil {
		return SSLIPIngressObservation{}, classifyPostgreSQL(err)
	}
	observation.Endpoint.PublicIPv4 = publicIPv4
	if observation.Endpoint.Validate() != nil || observation.ObservedAt.IsZero() {
		return SSLIPIngressObservation{}, ErrConflict
	}
	return observation, nil
}

func (s *PostgreSQLStore) RecordTargetRetry(ctx context.Context, lease Lease, code string, permanent bool, next, now time.Time) (Target, error) {
	if s == nil || s.pool == nil || !failureCodePattern.MatchString(code) || now.IsZero() || next.Before(now) || lease.Validate(now) != nil {
		return Target{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Target{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	current, err := scanTarget(tx.QueryRow(ctx, targetSelect+` WHERE target_key=$1 AND profile_revision=$2 FOR UPDATE`, lease.Target.Key, lease.Target.Revision))
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !sameLease(current, lease, now) {
		return Target{}, ErrLeaseLost
	}
	if err != nil {
		return Target{}, classifyPostgreSQL(err)
	}
	target, err := scanTarget(tx.QueryRow(ctx, `UPDATE edge_runtime_targets SET
		consecutive_failures=LEAST(30,consecutive_failures+1),last_failure_code=$7,next_observation_at=$8,
		runtime_state=CASE WHEN $9 OR consecutive_failures+1>=30 THEN 'failed' ELSE 'awaiting' END,
		lease_owner=NULL,lease_until=NULL,worker_contract=NULL,worker_config_digest=NULL,updated_at=$10
		WHERE target_key=$1 AND profile_revision=$2 AND active AND desired_digest=$3 AND runtime_config_digest=$4
		  AND lease_owner=$5 AND lease_epoch=$6 AND lease_until=$11 AND lease_until>$10
		  AND worker_contract=$12 AND worker_config_digest=$4
		RETURNING `+targetColumns, lease.Target.Key, lease.Target.Revision, lease.Target.DesiredDigest, lease.Target.RuntimeConfigDigest,
		lease.Owner, lease.Epoch, code, next.UTC(), permanent, now.UTC(), lease.Until, RuntimeContract))
	if errors.Is(err, pgx.ErrNoRows) {
		return Target{}, ErrLeaseLost
	}
	if err != nil {
		return Target{}, classifyPostgreSQL(err)
	}
	if target.Validate() != nil {
		return Target{}, ErrConflict
	}
	if current.State == StateReady {
		if err = invalidateEdgeProjectionBindings(ctx, tx, now.UTC()); err != nil {
			return Target{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return Target{}, classifyPostgreSQL(err)
	}
	return target, nil
}

// invalidateEdgeProjectionBindings requests same-head policy revalidation
// only when the approved edge target set or a target's semantic readiness
// changes. Ordinary successful polls do not wake every tenant binding.
func invalidateEdgeProjectionBindings(ctx context.Context, tx pgx.Tx, changedAt time.Time) error {
	if tx == nil || changedAt.IsZero() {
		return ErrInvalid
	}
	_, err := tx.Exec(ctx, `UPDATE git_repository_bindings
		SET state=CASE WHEN state IN ('diverged','missing-ref') THEN state ELSE 'indexing' END,
			updated_at=GREATEST(updated_at+interval '1 microsecond',$1)
		WHERE kind='environment' AND target_head_revision IS NOT NULL`, changedAt.UTC())
	return classifyPostgreSQL(err)
}

func (s *PostgreSQLStore) RecordReadiness(ctx context.Context, readiness Readiness) error {
	if s == nil || s.pool == nil || readiness.Validate() != nil {
		return ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var current Readiness
	err = tx.QueryRow(ctx, `SELECT worker_id,worker_epoch,contract_version,config_digest,(identity->>'targetCount')::integer,
		started_at,observed_at,lease_until FROM runtime_readiness
		WHERE runtime_kind='edge' AND scope_key='global' AND worker_id=$1 FOR UPDATE`, readiness.WorkerID).
		Scan(&current.WorkerID, &current.WorkerEpoch, &current.Contract, &current.ConfigDigest, &current.TargetCount,
			&current.StartedAt, &current.ObservedAt, &current.LeaseUntil)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return classifyPostgreSQL(err)
	}
	if err == nil {
		if readiness.WorkerEpoch < current.WorkerEpoch || readiness.WorkerEpoch > current.WorkerEpoch+1 {
			return ErrConflict
		}
		if readiness.WorkerEpoch == current.WorkerEpoch &&
			(readiness.Contract != current.Contract || readiness.ConfigDigest != current.ConfigDigest ||
				readiness.TargetCount != current.TargetCount || !readiness.StartedAt.Equal(current.StartedAt) ||
				readiness.ObservedAt.Before(current.ObservedAt)) {
			return ErrConflict
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO runtime_readiness(runtime_kind,scope_key,worker_id,worker_epoch,
		contract_version,config_digest,identity,observation,started_at,observed_at,lease_until,updated_at
	) VALUES('edge','global',$1,$2,$3,$4,jsonb_build_object('targetCount',$5::integer),'{}'::jsonb,$6,$7,$8,$7)
	ON CONFLICT(runtime_kind,scope_key,worker_id) DO UPDATE SET worker_epoch=excluded.worker_epoch,
		contract_version=excluded.contract_version,config_digest=excluded.config_digest,identity=excluded.identity,
		started_at=excluded.started_at,observed_at=excluded.observed_at,lease_until=excluded.lease_until,updated_at=excluded.updated_at`, readiness.WorkerID, readiness.WorkerEpoch,
		readiness.Contract, readiness.ConfigDigest, readiness.TargetCount, readiness.StartedAt.UTC(), readiness.ObservedAt.UTC(), readiness.LeaseUntil.UTC())
	if err != nil {
		return classifyPostgreSQL(err)
	}
	return classifyPostgreSQL(tx.Commit(ctx))
}

func (s *PostgreSQLStore) RuntimeReady(ctx context.Context, contract, configDigest string, targetCount int, now time.Time, maximumAge time.Duration) error {
	if s == nil || s.pool == nil || contract != RuntimeContract || !validDigest(configDigest) || targetCount < 1 || targetCount > MaximumTargets ||
		now.IsZero() || maximumAge < 30*time.Second || maximumAge > 15*time.Minute {
		return ErrInvalid
	}
	var workerReady, targetsReady bool
	err := s.pool.QueryRow(ctx, `SELECT
		EXISTS(SELECT 1 FROM runtime_readiness r
			WHERE r.runtime_kind='edge' AND r.scope_key='global' AND r.contract_version=$1 AND r.config_digest=$2
			  AND (r.identity->>'targetCount')::integer=$3
			  AND r.lease_until>$4 AND r.observed_at BETWEEN $4-make_interval(secs=>$5) AND $4+interval '5 seconds'),
		((SELECT count(*) FROM edge_runtime_targets WHERE active)=$3 AND NOT EXISTS(
			SELECT 1 FROM edge_runtime_targets target
			LEFT JOIN external_dns_integrations integration ON integration.id=target.integration_id
			WHERE target.active AND (
				target.runtime_config_digest<>$2 OR target.runtime_state<>'ready' OR target.last_observed_at IS NULL OR
				target.last_observed_at<$4-make_interval(secs=>$5) OR target.last_observed_at>$4+interval '5 seconds' OR
				(target.kind='external-dns' AND (
					integration.id IS NULL OR integration.lifecycle<>'active' OR integration.runtime_revision<>target.profile_revision OR integration.mode IS DISTINCT FROM target.management_mode OR
					integration.provider_kind IS DISTINCT FROM target.external_provider_kind OR
					integration.txt_owner_id IS DISTINCT FROM target.external_txt_owner_id OR
					integration.sync_policy IS DISTINCT FROM target.external_policy OR
					COALESCE((SELECT string_agg(suffix.value,',' ORDER BY suffix.value)
						FROM jsonb_array_elements_text(integration.allowed_domain_suffixes) AS suffix(value)),'')
						IS DISTINCT FROM target.external_domains OR
					(target.management_mode='adopted' AND COALESCE(integration.operator_profile_ref,'')<>target.profile_config_map) OR
					(target.management_mode='adopted' AND (integration.credential_secret_ref IS NOT NULL OR
						integration.provider_config_ref IS NOT NULL OR integration.egress_config_ref IS NOT NULL)) OR
					(target.management_mode='managed' AND (integration.operator_profile_ref IS NOT NULL OR
						COALESCE(integration.credential_secret_ref,'')<>target.external_credential_secret_ref OR
						COALESCE(integration.provider_config_ref,'')<>target.external_provider_config_ref OR
						COALESCE(integration.egress_config_ref,'')<>target.external_egress_config_ref))
				))
			)
		))`, contract, configDigest, targetCount, now.UTC(), int64(maximumAge.Seconds())).Scan(&workerReady, &targetsReady)
	if err != nil {
		return classifyPostgreSQL(err)
	}
	if !workerReady || !targetsReady {
		return ErrUnavailable
	}
	return nil
}

const targetColumns = `target_key,profile_revision,kind,COALESCE(integration_id::text,''),management_mode,namespace,
	profile_config_map,external_txt_owner_id,external_policy,external_domains,external_provider_kind,
	external_credential_secret_ref,external_provider_config_ref,external_egress_config_ref,desired_digest,runtime_config_digest,
	active,runtime_state,next_observation_at,last_observed_at,observed_identity_digest,observed_resource_versions,
	consecutive_failures,last_failure_code,COALESCE(lease_owner,''),lease_epoch,lease_until,COALESCE(worker_contract,''),
	COALESCE(worker_config_digest,''),created_at,updated_at`

const targetSelect = `SELECT ` + targetColumns + ` FROM edge_runtime_targets`
const targetSelectFromClaimed = `SELECT ` + targetColumns + ` FROM claimed`

func scanTarget(row pgx.Row) (Target, error) {
	var target Target
	err := row.Scan(&target.Key, &target.Revision, &target.Kind, &target.IntegrationID, &target.Mode, &target.Namespace,
		&target.ProfileConfigMap, &target.ExternalTXTOwnerID, &target.ExternalPolicy, &target.ExternalDomains,
		&target.ExternalProviderKind, &target.ExternalCredentialSecretRef, &target.ExternalProviderConfigRef, &target.ExternalEgressConfigRef,
		&target.DesiredDigest, &target.RuntimeConfigDigest, &target.Active, &target.State, &target.NextObservationAt,
		&target.LastObservedAt, &target.ObservedIdentityDigest, &target.ObservedResourceVersions, &target.ConsecutiveFailures,
		&target.LastFailureCode, &target.LeaseOwner, &target.LeaseEpoch, &target.LeaseUntil, &target.WorkerContract,
		&target.WorkerConfigDigest, &target.CreatedAt, &target.UpdatedAt)
	return target, err
}

func classifyPostgreSQL(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505", "40001", "40P01":
			return ErrConflict
		case "23503":
			return ErrNotFound
		case "23514", "22P02", "22001", "22007":
			return ErrInvalid
		}
	}
	return fmt.Errorf("edge runtime database operation: %w", err)
}

var _ Store = (*PostgreSQLStore)(nil)
