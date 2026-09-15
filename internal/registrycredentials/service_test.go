package registrycredentials

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/secrets"
)

var (
	testNow   = time.Date(2026, 9, 15, 9, 0, 0, 0, time.UTC)
	testActor = "10000000-0000-4000-8000-000000000001"
	testScope = secrets.Scope{
		OrganizationID: "10000000-0000-4000-8000-000000000002",
		ProjectID:      "10000000-0000-4000-8000-000000000003",
		EnvironmentID:  "10000000-0000-4000-8000-000000000004",
		ApplicationID:  "10000000-0000-4000-8000-000000000005",
		Namespace:      "tenant-runtime",
	}
)

type staticFingerprintKeys struct{ value []byte }

func (s staticFingerprintKeys) ActiveKey(context.Context) (secrets.FingerprintKey, error) {
	return secrets.FingerprintKey{ID: "registry-credential-hmac-v1", Bytes: append([]byte(nil), s.value...)}, nil
}

type fakeSealedProvider struct{}

func (fakeSealedProvider) StageStrictSealedSecret(_ context.Context, request secrets.StageRequest, _ *secrets.Material) (secrets.Artifact, error) {
	return secrets.Artifact{
		Provider: secrets.ProviderSealedSecrets, Namespace: request.Binding.Scope.Namespace,
		ObjectName: request.TargetSecretName, TargetSecretName: request.TargetSecretName, TargetSecretType: request.Version.TargetSecretType,
		ProviderRevision: "sealed-registry-credential-v1", ManifestDigest: "sha256:" + strings.Repeat("a", 64),
		SealedKeyFingerprint: "sha256:" + strings.Repeat("b", 64), CiphertextDigest: "sha256:" + strings.Repeat("c", 64),
	}, nil
}

func (fakeSealedProvider) ObserveStrictSealedSecret(_ context.Context, artifact secrets.Artifact) (secrets.ReadinessObservation, error) {
	return secrets.ReadinessObservation{Artifact: artifact, Status: secrets.ReadinessReady, ObservedAt: testNow.Add(time.Minute)}, nil
}

func (fakeSealedProvider) DeleteStrictSealedSecret(_ context.Context, artifact secrets.Artifact) (secrets.DeleteObservation, error) {
	return secrets.DeleteObservation{Artifact: artifact, Absent: true, ObservedAt: testNow.Add(2 * time.Minute)}, nil
}

func testRegistryCredentialService() (Service, secrets.Service, *secrets.MemoryStore, *MemoryStore) {
	secretStore := secrets.NewMemoryStore()
	secretService := secrets.Service{
		Store: secretStore, Keys: staticFingerprintKeys{value: []byte("0123456789abcdef0123456789abcdef")},
		SealedSecrets: fakeSealedProvider{}, Now: func() time.Time { return testNow },
	}
	credentialStore := NewMemoryStore()
	service := Service{Secrets: &secretService, Catalog: secretStore, Store: credentialStore}
	return service, secretService, secretStore, credentialStore
}

func newCredentialMaterial(t *testing.T, host string) *Material {
	t.Helper()
	material, err := NewMaterial([]byte(host), []byte("ci-bot"), []byte("s3cr3t-token"), []byte("ci-bot@example.test"))
	if err != nil {
		t.Fatal(err)
	}
	return material
}

func TestMaterialRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name                         string
		host, username, password, em string
	}{
		{name: "empty host", host: "", username: "u", password: "p"},
		{name: "wildcard host", host: "*.example.test", username: "u", password: "p"},
		{name: "empty username", host: "registry.example.test", username: "", password: "p"},
		{name: "empty password", host: "registry.example.test", username: "u", password: ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewMaterial([]byte(test.host), []byte(test.username), []byte(test.password), []byte(test.em)); !errors.Is(err, ErrInvalid) {
				t.Fatalf("accepted invalid material: %v", err)
			}
		})
	}
}

func TestMaterialDockerConfigJSONShapeAndLowercasedHost(t *testing.T) {
	material, err := NewMaterial([]byte("Registry.Example.Test:5000"), []byte("ci-bot"), []byte("s3cr3t"), nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := material.dockerConfigJSON()
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Auths map[string]struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Auth     string `json:"auth"`
			Email    string `json:"email,omitempty"`
		} `json:"auths"`
	}
	if err = json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	entry, ok := decoded.Auths["registry.example.test:5000"]
	if !ok || len(decoded.Auths) != 1 {
		t.Fatalf("auths=%#v", decoded.Auths)
	}
	if entry.Username != "ci-bot" || entry.Password != "s3cr3t" || entry.Email != "" {
		t.Fatalf("entry=%#v", entry)
	}
	if entry.Auth == "" {
		t.Fatal("auth field was not populated")
	}
}

func TestMaterialDestroyClearsBytes(t *testing.T) {
	material := newCredentialMaterial(t, "registry.example.test")
	material.Destroy()
	if !material.destroyed {
		t.Fatal("material was not marked destroyed")
	}
	if _, err := material.dockerConfigJSON(); !errors.Is(err, ErrMaterialGone) {
		t.Fatalf("destroyed material still usable: %v", err)
	}
	material.Destroy() // idempotent
}

