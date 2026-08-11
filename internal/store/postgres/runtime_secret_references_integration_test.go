package postgres

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/secrets"
	base "github.com/kuberploy/kuberploy/internal/store"
)

type referenceIntegrationKeys struct{}

func (referenceIntegrationKeys) ActiveKey(context.Context) (secrets.FingerprintKey, error) {
	return secrets.FingerprintKey{ID: secrets.DefaultFingerprintKeyID, Bytes: []byte("0123456789abcdef0123456789abcdef")}, nil
}

type referenceIntegrationSealer struct{}

func (referenceIntegrationSealer) StageStrictSealedSecret(_ context.Context, request secrets.StageRequest, material *secrets.Material) (secrets.Artifact, error) {
	if err := material.WithEntries(func(_ string, value []byte) error {
		if len(value) == 0 {
			return secrets.ErrInvalid
		}
		return nil
	}); err != nil {
		return secrets.Artifact{}, err
	}
	return secrets.Artifact{Provider: secrets.ProviderSealedSecrets, Namespace: request.Binding.Scope.Namespace,
		ObjectName: request.TargetSecretName, TargetSecretName: request.TargetSecretName, ProviderRevision: "store-reference-integration",
		ManifestDigest: "sha256:" + strings.Repeat("a", 64), SealedKeyFingerprint: "sha256:" + strings.Repeat("b", 64),
		CiphertextDigest: "sha256:" + strings.Repeat("c", 64), TargetSecretType: request.Version.TargetSecretType}, nil
}

func (referenceIntegrationSealer) ObserveStrictSealedSecret(_ context.Context, artifact secrets.Artifact) (secrets.ReadinessObservation, error) {
	return secrets.ReadinessObservation{Artifact: artifact, Status: secrets.ReadinessReady, ObservedAt: time.Now().UTC()}, nil
}

func (referenceIntegrationSealer) DeleteStrictSealedSecret(_ context.Context, artifact secrets.Artifact) (secrets.DeleteObservation, error) {
	return secrets.DeleteObservation{Artifact: artifact, Absent: true, ObservedAt: time.Now().UTC()}, nil
}

