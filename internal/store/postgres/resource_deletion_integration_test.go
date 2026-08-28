package postgres

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/builds"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/environmentfoundation"
	"github.com/kuberploy/kuberploy/internal/id"
	base "github.com/kuberploy/kuberploy/internal/store"
	"github.com/kuberploy/kuberploy/internal/testdb"
)

func TestPostgreSQLApplicationAndEnvironmentDeletion(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := t.Context()
	store, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = testdb.ApplyMigrations(ctx, store.pool); err != nil {
		t.Fatal(err)
	}

	suffix := strings.ReplaceAll(id.New(), "-", "")[:12]
	actorID := id.New()
	now := time.Now().UTC()
	if _, err = store.pool.Exec(ctx, `INSERT INTO users(id,display_name,role,issuer,subject,grant_revision,created_at) VALUES($1,$2,'platform-admin','resource-delete-test',$3,1,$4)`, actorID, "delete-admin-"+suffix, actorID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO access_grants(id,subject_user_id,role,scope_type,scope_id,source,created_by,created_at) VALUES($1,$2,'platform-admin','platform','platform','bootstrap',$2,$3)`, id.New(), actorID, now); err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(ctx, actorID, "delete-project-"+suffix, "delete-project-"+suffix, domain.CreateProject{Name: "Delete project", Slug: "delete-" + suffix})
	if err != nil {
		t.Fatal(err)
	}
	environment, err := store.CreateEnvironment(ctx, actorID, "delete-environment-"+suffix, "delete-environment-"+suffix, domain.CreateEnvironment{ProjectID: project.Value.ID, Name: "Disposable environment", Slug: "disposable"})
	if err != nil {
		t.Fatal(err)
	}
	bindingID, intentID := id.New(), id.New()
	createdAt := now.Add(-time.Minute)
	head, committed := strings.Repeat("a", 40), strings.Repeat("b", 40)
	manifest := []byte("apiVersion: v1\nkind: Namespace\n")
	manifestSum := sha256.Sum256(manifest)
	manifestDigest := "sha256:" + hex.EncodeToString(manifestSum[:])
	if _, err = store.pool.Exec(ctx, `INSERT INTO git_repository_bindings(
		id,kind,scope_id,provider,installation_id,repository_id,repository_owner,repository_name,target_ref,path_prefix,
		credential_secret_name,state,target_head_revision,indexed_revision,projection_generation,parser_version,
		target_head_observed_at,indexed_at,created_at,updated_at,credential_mode)
		VALUES($1,'platform',$1,'github',1,1,'kuberploy','fixture','refs/heads/main','platform','',
		'ready',$2,$2,1,'test',$3,$3,$3,$3,'github-app')`, bindingID, committed, now); err != nil {
		t.Fatal(err)
	}
	environmentBindingID := id.New()
	if _, err = store.pool.Exec(ctx, `INSERT INTO git_repository_bindings(
		id,kind,scope_id,project_id,environment_id,provider,installation_id,repository_id,repository_owner,repository_name,target_ref,path_prefix,
		credential_secret_name,state,target_head_revision,indexed_revision,projection_generation,parser_version,target_head_observed_at,indexed_at,
		created_at,updated_at,credential_mode)
		VALUES($1,'environment',$2,$3,$2,'github',1,1,'kuberploy','fixture','refs/heads/main',$4,'','ready',$5,$5,1,'test',$6,$6,$6,$6,'github-app')`,
		environmentBindingID, environment.Value.ID, project.Value.ID,
		"tenants/"+project.Value.ID+"/environments/"+environment.Value.ID, committed, now); err != nil {
		t.Fatal(err)
	}
	deliveryHash := "sha256:" + strings.Repeat("1", 64)
	if _, err = store.pool.Exec(ctx, `INSERT INTO git_projection_push_wakes(
		delivery_hash,github_app_id,installation_id,repository_id,target_ref,after_commit,received_at)
		VALUES($1,1,1,1,'refs/heads/main',$2,$3)`, deliveryHash, committed, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO git_projection_push_wake_targets(delivery_hash,binding_id,wake_generation)
		VALUES($1,$2,1)`, deliveryHash, environmentBindingID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO git_verified_head_observations(
		binding_id,provider,installation_id,repository_id,repository_owner,repository_name,target_ref,commit_revision,source,provider_request,observed_at)
		VALUES($1,'github',1,1,'kuberploy','fixture','refs/heads/main',$2,'verified-webhook','resource-delete-test',$3)`,
		environmentBindingID, committed, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO git_projection_generations(
		binding_id,generation,head_revision,parser_version,state,started_at,activated_at)
		VALUES($1,1,$2,'test','active',$3,$3)`, environmentBindingID, committed, now); err != nil {
		t.Fatal(err)
	}
	desiredStateCommandID, materializationID := id.New(), id.New()
	gitOpsTx, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gitOpsTx.Rollback(ctx) //nolint:errcheck
	if _, err = gitOpsTx.Exec(ctx, `ALTER TABLE argo_desired_state_commands DISABLE TRIGGER USER;
		ALTER TABLE argo_desired_state_materialization_receipts DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err = gitOpsTx.Exec(ctx, `INSERT INTO argo_desired_state_commands(
		id,generation,project_id,environment_id,platform_binding_id,environment_binding_id,
		platform_target_ref,environment_target_ref,environment_revision,environment_generation,path,
		argo_namespace,destination_namespace,argo_project,base_revision,write_base_revision,write_base_observed_at,
		precondition,expected_etag,policy_digest,catalog_digest,chart_repository,chart_name,chart_version,
		chart_digest,renderer_image,chart_digest_enforcement,content,content_sha256,message,state,
		committed_revision,committed_at,verified_at,next_attempt_at,created_at,updated_at,completed_at)
		VALUES($1,1,$2,$3,$4,$5,'refs/heads/main','refs/heads/main',$6,1,$7,
		'argocd',$8,$9,$10,$10,$11,'create-if-absent','',$12,$13,
		'oci://ghcr.io/kuberploy/charts','kuberploy-runtime','1.2.3',$14,$15,'native-oci-digest-v1',
		$16,$17,'resource deletion fixture','verified',$6,$11,$11,$11,$11,$11,$11)`,
		desiredStateCommandID, project.Value.ID, environment.Value.ID, bindingID, environmentBindingID,
		committed, "platform/argocd/environments/"+environment.Value.ID+".yaml", environment.Value.Namespace,
		environment.Value.ArgoProject, head, now, "sha256:"+strings.Repeat("f", 64),
		"sha256:"+strings.Repeat("c", 64), "sha256:"+strings.Repeat("d", 64),
		"ghcr.io/kuberploy/runtime-renderer@sha256:"+strings.Repeat("e", 64), manifest, manifestDigest); err != nil {
		t.Fatal(err)
	}
	if _, err = gitOpsTx.Exec(ctx, `INSERT INTO argo_desired_state_materialization_receipts(
		id,environment_binding_id,environment_revision,environment_generation,project_id,environment_id,
		platform_binding_id,platform_target_ref,environment_target_ref,desired_state_command_id,
		desired_state_generation,desired_state_revision,desired_state_content_sha256,policy_digest,catalog_digest,
		chart_repository,chart_name,chart_version,chart_digest,renderer_image,chart_digest_enforcement,created_at)
		VALUES($1,$2,$3,1,$4,$5,$6,'refs/heads/main','refs/heads/main',$7,1,$3,$8,$9,$10,
		'oci://ghcr.io/kuberploy/charts','kuberploy-runtime','1.2.3',$11,$12,'native-oci-digest-v1',$13)`,
		materializationID, environmentBindingID, committed, project.Value.ID, environment.Value.ID, bindingID,
		desiredStateCommandID, manifestDigest, "sha256:"+strings.Repeat("f", 64),
		"sha256:"+strings.Repeat("c", 64), "sha256:"+strings.Repeat("d", 64),
		"ghcr.io/kuberploy/runtime-renderer@sha256:"+strings.Repeat("e", 64), now); err != nil {
		t.Fatal(err)
	}
	if _, err = gitOpsTx.Exec(ctx, `ALTER TABLE argo_desired_state_materialization_receipts ENABLE TRIGGER USER;
		ALTER TABLE argo_desired_state_commands ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if err = gitOpsTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO environment_foundation_intents(
		id,environment_id,project_id,namespace,argo_project,platform_binding_id,target_ref,planned_head_revision,
		binding_generation,profile_digest,publisher_config_digest,publisher_contract,publisher_policy,manifest_path,
		manifest,manifest_digest,intent_digest,commit_trailer,state,active,next_attempt_at,attempts,consecutive_failures,
		last_failure_code,lease_epoch,write_base_revision,write_base_observed_at,committed_revision,
		committed_parent_revision,provider_request,published_at,completed_at,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,'refs/heads/main',$7,1,$8,$9,'environment-foundation-protected-git.v1',
		'platform-protected-git.v1',$10,$11,$12,$13,$14,'ready',true,$15,1,0,'',0,$7,$15,$16,$7,
		'test-provider-request',$15,$15,$17,$15)`, intentID, environment.Value.ID, project.Value.ID,
		environment.Value.Namespace, environment.Value.ArgoProject, bindingID, head,
		"sha256:"+strings.Repeat("c", 64), "sha256:"+strings.Repeat("d", 64),
		"platform/argocd/foundations/"+environment.Value.ID+".yaml", manifest, manifestDigest,
		"sha256:"+strings.Repeat("e", 64), "Kuberploy-Environment-Foundation-Intent: "+intentID,
		now, committed, createdAt); err != nil {
		t.Fatal(err)
	}
	application, err := store.CreateApplication(ctx, actorID, "delete-application-"+suffix, "delete-application-"+suffix, domain.CreateApplication{ProjectID: project.Value.ID, EnvironmentID: environment.Value.ID, Name: "Disposable App", Slug: "disposable"})
	if err != nil {
		t.Fatal(err)
	}
	deletedBindingID := id.New()
	if _, err = store.pool.Exec(ctx, `INSERT INTO secret_bindings(
		id,organization_id,project_id,environment_id,application_id,target_namespace,name,provider,state,
		active_version,created_by,created_at,updated_at,delete_started_at,deleted_at,purpose)
		VALUES($1,NULL,$2,$3,$4,$5,'deleted-secret','sealed-secrets','deleted',0,$6,$7,$7,$7,$7,'runtime-secret')`,
		deletedBindingID, project.Value.ID, environment.Value.ID, application.Value.ID, environment.Value.Namespace, actorID, now); err != nil {
		t.Fatal(err)
	}
	installationID, repositoryID, registryID, definitionID := id.New(), id.New(), id.New(), id.New()
	providerID := now.UnixNano() & 0x3fffffffffffffff
	if _, err = store.pool.Exec(ctx, `INSERT INTO github_installations(id,github_installation_id,account_login,account_type,owner_user_id,visibility,repository_selection,repository_count,github_app_id,github_account_id,lifecycle,permissions,last_verified_at,created_at,updated_at)
		VALUES($1,$2,'kuberploy','Organization',$3,'private','selected',1,$4,$5,'active','{"metadata":"read","contents":"read"}'::jsonb,$6,$6,$6)`, installationID, providerID, actorID, providerID+1, providerID+2, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO github_repositories(id,installation_id,github_repository_id,github_owner_id,owner_login,name,lifecycle,last_verified_at,created_at,updated_at)
		VALUES($1,$2,$3,$4,'kuberploy','delete-fixture','active',$5,$5,$5)`, repositoryID, installationID, providerID+3, providerID+2, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO registry_targets(id,name,mode,endpoint,repository_prefix,pull_credential_ref,created_at,updated_at) VALUES($1,$2,'managed','registry.test','apps','registry-auth',$3,$3)`, registryID, "delete-"+suffix, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO build_definitions(id,project_id,service_id,installation_id,repository_id,registry_target_id,trigger_ref,spec,definition_digest,generation,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,'refs/heads/main','{}',$7,1,$8,$8)`, definitionID, project.Value.ID, application.Value.ID, installationID, repositoryID, registryID, "sha256:"+strings.Repeat("9", 64), now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DeleteApplication(ctx, actorID, application.Value.ID, application.Value.Name, "delete-app-build-blocked-"+suffix, "delete-app-build-blocked", "request-app-build-blocked-"+suffix); !errors.Is(err, base.ErrApplicationDeletionBlocked) {
		t.Fatalf("source-configured App deletion err=%v", err)
	}
	buildStore, err := builds.NewPostgreSQLStore(store.pool)
	if err != nil {
		t.Fatal(err)
	}
	if replay, disconnectErr := buildStore.DeleteDefinition(ctx, actorID, application.Value.ID, definitionID, "disconnect-before-delete-"+suffix, "sha256:"+strings.Repeat("8", 64), "disconnect-before-app-delete-"+suffix, now); disconnectErr != nil || replay {
		t.Fatalf("disconnect before App deletion replay=%v err=%v", replay, disconnectErr)
	}
	if _, err = store.DeleteApplication(ctx, actorID, application.Value.ID, "Wrong", "delete-app-wrong-"+suffix, "wrong", "request-wrong-"+suffix); !errors.Is(err, base.ErrDeletionConfirmation) {
		t.Fatalf("wrong confirmation err=%v", err)
	}
	replay, err := store.DeleteApplication(ctx, actorID, application.Value.ID, application.Value.Name, "delete-app-"+suffix, "delete-app", "request-app-"+suffix)
	if err != nil || replay {
		t.Fatalf("delete App replay=%t err=%v", replay, err)
	}
	replay, err = store.DeleteApplication(ctx, actorID, application.Value.ID, application.Value.Name, "delete-app-"+suffix, "delete-app", "request-app-replay-"+suffix)
	if err != nil || !replay {
		t.Fatalf("delete App replay replay=%t err=%v", replay, err)
	}
	if _, err = store.GetApplication(ctx, application.Value.ID); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("deleted App err=%v", err)
	}
	var deletedBindingExists bool
	if err = store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM secret_bindings WHERE id=$1)`, deletedBindingID).Scan(&deletedBindingExists); err != nil || deletedBindingExists {
		t.Fatalf("deleted secret tombstone remained after App deletion exists=%t err=%v", deletedBindingExists, err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO runtime_registry_pull_artifacts(
		environment_id,namespace,registry_target_id,pull_credential_ref,profile_name,profile_revision,
		secret_name,active,runtime_state,next_observation_at,created_at,updated_at)
		VALUES($1,$2,$3,'registry-auth','runtime',1,$4,true,'awaiting',$5,$5,$5)`,
		environment.Value.ID, environment.Value.Namespace, registryID,
		"kuberploy-pull-"+strings.Repeat("a", 24), now); err != nil {
		t.Fatal(err)
	}

	replay, err = store.DeleteEnvironment(ctx, actorID, environment.Value.ID, environment.Value.Name, "delete-env-"+suffix, "delete-env", "request-env-"+suffix)
	if err != nil || replay {
		t.Fatalf("delete Environment replay=%t err=%v", replay, err)
	}
	if _, err = store.GetEnvironment(ctx, environment.Value.ID); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("deleted Environment err=%v", err)
	}
	var pullArtifacts int
	if err = store.pool.QueryRow(ctx, `SELECT count(*) FROM runtime_registry_pull_artifacts WHERE environment_id=$1`, environment.Value.ID).Scan(&pullArtifacts); err != nil || pullArtifacts != 0 {
		t.Fatalf("deleted Environment retained runtime registry pull artifacts count=%d err=%v", pullArtifacts, err)
	}
	var environmentBindingChildren int
	if err = store.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM git_repository_bindings WHERE id=$1) +
		(SELECT count(*) FROM git_projection_push_wake_targets WHERE binding_id=$1) +
		(SELECT count(*) FROM git_verified_head_observations WHERE binding_id=$1) +
		(SELECT count(*) FROM argo_desired_state_commands WHERE environment_binding_id=$1) +
		(SELECT count(*) FROM argo_desired_state_materialization_receipts WHERE environment_binding_id=$1)`, environmentBindingID).Scan(&environmentBindingChildren); err != nil || environmentBindingChildren != 0 {
		t.Fatalf("deleted Environment retained Git projection state count=%d err=%v", environmentBindingChildren, err)
	}
	var deletionState, queuedDigest string
	if err = store.pool.QueryRow(ctx, `SELECT state,expected_manifest_digest FROM environment_foundation_deletions WHERE environment_id=$1`, environment.Value.ID).Scan(&deletionState, &queuedDigest); err != nil {
		t.Fatal(err)
	}
	if deletionState != "pending" || queuedDigest != manifestDigest {
		t.Fatalf("foundation deletion state=%q digest=%q", deletionState, queuedDigest)
	}
	var intentCount int
	if err = store.pool.QueryRow(ctx, `SELECT count(*) FROM environment_foundation_intents WHERE environment_id=$1`, environment.Value.ID).Scan(&intentCount); err != nil || intentCount != 0 {
		t.Fatalf("foundation intent count=%d err=%v", intentCount, err)
	}
	foundationStore, err := environmentfoundation.NewPostgresStore(store.pool)
	if err != nil {
		t.Fatal(err)
	}
	worker := "resource-delete-test:worker"
	lease, found, err := foundationStore.ClaimDeletion(ctx, worker, time.Now().UTC(), time.Minute)
	if err != nil || !found || lease.Deletion.EnvironmentID != environment.Value.ID {
		t.Fatalf("claim foundation cleanup=%#v found=%t err=%v", lease, found, err)
	}
	cleanupRevision := strings.Repeat("f", 40)
	err = foundationStore.RecordDeletionReady(ctx, lease, environmentfoundation.DeletionReceipt{
		OperationID: lease.Deletion.ID, BindingID: lease.Deletion.BindingID, TargetRef: lease.Deletion.TargetRef,
		Path: lease.Deletion.Path, ParentRevision: lease.Deletion.RequiredAncestor, CommittedRevision: cleanupRevision,
		ProviderRequest: "test-foundation-cleanup",
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err = store.pool.QueryRow(ctx, `SELECT state FROM environment_foundation_deletions WHERE environment_id=$1`, environment.Value.ID).Scan(&deletionState); err != nil || deletionState != "ready" {
		t.Fatalf("completed foundation deletion state=%q err=%v", deletionState, err)
	}
	account, err := store.CreateServiceAccount(ctx, actorID, "delete-project-account-"+suffix, "delete-project-account", "request-project-account-"+suffix, domain.CreateServiceAccount{
		ProjectID: project.Value.ID, Name: "Cleanup automation", Role: domain.RoleViewer,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.DisableServiceAccount(ctx, actorID, account.Value.ID, "disable-project-account-"+suffix, "disable-project-account", "request-disable-project-account-"+suffix); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DeleteProject(ctx, actorID, project.Value.ID, "Wrong", "delete-project-wrong-"+suffix, "delete-project-wrong", "request-project-wrong-"+suffix); !errors.Is(err, base.ErrDeletionConfirmation) {
		t.Fatalf("wrong Project confirmation err=%v", err)
	}
	replay, err = store.DeleteProject(ctx, actorID, project.Value.ID, project.Value.Name, "delete-project-"+suffix, "delete-project", "request-project-"+suffix)
	if err != nil || replay {
		t.Fatalf("delete Project replay=%t err=%v", replay, err)
	}
	replay, err = store.DeleteProject(ctx, actorID, project.Value.ID, project.Value.Name, "delete-project-"+suffix, "delete-project", "request-project-replay-"+suffix)
	if err != nil || !replay {
		t.Fatalf("delete Project replay replay=%t err=%v", replay, err)
	}
	if _, err = store.GetProject(ctx, project.Value.ID); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("deleted Project err=%v", err)
	}
	var accountExists bool
	if err = store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)`, account.Value.ID).Scan(&accountExists); err != nil || accountExists {
		t.Fatalf("disabled service-account identity remained after Project deletion exists=%t err=%v", accountExists, err)
	}
}

