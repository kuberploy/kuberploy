package projectionpolicy

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/secrets"
)

func policyTestDocument(t testing.TB, scope DocumentScope, runtime domain.WorkloadRuntime) AppConfigPolicyDocument {
	t.Helper()
	runtime = domain.NormalizeWorkloadRuntime(runtime)
	if err := scope.Binding.Validate(); err != nil {
		t.Fatalf("invalid policy test binding: %v", err)
	}
	if problems := domain.ValidateWorkloadRuntime(runtime); len(problems) != 0 {
		t.Fatalf("invalid policy test runtime: %#v", problems)
	}
	parsed := map[string]any{"spec": map[string]any{"delivery": map[string]any{
		"mode": "image",
		"release": map[string]any{
			"repository": "registry.example.test/public/api",
			"digest":     "sha256:" + strings.Repeat("a", 64),
		},
	}}}
	document, err := newAppConfigPolicyDocument(scope, parsed, runtime)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func policyTestBasicAuthDocument(t testing.TB, scope DocumentScope, runtime domain.WorkloadRuntime, ref domain.SecretBindingRef) AppConfigPolicyDocument {
	t.Helper()
	parsed := map[string]any{"spec": map[string]any{
		"delivery": map[string]any{"mode": "image", "release": map[string]any{
			"repository": "registry.example.test/public/api", "digest": "sha256:" + strings.Repeat("a", 64),
		}},
		"middlewares": []any{map[string]any{"name": "login", "spec": map[string]any{"basicAuth": map[string]any{
			"secretBindingRef": map[string]any{"bindingId": ref.BindingID, "name": ref.Name, "key": ref.Key, "version": ref.Version},
		}}}},
	}}
	document, err := newAppConfigPolicyDocument(scope, parsed, domain.NormalizeWorkloadRuntime(runtime))
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func runtimeSecretPolicyConfig(t *testing.T) secrets.RuntimeConfig {
	t.Helper()
	config := secrets.DefaultRuntimeConfig()
	config.Enabled = true
	config.Namespaces = []string{"apps-production"}
	config.FingerprintSecretRef = "kuberploy-runtime-secret-fingerprint"
	config.SealingCertificateSecretRef = "sealed-secrets-key"
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	return config
}

func runtimeSecretDocumentScope(t *testing.T, namespace string) DocumentScope {
	t.Helper()
	now := time.Now().UTC()
	binding, err := gitprojection.NewGitHubEnvironmentBinding(
		"11111111-1111-4111-8111-111111111111",
		"22222222-2222-4222-8222-222222222222",
		"33333333-3333-4333-8333-333333333333",
		gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 1, RepositoryID: 2, Owner: "kuberploy", Name: "desired-state"},
		"refs/heads/main", now,
	)
	if err != nil {
		t.Fatal(err)
	}
	binding.TargetHeadRevision = strings.Repeat("a", 40)
	binding.TargetHeadObservedAt = now
	binding.State = gitprojection.BindingIndexing
	path, err := gitprojection.ApplicationPath(binding, "44444444-4444-4444-8444-444444444444")
	if err != nil {
		t.Fatal(err)
	}
	return DocumentScope{Binding: binding, OrganizationID: "55555555-5555-4555-8555-555555555555", Namespace: namespace,
		ApplicationID: "44444444-4444-4444-8444-444444444444", Path: path, SourceRevision: binding.TargetHeadRevision,
		ConfigRevision: strings.Repeat("b", 40), ContentSHA256: "sha256:" + strings.Repeat("c", 64)}
}

func TestRuntimeSecretReferencePolicyFailsClosedBeforeTransactionUse(t *testing.T) {
	policy := &RuntimeSecretReferencePolicy{Config: runtimeSecretPolicyConfig(t)}
	scope := runtimeSecretDocumentScope(t, "apps-other")
	runtime := domain.DefaultWorkloadRuntime(8080, nil)
	runtime.Env = []domain.WorkloadEnv{{Name: "PASSWORD", ValueFrom: &domain.WorkloadEnvValueFrom{SecretBindingRef: domain.SecretBindingRef{
		BindingID: "66666666-6666-4666-8666-666666666666", Name: "database", Key: "password", Version: 1,
	}}}}
	if _, err := policy.ValidateCurrentTx(t.Context(), nil, policyTestDocument(t, scope, runtime), time.Now().UTC()); !errors.Is(err, gitprojection.ErrInvalid) {
		t.Fatalf("nil transaction error=%v", err)
	}

	invalid := *policy
	invalid.Config.Namespaces = []string{"apps-production", "apps-production"}
	if _, err := invalid.ValidateCurrentTx(t.Context(), nil, policyTestDocument(t, scope, runtime), time.Now().UTC()); !errors.Is(err, gitprojection.ErrInvalid) {
		t.Fatalf("invalid configuration error=%v", err)
	}
}

func TestRuntimeSecretPolicyRequestIdentityIsStableAndSafe(t *testing.T) {
	scope := runtimeSecretDocumentScope(t, "apps-production")
	first := runtimeSecretPolicyRequestID(scope)
	if first != runtimeSecretPolicyRequestID(scope) || len(first) != len("runtime-secret-git:")+32 || strings.ContainsAny(first, " \t\r\n/") {
		t.Fatalf("request identity=%q", first)
	}
	scope.ConfigRevision = strings.Repeat("c", 40)
	if first == runtimeSecretPolicyRequestID(scope) {
		t.Fatal("config revision was omitted from request identity")
	}
}

func TestRuntimeSecretReferenceErrorClassification(t *testing.T) {
	for _, err := range []error{secrets.ErrInvalid, secrets.ErrNotFound, secrets.ErrConflict, secrets.ErrNotReady, secrets.ErrProviderMismatch} {
		if !semanticRuntimeSecretReferenceError(err) {
			t.Fatalf("semantic error %v was not classified", err)
		}
	}
	if semanticRuntimeSecretReferenceError(errors.New("database unavailable")) {
		t.Fatal("infrastructure error was downgraded to a diagnostic")
	}
}
