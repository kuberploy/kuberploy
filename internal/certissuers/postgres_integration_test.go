package certissuers

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/id"
	storepostgres "github.com/kuberploy/kuberploy/internal/store/postgres"
	"github.com/kuberploy/kuberploy/migrations"
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
	if err = storepostgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err = storepostgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("idempotent migration rerun: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	admin, tenant := id.New(), id.New()
	if _, err = pool.Exec(ctx, `INSERT INTO users(id,login,role,issuer,subject) VALUES($1,$2,'platform-admin','test',$2),($3,$4,'developer','test',$4)`, admin, "issuer-admin-"+admin[:8], tenant, "issuer-tenant-"+tenant[:8]); err != nil {
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
	if _, err = pool.Exec(ctx, `UPDATE cert_manager_issuer_profile_revisions SET spec_digest=$3 WHERE profile_id=$1 AND revision=$2`, created.Profile.ID, 1, "sha256:"+strings.Repeat("0", 64)); !isPGCode(err, "23514") {
		t.Fatalf("immutable revision update err=%v", err)
	}
	observed := now.Add(time.Second)
	if err = store.RecordObservation(ctx, Observation{ProfileID: created.Profile.ID, Revision: 1, State: Ready, ObservedSpecDigest: created.Revision.SpecDigest, ObservedGeneration: 1, ObservedAt: &observed, UpdatedAt: observed}); err != nil {
		t.Fatal(err)
	}
	identities, err := store.ReadyForHostname(ctx, "api.example.com", observed, 5*time.Minute, 20)
	if err != nil || len(identities) != 1 || identities[0].Name != created.Profile.Name {
		t.Fatalf("identities=%v err=%v", identities, err)
	}
	if _, err = store.Deactivate(ctx, Command{ActorID: admin, IdempotencyKey: "admin-deactivate-" + admin, RequestID: "request-admin-deactivate", Now: now.Add(2 * time.Second)}, Ref{ProfileID: created.Profile.ID, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err = pool.QueryRow(ctx, `SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&schema); err != nil || schema != migrations.CurrentSchema+".sql" {
		t.Fatalf("schema=%q err=%v", schema, err)
	}
}

func isPGCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}
