package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/domain"
	base "github.com/kuberploy/kuberploy/internal/store"
)

var registryRuntimeOwnerRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$`)
var registryRuntimeCodeRE = regexp.MustCompile(`^[a-z][a-z0-9.-]{0,62}$`)
var registryRuntimeDigestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

const (
	maximumRegistryObservationRepositories = 128
	maximumRegistryObservationNodes        = 65536
)

func validRegistryRuntimeDuration(value time.Duration) bool {
	return value >= 10*time.Second && value <= time.Hour
}

func validRegistryObservationLease(lease base.RegistryObservationLease) bool {
	return lease.TargetID != "" && registryRuntimeOwnerRE.MatchString(lease.Owner) && lease.Epoch > 0 && lease.Revision > 0 && !lease.Until.IsZero()
}

func (s *Store) ClaimRegistryObservation(ctx context.Context, targetID, owner string, now time.Time, duration time.Duration) (base.RegistryObservationWork, error) {
	if targetID == "" || !registryRuntimeOwnerRE.MatchString(owner) || now.IsZero() || !validRegistryRuntimeDuration(duration) {
		return base.RegistryObservationWork{}, base.ErrRegistryPolicyInvalid
	}
	now = databaseTime(now)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return base.RegistryObservationWork{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	target, err := registryTarget(ctx, tx, targetID)
	if err != nil {
		return base.RegistryObservationWork{}, err
	}
	if err = registryAdvisoryLock(ctx, tx, targetID, "runtime-observation"); err != nil {
		return base.RegistryObservationWork{}, err
	}
	var completedRevision, epoch int64
	var failures int
	var nextAt, updatedAt time.Time
	var oldOwner *string
	var oldUntil *time.Time
	err = tx.QueryRow(ctx, `SELECT completed_revision,consecutive_failures,next_observe_at,
		lease_owner,lease_epoch,lease_until,updated_at FROM registry_runtime_observation_cursors
		WHERE registry_target_id=$1 FOR UPDATE`, targetID).
		Scan(&completedRevision, &failures, &nextAt, &oldOwner, &epoch, &oldUntil, &updatedAt)
	reclaimed := false
	if errors.Is(err, pgx.ErrNoRows) {
		epoch = 1
		_, err = tx.Exec(ctx, `INSERT INTO registry_runtime_observation_cursors(
			registry_target_id,next_observe_at,lease_owner,lease_epoch,lease_until,updated_at
		) VALUES($1,$2,$3,$4,$5,$2)`, targetID, now, owner, epoch, now.Add(duration))
	} else if err == nil {
		if oldUntil != nil && oldUntil.After(now) {
			return base.RegistryObservationWork{}, base.ErrConflict
		}
		if oldOwner == nil && nextAt.After(now) {
			return base.RegistryObservationWork{}, base.ErrNotFound
		}
		reclaimed = oldOwner != nil
		epoch++
		_, err = tx.Exec(ctx, `UPDATE registry_runtime_observation_cursors SET
			lease_owner=$2,lease_epoch=$3,lease_until=$4,updated_at=$5
			WHERE registry_target_id=$1`, targetID, owner, epoch, now.Add(duration), now)
	}
	if err != nil {
		return base.RegistryObservationWork{}, classify(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return base.RegistryObservationWork{}, classify(err)
	}
	lease := base.RegistryObservationLease{TargetID: targetID, Owner: owner, Epoch: epoch, Revision: completedRevision + 1, Until: now.Add(duration)}
	return base.RegistryObservationWork{Target: target, Lease: lease, ConsecutiveFailures: failures, Reclaimed: reclaimed}, nil
}

func (s *Store) HeartbeatRegistryObservation(ctx context.Context, lease base.RegistryObservationLease, now time.Time, duration time.Duration) (base.RegistryObservationLease, error) {
	if !validRegistryObservationLease(lease) || now.IsZero() || !validRegistryRuntimeDuration(duration) {
		return base.RegistryObservationLease{}, base.ErrRegistryPolicyInvalid
	}
	now = databaseTime(now)
	until := now.Add(duration)
	result, err := s.pool.Exec(ctx, `UPDATE registry_runtime_observation_cursors SET lease_until=$5,updated_at=$6
		WHERE registry_target_id=$1 AND lease_owner=$2 AND lease_epoch=$3
		AND completed_revision=$4-1 AND lease_until>$6`, lease.TargetID, lease.Owner, lease.Epoch, lease.Revision, until, now)
	if err != nil {
		return base.RegistryObservationLease{}, classify(err)
	}
	if result.RowsAffected() != 1 {
		return base.RegistryObservationLease{}, base.ErrRegistryLeaseLost
	}
	lease.Until = until
	return lease, nil
}

func validateRegistryPublication(lease base.RegistryObservationLease, publication base.RegistryObservationPublication) error {
	if !validRegistryObservationLease(lease) || publication.ObservedAt.IsZero() || !publication.NextAt.After(publication.ObservedAt) ||
		publication.Inventory.RegistryTargetID != lease.TargetID || !publication.Inventory.Complete || publication.Inventory.ObservedAt.IsZero() ||
		publication.Inventory.Revision != registryObservationRevision(lease.Revision) || len(publication.Inventory.Repositories) > maximumRegistryObservationRepositories ||
		len(publication.Catalogs) != len(publication.Inventory.Repositories) {
		return base.ErrRegistryObservationIncomplete
	}
	repositories := append([]string(nil), publication.Inventory.Repositories...)
	sort.Strings(repositories)
	nodes := 0
	seen := make(map[string]struct{}, len(repositories))
	for _, catalog := range publication.Catalogs {
		observation := catalog.Observation
		if observation.RegistryTargetID != lease.TargetID || observation.Revision != lease.Revision || !observation.Complete ||
			!observation.ObservedAt.Equal(publication.ObservedAt) {
			return base.ErrRegistryObservationIncomplete
		}
		if _, duplicate := seen[observation.Repository]; duplicate {
			return base.ErrRegistryObservationIncomplete
		}
		seen[observation.Repository] = struct{}{}
		nodes += len(catalog.Manifests) + len(catalog.Blobs) + len(catalog.Children) + len(catalog.BlobLinks)
	}
	if nodes > maximumRegistryObservationNodes {
		return base.ErrRegistryObservationIncomplete
	}
	for _, repository := range repositories {
		if _, present := seen[repository]; !present {
			return base.ErrRegistryObservationIncomplete
		}
	}
	return nil
}

func registryObservationRevision(revision int64) string {
	return "registry-runtime-" + strconv.FormatInt(revision, 10)
}

func (s *Store) PublishRegistryObservation(ctx context.Context, lease base.RegistryObservationLease, publication base.RegistryObservationPublication) error {
	if err := validateRegistryPublication(lease, publication); err != nil {
		return err
	}
	publication.ObservedAt = databaseTime(publication.ObservedAt)
	publication.NextAt = databaseTime(publication.NextAt)
	publication.Inventory.ObservedAt = publication.ObservedAt
	var err error
	publication.Inventory, err = normalizeRegistryInventory(publication.Inventory)
	if err != nil {
		return err
	}
	for index := range publication.Catalogs {
		publication.Catalogs[index].Observation.ObservedAt = publication.ObservedAt
		publication.Catalogs[index], err = normalizeRegistryCatalog(publication.Catalogs[index])
		if err != nil {
			return err
		}
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = requireRegistryObservationLease(ctx, tx, lease, publication.ObservedAt); err != nil {
		return err
	}
	for _, catalog := range publication.Catalogs {
		if err = replaceRegistryCatalogTx(ctx, tx, catalog); err != nil {
			return err
		}
	}
	if err = recordRegistryInventoryTx(ctx, tx, publication.Inventory); err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `UPDATE registry_runtime_observation_cursors SET
		completed_revision=$4,completed_at=$5,next_observe_at=$6,consecutive_failures=0,
		last_error_code='',lease_owner=NULL,lease_until=NULL,updated_at=$5
		WHERE registry_target_id=$1 AND lease_owner=$2 AND lease_epoch=$3
		AND completed_revision=$4-1 AND lease_until>$5`, lease.TargetID, lease.Owner, lease.Epoch,
		lease.Revision, publication.ObservedAt, publication.NextAt)
	if err != nil {
		return classify(err)
	}
	if result.RowsAffected() != 1 {
		return base.ErrRegistryLeaseLost
	}
	return classify(tx.Commit(ctx))
}

func requireRegistryObservationLease(ctx context.Context, query registryDB, lease base.RegistryObservationLease, now time.Time) error {
	var active bool
	err := query.QueryRow(ctx, `SELECT lease_owner=$2 AND lease_epoch=$3 AND
		completed_revision=$4-1 AND lease_until>$5 FROM registry_runtime_observation_cursors
		WHERE registry_target_id=$1 FOR UPDATE`, lease.TargetID, lease.Owner, lease.Epoch, lease.Revision, now).Scan(&active)
	if err != nil {
		return classify(err)
	}
	if !active {
		return base.ErrRegistryLeaseLost
	}
	return nil
}

func (s *Store) FailRegistryObservation(ctx context.Context, lease base.RegistryObservationLease, outcome base.RegistryObservationOutcome, now time.Time) error {
	if !validRegistryObservationLease(lease) || now.IsZero() || !outcome.NextAt.After(now) || !registryRuntimeCodeRE.MatchString(outcome.FailureCode) {
		return base.ErrRegistryPolicyInvalid
	}
	now = databaseTime(now)
	result, err := s.pool.Exec(ctx, `UPDATE registry_runtime_observation_cursors SET
		next_observe_at=$4,consecutive_failures=consecutive_failures+1,last_error_code=$5,
		lease_owner=NULL,lease_until=NULL,updated_at=$6
		WHERE registry_target_id=$1 AND lease_owner=$2 AND lease_epoch=$3
		AND lease_until>$6 AND completed_revision=$7-1`, lease.TargetID, lease.Owner, lease.Epoch,
		databaseTime(outcome.NextAt), outcome.FailureCode, now, lease.Revision)
	if err != nil {
		return classify(err)
	}
	if result.RowsAffected() != 1 {
		return base.ErrRegistryLeaseLost
	}
	return nil
}

func (s *Store) RegistryObservationRoots(ctx context.Context, targetID string) (map[string][]string, error) {
	if targetID == "" {
		return nil, base.ErrRegistryPolicyInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT repository,digest FROM (
		SELECT repository,NULL::text AS digest FROM service_registry_policies WHERE registry_target_id=$1
		UNION
		SELECT repository,root_digest AS digest FROM registry_releases WHERE registry_target_id=$1
		UNION SELECT repository,root_digest FROM registry_cache_generations WHERE registry_target_id=$1 AND state NOT IN ('deleted','missing')
		UNION SELECT repository,digest FROM registry_artifact_references WHERE registry_target_id=$1
		UNION SELECT repository,digest FROM registry_manifests WHERE registry_target_id=$1 AND present=true
	) roots ORDER BY repository,digest LIMIT 65537`, targetID)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()
	result := make(map[string][]string)
	count := 0
	for rows.Next() {
		var repository string
		var digest *string
		if err = rows.Scan(&repository, &digest); err != nil {
			return nil, err
		}
		count++
		if count > 65536 || repository == "" || digest != nil && !registryRuntimeDigestRE.MatchString(*digest) {
			return nil, base.ErrRegistryObservationIncomplete
		}
		if _, exists := result[repository]; !exists {
			result[repository] = nil
		}
		if digest != nil {
			result[repository] = append(result[repository], *digest)
		}
	}
	return result, rows.Err()
}

