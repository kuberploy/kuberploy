package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

type exactReferenceProvider struct{ fakeProviders }

func (p *exactReferenceProvider) StageStrictSealedSecret(_ context.Context, request StageRequest, material *Material) (Artifact, error) {
	artifact, err := p.stage(request, material)
	if err == nil {
		artifact.ObjectName = request.TargetSecretName
	}
	return artifact, err
}

func referenceService(store Store, provider *exactReferenceProvider) Service {
	return Service{Store: store, Keys: staticKeys{key: []byte("0123456789abcdef0123456789abcdef")},
		SealedSecrets: provider, Now: func() time.Time { return testTime }}
}

func TestResolveBindingReferenceRequiresExactScopeActiveVersionAndDelivery(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	provider := &exactReferenceProvider{}
	service := referenceService(store, provider)
	created, err := service.Create(ctx, createRequest(t, ProviderSealedSecrets, "reference-only-value", "reference-resolve-01"))
	if err != nil {
		t.Fatal(err)
	}
	ref := domain.SecretBindingRef{BindingID: created.Binding.ID, Name: created.Binding.Name, Key: "password", Version: 1}
	delivery := Delivery{SourceKey: "password", Kind: DeliveryEnvironment, EnvironmentName: "DATABASE_PASSWORD"}
	if _, err = ResolveBindingReference(ctx, store, created.Binding.Scope, ref, delivery); !errors.Is(err, ErrNotReady) {
		t.Fatalf("awaiting version resolved: %v", err)
	}
	active, err := service.ReconcileVersion(ctx, created.Version.ID, "reference-ready-1")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveBindingReference(ctx, store, active.Binding.Scope, ref, delivery)
	if err != nil {
		t.Fatal(err)
	}
	expectedTarget := TargetSecretName(active.Binding, 1)
	if resolved.BindingID != active.Binding.ID || resolved.VersionID != active.Version.ID || resolved.Version != 1 ||
		resolved.Namespace != active.Binding.Scope.Namespace || resolved.TargetSecretName != expectedTarget || resolved.Key != "password" ||
		resolved.Delivery != delivery {
		t.Fatalf("resolved=%#v expected target=%q", resolved, expectedTarget)
	}
	encoded, err := json.Marshal(resolved)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"reference-only-value", "artifact", "manifestDigest", "ciphertext", "fingerprint", "providerRevision"} {
		if strings.Contains(strings.ToLower(string(encoded)), strings.ToLower(forbidden)) {
			t.Fatalf("unsafe resolved identity: %s", encoded)
		}
	}

	wrongScope := active.Binding.Scope
	wrongScope.ApplicationID = "10000000-0000-4000-8000-000000000099"
	if _, err = ResolveBindingReference(ctx, store, wrongScope, ref, delivery); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-application reference: %v", err)
	}
	wrongScope = active.Binding.Scope
	wrongScope.EnvironmentID = "10000000-0000-4000-8000-000000000099"
	if _, err = ResolveBindingReference(ctx, store, wrongScope, ref, delivery); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-environment reference: %v", err)
	}
	nameDrift := ref
	nameDrift.Name = "renamed"
	if _, err = ResolveBindingReference(ctx, store, active.Binding.Scope, nameDrift, delivery); !errors.Is(err, ErrNotFound) {
		t.Fatalf("binding name drift: %v", err)
	}
	wrongVersion := ref
	wrongVersion.Version = 2
	if _, err = ResolveBindingReference(ctx, store, active.Binding.Scope, wrongVersion, delivery); !errors.Is(err, ErrNotReady) {
		t.Fatalf("inactive version: %v", err)
	}
	wrongKey := ref
	wrongKey.Key = "other"
	if _, err = ResolveBindingReference(ctx, store, active.Binding.Scope, wrongKey,
		Delivery{SourceKey: "other", Kind: DeliveryEnvironment, EnvironmentName: "DATABASE_PASSWORD"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unauthorized key: %v", err)
	}
	if _, err = ResolveBindingReference(ctx, store, active.Binding.Scope, ref,
		Delivery{SourceKey: "password", Kind: DeliveryEnvironment, EnvironmentName: "OTHER_VARIABLE"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unauthorized delivery: %v", err)
	}
}

func TestResolveBindingReferenceRejectsRetainedAndMismatchedProviderIdentity(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	provider := &exactReferenceProvider{}
	service := referenceService(store, provider)
	created, err := service.Create(ctx, createRequest(t, ProviderSealedSecrets, "first", "reference-retain-01"))
	if err != nil {
		t.Fatal(err)
	}
	active, err := service.ReconcileVersion(ctx, created.Version.ID, "reference-retain-ready")
	if err != nil {
		t.Fatal(err)
	}
	ref := domain.SecretBindingRef{BindingID: active.Binding.ID, Name: active.Binding.Name, Key: "password", Version: 1}
	delivery := Delivery{SourceKey: "password", Kind: DeliveryEnvironment, EnvironmentName: "DATABASE_PASSWORD"}
	store.mu.Lock()
	corrupted := store.versions[active.Version.ID]
	corrupted.Artifact.ObjectName = "other-object"
	store.versions[active.Version.ID] = corrupted
	store.mu.Unlock()
	if _, err = ResolveBindingReference(ctx, store, active.Binding.Scope, ref, delivery); !errors.Is(err, ErrProviderMismatch) {
		t.Fatalf("mismatched provider identity: %v", err)
	}
	store.mu.Lock()
	corrupted = store.versions[active.Version.ID]
	corrupted.Artifact.ObjectName = TargetSecretName(active.Binding, 1)
	store.versions[active.Version.ID] = corrupted
	store.mu.Unlock()
	rotated, err := service.Rotate(ctx, RotateRequest{ActorID: testActor, BindingID: active.Binding.ID, ExpectedActiveVersion: 1,
		Deliveries: testDeliveries(), IdempotencyKey: "reference-retain-02", RequestID: "reference-rotate", Material: testMaterial(t, "second")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.ReconcileVersion(ctx, rotated.Version.ID, "reference-retain-ready-2"); err != nil {
		t.Fatal(err)
	}
	if _, err = ResolveBindingReference(ctx, store, active.Binding.Scope, ref, delivery); !errors.Is(err, ErrNotReady) {
		t.Fatalf("retained version resolved: %v", err)
	}
}

func TestRuntimeChartSecretNameMatchesTargetSecretName(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed")
	}
	chart := filepath.Join("..", "..", "charts", "kuberploy-runtime")
	fixture := filepath.Join(chart, "testdata", "workload-scheduling.yaml")
	assertRender := func(t *testing.T, values, bindingID, name string, version int64) {
		t.Helper()
		command := exec.Command(helm, "template", "secret-contract", chart, "-f", values)
		output, commandErr := command.CombinedOutput()
		if commandErr != nil {
			t.Fatalf("helm template: %v\n%s", commandErr, output)
		}
		expected := TargetSecretName(Binding{ID: bindingID, Name: name}, version)
		if !strings.Contains(string(output), "name: "+expected+"\n") {
			t.Fatalf("runtime chart did not render exact target %q:\n%s", expected, output)
		}
	}
	const bindingID = "44444444-4444-4444-8444-444444444444"
	assertRender(t, fixture, bindingID, "database", 3)

	raw, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	longName := "database-credentials-for-an-extremely-long-application-binding"
	longVersion := int64(9223372036854775807)
	changed := strings.Replace(string(raw), "name: database\n            key:", "name: "+longName+"\n            key:", 1)
	changed = strings.Replace(changed, "version: 3", "version: 9223372036854775807", 1)
	values := filepath.Join(t.TempDir(), "app.yaml")
	if err = os.WriteFile(values, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	assertRender(t, values, bindingID, longName, longVersion)

	missingID := strings.Replace(string(raw), "            bindingId: "+bindingID+"\n", "", 1)
	missingValues := filepath.Join(t.TempDir(), "missing-id.yaml")
	if err = os.WriteFile(missingValues, []byte(missingID), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, commandErr := exec.Command(helm, "template", "invalid", chart, "-f", missingValues).CombinedOutput(); commandErr == nil {
		t.Fatalf("chart accepted reference without bindingId:\n%s", output)
	}
	stringVersion := strings.Replace(string(raw), "version: 3", "version: v3", 1)
	stringValues := filepath.Join(t.TempDir(), "string-version.yaml")
	if err = os.WriteFile(stringValues, []byte(stringVersion), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, commandErr := exec.Command(helm, "template", "invalid", chart, "-f", stringValues).CombinedOutput(); commandErr == nil {
		t.Fatalf("chart accepted string version:\n%s", output)
	}
}

func TestRuntimeChartCustomCertificateNameMatchesTargetSecretName(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed")
	}
	chart := filepath.Join("..", "..", "charts", "kuberploy-runtime")
	fixture := filepath.Join(chart, "testdata", "custom-certificate.yaml")
	output, err := exec.Command(helm, "template", "custom-certificate-contract", chart, "-f", fixture,
		"--set-string", "kuberployExpectedIdentity.projectId=11111111-1111-4111-8111-111111111111",
		"--set-string", "kuberployExpectedIdentity.environmentId=22222222-2222-4222-8222-222222222222",
		"--set-string", "kuberployExpectedIdentity.applicationId=33333333-3333-4333-8333-333333333333").CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, output)
	}
	expected := TargetSecretName(Binding{ID: "44444444-4444-4444-8444-444444444444", Name: "route-certificate"}, 7)
	if expected != "kp-route-certificate-v7-57b5b21825" ||
		!strings.Contains(string(output), "secretName: "+expected+"\n") {
		t.Fatalf("runtime chart custom certificate did not render exact target %q:\n%s", expected, output)
	}
}
