package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type fakeSecretResources struct {
	objects      map[string]map[string]any
	next         int
	creates      []string
	deletes      []recordedSecretDelete
	retainDelete bool
}

type recordedSecretDelete struct {
	key           string
	preconditions secretDeletePreconditions
}

func newFakeSecretResources() *fakeSecretResources {
	return &fakeSecretResources{objects: map[string]map[string]any{}, next: 1}
}

func (f *fakeSecretResources) key(resource secretKubernetesResource, namespace, name string) string {
	return fmt.Sprintf("%d/%s/%s", resource, namespace, name)
}

func (f *fakeSecretResources) Get(_ context.Context, resource secretKubernetesResource, namespace, name string) (map[string]any, error) {
	object, ok := f.objects[f.key(resource, namespace, name)]
	if !ok {
		return nil, errSecretObjectNotFound
	}
	return cloneThroughJSON(object), nil
}

func (f *fakeSecretResources) Create(_ context.Context, resource secretKubernetesResource, namespace string, object map[string]any) (map[string]any, error) {
	key := f.key(resource, namespace, objectName(object))
	if _, exists := f.objects[key]; exists {
		return nil, errSecretObjectConflict
	}
	live := cloneThroughJSON(object)
	metadata := objectMetadata(live)
	metadata["uid"] = fmt.Sprintf("uid-%d", f.next)
	metadata["resourceVersion"] = fmt.Sprintf("%d", f.next)
	metadata["generation"] = int64(1)
	f.next++
	f.objects[key] = live
	f.creates = append(f.creates, key)
	return cloneThroughJSON(live), nil
}

func (f *fakeSecretResources) Delete(_ context.Context, resource secretKubernetesResource, namespace, name string, preconditions secretDeletePreconditions) error {
	key := f.key(resource, namespace, name)
	object, exists := f.objects[key]
	if !exists {
		return errSecretObjectNotFound
	}
	metadata := objectMetadata(object)
	if metadata["uid"] != preconditions.UID || metadata["resourceVersion"] != preconditions.ResourceVersion {
		return ErrProviderMismatch
	}
	f.deletes = append(f.deletes, recordedSecretDelete{key: key, preconditions: preconditions})
	if !f.retainDelete {
		delete(f.objects, key)
	}
	return nil
}

func cloneThroughJSON(value map[string]any) map[string]any {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.UseNumber()
	var cloned map[string]any
	if err = decoder.Decode(&cloned); err != nil {
		panic(err)
	}
	return normalizeSecretKubernetesJSON(cloned).(map[string]any)
}

func providerStageRequest(t *testing.T, provider ProviderKind) StageRequest {
	t.Helper()
	deliveries, err := normalizeDeliveries(testDeliveries())
	if err != nil {
		t.Fatal(err)
	}
	binding := Binding{
		ID: "20000000-0000-4000-8000-000000000001", Scope: testScope(), Name: "runtime-database", Provider: provider,
		Purpose: PurposeRuntimeSecret, State: BindingProvisioning, CreatedBy: testActor, CreatedAt: testTime, UpdatedAt: testTime,
	}
	version := Version{
		ID: "20000000-0000-4000-8000-000000000002", BindingID: binding.ID, Number: 1, Provider: provider, State: VersionStaging,
		Deliveries: deliveries, TargetSecretType: TargetSecretOpaque, CreatedAt: testTime, UpdatedAt: testTime, FingerprintKeyID: "runtime-secret-hmac-v1",
		ContentFingerprint: sha256.Sum256([]byte("test keyed content fingerprint")),
	}
	request := StageRequest{
		Binding: binding, Version: version, TargetSecretName: TargetSecretName(binding, 1), ExplicitKeys: deliveryKeys(deliveries), ImmutableTarget: true,
	}
	if provider == ProviderSealedSecrets {
		request.SealingScope = StrictSealingScope
	} else {
		request.ExternalRefreshPolicy = ExternalRefreshCreatedOnce
	}
	if request.validate() != nil {
		t.Fatal("invalid provider request fixture")
	}
	return request
}

type staticSealingKey struct{ key sealingPublicKey }

func (s staticSealingKey) ActivePublicKey(context.Context, time.Time) (sealingPublicKey, error) {
	return s.key, nil
}