func (s *Store) NextAcceptedRegistryCleanup(ctx context.Context, targetID string, now time.Time) (string, error) {
	if targetID == "" || now.IsZero() {
		return "", base.ErrRegistryPolicyInvalid
	}
	var planID string
	err := s.pool.QueryRow(ctx, `SELECT p.id::text FROM registry_cleanup_plans p
		JOIN registry_targets t ON t.id=p.registry_target_id AND t.mode='managed'
		JOIN idempotency_keys i ON i.resource_type='registry-cleanup-plan' AND i.resource_id=p.id
			AND i.scope='registry-cleanup.execute:'||p.id::text
		WHERE p.registry_target_id=$1 AND p.state IN ('preview','executing') AND p.created_at<=$2
		ORDER BY CASE p.state WHEN 'executing' THEN 0 ELSE 1 END,p.created_at,p.id LIMIT 1`, targetID, databaseTime(now)).Scan(&planID)
	return planID, classify(err)
}

func validRegistryMaintenanceLease(lease base.RegistryMaintenanceLease) bool {
	return lease.TargetID != "" && lease.PlanID != "" && registryRuntimeDigestRE.MatchString(lease.ExecutionKey) &&
		registryRuntimeDigestRE.MatchString(lease.CandidateSetDigest) && registryRuntimeOwnerRE.MatchString(lease.Owner) &&
		lease.Epoch > 0 && !lease.Until.IsZero()
}

