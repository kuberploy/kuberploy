package postgres

import (
	"context"
	"os"
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
