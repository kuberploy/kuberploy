package postgres

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/testdb"

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

func TestPostgreSQLAPIAcceptancePreservesIndexedRuntimeSecretGuards(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := t.Context()
	migrationPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if err = testdb.ApplyMigrations(ctx, migrationPool); err != nil {
		migrationPool.Close()
		t.Fatal(err)
	}
	migrationPool.Close()
	st, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC().Add(-time.Minute)
	actorID, organizationID, projectID, environmentID, applicationID := id.New(), id.New(), id.New(), id.New(), id.New()
	suffix := strings.ReplaceAll(projectID, "-", "")[:12]
	namespace := "store-secret-" + suffix
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users(id,login,role,issuer,subject,created_at) VALUES($1,$2,'platform-admin','store-secret',$2,$3)`, []any{actorID, "store-secret-" + suffix, now}},
		{`INSERT INTO access_grants(id,subject_user_id,role,scope_type,scope_id,source,created_by,created_at)
			VALUES($1,$2,'platform-admin','platform','platform','bootstrap',$2,$3)`, []any{id.New(), actorID, now}},
		{`INSERT INTO teams(id,name,slug,created_by,created_at) VALUES($1,'Store secret team',$2,$3,$4)`, []any{organizationID, "store-secret-team-" + suffix, actorID, now}},
		{`INSERT INTO projects(id,name,slug,team_id,created_at) VALUES($1,'Store secret project',$2,$3,$4)`, []any{projectID, "store-secret-project-" + suffix, organizationID, now}},
		{`INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at) VALUES($1,$2,'Production',$3,$4,$4,$5)`, []any{environmentID, projectID, "production-" + suffix, namespace, now}},
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
	usersMaterial, err := secrets.NewMaterial(map[string][]byte{"users": []byte("developer:$2y$05$integration")})
	if err != nil {
		t.Fatal(err)
	}
	basicAuth, err := service.Create(ctx, secrets.CreateRequest{ActorID: actorID, Scope: active.Binding.Scope,
		Name: "basic-auth", Provider: secrets.ProviderSealedSecrets,
		Deliveries: []secrets.Delivery{{SourceKey: "users", Kind: secrets.DeliveryFile,
			FilePath: secrets.MiddlewareBasicAuthUsersPath, FileMode: 0o400}},
		IdempotencyKey: "store-reference-basic-auth-0001", RequestID: "store-reference-basic-auth", Material: usersMaterial})
	if err != nil {
		t.Fatal(err)
	}
	basicAuthActive, err := service.ReconcileVersion(ctx, basicAuth.Version.ID, "store-reference-basic-auth-ready")
	if err != nil {
		t.Fatal(err)
	}
	basicAuthRef := domain.SecretBindingRef{BindingID: basicAuthActive.Binding.ID, Name: basicAuthActive.Binding.Name, Key: "users", Version: 1}
	combined, err := secrets.ResolveAppConfigBindingReferences(ctx, secretStore, active.Binding.Scope, runtime, []domain.SecretBindingRef{basicAuthRef})
	if err != nil {
		t.Fatal(err)
	}
	combinedDigest, err := combined.Digest()
	if err != nil {
		t.Fatal(err)
	}
	validationTx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	txCatalog, err := secrets.NewPostgreSQLBindingReferenceCatalogTx(validationTx)
	if err != nil {
		validationTx.Rollback(ctx) //nolint:errcheck
		t.Fatal(err)
	}
	if transactionResolved, resolveErr := secrets.ResolveAppConfigBindingReferences(ctx, txCatalog, active.Binding.Scope, runtime, []domain.SecretBindingRef{basicAuthRef}); resolveErr != nil || len(transactionResolved.Uses) != 2 {
		validationTx.Rollback(ctx) //nolint:errcheck
		t.Fatalf("transaction catalog combined plan=%#v err=%v", transactionResolved, resolveErr)
	}
	validated, err := validateRuntimeSecretReferencesTx(ctx, validationTx, actorID,
		&base.AppConfigReferencePlan{RuntimeSecretDigest: combinedDigest}, projectID, environmentID, applicationID, runtime,
		[]domain.SecretBindingRef{basicAuthRef})
	validationTx.Rollback(ctx) //nolint:errcheck
	if err != nil || len(validated.Uses) != 2 || len(validated.BindingVersions()) != 2 {
		t.Fatalf("combined workload+BasicAuth transaction plan=%#v err=%v", validated, err)
	}
	// Exercise the final projection transaction, not only the read-only
	// catalog: both environment and file deliveries must survive the exact
	// metadata recheck and become deletion guards atomically.
	combinedReferenceID := "store-reference-combined-" + suffix
	combinedTx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = secrets.ReplaceIndexedGitCurrentReferencesTx(ctx, combinedTx, combined, id.New(), combinedReferenceID,
		strings.Repeat("9", 40), "sha256:"+strings.Repeat("8", 64), "store-reference-combined", now.Add(2*time.Second)); err != nil {
		combinedTx.Rollback(ctx) //nolint:errcheck
		t.Fatal(err)
	}
	if err = combinedTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for _, bindingID := range []string{active.Binding.ID, basicAuthActive.Binding.ID} {
		var count int
		if err = st.pool.QueryRow(ctx, `SELECT count(*) FROM secret_binding_references
			WHERE binding_id=$1 AND kind='git-current' AND reference_id=$2`, bindingID, combinedReferenceID).Scan(&count); err != nil || count != 1 {
			t.Fatalf("combined git-current reference binding=%s count=%d err=%v", bindingID, count, err)
		}
	}
	binding, err := gitprojection.NewGitHubEnvironmentBinding(id.New(), projectID, environmentID,
		gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 11, RepositoryID: 22, Owner: "kuberploy", Name: "desired-state"},
		"refs/heads/main", now)
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("7", 40)
	binding.State, binding.TargetHeadRevision, binding.IndexedRevision, binding.ProjectionGeneration = gitprojection.BindingReady, head, head, 1
	binding.TargetHeadObservedAt, binding.IndexedAt, binding.UpdatedAt = now.Add(time.Second), now.Add(time.Second), now.Add(time.Second)
	projectionStore, err := gitprojection.NewPostgreSQLStore(st.pool)
	if err != nil {
		t.Fatal(err)
	}
	if err = projectionStore.PutBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}
	referenceID, err := gitprojection.ApplicationPath(binding, applicationID)
	if err != nil {
		t.Fatal(err)
	}
	writePlan := &gitprojection.WritePlan{BindingID: binding.ID, ProjectID: projectID, EnvironmentID: environmentID,
		ApplicationID: applicationID, BaseRevision: head, Precondition: gitprojection.MutationCreateIfAbsent,
		ChartDigest: "sha256:" + strings.Repeat("6", 64), PolicyVersion: binding.ParserVersion}
	workloadDigest, err := plan.Digest()
	if err != nil {
		t.Fatal(err)
	}
	create := domain.CreateDeployment{EnvironmentID: environmentID, ApplicationID: applicationID,
		Image: "registry.example/runtime-secret@sha256:" + strings.Repeat("5", 64), Runtime: runtime}
	initial, initialOperation, err := st.CreateDeployment(ctx, actorID, "secret-initial-create-0001", "secret-initial-create",
		"secret-initial-create", create, writePlan, &base.AppConfigReferencePlan{RuntimeSecretDigest: workloadDigest})
	if err != nil || initial.Replay || initialOperation.Status != "queued" {
		t.Fatalf("initial create=%#v operation=%#v err=%v", initial, initialOperation, err)
	}
	assertStoreReferenceCounts(t, st, active.Binding.ID, referenceID, 0, 0)
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

	// API acceptance is not Git activation. An ordinary candidate must preserve
	// the previous indexed guard while queued, running, failed, or superseded.
	ordinary := create
	ordinary.Runtime = domain.DefaultWorkloadRuntime(8080, nil)
	queued, queuedOperation, err := st.CreateDeployment(ctx, actorID, "secret-ordinary-queued-0001", "secret-ordinary-queued",
		"secret-ordinary-queued", ordinary, writePlan)
	if err != nil || queued.Replay || queuedOperation.Status != "queued" {
		t.Fatalf("queued candidate=%#v operation=%#v err=%v", queued, queuedOperation, err)
	}
	assertStoreReferenceCounts(t, st, active.Binding.ID, referenceID, 1, 0)
	runningOperation, execute, err := st.StartOperation(ctx, queuedOperation.ID, queuedOperation.Generation, "secret-guard-worker", time.Minute)
	if err != nil || !execute || runningOperation.Status != "running" {
		t.Fatalf("running operation=%#v execute=%t err=%v", runningOperation, execute, err)
	}
	assertStoreReferenceCounts(t, st, active.Binding.ID, referenceID, 1, 0)
	if err = st.FailOperation(ctx, queuedOperation.ID, queuedOperation.Generation, "secret-guard-worker", "GitWriteFailed", "synthetic Git failure"); err != nil {
		t.Fatal(err)
	}
	assertStoreReferenceCounts(t, st, active.Binding.ID, referenceID, 1, 0)
	_, supersededOperation, err := st.CreateDeployment(ctx, actorID, "secret-superseded-old-0001", "secret-superseded-old",
		"secret-superseded-old", ordinary, writePlan)
	if err != nil {
		t.Fatal(err)
	}
	_, newestOperation, err := st.CreateDeployment(ctx, actorID, "secret-superseded-new-0001", "secret-superseded-new",
		"secret-superseded-new", ordinary, writePlan)
	if err != nil || newestOperation.Status != "queued" {
		t.Fatalf("newest operation=%#v err=%v", newestOperation, err)
	}
	oldOperation, err := st.GetOperation(ctx, supersededOperation.ID)
	if err != nil || oldOperation.Status != "superseded" {
		t.Fatalf("superseded operation=%#v err=%v", oldOperation, err)
	}
	assertStoreReferenceCounts(t, st, active.Binding.ID, referenceID, 1, 0)
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
