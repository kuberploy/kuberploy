package certificates

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/secrets"
)

var (
	testNow   = time.Date(2026, 8, 9, 9, 0, 0, 0, time.UTC)
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
	return secrets.FingerprintKey{ID: "certificate-hmac-v1", Bytes: append([]byte(nil), s.value...)}, nil
}

type fakeSealedProvider struct{}

func (fakeSealedProvider) StageStrictSealedSecret(_ context.Context, request secrets.StageRequest, _ *secrets.Material) (secrets.Artifact, error) {
	return secrets.Artifact{
		Provider: secrets.ProviderSealedSecrets, Namespace: request.Binding.Scope.Namespace,
		ObjectName: request.TargetSecretName, TargetSecretName: request.TargetSecretName, TargetSecretType: request.Version.TargetSecretType,
		ProviderRevision: "sealed-certificate-v1", ManifestDigest: "sha256:" + strings.Repeat("a", 64),
		SealedKeyFingerprint: "sha256:" + strings.Repeat("b", 64), CiphertextDigest: "sha256:" + strings.Repeat("c", 64),
	}, nil
}

func (fakeSealedProvider) ObserveStrictSealedSecret(_ context.Context, artifact secrets.Artifact) (secrets.ReadinessObservation, error) {
	return secrets.ReadinessObservation{Artifact: artifact, Status: secrets.ReadinessReady, ObservedAt: testNow.Add(time.Minute)}, nil
}

func (fakeSealedProvider) DeleteStrictSealedSecret(_ context.Context, artifact secrets.Artifact) (secrets.DeleteObservation, error) {
	return secrets.DeleteObservation{Artifact: artifact, Absent: true, ObservedAt: testNow.Add(2 * time.Minute)}, nil
}

func testCertificateService() (Service, secrets.Service, *secrets.MemoryStore, *MemoryStore) {
	secretStore := secrets.NewMemoryStore()
	provider := fakeSealedProvider{}
	secretService := secrets.Service{
		Store: secretStore, Keys: staticFingerprintKeys{value: []byte("0123456789abcdef0123456789abcdef")},
		SealedSecrets: provider, Now: func() time.Time { return testNow },
	}
	certificateStore := NewMemoryStore()
	service := Service{Secrets: &secretService, Catalog: secretStore, Store: certificateStore, Now: func() time.Time { return testNow }}
	return service, secretService, secretStore, certificateStore
}

func certificatePEM(t *testing.T, names []string, usages []x509.ExtKeyUsage) ([]byte, []byte) {
	t.Helper()
	caPublic, caPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafPublic, leafPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test root"},
		NotBefore: testNow.Add(-24 * time.Hour), NotAfter: testNow.Add(365 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, caPublic, caPrivate)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "ignored.example.test"},
		NotBefore: testNow.Add(-time.Hour), NotAfter: testNow.Add(90 * 24 * time.Hour),
		DNSNames: names, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: usages,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, leafPublic, caPrivate)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(leafPrivate)
	if err != nil {
		t.Fatal(err)
	}
	certificate := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})...)
	key := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	clear(keyDER)
	clear(leafDER)
	clear(caDER)
	return certificate, key
}

