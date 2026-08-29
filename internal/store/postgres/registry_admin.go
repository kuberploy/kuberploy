package postgres

import (
	"context"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/registry"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func (s *Store) ListRegistryTargetsForActor(ctx context.Context, actor string) ([]domain.RegistryTarget, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = authorizeWith(ctx, tx, actor, domain.PermissionRegistryTargetsManage, domain.AccessTarget{Type: "platform", ID: "platform"}); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT id,name,mode,endpoint,repository_prefix,
		pull_credential_ref,push_credential_ref,cache_credential_ref,created_at,updated_at
		FROM registry_targets ORDER BY name,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	targets := make([]domain.RegistryTarget, 0)
	for rows.Next() {
		var target domain.RegistryTarget
		if err = rows.Scan(&target.ID, &target.Name, &target.Mode, &target.Endpoint, &target.RepositoryPrefix,
			&target.PullCredentialRef, &target.PushCredentialRef, &target.CacheCredentialRef,
			&target.CreatedAt, &target.UpdatedAt); err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return targets, nil
}

func (s *Store) CreateRegistryTargetForActor(ctx context.Context, actor, key, fingerprint, requestID string, target domain.RegistryTarget) (base.Result[domain.RegistryTarget], error) {
	if err := registry.ValidateTarget(target); err != nil {
		return base.Result[domain.RegistryTarget]{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return base.Result[domain.RegistryTarget]{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = authorizeWith(ctx, tx, actor, domain.PermissionRegistryTargetsManage, domain.AccessTarget{Type: "platform", ID: "platform"}); err != nil {
		return base.Result[domain.RegistryTarget]{}, err
	}
	if old, ok, findErr := findIdem(ctx, tx, actor, "registry-targets.create", key); findErr != nil {
		return base.Result[domain.RegistryTarget]{}, findErr
	} else if ok {
		if old.fingerprint != fingerprint {
			return base.Result[domain.RegistryTarget]{}, base.ErrIdempotencyConflict
		}
		current, loadErr := registryTarget(ctx, tx, old.resourceID)
		if loadErr != nil {
			return base.Result[domain.RegistryTarget]{}, loadErr
		}
		if err = tx.Commit(ctx); err != nil {
			return base.Result[domain.RegistryTarget]{}, err
		}
		return base.Result[domain.RegistryTarget]{Value: current, Replay: true}, nil
	}
	now := databaseTime(time.Now())
	target.CreatedAt = now
	target.UpdatedAt = now
	err = tx.QueryRow(ctx, `INSERT INTO registry_targets(
		id,name,mode,endpoint,repository_prefix,pull_credential_ref,
		push_credential_ref,cache_credential_ref,created_at,updated_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
	RETURNING id,name,mode,endpoint,repository_prefix,pull_credential_ref,
		push_credential_ref,cache_credential_ref,created_at,updated_at`,
		target.ID, target.Name, target.Mode, target.Endpoint, target.RepositoryPrefix,
		target.PullCredentialRef, target.PushCredentialRef, target.CacheCredentialRef,
		target.CreatedAt, target.UpdatedAt).Scan(&target.ID, &target.Name, &target.Mode,
		&target.Endpoint, &target.RepositoryPrefix, &target.PullCredentialRef,
		&target.PushCredentialRef, &target.CacheCredentialRef, &target.CreatedAt, &target.UpdatedAt)
	if err != nil {
		return base.Result[domain.RegistryTarget]{}, classify(err)
	}
	if err = putIdem(ctx, tx, actor, "registry-targets.create", key, fingerprint, "registry-target", target.ID, nil); err != nil {
		return base.Result[domain.RegistryTarget]{}, classify(err)
	}
	if err = audit(ctx, tx, actor, "registry-target.create", "registry-target", target.ID, requestID, registryTargetAuditDetail(target)); err != nil {
		return base.Result[domain.RegistryTarget]{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return base.Result[domain.RegistryTarget]{}, err
	}
	return base.Result[domain.RegistryTarget]{Value: target}, nil
}

func (s *Store) UpdateRegistryTargetForActor(ctx context.Context, actor, key, fingerprint, requestID string, target domain.RegistryTarget) (base.Result[domain.RegistryTarget], error) {
	if err := registry.ValidateTarget(target); err != nil {
		return base.Result[domain.RegistryTarget]{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return base.Result[domain.RegistryTarget]{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = authorizeWith(ctx, tx, actor, domain.PermissionRegistryTargetsManage, domain.AccessTarget{Type: "platform", ID: "platform"}); err != nil {
		return base.Result[domain.RegistryTarget]{}, err
	}
	idemScope := "registry-targets.update:" + target.ID
	if old, ok, findErr := findIdem(ctx, tx, actor, idemScope, key); findErr != nil {
		return base.Result[domain.RegistryTarget]{}, findErr
	} else if ok {
		if old.fingerprint != fingerprint {
			return base.Result[domain.RegistryTarget]{}, base.ErrIdempotencyConflict
		}
		current, loadErr := registryTarget(ctx, tx, old.resourceID)
		if loadErr != nil {
			return base.Result[domain.RegistryTarget]{}, loadErr
		}
		if err = tx.Commit(ctx); err != nil {
			return base.Result[domain.RegistryTarget]{}, err
		}
		return base.Result[domain.RegistryTarget]{Value: current, Replay: true}, nil
	}
	var current domain.RegistryTarget
	err = tx.QueryRow(ctx, `SELECT id,name,mode,endpoint,repository_prefix,
		pull_credential_ref,push_credential_ref,cache_credential_ref,created_at,updated_at
		FROM registry_targets WHERE id=$1 FOR UPDATE`, target.ID).Scan(&current.ID, &current.Name,
		&current.Mode, &current.Endpoint, &current.RepositoryPrefix, &current.PullCredentialRef,
		&current.PushCredentialRef, &current.CacheCredentialRef, &current.CreatedAt, &current.UpdatedAt)
	if err != nil {
		return base.Result[domain.RegistryTarget]{}, classify(err)
	}
	if current.Mode != target.Mode {
		return base.Result[domain.RegistryTarget]{}, base.ErrConflict
	}
	if current.RepositoryPrefix != target.RepositoryPrefix {
		var policiesExist bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM service_registry_policies WHERE registry_target_id=$1)`, target.ID).Scan(&policiesExist); err != nil {
			return base.Result[domain.RegistryTarget]{}, classify(err)
		}
		if policiesExist {
			return base.Result[domain.RegistryTarget]{}, base.ErrConflict
		}
	}
	target.CreatedAt = current.CreatedAt
	target.UpdatedAt = databaseTime(time.Now())
	err = tx.QueryRow(ctx, `UPDATE registry_targets SET name=$2,endpoint=$3,
		repository_prefix=$4,pull_credential_ref=$5,push_credential_ref=$6,
		cache_credential_ref=$7,updated_at=$8 WHERE id=$1
		RETURNING id,name,mode,endpoint,repository_prefix,pull_credential_ref,
		push_credential_ref,cache_credential_ref,created_at,updated_at`, target.ID,
		target.Name, target.Endpoint, target.RepositoryPrefix, target.PullCredentialRef,
		target.PushCredentialRef, target.CacheCredentialRef, target.UpdatedAt).Scan(&target.ID,
		&target.Name, &target.Mode, &target.Endpoint, &target.RepositoryPrefix,
		&target.PullCredentialRef, &target.PushCredentialRef, &target.CacheCredentialRef,
		&target.CreatedAt, &target.UpdatedAt)
	if err != nil {
		return base.Result[domain.RegistryTarget]{}, classify(err)
	}
	if err = putIdem(ctx, tx, actor, idemScope, key, fingerprint, "registry-target", target.ID, nil); err != nil {
		return base.Result[domain.RegistryTarget]{}, classify(err)
	}
	if err = audit(ctx, tx, actor, "registry-target.update", "registry-target", target.ID, requestID, registryTargetAuditDetail(target)); err != nil {
		return base.Result[domain.RegistryTarget]{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return base.Result[domain.RegistryTarget]{}, err
	}
	return base.Result[domain.RegistryTarget]{Value: target}, nil
}

func registryTargetAuditDetail(target domain.RegistryTarget) map[string]any {
	return map[string]any{
		"name": target.Name, "mode": target.Mode, "endpoint": target.Endpoint,
		"repositoryPrefix": target.RepositoryPrefix, "pullCredentialRef": target.PullCredentialRef,
		"pushCredentialRef": target.PushCredentialRef, "cacheCredentialRef": target.CacheCredentialRef,
	}
}

func (s *Store) RegistryLifecycleSnapshotsForActor(ctx context.Context, actor, applicationID string, now time.Time) ([]domain.RegistryLifecycleSnapshot, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = authorizeWith(ctx, tx, actor, domain.PermissionRegistryRead, domain.AccessTarget{Type: "application", ID: applicationID}); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT registry_target_id FROM service_registry_policies
		WHERE service_id=$1 ORDER BY registry_target_id`, applicationID)
	if err != nil {
		return nil, err
	}
	targetIDs := make([]string, 0)
	for rows.Next() {
		var targetID string
		if err = rows.Scan(&targetID); err != nil {
			rows.Close()
			return nil, err
		}
		targetIDs = append(targetIDs, targetID)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	snapshots := make([]domain.RegistryLifecycleSnapshot, 0, len(targetIDs))
	for _, targetID := range targetIDs {
		snapshot, snapshotErr := registryLifecycleSnapshot(ctx, tx, targetID, applicationID, now)
		if snapshotErr != nil {
			return nil, snapshotErr
		}
		snapshots = append(snapshots, snapshot)
	}
	sort.Slice(snapshots, func(i, j int) bool {
		if snapshots[i].Target.Name != snapshots[j].Target.Name {
			return snapshots[i].Target.Name < snapshots[j].Target.Name
		}
		return snapshots[i].Target.ID < snapshots[j].Target.ID
	})
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return snapshots, nil
}

func (s *Store) PutServiceRegistryPolicyForActor(ctx context.Context, actor, key, fingerprint, requestID, applicationID string, policy domain.ServiceRegistryPolicy) (base.Result[domain.ServiceRegistryPolicy], error) {
	if policy.ServiceID != applicationID {
		return base.Result[domain.ServiceRegistryPolicy]{}, base.ErrRegistryPolicyInvalid
	}
	policy = registry.NormalizePolicy(policy, time.Now())
	if err := registry.ValidatePolicy(policy); err != nil {
		return base.Result[domain.ServiceRegistryPolicy]{}, err
	}
	policy.CreatedAt = databaseTime(policy.CreatedAt)
	policy.UpdatedAt = databaseTime(policy.UpdatedAt)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return base.Result[domain.ServiceRegistryPolicy]{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = authorizeWith(ctx, tx, actor, domain.PermissionRegistryPolicyWrite, domain.AccessTarget{Type: "application", ID: applicationID}); err != nil {
		return base.Result[domain.ServiceRegistryPolicy]{}, err
	}
	target, err := registryTarget(ctx, tx, policy.RegistryTargetID)
	if err != nil {
		return base.Result[domain.ServiceRegistryPolicy]{}, err
	}
	if err = registry.ValidatePolicyForTarget(target, policy); err != nil {
		return base.Result[domain.ServiceRegistryPolicy]{}, err
	}
	idemScope := "registry-policies.put:" + applicationID
	if old, ok, findErr := findIdem(ctx, tx, actor, idemScope, key); findErr != nil {
		return base.Result[domain.ServiceRegistryPolicy]{}, findErr
	} else if ok {
		if old.fingerprint != fingerprint {
			return base.Result[domain.ServiceRegistryPolicy]{}, base.ErrIdempotencyConflict
		}
		if old.resourceID != applicationID {
			return base.Result[domain.ServiceRegistryPolicy]{}, base.ErrNotFound
		}
		current, loadErr := serviceRegistryPolicy(ctx, tx, policy.RegistryTargetID, applicationID)
		if loadErr != nil {
			return base.Result[domain.ServiceRegistryPolicy]{}, loadErr
		}
		if err = tx.Commit(ctx); err != nil {
			return base.Result[domain.ServiceRegistryPolicy]{}, err
		}
		return base.Result[domain.ServiceRegistryPolicy]{Value: current, Replay: true}, nil
	}
	err = tx.QueryRow(ctx, `INSERT INTO service_registry_policies(
		registry_target_id,service_id,repository,keep_last_successful,
		minimum_safety_age_seconds,
		cache_unused_expiry_seconds,cache_byte_quota,created_at,updated_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
	ON CONFLICT(registry_target_id,service_id) DO UPDATE SET
		repository=EXCLUDED.repository,keep_last_successful=EXCLUDED.keep_last_successful,
		minimum_safety_age_seconds=EXCLUDED.minimum_safety_age_seconds,
		cache_unused_expiry_seconds=EXCLUDED.cache_unused_expiry_seconds,
		cache_byte_quota=EXCLUDED.cache_byte_quota,updated_at=EXCLUDED.updated_at
	RETURNING registry_target_id,service_id,repository,keep_last_successful,
		minimum_safety_age_seconds,
		cache_unused_expiry_seconds,cache_byte_quota,created_at,updated_at`,
		policy.RegistryTargetID, policy.ServiceID, policy.Repository, policy.KeepLastSuccessful,
		int64(policy.MinimumSafetyAge/time.Second),
		int64(policy.CacheUnusedExpiry/time.Second), policy.CacheByteQuota,
		policy.CreatedAt, policy.UpdatedAt).Scan(&policy.RegistryTargetID, &policy.ServiceID,
		&policy.Repository, &policy.KeepLastSuccessful, newDurationScanner(&policy.MinimumSafetyAge),
		newDurationScanner(&policy.CacheUnusedExpiry),
		&policy.CacheByteQuota, &policy.CreatedAt, &policy.UpdatedAt)
	if err != nil {
		return base.Result[domain.ServiceRegistryPolicy]{}, classify(err)
	}
	if err = putIdem(ctx, tx, actor, idemScope, key, fingerprint, "registry-policy", applicationID, nil); err != nil {
		return base.Result[domain.ServiceRegistryPolicy]{}, classify(err)
	}
	if err = audit(ctx, tx, actor, "registry-policy.update", "application", applicationID, requestID, map[string]any{
		"registryTargetId": policy.RegistryTargetID, "repository": policy.Repository,
		"keepLastSuccessful": policy.KeepLastSuccessful, "minimumSafetyAgeSeconds": int64(policy.MinimumSafetyAge / time.Second),
		"cacheUnusedExpirySeconds": int64(policy.CacheUnusedExpiry / time.Second),
		"cacheByteQuota":           policy.CacheByteQuota,
	}); err != nil {
		return base.Result[domain.ServiceRegistryPolicy]{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return base.Result[domain.ServiceRegistryPolicy]{}, err
	}
	return base.Result[domain.ServiceRegistryPolicy]{Value: policy}, nil
}

func (s *Store) SaveRegistryCleanupPreviewForActor(ctx context.Context, actor, key, fingerprint, requestID, applicationID string, plan domain.RegistryCleanupPlan) (base.Result[domain.RegistryCleanupPlan], error) {
	if plan.ServiceID != applicationID || plan.ID == "" || plan.RegistryTargetID == "" || plan.PlanDigest == "" || plan.PlanDigest != base.RegistryCleanupPlanDigest(plan) {
		return base.Result[domain.RegistryCleanupPlan]{}, base.ErrRegistryPolicyInvalid
	}
	plan.CreatedAt = databaseTime(plan.CreatedAt)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return base.Result[domain.RegistryCleanupPlan]{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = authorizeWith(ctx, tx, actor, domain.PermissionRegistryCleanupPreview, domain.AccessTarget{Type: "application", ID: applicationID}); err != nil {
		return base.Result[domain.RegistryCleanupPlan]{}, err
	}
	idemScope := "registry-cleanup.preview:" + applicationID
	if old, ok, findErr := findIdem(ctx, tx, actor, idemScope, key); findErr != nil {
		return base.Result[domain.RegistryCleanupPlan]{}, findErr
	} else if ok {
		if old.fingerprint != fingerprint {
			return base.Result[domain.RegistryCleanupPlan]{}, base.ErrIdempotencyConflict
		}
		current, loadErr := registryCleanupPlan(ctx, tx, old.resourceID)
		if loadErr != nil || current.ServiceID != applicationID {
			if loadErr != nil {
				return base.Result[domain.RegistryCleanupPlan]{}, loadErr
			}
			return base.Result[domain.RegistryCleanupPlan]{}, base.ErrNotFound
		}
		if err = tx.Commit(ctx); err != nil {
			return base.Result[domain.RegistryCleanupPlan]{}, err
		}
		return base.Result[domain.RegistryCleanupPlan]{Value: current, Replay: true}, nil
	}
	saved, _, err := saveRegistryCleanupPlan(ctx, tx, plan)
	if err != nil {
		return base.Result[domain.RegistryCleanupPlan]{}, err
	}
	if err = putIdem(ctx, tx, actor, idemScope, key, fingerprint, "registry-cleanup-plan", saved.ID, nil); err != nil {
		return base.Result[domain.RegistryCleanupPlan]{}, classify(err)
	}
	if err = audit(ctx, tx, actor, "registry-cleanup.preview", "registry-cleanup-plan", saved.ID, requestID, map[string]any{
		"applicationId": applicationID, "registryTargetId": saved.RegistryTargetID,
		"planDigest": saved.PlanDigest, "estimatedBytes": saved.Summary.EstimatedBytes,
		"deletedManifests": saved.Summary.DeletedManifests, "garbageCollectBlobs": saved.Summary.GarbageCollectBlobs,
	}); err != nil {
		return base.Result[domain.RegistryCleanupPlan]{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return base.Result[domain.RegistryCleanupPlan]{}, err
	}
	return base.Result[domain.RegistryCleanupPlan]{Value: saved}, nil
}

func (s *Store) RegistryCleanupPlanForActor(ctx context.Context, actor, planID string) (domain.RegistryCleanupPlan, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return domain.RegistryCleanupPlan{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	plan, err := registryCleanupPlan(ctx, tx, planID)
	if err != nil {
		return domain.RegistryCleanupPlan{}, err
	}
	if err = authorizeWith(ctx, tx, actor, domain.PermissionRegistryRead, domain.AccessTarget{Type: "application", ID: plan.ServiceID}); err != nil {
		return domain.RegistryCleanupPlan{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.RegistryCleanupPlan{}, err
	}
	return plan, nil
}

func (s *Store) PrepareRegistryCleanupExecutionForActor(ctx context.Context, actor, key, fingerprint, requestID, applicationID, confirmation string) (base.Result[domain.RegistryCleanupPlan], error) {
	planID := confirmation
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return base.Result[domain.RegistryCleanupPlan]{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var targetID, serviceID, state string
	if err = tx.QueryRow(ctx, `SELECT registry_target_id,service_id,state FROM registry_cleanup_plans WHERE id=$1 FOR UPDATE`, planID).Scan(&targetID, &serviceID, &state); err != nil {
		return base.Result[domain.RegistryCleanupPlan]{}, classify(err)
	}
	if serviceID != applicationID {
		return base.Result[domain.RegistryCleanupPlan]{}, base.ErrNotFound
	}
	if err = authorizeWith(ctx, tx, actor, domain.PermissionRegistryCleanupExecute, domain.AccessTarget{Type: "application", ID: applicationID}); err != nil {
		return base.Result[domain.RegistryCleanupPlan]{}, err
	}
	target, err := registryTarget(ctx, tx, targetID)
	if err != nil {
		return base.Result[domain.RegistryCleanupPlan]{}, err
	}
	if target.Mode != domain.RegistryTargetManaged {
		return base.Result[domain.RegistryCleanupPlan]{}, base.ErrRegistryExternalLifecycle
	}
	idemScope := "registry-cleanup.execute:" + planID
	if old, ok, findErr := findIdem(ctx, tx, actor, idemScope, key); findErr != nil {
		return base.Result[domain.RegistryCleanupPlan]{}, findErr
	} else if ok {
		if old.fingerprint != fingerprint {
			return base.Result[domain.RegistryCleanupPlan]{}, base.ErrIdempotencyConflict
		}
		current, loadErr := registryCleanupPlan(ctx, tx, old.resourceID)
		if loadErr != nil || current.ServiceID != applicationID {
			if loadErr != nil {
				return base.Result[domain.RegistryCleanupPlan]{}, loadErr
			}
			return base.Result[domain.RegistryCleanupPlan]{}, base.ErrNotFound
		}
		if err = tx.Commit(ctx); err != nil {
			return base.Result[domain.RegistryCleanupPlan]{}, err
		}
		return base.Result[domain.RegistryCleanupPlan]{Value: current, Replay: true}, nil
	}
	plan, err := registryCleanupPlan(ctx, tx, planID)
	if err != nil {
		return base.Result[domain.RegistryCleanupPlan]{}, err
	}
	if state != "preview" && !base.RegistryCleanupPlanCanResumeOfflineSweep(plan) {
		return base.Result[domain.RegistryCleanupPlan]{}, base.ErrConflict
	}
	if err = putIdem(ctx, tx, actor, idemScope, key, fingerprint, "registry-cleanup-plan", plan.ID, nil); err != nil {
		return base.Result[domain.RegistryCleanupPlan]{}, classify(err)
	}
	if err = audit(ctx, tx, actor, "registry-cleanup.execute.accepted", "registry-cleanup-plan", plan.ID, requestID, map[string]any{
		"applicationId": applicationID, "registryTargetId": plan.RegistryTargetID,
		"planDigest": plan.PlanDigest, "confirmation": confirmation,
	}); err != nil {
		return base.Result[domain.RegistryCleanupPlan]{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return base.Result[domain.RegistryCleanupPlan]{}, err
	}
	return base.Result[domain.RegistryCleanupPlan]{Value: plan}, nil
}
