package postgres

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/testdb"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/id"
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
		WHERE n.nspname='public' AND c.relkind='r' AND c.relname <> '_prisma_migrations'`, 104)
	assertCatalogCount(t, ctx, pool, "native functions", `SELECT count(*)
		FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
		WHERE n.nspname='public'`, 65)
	assertCatalogCount(t, ctx, pool, "non-internal triggers", `SELECT count(*)
		FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname='public' AND NOT t.tgisinternal`, 70)
	assertCatalogCount(t, ctx, pool, "check constraints", `SELECT count(*)
		FROM pg_constraint c JOIN pg_namespace n ON n.oid=c.connamespace
		WHERE n.nspname='public' AND c.contype='c'`, 764)
	assertCatalogCount(t, ctx, pool, "deferred constraints", `SELECT count(*)
		FROM pg_constraint c JOIN pg_namespace n ON n.oid=c.connamespace
		WHERE n.nspname='public' AND c.condeferrable`, 10)
	assertCatalogCount(t, ctx, pool, "expression indexes", `SELECT count(*)
		FROM pg_index i JOIN pg_class c ON c.oid=i.indrelid JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname='public' AND i.indexprs IS NOT NULL`, 2)

	for _, function := range []string{
		"protect_git_pull_request_publication",
		"protect_secret_binding_version",
		"validate_helm_release_revision",
		"validate_configuration_profile_assignment",
		"validate_runtime_readiness",
		"validate_mutation_receipt",
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
		"configuration_profile_assignment_validate",
		"runtime_readiness_validate",
		"mutation_receipts_validate",
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

	workerID := "migration-readiness-" + id.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err = pool.Exec(ctx, `INSERT INTO runtime_readiness(runtime_kind,scope_key,worker_id,worker_epoch,
		contract_version,config_digest,identity,observation,started_at,observed_at,lease_until,updated_at)
		VALUES('source-build','global',$1,1,'source-build.v1',$2,
		jsonb_build_object('githubAppId',1,'builderNamespace','builds','builderAgentImage',$3::text),
		'{}'::jsonb,$4,$4,$4::timestamptz+interval '1 minute',$4)`, workerID, "sha256:"+strings.Repeat("1", 64),
		"registry.example/kuberploy-build-agent@sha256:"+strings.Repeat("2", 64), now); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `UPDATE runtime_readiness SET runtime_kind='edge',contract_version='edge-observer.v1',
		identity=jsonb_build_object('targetCount',1) WHERE runtime_kind='source-build' AND worker_id=$1`, workerID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("cross-kind readiness substitution err=%v", err)
	}

	actorID, receiptID := id.New(), id.New()
	if _, err = pool.Exec(ctx, `INSERT INTO users(id,login,role,issuer,subject,created_at)
		VALUES($1,$2,'platform-admin','migration-authority',$2,$3)`, actorID, "migration-receipt-"+actorID[:8], now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO mutation_receipts(actor_id,receipt_kind,namespace,scope_key,
		idempotency_key,request_digest,resource_type,resource_id,created_at)
		VALUES($1,'resource','migration.authority','global',$2,'request-fingerprint','migration-receipt',$3,$4)`,
		actorID, "receipt-"+receiptID, receiptID, now); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `UPDATE mutation_receipts SET request_digest='substituted'
		WHERE actor_id=$1 AND receipt_kind='resource' AND namespace='migration.authority'`, actorID)
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("mutation receipt update err=%v", err)
	}
	_, err = pool.Exec(ctx, `DELETE FROM mutation_receipts
		WHERE actor_id=$1 AND receipt_kind='resource' AND namespace='migration.authority'`, actorID)
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("mutation receipt delete err=%v", err)
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