func newCertificateMaterial(t *testing.T, names ...string) *Material {
	t.Helper()
	certificate, key := certificatePEM(t, names, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	defer clear(certificate)
	defer clear(key)
	material, err := NewMaterial(certificate, key)
	if err != nil {
		t.Fatal(err)
	}
	return material
}

func TestCertificateLifecycleAttestsExactTLSVersionAndNeverSerializesMaterial(t *testing.T) {
	service, secretService, _, certificateStore := testCertificateService()
	material := newCertificateMaterial(t, "api.example.test", "*.apps.example.test")
	created, err := service.Create(context.Background(), CreateRequest{
		ActorID: testActor, Scope: testScope, Name: "public-edge", IdempotencyKey: "create-certificate-0001",
		RequestID: "certificate-create", Material: material,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Binding.Purpose != secrets.PurposeTLSCertificate || created.Version.TargetSecretType != secrets.TargetSecretTLS ||
		created.Certificate.ValidateFor(created.Binding, created.Version) != nil {
		t.Fatalf("created=%#v", created)
	}
	if !created.Certificate.CoversHost("api.example.test") || !created.Certificate.CoversHost("one.apps.example.test") ||
		created.Certificate.CoversHost("deep.one.apps.example.test") || created.Certificate.CoversHost("other.example.test") {
		t.Fatalf("SAN coverage=%#v", created.Certificate.DNSNames)
	}
	if _, err = json.Marshal(material); !errors.Is(err, ErrNoSerialize) {
		t.Fatalf("material serialized: %v", err)
	}
	for _, formatted := range []string{
		fmt.Sprintf("%s", material), fmt.Sprintf("%#v", material), fmt.Sprintf("%d", material), fmt.Sprintf("%x", material),
	} {
		if formatted != redactedMaterial {
			t.Fatalf("material formatting=%q", formatted)
		}
	}
	if !material.destroyed {
		t.Fatal("request material was not destroyed")
	}

	active, err := secretService.ReconcileVersion(context.Background(), created.Version.ID, "certificate-ready-v1")
	if err != nil || active.Version.State != secrets.VersionActive {
		t.Fatalf("active=%#v err=%v", active, err)
	}
	rotated, err := service.Rotate(context.Background(), RotateRequest{
		ActorID: testActor, BindingID: created.Binding.ID, ExpectedActiveVersion: 1,
		IdempotencyKey: "rotate-certificate-0001", RequestID: "certificate-rotate",
		Material: newCertificateMaterial(t, "new.example.test"),
	})
	if err != nil || rotated.Version.Number != 2 || rotated.Version.TargetSecretType != secrets.TargetSecretTLS ||
		rotated.Certificate.CoversHost("api.example.test") || !rotated.Certificate.CoversHost("new.example.test") {
		t.Fatalf("rotated=%#v err=%v", rotated, err)
	}
	versions, err := certificateStore.Versions(context.Background(), created.Binding.ID)
	if err != nil || len(versions) != 2 || versions[0].Number != 1 || versions[1].Number != 2 {
		t.Fatalf("versions=%#v err=%v", versions, err)
	}
}

func TestCertificateCreateReplayRecoversAttestationWithoutAnotherSecretVersion(t *testing.T) {
	service, _, secretStore, certificateStore := testCertificateService()
	certificate, key := certificatePEM(t, []string{"api.example.test"}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	defer clear(certificate)
	defer clear(key)
	request := func() CreateRequest {
		material, err := NewMaterial(certificate, key)
		if err != nil {
			t.Fatal(err)
		}
		return CreateRequest{ActorID: testActor, Scope: testScope, Name: "replay-edge", IdempotencyKey: "create-certificate-replay",
			RequestID: "certificate-replay", Material: material}
	}
	first, err := service.Create(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Create(context.Background(), request())
	if err != nil || !second.Replay || second.Binding.ID != first.Binding.ID || second.Certificate.SecretVersionID != first.Certificate.SecretVersionID {
		t.Fatalf("second=%#v err=%v", second, err)
	}
	secretVersions, err := secretStore.Versions(context.Background(), first.Binding.ID)
	if err != nil || len(secretVersions) != 1 {
		t.Fatalf("secret versions=%#v err=%v", secretVersions, err)
	}
	certificateVersions, err := certificateStore.Versions(context.Background(), first.Binding.ID)
	if err != nil || len(certificateVersions) != 1 {
		t.Fatalf("certificate versions=%#v err=%v", certificateVersions, err)
	}
}

func TestCertificateValidationRejectsMismatchClientOnlyMalformedAndUnsafeSAN(t *testing.T) {
	validCertificate, validKey := certificatePEM(t, []string{"api.example.test"}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	defer clear(validCertificate)
	defer clear(validKey)
	_, otherKey := certificatePEM(t, []string{"api.example.test"}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	defer clear(otherKey)
	clientCertificate, clientKey := certificatePEM(t, []string{"api.example.test"}, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	defer clear(clientCertificate)
	defer clear(clientKey)
	unsafeCertificate, unsafeKey := certificatePEM(t, []string{"*.*.example.test"}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	defer clear(unsafeCertificate)
	defer clear(unsafeKey)

	cases := []struct {
		name        string
		certificate []byte
		key         []byte
	}{
		{name: "mismatched key", certificate: validCertificate, key: otherKey},
		{name: "client only", certificate: clientCertificate, key: clientKey},
		{name: "unsafe wildcard", certificate: unsafeCertificate, key: unsafeKey},
		{name: "trailing key block", certificate: validCertificate, key: append(append([]byte(nil), validKey...), validKey...)},
		{name: "certificate junk", certificate: append(append([]byte(nil), validCertificate...), []byte("not pem")...), key: validKey},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			material, err := NewMaterial(test.certificate, test.key)
			if err != nil {
				t.Fatal(err)
			}
			defer material.Destroy()
			if _, err = parseAndValidate(material, testNow); !errors.Is(err, ErrInvalid) {
				t.Fatalf("accepted invalid certificate: %v", err)
			}
		})
	}
}

func TestCertificateAttestationRejectsSecretIdentitySubstitution(t *testing.T) {
	service, _, _, store := testCertificateService()
	result, err := service.Create(context.Background(), CreateRequest{
		ActorID: testActor, Scope: testScope, Name: "identity-edge", IdempotencyKey: "create-certificate-identity",
		RequestID: "certificate-identity", Material: newCertificateMaterial(t, "api.example.test"),
	})
	if err != nil {
		t.Fatal(err)
	}
	tampered := result.Certificate
	tampered.SecretContentFingerprint = sha256.Sum256([]byte("different keyed identity"))
	if _, _, err = store.Record(context.Background(), tampered, result.Binding, result.Version); !errors.Is(err, ErrInvalid) {
		t.Fatalf("accepted substituted fingerprint: %v", err)
	}
	tampered = result.Certificate
	tampered.DNSNames[0] = "other.example.test"
	if _, _, err = store.Record(context.Background(), tampered, result.Binding, result.Version); !errors.Is(err, ErrConflict) {
		t.Fatalf("accepted rebound attestation: %v", err)
	}
}
