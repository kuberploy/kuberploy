package projectionpolicy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/testdb"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/secrets"
)

func seedRuntimeSecretProjectionDocument(t testing.TB, pool *pgxpool.Pool, binding gitprojection.Binding, applicationID, sourceRevision, configRevision string, raw []byte, generation int64, indexedAt time.Time) string {
	t.Helper()
	digest := sha256.Sum256(raw)
	contentSHA256 := "sha256:" + hex.EncodeToString(digest[:])
	path, err := gitprojection.ApplicationPath(binding, applicationID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(t.Context(), `INSERT INTO git_projection_generations(binding_id,generation,head_revision,parser_version,state,started_at,activated_at)
		VALUES($1,$2,$3,$4,'active',$5,$5)`, binding.ID, generation, sourceRevision, binding.ParserVersion, indexedAt); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(t.Context(), `INSERT INTO git_projected_documents(binding_id,generation,path,application_id,source_revision,config_revision,blob_id,content_sha256,raw,parsed,valid,diagnostics,schema_version,parser_version,indexed_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'{}'::jsonb,true,'[]'::jsonb,'config.kuberploy.io/v1alpha1',$10,$11)`,
		binding.ID, generation, path, applicationID, sourceRevision, configRevision, strings.Repeat(string(rune('a'+generation)), 40), contentSHA256, raw, binding.ParserVersion, indexedAt); err != nil {
		t.Fatal(err)
	}
	return contentSHA256
}

type projectionSecretKeys struct{}

func (projectionSecretKeys) ActiveKey(context.Context) (secrets.FingerprintKey, error) {
	return secrets.FingerprintKey{ID: secrets.DefaultFingerprintKeyID, Bytes: []byte("0123456789abcdef0123456789abcdef")}, nil
}

type projectionSealedProvider struct{}

func (projectionSealedProvider) StageStrictSealedSecret(_ context.Context, request secrets.StageRequest, material *secrets.Material) (secrets.Artifact, error) {
	if err := material.WithEntries(func(_ string, value []byte) error {
		if len(value) == 0 {
			return secrets.ErrInvalid
		}
		return nil
	}); err != nil {
		return secrets.Artifact{}, err
	}
	return secrets.Artifact{Provider: secrets.ProviderSealedSecrets, Namespace: request.Binding.Scope.Namespace,
		ObjectName: request.TargetSecretName, TargetSecretName: request.TargetSecretName, ProviderRevision: "sealed-provider-revision",
		ManifestDigest: "sha256:" + strings.Repeat("a", 64), SealedKeyFingerprint: "sha256:" + strings.Repeat("b", 64),
		CiphertextDigest: "sha256:" + strings.Repeat("c", 64), TargetSecretType: request.Version.TargetSecretType}, nil
}

func (projectionSealedProvider) ObserveStrictSealedSecret(_ context.Context, artifact secrets.Artifact) (secrets.ReadinessObservation, error) {
	return secrets.ReadinessObservation{Artifact: artifact, Status: secrets.ReadinessReady, ObservedAt: time.Now().UTC()}, nil
}

func (projectionSealedProvider) DeleteStrictSealedSecret(_ context.Context, artifact secrets.Artifact) (secrets.DeleteObservation, error) {
	return secrets.DeleteObservation{Artifact: artifact, Absent: true, ObservedAt: time.Now().UTC()}, nil
}

func cleanupRuntimeSecretProject(ctx context.Context, pool *pgxpool.Pool, projectID string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err = tx.Exec(ctx, `ALTER TABLE mutation_receipts DISABLE TRIGGER USER`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM mutation_receipts
		WHERE secret_binding_id IN (SELECT id FROM secret_bindings WHERE project_id=$1)
		   OR secret_version_id IN (SELECT v.id FROM secret_binding_versions v JOIN secret_bindings b ON b.id=v.binding_id WHERE b.project_id=$1)`, projectID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `ALTER TABLE mutation_receipts ENABLE TRIGGER USER`); err != nil {
		return err
	}
	for _, table := range []string{"secret_binding_deliveries", "secret_binding_events"} {
		if _, err = tx.Exec(ctx, `ALTER TABLE `+table+` DISABLE TRIGGER USER`); err != nil {
			return err
		}
	}
	for _, query := range []string{
		`DELETE FROM secret_binding_deliveries WHERE binding_id IN (SELECT id FROM secret_bindings WHERE project_id=$1)`,
		`DELETE FROM secret_binding_events WHERE binding_id IN (SELECT id FROM secret_bindings WHERE project_id=$1)`,
		`DELETE FROM secret_binding_references WHERE binding_id IN (SELECT id FROM secret_bindings WHERE project_id=$1)`,
		`DELETE FROM secret_binding_runtime_reconciliations WHERE binding_id IN (SELECT id FROM secret_bindings WHERE project_id=$1)`,
		`DELETE FROM tls_certificate_versions WHERE binding_id IN (SELECT id FROM secret_bindings WHERE project_id=$1)`,
		`DELETE FROM secret_binding_versions WHERE binding_id IN (SELECT id FROM secret_bindings WHERE project_id=$1)`,
		`DELETE FROM secret_bindings WHERE project_id=$1`,
	} {
		if _, err = tx.Exec(ctx, query, projectID); err != nil {
			return err
		}
	}
	for _, table := range []string{"secret_binding_events", "secret_binding_deliveries"} {
		if _, err = tx.Exec(ctx, `ALTER TABLE `+table+` ENABLE TRIGGER USER`); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func TestRuntimeSecretReferencePolicyPostgreSQLAtomicReferences(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = testdb.ApplyMigrations(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	actorID, organizationID, projectID, environmentID, environmentBID, applicationID := id.New(), id.New(), id.New(), id.New(), id.New(), id.New()
	suffix := strings.ReplaceAll(projectID, "-", "")[:12]
	namespace := "policy-secrets-" + suffix
	var gitBindingIDs []string
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if err := cleanupRuntimeSecretProject(cleanupCtx, pool, projectID); err != nil {
			t.Errorf("cleanup runtime-secret project: %v", err)
		}
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM git_projected_documents WHERE binding_id=ANY($1::uuid[])`, gitBindingIDs)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM git_projection_generations WHERE binding_id=ANY($1::uuid[])`, gitBindingIDs)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM git_safety_poll_cursors WHERE binding_id=ANY($1::uuid[])`, gitBindingIDs)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM git_verified_head_observations WHERE binding_id=ANY($1::uuid[])`, gitBindingIDs)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM git_repository_bindings WHERE id=ANY($1::uuid[])`, gitBindingIDs)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM applications WHERE id=$1`, applicationID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM environments WHERE project_id=$1`, projectID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM projects WHERE id=$1`, projectID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM teams WHERE id=$1`, organizationID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM users WHERE id=$1`, actorID)
	})
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users(id,login,role,issuer,subject,created_at) VALUES($1,$2,'platform-admin','test',$2,$3)`, []any{actorID, "policy-" + suffix, now}},
		{`INSERT INTO teams(id,name,slug,created_by,created_at) VALUES($1,'Policy team',$2,$3,$4)`, []any{organizationID, "policy-team-" + suffix, actorID, now}},
		{`INSERT INTO projects(id,name,slug,team_id,created_at) VALUES($1,'Policy project',$2,$3,$4)`, []any{projectID, "policy-project-" + suffix, organizationID, now}},
		{`INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at) VALUES($1,$2,'Production',$3,$4,$4,$5)`, []any{environmentID, projectID, "production-" + suffix, namespace, now}},
		{`INSERT INTO applications(id,project_id,name,slug,created_at) VALUES($1,$2,'API',$3,$4)`, []any{applicationID, projectID, "api-" + suffix, now}},
	} {
		if _, err = pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	secretStore, err := secrets.NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	service := secrets.Service{Store: secretStore, Keys: projectionSecretKeys{}, SealedSecrets: projectionSealedProvider{}, Now: func() time.Time { return now.Add(time.Second) }}
	material, err := secrets.NewMaterial(map[string][]byte{
		"password": []byte("projection-policy-private-value"),
		"users":    []byte("admin:$2y$05$projection-policy-private-value"),
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(ctx, secrets.CreateRequest{ActorID: actorID,
		Scope: secrets.Scope{OrganizationID: organizationID, ProjectID: projectID, EnvironmentID: environmentID, ApplicationID: applicationID, Namespace: namespace},
		Name:  "database", Provider: secrets.ProviderSealedSecrets,
		Deliveries: []secrets.Delivery{
			{SourceKey: "password", Kind: secrets.DeliveryEnvironment, EnvironmentName: "DATABASE_PASSWORD"},
			{SourceKey: "users", Kind: secrets.DeliveryFile, FilePath: "/var/run/secrets/kuberploy/traefik-basic-auth/users", FileMode: 0o400},
		},
		IdempotencyKey: "projection-policy-create-0001", RequestID: "projection-policy-create", Material: material})
	if err != nil {
		t.Fatal(err)
	}
	active, err := service.ReconcileVersion(ctx, created.Version.ID, "projection-policy-ready")
	if err != nil {
		t.Fatal(err)
	}
	// Direct Git in another environment must not consume the exact BasicAuth
	// binding identity even when the application ID is shared across both
	// destinations.
	namespaceB := namespace + "-b"
	if _, err = pool.Exec(ctx, `INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at) VALUES($1,$2,'Staging',$3,$4,$4,$5)`, environmentBID, projectID, "staging-"+suffix, namespaceB, now); err != nil {
		t.Fatal(err)
	}

	binding, err := gitprojection.NewGitHubEnvironmentBinding(id.New(), projectID, environmentID,
		gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 10, RepositoryID: 20, Owner: "kuberploy", Name: "desired-state"},
		"refs/heads/main", now)
	if err != nil {
		t.Fatal(err)
	}
	gitBindingIDs = append(gitBindingIDs, binding.ID)
	binding.TargetHeadRevision, binding.TargetHeadObservedAt, binding.State = strings.Repeat("d", 40), now.Add(2*time.Second), gitprojection.BindingIndexing
	binding.UpdatedAt = now.Add(2 * time.Second)
	documentPath, err := gitprojection.ApplicationPath(binding, applicationID)
	if err != nil {
		t.Fatal(err)
	}
	scope := DocumentScope{Binding: binding, OrganizationID: organizationID, Namespace: namespace, ApplicationID: applicationID,
		Path: documentPath, SourceRevision: binding.TargetHeadRevision, ConfigRevision: strings.Repeat("e", 40)}
	projectionStore, err := gitprojection.NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err = projectionStore.PutBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}
	originalRaw := []byte("runtime-secret-policy-document-v1\n")
	scope.ContentSHA256 = seedRuntimeSecretProjectionDocument(t, pool, binding, applicationID, scope.SourceRevision, scope.ConfigRevision, originalRaw, 1, now.Add(3*time.Second))
	config := runtimeSecretPolicyConfig(t)
	config.Namespaces = []string{namespace, namespaceB}
	policy := &RuntimeSecretReferencePolicy{Config: config}
	runtime := domain.NormalizeWorkloadRuntime(domain.DefaultWorkloadRuntime(8080, nil))
	runtime.Env = []domain.WorkloadEnv{{Name: "DATABASE_PASSWORD", ValueFrom: &domain.WorkloadEnvValueFrom{SecretBindingRef: domain.SecretBindingRef{
		BindingID: active.Binding.ID, Name: active.Binding.Name, Key: "password", Version: active.Version.Number,
	}}}}
	if _, err = secrets.ResolveWorkloadBindingReferences(ctx, secretStore, active.Binding.Scope, runtime); err != nil {
		t.Fatalf("fixture reference resolution: %v", err)
	}
	bindingB, err := gitprojection.NewGitHubEnvironmentBinding(id.New(), projectID, environmentBID,
		gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 10, RepositoryID: 20, Owner: "kuberploy", Name: "desired-state"},
		"refs/heads/main", now)
	if err != nil {
		t.Fatal(err)
	}
	gitBindingIDs = append(gitBindingIDs, bindingB.ID)
	bindingB.TargetHeadRevision, bindingB.TargetHeadObservedAt, bindingB.State = strings.Repeat("c", 40), now.Add(2*time.Second), gitprojection.BindingIndexing
	bindingB.UpdatedAt = now.Add(2 * time.Second)
	pathB, err := gitprojection.ApplicationPath(bindingB, applicationID)
	if err != nil {
		t.Fatal(err)
	}
	scopeB := DocumentScope{Binding: bindingB, OrganizationID: organizationID, Namespace: namespaceB, ApplicationID: applicationID,
		Path: pathB, SourceRevision: bindingB.TargetHeadRevision, ConfigRevision: strings.Repeat("9", 40), ContentSHA256: "sha256:" + strings.Repeat("9", 64)}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	basicAuthRef := domain.SecretBindingRef{BindingID: active.Binding.ID, Name: active.Binding.Name, Key: "users", Version: active.Version.Number}
	diagnostics, err := policy.ValidateCurrentTx(ctx, tx, policyTestBasicAuthDocument(t, scopeB, domain.DefaultWorkloadRuntime(8080, nil), basicAuthRef), now.Add(3*time.Second))
	if err != nil || len(diagnostics) != 1 || diagnostics[0].Code != "MiddlewareSecretReferenceUnresolved" {
		tx.Rollback(ctx) //nolint:errcheck
		t.Fatalf("cross-environment direct-Git BasicAuth diagnostics=%#v err=%v", diagnostics, err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err = policy.ValidateCurrentTx(ctx, tx, policyTestDocument(t, scope, runtime), now.Add(3*time.Second))
	if err != nil || len(diagnostics) != 0 {
		tx.Rollback(ctx) //nolint:errcheck
		t.Fatalf("diagnostics=%#v err=%v", diagnostics, err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	assertProjectionSecretReference(t, pool, active.Binding.ID, documentPath, active.Version.ID, binding.TargetHeadRevision, 1)

	// Rotation must not invalidate the exact indexed AppConfig that is still
	// running on the now-retained version. Otherwise the fail-closed config API
	// cannot read that document to republish it with the new active version.
	rotatedMaterial, materialErr := secrets.NewMaterial(map[string][]byte{
		"password": []byte("projection-policy-rotated-private-value"),
		"users":    []byte("admin:$2y$05$projection-policy-rotated-private-value"),
	})
	if materialErr != nil {
		t.Fatal(materialErr)
	}
	rotated, rotateErr := service.Rotate(ctx, secrets.RotateRequest{ActorID: actorID, BindingID: active.Binding.ID,
		ExpectedActiveVersion: 1, Deliveries: active.Version.Deliveries, IdempotencyKey: "projection-policy-rotate-0001",
		RequestID: "projection-policy-rotate", Material: rotatedMaterial})
	if rotateErr != nil {
		t.Fatal(rotateErr)
	}
	rotated, rotateErr = service.ReconcileVersion(ctx, rotated.Version.ID, "projection-policy-ready-2")
	if rotateErr != nil || rotated.Binding.ActiveVersion != 2 {
		t.Fatalf("rotation=%#v err=%v", rotated, rotateErr)
	}
	if _, resolveErr := secrets.ResolveWorkloadBindingReferences(ctx, secretStore, active.Binding.Scope, runtime); !errors.Is(resolveErr, secrets.ErrNotReady) {
		t.Fatalf("ordinary write resolver accepted retained Git version: %v", resolveErr)
	}
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err = policy.ValidateCurrentTx(ctx, tx, policyTestDocument(t, scope, runtime), now.Add(4*time.Second))
	if err != nil || len(diagnostics) != 0 {
		tx.Rollback(ctx) //nolint:errcheck
		t.Fatalf("retained exact Git reference diagnostics=%#v err=%v", diagnostics, err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	assertProjectionSecretReference(t, pool, active.Binding.ID, documentPath, active.Version.ID, binding.TargetHeadRevision, 1)

	changedGitScope := scope
	changedGitScope.SourceRevision = strings.Repeat("f", 40)
	changedGitScope.Binding.TargetHeadRevision = changedGitScope.SourceRevision
	changedGitScope.ContentSHA256 = seedRuntimeSecretProjectionDocument(t, pool, binding, applicationID, changedGitScope.SourceRevision, scope.ConfigRevision, originalRaw, 2, now.Add(5*time.Second))
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err = policy.ValidateCurrentTx(ctx, tx, policyTestDocument(t, changedGitScope, runtime), now.Add(5*time.Second))
	if err != nil || len(diagnostics) != 0 {
		tx.Rollback(ctx) //nolint:errcheck
		t.Fatalf("byte-identical descendant rejected retained version diagnostics=%#v err=%v", diagnostics, err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	assertProjectionSecretReference(t, pool, active.Binding.ID, documentPath, active.Version.ID, changedGitScope.SourceRevision, 1)

	changedBytesScope := changedGitScope
	changedBytesScope.SourceRevision = strings.Repeat("6", 40)
	changedBytesScope.Binding.TargetHeadRevision = changedBytesScope.SourceRevision
	changedBytesScope.ContentSHA256 = seedRuntimeSecretProjectionDocument(t, pool, binding, applicationID, changedBytesScope.SourceRevision, scope.ConfigRevision, []byte("changed-runtime-secret-policy-document\n"), 3, now.Add(6*time.Second))
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err = policy.ValidateCurrentTx(ctx, tx, policyTestDocument(t, changedBytesScope, runtime), now.Add(6*time.Second))
	if err != nil || len(diagnostics) != 1 || diagnostics[0].Code != "RuntimeSecretReferenceUnresolved" {
		tx.Rollback(ctx) //nolint:errcheck
		t.Fatalf("changed Git bytes restored retained version diagnostics=%#v err=%v", diagnostics, err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	assertProjectionSecretReference(t, pool, active.Binding.ID, documentPath, active.Version.ID, changedGitScope.SourceRevision, 1)

	// A caller cannot erase a team-owned project's durable organization scope
	// to resolve or silently orphan a Git-current guard.
	noOrganization := scope
	noOrganization.OrganizationID = ""
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = policy.ValidateCurrentTx(ctx, tx, policyTestDocument(t, noOrganization, runtime), now.Add(4*time.Second)); !errors.Is(err, gitprojection.ErrInvalid) {
		tx.Rollback(ctx) //nolint:errcheck
		t.Fatalf("erased-organization scope error=%v", err)
	}
	tx.Rollback(ctx) //nolint:errcheck
	emptyRuntime := domain.NormalizeWorkloadRuntime(domain.DefaultWorkloadRuntime(8080, nil))
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = policy.ValidateCurrentTx(ctx, tx, policyTestDocument(t, noOrganization, emptyRuntime), now.Add(4*time.Second)); !errors.Is(err, gitprojection.ErrInvalid) {
		tx.Rollback(ctx) //nolint:errcheck
		t.Fatalf("erased-organization current cleanup error=%v", err)
	}
	tx.Rollback(ctx) //nolint:errcheck
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = policy.ReconcileDeletedTx(ctx, tx, noOrganization, now.Add(4*time.Second)); !errors.Is(err, gitprojection.ErrInvalid) {
		tx.Rollback(ctx) //nolint:errcheck
		t.Fatalf("erased-organization deleted cleanup error=%v", err)
	}
	tx.Rollback(ctx) //nolint:errcheck
	assertProjectionSecretReference(t, pool, active.Binding.ID, documentPath, active.Version.ID, changedGitScope.SourceRevision, 1)

	// A semantic name drift becomes a safe diagnostic and preserves the exact
	// previously deployable reference.
	unresolved := runtime
	unresolved.Env = append([]domain.WorkloadEnv(nil), runtime.Env...)
	unresolved.Env[0].ValueFrom = &domain.WorkloadEnvValueFrom{SecretBindingRef: runtime.Env[0].ValueFrom.SecretBindingRef}
	unresolved.Env[0].ValueFrom.SecretBindingRef.Name = "other-name"
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err = policy.ValidateCurrentTx(ctx, tx, policyTestDocument(t, scope, unresolved), now.Add(4*time.Second))
	if err != nil || len(diagnostics) != 1 || diagnostics[0].Code != "RuntimeSecretReferenceUnresolved" {
		tx.Rollback(ctx) //nolint:errcheck
		t.Fatalf("unresolved diagnostics=%#v err=%v", diagnostics, err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	assertProjectionSecretReference(t, pool, active.Binding.ID, documentPath, active.Version.ID, changedGitScope.SourceRevision, 1)

	// An outer activation failure rolls back a successful empty-plan removal.
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics, err = policy.ValidateCurrentTx(ctx, tx, policyTestDocument(t, scope, emptyRuntime), now.Add(5*time.Second)); err != nil || len(diagnostics) != 0 {
		tx.Rollback(ctx) //nolint:errcheck
		t.Fatalf("empty diagnostics=%#v err=%v", diagnostics, err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	assertProjectionSecretReference(t, pool, active.Binding.ID, documentPath, active.Version.ID, changedGitScope.SourceRevision, 1)

	retained := secrets.Reference{BindingID: active.Binding.ID, VersionID: active.Version.ID, Kind: secrets.ReferenceRetainedRelease,
		Reference: "release-" + suffix, Revision: strings.Repeat("f", 40), CreatedAt: now.Add(6 * time.Second)}
	event := secrets.Event{ID: id.New(), BindingID: retained.BindingID, VersionID: retained.VersionID, ActorID: actorID,
		Kind: secrets.EventReferenceAdded, RequestID: "projection-policy-retained", OccurredAt: retained.CreatedAt}
	if err = secretStore.AddReference(ctx, retained, event); err != nil {
		t.Fatal(err)
	}
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = policy.ReconcileDeletedTx(ctx, tx, scope, now.Add(7*time.Second)); err != nil {
		tx.Rollback(ctx) //nolint:errcheck
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	assertProjectionSecretReference(t, pool, active.Binding.ID, documentPath, "", "", 0)
	var retainedCount int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM secret_binding_references WHERE binding_id=$1 AND kind='retained-release'`, active.Binding.ID).Scan(&retainedCount); err != nil || retainedCount != 1 {
		t.Fatalf("retained references=%d err=%v", retainedCount, err)
	}
	if _, err = service.Delete(ctx, actorID, active.Binding.ID, "projection-policy-delete-blocked"); !errors.Is(err, secrets.ErrReferenced) {
		t.Fatalf("retained release deletion guard=%v", err)
	}
	var persisted string
	if err = pool.QueryRow(ctx, `SELECT row_to_json(v)::text FROM secret_binding_versions v WHERE id=$1`, active.Version.ID).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(persisted, "projection-policy-private-value") {
		t.Fatal("runtime-secret material was persisted")
	}
}

func TestRuntimeSecretReferencePolicyPostgreSQLPersonalProject(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = testdb.ApplyMigrations(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	actorID, projectID, environmentID, applicationID := id.New(), id.New(), id.New(), id.New()
	suffix := strings.ReplaceAll(projectID, "-", "")[:12]
	namespace := "personal-secrets-" + suffix
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if err := cleanupRuntimeSecretProject(cleanupCtx, pool, projectID); err != nil {
			t.Errorf("cleanup personal runtime-secret project: %v", err)
		}
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM applications WHERE id=$1`, applicationID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM environments WHERE project_id=$1`, projectID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM projects WHERE id=$1`, projectID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM users WHERE id=$1`, actorID)
	})
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users(id,login,role,issuer,subject,created_at) VALUES($1,$2,'platform-admin','test',$2,$3)`, []any{actorID, "personal-policy-" + suffix, now}},
		{`INSERT INTO projects(id,name,slug,team_id,created_at) VALUES($1,'Personal project',$2,NULL,$3)`, []any{projectID, "personal-policy-" + suffix, now}},
		{`INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at) VALUES($1,$2,'Production',$3,$4,$4,$5)`, []any{environmentID, projectID, "personal-production-" + suffix, namespace, now}},
		{`INSERT INTO applications(id,project_id,name,slug,created_at) VALUES($1,$2,'API',$3,$4)`, []any{applicationID, projectID, "personal-api-" + suffix, now}},
	} {
		if _, err = pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	secretStore, err := secrets.NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	service := secrets.Service{Store: secretStore, Keys: projectionSecretKeys{}, SealedSecrets: projectionSealedProvider{}, Now: func() time.Time { return now.Add(time.Second) }}
	material, err := secrets.NewMaterial(map[string][]byte{"password": []byte("personal-policy-private-value")})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(ctx, secrets.CreateRequest{ActorID: actorID,
		Scope: secrets.Scope{ProjectID: projectID, EnvironmentID: environmentID, ApplicationID: applicationID, Namespace: namespace},
		Name:  "database", Provider: secrets.ProviderSealedSecrets,
		Deliveries:     []secrets.Delivery{{SourceKey: "password", Kind: secrets.DeliveryEnvironment, EnvironmentName: "DATABASE_PASSWORD"}},
		IdempotencyKey: "personal-projection-policy-create", RequestID: "personal-projection-policy-create", Material: material})
	if err != nil {
		t.Fatal(err)
	}
	active, err := service.ReconcileVersion(ctx, created.Version.ID, "personal-projection-policy-ready")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := gitprojection.NewGitHubEnvironmentBinding(id.New(), projectID, environmentID,
		gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 30, RepositoryID: 40, Owner: "kuberploy", Name: "desired-state"},
		"refs/heads/main", now)
	if err != nil {
		t.Fatal(err)
	}
	binding.TargetHeadRevision, binding.TargetHeadObservedAt, binding.State = strings.Repeat("7", 40), now.Add(2*time.Second), gitprojection.BindingIndexing
	binding.UpdatedAt = now.Add(2 * time.Second)
	path, err := gitprojection.ApplicationPath(binding, applicationID)
	if err != nil {
		t.Fatal(err)
	}
	scope := DocumentScope{Binding: binding, Namespace: namespace, ApplicationID: applicationID,
		Path: path, SourceRevision: binding.TargetHeadRevision, ConfigRevision: strings.Repeat("8", 40), ContentSHA256: "sha256:" + strings.Repeat("8", 64)}
	config := runtimeSecretPolicyConfig(t)
	config.Namespaces = []string{namespace}
	policy := &RuntimeSecretReferencePolicy{Config: config}
	runtime := domain.NormalizeWorkloadRuntime(domain.DefaultWorkloadRuntime(8080, nil))
	runtime.Env = []domain.WorkloadEnv{{Name: "DATABASE_PASSWORD", ValueFrom: &domain.WorkloadEnvValueFrom{SecretBindingRef: domain.SecretBindingRef{
		BindingID: active.Binding.ID, Name: active.Binding.Name, Key: "password", Version: active.Version.Number,
	}}}}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err := policy.ValidateCurrentTx(ctx, tx, policyTestDocument(t, scope, runtime), now.Add(3*time.Second))
	if err != nil || len(diagnostics) != 0 {
		tx.Rollback(ctx) //nolint:errcheck
		t.Fatalf("personal-project diagnostics=%#v err=%v", diagnostics, err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	assertProjectionSecretReference(t, pool, active.Binding.ID, path, active.Version.ID, binding.TargetHeadRevision, 1)

	// Supplying a team identity for a personal project is rejected before the
	// existing deletion guard can be changed.
	tampered := scope
	tampered.OrganizationID = id.New()
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = policy.ValidateCurrentTx(ctx, tx, policyTestDocument(t, tampered, domain.NormalizeWorkloadRuntime(domain.DefaultWorkloadRuntime(8080, nil))), now.Add(4*time.Second)); !errors.Is(err, gitprojection.ErrInvalid) {
		tx.Rollback(ctx) //nolint:errcheck
		t.Fatalf("tampered personal-project scope error=%v", err)
	}
	tx.Rollback(ctx) //nolint:errcheck
	assertProjectionSecretReference(t, pool, active.Binding.ID, path, active.Version.ID, binding.TargetHeadRevision, 1)

	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = policy.ReconcileDeletedTx(ctx, tx, scope, now.Add(5*time.Second)); err != nil {
		tx.Rollback(ctx) //nolint:errcheck
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	assertProjectionSecretReference(t, pool, active.Binding.ID, path, "", "", 0)
}

func assertProjectionSecretReference(t *testing.T, pool *pgxpool.Pool, bindingID, path, versionID, revision string, expected int) {
	t.Helper()
	var count int
	var storedVersion, storedRevision string
	err := pool.QueryRow(t.Context(), `SELECT count(*),COALESCE(max(version_id::text),''),COALESCE(max(revision),'')
		FROM secret_binding_references WHERE binding_id=$1 AND kind='git-current' AND reference_id=$2`, bindingID, path).Scan(&count, &storedVersion, &storedRevision)
	if err != nil || count != expected || expected == 1 && (storedVersion != versionID || storedRevision != revision) {
		t.Fatalf("Git-current count=%d version=%q revision=%q err=%v", count, storedVersion, storedRevision, err)
	}
}
