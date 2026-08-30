package projectionpolicy_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/testdb"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitops"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/projectionpolicy"
	"github.com/kuberploy/kuberploy/internal/secrets"
)

func TestPostgreSQLPolicyActivationPersistsExternalDNSReadinessDiagnostic(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
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

	userID, teamID, projectID := id.New(), id.New(), id.New()
	environmentID, applicationID, bindingID, integrationID := id.New(), id.New(), id.New(), id.New()
	identity := strings.ReplaceAll(userID[:8], "-", "")
	now := time.Now().UTC().Truncate(time.Microsecond)
	cleanup := func(cleanupContext context.Context) {
		_, _ = pool.Exec(cleanupContext, `DELETE FROM git_projected_documents WHERE binding_id=$1`, bindingID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM git_projection_generations WHERE binding_id=$1`, bindingID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM git_safety_poll_cursors WHERE binding_id=$1`, bindingID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM git_verified_head_observations WHERE binding_id=$1`, bindingID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM git_repository_bindings WHERE id=$1`, bindingID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM external_dns_integrations WHERE id=$1`, integrationID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM applications WHERE id=$1`, applicationID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM environments WHERE id=$1`, environmentID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM projects WHERE id=$1`, projectID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM teams WHERE id=$1`, teamID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM users WHERE id=$1`, userID)
	}
	cleanup(ctx)
	defer cleanup(context.Background())

	if _, err = pool.Exec(ctx, `INSERT INTO users(id,display_name,role,issuer,subject,grant_revision,created_at)
		VALUES($1,$2,'platform-admin','projection-policy',$3,1,$4)`, userID, "policy-"+identity, "policy-"+identity, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO teams(id,name,slug,created_by,created_at) VALUES($1,$2,$3,$4,$5)`,
		teamID, "Policy team "+identity, "policy-team-"+identity, userID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO projects(id,name,slug,team_id,created_at) VALUES($1,$2,$3,$4,$5)`,
		projectID, "Policy project "+identity, "policy-project-"+identity, teamID, now); err != nil {
		t.Fatal(err)
	}
	namespace := "policy-" + identity
	if _, err = pool.Exec(ctx, `INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at)
		VALUES($1,$2,'Production','production',$3,$3,$4)`, environmentID, projectID, namespace, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO applications(id,project_id,name,slug,created_at) VALUES($1,$2,'API','api',$3)`, applicationID, projectID, now); err != nil {
		t.Fatal(err)
	}
	allowedSuffix := identity + ".example.com"
	if _, err = pool.Exec(ctx, `INSERT INTO external_dns_integrations(
		id,slug,name,mode,provider_kind,txt_owner_id,allowed_domain_suffixes,sync_policy,destructive_sync_confirmed,
		credential_secret_ref,provider_config_ref,egress_config_ref,created_by,created_at,updated_at)
		VALUES($1,$2,$3,'managed','cloudflare',$4,$5,'upsert-only',false,$6,$7,$8,$9,$10,$10)`,
		integrationID, "public-"+identity, "Public DNS "+identity, "policy."+identity, `["`+allowedSuffix+`"]`,
		"credentials-"+identity, "provider-"+identity, "egress-"+identity, userID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO external_dns_integration_environments(integration_id,environment_id,created_at) VALUES($1,$2,$3)`, integrationID, environmentID, now); err != nil {
		t.Fatal(err)
	}

	binding, err := gitprojection.NewGitHubEnvironmentBinding(bindingID, projectID, environmentID,
		gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 7_200_000_000_000_001, RepositoryID: 7_200_000_000_000_002, Owner: "kuberploy", Name: "policy-integration"},
		"refs/heads/main", now)
	if err != nil {
		t.Fatal(err)
	}
	store, err := gitprojection.NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.PutBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}
	work, err := store.ClaimReconciliation(ctx, "projection-policy-postgres", now.Add(time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	binding, _, err = store.RecordVerifiedHead(ctx, gitprojection.VerifiedHead{BindingID: binding.ID, Repository: binding.Repository, TargetRef: binding.TargetRef,
		Commit: head, Source: gitprojection.ObservationPoll, ProviderRequest: "policy-postgres-head", ObservedAt: now.Add(2 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	generation, err := store.BeginGeneration(ctx, work.Lease, head, binding.ParserVersion, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	project := domain.Project{ID: projectID, Slug: "policy-project-" + identity}
	environment := domain.Environment{ID: environmentID, ProjectID: projectID, Slug: "production"}
	application := domain.Application{ID: applicationID, ProjectID: projectID, Name: "API", Slug: "api"}
	deployment := domain.Deployment{Image: "registry.example/api@sha256:" + strings.Repeat("c", 64), Runtime: domain.DefaultWorkloadRuntime(8080, nil),
		Route: &domain.Route{Hostname: "api." + allowedSuffix, PathPrefix: "/", TLSMode: "httpOnly"}}
	raw, err := gitops.RenderAppConfig(project, environment, application, deployment)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), "      tls:\n", "      dns:\n        mode: externalDns\n        integrationRef: public-"+identity+"\n        ttl: 300\n      tls:\n", 1))
	parsed, _, appDiagnostics := appconfig.ParseAndValidate(raw)
	if len(appDiagnostics) != 0 {
		t.Fatalf("test AppConfig is invalid: %#v\n%s", appDiagnostics, raw)
	}
	document, err := gitprojection.NewDocument(binding, generation.Number, applicationID, head, head, strings.Repeat("b", 40), raw, parsed, nil, now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.PutDocuments(ctx, generation, []gitprojection.Document{document}); err != nil {
		t.Fatal(err)
	}
	activated, err := store.ActivateGeneration(ctx, work.Lease, generation, &projectionpolicy.Validator{}, now.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	projected, err := store.Document(ctx, binding.ID, document.Path)
	if err != nil {
		t.Fatal(err)
	}
	if activated.State != gitprojection.BindingReady || projected.Valid || len(projected.Diagnostics) != 2 ||
		projected.Diagnostics[0].Code != "ExternalDNSRuntimeUnobserved" ||
		projected.Diagnostics[1].Code != "EdgeRoutePolicyUnavailable" {
		t.Fatalf("dynamic policy result was not persisted exactly: binding=%#v document=%#v", activated, projected)
	}

	// A later policy diagnostic must roll back every mutation performed by an
	// earlier policy for this document. A zero-diagnostic generation releases
	// the same savepoint and commits both validation and reconciliation.
	plainDeployment := deployment
	plainDeployment.Route = nil
	plainRaw, renderErr := gitops.RenderAppConfig(project, environment, application, plainDeployment)
	if renderErr != nil {
		t.Fatal(renderErr)
	}
	stageAggregate := func(sequence int, validator *projectionpolicy.Validator) gitprojection.Document {
		t.Helper()
		headCharacter := string(rune('d' + sequence))
		head := strings.Repeat(headCharacter, 40)
		observedAt := now.Add(time.Duration(6+sequence*5) * time.Second)
		binding, _, err = store.RecordVerifiedHead(ctx, gitprojection.VerifiedHead{BindingID: binding.ID, Repository: binding.Repository,
			TargetRef: binding.TargetRef, Commit: head, Source: gitprojection.ObservationPoll,
			ProviderRequest: "policy-aggregate-" + headCharacter, ObservedAt: observedAt})
		if err != nil {
			t.Fatal(err)
		}
		generation, beginErr := store.BeginGeneration(ctx, work.Lease, head, binding.ParserVersion, observedAt.Add(time.Second))
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		parsed, _, diagnostics := appconfig.ParseAndValidate(plainRaw)
		if len(diagnostics) != 0 {
			t.Fatalf("aggregate AppConfig invalid: %#v", diagnostics)
		}
		document, documentErr := gitprojection.NewDocument(binding, generation.Number, applicationID, head, head,
			strings.Repeat(string(rune('7'+sequence)), 40), plainRaw, parsed, nil, observedAt.Add(2*time.Second))
		if documentErr != nil {
			t.Fatal(documentErr)
		}
		if err = store.PutDocuments(ctx, generation, []gitprojection.Document{document}); err != nil {
			t.Fatal(err)
		}
		if binding, err = store.ActivateGeneration(ctx, work.Lease, generation, validator, observedAt.Add(3*time.Second)); err != nil {
			t.Fatal(err)
		}
		return document
	}
	originalProjectName := "Policy project " + identity
	mutatedProjectName := "Policy reconciled " + identity
	invalidDocument := stageAggregate(0, &projectionpolicy.Validator{
		Secrets:  &projectNameMutationPolicy{projectID: projectID, name: mutatedProjectName},
		Registry: diagnosticReferencePolicy{},
	})
	assertProjectedDiagnostic(t, store, bindingID, invalidDocument.Path, false, "SyntheticLaterPolicyDiagnostic")
	var storedProjectName string
	if err = pool.QueryRow(ctx, `SELECT name FROM projects WHERE id=$1`, projectID).Scan(&storedProjectName); err != nil || storedProjectName != originalProjectName {
		t.Fatalf("diagnostic leaked earlier policy mutation: name=%q err=%v", storedProjectName, err)
	}
	validDocument := stageAggregate(1, &projectionpolicy.Validator{
		Secrets: &projectNameMutationPolicy{projectID: projectID, name: mutatedProjectName},
	})
	assertProjectedDiagnostic(t, store, bindingID, validDocument.Path, true, "")
	if err = pool.QueryRow(ctx, `SELECT name FROM projects WHERE id=$1`, projectID).Scan(&storedProjectName); err != nil || storedProjectName != mutatedProjectName {
		t.Fatalf("zero-diagnostic policy mutation was not committed: name=%q err=%v", storedProjectName, err)
	}
}

type projectNameMutationPolicy struct {
	projectID string
	name      string
}

func (p *projectNameMutationPolicy) ValidateCurrentTx(ctx context.Context, tx pgx.Tx, _ projectionpolicy.AppConfigPolicyDocument, _ time.Time) ([]gitprojection.Diagnostic, error) {
	_, err := tx.Exec(ctx, `UPDATE projects SET name=$2 WHERE id=$1`, p.projectID, p.name)
	return nil, err
}

func (*projectNameMutationPolicy) ReconcileDeletedTx(context.Context, pgx.Tx, projectionpolicy.DocumentScope, time.Time) error {
	return nil
}

type diagnosticReferencePolicy struct{}

func (diagnosticReferencePolicy) ValidateCurrentTx(context.Context, pgx.Tx, projectionpolicy.AppConfigPolicyDocument, time.Time) ([]gitprojection.Diagnostic, error) {
	return []gitprojection.Diagnostic{{Code: "SyntheticLaterPolicyDiagnostic", Detail: "A later policy rejected this exact document.", Pointer: "/spec/delivery"}}, nil
}

func (diagnosticReferencePolicy) ReconcileDeletedTx(context.Context, pgx.Tx, projectionpolicy.DocumentScope, time.Time) error {
	return nil
}

func TestPostgreSQLPolicyPlatformOwnedProjectNoRefsAndSemanticSecretDiagnostic(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = testdb.ApplyMigrations(ctx, pool); err != nil {
		t.Fatal(err)
	}
	projectID, environmentID, applicationID, bindingID := id.New(), id.New(), id.New(), id.New()
	identity := strings.ReplaceAll(projectID[:8], "-", "")
	now := time.Now().UTC().Truncate(time.Microsecond)
	cleanup := func(cleanupContext context.Context) {
		_, _ = pool.Exec(cleanupContext, `DELETE FROM git_projected_documents WHERE binding_id=$1`, bindingID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM git_projection_generations WHERE binding_id=$1`, bindingID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM git_safety_poll_cursors WHERE binding_id=$1`, bindingID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM git_verified_head_observations WHERE binding_id=$1`, bindingID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM git_repository_bindings WHERE id=$1`, bindingID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM applications WHERE id=$1`, applicationID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM environments WHERE id=$1`, environmentID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM projects WHERE id=$1`, projectID)
	}
	cleanup(ctx)
	defer cleanup(context.Background())
	if _, err = pool.Exec(ctx, `INSERT INTO projects(id,name,slug,team_id,created_at) VALUES($1,$2,$3,NULL,$4)`,
		projectID, "Platform project "+identity, "platform-project-"+identity, now); err != nil {
		t.Fatal(err)
	}
	namespace := "platform-" + identity
	if _, err = pool.Exec(ctx, `INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at)
		VALUES($1,$2,'Production','production',$3,$3,$4)`, environmentID, projectID, namespace, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO applications(id,project_id,name,slug,created_at) VALUES($1,$2,'API','api',$3)`, applicationID, projectID, now); err != nil {
		t.Fatal(err)
	}
	binding, err := gitprojection.NewGitHubEnvironmentBinding(bindingID, projectID, environmentID,
		gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 7_300_000_000_000_001, RepositoryID: 7_300_000_000_000_002, Owner: "kuberploy", Name: "platform-policy"},
		"refs/heads/main", now)
	if err != nil {
		t.Fatal(err)
	}
	store, err := gitprojection.NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.PutBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}
	work, err := store.ClaimReconciliation(ctx, "projection-platform-policy", now.Add(time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	secretConfig := secrets.DefaultRuntimeConfig()
	secretConfig.Enabled, secretConfig.Namespaces = true, []string{namespace}
	secretConfig.FingerprintSecretRef, secretConfig.SealingCertificateSecretRef = "runtime-fingerprint", "sealed-secrets-key"
	if err = secretConfig.Validate(); err != nil {
		t.Fatal(err)
	}
	policy := &projectionpolicy.Validator{Secrets: &projectionpolicy.RuntimeSecretReferencePolicy{Config: secretConfig}}
	project := domain.Project{ID: projectID, Slug: "platform-project-" + identity}
	environment := domain.Environment{ID: environmentID, ProjectID: projectID, Slug: "production"}
	application := domain.Application{ID: applicationID, ProjectID: projectID, Name: "API", Slug: "api"}
	deployment := domain.Deployment{Image: "registry.example/api@sha256:" + strings.Repeat("d", 64), Runtime: domain.DefaultWorkloadRuntime(8080, nil)}
	plainRaw, err := gitops.RenderAppConfig(project, environment, application, deployment)
	if err != nil {
		t.Fatal(err)
	}
	stage := func(sequence int, raw []byte, includeDocument bool) (gitprojection.Binding, gitprojection.Document) {
		t.Helper()
		head := strings.Repeat(string(rune('a'+sequence)), 40)
		binding, _, err = store.RecordVerifiedHead(ctx, gitprojection.VerifiedHead{BindingID: binding.ID, Repository: binding.Repository, TargetRef: binding.TargetRef,
			Commit: head, Source: gitprojection.ObservationPoll, ProviderRequest: "platform-policy-" + string(rune('a'+sequence)), ObservedAt: now.Add(time.Duration(2+sequence*4) * time.Second)})
		if err != nil {
			t.Fatal(err)
		}
		generation, beginErr := store.BeginGeneration(ctx, work.Lease, head, binding.ParserVersion, now.Add(time.Duration(3+sequence*4)*time.Second))
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		var document gitprojection.Document
		if includeDocument {
			parsed, _, diagnostics := appconfig.ParseAndValidate(raw)
			if len(diagnostics) != 0 {
				t.Fatalf("invalid test AppConfig: %#v", diagnostics)
			}
			document, err = gitprojection.NewDocument(binding, generation.Number, applicationID, head, head,
				strings.Repeat(string(rune('e'+sequence)), 40), raw, parsed, nil, now.Add(time.Duration(4+sequence*4)*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if err = store.PutDocuments(ctx, generation, []gitprojection.Document{document}); err != nil {
				t.Fatal(err)
			}
		} else if err = store.PutDocuments(ctx, generation, nil); err != nil {
			t.Fatal(err)
		}
		activated, activateErr := store.ActivateGeneration(ctx, work.Lease, generation, policy, now.Add(time.Duration(5+sequence*4)*time.Second))
		if activateErr != nil {
			t.Fatal(activateErr)
		}
		return activated, document
	}

	activated, plainDocument := stage(0, plainRaw, true)
	plainProjected, err := store.Document(ctx, binding.ID, plainDocument.Path)
	if err != nil || !plainProjected.Valid || len(plainProjected.Diagnostics) != 0 || activated.State != gitprojection.BindingReady {
		t.Fatalf("platform-owned config without references was rejected: binding=%#v document=%#v err=%v", activated, plainProjected, err)
	}
	secretCandidate := appconfig.Apply(plainRaw, appconfig.Change{Mode: "jsonPatch", Patch: []appconfig.PatchOperation{{
		Op: "add", Path: "/spec/runtime/env", Value: []any{map[string]any{
			"name": "DATABASE_PASSWORD", "valueFrom": map[string]any{"secretBindingRef": map[string]any{
				"bindingId": id.New(), "name": "database", "key": "password", "version": 1,
			}},
		}},
	}}})
	if len(secretCandidate.Diagnostics) != 0 {
		t.Fatalf("secret test candidate is invalid: %#v", secretCandidate.Diagnostics)
	}
	_, secretDocument := stage(1, secretCandidate.Raw, true)
	secretProjected, err := store.Document(ctx, binding.ID, secretDocument.Path)
	if err != nil || secretProjected.Valid || len(secretProjected.Diagnostics) != 1 || secretProjected.Diagnostics[0].Code != "RuntimeSecretReferenceUnresolved" {
		t.Fatalf("platform-owned reference was not a stable semantic diagnostic: %#v err=%v", secretProjected, err)
	}
	// User deletion removes the App catalog row before the asynchronous Git
	// deletion is indexed. Empty-generation activation must still reconcile the
	// previous document and return the binding to ready.
	if _, err = pool.Exec(ctx, `DELETE FROM applications WHERE id=$1`, applicationID); err != nil {
		t.Fatal(err)
	}
	deleted, _ := stage(2, nil, false)
	if deleted.State != gitprojection.BindingReady || deleted.IndexedRevision != strings.Repeat("c", 40) {
		t.Fatalf("platform-owned deleted path with no Git-current guards did not activate: %#v", deleted)
	}
}
