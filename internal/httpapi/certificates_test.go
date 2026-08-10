package httpapi_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/certificates"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	"github.com/kuberploy/kuberploy/internal/secrets"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

var certificateHTTPNow = time.Date(2026, 8, 9, 9, 0, 0, 0, time.UTC)

type certificateAPIFixture struct {
	*apiFixture
	service       certificates.Service
	secretService secrets.Service
	admin         domain.User
	team          domain.Team
	project       domain.Project
	environment   domain.Environment
	application   domain.Application
}

func newCertificateAPI(t *testing.T, backendEnabled, readinessEnabled bool, readinessErr error) *certificateAPIFixture {
	t.Helper()
	central := memory.New()
	secretStore := secrets.NewMemoryStore()
	provider := &httpSecretProvider{}
	secretService := secrets.Service{Store: secretStore, Keys: httpSecretKeys{}, SealedSecrets: provider, Now: func() time.Time { return certificateHTTPNow }}
	service := certificates.Service{Secrets: &secretService, Catalog: secretStore, Store: certificates.NewMemoryStore(), Now: func() time.Time { return certificateHTTPNow }}
	var backend httpapi.CertificateManagementBackend
	if backendEnabled {
		var err error
		backend, err = httpapi.NewCertificateManagementBackend(service, secretStore)
		if err != nil {
			t.Fatal(err)
		}
	}
	var readiness httpapi.ReadinessProbe
	if readinessEnabled {
		readiness = httpSecretReadiness{err: readinessErr}
	}
	server := httptest.NewServer(httpapi.New(httpapi.Options{
		Store: central, BootstrapToken: "one-time-secret", Version: "test", Certificates: backend,
		CertificateReadiness: readiness, HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000),
	}))
	jar, _ := cookiejar.New(nil)
	base := &apiFixture{t: t, server: server, client: &http.Client{Jar: jar}, store: central}
	t.Cleanup(server.Close)
	fixture := &certificateAPIFixture{apiFixture: base, service: service, secretService: secretService}
	fixture.admin = fixture.bootstrap()
	fixture.provisionScope()
	return fixture
}

func (f *certificateAPIFixture) provisionScope() {
	r := f.request(http.MethodPost, "/v1/teams", "certificate-team", map[string]string{"name": "Certificates", "slug": "certificates"})
	f.team = decode[domain.Team](f.t, r)
	if r.StatusCode != http.StatusCreated {
		f.t.Fatalf("team status=%d", r.StatusCode)
	}
	r = f.request(http.MethodPost, "/v1/projects", "certificate-project", map[string]string{"name": "Certificate project", "slug": "certificate-project", "teamId": f.team.ID})
	f.project = decode[domain.Project](f.t, r)
	if r.StatusCode != http.StatusCreated {
		f.t.Fatalf("project status=%d", r.StatusCode)
	}
	r = f.request(http.MethodPost, "/v1/environments", "certificate-environment", map[string]string{"projectId": f.project.ID, "name": "Production", "slug": "production"})
	f.environment = decode[domain.Environment](f.t, r)
	if r.StatusCode != http.StatusCreated {
		f.t.Fatalf("environment status=%d", r.StatusCode)
	}
	r = f.request(http.MethodPost, "/v1/applications", "certificate-application", map[string]string{"projectId": f.project.ID, "name": "API", "slug": "api"})
	f.application = decode[domain.Application](f.t, r)
	if r.StatusCode != http.StatusCreated {
		f.t.Fatalf("application status=%d", r.StatusCode)
	}
}

func certificateHTTPPEM(t *testing.T, names ...string) ([]byte, []byte) {
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
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "HTTP test root"},
		NotBefore: certificateHTTPNow.Add(-24 * time.Hour), NotAfter: certificateHTTPNow.Add(365 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, caPublic, caPrivate)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "ignored.example.test"},
		NotBefore: certificateHTTPNow.Add(-time.Hour), NotAfter: certificateHTTPNow.Add(90 * 24 * time.Hour),
		DNSNames: names, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, leafPublic, caPrivate)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(leafPrivate)
	if err != nil {
		t.Fatal(err)
	}
	certificate := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})...)
	key := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	clear(keyDER)
	clear(leafDER)
	clear(caDER)
	return certificate, key
}

