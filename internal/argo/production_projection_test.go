package argo

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/certissuers"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitops"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

func TestDesiredStatePolicyMapsCertificateIssuerConflictToCandidateConflict(t *testing.T) {
	tests := []struct {
		name   string
		input  error
		mapped error
	}{
		{name: "issuer conflict", input: certissuers.ErrConflict, mapped: ErrConflict},
		{name: "issuer invalid", input: certissuers.ErrInvalid, mapped: ErrInvalid},
		{name: "policy conflict", input: gitprojection.ErrConflict, mapped: ErrConflict},
		{name: "policy invalid", input: gitprojection.ErrInvalid, mapped: ErrInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := classifyDesiredStatePolicyError(test.input)
			if !errors.Is(err, test.mapped) || !errors.Is(err, test.input) {
				t.Fatalf("policy error must remain a candidate error: %v", err)
			}
		})
	}
}

func desiredStatePolicyDocumentFixture(t testing.TB) (gitprojection.Binding, gitprojection.Document) {
	t.Helper()
	now := time.Now().UTC()
	const (
		bindingID     = "18111111-1111-4111-8111-111111111111"
		projectID     = "18211111-1111-4111-8111-111111111111"
		environmentID = "18311111-1111-4111-8111-111111111111"
		applicationID = "18411111-1111-4111-8111-111111111111"
		deploymentID  = "18511111-1111-4111-8111-111111111111"
	)
	binding, err := gitprojection.NewGitHubEnvironmentBinding(bindingID, projectID, environmentID,
		gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 101, RepositoryID: 102, Owner: "kuberploy", Name: "environment"},
		"refs/heads/main", now)
	if err != nil {
		t.Fatal(err)
	}
	revision := strings.Repeat("a", 40)
	binding.TargetHeadRevision, binding.IndexedRevision = revision, revision
	binding.TargetHeadObservedAt, binding.IndexedAt = now, now
	binding.ProjectionGeneration, binding.State, binding.UpdatedAt = 1, gitprojection.BindingReady, now
	project := domain.Project{ID: projectID, Name: "Projection policy", Slug: "projection-policy", CreatedAt: now}
	namespace, argoProject := domain.DeriveEnvironmentDestination(project, "production")
	environment := domain.Environment{ID: environmentID, ProjectID: projectID, Name: "Production", Slug: "production",
		Namespace: namespace, ArgoProject: argoProject, CreatedAt: now}
	application := domain.Application{ID: applicationID, ProjectID: projectID, Name: "API", Slug: "api", CreatedAt: now}
	runtime := domain.DefaultWorkloadRuntime(8080, nil)
	deployment := domain.Deployment{ID: deploymentID, EnvironmentID: environmentID, ApplicationID: applicationID,
		Image: "registry.example.test/api@sha256:" + strings.Repeat("1", 64), Replicas: 1, Port: 8080,
		Runtime: runtime, State: "ready", Generation: 1, CreatedAt: now, UpdatedAt: now}
	raw, err := gitops.RenderAppConfig(project, environment, application, deployment)
	if err != nil {
		t.Fatal(err)
	}
	document, err := gitprojection.NewDocument(binding, 1, applicationID, revision, revision, strings.Repeat("b", 40), raw,
		map[string]any{"schema": "already-indexed"}, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	return binding, document
}

func TestRevalidateDesiredStatePolicyDocumentDiscardsPersistedDynamicDiagnostic(t *testing.T) {
	binding, document := desiredStatePolicyDocumentFixture(t)
	document.Valid = false
	document.Diagnostics = []gitprojection.Diagnostic{{Code: "TraefikRuntimeUnobserved", Detail: "No fresh exact Traefik runtime observation is available for this route.", Pointer: "/spec/routes/0"}}

	revalidated, err := revalidateDesiredStatePolicyDocument(binding, document)
	if err != nil || !revalidated.Valid || len(revalidated.Diagnostics) != 0 || revalidated.Parsed == nil {
		t.Fatalf("schema-valid AppConfig did not recover for fresh dynamic policy evaluation: document=%#v err=%v", revalidated, err)
	}
}

