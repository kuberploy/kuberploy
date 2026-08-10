package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/id"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func (s *Store) CreatePlatformUpgrade(ctx context.Context, actor, key, fingerprint, requestID string, in domain.CreatePlatformUpgrade) (base.Result[domain.PlatformUpgrade], domain.Operation, error) {
	if !base.ExactSHA256Matches(in.Release.ManifestBytes, in.Release.ManifestDigest) {
		return base.Result[domain.PlatformUpgrade]{}, domain.Operation{}, base.ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return base.Result[domain.PlatformUpgrade]{}, domain.Operation{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('kuberploy-platform-upgrade'))`); err != nil {
		return base.Result[domain.PlatformUpgrade]{}, domain.Operation{}, err
	}
	if old, ok, err := findIdem(ctx, tx, actor, "platform-upgrades.create", key); err != nil {
		return base.Result[domain.PlatformUpgrade]{}, domain.Operation{}, err
	} else if ok {
		if old.fingerprint != fingerprint {
			return base.Result[domain.PlatformUpgrade]{}, domain.Operation{}, base.ErrIdempotencyConflict
		}
		u, e := getPlatformUpgrade(ctx, tx, old.resourceID)
		if e != nil {
			return base.Result[domain.PlatformUpgrade]{}, domain.Operation{}, e
		}
		opID := u.OperationID
		if old.operationID != nil {
			opID = *old.operationID
		}
		op, e := getOperation(ctx, tx, opID)
		return base.Result[domain.PlatformUpgrade]{Value: u, Replay: true}, op, e
	}
	var active bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM platform_upgrades WHERE state IN ('queued','running'))`).Scan(&active); err != nil {
		return base.Result[domain.PlatformUpgrade]{}, domain.Operation{}, err
	}
	if active {
		return base.Result[domain.PlatformUpgrade]{}, domain.Operation{}, base.ErrUpgradeInProgress
	}
	now := time.Now().UTC()
	upgradeID, opID := id.New(), id.New()
	manifestJSON, err := json.Marshal(in.Release.Manifest)
	if err != nil {
		return base.Result[domain.PlatformUpgrade]{}, domain.Operation{}, err
	}
	progress, _ := json.Marshal([]domain.ProgressStep{{Name: "upgrade", Status: "pending"}})
	op := domain.Operation{ID: opID, Kind: "platform.upgrade", Status: "queued", TargetType: "platform-upgrade", TargetID: upgradeID, RequestID: requestID, Generation: 1, Progress: []domain.ProgressStep{{Name: "upgrade", Status: "pending"}}, CreatedAt: now, UpdatedAt: now}
	if _, err = tx.Exec(ctx, `INSERT INTO operations(id,kind,status,target_type,target_id,request_id,generation,progress,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$9)`, op.ID, op.Kind, op.Status, op.TargetType, op.TargetID, op.RequestID, op.Generation, progress, now); err != nil {
		return base.Result[domain.PlatformUpgrade]{}, domain.Operation{}, classify(err)
	}
	u := domain.PlatformUpgrade{ID: upgradeID, Version: in.Release.Version, ManifestDigest: in.Release.ManifestDigest, Manifest: in.Release.Manifest, ManifestBytes: append([]byte(nil), in.Release.ManifestBytes...), State: "queued", OperationID: opID, CreatedAt: now, UpdatedAt: now}
	if _, err = tx.Exec(ctx, `INSERT INTO platform_upgrades(id,version,manifest_digest,manifest,manifest_bytes,state,operation_id,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$8)`, u.ID, u.Version, u.ManifestDigest, manifestJSON, u.ManifestBytes, u.State, u.OperationID, now); err != nil {
		if errors.Is(classify(err), base.ErrConflict) {
			return base.Result[domain.PlatformUpgrade]{}, domain.Operation{}, base.ErrUpgradeInProgress
		}
		return base.Result[domain.PlatformUpgrade]{}, domain.Operation{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO outbox(operation_id,kind,scope_id,generation,trace_id) VALUES($1,$2,$3,$4,$5)`, op.ID, op.Kind, u.ID, op.Generation, requestID); err != nil {
		return base.Result[domain.PlatformUpgrade]{}, domain.Operation{}, err
	}
	if err = audit(ctx, tx, actor, "platform.upgrade.accepted", "platform-upgrade", u.ID, requestID, map[string]any{"operationId": op.ID, "targetVersion": u.Version, "manifestDigest": u.ManifestDigest}); err != nil {
		return base.Result[domain.PlatformUpgrade]{}, domain.Operation{}, err
	}
	if err = putIdem(ctx, tx, actor, "platform-upgrades.create", key, fingerprint, "platform-upgrade", u.ID, &opID); err != nil {
		return base.Result[domain.PlatformUpgrade]{}, domain.Operation{}, classify(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return base.Result[domain.PlatformUpgrade]{}, domain.Operation{}, err
	}
	return base.Result[domain.PlatformUpgrade]{Value: u}, op, nil
}

func getPlatformUpgrade(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id string) (domain.PlatformUpgrade, error) {
	var u domain.PlatformUpgrade
	var manifest, result []byte
	err := q.QueryRow(ctx, `SELECT id,version,manifest_digest,manifest,manifest_bytes,state,operation_id,runner_ref,result,created_at,updated_at FROM platform_upgrades WHERE id=$1`, id).Scan(&u.ID, &u.Version, &u.ManifestDigest, &manifest, &u.ManifestBytes, &u.State, &u.OperationID, &u.RunnerRef, &result, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return u, classify(err)
	}
	if err = json.Unmarshal(manifest, &u.Manifest); err != nil {
		return u, err
	}
	if len(result) > 0 {
		if err = json.Unmarshal(result, &u.Result); err != nil {
			return u, err
		}
	}
	return u, nil
}
func (s *Store) GetPlatformUpgrade(ctx context.Context, id string) (domain.PlatformUpgrade, error) {
	return getPlatformUpgrade(ctx, s.pool, id)
}

func (s *Store) RecordUpgradeRunner(ctx context.Context, operationID string, generation int64, worker, runnerRef string) error {
	if worker == "" || runnerRef == "" {
		return base.ErrConflict
	}
	tag, err := s.pool.Exec(ctx, `UPDATE platform_upgrades u SET runner_ref=$4,updated_at=now() FROM operations o WHERE u.operation_id=o.id AND o.id=$1 AND o.generation=$2 AND o.kind='platform.upgrade' AND o.status='running' AND o.lease_owner=$3 AND o.lease_until>now() AND (u.runner_ref='' OR u.runner_ref=$4)`, operationID, generation, worker, runnerRef)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: upgrade operation changed or runner identity differs", base.ErrConflict)
	}
	return nil
}

func (s *Store) RequeueOperation(ctx context.Context, operationID string, generation int64, worker, code, detail string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	now := time.Now().UTC()
	var kind, status, owner string
	var gen int64
	var leaseUntil *time.Time
	if err = tx.QueryRow(ctx, `SELECT kind,status,generation,COALESCE(lease_owner,''),lease_until FROM operations WHERE id=$1 FOR UPDATE`, operationID).Scan(&kind, &status, &gen, &owner, &leaseUntil); err != nil {
		return classify(err)
	}
	if gen != generation || kind != "platform.upgrade" && kind != "deployment.git-write" {
		return fmt.Errorf("%w: operation generation or kind changed", base.ErrConflict)
	}
	// Completion can commit while its response is lost. A reconcile-pending
	// fallback must be a no-op in that case, never downgrade success.
	if status == "succeeded" || status == "queued" {
		return nil
	}
	if !validOperationLease(status, owner, leaseUntil, worker, now) {
		return base.ErrOperationLeaseLost
	}
	step := "git-write"
	if kind == "platform.upgrade" {
		step = "upgrade"
	}
	progress, _ := json.Marshal([]domain.ProgressStep{{Name: step, Status: "pending", Detail: detail}})
	if _, err = tx.Exec(ctx, `UPDATE operations SET status='queued',problem=NULL,progress=$2,lease_owner=NULL,lease_until=NULL,updated_at=$3,finished_at=NULL WHERE id=$1`, operationID, progress, now); err != nil {
		return err
	}
	if kind == "platform.upgrade" {
		upgradeTag, updateErr := tx.Exec(ctx, `UPDATE platform_upgrades SET state='queued',result=jsonb_build_object('code',$2,'detail',$3),updated_at=$4 WHERE operation_id=$1 AND state IN ('queued','running')`, operationID, code, detail, now)
		if updateErr != nil {
			return updateErr
		}
		if upgradeTag.RowsAffected() == 0 {
			return fmt.Errorf("%w: platform upgrade state changed", base.ErrConflict)
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) CompleteUpgradeOperation(ctx context.Context, operationID string, generation int64, worker, runnerRef string, result map[string]any) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var targetID, actorID, requestID, status, owner, kind string
	var gen int64
	var leaseUntil *time.Time
	if err = tx.QueryRow(ctx, `SELECT target_id,status,generation,request_id,kind,COALESCE(lease_owner,''),lease_until FROM operations WHERE id=$1 FOR UPDATE`, operationID).Scan(&targetID, &status, &gen, &requestID, &kind, &owner, &leaseUntil); err != nil {
		return classify(err)
	}
	if status == "succeeded" {
		return nil
	}
	if gen != generation || kind != "platform.upgrade" {
		return fmt.Errorf("%w: operation generation changed", base.ErrConflict)
	}
	now := time.Now().UTC()
	if !validOperationLease(status, owner, leaseUntil, worker, now) {
		return base.ErrOperationLeaseLost
	}
	progress, _ := json.Marshal([]domain.ProgressStep{{Name: "upgrade", Status: "succeeded", Detail: "runner completed: " + runnerRef, FinishedAt: &now}})
	resultJSON, _ := json.Marshal(result)
	if _, err = tx.Exec(ctx, `UPDATE operations SET status='succeeded',progress=$2,lease_owner=NULL,lease_until=NULL,updated_at=$3,finished_at=$3 WHERE id=$1`, operationID, progress, now); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE platform_upgrades SET state='succeeded',runner_ref=$2,result=$3,updated_at=$4 WHERE id=$1`, targetID, runnerRef, resultJSON, now); err != nil {
		return err
	}
	err = tx.QueryRow(ctx, `SELECT actor_id FROM idempotency_keys WHERE operation_id=$1 LIMIT 1`, operationID).Scan(&actorID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if actorID != "" {
		if err = audit(ctx, tx, actorID, "platform.upgrade.succeeded", "platform-upgrade", targetID, requestID, map[string]any{"operationId": operationID, "runnerRef": runnerRef}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