func certificatePayload(environmentID, name string, certificate, key []byte) map[string]any {
	return map[string]any{"environmentId": environmentID, "name": name, "certificatePem": string(certificate), "privateKeyPem": string(key)}
}

func assertCertificateResponseSafe(t *testing.T, response *http.Response, expected int, forbidden ...string) []byte {
	t.Helper()
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Pragma") != "no-cache" {
		t.Fatalf("certificate response is cacheable: %#v", response.Header)
	}
	for _, value := range forbidden {
		if value != "" && bytes.Contains(body, []byte(value)) {
			t.Fatal("certificate response disclosed write-only material (body intentionally not logged)")
		}
	}
	for _, field := range []string{`"certificatePem"`, `"privateKeyPem"`, `"secretVersionId"`, `"targetSecretName"`, `"provider"`, `"artifact"`, `"ciphertext"`, `"manifestDigest"`, `"namespace"`, `"organizationId"`, `"projectId"`} {
		if bytes.Contains(body, []byte(field)) {
			t.Fatalf("certificate response disclosed forbidden field %s", field)
		}
	}
	if response.StatusCode != expected {
		t.Fatalf("status=%d, want=%d (body intentionally not logged)", response.StatusCode, expected)
	}
	return body
}

func TestCertificateHTTPWriteOnlyLifecycleMetadataAndAuditEvents(t *testing.T) {
	f := newCertificateAPI(t, true, true, nil)
	certificate, key := certificateHTTPPEM(t, "api.example.test", "*.apps.example.test")
	defer clear(certificate)
	defer clear(key)
	path := "/v1/applications/" + f.application.ID + "/certificate-bindings"
	payload := certificatePayload(f.environment.ID, "public-edge", certificate, key)
	response := f.request(http.MethodPost, path, "certificate-create-0001", payload)
	body := assertCertificateResponseSafe(t, response, http.StatusCreated, string(certificate), string(key), base64.StdEncoding.EncodeToString(key))
	var created struct {
		ID            string `json:"id"`
		State         string `json:"state"`
		ActiveVersion int64  `json:"activeVersion"`
		Versions      []struct {
			Number   int64    `json:"number"`
			DNSNames []string `json:"dnsNames"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.State != "provisioning" || len(created.Versions) != 1 || created.Versions[0].Number != 1 || !slicesContain(created.Versions[0].DNSNames, "api.example.test") {
		t.Fatalf("unexpected safe certificate metadata: %#v", created)
	}

	replay := f.request(http.MethodPost, path, "certificate-create-0001", payload)
	if replay.Header.Get("Idempotent-Replay") != "true" {
		t.Fatal("matching certificate create did not report its immutable replay")
	}
	assertCertificateResponseSafe(t, replay, http.StatusCreated, string(certificate), string(key))

	listed := f.request(http.MethodGet, path+"?environmentId="+f.environment.ID, "", nil)
	listBody := assertCertificateResponseSafe(t, listed, http.StatusOK, string(certificate), string(key))
	if !bytes.Contains(listBody, []byte(created.ID)) || bytes.Contains(listBody, []byte(`"versions"`)) {
		t.Fatalf("certificate list did not stay metadata-only: %s", listBody)
	}
	got := f.request(http.MethodGet, "/v1/certificate-bindings/"+created.ID, "", nil)
	getBody := assertCertificateResponseSafe(t, got, http.StatusOK, string(certificate), string(key))
	if !bytes.Contains(getBody, []byte(`"leafFingerprint":"sha256:`)) || !bytes.Contains(getBody, []byte(`"versions"`)) {
		t.Fatalf("certificate detail omitted public attestation: %s", getBody)
	}

	secretVersions, err := f.secretService.Store.Versions(context.Background(), created.ID)
	if err != nil || len(secretVersions) != 1 {
		t.Fatalf("secret versions=%#v err=%v", secretVersions, err)
	}
	active, err := f.secretService.ReconcileVersion(context.Background(), secretVersions[0].ID, "certificate-http-ready-1")
	if err != nil || active.Binding.ActiveVersion != 1 {
		t.Fatalf("activate=%#v err=%v", active, err)
	}

	rotatedCertificate, rotatedKey := certificateHTTPPEM(t, "new.example.test")
	defer clear(rotatedCertificate)
	defer clear(rotatedKey)
	rotation := map[string]any{"expectedActiveVersion": 1, "certificatePem": string(rotatedCertificate), "privateKeyPem": string(rotatedKey)}
	response = f.request(http.MethodPost, "/v1/certificate-bindings/"+created.ID+"/versions", "certificate-rotate-0001", rotation)
	rotatedBody := assertCertificateResponseSafe(t, response, http.StatusCreated, string(rotatedCertificate), string(rotatedKey))
	if !bytes.Contains(rotatedBody, []byte(`"number":2`)) || !bytes.Contains(rotatedBody, []byte(`"new.example.test"`)) {
		t.Fatalf("rotation metadata=%s", rotatedBody)
	}
	secretVersions, err = f.secretService.Store.Versions(context.Background(), created.ID)
	if err != nil || len(secretVersions) != 2 {
		t.Fatalf("rotated secret versions=%#v err=%v", secretVersions, err)
	}
	active, err = f.secretService.ReconcileVersion(context.Background(), secretVersions[1].ID, "certificate-http-ready-2")
	if err != nil || active.Binding.ActiveVersion != 2 {
		t.Fatalf("activate rotation=%#v err=%v", active, err)
	}

	response = f.request(http.MethodDelete, "/v1/certificate-bindings/"+created.ID, "certificate-delete-0001", nil)
	assertCertificateResponseSafe(t, response, http.StatusNoContent)
	pending, err := f.secretService.Store.PendingEvents(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	seenActorRequest := false
	for _, event := range pending {
		if event.ActorID == f.admin.ID && event.RequestID != "" {
			seenActorRequest = true
		}
	}
	if !seenActorRequest {
		t.Fatal("certificate lifecycle did not persist actor/request-bound audit events")
	}
}

func TestCertificateHTTPRejectsUnsafeBodiesQueriesAndBearerSessions(t *testing.T) {
	f := newCertificateAPI(t, true, true, nil)
	certificate, key := certificateHTTPPEM(t, "api.example.test")
	defer clear(certificate)
	defer clear(key)
	path := "/v1/applications/" + f.application.ID + "/certificate-bindings"

	raw := `{"environmentId":"` + f.environment.ID + `","name":"first","name":"second","certificatePem":` + mustJSON(t, string(certificate)) + `,"privateKeyPem":` + mustJSON(t, string(key)) + `}`
	response := f.rawCertificateRequest(http.MethodPost, path, "certificate-duplicate-1", raw, "application/json")
	assertCertificateResponseSafe(t, response, http.StatusBadRequest, string(certificate), string(key))

	unknown := certificatePayload(f.environment.ID, "unknown-field", certificate, key)
	unknown["targetSecretName"] = "caller-chosen-secret"
	response = f.request(http.MethodPost, path, "certificate-unknown-01", unknown)
	assertCertificateResponseSafe(t, response, http.StatusBadRequest, string(certificate), string(key), "caller-chosen-secret")

	validJSON, _ := json.Marshal(certificatePayload(f.environment.ID, "trailing", certificate, key))
	response = f.rawCertificateRequest(http.MethodPost, path, "certificate-trailing-1", string(validJSON)+` {}`, "application/json")
	assertCertificateResponseSafe(t, response, http.StatusBadRequest, string(certificate), string(key))

	response = f.request(http.MethodPost, path, "", certificatePayload(f.environment.ID, "missing-idempotency", certificate, key))
	assertCertificateResponseSafe(t, response, http.StatusBadRequest, string(certificate), string(key))

	response = f.request(http.MethodPost, path, "certificate-invalid-pem", certificatePayload(f.environment.ID, "invalid-pem", []byte("not a certificate"), key))
	assertCertificateResponseSafe(t, response, http.StatusUnprocessableEntity, "not a certificate", string(key))

	oversized := strings.Repeat("x", certificates.MaxCertificatePEMBytes+1)
	response = f.request(http.MethodPost, path, "certificate-oversized-1", map[string]any{"environmentId": f.environment.ID, "name": "oversized", "certificatePem": oversized, "privateKeyPem": string(key)})
	assertCertificateResponseSafe(t, response, http.StatusUnprocessableEntity, oversized[:128], string(key))

	for _, query := range []string{"?unknown=value", "?environmentId=" + f.environment.ID + "&environmentId=" + f.environment.ID} {
		response = f.request(http.MethodGet, path+query, "", nil)
		assertCertificateResponseSafe(t, response, http.StatusBadRequest)
	}

	createdResponse := f.request(http.MethodPost, path, "certificate-bearer-01", certificatePayload(f.environment.ID, "bearer-check", certificate, key))
	createdBody := assertCertificateResponseSafe(t, createdResponse, http.StatusCreated, string(certificate), string(key))
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(createdBody, &created); err != nil {
		t.Fatal(err)
	}
	response = f.rawCertificateRequest(http.MethodDelete, "/v1/certificate-bindings/"+created.ID, "certificate-body-delete", `{}`, "application/json")
	assertCertificateResponseSafe(t, response, http.StatusBadRequest)

	response = f.request(http.MethodPost, "/v1/projects/"+f.project.ID+"/service-accounts", "certificate-service-account", map[string]string{"name": "Certificate reader", "role": "project-admin"})
	account := decode[domain.ServiceAccount](t, response)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("service account status=%d", response.StatusCode)
	}
	response = f.request(http.MethodPost, "/v1/service-accounts/"+account.ID+"/tokens", "certificate-service-token", map[string]any{
		"name": "Certificate API", "scopes": []string{"app.read", "app.edit"}, "expiresAt": time.Now().UTC().Add(time.Hour),
	})
	issue := decode[struct {
		Token string `json:"token"`
	}](t, response)
	if response.StatusCode != http.StatusCreated || issue.Token == "" {
		t.Fatalf("token issue status=%d", response.StatusCode)
	}
	bearerRequest, _ := http.NewRequest(http.MethodGet, f.server.URL+"/v1/certificate-bindings/"+created.ID, nil)
	bearerRequest.Header.Set("Authorization", "Bearer "+issue.Token)
	response, err := (&http.Client{}).Do(bearerRequest)
	if err != nil {
		t.Fatal(err)
	}
	bearerBody := assertCertificateResponseSafe(t, response, http.StatusForbidden, issue.Token)
	if !bytes.Contains(bearerBody, []byte(`"code":"HumanSessionRequired"`)) {
		t.Fatalf("certificate metadata was not human-session-only: %s", bearerBody)
	}
}

func TestCertificateCapabilityAndRoutesStayClosedWithoutExactProbe(t *testing.T) {
	for _, test := range []struct {
		name               string
		backend, readiness bool
		readinessErr       error
	}{
		{name: "backend only", backend: true},
		{name: "failed readiness", backend: true, readiness: true, readinessErr: context.DeadlineExceeded},
		{name: "probe only", readiness: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newCertificateAPI(t, test.backend, test.readiness, test.readinessErr)
			response := f.request(http.MethodGet, "/v1/applications/"+f.application.ID+"/certificate-bindings", "", nil)
			body := assertCertificateResponseSafe(t, response, http.StatusServiceUnavailable)
			if !bytes.Contains(body, []byte(`"code":"CertificateBindingsUnavailable"`)) {
				t.Fatalf("unexpected unavailable response: %s", body)
			}
			response = f.request(http.MethodGet, "/v1/capabilities", "", nil)
			capability := decode[struct {
				Features map[string]bool `json:"features"`
			}](t, response)
			if capability.Features["customCertificates"] {
				t.Fatal("custom certificates were advertised without an exact healthy readiness probe")
			}
		})
	}
}

func (f *certificateAPIFixture) rawCertificateRequest(method, path, key, body, mediaType string) *http.Response {
	f.t.Helper()
	request, err := http.NewRequest(method, f.server.URL+path, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	if mediaType != "" {
		request.Header.Set("Content-Type", mediaType)
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	request.Header.Set("X-CSRF-Token", f.csrf)
	response, err := f.client.Do(request)
	if err != nil {
		f.t.Fatal(err)
	}
	return response
}

func mustJSON(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func slicesContain(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
