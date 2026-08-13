package postgres

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/registry"
)

func TestRefreshRegistryProtectionFromExactDurableSources(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := context.Background()
	st, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := databaseTime(time.Now())
	projectID, environmentID, applicationID := id.New(), id.New(), id.New()
	deploymentID, initialOperationID, activeOperationID := id.New(), id.New(), id.New()
	bindingID, targetID := id.New(), id.New()
	repository := "integration/" + targetID + "/service"
	configRevisionA, configRevisionB := strings.Repeat("a", 40), strings.Repeat("b", 40)
	digestA, digestB, digestC := postgresRegistryDigest("a"), postgresRegistryDigest("b"), postgresRegistryDigest("c")
	image := func(digest string) string { return "registry.integration.test/" + repository + "@" + digest }

	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO projects(id,name,slug,created_at) VALUES($1,$2,$3,$4)`, []any{projectID, "registry-protection-" + projectID, "rp-" + projectID, now}},
		{`INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at) VALUES($1,$2,'Dev','dev',$3,$4,$5)`, []any{environmentID, projectID, "kp-rp-" + environmentID, "kp-rp-" + environmentID, now}},
		{`INSERT INTO applications(id,project_id,name,slug,created_at) VALUES($1,$2,'API','api',$3)`, []any{applicationID, projectID, now}},
		{`INSERT INTO operations(id,kind,status,target_type,target_id,request_id,generation,progress,git_revision,created_at,updated_at,finished_at) VALUES($1,'deployment.apply','succeeded','deployment',$2,$3,1,'[]',$4,$5,$5,$5)`, []any{initialOperationID, deploymentID, "rp-initial-" + deploymentID, configRevisionA, now}},
		{`INSERT INTO deployments(id,environment_id,application_id,image,replicas,port,environment,route,runtime,state,operation_id,desired_revision,observed_revision,generation,created_at,updated_at) VALUES($1,$2,$3,$4,1,8080,'{}',NULL,'{}','active',$5,$6,$6,1,$7,$7)`, []any{deploymentID, environmentID, applicationID, image(digestA), initialOperationID, configRevisionA, now}},
		{`INSERT INTO operations(id,kind,status,target_type,target_id,request_id,generation,progress,git_revision,created_at,updated_at) VALUES($1,'deployment.apply','queued','deployment',$2,$3,2,'[]','',$4,$4)`, []any{activeOperationID, deploymentID, "rp-active-" + deploymentID, now}},
		{`INSERT INTO deployment_operation_inputs(operation_id,deployment_id,image,replicas,port,environment,route,runtime,created_at) VALUES($1,$2,$3,1,8080,'{}',NULL,'{}',$4)`, []any{activeOperationID, deploymentID, image(digestC), now}},
		{`INSERT INTO git_repository_bindings(id,kind,scope_id,project_id,environment_id,provider,installation_id,repository_id,repository_owner,repository_name,target_ref,path_prefix,credential_secret_name,state,target_head_revision,indexed_revision,projection_generation,parser_version,target_head_observed_at,indexed_at,created_at,updated_at) VALUES($1,'environment',$2,$3,$2,'github',1,1,'kuberploy','registry-protection','refs/heads/main',$4,'registry-protection','ready',$5,$5,1,'appconfig.v1',$6,$6,$6,$6)`, []any{bindingID, environmentID, projectID, "tenants/" + projectID + "/environments/" + environmentID, strings.Repeat("f", 40), now}},
		{`INSERT INTO git_projection_generations(binding_id,generation,head_revision,parser_version,state,started_at,activated_at) VALUES($1,1,$2,'appconfig.v1','active',$3,$3)`, []any{bindingID, strings.Repeat("f", 40), now}},
	}
	for _, statement := range statements {
		if _, err = st.pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	insertDocument := func(generation int64, sourceRevision, configRevision, digest string, indexedAt time.Time) {
		t.Helper()
		parsed, marshalErr := json.Marshal(map[string]any{"spec": map[string]any{"delivery": map[string]any{"release": map[string]any{"repository": "registry.integration.test/" + repository, "digest": digest}}}})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		path := "tenants/" + projectID + "/environments/" + environmentID + "/applications/" + applicationID + ".yaml"
		_, insertErr := st.pool.Exec(ctx, `INSERT INTO git_projected_documents(binding_id,generation,path,application_id,source_revision,config_revision,blob_id,content_sha256,raw,parsed,valid,diagnostics,schema_version,parser_version,indexed_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,'app: valid',$9,true,'[]','kuberploy.io/v1alpha1','appconfig.v1',$10)`, bindingID, generation, path, applicationID, sourceRevision, configRevision,
			strings.Repeat("d", 40), "sha256:"+strings.Repeat("e", 64), parsed, indexedAt)
		if insertErr != nil {
			t.Fatal(insertErr)
		}
	}
	insertDocument(1, strings.Repeat("f", 40), configRevisionA, digestA, now)
	if _, err = st.pool.Exec(ctx, `INSERT INTO argo_application_observations(deployment_id,application_id,project_id,environment_id,argo_uid,argo_namespace,argo_name,destination_namespace,desired_revision,observed_revision,sync_status,health_status,operation_phase,message,resources,observed_at,updated_at)
		VALUES($1,$2,$3,$4,$5,'argocd',$6,$7,$8,$8,'synced','healthy','succeeded','','[]',$9,$9)`, deploymentID, applicationID, projectID, environmentID, id.New(), "kp-d-"+deploymentID, "kp-rp-"+environmentID, configRevisionA, now); err != nil {
		t.Fatal(err)
	}
	target, err := st.PutRegistryTarget(ctx, domain.RegistryTarget{ID: targetID, Name: "registry-protection", Mode: domain.RegistryTargetManaged, Endpoint: "https://registry.integration.test", RepositoryPrefix: "integration", CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.PutServiceRegistryPolicy(ctx, registry.DefaultPolicy(target.ID, applicationID, repository, now)); err != nil {
		t.Fatal(err)
	}

	assertProtection := func(authority domain.RegistryAuthority, complete bool, wantDigest string) {
		t.Helper()
		var gotComplete bool
		if err = st.pool.QueryRow(ctx, `SELECT complete FROM registry_authority_observations WHERE registry_target_id=$1 AND service_id=$2 AND authority=$3`, targetID, applicationID, authority).Scan(&gotComplete); err != nil {
			t.Fatal(err)
		}
		if gotComplete != complete {
			t.Fatalf("authority %s complete=%t want=%t", authority, gotComplete, complete)
		}
		if wantDigest == "" {
			return
		}
		var digest string
		if err = st.pool.QueryRow(ctx, `SELECT digest FROM registry_artifact_references WHERE registry_target_id=$1 AND service_id=$2 AND kind=$3`, targetID, applicationID, map[domain.RegistryAuthority]domain.RegistryArtifactReferenceKind{
			domain.RegistryAuthorityGitIntent: domain.RegistryReferenceCurrentGitIntent, domain.RegistryAuthorityRuntime: domain.RegistryReferenceObservedRunning, domain.RegistryAuthorityOperations: domain.RegistryReferenceActiveOperation,
		}[authority]).Scan(&digest); err != nil {
			t.Fatal(err)
		}
		if digest != wantDigest {
			t.Fatalf("authority %s digest=%s want=%s", authority, digest, wantDigest)
		}
	}

	if err = st.RefreshRegistryProtection(ctx, targetID, applicationID, now, true); err != nil {
		t.Fatal(err)
	}
	assertProtection(domain.RegistryAuthorityGitIntent, true, digestA)
	assertProtection(domain.RegistryAuthorityRuntime, true, digestA)
	assertProtection(domain.RegistryAuthorityOperations, true, digestC)

	secondAt := now.Add(time.Minute)
	if _, err = st.pool.Exec(ctx, `INSERT INTO git_projection_generations(binding_id,generation,head_revision,parser_version,state,started_at,activated_at) VALUES($1,2,$2,'appconfig.v1','active',$3,$3)`, bindingID, strings.Repeat("e", 40), secondAt); err != nil {
		t.Fatal(err)
	}
	insertDocument(2, strings.Repeat("e", 40), configRevisionB, digestB, secondAt)
	if _, err = st.pool.Exec(ctx, `UPDATE git_repository_bindings SET target_head_revision=$2,indexed_revision=$2,projection_generation=2,target_head_observed_at=$3,indexed_at=$3,updated_at=$3 WHERE id=$1`, bindingID, strings.Repeat("e", 40), secondAt); err != nil {
		t.Fatal(err)
	}
	if err = st.RefreshRegistryProtection(ctx, targetID, applicationID, secondAt, false); err != nil {
		t.Fatal(err)
	}
	assertProtection(domain.RegistryAuthorityGitIntent, true, digestB)
	assertProtection(domain.RegistryAuthorityRuntime, true, digestA)
	assertProtection(domain.RegistryAuthorityOperations, true, digestC)

	// A dependency-readiness outage can make an otherwise parseable current
	// AppConfig document invalid while the Git head remains unchanged. The
	// safety poll will not manufacture a new generation for that same head.
	// Cleanup protection must still preserve the exact immutable image parsed
	// from the current Git document; validity continues to gate deployment, not
	// this protection-only, fail-safe root.
	if _, err = st.pool.Exec(ctx, `UPDATE git_projected_documents SET valid=false,
		diagnostics='[{"code":"TraefikRuntimeUnobserved"}]'
		WHERE binding_id=$1 AND generation=2 AND application_id=$2`, bindingID, applicationID); err != nil {
		t.Fatal(err)
	}
	if err = st.RefreshRegistryProtection(ctx, targetID, applicationID, secondAt.Add(time.Second), true); err != nil {
		t.Fatal(err)
	}
	assertProtection(domain.RegistryAuthorityGitIntent, true, digestB)

	if _, err = st.pool.Exec(ctx, `UPDATE operations SET status='succeeded',finished_at=$2,updated_at=$2 WHERE id=$1`, activeOperationID, secondAt); err != nil {
		t.Fatal(err)
	}
	if _, err = st.pool.Exec(ctx, `UPDATE argo_application_observations SET observed_at=$2,updated_at=$2 WHERE deployment_id=$1`, deploymentID, secondAt.Add(-16*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err = st.RefreshRegistryProtection(ctx, targetID, applicationID, secondAt, false); err != nil {
		t.Fatal(err)
	}
	assertProtection(domain.RegistryAuthorityRuntime, false, "")
	assertProtection(domain.RegistryAuthorityOperations, true, "")
	var activeOperationReferences int
	if err = st.pool.QueryRow(ctx, `SELECT count(*) FROM registry_artifact_references WHERE registry_target_id=$1 AND service_id=$2 AND kind='active-operation'`, targetID, applicationID).Scan(&activeOperationReferences); err != nil {
		t.Fatal(err)
	}
	if activeOperationReferences != 0 {
		t.Fatalf("terminal operation retained %d active roots", activeOperationReferences)
	}
}
