package secrets

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testActor        = "10000000-0000-4000-8000-000000000001"
	testOrganization = "10000000-0000-4000-8000-000000000002"
	testProject      = "10000000-0000-4000-8000-000000000003"
	testEnvironment  = "10000000-0000-4000-8000-000000000004"
	testApplication  = "10000000-0000-4000-8000-000000000005"
)

var testTime = time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

type staticKeys struct{ key []byte }

func (k staticKeys) ActiveKey(context.Context) (FingerprintKey, error) {
	return FingerprintKey{ID: "runtime-secret-hmac-v1", Bytes: append([]byte(nil), k.key...)}, nil
}

type fakeProviders struct {
	mu                sync.Mutex
	stageCalls        int
	deleteCalls       int
	lastRequest       StageRequest
	seen              map[string]string
	status            ReadinessStatus
	failureCode       string
	mismatchStage     bool
	mismatchReadiness bool
	stageErr          error
	observeErr        error
	deleteErr         error
}

func (p *fakeProviders) stage(request StageRequest, material *Material) (Artifact, error) {
	if request.validate() != nil {
		return Artifact{}, ErrInvalid
	}
	seen := map[string]string{}
	if err := material.WithEntries(func(key string, value []byte) error {
		seen[key] = string(value)
		return nil
	}); err != nil {
		return Artifact{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stageCalls++
	p.lastRequest, p.seen = request, seen
	if p.stageErr != nil {
		return Artifact{}, p.stageErr
	}
	artifact := testArtifact(request.Binding.Provider, request.Binding.Scope.Namespace, request.TargetSecretName, request.Version.TargetSecretType)
	if p.mismatchStage {
		artifact.Namespace = "other-namespace"
	}
	return artifact, nil
}

func (p *fakeProviders) observe(artifact Artifact) (ReadinessObservation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.observeErr != nil {
		return ReadinessObservation{}, p.observeErr
	}
	status := p.status
	if status == "" {
		status = ReadinessReady
	}
	observed := artifact
	if p.mismatchReadiness {
		observed.TargetSecretName = "wrong-target"
	}
	return ReadinessObservation{Artifact: observed, Status: status, FailureCode: p.failureCode, ObservedAt: testTime.Add(time.Minute)}, nil
}

func (p *fakeProviders) remove(artifact Artifact) (DeleteObservation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deleteCalls++
	if p.deleteErr != nil {
		return DeleteObservation{}, p.deleteErr
	}
	return DeleteObservation{Artifact: artifact, Absent: true, ObservedAt: testTime.Add(2 * time.Minute)}, nil
}

func (p *fakeProviders) StageExternalSecret(_ context.Context, request StageRequest, material *Material) (Artifact, error) {
	if request.Binding.Provider != ProviderExternalSecrets || request.SealingScope != "" {
		return Artifact{}, ErrInvalid
	}
	return p.stage(request, material)
}
func (p *fakeProviders) ObserveExternalSecret(_ context.Context, artifact Artifact) (ReadinessObservation, error) {
	return p.observe(artifact)
}
func (p *fakeProviders) DeleteExternalSecret(_ context.Context, artifact Artifact) (DeleteObservation, error) {
	return p.remove(artifact)
}
func (p *fakeProviders) StageStrictSealedSecret(_ context.Context, request StageRequest, material *Material) (Artifact, error) {
	if request.Binding.Provider != ProviderSealedSecrets || request.SealingScope != StrictSealingScope {
		return Artifact{}, ErrInvalid
	}
	return p.stage(request, material)
}
func (p *fakeProviders) ObserveStrictSealedSecret(_ context.Context, artifact Artifact) (ReadinessObservation, error) {
	return p.observe(artifact)
}
func (p *fakeProviders) DeleteStrictSealedSecret(_ context.Context, artifact Artifact) (DeleteObservation, error) {
	return p.remove(artifact)
}

func testArtifact(provider ProviderKind, namespace, target string, targetType TargetSecretType) Artifact {
	artifact := Artifact{Provider: provider, Namespace: namespace, ObjectName: target + "-provider", TargetSecretName: target,
		TargetSecretType: targetType,
		ProviderRevision: "provider-version-1", ManifestDigest: "sha256:" + strings.Repeat("a", 64)}
	if provider == ProviderSealedSecrets {
		artifact.SealedKeyFingerprint = "sha256:" + strings.Repeat("b", 64)
		artifact.CiphertextDigest = "sha256:" + strings.Repeat("c", 64)
	}
	return artifact
}

func testScope() Scope {
	return Scope{OrganizationID: testOrganization, ProjectID: testProject, EnvironmentID: testEnvironment, ApplicationID: testApplication, Namespace: "runtime-test"}
}

func testDeliveries() []Delivery {
	return []Delivery{
		{SourceKey: "token", Kind: DeliveryFile, FilePath: "/var/run/secrets/kuberploy/auth/token", FileMode: 0o400},
		{SourceKey: "password", Kind: DeliveryEnvironment, EnvironmentName: "DATABASE_PASSWORD"},
	}
}

func testMaterial(t *testing.T, password string) *Material {
	t.Helper()
	material, err := NewMaterial(map[string][]byte{"password": []byte(password), "token": []byte("ghs_provider_token")})
	if err != nil {
		t.Fatal(err)
	}
	return material
}

func testService(store Store, providers *fakeProviders) Service {
	return Service{Store: store, Keys: staticKeys{key: []byte("0123456789abcdef0123456789abcdef")}, ExternalSecrets: providers, SealedSecrets: providers, Now: func() time.Time { return testTime }}
}

func createRequest(t *testing.T, provider ProviderKind, password, idem string) CreateRequest {
	t.Helper()
	return CreateRequest{ActorID: testActor, Scope: testScope(), Name: "database", Provider: provider, Deliveries: testDeliveries(),
		IdempotencyKey: idem, RequestID: "request-create-1", Material: testMaterial(t, password)}
}

func TestScopeAllowsPersonalProjectAndRejectsMalformedOrganization(t *testing.T) {
	personal := testScope()
	personal.OrganizationID = ""
	if err := personal.Validate(); err != nil {
		t.Fatalf("personal project scope: %v", err)
	}
	malformed := personal
	malformed.OrganizationID = "not-a-team-uuid"
	if !errors.Is(malformed.Validate(), ErrInvalid) {
		t.Fatalf("malformed organization was accepted: %#v", malformed)
	}
}

func TestWriteOnlyExternalSecretLifecycleRotationReferencesAndDelete(t *testing.T) {
	store := NewMemoryStore()
	provider := &fakeProviders{}
	service := testService(store, provider)
	material := testMaterial(t, "correct horse battery staple")
	request := CreateRequest{ActorID: testActor, Scope: testScope(), Name: "database", Provider: ProviderExternalSecrets,
		Deliveries: testDeliveries(), IdempotencyKey: "create-database-0001", RequestID: "request-create-1", Material: material}
	created, err := service.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if created.Replay || created.Binding.State != BindingProvisioning || created.Version.State != VersionAwaitingReadiness || created.Version.Number != 1 {
		t.Fatalf("created=%#v", created)
	}
	bindings, err := store.ListBindings(context.Background(), testApplication, testEnvironment)
	if err != nil || len(bindings) != 1 || bindings[0].ID != created.Binding.ID {
		t.Fatalf("bindings=%#v err=%v", bindings, err)
	}
	bindings, err = store.ListBindings(context.Background(), testApplication, "10000000-0000-4000-8000-000000000099")
	if err != nil || len(bindings) != 0 {
		t.Fatalf("cross-environment bindings=%#v err=%v", bindings, err)
	}
	if _, err = store.ListBindings(context.Background(), "not-an-id", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid list scope: %v", err)
	}
	if err = material.WithEntries(func(string, []byte) error { return nil }); !errors.Is(err, ErrPlaintextDestroyed) {
		t.Fatalf("material remains readable: %v", err)
	}
	provider.mu.Lock()
	if provider.seen["password"] != "correct horse battery staple" || provider.lastRequest.TargetSecretName != created.Version.Artifact.TargetSecretName {
		t.Fatalf("provider request=%#v seen=%#v", provider.lastRequest, provider.seen)
	}
	provider.mu.Unlock()
	encoded, err := json.Marshal(created)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"correct horse battery staple", "ghs_provider_token", base64.StdEncoding.EncodeToString([]byte("correct horse battery staple"))} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("plaintext/base64 disclosed: %s", encoded)
		}
	}
	rawSHA := sha256.Sum256([]byte("correct horse battery staple"))
	if created.Version.ContentFingerprint == rawSHA {
		t.Fatal("content fingerprint is an offline-guessable raw SHA-256")
	}
	active, err := service.ReconcileVersion(context.Background(), created.Version.ID, "controller-ready-1")
	if err != nil || active.Binding.State != BindingReady || active.Binding.ActiveVersion != 1 || active.Version.State != VersionActive {
		t.Fatalf("active=%#v err=%v", active, err)
	}

	rotated, err := service.Rotate(context.Background(), RotateRequest{ActorID: testActor, BindingID: active.Binding.ID, ExpectedActiveVersion: 1,
		Deliveries: testDeliveries(), IdempotencyKey: "rotate-database-0001", RequestID: "request-rotate-1", Material: testMaterial(t, "new password")})
	if err != nil || rotated.Version.Number != 2 || rotated.Version.State != VersionAwaitingReadiness {
		t.Fatalf("rotated=%#v err=%v", rotated, err)
	}
	rotated, err = service.ReconcileVersion(context.Background(), rotated.Version.ID, "controller-ready-2")
	if err != nil || rotated.Binding.ActiveVersion != 2 || rotated.Version.State != VersionActive {
		t.Fatalf("rotated active=%#v err=%v", rotated, err)
	}
	versions, err := store.Versions(context.Background(), active.Binding.ID)
	if err != nil || len(versions) != 2 || versions[0].State != VersionRetained || versions[1].State != VersionActive {
		t.Fatalf("versions=%#v err=%v", versions, err)
	}
	reference := Reference{BindingID: active.Binding.ID, VersionID: versions[0].ID, Kind: ReferenceRetainedRelease,
		Reference: "release-42", Revision: "sha256:" + strings.Repeat("d", 64)}
	if err = service.AddReference(context.Background(), testActor, "reference-add-1", reference); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Delete(context.Background(), testActor, active.Binding.ID, "delete-binding-1"); !errors.Is(err, ErrReferenced) {
		t.Fatalf("delete with retained release reference: %v", err)
	}
	if err = service.RemoveReference(context.Background(), testActor, active.Binding.ID, ReferenceRetainedRelease, "release-42", "reference-remove-1"); err != nil {
		t.Fatal(err)
	}
	deleted, err := service.Delete(context.Background(), testActor, active.Binding.ID, "delete-binding-2")
	if err != nil || deleted.State != BindingDeleted || deleted.ActiveVersion != 0 {
		t.Fatalf("deleted=%#v err=%v", deleted, err)
	}
	provider.mu.Lock()
	deleteCalls := provider.deleteCalls
	provider.mu.Unlock()
	if deleteCalls != 2 {
		t.Fatalf("delete calls=%d, want one per immutable provider artifact", deleteCalls)
	}
}