func TestPostgreSQLResourceDeletionRejectsDeploymentHistory(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := t.Context()
	store, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = testdb.ApplyMigrations(ctx, store.pool); err != nil {
		t.Fatal(err)
	}

	suffix := strings.ReplaceAll(id.New(), "-", "")[:12]
	actorID := id.New()
	now := time.Now().UTC()
	if _, err = store.pool.Exec(ctx, `INSERT INTO users(id,display_name,role,issuer,subject,grant_revision,created_at) VALUES($1,$2,'platform-admin','resource-delete-block-test',$3,1,$4)`, actorID, "delete-block-admin-"+suffix, actorID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO access_grants(id,subject_user_id,role,scope_type,scope_id,source,created_by,created_at) VALUES($1,$2,'platform-admin','platform','platform','bootstrap',$2,$3)`, id.New(), actorID, now); err != nil {
		t.Fatal(err)
	}
	project, _ := store.CreateProject(ctx, actorID, "blocked-project-"+suffix, "blocked-project-"+suffix, domain.CreateProject{Name: "Blocked project", Slug: "blocked-" + suffix})
	environment, _ := store.CreateEnvironment(ctx, actorID, "blocked-environment-"+suffix, "blocked-environment-"+suffix, domain.CreateEnvironment{ProjectID: project.Value.ID, Name: "Production", Slug: "production"})
	application, _ := store.CreateApplication(ctx, actorID, "blocked-application-"+suffix, "blocked-application-"+suffix, domain.CreateApplication{ProjectID: project.Value.ID, Name: "API", Slug: "api"})
	_, _, err = store.CreateDeployment(ctx, actorID, "blocked-deployment-"+suffix, "blocked-deployment-"+suffix, "request-"+suffix, domain.CreateDeployment{
		EnvironmentID: environment.Value.ID, ApplicationID: application.Value.ID,
		Image: "registry.example.test/api@sha256:" + strings.Repeat("a", 64), Replicas: 1, Port: 8080,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.DeleteApplication(ctx, actorID, application.Value.ID, application.Value.Name, "blocked-app-"+suffix, "blocked-app", "request-blocked-app-"+suffix); !errors.Is(err, base.ErrApplicationDeletionBlocked) {
		t.Fatalf("active App deletion err=%v", err)
	}
	if _, err = store.DeleteEnvironment(ctx, actorID, environment.Value.ID, environment.Value.Name, "blocked-env-"+suffix, "blocked-env", "request-blocked-env-"+suffix); !errors.Is(err, base.ErrEnvironmentDeletionBlocked) {
		t.Fatalf("active Environment deletion err=%v", err)
	}
	if _, err = store.DeleteProject(ctx, actorID, project.Value.ID, project.Value.Name, "blocked-project-"+suffix, "blocked-project", "request-blocked-project-"+suffix); !errors.Is(err, base.ErrProjectDeletionBlocked) {
		t.Fatalf("non-empty Project deletion err=%v", err)
	}
}

func TestPostgreSQLEnvironmentDeletionRemovesUnpublishedFailedFoundation(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := t.Context()
	store, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = testdb.ApplyMigrations(ctx, store.pool); err != nil {
		t.Fatal(err)
	}

	suffix := strings.ReplaceAll(id.New(), "-", "")[:12]
	actorID := id.New()
	now := time.Now().UTC()
	if _, err = store.pool.Exec(ctx, `INSERT INTO users(id,display_name,role,issuer,subject,grant_revision,created_at)
		VALUES($1,$2,'platform-admin','failed-foundation-delete-test',$3,1,$4)`, actorID, "failed-foundation-admin-"+suffix, actorID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO access_grants(id,subject_user_id,role,scope_type,scope_id,source,created_by,created_at)
		VALUES($1,$2,'platform-admin','platform','platform','bootstrap',$2,$3)`, id.New(), actorID, now); err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(ctx, actorID, "failed-foundation-project-"+suffix, "failed-foundation-project-"+suffix,
		domain.CreateProject{Name: "Failed foundation project", Slug: "failed-foundation-" + suffix})
	if err != nil {
		t.Fatal(err)
	}
	environment, err := store.CreateEnvironment(ctx, actorID, "failed-foundation-environment-"+suffix, "failed-foundation-environment-"+suffix,
		domain.CreateEnvironment{ProjectID: project.Value.ID, Name: "Failed foundation environment", Slug: "failed"})
	if err != nil {
		t.Fatal(err)
	}

	bindingID, intentID := id.New(), id.New()
	head := strings.Repeat("a", 40)
	manifest := []byte("apiVersion: v1\nkind: Namespace\n")
	manifestSum := sha256.Sum256(manifest)
	manifestDigest := "sha256:" + hex.EncodeToString(manifestSum[:])
	err = store.pool.QueryRow(ctx, `SELECT id,target_head_revision FROM git_repository_bindings WHERE kind='platform'`).Scan(&bindingID, &head)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err = store.pool.Exec(ctx, `INSERT INTO git_repository_bindings(
			id,kind,scope_id,provider,installation_id,repository_id,repository_owner,repository_name,target_ref,path_prefix,
			credential_secret_name,state,target_head_revision,indexed_revision,projection_generation,parser_version,
			target_head_observed_at,indexed_at,created_at,updated_at,credential_mode)
			VALUES($1,'platform',$1,'github',1,1,'kuberploy','fixture','refs/heads/main','platform','',
			'ready',$2,$2,1,'test',$3,$3,$3,$3,'github-app')`, bindingID, head, now); err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO environment_foundation_intents(
		id,environment_id,project_id,namespace,argo_project,platform_binding_id,target_ref,planned_head_revision,
		binding_generation,profile_digest,publisher_config_digest,publisher_contract,publisher_policy,manifest_path,
		manifest,manifest_digest,intent_digest,commit_trailer,state,active,next_attempt_at,attempts,consecutive_failures,
		last_failure_code,lease_epoch,completed_at,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,'refs/heads/main',$7,1,$8,$9,'environment-foundation-protected-git.v1',
		'platform-protected-git.v1',$10,$11,$12,$13,$14,'failed',false,$15,1,1,'protected-git-rejected',0,$15,$16,$15)`,
		intentID, environment.Value.ID, project.Value.ID, environment.Value.Namespace, environment.Value.ArgoProject,
		bindingID, head, "sha256:"+strings.Repeat("b", 64), "sha256:"+strings.Repeat("c", 64),
		"platform/argocd/foundations/"+environment.Value.ID+".yaml", manifest, manifestDigest,
		"sha256:"+strings.Repeat("d", 64), "Kuberploy-Environment-Foundation-Intent: "+intentID,
		now, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	replay, err := store.DeleteEnvironment(ctx, actorID, environment.Value.ID, environment.Value.Name,
		"delete-failed-foundation-"+suffix, "delete-failed-foundation", "request-delete-failed-foundation-"+suffix)
	if err != nil || replay {
		t.Fatalf("delete Environment replay=%t err=%v", replay, err)
	}
	if _, err = store.GetEnvironment(ctx, environment.Value.ID); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("deleted Environment err=%v", err)
	}
	var intentCount, deletionCount int
	if err = store.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM environment_foundation_intents WHERE environment_id=$1),
		(SELECT count(*) FROM environment_foundation_deletions WHERE environment_id=$1)`, environment.Value.ID).
		Scan(&intentCount, &deletionCount); err != nil {
		t.Fatal(err)
	}
	if intentCount != 0 || deletionCount != 0 {
		t.Fatalf("unpublished failed foundation cleanup intents=%d deletions=%d", intentCount, deletionCount)
	}
}
