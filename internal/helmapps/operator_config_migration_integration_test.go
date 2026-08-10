package helmapps

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/migrations"
)

const legacyOperatorConfigDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

func TestMigration032FailsClosedLegacyHelmWorkAndReadiness(t *testing.T) {
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
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	schema := "helm032_" + strings.ReplaceAll(id.New(), "-", "")
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err = conn.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), "SET search_path TO public")
		_, _ = conn.Exec(context.Background(), "DROP SCHEMA "+quotedSchema+" CASCADE")
	}()
	if _, err = conn.Exec(ctx, "SET search_path TO "+quotedSchema); err != nil {
		t.Fatal(err)
	}
	applyHelmMigrationsThrough(t, ctx, conn, "031_environment_namespace_foundations.sql")

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f := newHelmReleasePGFixture()
	setupHelmReleasePGFixture(t, ctx, tx, f)
	queuedID := id.New()
	processingID := id.New()
	insertLegacyHelmRenderCommand(t, ctx, tx, f, queuedID, f.now)
	insertLegacyHelmRenderCommand(t, ctx, tx, f, processingID, f.now)
	if _, err = tx.Exec(ctx, `UPDATE helm_render_commands SET state='processing',attempts=1,
		lease_owner='helm-legacy-worker-0001',lease_epoch=1,lease_until=$2,
		worker_contract=$3,worker_renderer_image=$4,worker_renderer_version=$5,
		worker_policy_version=$6,worker_limits_digest=$7,updated_at=$1
		WHERE id=$8`, f.now, f.now.Add(time.Minute), RendererContract, RendererImage,
		HelmVersion, PolicyVersion, helmPGDigest([]byte("legacy-limits")), processingID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO helm_renderer_readiness(
		worker_id,worker_epoch,contract_version,renderer_image,renderer_version,
		policy_version,limits_digest,started_at,observed_at,lease_until
	) VALUES($1,1,$2,$3,$4,$5,$6,$7,$7,$8)`, "helm-legacy-worker-0001",
		RendererContract, RendererImage, HelmVersion, PolicyVersion,
		helmPGDigest([]byte("legacy-limits")), f.now, f.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	body, err := migrations.FS.ReadFile("032_helm_operator_config_fencing.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, string(body)); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.Query(ctx, `SELECT id::text,state,operator_config_digest,
		COALESCE(last_failure_code,''),lease_owner,worker_operator_config_digest,completed_at
		FROM helm_render_commands WHERE id=ANY($1::uuid[]) ORDER BY id`, []string{queuedID, processingID})
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var commandID, state, operatorDigest, failureCode string
		var leaseOwner, workerDigest *string
		var completedAt *time.Time
		if err = rows.Scan(&commandID, &state, &operatorDigest, &failureCode,
			&leaseOwner, &workerDigest, &completedAt); err != nil {
			t.Fatal(err)
		}
		if state != "failed" || operatorDigest != legacyOperatorConfigDigest ||
			failureCode != "operator-config-upgrade" || leaseOwner != nil || workerDigest != nil || completedAt == nil {
			t.Fatalf("legacy command not failed closed: id=%s state=%s digest=%s code=%s owner=%v worker=%v completed=%v",
				commandID, state, operatorDigest, failureCode, leaseOwner, workerDigest, completedAt)
		}
		seen++
	}
	if err = rows.Err(); err != nil || seen != 2 {
		t.Fatalf("legacy command rows=%d err=%v", seen, err)
	}
	var readinessCount int
	if err = conn.QueryRow(ctx, `SELECT count(*) FROM helm_renderer_readiness`).Scan(&readinessCount); err != nil || readinessCount != 0 {
		t.Fatalf("legacy readiness survived: count=%d err=%v", readinessCount, err)
	}
	if _, err = conn.Exec(ctx, `UPDATE helm_render_commands SET operator_config_digest=$2 WHERE id=$1`,
		queuedID, helmPGOperatorDigest()); !postgresCheckViolation(err) {
		t.Fatalf("terminal legacy operator digest was mutable: %v", err)
	}
}

func applyHelmMigrationsThrough(t *testing.T, ctx context.Context, conn *pgxpool.Conn, through string) {
	t.Helper()
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") && entry.Name() <= through {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		body, readErr := migrations.FS.ReadFile(name)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if _, execErr := conn.Exec(ctx, string(body)); execErr != nil {
			t.Fatalf("apply %s: %v", name, execErr)
		}
	}
}

func insertLegacyHelmRenderCommand(t *testing.T, ctx context.Context, tx pgx.Tx,
	f helmReleasePGFixture, commandID string, at time.Time) {
	t.Helper()
	descriptor := []byte("apiVersion: kuberploy.io/v1alpha1\nkind: ApprovedHelmApplication\n")
	_, err := tx.Exec(ctx, `INSERT INTO helm_render_commands(
		id,idempotency_scope,idempotency_key,approval_id,approval_revision,project_id,
		environment_id,application_id,namespace,release_name,descriptor_yaml,values_yaml,
		descriptor_digest,values_digest,input_digest,state,available_at,created_at,updated_at
	) VALUES($1,$2,$3,$4,1,$5,$6,$7,$8,'sample',$9,$10,$11,$12,$13,'queued',$14,$14,$14)`,
		commandID, f.userID, "legacy-render-"+commandID, f.approvalID, f.projectID,
		f.environmentID, f.applicationID, f.namespace, descriptor, f.values,
		helmPGDigest(descriptor), f.valuesDigest,
		helmPGDigest(append(append([]byte{}, descriptor...), f.values...)), at)
	if err != nil {
		t.Fatal(err)
	}
}
