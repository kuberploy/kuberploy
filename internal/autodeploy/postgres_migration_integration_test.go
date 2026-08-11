package autodeploy_test

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
	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/autodeploy"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitops"
	"github.com/kuberploy/kuberploy/internal/id"
	base "github.com/kuberploy/kuberploy/internal/store"
	storepostgres "github.com/kuberploy/kuberploy/internal/store/postgres"
)

func TestAutoDeployMigrationRejectsDirectAuthoritySubstitution(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL")
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
	creator, actor, project, environment, application, otherApplication, deployment, operation := id.New(), id.New(), id.New(), id.New(), id.New(), id.New(), id.New(), id.New()
	installation, repository, registry, definition, attempt, policy := id.New(), id.New(), id.New(), id.New(), id.New(), id.New()
	seed := now.UnixNano() & 0x3fffffffffffffff
	claimHalf := strings.ReplaceAll(id.New(), "-", "")
	claimKey := claimHalf + claimHalf
	digest := "sha256:" + strings.Repeat("a", 64)
	etag := `"sha256:` + strings.Repeat("b", 64) + `"`
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id,login,role,issuer,subject,created_at) VALUES($1,$2,'platform-admin',$2,$2,$3),($4,$5,'developer',$5,$5,$3)`, []any{creator, "ad-creator-" + creator[:8], now, actor, "ad-actor-" + actor[:8]}},
		{`INSERT INTO projects(id,name,slug,created_at) VALUES($1,'Auto deploy',$2,$3)`, []any{project, "ad-" + project[:8], now}},
		{`INSERT INTO access_grants(id,subject_user_id,role,scope_type,scope_id,source,created_by,created_at) VALUES($1,$2,'platform-admin','platform','platform','bootstrap',$2,$3),($4,$5,'developer','project',$6,'service-account',$2,$3)`, []any{id.New(), creator, now, id.New(), actor, project}},
		{`INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at) VALUES($1,$2,'Auto','auto',$3,$4,$5)`, []any{environment, project, "ad-" + environment[:8], "ad-argo-" + environment[:8], now}},
		{`INSERT INTO applications(id,project_id,name,slug,created_at) VALUES($1,$2,'App','app',$4),($3,$2,'Other','other',$4)`, []any{application, project, otherApplication, now}},
		{`INSERT INTO service_accounts(id,project_id,name,role,created_by,created_at) VALUES($1,$2,'Auto deploy','developer',$3,$4)`, []any{actor, project, creator, now}},
		{`INSERT INTO github_installations(id,github_installation_id,account_login,account_type,owner_user_id,visibility,repository_selection,repository_count,github_app_id,github_account_id,lifecycle,permissions,last_verified_at,created_at,updated_at) VALUES($1,$2,'kuberploy','Organization',$3,'private','selected',1,$4,$5,'active','{"metadata":"read","contents":"read"}'::jsonb,$6,$6,$6)`, []any{installation, seed + 1, creator, seed + 2, seed + 3, now}},
		{`INSERT INTO github_repositories(id,installation_id,github_repository_id,github_owner_id,owner_login,name,lifecycle,last_verified_at,created_at,updated_at) VALUES($1,$2,$3,$4,'kuberploy','auto-repo','active',$5,$5,$5)`, []any{repository, installation, seed + 4, seed + 3, now}},
		{`INSERT INTO registry_targets(id,name,mode,endpoint,repository_prefix,created_at,updated_at) VALUES($1,$2,'managed','registry.test','apps',$3,$3)`, []any{registry, "ad-" + registry[:8], now}},
		{`INSERT INTO build_definitions(id,project_id,service_id,installation_id,repository_id,registry_target_id,trigger_ref,spec,definition_digest,generation,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,'refs/heads/main','{}',$7,1,$8,$8)`, []any{definition, project, application, installation, repository, registry, digest, now}},
		{`INSERT INTO operations(id,kind,status,target_type,target_id,request_id,generation,created_at,updated_at) VALUES($1,'deployment.git-write','succeeded','deployment',$2,'fixture',1,$3,$3)`, []any{operation, deployment, now}},
		{`INSERT INTO deployments(id,environment_id,application_id,image,replicas,port,environment,state,operation_id,generation,runtime,created_at,updated_at) VALUES($1,$2,$3,$4,1,8080,'{}','ready',$5,1,'{}',$6,$6)`, []any{deployment, environment, application, "registry.test/apps/app@" + digest, operation, now}},
		{`INSERT INTO github_one_time_claims(kind,claim_key,retain_until,permanent,created_at) VALUES('github-delivery',$1,$2,true,$3)`, []any{claimKey, now.Add(time.Hour), now}},
		{`INSERT INTO github_webhook_receipts(claim_key,github_app_id,github_installation_id,delivery_id,event,body_sha256,repository_id,git_ref,state,received_at,completed_at,updated_at) VALUES($1,$2,$3,$4,'push',$5,$6,'refs/heads/main','enqueued',$7,$7,$7)`, []any{claimKey, seed + 2, seed + 1, id.New(), "sha256:" + strings.Repeat("d", 64), seed + 4, now}},
		{`INSERT INTO build_attempts(id,definition_id,delivery_claim_key,project_id,service_id,commit_sha,git_ref,generation,definition_digest,plan_request,checkout_request,input_digest,registry_mode,state,max_attempts,job_namespace,job_name,cache_candidate,result,completed_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,'refs/heads/main',1,$7,'{}','{}',$8,'managed','succeeded',1,'builder','job','cache','{}',$9,$9,$9)`, []any{attempt, definition, claimKey, project, application, strings.Repeat("e", 40), digest, "sha256:" + strings.Repeat("f", 64), now}},
		{`INSERT INTO registry_releases(id,registry_target_id,service_id,repository,root_digest,created_at,succeeded_at) VALUES($1,$2,$3,'apps/app',$4,$5,$5)`, []any{attempt, registry, application, digest, now}},
	}
	for _, statement := range statements {
		if _, err = pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("fixture: %v", err)
		}
	}
	management, err := storepostgres.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer management.Close()
	var policiesBefore, commandsBefore, auditsBefore int
	if err = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM auto_deploy_policies),(SELECT count(*) FROM auto_deploy_policy_commands),(SELECT count(*) FROM audit_events WHERE target_type='auto-deploy-policy')`).
		Scan(&policiesBefore, &commandsBefore, &auditsBefore); err != nil {
		t.Fatal(err)
	}
	service := &autodeploy.PolicyService{Catalog: management, Store: management, NewID: func() (string, error) { return id.New(), nil }}
	_, _, _, err = service.Create(ctx, creator, autodeploy.CreatePolicyInput{ExpectedApplicationID: otherApplication, BuildDefinitionID: definition,
		EnvironmentID: environment, TemplateDeploymentID: deployment, ServiceActorID: actor, Enabled: true,
		IdempotencyKey: "cross-application-create", RequestDigest: "sha256:" + strings.Repeat("1", 64), RequestID: "cross-application-create"})
	if !errors.Is(err, autodeploy.ErrConflict) {
		t.Fatalf("cross-application path create err=%v", err)
	}
	var policiesAfter, commandsAfter, auditsAfter int
	if err = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM auto_deploy_policies),(SELECT count(*) FROM auto_deploy_policy_commands),(SELECT count(*) FROM audit_events WHERE target_type='auto-deploy-policy')`).
		Scan(&policiesAfter, &commandsAfter, &auditsAfter); err != nil {
		t.Fatal(err)
	}
	if policiesAfter != policiesBefore || commandsAfter != commandsBefore || auditsAfter != auditsBefore {
		t.Fatalf("cross-application create wrote rows: policy %d→%d command %d→%d audit %d→%d", policiesBefore, policiesAfter, commandsBefore, commandsAfter, auditsBefore, auditsAfter)
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO auto_deploy_policies(id,build_definition_id,project_id,application_id,environment_id,current_revision,created_by,created_at) VALUES($1,$2,$3,$4,$5,1,$6,$7)`, policy, definition, project, application, environment, creator, now); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO auto_deploy_policy_revisions(policy_id,revision,enabled,source_deployment_id,source_deployment_generation,source_config_etag,config_intent,template_digest,service_actor_id,created_by,created_at) VALUES($1,1,true,$2,1,$3,$4,$5,$6,$7,$8)`, policy, deployment, etag, []byte(`{}`), digest, actor, creator, now); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	idem := autodeploy.IdempotencyKey(policy, 1, attempt)
	if _, err = pool.Exec(ctx, `INSERT INTO auto_deploy_runs(attempt_id,policy_id,policy_revision,definition_id,definition_digest,release_id,template_digest,source_deployment_id,source_deployment_generation,source_config_etag,idempotency_key,state,attempts,available_at,lease_epoch,created_at,updated_at) VALUES($1,$2,1,$3,$4,$1,$5,$6,1,$7,$8,'pending',0,$9,0,$9,$9)`, attempt, policy, definition, digest, digest, deployment, etag, idem, now); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `UPDATE auto_deploy_runs SET definition_digest=$3 WHERE attempt_id=$1 AND policy_id=$2`, attempt, policy, "sha256:"+strings.Repeat("0", 64))
	assertPGCode(t, err, "23514")
	_, err = pool.Exec(ctx, `UPDATE auto_deploy_runs SET state='submitted',operation_id=$3,updated_at=$4,completed_at=$4 WHERE attempt_id=$1 AND policy_id=$2`, attempt, policy, operation, now.Add(time.Second))
	assertPGCode(t, err, "23514")
	_, err = pool.Exec(ctx, `UPDATE auto_deploy_runs SET state='failed',updated_at=$3,completed_at=$3 WHERE attempt_id=$1 AND policy_id=$2`, attempt, policy, now.Add(time.Second))
	assertPGCode(t, err, "23514")
	_, err = pool.Exec(ctx, `UPDATE auto_deploy_policy_revisions SET source_config_etag='weak' WHERE policy_id=$1 AND revision=1`, policy)
	assertPGCode(t, err, "23514")

	acquiredAt := now.Add(2 * time.Second)
	leaseUntil := now.Add(10 * time.Second)
	if _, err = pool.Exec(ctx, `UPDATE auto_deploy_runs SET state='processing',attempts=1,lease_owner='worker-one',lease_until=$3,lease_epoch=1,updated_at=$4 WHERE attempt_id=$1 AND policy_id=$2`, attempt, policy, leaseUntil, acquiredAt); err != nil {
		t.Fatalf("acquire fixture run: %v", err)
	}
	expiredAt := leaseUntil.Add(time.Second)

	_, err = pool.Exec(ctx, `UPDATE auto_deploy_runs SET lease_until=$3,updated_at=$4 WHERE attempt_id=$1 AND policy_id=$2`, attempt, policy, expiredAt.Add(time.Minute), expiredAt)
	assertPGCode(t, err, "23514")
	_, err = pool.Exec(ctx, `UPDATE auto_deploy_runs SET state='pending',available_at=$3,lease_owner=NULL,lease_until=NULL,failure_code='retryable',updated_at=$4 WHERE attempt_id=$1 AND policy_id=$2`, attempt, policy, expiredAt.Add(time.Minute), expiredAt)
	assertPGCode(t, err, "23514")
	_, err = pool.Exec(ctx, `UPDATE auto_deploy_runs SET state='submitted',lease_owner=NULL,lease_until=NULL,operation_id=$3,deployment_id=$4,completed_at=$5,updated_at=$5 WHERE attempt_id=$1 AND policy_id=$2`, attempt, policy, operation, deployment, expiredAt)
	assertPGCode(t, err, "23514")
	_, err = pool.Exec(ctx, `UPDATE auto_deploy_runs SET state='failed',lease_owner=NULL,lease_until=NULL,failure_code='terminal',completed_at=$3,updated_at=$3 WHERE attempt_id=$1 AND policy_id=$2`, attempt, policy, expiredAt)
	assertPGCode(t, err, "23514")

	reclaimedUntil := expiredAt.Add(time.Minute)
	if _, err = pool.Exec(ctx, `UPDATE auto_deploy_runs SET attempts=2,lease_owner='worker-two',lease_until=$3,lease_epoch=2,failure_code='',updated_at=$4 WHERE attempt_id=$1 AND policy_id=$2`, attempt, policy, reclaimedUntil, expiredAt); err != nil {
		t.Fatalf("reclaim expired run: %v", err)
	}
	var state, owner string
	var attempts, epoch int64
	if err = pool.QueryRow(ctx, `SELECT state,attempts,lease_owner,lease_epoch FROM auto_deploy_runs WHERE attempt_id=$1 AND policy_id=$2`, attempt, policy).Scan(&state, &attempts, &owner, &epoch); err != nil {
		t.Fatal(err)
	}
	if state != "processing" || attempts != 2 || owner != "worker-two" || epoch != 2 {
		t.Fatalf("unexpected reclaimed run: state=%s attempts=%d owner=%s epoch=%d", state, attempts, owner, epoch)
	}

	status, err := management.PolicyForActor(ctx, creator, policy)
	if err != nil || status.Policy.ID != policy || status.CurrentRevision.Revision != 1 {
		t.Fatalf("policy status=%#v err=%v", status, err)
	}
	if err = management.AuthorizeAutoDeploy(ctx, actor, domain.AutomationScopeAppEdit, project, environment, application); err != nil {
		t.Fatalf("service actor was not freshly authorized: %v", err)
	}
	raw, err := gitops.RenderAppConfig(domain.Project{ID: project}, domain.Environment{ID: environment},
		domain.Application{ID: application, Slug: "app"}, domain.Deployment{ID: deployment, ApplicationID: application,
			EnvironmentID: environment, Generation: 1, Image: "registry.test/apps/app@" + digest,
			Runtime: domain.NormalizeWorkloadRuntime(domain.WorkloadRuntime{Replicas: 1, Ports: []domain.WorkloadPort{{Name: "http", ContainerPort: 8080}}})})
	if err != nil {
		t.Fatal(err)
	}
	intent, templateDigest, diagnostics := appconfig.AutoDeployIntentTemplate(raw)
	if len(diagnostics) != 0 {
		t.Fatalf("template diagnostics=%#v", diagnostics)
	}
	revision := autodeploy.Revision{PolicyID: policy, Revision: 2, Enabled: false,
		Template:       autodeploy.Template{SourceDeploymentID: deployment, SourceDeploymentGeneration: 1, SourceConfigETag: etag, ConfigIntent: intent},
		TemplateDigest: templateDigest, ServiceActorID: actor, CreatedBy: creator, CreatedAt: now.Add(time.Minute)}
	updated, storedRevision, replay, err := management.RevisePolicy(ctx, status.Policy, revision, "disable-policy", "sha256:"+strings.Repeat("9", 64), "request-disable-policy")
	if err != nil || replay || updated.CurrentRevision != 2 || storedRevision.Enabled {
		t.Fatalf("revise policy=%#v revision=%#v replay=%v err=%v", updated, storedRevision, replay, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE service_accounts SET disabled_at=$2 WHERE id=$1`, actor, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE deployments SET config_raw=$2,config_etag=$3,config_version=config_version+1,generation=generation+1,updated_at=$4 WHERE id=$1`,
		deployment, []byte(`{"apiVersion":"changed"}`), `"sha256:`+strings.Repeat("7", 64)+`"`, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	_, _, replay, err = management.RevisePolicy(ctx, status.Policy, revision, "disable-policy", "sha256:"+strings.Repeat("9", 64), "request-disable-policy-replay")
	if err != nil || !replay {
		t.Fatalf("revision replay after disabled SA/config drift=%v err=%v", replay, err)
	}
	if _, _, _, err = management.RevisePolicy(ctx, status.Policy, revision, "disable-policy", "sha256:"+strings.Repeat("8", 64), "request-disable-policy-conflict"); !errors.Is(err, base.ErrIdempotencyConflict) {
		t.Fatalf("changed accepted revision digest err=%v", err)
	}
	revision3 := revision
	revision3.Revision, revision3.Enabled, revision3.CreatedAt = 3, true, now.Add(3*time.Minute)
	if _, _, _, err = management.RevisePolicy(ctx, updated, revision3, "disabled-service-actor", "sha256:"+strings.Repeat("6", 64), "request-disabled-service-actor"); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("new revision selected disabled service actor err=%v", err)
	}
	revisions, err := management.PolicyRevisionsForActor(ctx, creator, policy, 10)
	if err != nil || len(revisions) != 2 || revisions[0].Revision != 2 || revisions[0].Enabled {
		t.Fatalf("revision history=%#v err=%v", revisions, err)
	}
}

func assertPGCode(t *testing.T, err error, want string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != want {
		t.Fatalf("want SQLSTATE %s, got %v", want, err)
	}
}