func registryCandidateSetDigest(digests []string) (string, error) {
	ordered := append([]string(nil), digests...)
	sort.Strings(ordered)
	for index, digest := range ordered {
		if !registryRuntimeDigestRE.MatchString(digest) || index > 0 && digest == ordered[index-1] {
			return "", base.ErrRegistryPolicyInvalid
		}
	}
	encoded, _ := json.Marshal(ordered)
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (s *Store) validateMaintenancePlan(ctx context.Context, tx pgx.Tx, targetID, planID, owner, candidateSetDigest string, now time.Time) error {
	var mode domain.RegistryTargetMode
	var planTarget, state string
	err := tx.QueryRow(ctx, `SELECT t.mode,p.registry_target_id::text,p.state
		FROM registry_cleanup_plans p JOIN registry_targets t ON t.id=p.registry_target_id
		WHERE p.id=$1 FOR UPDATE OF p,t`, planID).Scan(&mode, &planTarget, &state)
	if err != nil {
		return classify(err)
	}
	if mode != domain.RegistryTargetManaged || planTarget != targetID {
		return base.ErrRegistryExternalLifecycle
	}
	if state != "executing" {
		return base.ErrConflict
	}
	var held bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM registry_cleanup_leases
		WHERE registry_target_id=$1 AND plan_id=$2 AND repository='*' AND owner=$3 AND lease_until>$4)`,
		targetID, planID, owner, now).Scan(&held)
	if err != nil || !held {
		if err != nil {
			return err
		}
		return base.ErrRegistryLeaseLost
	}
	rows, err := tx.Query(ctx, `SELECT digest FROM registry_cleanup_items WHERE plan_id=$1
		AND resource_kind='blob' AND disposition='delete' AND state IN ('deleting','deleted') ORDER BY digest`, planID)
	if err != nil {
		return err
	}
	var digests []string
	for rows.Next() {
		var digest string
		if err = rows.Scan(&digest); err != nil {
			rows.Close()
			return err
		}
		digests = append(digests, digest)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	digest, err := registryCandidateSetDigest(digests)
	if err != nil || digest != candidateSetDigest || len(digests) == 0 {
		return base.ErrRegistrySnapshotStale
	}
	return nil
}

func (s *Store) AcquireRegistryMaintenance(ctx context.Context, targetID, planID, executionKey, candidateSetDigest, owner string, now time.Time, duration time.Duration) (base.RegistryMaintenanceLease, error) {
	if targetID == "" || planID == "" || !registryRuntimeDigestRE.MatchString(executionKey) || !registryRuntimeDigestRE.MatchString(candidateSetDigest) ||
		!registryRuntimeOwnerRE.MatchString(owner) || now.IsZero() || !validRegistryRuntimeDuration(duration) {
		return base.RegistryMaintenanceLease{}, base.ErrRegistryPolicyInvalid
	}
	now = databaseTime(now)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return base.RegistryMaintenanceLease{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = registryAdvisoryLock(ctx, tx, targetID, "runtime-maintenance"); err != nil {
		return base.RegistryMaintenanceLease{}, err
	}
	if err = s.validateMaintenancePlan(ctx, tx, targetID, planID, owner, candidateSetDigest, now); err != nil {
		return base.RegistryMaintenanceLease{}, err
	}
	lease, found, err := registryMaintenanceLease(ctx, tx, targetID, executionKey, true)
	if err != nil {
		return base.RegistryMaintenanceLease{}, err
	}
	if found {
		if lease.PlanID != planID || lease.CandidateSetDigest != candidateSetDigest {
			return base.RegistryMaintenanceLease{}, base.ErrConflict
		}
		if lease.ReleasedAt.IsZero() && lease.Until.After(now) && lease.Owner != owner {
			return base.RegistryMaintenanceLease{}, base.ErrConflict
		}
		lease.Owner = owner
		lease.Epoch++
		lease.Until = now.Add(duration)
		if !lease.ReleasedAt.IsZero() || lease.State == "restored" || lease.State == "failed" {
			lease.State, lease.Mode, lease.DeploymentUID, lease.OriginalReplicas = "acquired", "", "", 0
			lease.CheckpointRevision, lease.CheckpointDigest, lease.CheckpointObservedAt = "", "", time.Time{}
			lease.SweepJobUID, lease.RestoredAt, lease.ReleasedAt = "", time.Time{}, time.Time{}
		}
		_, err = tx.Exec(ctx, `UPDATE registry_runtime_maintenance_executions SET
			state=$4,maintenance_mode=NULLIF($5,''),deployment_uid=$6,original_replicas=$7,
			checkpoint_revision=$8,checkpoint_digest=$9,checkpoint_observed_at=NULLIF($10,'0001-01-01T00:00:00Z'::timestamptz),
			sweep_job_uid=$11,lease_owner=$12,lease_epoch=$13,lease_until=$14,
			restored_at=NULLIF($15,'0001-01-01T00:00:00Z'::timestamptz),released_at=NULLIF($16,'0001-01-01T00:00:00Z'::timestamptz),updated_at=$17
			WHERE registry_target_id=$1 AND execution_key=$2 AND plan_id=$3`, targetID, executionKey, planID,
			lease.State, lease.Mode, lease.DeploymentUID, nullableReplicas(lease), lease.CheckpointRevision, lease.CheckpointDigest,
			lease.CheckpointObservedAt, lease.SweepJobUID, owner, lease.Epoch, lease.Until, lease.RestoredAt, lease.ReleasedAt, now)
	} else {
		var active string
		activeErr := tx.QueryRow(ctx, `SELECT execution_key FROM registry_runtime_maintenance_executions
			WHERE registry_target_id=$1 AND released_at IS NULL FOR UPDATE`, targetID).Scan(&active)
		if activeErr == nil {
			return base.RegistryMaintenanceLease{}, base.ErrConflict
		}
		if !errors.Is(activeErr, pgx.ErrNoRows) {
			return base.RegistryMaintenanceLease{}, activeErr
		}
		lease = base.RegistryMaintenanceLease{TargetID: targetID, PlanID: planID, ExecutionKey: executionKey,
			CandidateSetDigest: candidateSetDigest, Owner: owner, Epoch: 1, Until: now.Add(duration), State: "acquired"}
		_, err = tx.Exec(ctx, `INSERT INTO registry_runtime_maintenance_executions(
			registry_target_id,execution_key,plan_id,candidate_set_digest,state,lease_owner,lease_epoch,lease_until,created_at,updated_at
		) VALUES($1,$2,$3,$4,'acquired',$5,1,$6,$7,$7)`, targetID, executionKey, planID, candidateSetDigest, owner, lease.Until, now)
	}
	if err != nil {
		return base.RegistryMaintenanceLease{}, classify(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return base.RegistryMaintenanceLease{}, classify(err)
	}
	return lease, nil
}

func nullableReplicas(lease base.RegistryMaintenanceLease) any {
	if lease.DeploymentUID == "" {
		return nil
	}
	return lease.OriginalReplicas
}

func registryMaintenanceLease(ctx context.Context, q registryDB, targetID, executionKey string, lock bool) (base.RegistryMaintenanceLease, bool, error) {
	suffix := ""
	if lock {
		suffix = " FOR UPDATE"
	}
	var lease base.RegistryMaintenanceLease
	var mode *string
	var replicas *int32
	var checkpointAt, restoredAt, releasedAt *time.Time
	err := q.QueryRow(ctx, `SELECT registry_target_id::text,plan_id::text,execution_key,candidate_set_digest,
		lease_owner,lease_epoch,lease_until,state,maintenance_mode,deployment_uid,original_replicas,
		checkpoint_revision,checkpoint_digest,checkpoint_observed_at,sweep_job_uid,restored_at,released_at
		FROM registry_runtime_maintenance_executions WHERE registry_target_id=$1 AND execution_key=$2`+suffix,
		targetID, executionKey).Scan(&lease.TargetID, &lease.PlanID, &lease.ExecutionKey, &lease.CandidateSetDigest,
		&lease.Owner, &lease.Epoch, &lease.Until, &lease.State, &mode, &lease.DeploymentUID, &replicas,
		&lease.CheckpointRevision, &lease.CheckpointDigest, &checkpointAt, &lease.SweepJobUID, &restoredAt, &releasedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return base.RegistryMaintenanceLease{}, false, nil
	}
	if err != nil {
		return base.RegistryMaintenanceLease{}, false, classify(err)
	}
	if mode != nil {
		lease.Mode = *mode
	}
	if replicas != nil {
		lease.OriginalReplicas = *replicas
	}
	if checkpointAt != nil {
		lease.CheckpointObservedAt = *checkpointAt
	}
	if restoredAt != nil {
		lease.RestoredAt = *restoredAt
	}
	if releasedAt != nil {
		lease.ReleasedAt = *releasedAt
	}
	return lease, true, nil
}

func requireRegistryMaintenanceLease(ctx context.Context, tx pgx.Tx, lease base.RegistryMaintenanceLease, now time.Time) (base.RegistryMaintenanceLease, error) {
	if !validRegistryMaintenanceLease(lease) || now.IsZero() {
		return base.RegistryMaintenanceLease{}, base.ErrRegistryPolicyInvalid
	}
	current, found, err := registryMaintenanceLease(ctx, tx, lease.TargetID, lease.ExecutionKey, true)
	if err != nil {
		return base.RegistryMaintenanceLease{}, err
	}
	if !found || current.PlanID != lease.PlanID || current.CandidateSetDigest != lease.CandidateSetDigest ||
		current.Owner != lease.Owner || current.Epoch != lease.Epoch || !current.Until.After(now) || !current.ReleasedAt.IsZero() {
		return base.RegistryMaintenanceLease{}, base.ErrRegistryLeaseLost
	}
	return current, nil
}

func (s *Store) HeartbeatRegistryMaintenance(ctx context.Context, lease base.RegistryMaintenanceLease, now time.Time, duration time.Duration) (base.RegistryMaintenanceLease, error) {
	if !validRegistryMaintenanceLease(lease) || now.IsZero() || !validRegistryRuntimeDuration(duration) {
		return base.RegistryMaintenanceLease{}, base.ErrRegistryPolicyInvalid
	}
	now = databaseTime(now)
	until := now.Add(duration)
	result, err := s.pool.Exec(ctx, `UPDATE registry_runtime_maintenance_executions SET lease_until=$6,updated_at=$7
		WHERE registry_target_id=$1 AND execution_key=$2 AND plan_id=$3 AND lease_owner=$4 AND lease_epoch=$5
		AND lease_until>$7 AND released_at IS NULL`, lease.TargetID, lease.ExecutionKey, lease.PlanID, lease.Owner, lease.Epoch, until, now)
	if err != nil {
		return base.RegistryMaintenanceLease{}, classify(err)
	}
	if result.RowsAffected() != 1 {
		return base.RegistryMaintenanceLease{}, base.ErrRegistryLeaseLost
	}
	lease.Until = until
	return lease, nil
}

func (s *Store) EnterRegistryMaintenance(ctx context.Context, lease base.RegistryMaintenanceLease, deploymentUID string, originalReplicas int32, mode string, now time.Time) (base.RegistryMaintenanceLease, error) {
	if deploymentUID == "" || strings.ContainsAny(deploymentUID, "\x00\r\n\t ") || originalReplicas < 1 || mode != "stopped" || now.IsZero() {
		return base.RegistryMaintenanceLease{}, base.ErrRegistryPolicyInvalid
	}
	return s.transitionRegistryMaintenance(ctx, lease, now, func(current base.RegistryMaintenanceLease) (base.RegistryMaintenanceLease, error) {
		if current.DeploymentUID != deploymentUID || current.OriginalReplicas != originalReplicas ||
			(current.State != "acquired" && current.State != "entered" && current.State != "checkpointed" && current.State != "sweeping" && current.State != "swept") {
			return base.RegistryMaintenanceLease{}, base.ErrConflict
		}
		current.State, current.Mode, current.DeploymentUID, current.OriginalReplicas = "entered", mode, deploymentUID, originalReplicas
		return current, nil
	})
}

func (s *Store) PrepareRegistryMaintenanceStop(ctx context.Context, lease base.RegistryMaintenanceLease, deploymentUID string, originalReplicas int32, now time.Time) (base.RegistryMaintenanceLease, error) {
	if deploymentUID == "" || strings.ContainsAny(deploymentUID, "\x00\r\n\t ") || originalReplicas < 1 || now.IsZero() {
		return base.RegistryMaintenanceLease{}, base.ErrRegistryPolicyInvalid
	}
	return s.transitionRegistryMaintenance(ctx, lease, now, func(current base.RegistryMaintenanceLease) (base.RegistryMaintenanceLease, error) {
		if current.State != "acquired" || current.Mode != "" ||
			(current.DeploymentUID != "" && (current.DeploymentUID != deploymentUID || current.OriginalReplicas != originalReplicas)) {
			return base.RegistryMaintenanceLease{}, base.ErrConflict
		}
		current.DeploymentUID, current.OriginalReplicas = deploymentUID, originalReplicas
		return current, nil
	})
}

func (s *Store) RecordRegistryCheckpoint(ctx context.Context, lease base.RegistryMaintenanceLease, revision, digest string, observedAt, now time.Time) (base.RegistryMaintenanceLease, error) {
	if revision == "" || len(revision) > 256 || !registryRuntimeDigestRE.MatchString(digest) || observedAt.IsZero() || now.IsZero() || observedAt.After(now) {
		return base.RegistryMaintenanceLease{}, base.ErrRegistryPolicyInvalid
	}
	return s.transitionRegistryMaintenance(ctx, lease, now, func(current base.RegistryMaintenanceLease) (base.RegistryMaintenanceLease, error) {
		if current.State != "entered" && current.State != "sweeping" && current.State != "swept" &&
			(current.State != "checkpointed" || current.CheckpointRevision != revision || current.CheckpointDigest != digest || !current.CheckpointObservedAt.Equal(observedAt)) {
			return base.RegistryMaintenanceLease{}, base.ErrConflict
		}
		current.State, current.CheckpointRevision, current.CheckpointDigest, current.CheckpointObservedAt = "checkpointed", revision, digest, databaseTime(observedAt)
		return current, nil
	})
}

func (s *Store) transitionRegistryMaintenance(ctx context.Context, lease base.RegistryMaintenanceLease, now time.Time, transition func(base.RegistryMaintenanceLease) (base.RegistryMaintenanceLease, error)) (base.RegistryMaintenanceLease, error) {
	now = databaseTime(now)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return base.RegistryMaintenanceLease{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	current, err := requireRegistryMaintenanceLease(ctx, tx, lease, now)
	if err != nil {
		return base.RegistryMaintenanceLease{}, err
	}
	current, err = transition(current)
	if err != nil {
		return base.RegistryMaintenanceLease{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE registry_runtime_maintenance_executions SET state=$6,
		maintenance_mode=NULLIF($7,''),deployment_uid=$8,original_replicas=$9,
		checkpoint_revision=$10,checkpoint_digest=$11,
		checkpoint_observed_at=NULLIF($12,'0001-01-01T00:00:00Z'::timestamptz),sweep_job_uid=$13,updated_at=$14
		WHERE registry_target_id=$1 AND execution_key=$2 AND plan_id=$3 AND lease_owner=$4 AND lease_epoch=$5`,
		current.TargetID, current.ExecutionKey, current.PlanID, current.Owner, current.Epoch, current.State, current.Mode,
		current.DeploymentUID, nullableReplicas(current), current.CheckpointRevision, current.CheckpointDigest,
		current.CheckpointObservedAt, current.SweepJobUID, now)
	if err != nil {
		return base.RegistryMaintenanceLease{}, classify(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return base.RegistryMaintenanceLease{}, classify(err)
	}
	return current, nil
}

