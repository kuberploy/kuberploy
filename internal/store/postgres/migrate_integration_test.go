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
	"github.com/kuberploy/kuberploy/migrations"
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

	recoveryHistoryID := id.New()
	recoveryLogs := strings.Join([]string{
		"Migration name: 011_helm_application_cascade_preflight",
		"Database error code: 23514",
		"Terminal Helm protected Application intents are immutable",
		"PL/pgSQL function public.validate_helm_protected_application_intent()",
	}, "\n")
	if _, err = pool.Exec(ctx, `INSERT INTO _prisma_migrations(
		id,checksum,migration_name,logs,started_at,rolled_back_at,applied_steps_count
		) SELECT $1,$2,$3::varchar(255),$4,started_at-interval '2 seconds',started_at-interval '1 second',0
			FROM _prisma_migrations WHERE migration_name=$3::varchar(255) AND finished_at IS NOT NULL`, recoveryHistoryID,
		migrations.RecoverableRC171Checksum, migrations.RecoverableRC171Migration, recoveryLogs); err != nil {
		t.Fatal(err)
	}
	if err = VerifySchema(ctx, pool); err != nil {
		t.Fatalf("verify exact RC171 rolled-back evidence: %v", err)
	}
	interruptedHistoryID := id.New()
	if _, err = pool.Exec(ctx, `INSERT INTO _prisma_migrations(
		id,checksum,migration_name,logs,started_at,rolled_back_at,applied_steps_count
		) SELECT $1,$2,$3::varchar(255),'',started_at-interval '900 milliseconds',started_at-interval '500 milliseconds',0
			FROM _prisma_migrations WHERE migration_name=$3::varchar(255) AND finished_at IS NOT NULL`, interruptedHistoryID,
		migrations.RecoverableRC171Checksum, migrations.RecoverableRC171Migration); err != nil {
		t.Fatal(err)
	}
	if err = VerifySchema(ctx, pool); err != nil {
		t.Fatalf("verify bounded interrupted RC171 evidence: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE _prisma_migrations SET applied_steps_count=0
		WHERE migration_name=$1 AND finished_at IS NOT NULL`, migrations.RecoverableRC171Migration); err != nil {
		t.Fatal(err)
	}
	if err = VerifySchema(ctx, pool); err != nil {
		t.Fatalf("verify crash-attested applied RC171 evidence: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE _prisma_migrations SET applied_steps_count=1
		WHERE migration_name=$1 AND finished_at IS NOT NULL`, migrations.RecoverableRC171Migration); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE _prisma_migrations AS interruption
		SET started_at=failure.started_at-interval '1 second'
		FROM _prisma_migrations AS failure
		WHERE interruption.id=$1 AND failure.id=$2`, interruptedHistoryID, recoveryHistoryID); err != nil {
		t.Fatal(err)
	}
	if err = VerifySchema(ctx, pool); !errors.Is(err, ErrMigrationMismatch) {
		t.Fatalf("reversed RC171 recovery chronology err=%v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE _prisma_migrations AS interruption
		SET started_at=applied.started_at-interval '900 milliseconds'
		FROM _prisma_migrations AS applied
		WHERE interruption.id=$1 AND applied.migration_name=$2 AND applied.finished_at IS NOT NULL`,
		interruptedHistoryID, migrations.RecoverableRC171Migration); err != nil {
		t.Fatal(err)
	}
	history, historyErr := migrations.History()
	if historyErr != nil {
		t.Fatal(historyErr)
	}
	cleanupChecksum := history[len(history)-1].Checksum
	cleanupHistoryID := id.New()
	if _, err = pool.Exec(ctx, `INSERT INTO _prisma_migrations(
		id,checksum,migration_name,logs,started_at,rolled_back_at,applied_steps_count
		) SELECT $1,$2,$3::varchar(255),'',prior.finished_at+(current_row.started_at-prior.finished_at)/3,
			prior.finished_at+((current_row.started_at-prior.finished_at)*2)/3,1
			FROM _prisma_migrations AS current_row
			JOIN _prisma_migrations AS prior ON prior.migration_name=$4::varchar(255) AND prior.finished_at IS NOT NULL
			WHERE current_row.migration_name=$3::varchar(255) AND current_row.finished_at IS NOT NULL`, cleanupHistoryID,
		cleanupChecksum, migrations.RecoverableRC171CleanupMigration, migrations.RecoverableRC171Migration); err != nil {
		t.Fatal(err)
	}
	if err = VerifySchema(ctx, pool); err != nil {
		t.Fatalf("verify interrupted migration cleanup evidence: %v", err)
	}
	duplicateCleanupHistoryID := id.New()
	if _, err = pool.Exec(ctx, `INSERT INTO _prisma_migrations(
		id,checksum,migration_name,logs,started_at,rolled_back_at,applied_steps_count
		) SELECT $1,$2,$3::varchar(255),'',prior.finished_at+(current_row.started_at-prior.finished_at)/3,
			prior.finished_at+((current_row.started_at-prior.finished_at)*2)/3,0
			FROM _prisma_migrations AS current_row
			JOIN _prisma_migrations AS prior ON prior.migration_name=$4::varchar(255) AND prior.finished_at IS NOT NULL
			WHERE current_row.migration_name=$3::varchar(255) AND current_row.finished_at IS NOT NULL`, duplicateCleanupHistoryID,
		cleanupChecksum, migrations.RecoverableRC171CleanupMigration, migrations.RecoverableRC171Migration); err != nil {
		t.Fatal(err)
	}
	if err = VerifySchema(ctx, pool); !errors.Is(err, ErrMigrationMismatch) {
		t.Fatalf("duplicate interrupted cleanup evidence err=%v", err)
	}
	if _, err = pool.Exec(ctx, `DELETE FROM _prisma_migrations WHERE id=$1`, duplicateCleanupHistoryID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE _prisma_migrations SET logs='forged' WHERE id=$1`, recoveryHistoryID); err != nil {
		t.Fatal(err)
	}
	if err = VerifySchema(ctx, pool); !errors.Is(err, ErrMigrationMismatch) {
		t.Fatalf("forged rolled-back evidence err=%v", err)
	}
	if _, err = pool.Exec(ctx, `DELETE FROM _prisma_migrations WHERE id IN ($1,$2,$3)`, recoveryHistoryID, interruptedHistoryID, cleanupHistoryID); err != nil {
		t.Fatal(err)
	}

	assertCatalogCount(t, ctx, pool, "application tables", `SELECT count(*)
		FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname='public' AND c.relkind='r' AND c.relname <> '_prisma_migrations'`, 109)
	assertCatalogCount(t, ctx, pool, "native functions", `SELECT count(*)
		FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
		WHERE n.nspname='public'`, 104)
	assertCatalogCount(t, ctx, pool, "non-internal triggers", `SELECT count(*)
		FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname='public' AND NOT t.tgisinternal`, 101)
	assertCatalogCount(t, ctx, pool, "check constraints", `SELECT count(*)
		FROM pg_constraint c JOIN pg_namespace n ON n.oid=c.connamespace
		WHERE n.nspname='public' AND c.contype='c'`, 913)
	assertCatalogCount(t, ctx, pool, "deferred constraints", `SELECT count(*)
		FROM pg_constraint c JOIN pg_namespace n ON n.oid=c.connamespace
		WHERE n.nspname='public' AND c.condeferrable`, 13)
	assertCatalogCount(t, ctx, pool, "expression indexes", `SELECT count(*)
		FROM pg_index i JOIN pg_class c ON c.oid=i.indrelid JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname='public' AND i.indexprs IS NOT NULL`, 1)

	for _, function := range []string{
		"protect_git_pull_request_publication",
		"protect_secret_binding_version",
		"validate_helm_release_revision",
		"validate_configuration_profile_assignment",
		"validate_runtime_readiness",
		"validate_mutation_receipt",
		"protect_preview_authority",
		"protect_git_write_command",
		"validate_git_write_operation",
		"validate_tls_certificate_runtime_readiness",
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
		"preview_authorities_protect",
		"git_write_commands_protect",
		"git_write_commands_operation",
		"runtime_readiness_tls_certificate_validate",
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