func TestPostgreSQLNoReferencePlanAtomicallyRemovesOnlyExactGitCurrentGuards(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := t.Context()
	st, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err = Migrate(ctx, st.pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	actorID, organizationID, projectID, environmentID, applicationID := id.New(), id.New(), id.New(), id.New(), id.New()
	suffix := strings.ReplaceAll(projectID, "-", "")[:12]
	namespace := "store-secret-" + suffix
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users(id,login,role,issuer,subject,created_at) VALUES($1,$2,'platform-admin','store-secret',$2,$3)`, []any{actorID, "store-secret-" + suffix, now}},
		{`INSERT INTO teams(id,name,slug,created_by,created_at) VALUES($1,'Store secret team',$2,$3,$4)`, []any{organizationID, "store-secret-team-" + suffix, actorID, now}},
		{`INSERT INTO projects(id,name,slug,team_id,created_at) VALUES($1,'Store secret project',$2,$3,$4)`, []any{projectID, "store-secret-project-" + suffix, organizationID, now}},
		{`INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at) VALUES($1,$2,'Production',$3,$4,$5,$6)`, []any{environmentID, projectID, "production-" + suffix, namespace, "kp-p-" + suffix, now}},
		{`INSERT INTO applications(id,project_id,name,slug,created_at) VALUES($1,$2,'API',$3,$4)`, []any{applicationID, projectID, "api-" + suffix, now}},
	} {
		if _, err = st.pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	secretStore, err := secrets.NewPostgreSQLStore(st.pool)
	if err != nil {
		t.Fatal(err)
	}
	service := secrets.Service{Store: secretStore, Keys: referenceIntegrationKeys{}, SealedSecrets: referenceIntegrationSealer{}, Now: func() time.Time { return now.Add(time.Second) }}
	material, err := secrets.NewMaterial(map[string][]byte{"password": []byte("write-only-integration-value")})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(ctx, secrets.CreateRequest{ActorID: actorID,
		Scope: secrets.Scope{OrganizationID: organizationID, ProjectID: projectID, EnvironmentID: environmentID, ApplicationID: applicationID, Namespace: namespace},
		Name:  "database", Provider: secrets.ProviderSealedSecrets,
		Deliveries:     []secrets.Delivery{{SourceKey: "password", Kind: secrets.DeliveryEnvironment, EnvironmentName: "DATABASE_PASSWORD"}},
		IdempotencyKey: "store-reference-create-0001", RequestID: "store-reference-create", Material: material})
	if err != nil {
		t.Fatal(err)
	}
	active, err := service.ReconcileVersion(ctx, created.Version.ID, "store-reference-ready")
	if err != nil {
		t.Fatal(err)
	}
	runtime := domain.NormalizeWorkloadRuntime(domain.DefaultWorkloadRuntime(8080, nil))
	runtime.Env = []domain.WorkloadEnv{{Name: "DATABASE_PASSWORD", ValueFrom: &domain.WorkloadEnvValueFrom{SecretBindingRef: domain.SecretBindingRef{
		BindingID: active.Binding.ID, Name: active.Binding.Name, Key: "password", Version: active.Version.Number,
	}}}}
	plan, err := secrets.ResolveWorkloadBindingReferences(ctx, secretStore, active.Binding.Scope, runtime)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := gitprojection.NewGitHubEnvironmentBinding(id.New(), projectID, environmentID,
		gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 11, RepositoryID: 22, Owner: "kuberploy", Name: "desired-state"},
		"refs/heads/main", now)
	if err != nil {
		t.Fatal(err)
	}
	referenceID, err := gitprojection.ApplicationPath(binding, applicationID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = secrets.ReplaceGitCurrentReferencesTx(ctx, tx, plan, actorID, referenceID, "sha256:"+strings.Repeat("d", 64), "store-reference-add", now.Add(2*time.Second)); err != nil {
		tx.Rollback(ctx) //nolint:errcheck
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	retained := secrets.Reference{BindingID: active.Binding.ID, VersionID: active.Version.ID, Kind: secrets.ReferenceRetainedRelease,
		Reference: "store-reference-retained", Revision: strings.Repeat("e", 40), CreatedAt: now.Add(3 * time.Second)}
	retainedEvent := secrets.Event{ID: id.New(), BindingID: retained.BindingID, VersionID: retained.VersionID, ActorID: actorID,
		Kind: secrets.EventReferenceAdded, RequestID: "store-reference-retained", OccurredAt: retained.CreatedAt}
	if err = secretStore.AddReference(ctx, retained, retainedEvent); err != nil {
		t.Fatal(err)
	}

	tx, err = st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = removeRuntimeSecretReferencesTx(ctx, tx, actorID, binding, projectID, environmentID, applicationID,
		[]byte("ordinary-appconfig"), "store-reference-remove", now.Add(4*time.Second)); err != nil {
		tx.Rollback(ctx) //nolint:errcheck
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	assertStoreReferenceCounts(t, st, active.Binding.ID, referenceID, 0, 1)

	// Re-add and prove a caller rollback cannot leak the removal.
	tx, err = st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = secrets.ReplaceGitCurrentReferencesTx(ctx, tx, plan, actorID, referenceID, "sha256:"+strings.Repeat("f", 64), "store-reference-readd", now.Add(5*time.Second)); err != nil {
		tx.Rollback(ctx) //nolint:errcheck
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tx, err = st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = removeRuntimeSecretReferencesTx(ctx, tx, actorID, binding, projectID, environmentID, applicationID,
		[]byte("ordinary-appconfig-next"), "store-reference-rollback", now.Add(6*time.Second)); err != nil {
		tx.Rollback(ctx) //nolint:errcheck
		t.Fatal(err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	assertStoreReferenceCounts(t, st, active.Binding.ID, referenceID, 1, 1)

	// A platform project with no organization is an unaffected proven-empty
	// no-op, but a cross-scope/corrupt row at its exact path fails closed.
	platformProjectID, platformEnvironmentID, platformApplicationID := id.New(), id.New(), id.New()
	platformSuffix := strings.ReplaceAll(platformProjectID, "-", "")[:12]
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO projects(id,name,slug,created_at) VALUES($1,'Platform project',$2,$3)`, []any{platformProjectID, "platform-" + platformSuffix, now}},
		{`INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at) VALUES($1,$2,'Platform',$3,$4,$5,$6)`, []any{platformEnvironmentID, platformProjectID, "platform-" + platformSuffix, "platform-" + platformSuffix, "kp-p-" + platformSuffix, now}},
		{`INSERT INTO applications(id,project_id,name,slug,created_at) VALUES($1,$2,'Platform API',$3,$4)`, []any{platformApplicationID, platformProjectID, "platform-api-" + platformSuffix, now}},
	} {
		if _, err = st.pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	platformBinding, err := gitprojection.NewGitHubEnvironmentBinding(id.New(), platformProjectID, platformEnvironmentID,
		gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 33, RepositoryID: 44, Owner: "kuberploy", Name: "platform-state"},
		"refs/heads/main", now)
	if err != nil {
		t.Fatal(err)
	}
	tx, err = st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = removeRuntimeSecretReferencesTx(ctx, tx, actorID, platformBinding, platformProjectID, platformEnvironmentID,
		platformApplicationID, []byte("platform-ordinary"), "platform-reference-empty", now.Add(7*time.Second)); err != nil {
		tx.Rollback(ctx) //nolint:errcheck
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	platformPath, err := gitprojection.ApplicationPath(platformBinding, platformApplicationID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err = st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = secrets.ReplaceGitCurrentReferencesTx(ctx, tx, plan, actorID, platformPath, "sha256:"+strings.Repeat("1", 64), "platform-reference-corrupt", now.Add(8*time.Second)); err != nil {
		tx.Rollback(ctx) //nolint:errcheck
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tx, err = st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = removeRuntimeSecretReferencesTx(ctx, tx, actorID, platformBinding, platformProjectID, platformEnvironmentID,
		platformApplicationID, []byte("platform-ordinary"), "platform-reference-corrupt-remove", now.Add(9*time.Second)); !errors.Is(err, base.ErrPreconditionFailed) {
		tx.Rollback(ctx) //nolint:errcheck
		t.Fatalf("cross-scope platform cleanup error=%v", err)
	}
	tx.Rollback(ctx) //nolint:errcheck
	var corruptCount int
	if err = st.pool.QueryRow(ctx, `SELECT count(*) FROM secret_binding_references WHERE kind='git-current' AND reference_id=$1`, platformPath).Scan(&corruptCount); err != nil || corruptCount != 1 {
		t.Fatalf("cross-scope guard count=%d err=%v", corruptCount, err)
	}
}

func assertStoreReferenceCounts(t *testing.T, st *Store, bindingID, referenceID string, gitCurrent, retained int) {
	t.Helper()
	var actualGit, actualRetained int
	err := st.pool.QueryRow(t.Context(), `SELECT
		count(*) FILTER (WHERE kind='git-current' AND reference_id=$2),
		count(*) FILTER (WHERE kind='retained-release')
		FROM secret_binding_references WHERE binding_id=$1`, bindingID, referenceID).Scan(&actualGit, &actualRetained)
	if err != nil || actualGit != gitCurrent || actualRetained != retained {
		t.Fatalf("references git=%d/%d retained=%d/%d err=%v", actualGit, gitCurrent, actualRetained, retained, err)
	}
}
