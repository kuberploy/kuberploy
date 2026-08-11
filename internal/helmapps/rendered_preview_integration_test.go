package helmapps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/id"
	platformpostgres "github.com/kuberploy/kuberploy/internal/store/postgres"
)

func TestPostgresRenderedPreviewResolvesExactSuccessfulHeadAndRedacts(t *testing.T) {
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
	schema := "helmpreview_" + strings.ReplaceAll(id.New(), "-", "")
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
	if err = platformpostgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	f := newHelmReleasePGFixture()
	setup, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	setupHelmReleasePGFixture(t, ctx, setup, f)
	if err = setup.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	store, err := NewPostgresStore(pool, helmPGOperatorDigest())
	if err != nil {
		t.Fatal(err)
	}
	approval, err := store.Approval(ctx, ApprovalKey{ID: f.approvalID, Revision: 1})
	if err != nil {
		t.Fatal(err)
	}
	target := ReleaseTarget{ProjectID: f.projectID, EnvironmentID: f.environmentID,
		ApplicationID: f.applicationID}
	desired, err := NewDesiredRender(id.New(), f.userID, "preview-render-"+id.New(), approval,
		DestinationIdentity{ProjectID: f.projectID, EnvironmentID: f.environmentID,
			ApplicationID: f.applicationID, ApplicationSlug: "sample", Namespace: f.namespace}, f.values)
	if err != nil {
		t.Fatal(err)
	}
	if _, replay, submitErr := store.Submit(ctx, desired, f.now); submitErr != nil || replay {
		t.Fatalf("submit replay=%v err=%v", replay, submitErr)
	}
	revisionID := id.New()
	releaseTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = insertHelmReleaseErr(ctx, releaseTx, f, helmReleaseInsert{id: revisionID, generation: 1,
		action: "initial", commandID: desired.ID, values: f.values, valuesDigest: f.valuesDigest},
		f.now.Add(2*time.Second)); err != nil {
		_ = releaseTx.Rollback(ctx)
		t.Fatal(err)
	}
	if _, err = releaseTx.Exec(ctx, `INSERT INTO helm_release_heads(
		project_id,environment_id,application_id,revision_id,generation,updated_at
	) VALUES($1,$2,$3,$4,1,$5)`, f.projectID, f.environmentID, f.applicationID, revisionID,
		f.now.Add(2*time.Second)); err != nil {
		_ = releaseTx.Rollback(ctx)
		t.Fatal(err)
	}
	if err = releaseTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	lease, err := store.Claim(ctx, "helm-preview-worker-0001",
		ExpectedRenderWorkerIdentity(helmPGOperatorDigest()), f.now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	labels := lease.Command.Descriptor.RequiredLabels()
	raw := []byte(fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: preview-safe
  namespace: %s
  labels:
    app.kubernetes.io/instance: %s
    app.kubernetes.io/name: %s
    kuberploy.io/application: %s
    kuberploy.io/environment: %s
    kuberploy.io/project: %s
data:
  password: never-return-this-value
`, f.namespace, labels["app.kubernetes.io/instance"], labels["app.kubernetes.io/name"],
		labels["kuberploy.io/application"], labels["kuberploy.io/environment"], labels["kuberploy.io/project"]))
	validated, err := ValidateRenderedManifests(raw, lease.Command.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Complete(ctx, lease, validated, f.now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	service, err := NewPostgresRenderedManifestPreviewService(pool)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := service.Preview(ctx, target)
	if err != nil || preview.ReleaseRevisionID != revisionID || preview.ResourceCount != 1 ||
		len(preview.Resources) != 1 || preview.Resources[0].Name != "preview-safe" ||
		preview.PreviewBytes != len(preview.Resources[0].SanitizedYAML) || preview.Resources[0].PreviewOmitted ||
		preview.ManifestDigest != validated.ManifestDigest || preview.InventoryDigest != validated.InventoryDigest {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	encoded, err := json.Marshal(preview)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"never-return-this-value", "password", desired.ID,
		"helm-preview-worker-0001"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("preview leaked %q: %s", forbidden, encoded)
		}
	}
	if !strings.Contains(preview.Resources[0].SanitizedYAML, "data: '[REDACTED]'") {
		t.Fatalf("ConfigMap payload was not explicitly redacted: %s", preview.Resources[0].SanitizedYAML)
	}
	wrong := target
	wrong.ProjectID = id.New()
	if _, err = service.Preview(ctx, wrong); !errors.Is(err, ErrNotFound) {
		t.Fatalf("mismatched target resolved preview: %v", err)
	}
}
