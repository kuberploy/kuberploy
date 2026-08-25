package helmdirect

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/testdb"
)

func TestPostgresStoreDirectArgoLifecycle(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = testdb.ApplyMigrations(ctx, pool); err != nil {
		t.Fatal(err)
	}

	const (
		actorID       = "71000000-0000-4000-8000-000000000001"
		projectID     = "71000000-0000-4000-8000-000000000002"
		environmentID = "71000000-0000-4000-8000-000000000003"
		applicationID = "71000000-0000-4000-8000-000000000004"
	)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id=$1`, projectID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, actorID)
	})
	if _, err = pool.Exec(ctx, `INSERT INTO users(id,display_name,role,issuer,subject,created_at)
		VALUES($1,'Helm actor','platform-admin','test','helm-direct',$2)`, actorID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO projects(id,name,slug,created_at) VALUES($1,'Helm project','helm-direct-project',$2)`, projectID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at)
		VALUES($1,$2,'Production','production','kp-helm-production','kp-helm-production',$3)`, environmentID, projectID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO applications(id,project_id,name,slug,source_kind,created_at)
		VALUES($1,$2,'Valkey','valkey','helm',$3)`, applicationID, projectID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO environment_app_placements(project_id,environment_id,application_id,created_at,updated_at)
		VALUES($1,$2,$3,$4,$4)`, projectID, environmentID, applicationID, now); err != nil {
		t.Fatal(err)
	}

	store, err := NewPostgresStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	target := Target{ProjectID: projectID, EnvironmentID: environmentID, ApplicationID: applicationID}
	actor := func(key string) Actor { return Actor{ID: actorID, IdempotencyKey: key, RequestID: key} }
	firstRequest := DeployRequest{Target: target, Actor: actor("helm-direct-deploy-0001"),
		Source: Source{Kind: SourceGit, RepositoryURL: "https://github.com/valkey-io/valkey-helm.git", Path: "valkey", TargetRevision: "main"},
		Values: []byte("replicaCount: 1\n")}
	first, replay, err := store.Deploy(ctx, firstRequest, now)
	if err != nil || replay || first.Generation != 1 || first.State != StatePending {
		t.Fatalf("first=%#v replay=%t err=%v", first, replay, err)
	}
	replayed, replay, err := store.Deploy(ctx, firstRequest, now.Add(time.Second))
	if err != nil || !replay || replayed.ID != first.ID {
		t.Fatalf("replayed=%#v replay=%t err=%v", replayed, replay, err)
	}
	changed := firstRequest
	changed.Values = []byte("replicaCount: 2\n")
	if _, _, err = store.Deploy(ctx, changed, now.Add(2*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("idempotency key accepted changed values: %v", err)
	}
	if err = store.MarkApplied(ctx, first.ID, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}

	secondRequest := firstRequest
	secondRequest.Actor = actor("helm-direct-deploy-0002")
	secondRequest.Source = Source{Kind: SourceHelmRepository, RepositoryURL: "https://charts.example.test", Chart: "valkey", TargetRevision: "0.11.0"}
	secondRequest.Values = nil
	second, replay, err := store.Deploy(ctx, secondRequest, now.Add(4*time.Second))
	if err != nil || replay || second.Generation != 2 || string(second.ValuesYAML) != "{}\n" || second.ParentRevisionID != first.ID {
		t.Fatalf("second=%#v replay=%t err=%v", second, replay, err)
	}
	if err = store.MarkApplied(ctx, second.ID, now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}

	rollback, replay, err := store.Rollback(ctx, MutationRequest{Target: target, Actor: actor("helm-direct-rollback-0001"), RollbackSourceID: first.ID}, now.Add(6*time.Second))
	if err != nil || replay || rollback.Generation != 3 || rollback.Source.Kind != SourceGit || rollback.RollbackSourceRevisionID != first.ID {
		t.Fatalf("rollback=%#v replay=%t err=%v", rollback, replay, err)
	}
	if err = store.MarkApplied(ctx, rollback.ID, now.Add(7*time.Second)); err != nil {
		t.Fatal(err)
	}
	disabled, replay, err := store.Disable(ctx, MutationRequest{Target: target, Actor: actor("helm-direct-disable-0001")}, now.Add(8*time.Second))
	if err != nil || replay || disabled.Generation != 4 || disabled.DesiredEnabled {
		t.Fatalf("disabled=%#v replay=%t err=%v", disabled, replay, err)
	}
	if err = store.MarkApplied(ctx, disabled.ID, now.Add(9*time.Second)); err != nil {
		t.Fatal(err)
	}

	head, err := store.Head(ctx, target)
	history, historyErr := store.History(ctx, target, 10)
	if err != nil || historyErr != nil || head.ID != disabled.ID || len(history) != 4 || history[0].Generation != 4 || history[3].Generation != 1 {
		t.Fatalf("head=%#v history=%#v err=%v historyErr=%v", head, history, err, historyErr)
	}
	var state, desired string
	if err = pool.QueryRow(ctx, `SELECT state,desired_state FROM environment_app_placements
		WHERE environment_id=$1 AND application_id=$2`, environmentID, applicationID).Scan(&state, &desired); err != nil || state != "draft" || desired != "stopped" {
		t.Fatalf("placement state=%q desired=%q err=%v", state, desired, err)
	}
}