func testSealingCertificate(t *testing.T) (*rsa.PrivateKey, sealingPublicKey) {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "kuberploy-test-sealing-key"},
		NotBefore: testTime.Add(-time.Hour), NotAfter: testTime.Add(24 * time.Hour), KeyUsage: x509.KeyUsageKeyEncipherment,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	encoded := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	parsed, err := parseSealingCertificate(encoded, testTime)
	clear(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return privateKey, parsed
}

func TestStrictSealedSecretsAdapterBindsCiphertextAndAdoptsExactRetry(t *testing.T) {
	privateKey, publicKey := testSealingCertificate(t)
	resources := newFakeSecretResources()
	adapter, err := newStrictSealedSecretsAdapter(resources, staticSealingKey{key: publicKey}, rand.Reader, func() time.Time { return testTime })
	if err != nil {
		t.Fatal(err)
	}
	request := providerStageRequest(t, ProviderSealedSecrets)
	material := testMaterial(t, "sealed plaintext must not escape")
	artifact, err := adapter.StageStrictSealedSecret(context.Background(), request, material)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Provider != ProviderSealedSecrets || artifact.ObjectName != request.TargetSecretName || artifact.Namespace != request.Binding.Scope.Namespace {
		t.Fatalf("artifact=%#v", artifact)
	}
	live := resources.objects[resources.key(resourceSealedSecret, artifact.Namespace, artifact.ObjectName)]
	encoded, _ := json.Marshal(live)
	if strings.Contains(string(encoded), "sealed plaintext must not escape") || strings.Contains(string(encoded), base64.StdEncoding.EncodeToString([]byte("sealed plaintext must not escape"))) {
		t.Fatal("plaintext or direct base64 plaintext persisted in SealedSecret")
	}
	annotations := objectMetadata(live)["annotations"].(map[string]any)
	if _, broad := annotations["sealedsecrets.bitnami.com/cluster-wide"]; broad || annotations[sealingKeyAnnotation] != artifact.SealedKeyFingerprint {
		t.Fatalf("strict sealing annotations=%#v", annotations)
	}
	encrypted := live["spec"].(map[string]any)["encryptedData"].(map[string]any)
	passwordCiphertext, err := base64.StdEncoding.DecodeString(encrypted["password"].(string))
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := openStrictValue(privateKey, []byte(artifact.Namespace+"/"+artifact.TargetSecretName), passwordCiphertext)
	if err != nil || string(plaintext) != "sealed plaintext must not escape" {
		t.Fatalf("strict decrypt failed: %v", err)
	}
	clear(plaintext)
	if _, err = openStrictValue(privateKey, []byte("other/"+artifact.TargetSecretName), passwordCiphertext); err == nil {
		t.Fatal("ciphertext decrypted under a different namespace scope")
	}
	clear(passwordCiphertext)

	retryMaterial := testMaterial(t, "sealed plaintext must not escape")
	replayed, err := adapter.StageStrictSealedSecret(context.Background(), request, retryMaterial)
	if err != nil || replayed != artifact || len(resources.creates) != 1 {
		t.Fatalf("retry=%#v creates=%d err=%v", replayed, len(resources.creates), err)
	}
}

func TestStrictSealedSecretsAdapterRejectsMutationObservesReadinessAndDeletesWithPreconditions(t *testing.T) {
	_, publicKey := testSealingCertificate(t)
	resources := newFakeSecretResources()
	adapter, _ := newStrictSealedSecretsAdapter(resources, staticSealingKey{key: publicKey}, rand.Reader, func() time.Time { return testTime })
	request := providerStageRequest(t, ProviderSealedSecrets)
	artifact, err := adapter.StageStrictSealedSecret(context.Background(), request, testMaterial(t, "sealed value"))
	if err != nil {
		t.Fatal(err)
	}
	key := resources.key(resourceSealedSecret, artifact.Namespace, artifact.ObjectName)
	live := resources.objects[key]
	live["status"] = map[string]any{
		"observedGeneration": int64(1), "conditions": []any{map[string]any{"type": "Synced", "status": "True"}},
	}
	observation, err := adapter.ObserveStrictSealedSecret(context.Background(), artifact)
	if err != nil || observation.Status != ReadinessReady || observation.Artifact != artifact {
		t.Fatalf("observation=%#v err=%v", observation, err)
	}
	live["spec"].(map[string]any)["template"].(map[string]any)["immutable"] = false
	if _, err = adapter.ObserveStrictSealedSecret(context.Background(), artifact); !errors.Is(err, ErrProviderMismatch) {
		t.Fatalf("mutated template accepted: %v", err)
	}
	live["spec"].(map[string]any)["template"].(map[string]any)["immutable"] = true
	crossScope := artifact
	crossScope.Namespace = "other-runtime"
	if _, err = adapter.DeleteStrictSealedSecret(context.Background(), crossScope); !errors.Is(err, ErrProviderMismatch) {
		t.Fatalf("cross-namespace artifact accepted: %v", err)
	}
	if _, err = adapter.DeleteStrictSealedSecret(context.Background(), artifact); err != nil {
		t.Fatal(err)
	}
	if len(resources.deletes) != 1 || resources.deletes[0].preconditions.UID == "" || resources.deletes[0].preconditions.ResourceVersion == "" {
		t.Fatalf("delete preconditions=%#v", resources.deletes)
	}
}