func TestServiceCreateSealsAndPersistsPublicMetadataOnly(t *testing.T) {
	service, secretService, secretStore, credentialStore := testRegistryCredentialService()
	material := newCredentialMaterial(t, "registry.example.test")
	created, err := service.Create(context.Background(), CreateRequest{
		ActorID: testActor, Scope: testScope, Name: "ci-registry", IdempotencyKey: "create-registry-credential-0001",
		RequestID: "registry-credential-create", Material: material,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Credential.Host != "registry.example.test" || created.Credential.Username != "ci-bot" {
		t.Fatalf("credential=%#v", created.Credential)
	}
	if !material.destroyed {
		t.Fatal("request material was not destroyed")
	}
	if created.Binding.Purpose != secrets.PurposeRegistryPullCredential || created.Version.TargetSecretType != secrets.TargetSecretDockerConfigJSON {
		t.Fatalf("binding=%#v version=%#v", created.Binding, created.Version)
	}

	active, err := secretService.ReconcileVersion(context.Background(), created.Version.ID, "registry-credential-ready-v1")
	if err != nil || active.Version.State != secrets.VersionActive {
		t.Fatalf("active=%#v err=%v", active, err)
	}
	rotated, err := service.Rotate(context.Background(), RotateRequest{
		ActorID: testActor, BindingID: created.Binding.ID, ExpectedActiveVersion: 1,
		IdempotencyKey: "rotate-registry-credential-0001", RequestID: "registry-credential-rotate",
		Material: newCredentialMaterial(t, "registry.example.test"),
	})
	if err != nil || rotated.Version.Number != 2 || rotated.Version.TargetSecretType != secrets.TargetSecretDockerConfigJSON {
		t.Fatalf("rotated=%#v err=%v", rotated, err)
	}
	versions, err := credentialStore.Versions(context.Background(), created.Binding.ID)
	if err != nil || len(versions) != 2 || versions[0].Number != 1 || versions[1].Number != 2 {
		t.Fatalf("versions=%#v err=%v", versions, err)
	}
	secretVersions, err := secretStore.Versions(context.Background(), created.Binding.ID)
	if err != nil || len(secretVersions) != 2 {
		t.Fatalf("secret versions=%#v err=%v", secretVersions, err)
	}
}

func TestServiceCreateReplayRecoversAttestationWithoutAnotherSecretVersion(t *testing.T) {
	service, _, secretStore, credentialStore := testRegistryCredentialService()
	request := func() CreateRequest {
		return CreateRequest{ActorID: testActor, Scope: testScope, Name: "replay-edge", IdempotencyKey: "create-registry-credential-replay",
			RequestID: "registry-credential-replay", Material: newCredentialMaterial(t, "registry.example.test")}
	}
	first, err := service.Create(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Create(context.Background(), request())
	if err != nil || !second.Replay || second.Binding.ID != first.Binding.ID || second.Credential.SecretVersionID != first.Credential.SecretVersionID {
		t.Fatalf("second=%#v err=%v", second, err)
	}
	secretVersions, err := secretStore.Versions(context.Background(), first.Binding.ID)
	if err != nil || len(secretVersions) != 1 {
		t.Fatalf("secret versions=%#v err=%v", secretVersions, err)
	}
	credentialVersions, err := credentialStore.Versions(context.Background(), first.Binding.ID)
	if err != nil || len(credentialVersions) != 1 {
		t.Fatalf("credential versions=%#v err=%v", credentialVersions, err)
	}
}

func TestAttestationRejectsSecretIdentitySubstitution(t *testing.T) {
	service, _, _, store := testRegistryCredentialService()
	result, err := service.Create(context.Background(), CreateRequest{
		ActorID: testActor, Scope: testScope, Name: "identity-edge", IdempotencyKey: "create-registry-credential-identity",
		RequestID: "registry-credential-identity", Material: newCredentialMaterial(t, "registry.example.test"),
	})
	if err != nil {
		t.Fatal(err)
	}
	tampered := result.Credential
	tampered.SecretContentFingerprint = sha256.Sum256([]byte("different keyed identity"))
	if _, _, err = store.Record(context.Background(), tampered, result.Binding, result.Version); !errors.Is(err, ErrInvalid) {
		t.Fatalf("accepted substituted fingerprint: %v", err)
	}
	tampered = result.Credential
	tampered.Host = "other.example.test"
	if _, _, err = store.Record(context.Background(), tampered, result.Binding, result.Version); !errors.Is(err, ErrConflict) {
		t.Fatalf("accepted rebound attestation: %v", err)
	}
}

func TestServiceDeleteRejectsWrongPurposeBinding(t *testing.T) {
	service, secretService, _, _ := testRegistryCredentialService()
	other, err := secretService.Create(context.Background(), secrets.CreateRequest{
		ActorID: testActor, Scope: testScope, Name: "unrelated-runtime-secret", Provider: secrets.ProviderSealedSecrets,
		Purpose: secrets.PurposeRuntimeSecret, TargetSecretType: secrets.TargetSecretOpaque,
		Deliveries:     []secrets.Delivery{{SourceKey: "VALUE", Kind: secrets.DeliveryEnvironment, EnvironmentName: "VALUE"}},
		IdempotencyKey: "create-runtime-secret-0001", RequestID: "runtime-secret-create",
		Material: mustMaterial(t, map[string][]byte{"VALUE": []byte("hello")}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Delete(context.Background(), testActor, other.Binding.ID, "registry-credential-delete-wrong-purpose"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted a non-registry-credential binding: %v", err)
	}
}

func mustMaterial(t *testing.T, values map[string][]byte) *secrets.Material {
	t.Helper()
	material, err := secrets.NewMaterial(values)
	if err != nil {
		t.Fatal(err)
	}
	return material
}
