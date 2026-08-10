package postgres

import (
	"os"
	"testing"
)

func TestOutboxDatasetReconciliationReplaysOnlyNonTerminalWork(t *testing.T) {
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
	queuedID := "74444444-4444-4444-8444-444444444441"
	terminalID := "74444444-4444-4444-8444-444444444442"
	t.Cleanup(func() {
		_, _ = store.pool.Exec(ctx, `DELETE FROM operations WHERE id=ANY($1::uuid[])`, []string{queuedID, terminalID})
		_, _ = store.pool.Exec(ctx, `DELETE FROM outbox_valkey_dataset WHERE singleton=true`)
	})
	if _, err = store.pool.Exec(ctx, `DELETE FROM operations WHERE id=ANY($1::uuid[])`, []string{queuedID, terminalID}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `DELETE FROM outbox_valkey_dataset WHERE singleton=true`); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO operations(id,kind,status,target_type,target_id,request_id,generation,finished_at)
		VALUES($1,'deployment.git-write','queued','deployment',$1,'dataset-replay-queued',1,NULL),
		($2,'deployment.git-write','succeeded','deployment',$2,'dataset-replay-terminal',1,now())`, queuedID, terminalID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO outbox(operation_id,kind,scope_id,generation,trace_id,published_at)
		VALUES($1,'deployment.git-write',$1,1,'dataset-replay-queued',now()),
		($2,'deployment.git-write',$2,1,'dataset-replay-terminal',now())`, queuedID, terminalID); err != nil {
		t.Fatal(err)
	}
	firstDataset := "11111111-1111-4111-8111-111111111111"
	if replayed, reconcileErr := store.ReconcileOutboxDataset(ctx, firstDataset); reconcileErr != nil || replayed != 1 {
		t.Fatalf("initial replayed=%d err=%v", replayed, reconcileErr)
	}
	pending, err := store.PendingOutbox(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].OperationID != queuedID {
		t.Fatalf("pending=%#v err=%v", pending, err)
	}
	if err = store.MarkOutboxPublished(ctx, queuedID); err != nil {
		t.Fatal(err)
	}
	if replayed, reconcileErr := store.ReconcileOutboxDataset(ctx, firstDataset); reconcileErr != nil || replayed != 0 {
		t.Fatalf("stable replayed=%d err=%v", replayed, reconcileErr)
	}
	pending, err = store.PendingOutbox(ctx, 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("stable pending=%#v err=%v", pending, err)
	}
	secondDataset := "22222222-2222-4222-8222-222222222222"
	if replayed, reconcileErr := store.ReconcileOutboxDataset(ctx, secondDataset); reconcileErr != nil || replayed != 1 {
		t.Fatalf("replacement replayed=%d err=%v", replayed, reconcileErr)
	}
	if _, err = store.pool.Exec(ctx, `UPDATE operations SET status='succeeded',finished_at=now() WHERE id=$1`, queuedID); err != nil {
		t.Fatal(err)
	}
	if err = store.MarkOutboxPublished(ctx, queuedID); err != nil {
		t.Fatal(err)
	}
	if replayed, reconcileErr := store.ReconcileOutboxDataset(ctx, "33333333-3333-4333-8333-333333333333"); reconcileErr != nil || replayed != 0 {
		t.Fatalf("terminal replacement replayed=%d err=%v", replayed, reconcileErr)
	}
	if _, reconcileErr := store.ReconcileOutboxDataset(ctx, "not-a-dataset-id"); reconcileErr == nil {
		t.Fatal("invalid dataset identity was accepted")
	}
}