func TestStrictSealedSecretsAdapterRendersAndAdoptsExactTLSSecretType(t *testing.T) {
	_, publicKey := testSealingCertificate(t)
	resources := newFakeSecretResources()
	adapter, err := newStrictSealedSecretsAdapter(resources, staticSealingKey{key: publicKey}, rand.Reader, func() time.Time { return testTime })
	if err != nil {
		t.Fatal(err)
	}
	request := providerStageRequest(t, ProviderSealedSecrets)
	request.Binding.Purpose = PurposeTLSCertificate
	request.Version.TargetSecretType = TargetSecretTLS
	artifact, err := adapter.StageStrictSealedSecret(context.Background(), request, testMaterial(t, "tls material"))
	if err != nil || artifact.TargetSecretType != TargetSecretTLS {
		t.Fatalf("artifact=%#v err=%v", artifact, err)
	}
	live := resources.objects[resources.key(resourceSealedSecret, artifact.Namespace, artifact.ObjectName)]
	template := live["spec"].(map[string]any)["template"].(map[string]any)
	if template["type"] != string(TargetSecretTLS) {
		t.Fatalf("template type=%#v", template["type"])
	}
	template["type"] = string(TargetSecretOpaque)
	if _, err = adapter.ObserveStrictSealedSecret(context.Background(), artifact); !errors.Is(err, ErrProviderMismatch) {
		t.Fatalf("mutated target type accepted: %v", err)
	}
}

func openStrictValue(privateKey *rsa.PrivateKey, label, value []byte) ([]byte, error) {
	if len(value) < 3 {
		return nil, errors.New("short ciphertext")
	}
	keyLength := int(binary.BigEndian.Uint16(value[:2]))
	if keyLength == 0 || 2+keyLength >= len(value) {
		return nil, errors.New("invalid encrypted key length")
	}
	sessionKey, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, privateKey, value[2:2+keyLength], label)
	if err != nil {
		return nil, err
	}
	defer clear(sessionKey)
	block, err := aes.NewCipher(sessionKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, make([]byte, aead.NonceSize()), value[2+keyLength:], nil)
}

type testStoreProvider struct{}

func (testStoreProvider) validateExternalSecretStoreProvider() error { return nil }
func (testStoreProvider) externalSecretStoreProviderSpec() map[string]any {
	return map[string]any{"vault": map[string]any{
		"server": "https://vault.operator.invalid", "path": "kuberploy", "version": "v2",
		"auth": map[string]any{"kubernetes": map[string]any{"mountPath": "kubernetes", "role": "kuberploy-runtime"}},
	}}
}

type fakeRemoteWriter struct {
	stageCalls  int
	deleteCalls int
	sawValues   bool
	stageErr    error
	deleteErr   error
	deleteReady bool
}

type unprofiledRemoteWriter struct{}

func (unprofiledRemoteWriter) StageRemoteMaterial(context.Context, RemoteWriteRequest, *Material) (RemoteWriteResult, error) {
	return RemoteWriteResult{}, nil
}
func (unprofiledRemoteWriter) DeleteRemoteMaterial(context.Context, RemoteDeleteRequest) (bool, error) {
	return false, nil
}

func (w *fakeRemoteWriter) externalSecretStoreProfile() externalSecretStoreProfile {
	return externalSecretStoreProfile{provider: testStoreProvider{}}
}