func (s *Store) BeginRegistryGCSweep(ctx context.Context, lease base.RegistryMaintenanceLease, helperJobUID string, now time.Time) (base.RegistryGCSweepReceipt, bool, error) {
	if helperJobUID == "" || strings.ContainsAny(helperJobUID, "\x00\r\n\t ") || now.IsZero() {
		return base.RegistryGCSweepReceipt{}, false, base.ErrRegistryPolicyInvalid
	}
	now = databaseTime(now)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return base.RegistryGCSweepReceipt{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	current, err := requireRegistryMaintenanceLease(ctx, tx, lease, now)
	if err != nil {
		return base.RegistryGCSweepReceipt{}, false, err
	}
	receipt, found, err := registryGCReceipt(ctx, tx, lease.TargetID, lease.ExecutionKey)
	if err != nil {
		return base.RegistryGCSweepReceipt{}, false, err
	}
	if found {
		if receipt.PlanID != lease.PlanID || receipt.CandidateSetDigest != lease.CandidateSetDigest {
			return base.RegistryGCSweepReceipt{}, false, base.ErrConflict
		}
		return receipt, true, tx.Commit(ctx)
	}
	if current.State != "checkpointed" && (current.State != "sweeping" || current.SweepJobUID != helperJobUID) {
		return base.RegistryGCSweepReceipt{}, false, base.ErrConflict
	}
	if current.State == "checkpointed" {
		_, err = tx.Exec(ctx, `UPDATE registry_runtime_maintenance_executions SET state='sweeping',sweep_job_uid=$6,updated_at=$7
			WHERE registry_target_id=$1 AND execution_key=$2 AND plan_id=$3 AND lease_owner=$4 AND lease_epoch=$5`,
			lease.TargetID, lease.ExecutionKey, lease.PlanID, lease.Owner, lease.Epoch, helperJobUID, now)
		if err != nil {
			return base.RegistryGCSweepReceipt{}, false, classify(err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return base.RegistryGCSweepReceipt{}, false, classify(err)
	}
	return base.RegistryGCSweepReceipt{}, false, nil
}

func (s *Store) RegistryGCSweepReceipt(ctx context.Context, lease base.RegistryMaintenanceLease, now time.Time) (base.RegistryGCSweepReceipt, bool, error) {
	if !validRegistryMaintenanceLease(lease) || now.IsZero() {
		return base.RegistryGCSweepReceipt{}, false, base.ErrRegistryPolicyInvalid
	}
	now = databaseTime(now)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return base.RegistryGCSweepReceipt{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = requireRegistryMaintenanceLease(ctx, tx, lease, now); err != nil {
		return base.RegistryGCSweepReceipt{}, false, err
	}
	receipt, found, err := registryGCReceipt(ctx, tx, lease.TargetID, lease.ExecutionKey)
	if err != nil {
		return base.RegistryGCSweepReceipt{}, false, err
	}
	if found && (receipt.PlanID != lease.PlanID || receipt.CandidateSetDigest != lease.CandidateSetDigest) {
		return base.RegistryGCSweepReceipt{}, false, base.ErrConflict
	}
	return receipt, found, tx.Commit(ctx)
}

func registryGCReceipt(ctx context.Context, q registryDB, targetID, executionKey string) (base.RegistryGCSweepReceipt, bool, error) {
	var receipt base.RegistryGCSweepReceipt
	err := q.QueryRow(ctx, `SELECT registry_target_id::text,execution_key,plan_id::text,candidate_set_digest,
		checkpoint_revision,provider_sweep_id,helper_job_uid,started_at,completed_at
		FROM registry_runtime_gc_sweep_receipts WHERE registry_target_id=$1 AND execution_key=$2`, targetID, executionKey).
		Scan(&receipt.TargetID, &receipt.ExecutionKey, &receipt.PlanID, &receipt.CandidateSetDigest,
			&receipt.CheckpointRevision, &receipt.ProviderSweepID, &receipt.HelperJobUID, &receipt.StartedAt, &receipt.CompletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return base.RegistryGCSweepReceipt{}, false, nil
	}
	return receipt, err == nil, classify(err)
}

func (s *Store) CompleteRegistryGCSweep(ctx context.Context, lease base.RegistryMaintenanceLease, receipt base.RegistryGCSweepReceipt, now time.Time) error {
	if receipt.TargetID != lease.TargetID || receipt.ExecutionKey != lease.ExecutionKey || receipt.PlanID != lease.PlanID ||
		receipt.CandidateSetDigest != lease.CandidateSetDigest || receipt.CheckpointRevision == "" || receipt.ProviderSweepID == "" ||
		receipt.HelperJobUID == "" || receipt.StartedAt.IsZero() || receipt.CompletedAt.Before(receipt.StartedAt) || now.Before(receipt.CompletedAt) {
		return base.ErrRegistryPolicyInvalid
	}
	now = databaseTime(now)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	current, err := requireRegistryMaintenanceLease(ctx, tx, lease, now)
	if err != nil {
		return err
	}
	if current.State != "sweeping" || current.SweepJobUID == "" || current.CheckpointRevision != receipt.CheckpointRevision {
		return base.ErrConflict
	}
	result, err := tx.Exec(ctx, `INSERT INTO registry_runtime_gc_sweep_receipts(
		registry_target_id,execution_key,plan_id,candidate_set_digest,checkpoint_revision,
		provider_sweep_id,helper_job_uid,started_at,completed_at,created_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT DO NOTHING`, receipt.TargetID,
		receipt.ExecutionKey, receipt.PlanID, receipt.CandidateSetDigest, receipt.CheckpointRevision,
		receipt.ProviderSweepID, receipt.HelperJobUID, databaseTime(receipt.StartedAt), databaseTime(receipt.CompletedAt), now)
	if err != nil {
		return classify(err)
	}
	if result.RowsAffected() == 0 {
		existing, found, loadErr := registryGCReceipt(ctx, tx, receipt.TargetID, receipt.ExecutionKey)
		if loadErr != nil || !found || existing != receipt {
			if loadErr != nil {
				return loadErr
			}
			return base.ErrConflict
		}
	}
	_, err = tx.Exec(ctx, `UPDATE registry_runtime_maintenance_executions SET state='swept',updated_at=$6
		WHERE registry_target_id=$1 AND execution_key=$2 AND plan_id=$3 AND lease_owner=$4 AND lease_epoch=$5`,
		lease.TargetID, lease.ExecutionKey, lease.PlanID, lease.Owner, lease.Epoch, now)
	if err != nil {
		return classify(err)
	}
	return classify(tx.Commit(ctx))
}

func (s *Store) MarkRegistryMaintenanceRestored(ctx context.Context, lease base.RegistryMaintenanceLease, now time.Time) error {
	if now.IsZero() {
		return base.ErrRegistryPolicyInvalid
	}
	now = databaseTime(now)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	current, err := requireRegistryMaintenanceLease(ctx, tx, lease, now)
	if err != nil {
		return err
	}
	if current.State == "restored" {
		return tx.Commit(ctx)
	}
	if current.State != "entered" && current.State != "checkpointed" && current.State != "sweeping" && current.State != "swept" && current.State != "failed" {
		return base.ErrConflict
	}
	_, err = tx.Exec(ctx, `UPDATE registry_runtime_maintenance_executions SET state='restored',restored_at=$6,updated_at=$6
		WHERE registry_target_id=$1 AND execution_key=$2 AND plan_id=$3 AND lease_owner=$4 AND lease_epoch=$5`,
		lease.TargetID, lease.ExecutionKey, lease.PlanID, lease.Owner, lease.Epoch, now)
	if err != nil {
		return classify(err)
	}
	return classify(tx.Commit(ctx))
}

func (s *Store) ReleaseRegistryMaintenance(ctx context.Context, lease base.RegistryMaintenanceLease, now time.Time) error {
	if now.IsZero() {
		return base.ErrRegistryPolicyInvalid
	}
	now = databaseTime(now)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	current, found, err := registryMaintenanceLease(ctx, tx, lease.TargetID, lease.ExecutionKey, true)
	if err != nil {
		return err
	}
	if found && !current.ReleasedAt.IsZero() && current.Owner == lease.Owner && current.Epoch == lease.Epoch {
		return tx.Commit(ctx)
	}
	current, err = requireRegistryMaintenanceLease(ctx, tx, lease, now)
	if err != nil {
		return err
	}
	if current.State == "acquired" {
		_, err = tx.Exec(ctx, `UPDATE registry_runtime_maintenance_executions SET state='released',restored_at=$6,released_at=$6,updated_at=$6
			WHERE registry_target_id=$1 AND execution_key=$2 AND plan_id=$3 AND lease_owner=$4 AND lease_epoch=$5`,
			lease.TargetID, lease.ExecutionKey, lease.PlanID, lease.Owner, lease.Epoch, now)
	} else if current.State == "restored" {
		_, err = tx.Exec(ctx, `UPDATE registry_runtime_maintenance_executions SET state='released',released_at=$6,updated_at=$6
			WHERE registry_target_id=$1 AND execution_key=$2 AND plan_id=$3 AND lease_owner=$4 AND lease_epoch=$5`,
			lease.TargetID, lease.ExecutionKey, lease.PlanID, lease.Owner, lease.Epoch, now)
	} else {
		return base.ErrConflict
	}
	if err != nil {
		return classify(err)
	}
	return classify(tx.Commit(ctx))
}

var _ base.RegistryRuntimeStore = (*Store)(nil)
