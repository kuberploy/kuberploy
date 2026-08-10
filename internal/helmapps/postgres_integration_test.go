package helmapps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/id"
	platformpostgres "github.com/kuberploy/kuberploy/internal/store/postgres"
)

func TestPostgresStoreApprovalRenderLeaseAndResult(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = platformpostgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	operatorDigest := digestBytes([]byte("helm-postgres-integration-operator.v1"))
	store, err := NewPostgresStore(pool, operatorDigest)
	if err != nil {
		t.Fatal(err)
	}

	userID, projectID, environmentID, applicationID := id.New(), id.New(), id.New(), id.New()
	approvalID, commandID, scopeID := id.New(), id.New(), userID
	namespace := "helm-" + strings.ReplaceAll(environmentID[:8], "-", "")
	now := time.Now().UTC().Truncate(time.Microsecond)
	_, err = pool.Exec(ctx, `INSERT INTO users(id,login,role,issuer,subject,created_at)
		VALUES($1,$2,'platform-admin',$3,$4,$5)`, userID, "helm-test-"+userID, "helm-test", userID, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO projects(id,name,slug,created_at) VALUES($1,$2,$3,$4)`,
		projectID, "Helm Test", "helm-"+projectID, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7)`, environmentID, projectID, "Helm Test", "helm", namespace, "helm", now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO applications(id,project_id,name,slug,created_at)
		VALUES($1,$2,$3,$4,$5)`, applicationID, projectID, "Sample", "sample", now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM helm_renderer_readiness WHERE worker_id=$1`, "helm-postgres-worker-0001")
		_, _ = pool.Exec(context.Background(), `DELETE FROM helm_renderer_readiness WHERE worker_id=$1`, "helm-postgres-worker-old1")
		_, _ = pool.Exec(context.Background(), `DELETE FROM helm_render_results WHERE command_id=$1`, commandID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM helm_render_commands WHERE id=$1`, commandID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM helm_chart_approvals WHERE approval_id=$1`, approvalID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM applications WHERE id=$1`, applicationID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM environments WHERE id=$1`, environmentID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id=$1`, projectID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, userID)
	})

	approval := Approval{ApprovalKey: ApprovalKey{ID: approvalID, Revision: 1},
		OCIRepository: "oci://registry.example.com/platform/sample", ChartVersion: "1.2.3",
		ManifestDigest: digestBytes([]byte(approvalID + "-manifest")), PackageDigest: digestBytes([]byte(approvalID + "-package")),
		ValuesSchemaDigest: digestBytes([]byte(approvalID + "-schema")), RendererImage: RendererImage,
		RendererVersion: HelmVersion, PolicyVersion: PolicyVersion, CreatedBy: userID,
		IdempotencyKey: "approval-" + approvalID, CreatedAt: now}
	if _, replay, putErr := store.PutApproval(ctx, approval); putErr != nil || replay {
		t.Fatalf("put approval: replay=%v err=%v", replay, putErr)
	}
	if _, replay, putErr := store.PutApproval(ctx, approval); putErr != nil || !replay {
		t.Fatalf("replay approval: replay=%v err=%v", replay, putErr)
	}
	destination := DestinationIdentity{ProjectID: projectID, EnvironmentID: environmentID,
		ApplicationID: applicationID, ApplicationSlug: "sample", Namespace: namespace}
	desired, err := NewDesiredRender(commandID, scopeID, "render-"+commandID, approval, destination, []byte("{}\n"))
	if err != nil {
		t.Fatal(err)
	}
	terminalInsertID := id.New()
	_, err = pool.Exec(ctx, `INSERT INTO helm_render_commands(
		id,idempotency_scope,idempotency_key,approval_id,approval_revision,project_id,
		environment_id,application_id,namespace,release_name,descriptor_yaml,values_yaml,
		descriptor_digest,values_digest,input_digest,operator_config_digest,state,available_at,created_at,updated_at,completed_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,'succeeded',$17,$17,$17,$17)`,
		terminalInsertID, scopeID, "terminal-insert-"+terminalInsertID, approval.ID, approval.Revision,
		projectID, environmentID, applicationID, namespace, "sample", desired.DescriptorYAML,
		desired.ValuesYAML, desired.DescriptorDigest, desired.ValuesDigest, desired.InputDigest, operatorDigest, now)
	if !postgresCheckViolation(err) {
		t.Fatalf("direct terminal INSERT was not rejected: %v", err)
	}
	mismatchedDestination := destination
	mismatchedDestination.ApplicationSlug = "other"
	mismatchedID := id.New()
	mismatched, newErr := NewDesiredRender(mismatchedID, scopeID, "release-mismatch-"+mismatchedID,
		approval, mismatchedDestination, []byte("{}\n"))
	if newErr != nil {
		t.Fatal(newErr)
	}
	_, err = pool.Exec(ctx, `INSERT INTO helm_render_commands(
		id,idempotency_scope,idempotency_key,approval_id,approval_revision,project_id,
		environment_id,application_id,namespace,release_name,descriptor_yaml,values_yaml,
		descriptor_digest,values_digest,input_digest,operator_config_digest,state,available_at,created_at,updated_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,'queued',$17,$17,$17)`,
		mismatched.ID, scopeID, mismatched.IdempotencyKey, approval.ID, approval.Revision,
		projectID, environmentID, applicationID, namespace, mismatched.Descriptor.ReleaseName,
		mismatched.DescriptorYAML, mismatched.ValuesYAML, mismatched.DescriptorDigest,
		mismatched.ValuesDigest, mismatched.InputDigest, operatorDigest, now)
	if !postgresCheckViolation(err) {
		t.Fatalf("durable application release mismatch was not rejected: %v", err)
	}
	if _, replay, err := store.Submit(ctx, desired, now); err != nil || replay {
		t.Fatalf("submit: replay=%v err=%v", replay, err)
	}
	oldDigest := digestBytes([]byte("helm-postgres-integration-old-operator.v1"))
	oldStore, err := NewPostgresStore(pool, oldDigest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = oldStore.Claim(ctx, "helm-postgres-worker-old1", ExpectedRenderWorkerIdentity(oldDigest), now, time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old-config worker adopted new command: %v", err)
	}
	first, err := store.Claim(ctx, "helm-postgres-worker-0001", ExpectedRenderWorkerIdentity(operatorDigest), now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Claim(ctx, "helm-postgres-worker-0002", ExpectedRenderWorkerIdentity(operatorDigest), first.Until, time.Minute)
	if err != nil || second.Epoch != first.Epoch+1 || second.Command.Attempts != 2 {
		t.Fatalf("reclaim: %+v %v", second, err)
	}
	if _, err = store.Heartbeat(ctx, first, first.Until, time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale heartbeat accepted: %v", err)
	}
	labels := second.Command.Descriptor.RequiredLabels()
	manifest := []byte(fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: sample\n  namespace: %s\n  labels:\n    app.kubernetes.io/instance: %s\n    app.kubernetes.io/name: %s\n    kuberploy.io/application: %s\n    kuberploy.io/environment: %s\n    kuberploy.io/project: %s\ndata:\n  x: y\n",
		namespace, labels["app.kubernetes.io/instance"], labels["app.kubernetes.io/name"],
		labels["kuberploy.io/application"], labels["kuberploy.io/environment"], labels["kuberploy.io/project"]))
	validated, err := ValidateRenderedManifests(manifest, second.Command.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO helm_render_results(
		command_id,input_digest,operator_config_digest,manifest_digest,inventory_digest,rendered_manifests,
		resource_count,output_bytes,renderer_image,renderer_version,policy_version,limits_digest,completed_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, second.Command.ID,
		second.Command.InputDigest, oldDigest, validated.ManifestDigest, validated.InventoryDigest,
		validated.Raw, validated.ResourceCount, len(validated.Raw), RendererImage, HelmVersion,
		PolicyVersion, LimitsDigest(), first.Until.Add(time.Second))
	if !postgresCheckViolation(err) {
		t.Fatalf("substituted result operator digest accepted: %v", err)
	}
	result, err := store.Complete(ctx, second, validated, first.Until.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.Result(ctx, commandID)
	if err != nil || stored.ManifestDigest != result.ManifestDigest {
		t.Fatalf("result: %+v %v", stored, err)
	}
	_, err = pool.Exec(ctx, `UPDATE helm_render_commands SET updated_at=updated_at+interval '1 second' WHERE id=$1`, commandID)
	if !postgresCheckViolation(err) {
		t.Fatalf("direct terminal UPDATE was not rejected: %v", err)
	}
	readiness := Readiness{WorkerID: "helm-postgres-worker-0001", WorkerEpoch: 1,
		RenderWorkerIdentity: ExpectedRenderWorkerIdentity(operatorDigest), StartedAt: now, ObservedAt: now,
		LeaseUntil: now.Add(time.Minute)}
	if err = store.PutReadiness(ctx, readiness); err != nil {
		t.Fatal(err)
	}
	oldReadiness := readiness
	oldReadiness.WorkerID = "helm-postgres-worker-old1"
	oldReadiness.RenderWorkerIdentity = ExpectedRenderWorkerIdentity(oldDigest)
	if err = oldStore.PutReadiness(ctx, oldReadiness); err != nil {
		t.Fatal(err)
	}
	if ready, readyErr := store.RuntimeReady(ctx, now.Add(time.Second)); readyErr != nil || !ready {
		t.Fatalf("readiness: %v %v", ready, readyErr)
	}
	if ready, readyErr := oldStore.RuntimeReady(ctx, now.Add(time.Second)); readyErr != nil || !ready {
		t.Fatalf("old store did not see its own exact readiness: %v %v", ready, readyErr)
	}
	if _, err = pool.Exec(ctx, `UPDATE helm_renderer_readiness SET operator_config_digest=$2
		WHERE worker_id=$1`, readiness.WorkerID, oldDigest); !postgresCheckViolation(err) {
		t.Fatalf("same-epoch readiness digest substitution accepted: %v", err)
	}
}

func postgresCheckViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23514"
}
