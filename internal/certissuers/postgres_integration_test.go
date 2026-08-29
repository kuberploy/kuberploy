package certissuers

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/testdb"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/id"
	storepostgres "github.com/kuberploy/kuberploy/internal/store/postgres"
)

func TestPostgresAdminFenceImmutabilityAndReadyCatalog(t *testing.T) {
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
	if err = testdb.ApplyMigrations(ctx, pool); err != nil {
		t.Fatalf("idempotent migration rerun: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	admin, tenant := id.New(), id.New()
	if _, err = pool.Exec(ctx, `INSERT INTO users(id,display_name,role,issuer,subject) VALUES($1,$2,'platform-admin','test',$2),($3,$4,'developer','test',$4)`, admin, "issuer-admin-"+admin[:8], tenant, "issuer-tenant-"+tenant[:8]); err != nil {
		t.Fatal(err)
	}
	store, _ := NewPostgresStore(pool)
	if _, err = store.Create(ctx, Command{ActorID: tenant, IdempotencyKey: "tenant-cannot-create", RequestID: "request-tenant-cannot-create", Now: now}, "tenant-issuer-"+tenant[:8], httpSpec()); !errors.Is(err, ErrConflict) {
		t.Fatalf("tenant mutation err=%v", err)
	}
	created, err := store.Create(ctx, Command{ActorID: admin, IdempotencyKey: "admin-create-" + admin, RequestID: "request-admin-create", Now: now}, "issuer-"+admin[:8], dnsSpec("example.com"))
	if err != nil {
		t.Fatal(err)
	}
	schedulingProfile := id.New()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO configuration_profiles(id,kind,name,lifecycle,current_revision,created_by,created_at)
		VALUES($1,'scheduling',$2,'active',1,$3,$4)`, schedulingProfile, "not-an-issuer-"+schedulingProfile[:8], admin, now); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO configuration_profile_revisions(profile_id,revision,profile_kind,spec,spec_digest,assignments_digest,created_by,created_at)
		VALUES($1,1,'scheduling','{}'::jsonb,$2,$3,$4,$5)`, schedulingProfile, "sha256:"+strings.Repeat("1", 64), "sha256:"+strings.Repeat("2", 64), admin, now); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO cert_manager_issuer_observations(profile_id,revision,state,updated_at)
		VALUES($1,1,'pending',$2)`, schedulingProfile, now); !isPGCode(err, "23503") {
		t.Fatalf("cross-kind issuer observation err=%v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE configuration_profile_revisions SET spec_digest=$3 WHERE profile_id=$1 AND revision=$2 AND profile_kind='certificate-issuer'`, created.Profile.ID, 1, "sha256:"+strings.Repeat("0", 64)); !isPGCode(err, "23514") {
		t.Fatalf("immutable revision update err=%v", err)
	}
	observed := now.Add(time.Second)
	if err = store.RecordObservation(ctx, Observation{ProfileID: created.Profile.ID, Revision: 1, State: Ready, ObservedSpecDigest: created.Revision.SpecDigest, ObservedGeneration: 1, ObservedAt: &observed, UpdatedAt: observed}); err != nil {
		t.Fatal(err)
	}
	storedObservation, err := store.Observation(ctx, created.Profile.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if storedObservation.ObservedAt == nil || storedObservation.ObservedAt.Location() != time.UTC || storedObservation.UpdatedAt.Location() != time.UTC {
		t.Fatalf("PostgreSQL observation timestamps were not normalized to UTC: %#v", storedObservation)
	}
	identities, err := store.ReadyForHostname(ctx, "api.example.com", observed, 5*time.Minute, 20)
	if err != nil || len(identities) != 1 || identities[0].Name != created.Profile.Name {
		t.Fatalf("identities=%v err=%v", identities, err)
	}
	deactivateCommand := Command{ActorID: admin, IdempotencyKey: "admin-deactivate-" + admin, RequestID: "request-admin-deactivate", Now: now.Add(2 * time.Second)}
	deactivateRef := Ref{ProfileID: created.Profile.ID, Revision: 1}
	if _, err = store.Deactivate(ctx, deactivateCommand, deactivateRef); err != nil {
		t.Fatal(err)
	}
	if replay, found, replayErr := store.ReplayDeactivate(ctx, deactivateCommand, deactivateRef); replayErr != nil || !found || !replay.Replay || replay.Profile.Lifecycle != Deactivated {
		t.Fatalf("deactivation replay=%+v found=%t err=%v", replay, found, replayErr)
	}
	missCommand := deactivateCommand
	missCommand.IdempotencyKey = "admin-deactivate-miss-" + admin
	if replay, found, replayErr := store.ReplayDeactivate(ctx, missCommand, deactivateRef); replayErr != nil || found || replay.Profile.ID != "" || replay.Replay {
		t.Fatalf("deactivation replay miss=%+v found=%t err=%v", replay, found, replayErr)
	}
	if err = storepostgres.VerifySchema(ctx, pool); err != nil {
		t.Fatalf("verify Prisma migration history: %v", err)
	}
}

func TestPostgresReferencedIssuerCanBeRevisedButNotDeactivated(t *testing.T) {
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

	now := time.Now().UTC().Truncate(time.Microsecond)
	actor, team, project, environment, application, binding := id.New(), id.New(), id.New(), id.New(), id.New(), id.New()
	suffix := actor[:8]
	if _, err = pool.Exec(ctx, `INSERT INTO users(id,display_name,role,issuer,subject) VALUES($1,$2,'platform-admin','test',$3)`, actor, "issuer-revision-"+suffix, actor); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO teams(id,name,slug,created_by) VALUES($1,$2,$3,$4)`, team, "Issuer revision team "+suffix, "issuer-revision-"+suffix, actor); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO projects(id,name,slug,team_id) VALUES($1,$2,$3,$4)`, project, "Issuer revision project "+suffix, "issuer-revision-"+suffix, team); err != nil {
		t.Fatal(err)
	}
	namespace := "issuer-revision-" + environment[:8]
	if _, err = pool.Exec(ctx, `INSERT INTO environments(id,project_id,name,slug,namespace,argo_project) VALUES($1,$2,'Production','production',$3,$3)`, environment, project, namespace); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO applications(id,project_id,name,slug) VALUES($1,$2,'API','api')`, application, project); err != nil {
		t.Fatal(err)
	}
	prefix := "tenants/" + project + "/environments/" + environment
	if _, err = pool.Exec(ctx, `INSERT INTO git_repository_bindings(id,kind,scope_id,project_id,environment_id,provider,installation_id,repository_id,repository_owner,repository_name,target_ref,path_prefix,credential_secret_name,state,target_head_revision,indexed_revision,projection_generation,parser_version,target_head_observed_at,indexed_at,created_at,updated_at)
		VALUES($1,'environment',$2,$3,$2,'github',91,92,'kuberploy','issuer-revisions','refs/heads/main',$4,'git-credentials','ready',$5,$5,1,'appconfig-v1',$6,$6,$6,$6)`, binding, environment, project, prefix, strings.Repeat("a", 40), now); err != nil {
		t.Fatal(err)
	}

	store, err := NewPostgresStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	name := "issuer-" + suffix
	created, err := store.Create(ctx, Command{ActorID: actor, IdempotencyKey: "issuer-revision-create-" + actor, RequestID: "request-issuer-revision-create", Now: now}, name, dnsSpec("example.com"))
	if err != nil {
		t.Fatal(err)
	}
	observedAt := now.Add(time.Second)
	if err = store.RecordObservation(ctx, Observation{ProfileID: created.Profile.ID, Revision: 1, State: Ready, ObservedSpecDigest: created.Revision.SpecDigest, ObservedGeneration: 1, ObservedAt: &observedAt, UpdatedAt: observedAt}); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	path := prefix + "/apps/" + application + "/app.yaml"
	matched, err := store.ReconcileReferencesTx(ctx, tx, application, environment, path, []Selection{{Hostname: "api.example.com", IssuerName: name}}, observedAt, 5*time.Minute)
	if err != nil || !matched {
		_ = tx.Rollback(ctx)
		t.Fatalf("reference matched=%t err=%v", matched, err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	revisedSpec := dnsSpec("example.com")
	revisedSpec.ACME.Email = "revised@example.com"
	revised, err := store.Revise(ctx, Command{ActorID: actor, IdempotencyKey: "issuer-revision-revise-" + actor, RequestID: "request-issuer-revision-revise", Now: now.Add(2 * time.Second)}, Ref{ProfileID: created.Profile.ID, Revision: 1}, revisedSpec)
	if err != nil || revised.Profile.CurrentRevision != 2 || revised.Revision.Revision != 2 {
		t.Fatalf("revised=%+v err=%v", revised, err)
	}
	var retainedRevision int64
	if err = pool.QueryRow(ctx, `SELECT revision FROM cert_manager_issuer_references WHERE profile_id=$1 AND git_path=$2`, created.Profile.ID, path).Scan(&retainedRevision); err != nil || retainedRevision != 1 {
		t.Fatalf("retained revision=%d err=%v", retainedRevision, err)
	}
	_, err = store.Deactivate(ctx, Command{ActorID: actor, IdempotencyKey: "issuer-revision-deactivate-" + actor, RequestID: "request-issuer-revision-deactivate", Now: now.Add(3 * time.Second)}, Ref{ProfileID: created.Profile.ID, Revision: 2})
	if !errors.Is(err, ErrReferenced) {
		t.Fatalf("referenced issuer deactivation err=%v", err)
	}
}

func isPGCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}