func (w *fakeRemoteWriter) StageRemoteMaterial(_ context.Context, request RemoteWriteRequest, material *Material) (RemoteWriteResult, error) {
	w.stageCalls++
	if w.stageErr != nil {
		return RemoteWriteResult{}, w.stageErr
	}
	seen := []string{}
	if err := material.WithEntries(func(key string, value []byte) error {
		if len(value) == 0 {
			return ErrInvalid
		}
		w.sawValues = true
		seen = append(seen, key)
		return nil
	}); err != nil {
		return RemoteWriteResult{}, err
	}
	slices.Sort(seen)
	if !slices.Equal(seen, request.ExplicitKeys) {
		return RemoteWriteResult{}, ErrInvalid
	}
	references := make([]RemoteKeyReference, len(seen))
	for index, key := range seen {
		references[index] = RemoteKeyReference{SecretKey: key, RemoteKey: "runtime/" + request.VersionID, Property: key, Version: "version-0001"}
	}
	return RemoteWriteResult{Revision: "remote-revision-0001", References: references}, nil
}

func (w *fakeRemoteWriter) DeleteRemoteMaterial(context.Context, RemoteDeleteRequest) (bool, error) {
	w.deleteCalls++
	if w.deleteErr != nil {
		return false, w.deleteErr
	}
	return w.deleteReady, nil
}