func TestCreateIdempotencyNeverRedisclosesOrCreatesAnotherVersion(t *testing.T) {
	store := NewMemoryStore()
	provider := &fakeProviders{}
	service := testService(store, provider)
	first, err := service.Create(context.Background(), createRequest(t, ProviderExternalSecrets, "same", "create-idempotent-1"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Create(context.Background(), createRequest(t, ProviderExternalSecrets, "same", "create-idempotent-1"))
	if err != nil || !second.Replay || second.Binding.ID != first.Binding.ID || second.Version.ID != first.Version.ID {
		t.Fatalf("second=%#v err=%v", second, err)
	}
	provider.mu.Lock()
	stageCalls := provider.stageCalls
	provider.mu.Unlock()
	if stageCalls != 1 {
		t.Fatalf("stage calls=%d", stageCalls)
	}
	_, err = service.Create(context.Background(), createRequest(t, ProviderExternalSecrets, "different", "create-idempotent-1"))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("rebound idempotency key: %v", err)
	}
	versions, err := store.Versions(context.Background(), first.Binding.ID)
	if err != nil || len(versions) != 1 {
		t.Fatalf("versions=%#v err=%v", versions, err)
	}
}

func TestConcurrentCreateConvergesOnOneImmutableVersion(t *testing.T) {
	store := NewMemoryStore()
	provider := &fakeProviders{}
	service := testService(store, provider)
	const workers = 24
	results := make(chan MutationResult, workers)
	errorsSeen := make(chan error, workers)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := service.Create(context.Background(), createRequest(t, ProviderExternalSecrets, "same", "create-concurrent-1"))
			results <- result
			errorsSeen <- err
		}()
	}
	wait.Wait()
	close(results)
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Errorf("concurrent create: %v", err)
		}
	}
	ids := map[string]struct{}{}
	for result := range results {
		ids[result.Version.ID] = struct{}{}
	}
	if len(ids) != 1 {
		t.Fatalf("version IDs=%v", ids)
	}
}

