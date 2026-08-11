package postgres

import (
	"context"
	"os"
	"testing"

	"github.com/kuberploy/kuberploy/internal/testdb"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPrismaMigrationPreservesNativePostgreSQLAuthority(t *testing.T) {
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
	if err = VerifySchema(ctx, pool); err != nil {
		t.Fatalf("verify exact Prisma history: %v", err)
	}

	assertCatalogCount(t, ctx, pool, "application tables", `SELECT count(*)
		FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname='public' AND c.relkind='r' AND c.relname <> '_prisma_migrations'`, 126)
	assertCatalogCount(t, ctx, pool, "native functions", `SELECT count(*)
		FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
		WHERE n.nspname='public'`, 81)
	assertCatalogCount(t, ctx, pool, "non-internal triggers", `SELECT count(*)
		FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname='public' AND NOT t.tgisinternal`, 89)
	assertCatalogCount(t, ctx, pool, "check constraints", `SELECT count(*)
		FROM pg_constraint c JOIN pg_namespace n ON n.oid=c.connamespace
		WHERE n.nspname='public' AND c.contype='c'`, 876)
	assertCatalogCount(t, ctx, pool, "deferred constraints", `SELECT count(*)
		FROM pg_constraint c JOIN pg_namespace n ON n.oid=c.connamespace
		WHERE n.nspname='public' AND c.condeferrable`, 12)
	assertCatalogCount(t, ctx, pool, "expression indexes", `SELECT count(*)
		FROM pg_index i JOIN pg_class c ON c.oid=i.indrelid JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname='public' AND i.indexprs IS NOT NULL`, 2)

	for _, function := range []string{
		"protect_git_pull_request_publication",
		"protect_secret_binding_version",
		"validate_helm_release_revision",
		"validate_scheduling_profile_assignment",
		"enqueue_auto_deploy_runs",
	} {
		var present bool
		if err = pool.QueryRow(ctx, `SELECT EXISTS(
			SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
			WHERE n.nspname='public' AND p.proname=$1)`, function).Scan(&present); err != nil {
			t.Fatal(err)
		}
		if !present {
			t.Errorf("native PostgreSQL function %q is missing", function)
		}
	}
	for _, trigger := range []string{
		"git_pull_request_publications_protect",
		"secret_binding_versions_protect",
		"helm_release_revisions_validate",
		"scheduling_profile_assignment_validate",
		"build_release_enqueue_auto_deploy",
	} {
		var present bool
		if err = pool.QueryRow(ctx, `SELECT EXISTS(
			SELECT 1 FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid
			JOIN pg_namespace n ON n.oid=c.relnamespace
			WHERE n.nspname='public' AND NOT t.tgisinternal AND t.tgname=$1)`, trigger).Scan(&present); err != nil {
			t.Fatal(err)
		}
		if !present {
			t.Errorf("native PostgreSQL trigger %q is missing", trigger)
		}
	}
}

func assertCatalogCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, label, query string, expected int) {
	t.Helper()
	var actual int
	if err := pool.QueryRow(ctx, query).Scan(&actual); err != nil {
		t.Fatal(err)
	}
	if actual != expected {
		t.Errorf("%s count=%d, expected %d", label, actual, expected)
	}
}