func TestExternalSecretsAdapterRequiresProfileCreatesExactObjectsAndAdopts(t *testing.T) {
	if _, err := NewInClusterExternalSecretsProvider(unprofiledRemoteWriter{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unprofiled production writer accepted: %v", err)
	}
	if _, err := newExternalSecretsAdapter(newFakeSecretResources(), nil, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing remote writer accepted: %v", err)
	}
	resources := newFakeSecretResources()
	writer := &fakeRemoteWriter{deleteReady: true}
	adapter, err := newExternalSecretsAdapter(resources, writer, func() time.Time { return testTime })
	if err != nil {
		t.Fatal(err)
	}
	request := providerStageRequest(t, ProviderExternalSecrets)
	artifact, err := adapter.StageExternalSecret(context.Background(), request, testMaterial(t, "external value"))
	if err != nil {
		t.Fatal(err)
	}
	if !writer.sawValues || writer.stageCalls != 1 || artifact.Provider != ProviderExternalSecrets || artifact.CiphertextDigest != "" {
		t.Fatalf("writer/artifact mismatch: calls=%d artifact=%#v", writer.stageCalls, artifact)
	}
	external := resources.objects[resources.key(resourceExternalSecret, artifact.Namespace, artifact.ObjectName)]
	store := resources.objects[resources.key(resourceSecretStore, artifact.Namespace, externalStoreName(artifact.TargetSecretName))]
	storeSpec := store["spec"].(map[string]any)
	externalSpec := external["spec"].(map[string]any)
	if storeSpec["controller"] != externalControllerClass || externalSpec["refreshPolicy"] != ExternalRefreshCreatedOnce || externalSpec["refreshInterval"] != "0s" ||
		externalSpec["target"].(map[string]any)["immutable"] != true || externalSpec["secretStoreRef"].(map[string]any)["kind"] != "SecretStore" {
		t.Fatalf("store=%#v external=%#v", storeSpec, externalSpec)
	}
	if _, broad := externalSpec["dataFrom"]; broad {
		t.Fatal("ExternalSecret accepted broad dataFrom")
	}
	replayed, err := adapter.StageExternalSecret(context.Background(), request, testMaterial(t, "external value"))
	if err != nil || replayed != artifact || writer.stageCalls != 1 || len(resources.creates) != 2 {
		t.Fatalf("retry=%#v calls=%d creates=%d err=%v", replayed, writer.stageCalls, len(resources.creates), err)
	}
}

func TestExternalSecretsAdapterReadinessMutationAndDeletion(t *testing.T) {
	resources := newFakeSecretResources()
	writer := &fakeRemoteWriter{deleteReady: true}
	adapter, _ := newExternalSecretsAdapter(resources, writer, func() time.Time { return testTime })
	request := providerStageRequest(t, ProviderExternalSecrets)
	artifact, err := adapter.StageExternalSecret(context.Background(), request, testMaterial(t, "external value"))
	if err != nil {
		t.Fatal(err)
	}
	externalKey := resources.key(resourceExternalSecret, artifact.Namespace, artifact.ObjectName)
	storeKey := resources.key(resourceSecretStore, artifact.Namespace, externalStoreName(artifact.TargetSecretName))
	resources.objects[storeKey]["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
	resources.objects[externalKey]["status"] = map[string]any{
		"syncedResourceVersion": "remote-version-0001", "conditions": []any{map[string]any{"type": "Ready", "status": "True"}},
	}
	observation, err := adapter.ObserveExternalSecret(context.Background(), artifact)
	if err != nil || observation.Status != ReadinessReady {
		t.Fatalf("observation=%#v err=%v", observation, err)
	}
	resources.objects[storeKey]["spec"].(map[string]any)["controller"] = "attacker"
	if _, err = adapter.ObserveExternalSecret(context.Background(), artifact); !errors.Is(err, ErrProviderMismatch) {
		t.Fatalf("mutated controller accepted: %v", err)
	}
	resources.objects[storeKey]["spec"].(map[string]any)["controller"] = externalControllerClass
	if _, err = adapter.DeleteExternalSecret(context.Background(), artifact); err != nil {
		t.Fatal(err)
	}
	if len(resources.deletes) != 2 || writer.deleteCalls != 1 {
		t.Fatalf("deletes=%#v writer=%d", resources.deletes, writer.deleteCalls)
	}
}

func TestExternalSecretsDeletionResumesAfterExternalSecretWasAlreadyRemoved(t *testing.T) {
	resources := newFakeSecretResources()
	writer := &fakeRemoteWriter{deleteReady: true}
	adapter, _ := newExternalSecretsAdapter(resources, writer, func() time.Time { return testTime })
	request := providerStageRequest(t, ProviderExternalSecrets)
	artifact, err := adapter.StageExternalSecret(context.Background(), request, testMaterial(t, "external value"))
	if err != nil {
		t.Fatal(err)
	}
	delete(resources.objects, resources.key(resourceExternalSecret, artifact.Namespace, artifact.ObjectName))
	if _, err = adapter.DeleteExternalSecret(context.Background(), artifact); err != nil {
		t.Fatal(err)
	}
	if len(resources.deletes) != 1 || writer.deleteCalls != 1 || strings.Contains(resources.deletes[0].key, "/"+artifact.ObjectName) {
		t.Fatalf("partial cleanup did not resume safely: deletes=%#v writer=%d", resources.deletes, writer.deleteCalls)
	}
}

func TestProviderErrorsNeverEchoMaterial(t *testing.T) {
	resources := newFakeSecretResources()
	writer := &fakeRemoteWriter{stageErr: errors.New("backend leaked super-secret-value")}
	adapter, _ := newExternalSecretsAdapter(resources, writer, func() time.Time { return testTime })
	_, err := adapter.StageExternalSecret(context.Background(), providerStageRequest(t, ProviderExternalSecrets), testMaterial(t, "super-secret-value"))
	if !errors.Is(err, ErrProviderOperation) || strings.Contains(err.Error(), "super-secret-value") {
		t.Fatalf("provider material escaped: %v", err)
	}
}

func TestSecretKubernetesRESTIsClosedBoundedAndDoesNotEchoBodies(t *testing.T) {
	const token = "service-account.header.signature"
	const secret = "ciphertext-or-provider-secret-must-not-echo"
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+token || request.URL.Path != "/apis/bitnami.com/v1alpha1/namespaces/runtime-test/sealedsecrets" {
			t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
		}
		_, _ = io.Copy(io.Discard, request.Body)
		response.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = response.Write([]byte(`{"message":"` + secret + `"}`))
	}))
	defer server.Close()
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &inClusterSecretResources{baseURL: server.URL, http: server.Client(), tokenPath: tokenPath}
	object := map[string]any{
		"apiVersion": "bitnami.com/v1alpha1", "kind": "SealedSecret",
		"metadata": map[string]any{"name": "one", "namespace": "runtime-test"},
		"spec":     map[string]any{"encryptedData": map[string]any{"password": []byte(secret)}},
	}
	_, err := client.Create(context.Background(), resourceSealedSecret, "runtime-test", object)
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), base64.StdEncoding.EncodeToString([]byte(secret))) {
		t.Fatalf("Kubernetes body escaped: %v", err)
	}
	if _, err = secretResourcePath(secretKubernetesResource(99), "runtime-test", "one"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("arbitrary Kubernetes resource accepted: %v", err)
	}
}