func TestStrictSealedSecretAndExactObservation(t *testing.T) {
	store := NewMemoryStore()
	provider := &fakeProviders{}
	service := testService(store, provider)
	created, err := service.Create(context.Background(), createRequest(t, ProviderSealedSecrets, "sealed-value", "create-sealed-0001"))
	if err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	scope := provider.lastRequest.SealingScope
	provider.mu.Unlock()
	if scope != StrictSealingScope || created.Version.Artifact == nil || created.Version.Artifact.CiphertextDigest == "" || created.Version.Artifact.SealedKeyFingerprint == "" {
		t.Fatalf("strict sealed artifact=%#v scope=%q", created.Version.Artifact, scope)
	}
	provider.mismatchReadiness = true
	if _, err = service.ReconcileVersion(context.Background(), created.Version.ID, "sealed-observe-mismatch"); !errors.Is(err, ErrProviderMismatch) {
		t.Fatalf("mismatched observation: %v", err)
	}
	stored, _ := store.Version(context.Background(), created.Version.ID)
	if stored.State != VersionAwaitingReadiness {
		t.Fatalf("mismatch activated version: %#v", stored)
	}
	provider.mismatchReadiness = false
	if result, readyErr := service.ReconcileVersion(context.Background(), created.Version.ID, "sealed-observe-ready"); readyErr != nil || result.Version.State != VersionActive {
		t.Fatalf("result=%#v err=%v", result, readyErr)
	}
}

