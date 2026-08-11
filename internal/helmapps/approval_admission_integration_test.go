package helmapps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/kuberploy/kuberploy/internal/testdb"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/id"
)

func TestPostgresApprovalAdmissionIsAtomicAndExactlyIdempotent(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "helmadmit_" + strings.ReplaceAll(id.New(), "-", "")
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP SCHEMA "+quotedSchema+" CASCADE") //nolint:errcheck
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = testdb.ApplyMigrations(ctx, pool); err != nil {
		t.Fatal(err)
	}

	actorID := id.New()
	now := testTime
	if _, err = pool.Exec(ctx, `INSERT INTO users(id,login,role,issuer,subject,created_at)
		VALUES($1,$2,'platform-admin','helm-admission-test',$3,$4)`, actorID,
		"helm-admission-"+actorID, actorID, now); err != nil {
		t.Fatal(err)
	}
	files := testChartFiles()
	packageBytes := packageChart(t, files)
	approval := testApproval(t, packageBytes, files)
	approval.ID, approval.CreatedBy, approval.IdempotencyKey = id.New(), actorID, "helm-admission-"+id.New()
	approval.CreatedAt = now
	documentsDigest, err := approvalDocumentsDigest(approval.ApprovalKey,
		files["values.schema.json"], files["values.yaml"])
	if err != nil {
		t.Fatal(err)
	}
	document := ApprovalDocument{Approval: approval, ValuesSchemaJSON: files["values.schema.json"],
		DefaultValuesYAML: files["values.yaml"], DocumentsDigest: documentsDigest, CreatedAt: now}
	store, err := NewPostgresApprovalAdmissionStore(pool)
	if err != nil {
		t.Fatal(err)
	}

	functionName := "reject_helm_admit_" + strings.ReplaceAll(id.New(), "-", "")
	quotedFunction := pgx.Identifier{functionName}.Sanitize()
	if _, err = pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'test rejection' USING ERRCODE='23514'; END $$;
		CREATE TRIGGER helm_admission_atomic_test BEFORE INSERT ON helm_chart_approval_documents
		FOR EACH ROW EXECUTE FUNCTION %s()`, quotedFunction, quotedFunction)); err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.AdmitApproval(ctx, document); err == nil {
		t.Fatal("document rejection did not abort admission")
	}
	var approvalCount int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM helm_chart_approvals WHERE approval_id=$1`,
		approval.ID).Scan(&approvalCount); err != nil || approvalCount != 0 {
		t.Fatalf("partial approval survived document failure: count=%d err=%v", approvalCount, err)
	}
	if _, err = pool.Exec(ctx, "DROP TRIGGER helm_admission_atomic_test ON helm_chart_approval_documents; DROP FUNCTION "+quotedFunction+"()"); err != nil {
		t.Fatal(err)
	}
	stored, replay, err := store.AdmitApproval(ctx, document)
	if err != nil || replay || stored.Validate() != nil {
		t.Fatalf("admit=%+v replay=%v err=%v", stored, replay, err)
	}
	replayed, replay, err := store.AdmitApproval(ctx, document)
	if err != nil || !replay || replayed.Approval.ID != approval.ID {
		t.Fatalf("replay=%+v replay=%v err=%v", replayed, replay, err)
	}
	catalog, err := store.ApprovalAdmissionCatalog(ctx, 1)
	if err != nil || len(catalog) != 1 || catalog[0].Approval.ID != approval.ID {
		t.Fatalf("catalog=%+v err=%v", catalog, err)
	}
	conflict := document
	conflict.Approval.ID = id.New()
	conflict.Approval.ChartVersion = "1.2.4"
	conflict.DocumentsDigest, err = approvalDocumentsDigest(conflict.Approval.ApprovalKey,
		conflict.ValuesSchemaJSON, conflict.DefaultValuesYAML)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.AdmitApproval(ctx, conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting idempotency replay accepted: %v", err)
	}
}
