package registrycredentials

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/testdb"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/secrets"
)

func TestPostgreSQLRegistryCredentialAttestationContract(t *testing.T) {
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
	if err = testdb.ApplyMigrations(ctx, pool); err != nil {
		t.Fatal(err)
	}
	actorID := id.New()
	scope := secrets.Scope{
		OrganizationID: id.New(), ProjectID: id.New(), EnvironmentID: id.New(), ApplicationID: id.New(),
		Namespace: "registry-credential-" + actorID[:8],
	}
	suffix := actorID[:8]
	if _, err = pool.Exec(ctx, "INSERT INTO users(id,display_name,role,issuer,subject,created_at) VALUES($1,$2,'platform-admin','test',$3,$4)",
		actorID, "registry-credential-"+suffix, "registry-credential-"+actorID, testNow); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, "INSERT INTO teams(id,name,slug,created_by,created_at) VALUES($1,'Registry credential team',$2,$3,$4)",
		scope.OrganizationID, "registry-credential-"+suffix, actorID, testNow); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, "INSERT INTO projects(id,name,slug,team_id,created_at) VALUES($1,'Registry credential project',$2,$3,$4)",
		scope.ProjectID, "registry-credential-"+suffix, scope.OrganizationID, testNow); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, "INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at) VALUES($1,$2,'Registry credential environment','registry-credential-environment',$3,$4,$5)",
		scope.EnvironmentID, scope.ProjectID, scope.Namespace, "registry-credential-"+suffix, testNow); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, "INSERT INTO applications(id,project_id,name,slug,created_at) VALUES($1,$2,'Registry credential app','registry-credential-app',$3)",
		scope.ApplicationID, scope.ProjectID, testNow); err != nil {
		t.Fatal(err)
	}
	secretStore, err := secrets.NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	credentialStore, err := NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	secretService := secrets.Service{
		Store: secretStore, Keys: staticFingerprintKeys{value: []byte("0123456789abcdef0123456789abcdef")},
		SealedSecrets: fakeSealedProvider{}, Now: func() time.Time { return testNow },
	}
	service := Service{Secrets: &secretService, Catalog: secretStore, Store: credentialStore}
	request := func() CreateRequest {
		material, materialErr := NewMaterial([]byte("registry.example.test"), []byte("ci-bot"), []byte("s3cr3t-token"), []byte("ci-bot@example.test"))
		if materialErr != nil {
			t.Fatal(materialErr)
		}
		return CreateRequest{
			ActorID: actorID, Scope: scope, Name: "public-edge", IdempotencyKey: "postgres-registry-credential-1",
			RequestID: "postgres-registry-credential", Material: material,
		}
	}
	created, err := service.Create(ctx, request())
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := service.Create(ctx, request())
	if err != nil || !replayed.Replay || replayed.Credential.SecretVersionID != created.Credential.SecretVersionID {
		t.Fatalf("replayed=%#v err=%v", replayed, err)
	}
	stored, err := credentialStore.Version(ctx, created.Version.ID)
	if err != nil || !sameVersion(stored, created.Credential) {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}

	if _, err = pool.Exec(ctx, "UPDATE secret_bindings SET purpose='runtime-secret' WHERE id=$1", created.Binding.ID); err == nil {
		t.Fatal("registry credential binding purpose was mutable")
	}
	if _, err = pool.Exec(ctx, "UPDATE secret_binding_versions SET target_secret_type='Opaque' WHERE id=$1", created.Version.ID); err == nil {
		t.Fatal("registry credential target Secret type was mutable")
	}
	if _, err = pool.Exec(ctx, "UPDATE registry_pull_credential_versions SET host='other.example.test' WHERE version_id=$1", created.Version.ID); err == nil {
		t.Fatal("registry credential attestation was mutable")
	}
	if _, err = pool.Exec(ctx, "DELETE FROM registry_pull_credential_versions WHERE version_id=$1", created.Version.ID); err == nil {
		t.Fatal("registry credential attestation was deletable")
	}
	if _, err = pool.Exec(ctx, "INSERT INTO registry_pull_credential_versions("+
		"version_id,binding_id,version_number,secret_content_fingerprint,host,username,created_by,created_at) "+
		"VALUES($1,$2,99,$3,'registry.example.test','ci-bot',$4,$5)",
		created.Version.ID, created.Binding.ID, created.Version.ContentFingerprint[:], actorID, testNow); err == nil {
		t.Fatal("rebound registry credential version number was accepted")
	}
	if _, err = pool.Exec(ctx, "INSERT INTO secret_bindings("+
		"id,organization_id,project_id,environment_id,application_id,target_namespace,name,provider,purpose,state,active_version,created_by,created_at,updated_at) "+
		"VALUES('10000000-0000-4000-8000-000000000098',$1,$2,$3,$4,$5,'bad-registry-credential','external-secrets','registry-pull-credential','provisioning',0,$6,$7,$7)",
		scope.OrganizationID, scope.ProjectID, scope.EnvironmentID, scope.ApplicationID, scope.Namespace, actorID, testNow); err == nil {
		t.Fatal("registry-pull-credential purpose accepted an External Secrets provider")
	}

	active, err := secretService.ReconcileVersion(ctx, created.Version.ID, "postgres-registry-credential-ready")
	if err != nil || active.Version.State != secrets.VersionActive {
		t.Fatalf("active=%#v err=%v", active, err)
	}

	rotated, err := service.Rotate(ctx, RotateRequest{
		ActorID: actorID, BindingID: created.Binding.ID, ExpectedActiveVersion: 1,
		IdempotencyKey: "postgres-registry-credential-rotation", RequestID: "postgres-registry-credential-rotation",
		Material: func() *Material {
			material, materialErr := NewMaterial([]byte("registry.example.test"), []byte("ci-bot"), []byte("new-token"), nil)
			if materialErr != nil {
				t.Fatal(materialErr)
			}
			return material
		}(),
	})
	if err != nil || rotated.Version.Number != 2 {
		t.Fatalf("rotated=%#v err=%v", rotated, err)
	}
	versions, err := credentialStore.Versions(ctx, created.Binding.ID)
	if err != nil || len(versions) != 2 {
		t.Fatalf("versions=%#v err=%v", versions, err)
	}
	if _, err = secretService.ReconcileVersion(ctx, rotated.Version.ID, "postgres-registry-credential-v2-ready"); err != nil {
		t.Fatal(err)
	}

	if _, err = service.Delete(ctx, actorID, created.Binding.ID, "postgres-registry-credential-delete"); err != nil {
		t.Fatalf("delete: %v", err)
	}
}
