package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/domain"
	base "github.com/kuberploy/kuberploy/internal/store"
)

type cleanupObservationsJSON struct {
	Inventory   domain.RegistryInventoryObservation   `json:"inventory"`
	Catalogs    []domain.RegistryCatalogObservation   `json:"catalogs"`
	Authorities []domain.RegistryAuthorityObservation `json:"authorities"`
}

func (s *Store) SaveRegistryCleanupPlan(ctx context.Context, plan domain.RegistryCleanupPlan) (domain.RegistryCleanupPlan, bool, error) {
	if plan.ID == "" || plan.RegistryTargetID == "" || plan.ServiceID == "" || plan.PlanDigest == "" || plan.PlanDigest != base.RegistryCleanupPlanDigest(plan) {
		return domain.RegistryCleanupPlan{}, false, base.ErrRegistryPolicyInvalid
	}
	plan.CreatedAt = databaseTime(plan.CreatedAt)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return domain.RegistryCleanupPlan{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	saved, replay, err := saveRegistryCleanupPlan(ctx, tx, plan)
	if err != nil {
		return domain.RegistryCleanupPlan{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.RegistryCleanupPlan{}, false, err
	}
	return saved, replay, nil
}

func saveRegistryCleanupPlan(ctx context.Context, tx pgx.Tx, plan domain.RegistryCleanupPlan) (domain.RegistryCleanupPlan, bool, error) {
	var err error
	if err = registryAdvisoryLock(ctx, tx, plan.RegistryTargetID, plan.ServiceID, "cleanup-plan"); err != nil {
		return domain.RegistryCleanupPlan{}, false, err
	}
	target, err := registryTarget(ctx, tx, plan.RegistryTargetID)
	if err != nil {
		return domain.RegistryCleanupPlan{}, false, err
	}
	if target.Mode != domain.RegistryTargetManaged {
		return domain.RegistryCleanupPlan{}, false, base.ErrRegistryExternalLifecycle
	}
	current, err := registryLifecycleSnapshot(ctx, tx, plan.RegistryTargetID, plan.ServiceID, plan.CreatedAt)
	if err != nil {
		return domain.RegistryCleanupPlan{}, false, err
	}
	if base.RegistrySnapshotToken(current) != plan.SnapshotToken || base.RegistryAuthorityToken(current) != plan.AuthorityToken {
		return domain.RegistryCleanupPlan{}, false, base.ErrRegistrySnapshotStale
	}
	var existingID string
	err = tx.QueryRow(ctx, `SELECT id FROM registry_cleanup_plans
		WHERE registry_target_id=$1 AND service_id=$2 AND plan_digest=$3`,
		plan.RegistryTargetID, plan.ServiceID, plan.PlanDigest).Scan(&existingID)
	if err == nil {
		existing, loadErr := registryCleanupPlan(ctx, tx, existingID)
		if loadErr != nil {
			return domain.RegistryCleanupPlan{}, false, loadErr
		}
		return existing, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.RegistryCleanupPlan{}, false, err
	}
	policyJSON, _ := json.Marshal(plan.Policy)
	observationsJSON, _ := json.Marshal(cleanupObservationsJSON{Inventory: plan.Inventory, Catalogs: plan.Catalogs, Authorities: plan.Authorities})
	summaryJSON, _ := json.Marshal(plan.Summary)
	_, err = tx.Exec(ctx, `INSERT INTO registry_cleanup_plans(
		id,registry_target_id,service_id,snapshot_token,authority_token,plan_digest,
		state,policy,observations,summary,created_at
	) VALUES($1,$2,$3,$4,$5,$6,'preview',$7,$8,$9,$10)`, plan.ID,
		plan.RegistryTargetID, plan.ServiceID, plan.SnapshotToken, plan.AuthorityToken,
		plan.PlanDigest, policyJSON, observationsJSON, summaryJSON, plan.CreatedAt)
	if err != nil {
		return domain.RegistryCleanupPlan{}, false, classify(err)
	}
	for _, item := range plan.Items {
		state := item.State
		if state == "" {
			state = "planned"
			if item.Disposition == domain.RegistryCleanupProtect {
				state = "protected"
			}
		}
		reasonsJSON, _ := json.Marshal(item.Reasons)
		updatedAt := databaseTime(item.UpdatedAt)
		if updatedAt.IsZero() {
			updatedAt = plan.CreatedAt
		}
		_, err = tx.Exec(ctx, `INSERT INTO registry_cleanup_items(
			plan_id,ordinal,repository,resource_kind,digest,disposition,action,
			estimated_bytes,reasons,state,provider_message,updated_at
		) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, plan.ID,
			item.Ordinal, item.Repository, item.ResourceKind, item.Digest,
			item.Disposition, item.Action, item.EstimatedBytes, reasonsJSON,
			state, item.ProviderMessage, updatedAt)
		if err != nil {
			return domain.RegistryCleanupPlan{}, false, classify(err)
		}
	}
	saved, err := registryCleanupPlan(ctx, tx, plan.ID)
	return saved, false, err
}

func (s *Store) RegistryCleanupPlan(ctx context.Context, planID string) (domain.RegistryCleanupPlan, error) {
	return registryCleanupPlan(ctx, s.pool, planID)
}

func registryCleanupPlan(ctx context.Context, q registryDB, planID string) (domain.RegistryCleanupPlan, error) {
	var plan domain.RegistryCleanupPlan
	var policyJSON, observationsJSON, summaryJSON []byte
	err := q.QueryRow(ctx, `SELECT id,registry_target_id,service_id,snapshot_token,
		authority_token,plan_digest,state,policy,observations,summary,created_at,
		claimed_at,completed_at,failure FROM registry_cleanup_plans WHERE id=$1`, planID).
		Scan(&plan.ID, &plan.RegistryTargetID, &plan.ServiceID, &plan.SnapshotToken,
			&plan.AuthorityToken, &plan.PlanDigest, &plan.State, &policyJSON,
			&observationsJSON, &summaryJSON, &plan.CreatedAt, &plan.ClaimedAt,
			&plan.CompletedAt, &plan.Failure)
	if err != nil {
		return domain.RegistryCleanupPlan{}, classify(err)
	}
	var observations cleanupObservationsJSON
	if err = json.Unmarshal(policyJSON, &plan.Policy); err != nil {
		return domain.RegistryCleanupPlan{}, err
	}
	if err = json.Unmarshal(observationsJSON, &observations); err != nil {
		return domain.RegistryCleanupPlan{}, err
	}
	plan.Inventory = observations.Inventory
	plan.Catalogs = observations.Catalogs
	plan.Authorities = observations.Authorities
	if err = json.Unmarshal(summaryJSON, &plan.Summary); err != nil {
		return domain.RegistryCleanupPlan{}, err
	}
	rows, err := q.Query(ctx, `SELECT ordinal,repository,resource_kind,digest,
		disposition,action,estimated_bytes,reasons,state,provider_message,updated_at
		FROM registry_cleanup_items WHERE plan_id=$1 ORDER BY ordinal`, planID)
	if err != nil {
		return domain.RegistryCleanupPlan{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var item domain.RegistryCleanupItem
		var reasonsJSON []byte
		if err = rows.Scan(&item.Ordinal, &item.Repository, &item.ResourceKind, &item.Digest,
			&item.Disposition, &item.Action, &item.EstimatedBytes, &reasonsJSON,
			&item.State, &item.ProviderMessage, &item.UpdatedAt); err != nil {
			return domain.RegistryCleanupPlan{}, err
		}
		if err = json.Unmarshal(reasonsJSON, &item.Reasons); err != nil {
			return domain.RegistryCleanupPlan{}, err
		}
		plan.Items = append(plan.Items, item)
	}
	return plan, rows.Err()
}

func (s *Store) ClaimRegistryCleanupPlan(ctx context.Context, planID, owner string, now time.Time, leaseDuration time.Duration) (domain.RegistryCleanupPlan, bool, error) {
	if owner == "" || leaseDuration <= 0 {
		return domain.RegistryCleanupPlan{}, false, base.ErrRegistryPolicyInvalid
	}
	now = databaseTime(now)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return domain.RegistryCleanupPlan{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var targetID, serviceID string
	if err = tx.QueryRow(ctx, `SELECT registry_target_id,service_id FROM registry_cleanup_plans WHERE id=$1 FOR UPDATE`, planID).Scan(&targetID, &serviceID); err != nil {
		return domain.RegistryCleanupPlan{}, false, classify(err)
	}
	if err = registryAdvisoryLock(ctx, tx, targetID, serviceID, "cleanup-execution"); err != nil {
		return domain.RegistryCleanupPlan{}, false, err
	}
	plan, err := registryCleanupPlan(ctx, tx, planID)
	if err != nil {
		return domain.RegistryCleanupPlan{}, false, err
	}
	target, err := registryTarget(ctx, tx, targetID)
	if err != nil {
		return domain.RegistryCleanupPlan{}, false, err
	}
	if target.Mode != domain.RegistryTargetManaged {
		return domain.RegistryCleanupPlan{}, false, base.ErrRegistryExternalLifecycle
	}
	if plan.State == "executing" {
		held, leaseErr := registryPlanLeasesHeld(ctx, tx, plan, owner, now)
		if leaseErr != nil {
			return domain.RegistryCleanupPlan{}, false, leaseErr
		}
		if !held {
			if err = acquireRegistryCleanupLeases(ctx, tx, plan, owner, now, leaseDuration); err != nil {
				return domain.RegistryCleanupPlan{}, false, err
			}
			if err = tx.Commit(ctx); err != nil {
				return domain.RegistryCleanupPlan{}, false, err
			}
			return plan, true, nil
		}
		if err = tx.Commit(ctx); err != nil {
			return domain.RegistryCleanupPlan{}, false, err
		}
		return plan, false, nil
	}
	if plan.State != "preview" {
		return plan, false, base.ErrConflict
	}
	snapshot, err := registryLifecycleSnapshot(ctx, tx, targetID, serviceID, now)
	if err != nil {
		return domain.RegistryCleanupPlan{}, false, err
	}
	if base.RegistrySnapshotToken(snapshot) != plan.SnapshotToken {
		if _, updateErr := tx.Exec(ctx, `UPDATE registry_cleanup_plans SET state='superseded' WHERE id=$1`, planID); updateErr != nil {
			return domain.RegistryCleanupPlan{}, false, updateErr
		}
		if err = tx.Commit(ctx); err != nil {
			return domain.RegistryCleanupPlan{}, false, err
		}
		return domain.RegistryCleanupPlan{}, false, base.ErrRegistrySnapshotStale
	}
	if err = acquireRegistryCleanupLeases(ctx, tx, plan, owner, now, leaseDuration); err != nil {
		return domain.RegistryCleanupPlan{}, false, err
	}
	_, err = tx.Exec(ctx, `UPDATE registry_cleanup_plans SET state='executing',claimed_at=$2 WHERE id=$1`, planID, now)
	if err != nil {
		return domain.RegistryCleanupPlan{}, false, classify(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.RegistryCleanupPlan{}, false, err
	}
	plan.State = "executing"
	plan.ClaimedAt = &now
	return plan, true, nil
}

func acquireRegistryCleanupLeases(ctx context.Context, tx pgx.Tx, plan domain.RegistryCleanupPlan, owner string, now time.Time, leaseDuration time.Duration) error {
	leaseUntil := now.Add(leaseDuration)
	for _, repository := range registryCleanupLeaseRepositories(plan) {
		var currentPlanID, currentOwner string
		var currentUntil time.Time
		err := tx.QueryRow(ctx, `SELECT plan_id,owner,lease_until FROM registry_cleanup_leases
			WHERE registry_target_id=$1 AND repository=$2 FOR UPDATE`, plan.RegistryTargetID, repository).
			Scan(&currentPlanID, &currentOwner, &currentUntil)
		if err == nil && currentUntil.After(now) && (currentPlanID != plan.ID || currentOwner != owner) {
			return base.ErrConflict
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO registry_cleanup_leases(
			registry_target_id,repository,plan_id,owner,lease_until
		) VALUES($1,$2,$3,$4,$5)
		ON CONFLICT(registry_target_id,repository) DO UPDATE SET
			plan_id=EXCLUDED.plan_id,owner=EXCLUDED.owner,lease_until=EXCLUDED.lease_until`,
			plan.RegistryTargetID, repository, plan.ID, owner, leaseUntil)
		if err != nil {
			return classify(err)
		}
	}
	return nil
}

func (s *Store) RenewRegistryCleanupPlanLeases(ctx context.Context, planID, owner string, now time.Time, leaseDuration time.Duration) error {
	if owner == "" || leaseDuration <= 0 {
		return base.ErrRegistryPolicyInvalid
	}
	now = databaseTime(now)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	plan, err := registryCleanupPlan(ctx, tx, planID)
	if err != nil {
		return err
	}
	held, err := registryPlanLeasesHeld(ctx, tx, plan, owner, now)
	if err != nil {
		return err
	}
	if plan.State != "executing" || !held {
		return base.ErrRegistryLeaseLost
	}
	tag, err := tx.Exec(ctx, `UPDATE registry_cleanup_leases SET lease_until=$3
		WHERE plan_id=$1 AND owner=$2 AND lease_until>$4`, planID, owner, now.Add(leaseDuration), now)
	if err != nil {
		return err
	}
	if int(tag.RowsAffected()) != len(registryCleanupLeaseRepositories(plan)) {
		return base.ErrRegistryLeaseLost
	}
	return tx.Commit(ctx)
}

func (s *Store) AuthorizeRegistryCleanupItem(ctx context.Context, planID string, ordinal int, owner string, now time.Time) (domain.RegistryCleanupItem, error) {
	now = databaseTime(now)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return domain.RegistryCleanupItem{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var targetID, serviceID string
	if err = tx.QueryRow(ctx, `SELECT registry_target_id,service_id FROM registry_cleanup_plans WHERE id=$1 FOR UPDATE`, planID).Scan(&targetID, &serviceID); err != nil {
		return domain.RegistryCleanupItem{}, classify(err)
	}
	if err = registryAdvisoryLock(ctx, tx, targetID, serviceID, "cleanup-execution"); err != nil {
		return domain.RegistryCleanupItem{}, err
	}
	plan, err := registryCleanupPlan(ctx, tx, planID)
	if err != nil {
		return domain.RegistryCleanupItem{}, err
	}
	target, err := registryTarget(ctx, tx, targetID)
	if err != nil {
		return domain.RegistryCleanupItem{}, err
	}
	if target.Mode != domain.RegistryTargetManaged {
		return domain.RegistryCleanupItem{}, base.ErrRegistryExternalLifecycle
	}
	item, ok := cleanupPlanItem(plan, ordinal)
	if !ok {
		return domain.RegistryCleanupItem{}, base.ErrNotFound
	}
	if plan.State != "executing" || item.Disposition != domain.RegistryCleanupDelete || item.State != "planned" {
		return domain.RegistryCleanupItem{}, base.ErrConflict
	}
	held, err := registryItemLeasesHeld(ctx, tx, plan, item, owner, now)
	if err != nil {
		return domain.RegistryCleanupItem{}, err
	}
	if !held {
		return domain.RegistryCleanupItem{}, base.ErrRegistryLeaseLost
	}
	if item.ResourceKind == "blob" {
		var unfinished int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM registry_cleanup_items
			WHERE plan_id=$1 AND resource_kind<>'blob' AND disposition='delete' AND state<>'deleted'`, planID).Scan(&unfinished); err != nil {
			return domain.RegistryCleanupItem{}, err
		}
		if unfinished != 0 {
			return domain.RegistryCleanupItem{}, base.ErrRegistrySnapshotStale
		}
	}
	snapshot, err := registryLifecycleSnapshot(ctx, tx, targetID, serviceID, now)
	if err != nil {
		return domain.RegistryCleanupItem{}, err
	}
	if base.RegistryAuthorityToken(snapshot) != plan.AuthorityToken {
		return domain.RegistryCleanupItem{}, base.ErrRegistrySnapshotStale
	}
	tag, err := tx.Exec(ctx, `UPDATE registry_cleanup_items SET state='deleting',updated_at=$3
		WHERE plan_id=$1 AND ordinal=$2 AND state='planned'`, planID, ordinal, now)
	if err != nil {
		return domain.RegistryCleanupItem{}, err
	}
	if tag.RowsAffected() != 1 {
		return domain.RegistryCleanupItem{}, base.ErrConflict
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.RegistryCleanupItem{}, err
	}
	item.State = "deleting"
	item.UpdatedAt = now
	return item, nil
}

func (s *Store) RecordRegistryCleanupItemResult(ctx context.Context, planID string, ordinal int, owner string, result domain.RegistryCleanupItemResult) error {
	if result.State != "deleted" && result.State != "skipped" && result.State != "failed" || result.ObservedAt.IsZero() {
		return base.ErrRegistryPolicyInvalid
	}
	result.ObservedAt = databaseTime(result.ObservedAt)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var targetID, serviceID string
	if err = tx.QueryRow(ctx, `SELECT registry_target_id,service_id FROM registry_cleanup_plans WHERE id=$1 FOR UPDATE`, planID).Scan(&targetID, &serviceID); err != nil {
		return classify(err)
	}
	if err = registryAdvisoryLock(ctx, tx, targetID, serviceID, "cleanup-execution"); err != nil {
		return err
	}
	plan, err := registryCleanupPlan(ctx, tx, planID)
	if err != nil {
		return err
	}
	item, ok := cleanupPlanItem(plan, ordinal)
	if !ok {
		return base.ErrNotFound
	}
	if plan.State != "executing" {
		return base.ErrRegistryLeaseLost
	}
	held, err := registryItemLeasesHeld(ctx, tx, plan, item, owner, result.ObservedAt)
	if err != nil {
		return err
	}
	if !held {
		return base.ErrRegistryLeaseLost
	}
	if item.State != "deleting" {
		if item.State == result.State && item.ProviderMessage == result.ProviderMessage {
			return tx.Commit(ctx)
		}
		return base.ErrConflict
	}
	_, err = tx.Exec(ctx, `UPDATE registry_cleanup_items SET state=$3,
		provider_message=$4,updated_at=$5 WHERE plan_id=$1 AND ordinal=$2`,
		planID, ordinal, result.State, result.ProviderMessage, result.ObservedAt)
	if err != nil {
		return err
	}
	if result.State == "deleted" {
		if item.ResourceKind == "blob" {
			_, err = tx.Exec(ctx, `UPDATE registry_blobs SET present=false,deleted_at=$3
				WHERE registry_target_id=$1 AND digest=$2 AND present=true`, targetID, item.Digest, result.ObservedAt)
		} else {
			_, err = tx.Exec(ctx, `UPDATE registry_manifests SET present=false,deleted_at=$4
				WHERE registry_target_id=$1 AND repository=$2 AND digest=$3 AND present=true`,
				targetID, item.Repository, item.Digest, result.ObservedAt)
			if err == nil {
				_, err = tx.Exec(ctx, `UPDATE registry_releases SET availability='expired',
					availability_observed_at=$4 WHERE registry_target_id=$1
					AND repository=$2 AND root_digest=$3`, targetID, item.Repository, item.Digest, result.ObservedAt)
			}
			if err == nil {
				_, err = tx.Exec(ctx, `UPDATE registry_cache_generations SET state='deleted'
					WHERE registry_target_id=$1 AND repository=$2 AND root_digest=$3`, targetID, item.Repository, item.Digest)
			}
		}
		if err != nil {
			return err
		}
	}
	snapshot, err := registryLifecycleSnapshot(ctx, tx, targetID, serviceID, result.ObservedAt)
	if err != nil {
		return err
	}
	authorityToken := base.RegistryAuthorityToken(snapshot)
	if _, err = tx.Exec(ctx, `UPDATE registry_cleanup_plans SET authority_token=$2 WHERE id=$1`, planID, authorityToken); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) FinishRegistryCleanupPlan(ctx context.Context, planID, owner string, succeeded bool, failure string, now time.Time) error {
	now = databaseTime(now)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	plan, err := registryCleanupPlan(ctx, tx, planID)
	if err != nil {
		return err
	}
	desired := "failed"
	if succeeded {
		desired = "succeeded"
	}
	if plan.State == desired {
		return tx.Commit(ctx)
	}
	held, err := registryPlanLeasesHeld(ctx, tx, plan, owner, now)
	if err != nil {
		return err
	}
	if plan.State != "executing" || !held {
		return base.ErrRegistryLeaseLost
	}
	if succeeded {
		var unfinished int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM registry_cleanup_items
			WHERE plan_id=$1 AND disposition='delete' AND state<>'deleted'`, planID).Scan(&unfinished); err != nil {
			return err
		}
		if unfinished != 0 {
			return base.ErrConflict
		}
		failure = ""
	} else if failure == "" {
		return base.ErrRegistryPolicyInvalid
	}
	if _, err = tx.Exec(ctx, `UPDATE registry_cleanup_plans SET state=$2,
		completed_at=$3,failure=$4 WHERE id=$1`, planID, desired, now, failure); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM registry_cleanup_leases WHERE plan_id=$1 AND owner=$2`, planID, owner); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func registryCleanupLeaseRepositories(plan domain.RegistryCleanupPlan) []string {
	set := map[string]struct{}{"*": {}}
	for _, item := range plan.Items {
		if item.Disposition == domain.RegistryCleanupDelete {
			set[item.Repository] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for repository := range set {
		out = append(out, repository)
	}
	sort.Strings(out)
	return out
}

func registryPlanLeasesHeld(ctx context.Context, q registryDB, plan domain.RegistryCleanupPlan, owner string, now time.Time) (bool, error) {
	var count int
	err := q.QueryRow(ctx, `SELECT count(*) FROM registry_cleanup_leases
		WHERE registry_target_id=$1 AND plan_id=$2 AND owner=$3 AND lease_until>$4`,
		plan.RegistryTargetID, plan.ID, owner, now).Scan(&count)
	return count == len(registryCleanupLeaseRepositories(plan)), err
}

func registryItemLeasesHeld(ctx context.Context, q registryDB, plan domain.RegistryCleanupPlan, item domain.RegistryCleanupItem, owner string, now time.Time) (bool, error) {
	repositories := []string{"*"}
	if item.Repository != "*" {
		repositories = append(repositories, item.Repository)
	}
	var count int
	err := q.QueryRow(ctx, `SELECT count(*) FROM registry_cleanup_leases
		WHERE registry_target_id=$1 AND plan_id=$2 AND owner=$3
			AND lease_until>$4 AND repository=ANY($5)`, plan.RegistryTargetID,
		plan.ID, owner, now, repositories).Scan(&count)
	return count == len(repositories), err
}

func cleanupPlanItem(plan domain.RegistryCleanupPlan, ordinal int) (domain.RegistryCleanupItem, bool) {
	for _, item := range plan.Items {
		if item.Ordinal == ordinal {
			return item, true
		}
	}
	return domain.RegistryCleanupItem{}, false
}
