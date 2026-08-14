package postgres

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/id"
)

func TestPostgreSQLFailOperationRecordsNonUpgradeFailure(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := t.Context()
	store, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	operationID, targetID := id.New(), id.New()
	worker := "failure-recorder-" + operationID[:8]
	now := databaseTime(time.Now().UTC())
	if _, err = store.pool.Exec(ctx, `INSERT INTO operations(
		id,kind,status,target_type,target_id,request_id,generation,progress,created_at,updated_at)
		VALUES($1,'qualification.failure-record','queued','qualification',$2,$3,1,
		'[{"name":"git-write","status":"pending"}]'::jsonb,$4,$4)`,
		operationID, targetID, "failure-record-"+operationID, now); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, cleanupErr := store.pool.Exec(cleanupCtx, `DELETE FROM operations WHERE id=$1`, operationID); cleanupErr != nil {
			t.Errorf("delete failure-record operation: %v", cleanupErr)
		}
	}()

	started, execute, err := store.StartOperation(ctx, operationID, 1, worker, time.Minute)
	if err != nil || !execute || started.Status != "running" {
		t.Fatalf("start operation: operation=%#v execute=%t err=%v", started, execute, err)
	}
	if err = store.FailOperation(ctx, operationID, 1, worker, "GitWriteFailed", "compare-and-swap conflict"); err != nil {
		t.Fatalf("record failure: %v", err)
	}
	failed, err := store.GetOperation(ctx, operationID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != "failed" || failed.Problem == nil || failed.Problem.Code != "GitWriteFailed" ||
		failed.Problem.Detail != "compare-and-swap conflict" || failed.FinishedAt == nil || len(failed.Progress) != 1 ||
		failed.Progress[0].Name != "git-write" || failed.Progress[0].Status != "failed" {
		t.Fatalf("failed operation was not recorded exactly: %#v", failed)
	}
}

func TestPostgreSQLRequeueUpgradeRecordsTypedReason(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := t.Context()
	store, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	operationID, upgradeID := id.New(), id.New()
	worker := "upgrade-requeue-" + operationID[:8]
	now := databaseTime(time.Now().UTC())
	if _, err = store.pool.Exec(ctx, `INSERT INTO operations(
		id,kind,status,target_type,target_id,request_id,generation,progress,lease_owner,lease_until,created_at,updated_at)
		VALUES($1,'platform.upgrade','running','platform-upgrade',$2,$3,1,
		'[{"name":"upgrade","status":"running"}]'::jsonb,$4,$5,$6,$6)`,
		operationID, upgradeID, "upgrade-requeue-"+operationID, worker, now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO platform_upgrades(
		id,version,manifest_digest,manifest,manifest_bytes,state,operation_id,result,created_at,updated_at)
		VALUES($1,'v0.0.0-test',$2,'{}'::jsonb,'{}'::bytea,'running',$3,
		'{"action":"upgrade"}'::jsonb,$4,$4)`,
		upgradeID, "sha256:"+strings.Repeat("0", 64), operationID, now); err != nil {
		_, _ = store.pool.Exec(context.Background(), `DELETE FROM operations WHERE id=$1`, operationID)
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, cleanupErr := store.pool.Exec(cleanupCtx, `DELETE FROM platform_upgrades WHERE id=$1`, upgradeID); cleanupErr != nil {
			t.Errorf("delete upgrade-requeue platform row: %v", cleanupErr)
		}
		if _, cleanupErr := store.pool.Exec(cleanupCtx, `DELETE FROM operations WHERE id=$1`, operationID); cleanupErr != nil {
			t.Errorf("delete upgrade-requeue operation: %v", cleanupErr)
		}
	}()

	if err = store.RequeueOperation(ctx, operationID, 1, worker, "UpgradePending", "runner still active"); err != nil {
		t.Fatalf("requeue upgrade: %v", err)
	}
	requeued, err := store.GetOperation(ctx, operationID)
	if err != nil {
		t.Fatal(err)
	}
	var state, code, detail string
	if err = store.pool.QueryRow(ctx, `SELECT state,result->>'code',result->>'detail' FROM platform_upgrades WHERE id=$1`, upgradeID).
		Scan(&state, &code, &detail); err != nil {
		t.Fatal(err)
	}
	if requeued.Status != "queued" || state != "queued" || code != "UpgradePending" || detail != "runner still active" {
		t.Fatalf("upgrade reason was not recorded exactly: operation=%#v state=%q code=%q detail=%q", requeued, state, code, detail)
	}
}
