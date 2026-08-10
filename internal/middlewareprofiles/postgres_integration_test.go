package middlewareprofiles_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/id"
	profiles "github.com/kuberploy/kuberploy/internal/middlewareprofiles"
	storepostgres "github.com/kuberploy/kuberploy/internal/store/postgres"
)

func TestPostgresMiddlewareProfileLifecycleAndReferenceFences(t *testing.T) {
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
	if err = storepostgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err = storepostgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("idempotent migration rerun: %v", err)
	}
	actor, team, project, environment, application, binding := id.New(), id.New(), id.New(), id.New(), id.New(), id.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err = pool.Exec(ctx, `INSERT INTO users(id,login,role,issuer,subject) VALUES($1,'middleware-admin','platform-admin','test',$2)`, actor, actor); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO teams(id,name,slug,created_by) VALUES($1,'Middleware team',$2,$3)`, team, "middleware-"+team[:8], actor); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO projects(id,name,slug,team_id) VALUES($1,'Middleware project',$2,$3)`, project, "middleware-"+project[:8], team); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO environments(id,project_id,name,slug,namespace,argo_project) VALUES($1,$2,'Production','production',$3,'middleware')`, environment, project, "middleware-"+environment[:8]); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO applications(id,project_id,name,slug) VALUES($1,$2,'API','api')`, application, project); err != nil {
		t.Fatal(err)
	}
	prefix := "tenants/" + project + "/environments/" + environment
	if _, err = pool.Exec(ctx, `INSERT INTO git_repository_bindings(id,kind,scope_id,project_id,environment_id,provider,installation_id,repository_id,repository_owner,repository_name,target_ref,path_prefix,credential_secret_name,state,target_head_revision,indexed_revision,projection_generation,parser_version,target_head_observed_at,indexed_at,created_at,updated_at)
		VALUES($1,'environment',$2,$3,$2,'github',91,92,'kuberploy','profiles','refs/heads/main',$4,'git-credentials','ready',$5,$5,1,'appconfig-v1',$6,$6,$6,$6)`, binding, environment, project, prefix, strings.Repeat("a", 40), now); err != nil {
		t.Fatal(err)
	}
	store, _ := profiles.NewPostgresStore(pool)
	spec := profiles.Spec{"rateLimit": map[string]any{"average": float64(100), "burst": float64(200)}}
	created, err := store.Create(ctx, pgCommand(actor, "pg-create-profile", now), "api-limit", spec, []profiles.Assignment{{Scope: profiles.ProjectScope, ID: project}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Create(ctx, pgCommand(actor, "pg-create-same-name", now.Add(time.Second)), "api-limit", spec, []profiles.Assignment{{Scope: profiles.ApplicationScope, ID: application}}); err != nil {
		t.Fatalf("same logical name in another scope should be allowed: %v", err)
	}
	_, err = pool.Exec(ctx, `UPDATE middleware_profiles SET lifecycle='deactivated',current_revision=current_revision+1,deactivated_by=$2,deactivated_at=$3 WHERE id=$1`, created.Profile.ID, actor, now)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("deactivation advanced revision: %v", err)
	}
	ref := profiles.Ref{ProfileID: created.Profile.ID, Revision: 1, SpecDigest: created.Revision.SpecDigest, AssignmentsDigest: created.Revision.AssignmentsDigest}
	path := prefix + "/apps/" + application + "/app.yaml"
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	matched, err := store.ValidateMaterializedTx(ctx, tx, []profiles.MaterializedDefinition{{Name: "api-limit", ProfileRef: &ref, Spec: spec}}, profiles.Target{ProjectID: project, EnvironmentID: environment, ApplicationID: application}, application, environment, path, now)
	if err != nil || !matched {
		_ = tx.Rollback(ctx)
		t.Fatalf("exact materialization=%t err=%v", matched, err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	badTx, _ := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	matched, err = store.ValidateMaterializedTx(ctx, badTx, []profiles.MaterializedDefinition{{Name: "api-limit", ProfileRef: &ref, Spec: spec}}, profiles.Target{ProjectID: project, EnvironmentID: environment, ApplicationID: application}, application, environment, "tenants/wrong/apps/"+application+"/app.yaml", now)
	_ = badTx.Rollback(ctx)
	if matched || !errors.Is(err, profiles.ErrInvalid) {
		t.Fatalf("mismatched Git destination persisted matched=%t err=%v", matched, err)
	}
	if _, err = store.Deactivate(ctx, pgCommand(actor, "pg-deactivate-referenced", now.Add(2*time.Second)), profiles.Ref{ProfileID: created.Profile.ID, Revision: 1}); !errors.Is(err, profiles.ErrReferenced) {
		t.Fatalf("referenced deactivate: %v", err)
	}
	deleteTx, _ := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err = store.ReconcileDeletedTx(ctx, deleteTx, path); err != nil {
		t.Fatal(err)
	}
	if err = deleteTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Deactivate(ctx, pgCommand(actor, "pg-deactivate-unreferenced", now.Add(3*time.Second)), profiles.Ref{ProfileID: created.Profile.ID, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	if got := storepostgresSchema(t, ctx, pool); got != "035_middleware_profiles.sql" {
		t.Fatalf("current schema=%q", got)
	}
}

func pgCommand(actor, key string, now time.Time) profiles.Command {
	return profiles.Command{ActorID: actor, IdempotencyKey: key, RequestID: "request-" + key, Now: now}
}

func storepostgresSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var value string
	if err := pool.QueryRow(ctx, `SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}