func TestRevalidatedDesiredStatePolicyDocumentStillBlocksOnFreshDynamicDiagnostic(t *testing.T) {
	binding, document := desiredStatePolicyDocumentFixture(t)
	document.Valid = false
	document.Diagnostics = []gitprojection.Diagnostic{{Code: "TraefikRuntimeUnobserved", Detail: "persisted", Pointer: "/spec/routes/0"}}
	revalidated, err := revalidateDesiredStatePolicyDocument(binding, document)
	if err != nil {
		t.Fatal(err)
	}
	fresh := gitprojection.AppConfigPolicyValidation{Diagnostics: map[string][]gitprojection.Diagnostic{
		revalidated.Path: {{Code: "TraefikRuntimeUnobserved", Detail: "No fresh exact Traefik runtime observation is available for this route.", Pointer: "/spec/routes/0"}},
	}}
	if err = desiredStatePolicyValidationReady(fresh); !errors.Is(err, ErrConflict) {
		t.Fatalf("fresh dynamic diagnostic did not block desired state: %v", err)
	}
}

func TestRevalidateDesiredStatePolicyDocumentRejectsSchemaInvalidAppConfig(t *testing.T) {
	binding, document := desiredStatePolicyDocumentFixture(t)
	document, err := gitprojection.NewDocument(binding, document.Generation, document.ApplicationID, document.SourceRevision,
		document.ConfigRevision, document.BlobID, []byte("apiVersion: config.kuberploy.io/v1alpha1\nkind: AppConfig\nspec: []\n"), nil,
		[]gitprojection.Diagnostic{{Code: "SchemaViolation", Detail: "spec must be an object", Pointer: "/spec"}}, document.IndexedAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = revalidateDesiredStatePolicyDocument(binding, document); !errors.Is(err, ErrConflict) {
		t.Fatalf("schema-invalid AppConfig accepted: %v", err)
	}
}

func TestRevalidateDesiredStatePolicyDocumentRejectsSchemaValidBindingMismatch(t *testing.T) {
	binding, document := desiredStatePolicyDocumentFixture(t)
	wrongApplicationID := "18611111-1111-4111-8111-111111111111"
	raw := []byte(strings.ReplaceAll(string(document.Raw), document.ApplicationID, wrongApplicationID))
	document, err := gitprojection.NewDocument(binding, document.Generation, document.ApplicationID, document.SourceRevision,
		document.ConfigRevision, document.BlobID, raw, nil,
		[]gitprojection.Diagnostic{{Code: "BindingMismatch", Detail: "The document identity does not match its server-owned Git binding and path.", Pointer: "/metadata/id"}}, document.IndexedAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = revalidateDesiredStatePolicyDocument(binding, document); !errors.Is(err, ErrConflict) {
		t.Fatalf("schema-valid AppConfig binding mismatch accepted: %v", err)
	}
}

func TestRevalidateDesiredStatePolicyDocumentKeepsDependencyDiagnosticsStrict(t *testing.T) {
	binding, document := desiredStatePolicyDocumentFixture(t)
	paths, err := gitprojection.DependencyPaths(binding)
	if err != nil || len(paths) == 0 {
		t.Fatalf("dependency paths unavailable: %#v err=%v", paths, err)
	}
	dependency, err := gitprojection.NewDependencyDocument(binding, document.Generation, paths[0], document.SourceRevision,
		document.ConfigRevision, strings.Repeat("c", 40), []byte("apiVersion: variables.kuberploy.io/v1alpha1\nkind: VariableSet\nspec: []\n"), nil,
		[]gitprojection.Diagnostic{{Code: "SchemaViolation", Detail: "spec must be an object", Pointer: "/spec"}}, document.IndexedAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = revalidateDesiredStatePolicyDocument(binding, dependency); !errors.Is(err, ErrConflict) {
		t.Fatalf("invalid dependency document accepted: %v", err)
	}
}
