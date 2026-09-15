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
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/kuberploy/kuberploy/internal/builds"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/environmentfoundation"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/secrets"
	base "github.com/kuberploy/kuberploy/internal/store"
	"github.com/kuberploy/kuberploy/internal/testdb"
)

func assertPostgresState(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("PostgreSQL error=%v, want SQLSTATE %s", err, code)
	}
}

func assertSecretHistoryIdentity(t *testing.T, store *Store, bindingID, versionID string) {
	t.Helper()
	ctx := t.Context()
	// A second real binding makes a version substitution distinguishable from
	// an unknown ID. Everything in this transaction is rolled back afterward.
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	otherBindingID, otherVersionID := id.New(), id.New()
	if _, err = tx.Exec(ctx, `INSERT INTO secret_bindings(id,project_id,environment_id,application_id,
		target_namespace,name,provider,state,active_version,created_by,created_at,updated_at,delete_started_at,deleted_at)
		SELECT $2,project_id,environment_id,application_id,target_namespace,'other-history-binding',
		provider,state,active_version,created_by,created_at,updated_at,delete_started_at,deleted_at FROM secret_bindings WHERE id=$1`,
		bindingID, otherBindingID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO secret_binding_versions(id,binding_id,version_number,provider,state,
		fingerprint_key_id,content_fingerprint,staged_at,created_at,updated_at)
		SELECT $2,$3,version_number,provider,state,fingerprint_key_id,content_fingerprint,staged_at,created_at,updated_at
		FROM secret_binding_versions WHERE id=$1`, versionID, otherVersionID, otherBindingID); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO secret_binding_deliveries(version_id,binding_id,ordinal,source_key,kind,environment_name)
		 VALUES($1,$2,1,'value','environment','SUBSTITUTED')`,
		`INSERT INTO secret_binding_events(id,version_id,binding_id,kind,request_id,occurred_at)
		 VALUES(gen_random_uuid(),$1,$2,'binding-deleted','substituted',now())`,
	} {
		for _, candidateVersion := range []string{otherVersionID, id.New()} {
			if _, err = tx.Exec(ctx, `SAVEPOINT history_identity`); err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(ctx, statement, candidateVersion, bindingID)
			assertPostgresState(t, err, "23503")
			if _, err = tx.Exec(ctx, `ROLLBACK TO SAVEPOINT history_identity`); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// Either kind of accepted history write holds both identity rows against a
	// concurrent delete until commit, just as the previous foreign keys did.
	for _, statement := range []string{
		`INSERT INTO secret_binding_deliveries(version_id,binding_id,ordinal,source_key,kind,environment_name)
		 VALUES($1,$2,1,'value','environment','LOCKED')`,
		`INSERT INTO secret_binding_events(id,version_id,binding_id,kind,request_id,occurred_at)
		 VALUES(gen_random_uuid(),$1,$2,'binding-deleted','locked',now())`,
	} {
		lockTx, beginErr := store.pool.Begin(ctx)
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		defer lockTx.Rollback(ctx) //nolint:errcheck
		if _, err = lockTx.Exec(ctx, statement, versionID, bindingID); err != nil {
			t.Fatal(err)
		}
		_, err = store.pool.Exec(ctx, `SELECT id FROM secret_bindings WHERE id=$1 FOR UPDATE NOWAIT`, bindingID)
		assertPostgresState(t, err, "55P03")
		_, err = store.pool.Exec(ctx, `SELECT id FROM secret_binding_versions WHERE id=$1 FOR UPDATE NOWAIT`, versionID)
		assertPostgresState(t, err, "55P03")
		if err = lockTx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

func assertSecretCleanupLockOrder(t *testing.T, store *Store, applicationID, bindingID, versionID string) {
	t.Helper()
	ctx := t.Context()
	writer, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(ctx) //nolint:errcheck
	if _, err = writer.Exec(ctx, `SELECT id FROM secret_bindings WHERE id=$1 FOR KEY SHARE`, bindingID); err != nil {
		t.Fatal(err)
	}
	cleanup, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var cleanupPID int
	if err = cleanup.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&cleanupPID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		purgeErr := purgeDeletedSecretBindings(ctx, cleanup, "application_id", applicationID)
		_ = cleanup.Rollback(ctx)
		done <- purgeErr
	}()
	// Wait until cleanup is actually blocked on this writer, avoiding timing
	// assumptions about which goroutine reaches PostgreSQL first.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err = store.pool.QueryRow(ctx, `SELECT wait_event_type='Lock' FROM pg_stat_activity WHERE pid=$1`, cleanupPID).Scan(&waiting); err == nil && waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cleanup did not wait for the in-flight history writer")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Cleanup must not already hold the version: the writer still needs that
	// identity lock to finish its event and release the binding without deadlock.
	if _, err = writer.Exec(ctx, `SELECT id FROM secret_binding_versions WHERE id=$1 FOR KEY SHARE NOWAIT`, versionID); err != nil {
		t.Fatalf("cleanup inverted history identity lock order: %v", err)
	}
	if _, err = writer.Exec(ctx, `INSERT INTO secret_binding_events(id,version_id,binding_id,kind,request_id,occurred_at)
		VALUES($1,$2,$3,'binding-deleted','concurrent-cleanup',now())`, id.New(), versionID, bindingID); err != nil {
		t.Fatal(err)
	}
	if err = writer.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatalf("cleanup after completed history writer: %v", err)
	}
}

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
	deletedVersionID := id.New()
	if _, err = store.pool.Exec(ctx, `INSERT INTO secret_binding_versions(
		id,binding_id,version_number,provider,state,fingerprint_key_id,content_fingerprint,staged_at,created_at,updated_at)
		VALUES($1,$2,1,'sealed-secrets','deleted','resource-delete-test',decode(repeat('00',32),'hex'),$3,$3,$3)`,
		deletedVersionID, deletedBindingID, now); err != nil {
		t.Fatal(err)
	}
	// A real secret lifecycle retains immutable delivery and event history after
	// provider deletion. A bare tombstone does not exercise resource cleanup.
	if _, err = store.pool.Exec(ctx, `INSERT INTO secret_binding_deliveries(
		version_id,binding_id,ordinal,source_key,kind,environment_name)
		VALUES($1,$2,0,'value','environment','DELETION_TEST_SECRET')`, deletedVersionID, deletedBindingID); err != nil {
		t.Fatal(err)
	}
	deletedEventID := id.New()
	if _, err = store.pool.Exec(ctx, `INSERT INTO secret_binding_events(
		id,binding_id,version_id,actor_id,kind,request_id,occurred_at)
		VALUES($1,$2,$3,$4,'binding-deleted',$5,$6)`, deletedEventID, deletedBindingID,
		deletedVersionID, actorID, "secret-deleted-"+suffix, now); err != nil {
		t.Fatal(err)
	}
	assertSecretHistoryIdentity(t, store, deletedBindingID, deletedVersionID)
	assertSecretCleanupLockOrder(t, store, application.Value.ID, deletedBindingID, deletedVersionID)
	// A registry pull credential or custom certificate binding also leaves a
	// permanent append-only attestation row behind once deleted. App deletion
	// must still succeed: the append-only trigger only blocks UPDATE, not the
	// DELETE this cascade performs once the App itself is gone.
	registryBindingID, registryVersionID := id.New(), id.New()
	if _, err = store.pool.Exec(ctx, `INSERT INTO secret_bindings(
		id,organization_id,project_id,environment_id,application_id,target_namespace,name,provider,state,
		active_version,created_by,created_at,updated_at,delete_started_at,deleted_at,purpose)
		VALUES($1,NULL,$2,$3,$4,$5,'deleted-registry-credential','sealed-secrets','deleted',0,$6,$7,$7,$7,$7,'registry-pull-credential')`,
		registryBindingID, project.Value.ID, environment.Value.ID, application.Value.ID, environment.Value.Namespace, actorID, now); err != nil {
		t.Fatal(err)
	}
	digest64 := "sha256:" + strings.Repeat("7", 64)
	if _, err = store.pool.Exec(ctx, `INSERT INTO secret_binding_versions(
		id,binding_id,version_number,provider,state,fingerprint_key_id,content_fingerprint,
		provider_object_name,target_secret_name,provider_revision,manifest_digest,sealed_key_fingerprint,
		ciphertext_digest,target_secret_type,staged_at,activated_at,retained_at,created_at,updated_at)
		VALUES($1,$2,1,'sealed-secrets','retained','resource-delete-test',decode(repeat('00',32),'hex'),
		'rc485-cred','rc485-cred','v1',$3,$3,$3,'kubernetes.io/dockerconfigjson',$4,$4,$4,$4,$4)`,
		registryVersionID, registryBindingID, digest64, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO secret_binding_events(
		id,binding_id,version_id,actor_id,kind,request_id,occurred_at)
		VALUES($1,$2,$3,$4,'version-staging',$5,$6)`, id.New(), registryBindingID,
		registryVersionID, actorID, "registry-staging-"+suffix, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO registry_pull_credential_versions(
		version_id,binding_id,version_number,secret_content_fingerprint,host,username,created_by,created_at)
		VALUES($1,$2,1,decode(repeat('00',32),'hex'),'registry.example.com','deletion-test-user',$3,$4)`,
		registryVersionID, registryBindingID, actorID, now); err != nil {
		t.Fatal(err)
	}
	tlsBindingID, tlsVersionID := id.New(), id.New()
	if _, err = store.pool.Exec(ctx, `INSERT INTO secret_bindings(
		id,organization_id,project_id,environment_id,application_id,target_namespace,name,provider,state,
		active_version,created_by,created_at,updated_at,delete_started_at,deleted_at,purpose)
		VALUES($1,NULL,$2,$3,$4,$5,'deleted-tls-certificate','sealed-secrets','deleted',0,$6,$7,$7,$7,$7,'tls-certificate')`,
		tlsBindingID, project.Value.ID, environment.Value.ID, application.Value.ID, environment.Value.Namespace, actorID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO secret_binding_versions(
		id,binding_id,version_number,provider,state,fingerprint_key_id,content_fingerprint,
		provider_object_name,target_secret_name,provider_revision,manifest_digest,sealed_key_fingerprint,
		ciphertext_digest,target_secret_type,staged_at,activated_at,retained_at,created_at,updated_at)
		VALUES($1,$2,1,'sealed-secrets','retained','resource-delete-test',decode(repeat('00',32),'hex'),
		'rc485-tls','rc485-tls','v1',$3,$3,$3,'kubernetes.io/tls',$4,$4,$4,$4,$4)`,
		tlsVersionID, tlsBindingID, digest64, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO secret_binding_events(
		id,binding_id,version_id,actor_id,kind,request_id,occurred_at)
		VALUES($1,$2,$3,$4,'version-staging',$5,$6)`, id.New(), tlsBindingID,
		tlsVersionID, actorID, "tls-staging-"+suffix, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO tls_certificate_versions(
		version_id,binding_id,version_number,secret_content_fingerprint,leaf_fingerprint,public_key_fingerprint,
		dns_names,ip_addresses,not_before,not_after,created_by,created_at)
		VALUES($1,$2,1,decode(repeat('00',32),'hex'),$3,$3,'["deletion-test.example.com"]'::jsonb,'[]'::jsonb,$4,$5,$6,$4)`,
		tlsVersionID, tlsBindingID, digest64, now, now.Add(90*24*time.Hour), actorID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO mutation_receipts(
		actor_id,receipt_kind,namespace,scope_key,idempotency_key,request_fingerprint,secret_binding_id,secret_version_id,created_at)
		VALUES($1,'secret-binding','create',$2,$3,decode(repeat('11',32),'hex'),$4,$5,$6)`,
		actorID, application.Value.ID, "deleted-secret-key-"+suffix, deletedBindingID, deletedVersionID, now); err != nil {
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
	if _, err = store.pool.Exec(ctx, `UPDATE applications SET source_kind='github',build_source_id=$1,build_source_kind='github',build_source_installation_id=$2,
		build_source_repository_id=$3,build_source_registry_target_id=$4,build_source_trigger_ref='refs/heads/main',build_source_spec='{}',
		build_source_digest=$5,build_source_revision=1,build_source_created_at=$6,build_source_updated_at=$6 WHERE id=$7`,
		definitionID, installationID, repositoryID, registryID, "sha256:"+strings.Repeat("9", 64), now, application.Value.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO service_registry_policies(
		registry_target_id,service_id,repository,keep_last_successful,minimum_safety_age_seconds,
		cache_unused_expiry_seconds,cache_byte_quota,created_at,updated_at)
		VALUES($1,$2,'apps/disposable',1,60,60,1048576,$3,$3)`, registryID, application.Value.ID, now); err != nil {
		t.Fatal(err)
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
	var registryRows int
	if err = store.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM service_registry_policies WHERE service_id=$1) +
		(SELECT count(*) FROM registry_artifact_references WHERE service_id=$1) +
		(SELECT count(*) FROM registry_authority_observations WHERE service_id=$1) +
		(SELECT count(*) FROM registry_cache_generations WHERE service_id=$1) +
		(SELECT count(*) FROM registry_releases WHERE service_id=$1)`, application.Value.ID).Scan(&registryRows); err != nil || registryRows != 0 {
		t.Fatalf("deleted App retained registry lifecycle rows=%d err=%v", registryRows, err)
	}
	var deletedBindingExists bool
	if err = store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM secret_bindings WHERE id=$1)`, deletedBindingID).Scan(&deletedBindingExists); err != nil || deletedBindingExists {
		t.Fatalf("deleted secret tombstone remained after App deletion exists=%t err=%v", deletedBindingExists, err)
	}
	var orphanedExtensionRows int
	if err = store.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM secret_bindings WHERE id IN ($1,$2)) +
		(SELECT count(*) FROM secret_binding_versions WHERE id IN ($3,$4)) +
		(SELECT count(*) FROM registry_pull_credential_versions WHERE binding_id=$1) +
		(SELECT count(*) FROM tls_certificate_versions WHERE binding_id=$2)`,
		registryBindingID, tlsBindingID, registryVersionID, tlsVersionID).Scan(&orphanedExtensionRows); err != nil || orphanedExtensionRows != 0 {
		t.Fatalf("App deletion left registry-credential/TLS-certificate rows behind rows=%d err=%v", orphanedExtensionRows, err)
	}
	var retainedReceipt bool
	if err = store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mutation_receipts WHERE secret_binding_id=$1 AND secret_version_id=$2)`, deletedBindingID, deletedVersionID).Scan(&retainedReceipt); err != nil || !retainedReceipt {
		t.Fatalf("immutable secret mutation receipt was not retained exists=%t err=%v", retainedReceipt, err)
	}
	var retainedHistory int
	if err = store.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM secret_binding_deliveries WHERE binding_id=$1 AND version_id=$2 AND source_key='value') +
		(SELECT count(*) FROM secret_binding_events WHERE id=$3 AND binding_id=$1 AND version_id=$2 AND kind='binding-deleted')`,
		deletedBindingID, deletedVersionID, deletedEventID).Scan(&retainedHistory); err != nil || retainedHistory != 2 {
		t.Fatalf("immutable secret history was not retained rows=%d err=%v", retainedHistory, err)
	}
	for _, statement := range []string{
		`DELETE FROM secret_binding_deliveries WHERE binding_id=$1`,
		`UPDATE secret_binding_deliveries SET source_key='rewritten' WHERE binding_id=$1`,
		`DELETE FROM secret_binding_events WHERE binding_id=$1`,
		`UPDATE secret_binding_events SET request_id='rewritten' WHERE binding_id=$1`,
	} {
		_, err = store.pool.Exec(ctx, statement, deletedBindingID)
		assertPostgresState(t, err, "23514")
	}
	_, err = store.pool.Exec(ctx, `INSERT INTO secret_binding_events(id,binding_id,kind,request_id,occurred_at)
		VALUES($1,$2,'binding-deleted','missing-binding',now())`, id.New(), deletedBindingID)
	assertPostgresState(t, err, "23503")
	_, err = store.pool.Exec(ctx, `INSERT INTO secret_binding_deliveries(version_id,binding_id,ordinal,source_key,kind,environment_name)
		VALUES($1,$2,1,'value','environment','AFTER_DELETION')`, deletedVersionID, deletedBindingID)
	assertPostgresState(t, err, "23503")
	// Publication acknowledgment remains legal even if resource cleanup wins
	// the race with the event publisher; event identity remains immutable.
	secretStore, err := secrets.NewPostgreSQLStore(store.pool)
	if err != nil {
		t.Fatal(err)
	}
	events, err := secretStore.PendingEvents(ctx, 1000)
	if err != nil {
		t.Fatalf("read retained unpublished events: %v", err)
	}
	var pendingFound bool
	for _, event := range events {
		if event.ID != deletedEventID {
			continue
		}
		pendingFound = true
		if event.BindingID != deletedBindingID || event.VersionID != deletedVersionID || event.ActorID != actorID {
			t.Fatalf("retained event lost identity: %#v", event)
		}
		if err = secretStore.MarkEventPublished(ctx, event.ID, event.OccurredAt); err != nil {
			t.Fatalf("publish retained event: %v", err)
		}
	}
	if !pendingFound {
		t.Fatal("retained unpublished event was stranded after resource deletion")
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
	var storedProviderRequest string
	if err = store.pool.QueryRow(ctx, `SELECT state,provider_request FROM environment_foundation_deletions WHERE environment_id=$1`, environment.Value.ID).Scan(&deletionState, &storedProviderRequest); err != nil || deletionState != "ready" || !strings.HasPrefix(storedProviderRequest, "cleanup-v2:sha256:") {
		t.Fatalf("completed Environment Git cleanup state=%q contract=%q err=%v", deletionState, storedProviderRequest, err)
	}
	if _, err = store.pool.Exec(ctx, `UPDATE environment_foundation_deletions SET provider_request='legacy-foundation-cleanup' WHERE environment_id=$1`, environment.Value.ID); err != nil {
		t.Fatal(err)
	}
	legacyLease, found, err := foundationStore.ClaimDeletion(ctx, worker, time.Now().UTC().Add(time.Second), time.Minute)
	if err != nil || !found || legacyLease.Deletion.EnvironmentID != environment.Value.ID || legacyLease.Deletion.CompletedAt != nil {
		t.Fatalf("legacy cleanup replay=%#v found=%t err=%v", legacyLease, found, err)
	}
	if err = foundationStore.RecordDeletionReady(ctx, legacyLease, environmentfoundation.DeletionReceipt{
		OperationID: legacyLease.Deletion.ID, BindingID: legacyLease.Deletion.BindingID, TargetRef: legacyLease.Deletion.TargetRef,
		Path: legacyLease.Deletion.Path, ParentRevision: cleanupRevision, CommittedRevision: cleanupRevision,
		ProviderRequest: "test-environment-cleanup-v2",
	}, time.Now().UTC().Add(2*time.Second)); err != nil {
		t.Fatal(err)
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
	if _, err = store.pool.Exec(ctx, `INSERT INTO audit_events(id,actor_id,action,target_type,target_id,request_id,created_at)
		VALUES($1,$2,'service-account.test','project',$3,$4,$5)`, id.New(), account.Value.ID, project.Value.ID, "service-account-audit-"+suffix, now); err != nil {
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
	var serviceAccountExists bool
	var tombstoneIssuer string
	if err = store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM service_accounts WHERE id=$1),issuer FROM users WHERE id=$1`, account.Value.ID).Scan(&serviceAccountExists, &tombstoneIssuer); err != nil || serviceAccountExists || tombstoneIssuer != "kuberploy:deleted" {
		t.Fatalf("disabled service-account cleanup exists=%t issuer=%q err=%v", serviceAccountExists, tombstoneIssuer, err)
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

func TestPostgreSQLApplicationDeletionRequiresAppliedHelmDisable(t *testing.T) {
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
	if _, err = store.pool.Exec(ctx, `INSERT INTO users(id,display_name,role,issuer,subject,grant_revision,created_at) VALUES($1,$2,'platform-admin','helm-delete-test',$3,1,$4)`, actorID, "helm-delete-admin-"+suffix, actorID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO access_grants(id,subject_user_id,role,scope_type,scope_id,source,created_by,created_at) VALUES($1,$2,'platform-admin','platform','platform','bootstrap',$2,$3)`, id.New(), actorID, now); err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(ctx, actorID, "helm-delete-project-"+suffix, "helm-delete-project-"+suffix, domain.CreateProject{Name: "Helm delete project", Slug: "helm-delete-" + suffix})
	if err != nil {
		t.Fatal(err)
	}
	environment, err := store.CreateEnvironment(ctx, actorID, "helm-delete-environment-"+suffix, "helm-delete-environment-"+suffix, domain.CreateEnvironment{ProjectID: project.Value.ID, Name: "Production", Slug: "production"})
	if err != nil {
		t.Fatal(err)
	}
	application, err := store.CreateApplication(ctx, actorID, "helm-delete-application-"+suffix, "helm-delete-application-"+suffix, domain.CreateApplication{ProjectID: project.Value.ID, EnvironmentID: environment.Value.ID, Name: "Valkey", Slug: "valkey", SourceKind: domain.ApplicationSourceHelm})
	if err != nil {
		t.Fatal(err)
	}
	values := []byte("{}\n")
	valuesSum := sha256.Sum256(values)
	valuesDigest := "sha256:" + hex.EncodeToString(valuesSum[:])
	activeRevisionID := id.New()
	if _, err = store.pool.Exec(ctx, `INSERT INTO helm_app_revisions(
		id,generation,project_id,environment_id,application_id,release_name,destination_namespace,argo_project,
		source_kind,repository_url,chart,target_revision,chart_path,values_yaml,values_digest,action,desired_enabled,
		state,failure_code,actor_id,idempotency_key,request_id,created_at,updated_at)
		VALUES($1,1,$2,$3,$4,'valkey',$5,$6,'helm-repository','https://charts.example.test','valkey','1.0.0','',$7,$8,'deploy',true,'applied','',$9,$10,$11,$12,$12)`,
		activeRevisionID, project.Value.ID, environment.Value.ID, application.Value.ID, environment.Value.Namespace, environment.Value.ArgoProject,
		values, valuesDigest, actorID, "helm-active-"+suffix, "request-active-"+suffix, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO helm_app_heads(project_id,environment_id,application_id,revision_id,generation,updated_at) VALUES($1,$2,$3,$4,1,$5)`,
		project.Value.ID, environment.Value.ID, application.Value.ID, activeRevisionID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DeleteEnvironment(ctx, actorID, environment.Value.ID, environment.Value.Name, "helm-environment-delete-blocked-"+suffix, "helm-environment-delete-blocked", "request-helm-environment-delete-blocked-"+suffix); !errors.Is(err, base.ErrEnvironmentDeletionBlocked) {
		t.Fatalf("active Helm Environment deletion err=%v", err)
	}
	if _, err = store.DeleteApplication(ctx, actorID, application.Value.ID, application.Value.Name, "helm-delete-blocked-"+suffix, "helm-delete-blocked", "request-helm-delete-blocked-"+suffix); !errors.Is(err, base.ErrApplicationDeletionBlocked) {
		t.Fatalf("active Helm App deletion err=%v", err)
	}

	disabledRevisionID := id.New()
	if _, err = store.pool.Exec(ctx, `INSERT INTO helm_app_revisions(
		id,generation,project_id,environment_id,application_id,release_name,destination_namespace,argo_project,
		source_kind,repository_url,chart,target_revision,chart_path,values_yaml,values_digest,action,desired_enabled,
		state,failure_code,actor_id,idempotency_key,request_id,parent_revision_id,created_at,updated_at)
		VALUES($1,2,$2,$3,$4,'valkey',$5,$6,'helm-repository','https://charts.example.test','valkey','1.0.0','',$7,$8,'disable',false,'applied','',$9,$10,$11,$12,$13,$13)`,
		disabledRevisionID, project.Value.ID, environment.Value.ID, application.Value.ID, environment.Value.Namespace, environment.Value.ArgoProject,
		values, valuesDigest, actorID, "helm-disabled-"+suffix, "request-disabled-"+suffix, activeRevisionID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `UPDATE helm_app_heads SET revision_id=$1,generation=2,updated_at=$2 WHERE environment_id=$3 AND application_id=$4`,
		disabledRevisionID, now.Add(time.Second), environment.Value.ID, application.Value.ID); err != nil {
		t.Fatal(err)
	}
	if replay, deleteErr := store.DeleteApplication(ctx, actorID, application.Value.ID, application.Value.Name, "helm-delete-ready-"+suffix, "helm-delete-ready", "request-helm-delete-ready-"+suffix); deleteErr != nil || replay {
		t.Fatalf("disabled Helm App delete replay=%t err=%v", replay, deleteErr)
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