func TestTLSSecretTypeIsImmutableAndSealedSecretsOnly(t *testing.T) {
	store := NewMemoryStore()
	provider := &fakeProviders{}
	service := testService(store, provider)
	deliveries := []Delivery{
		{SourceKey: "tls.crt", Kind: DeliveryFile, FilePath: "/var/run/secrets/kuberploy/tls/tls.crt", FileMode: 0o400},
		{SourceKey: "tls.key", Kind: DeliveryFile, FilePath: "/var/run/secrets/kuberploy/tls/tls.key", FileMode: 0o400},
	}
	material, err := NewMaterial(map[string][]byte{"tls.crt": []byte("certificate"), "tls.key": []byte("private-key")})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(context.Background(), CreateRequest{
		ActorID: testActor, Scope: testScope(), Name: "edge-certificate", Provider: ProviderSealedSecrets,
		Deliveries: deliveries, IdempotencyKey: "create-tls-secret-0001", RequestID: "create-tls-secret",
		Material: material, TargetSecretType: TargetSecretTLS, Purpose: PurposeTLSCertificate,
	})
	if err != nil || created.Version.TargetSecretType != TargetSecretTLS || created.Version.Artifact == nil ||
		created.Version.Artifact.TargetSecretType != TargetSecretTLS {
		t.Fatalf("created=%#v err=%v", created, err)
	}
	active, err := service.ReconcileVersion(context.Background(), created.Version.ID, "tls-secret-ready")
	if err != nil {
		t.Fatal(err)
	}
	rotateMaterial, err := NewMaterial(map[string][]byte{"tls.crt": []byte("new-certificate"), "tls.key": []byte("new-private-key")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Rotate(context.Background(), RotateRequest{
		ActorID: testActor, BindingID: active.Binding.ID, ExpectedActiveVersion: 1, Deliveries: deliveries,
		IdempotencyKey: "rotate-tls-as-opaque-1", RequestID: "rotate-tls-as-opaque", Material: rotateMaterial,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("TLS target type changed during rotation: %v", err)
	}

	externalMaterial, err := NewMaterial(map[string][]byte{"tls.crt": []byte("certificate"), "tls.key": []byte("private-key")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Create(context.Background(), CreateRequest{
		ActorID: testActor, Scope: testScope(), Name: "external-certificate", Provider: ProviderExternalSecrets,
		Deliveries: deliveries, IdempotencyKey: "create-external-tls-1", RequestID: "create-external-tls",
		Material: externalMaterial, TargetSecretType: TargetSecretTLS, Purpose: PurposeTLSCertificate,
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("external provider accepted TLS target: %v", err)
	}
}

func TestProviderRedirectFailsClosedAndPersistsOnlySafeFailure(t *testing.T) {
	store := NewMemoryStore()
	provider := &fakeProviders{mismatchStage: true}
	service := testService(store, provider)
	result, err := service.Create(context.Background(), createRequest(t, ProviderExternalSecrets, "do-not-store", "create-mismatch-001"))
	if !errors.Is(err, ErrProviderMismatch) || result.Version.State != VersionFailed || result.Binding.State != BindingFailed {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	stored, readErr := store.Version(context.Background(), result.Version.ID)
	if readErr != nil || stored.Artifact != nil || stored.FailureCode != "provider-stage-failed" {
		t.Fatalf("stored=%#v err=%v", stored, readErr)
	}
	events, eventsErr := store.PendingEvents(context.Background(), 20)
	if eventsErr != nil {
		t.Fatal(eventsErr)
	}
	encoded, _ := json.Marshal(struct {
		Version Version
		Events  []Event
	}{stored, events})
	if strings.Contains(string(encoded), "do-not-store") || strings.Contains(string(encoded), base64.StdEncoding.EncodeToString([]byte("do-not-store"))) {
		t.Fatalf("secret reached durable models: %s", encoded)
	}
}

func TestAdversarialDeliveryAndMaterialValidation(t *testing.T) {
	invalidDeliveries := []Delivery{
		{SourceKey: "password", Kind: DeliveryFile, FilePath: "/etc/shadow", FileMode: 0o400},
		{SourceKey: "password", Kind: DeliveryFile, FilePath: "/var/run/secrets/kuberploy/../escape", FileMode: 0o400},
		{SourceKey: "password", Kind: DeliveryFile, FilePath: "/var/run/secrets/kuberploy/a", FileMode: 0o777},
		{SourceKey: "password", Kind: DeliveryEnvironment, EnvironmentName: "BAD-NAME"},
	}
	for _, delivery := range invalidDeliveries {
		if delivery.Validate() == nil {
			t.Errorf("accepted %#v", delivery)
		}
	}
	material, err := NewMaterial(map[string][]byte{"password": []byte("value")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = json.Marshal(material); !errors.Is(err, ErrPlaintextSerialization) {
		t.Fatalf("material serialized: %v", err)
	}
	if fmt.Sprint(material) != "[REDACTED runtime-secret material]" || fmt.Sprintf("%#v", material) != "[REDACTED runtime-secret material]" {
		t.Fatalf("material formatting was not redacted: %s / %#v", material, material)
	}
	service := testService(NewMemoryStore(), &fakeProviders{})
	_, err = service.Create(context.Background(), CreateRequest{ActorID: testActor, Scope: testScope(), Name: "database", Provider: ProviderExternalSecrets,
		Deliveries: []Delivery{{SourceKey: "missing", Kind: DeliveryEnvironment, EnvironmentName: "PASSWORD"}}, IdempotencyKey: "missing-key-input-1", RequestID: "missing-key", Material: material})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown source key: %v", err)
	}
}

func TestPendingRotationBlocksCompetingRotation(t *testing.T) {
	store := NewMemoryStore()
	provider := &fakeProviders{}
	service := testService(store, provider)
	created, err := service.Create(context.Background(), createRequest(t, ProviderExternalSecrets, "v1", "create-pending-0001"))
	if err != nil {
		t.Fatal(err)
	}
	active, err := service.ReconcileVersion(context.Background(), created.Version.ID, "ready-pending-v1")
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.Rotate(context.Background(), RotateRequest{ActorID: testActor, BindingID: active.Binding.ID, ExpectedActiveVersion: 1,
		Deliveries: testDeliveries(), IdempotencyKey: "rotate-pending-0001", RequestID: "rotate-one", Material: testMaterial(t, "v2")})
	if err != nil || first.Version.State != VersionAwaitingReadiness {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	_, err = service.Rotate(context.Background(), RotateRequest{ActorID: testActor, BindingID: active.Binding.ID, ExpectedActiveVersion: 1,
		Deliveries: testDeliveries(), IdempotencyKey: "rotate-pending-0002", RequestID: "rotate-two", Material: testMaterial(t, "v3")})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("competing rotation: %v", err)
	}
}

func TestProviderErrorsAreNeverEchoed(t *testing.T) {
	provider := &fakeProviders{stageErr: errors.New("provider rejected plaintext=do-not-echo")}
	service := testService(NewMemoryStore(), provider)
	_, err := service.Create(context.Background(), createRequest(t, ProviderExternalSecrets, "do-not-echo", "provider-error-001"))
	if !errors.Is(err, ErrProviderOperation) || strings.Contains(err.Error(), "do-not-echo") || errors.Unwrap(err) != nil {
		t.Fatalf("provider error leaked: %v", err)
	}
}

func TestTargetSecretNameRemainsValidAtMaximumVersion(t *testing.T) {
	binding := Binding{ID: "20000000-0000-4000-8000-000000000001", Scope: testScope(), Name: strings.Repeat("a", 63),
		Provider: ProviderExternalSecrets, Purpose: PurposeRuntimeSecret, State: BindingReady, ActiveVersion: 1,
		CreatedBy: testActor, CreatedAt: testTime, UpdatedAt: testTime}
	name := TargetSecretName(binding, int64(^uint64(0)>>1))
	if len(name) > 63 || !dnsLabelRE.MatchString(name) {
		t.Fatalf("target name=%q len=%d", name, len(name))
	}
}
