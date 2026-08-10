package httpapi_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	"github.com/kuberploy/kuberploy/internal/secrets"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

type httpSecretKeys struct{}

type httpSecretReadiness struct{ err error }

func (p httpSecretReadiness) Probe(context.Context) error { return p.err }

func (httpSecretKeys) ActiveKey(context.Context) (secrets.FingerprintKey, error) {
	return secrets.FingerprintKey{ID: "http-secret-key-v1", Bytes: []byte("0123456789abcdef0123456789abcdef")}, nil
}

func httpRuntimeSecretConfig(t *testing.T) secrets.RuntimeConfig {
	t.Helper()
	config := secrets.DefaultRuntimeConfig()
	config.Enabled = true
	config.Namespaces = []string{"kp-secret-project-production"}
	config.FingerprintSecretRef = "kuberploy-runtime-secret-fingerprint"
	config.FingerprintKeyID = "http-secret-key-v1"
	config.SealingCertificateSecretRef = "sealed-secrets-key"
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	return config
}

type httpSecretProvider struct {
	mu         sync.Mutex
	stageCalls int
}

func (p *httpSecretProvider) stage(request secrets.StageRequest, material *secrets.Material) (secrets.Artifact, error) {
	entries := 0
	if err := material.WithEntries(func(_ string, value []byte) error {
		if len(value) == 0 {
			return secrets.ErrInvalid
		}
		entries++
		return nil
	}); err != nil || entries == 0 {
		return secrets.Artifact{}, secrets.ErrInvalid
	}
	p.mu.Lock()
	p.stageCalls++
	p.mu.Unlock()
	artifact := secrets.Artifact{Provider: request.Binding.Provider, Namespace: request.Binding.Scope.Namespace,
		ObjectName: request.TargetSecretName, TargetSecretName: request.TargetSecretName, TargetSecretType: request.Version.TargetSecretType,
		ProviderRevision: "provider-revision-1",
		ManifestDigest:   "sha256:" + strings.Repeat("a", 64)}
	if request.Binding.Provider == secrets.ProviderSealedSecrets {
		artifact.SealedKeyFingerprint = "sha256:" + strings.Repeat("b", 64)
		artifact.CiphertextDigest = "sha256:" + strings.Repeat("c", 64)
	}
	return artifact, nil
}

func (p *httpSecretProvider) observe(artifact secrets.Artifact) (secrets.ReadinessObservation, error) {
	return secrets.ReadinessObservation{Artifact: artifact, Status: secrets.ReadinessReady, ObservedAt: time.Now().UTC()}, nil
}

func (p *httpSecretProvider) remove(artifact secrets.Artifact) (secrets.DeleteObservation, error) {
	return secrets.DeleteObservation{Artifact: artifact, Absent: true, ObservedAt: time.Now().UTC()}, nil
}

func (p *httpSecretProvider) StageExternalSecret(_ context.Context, request secrets.StageRequest, material *secrets.Material) (secrets.Artifact, error) {
	return p.stage(request, material)
}
func (p *httpSecretProvider) ObserveExternalSecret(_ context.Context, artifact secrets.Artifact) (secrets.ReadinessObservation, error) {
	return p.observe(artifact)
}
func (p *httpSecretProvider) DeleteExternalSecret(_ context.Context, artifact secrets.Artifact) (secrets.DeleteObservation, error) {
	return p.remove(artifact)
}
func (p *httpSecretProvider) StageStrictSealedSecret(_ context.Context, request secrets.StageRequest, material *secrets.Material) (secrets.Artifact, error) {
	return p.stage(request, material)
}
func (p *httpSecretProvider) ObserveStrictSealedSecret(_ context.Context, artifact secrets.Artifact) (secrets.ReadinessObservation, error) {
	return p.observe(artifact)
}
func (p *httpSecretProvider) DeleteStrictSealedSecret(_ context.Context, artifact secrets.Artifact) (secrets.DeleteObservation, error) {
	return p.remove(artifact)
}

type secretAPIFixture struct {
	*apiFixture
	secretStore *secrets.MemoryStore
	service     secrets.Service
	provider    *httpSecretProvider
	admin       domain.User
	team        domain.Team
	project     domain.Project
	environment domain.Environment
	application domain.Application
}

func newSecretAPI(t *testing.T, configured bool) *secretAPIFixture {
	t.Helper()
	central := memory.New()
	secretStore := secrets.NewMemoryStore()
	provider := &httpSecretProvider{}
	service := secrets.Service{Store: secretStore, Keys: httpSecretKeys{}, SealedSecrets: provider}
	var backend httpapi.RuntimeSecretBackend
	if configured {
		var err error
		backend, err = httpapi.NewRuntimeSecretBackend(service, httpRuntimeSecretConfig(t))
		if err != nil {
			t.Fatal(err)
		}
	}
	var readiness httpapi.ReadinessProbe
	if configured {
		readiness = httpSecretReadiness{}
	}
	server := httptest.NewServer(httpapi.New(httpapi.Options{Store: central, BootstrapToken: "one-time-secret", Version: "test", RuntimeSecrets: backend, RuntimeSecretReadiness: readiness, HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000)}))
	jar, _ := cookiejar.New(nil)
	base := &apiFixture{t: t, server: server, client: &http.Client{Jar: jar}, store: central}
	t.Cleanup(server.Close)
	fixture := &secretAPIFixture{apiFixture: base, secretStore: secretStore, service: service, provider: provider}
	fixture.admin = fixture.bootstrap()
	if configured {
		fixture.provisionScope()
	}
	return fixture
}

func TestConfiguredRuntimeSecretsDoNotGatePlatformAppWithoutReferences(t *testing.T) {
	f := newSecretAPI(t, true)
	projectResponse := f.request(http.MethodPost, "/v1/projects", "platform-no-secret-project", map[string]string{
		"name": "Platform no secrets", "slug": "platform-no-secrets",
	})
	project := decode[domain.Project](t, projectResponse)
	if projectResponse.StatusCode != http.StatusCreated || project.ID == "" || project.TeamID != "" {
		t.Fatalf("project status=%d team=%q", projectResponse.StatusCode, project.TeamID)
	}
	environmentResponse := f.request(http.MethodPost, "/v1/environments", "platform-no-secret-environment", map[string]string{
		"projectId": project.ID, "name": "Production", "slug": "production",
	})
	environment := decode[domain.Environment](t, environmentResponse)
	if environmentResponse.StatusCode != http.StatusCreated {
		t.Fatalf("environment status=%d", environmentResponse.StatusCode)
	}
	applicationResponse := f.request(http.MethodPost, "/v1/applications", "platform-no-secret-application", map[string]string{
		"projectId": project.ID, "name": "API", "slug": "api",
	})
	application := decode[domain.Application](t, applicationResponse)
	if applicationResponse.StatusCode != http.StatusCreated {
		t.Fatalf("application status=%d", applicationResponse.StatusCode)
	}
	response := f.request(http.MethodPost, "/v1/deployments", "platform-no-secret-deployment", map[string]any{
		"environmentId": environment.ID,
		"applicationId": application.ID,
		"image":         "ghcr.io/kuberploy/example@sha256:" + strings.Repeat("a", 64),
		"runtime":       domain.DefaultWorkloadRuntime(8080, nil),
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("deployment status=%d body=%s", response.StatusCode, body)
	}
}

func (f *secretAPIFixture) provisionScope() {
	r := f.request(http.MethodPost, "/v1/teams", "secret-team", map[string]string{"name": "Runtime secrets", "slug": "runtime-secrets"})
	f.team = decode[domain.Team](f.t, r)
	if r.StatusCode != http.StatusCreated {
		f.t.Fatalf("team status=%d", r.StatusCode)
	}
	r = f.request(http.MethodPost, "/v1/projects", "secret-project", map[string]string{"name": "Secret project", "slug": "secret-project", "teamId": f.team.ID})
	f.project = decode[domain.Project](f.t, r)
	if r.StatusCode != http.StatusCreated {
		f.t.Fatalf("project status=%d", r.StatusCode)
	}
	r = f.request(http.MethodPost, "/v1/environments", "secret-environment", map[string]string{"projectId": f.project.ID, "name": "Production", "slug": "production"})
	f.environment = decode[domain.Environment](f.t, r)
	if r.StatusCode != http.StatusCreated {
		f.t.Fatalf("environment status=%d", r.StatusCode)
	}
	r = f.request(http.MethodPost, "/v1/applications", "secret-application", map[string]string{"projectId": f.project.ID, "name": "API", "slug": "api"})
	f.application = decode[domain.Application](f.t, r)
	if r.StatusCode != http.StatusCreated {
		f.t.Fatalf("application status=%d", r.StatusCode)
	}
}

func secretPayload(environmentID, password string) map[string]any {
	return map[string]any{
		"environmentId": environmentID,
		"name":          "database",
		"provider":      "sealed-secrets",
		"values":        map[string]string{"password": password, "token": "ghs_runtime_token"},
		"deliveries": []map[string]any{
			{"sourceKey": "password", "kind": "environment", "environmentName": "DATABASE_PASSWORD"},
			{"sourceKey": "token", "kind": "file", "filePath": "/var/run/secrets/kuberploy/github/token", "fileMode": 256},
		},
	}
}

func assertSecretResponseSafe(t *testing.T, response *http.Response, expectedStatus int, forbidden ...string) []byte {
	t.Helper()
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Pragma") != "no-cache" {
		t.Fatalf("secret response is cacheable: %#v", response.Header)
	}
	for _, value := range forbidden {
		if value != "" && bytes.Contains(body, []byte(value)) {
			t.Fatalf("secret response disclosed forbidden request material (body intentionally not logged)")
		}
	}
	for _, forbiddenField := range []string{`"values"`, `"artifact"`, `"contentFingerprint"`, `"requestFingerprint"`, `"fingerprintKeyId"`, `"providerRevision"`, `"manifestDigest"`, `"ciphertextDigest"`, `"sealedKeyFingerprint"`, `"organizationId"`, `"projectId"`, `"namespace"`} {
		if bytes.Contains(body, []byte(forbiddenField)) {
			t.Fatalf("secret response disclosed forbidden field %s", forbiddenField)
		}
	}
	if response.StatusCode != expectedStatus {
		t.Fatalf("status=%d, want %d (body intentionally not logged)", response.StatusCode, expectedStatus)
	}
	return body
}

func TestRuntimeSecretHTTPWriteOnlyLifecycleAndNoLeak(t *testing.T) {
	f := newSecretAPI(t, true)
	plaintext := "correct horse battery staple"
	encoded := base64.StdEncoding.EncodeToString([]byte(plaintext))
	path := "/v1/applications/" + f.application.ID + "/secret-bindings"
	r := f.request(http.MethodPost, path, "create-database-0001", secretPayload(f.environment.ID, plaintext))
	body := assertSecretResponseSafe(t, r, http.StatusCreated, plaintext, encoded, "ghs_runtime_token")
	var created struct {
		ID       string `json:"id"`
		State    string `json:"state"`
		Versions []struct {
			ID         string           `json:"id"`
			Number     int64            `json:"number"`
			State      string           `json:"state"`
			Deliveries []map[string]any `json:"deliveries"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.State != "provisioning" || len(created.Versions) != 1 || created.Versions[0].State != "awaiting-readiness" || len(created.Versions[0].Deliveries) != 2 {
		t.Fatalf("created=%#v", created)
	}
	if r := f.request(http.MethodPost, path, "create-database-0001", secretPayload(f.environment.ID, plaintext)); r.Header.Get("Idempotent-Replay") != "true" {
		assertSecretResponseSafe(t, r, http.StatusCreated, plaintext, encoded)
		t.Fatal("matching create did not identify its idempotency replay")
	} else {
		replayed := assertSecretResponseSafe(t, r, http.StatusCreated, plaintext, encoded)
		if !bytes.Contains(replayed, []byte(created.ID)) {
			t.Fatalf("replay returned another binding: %s", replayed)
		}
	}
	r = f.request(http.MethodPost, path, "create-database-0001", secretPayload(f.environment.ID, "different private value"))
	assertSecretResponseSafe(t, r, http.StatusConflict, "different private value", base64.StdEncoding.EncodeToString([]byte("different private value")))

	r = f.request(http.MethodGet, path+"?environmentId="+f.environment.ID, "", nil)
	list := assertSecretResponseSafe(t, r, http.StatusOK, plaintext, encoded)
	if !bytes.Contains(list, []byte(created.ID)) {
		t.Fatalf("list omitted binding: %s", list)
	}
	r = f.request(http.MethodGet, "/v1/secret-bindings/"+created.ID, "", nil)
	get := assertSecretResponseSafe(t, r, http.StatusOK, plaintext, encoded)
	if !bytes.Contains(get, []byte(`"versions"`)) || !bytes.Contains(get, []byte(`"deliveries"`)) {
		t.Fatalf("metadata read omitted versions/deliveries: %s", get)
	}

	active, err := f.service.ReconcileVersion(context.Background(), created.Versions[0].ID, "http-ready-version-1")
	if err != nil || active.Binding.ActiveVersion != 1 {
		t.Fatalf("activate=%#v err=%v", active, err)
	}
	rotation := map[string]any{
		"expectedActiveVersion": 2,
		"values":                map[string]string{"password": "rotated plaintext", "token": "new_runtime_token"},
		"deliveries": []map[string]any{
			{"sourceKey": "password", "kind": "environment", "environmentName": "DATABASE_PASSWORD"},
			{"sourceKey": "token", "kind": "file", "filePath": "/var/run/secrets/kuberploy/github/token", "fileMode": 256},
		},
	}
	r = f.request(http.MethodPost, "/v1/secret-bindings/"+created.ID+"/versions", "rotate-database-0001", rotation)
	assertSecretResponseSafe(t, r, http.StatusConflict, "rotated plaintext", "new_runtime_token")
	rotation["expectedActiveVersion"] = 1
	r = f.request(http.MethodPost, "/v1/secret-bindings/"+created.ID+"/versions", "rotate-database-0002", rotation)
	rotatedBody := assertSecretResponseSafe(t, r, http.StatusCreated, "rotated plaintext", "new_runtime_token")
	var rotated struct {
		Versions []struct {
			ID     string `json:"id"`
			Number int64  `json:"number"`
			State  string `json:"state"`
		} `json:"versions"`
	}
	if err = json.Unmarshal(rotatedBody, &rotated); err != nil || len(rotated.Versions) != 1 || rotated.Versions[0].Number != 2 {
		t.Fatalf("rotated=%#v err=%v", rotated, err)
	}
	r = f.request(http.MethodPost, "/v1/secret-bindings/"+created.ID+"/versions", "rotate-database-0002", rotation)
	if r.Header.Get("Idempotent-Replay") != "true" {
		t.Fatal("matching rotation did not identify replay")
	}
	assertSecretResponseSafe(t, r, http.StatusCreated, "rotated plaintext", "new_runtime_token")

	r = f.request(http.MethodDelete, "/v1/secret-bindings/"+created.ID, "delete-database-0001", nil)
	assertSecretResponseSafe(t, r, http.StatusConflict)
	active, err = f.service.ReconcileVersion(context.Background(), rotated.Versions[0].ID, "http-ready-version-2")
	if err != nil || active.Binding.ActiveVersion != 2 {
		t.Fatalf("activate rotation=%#v err=%v", active, err)
	}
	if err = f.service.AddReference(context.Background(), f.admin.ID, "http-add-git-reference", secrets.Reference{BindingID: created.ID,
		VersionID: active.Version.ID, Kind: secrets.ReferenceGitCurrent, Reference: "tenants/runtime/app.yaml", Revision: strings.Repeat("d", 40)}); err != nil {
		t.Fatal(err)
	}
	r = f.request(http.MethodDelete, "/v1/secret-bindings/"+created.ID, "delete-database-0002", nil)
	assertSecretResponseSafe(t, r, http.StatusConflict)
	if err = f.service.RemoveReference(context.Background(), f.admin.ID, created.ID, secrets.ReferenceGitCurrent, "tenants/runtime/app.yaml", "http-remove-git-reference"); err != nil {
		t.Fatal(err)
	}
	r = f.request(http.MethodDelete, "/v1/secret-bindings/"+created.ID, "delete-database-0003", nil)
	assertSecretResponseSafe(t, r, http.StatusNoContent)
	r = f.request(http.MethodDelete, "/v1/secret-bindings/"+created.ID, "delete-database-0003", nil)
	if r.Header.Get("Idempotent-Replay") != "true" {
		t.Fatal("repeated delete did not identify replay")
	}
	assertSecretResponseSafe(t, r, http.StatusNoContent)
}

func TestRuntimeSecretHTTPRejectsUnsafeCredentialsScopeAndBodies(t *testing.T) {
	f := newSecretAPI(t, true)
	path := "/v1/applications/" + f.application.ID + "/secret-bindings"
	payload := secretPayload(f.environment.ID, "never echo this value")

	encoded, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, f.server.URL+path, bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "csrf-required-0001")
	r, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	assertSecretResponseSafe(t, r, http.StatusForbidden, "never echo this value")

	r = f.request(http.MethodPost, path, "", payload)
	assertSecretResponseSafe(t, r, http.StatusBadRequest, "never echo this value")

	tampered := secretPayload(f.environment.ID, "caller scope secret")
	tampered["organizationId"] = f.team.ID
	r = f.request(http.MethodPost, path, "unknown-scope-0001", tampered)
	assertSecretResponseSafe(t, r, http.StatusBadRequest, "caller scope secret")

	r = f.request(http.MethodPost, path, "provider-unknown-0001", map[string]any{
		"environmentId": f.environment.ID, "name": "unknown", "provider": "caller-provider",
		"values": map[string]string{"key": "private"}, "deliveries": []map[string]any{{"sourceKey": "key", "kind": "environment", "environmentName": "KEY"}},
	})
	assertSecretResponseSafe(t, r, http.StatusUnprocessableEntity, "private")

	oversized := strings.Repeat("x", secrets.MaxValueBytes+1)
	r = f.request(http.MethodPost, path, "oversized-value-0001", map[string]any{
		"environmentId": f.environment.ID, "name": "oversized", "provider": "sealed-secrets",
		"values": map[string]string{"key": oversized}, "deliveries": []map[string]any{{"sourceKey": "key", "kind": "environment", "environmentName": "KEY"}},
	})
	assertSecretResponseSafe(t, r, http.StatusUnprocessableEntity, oversized[:64])

	totalValues, totalDeliveries := map[string]string{}, make([]map[string]any, 0, 5)
	for index, key := range []string{"one", "two", "three", "four", "five"} {
		totalValues[key] = strings.Repeat(string(rune('a'+index)), 60<<10)
		totalDeliveries = append(totalDeliveries, map[string]any{"sourceKey": key, "kind": "environment", "environmentName": strings.ToUpper(key)})
	}
	r = f.request(http.MethodPost, path, "oversized-total-0001", map[string]any{"environmentId": f.environment.ID, "name": "oversized-total", "provider": "sealed-secrets", "values": totalValues, "deliveries": totalDeliveries})
	assertSecretResponseSafe(t, r, http.StatusUnprocessableEntity)

	other := f.request(http.MethodPost, "/v1/projects", "other-project", map[string]string{"name": "Other", "slug": "other", "teamId": f.team.ID})
	otherProject := decode[domain.Project](t, other)
	other = f.request(http.MethodPost, "/v1/environments", "other-environment", map[string]string{"projectId": otherProject.ID, "name": "Other", "slug": "other"})
	otherEnvironment := decode[domain.Environment](t, other)
	r = f.request(http.MethodPost, path, "cross-project-0001", secretPayload(otherEnvironment.ID, "cross project private"))
	assertSecretResponseSafe(t, r, http.StatusNotFound, "cross project private")

	raw := []byte(`{"environmentId":"` + f.environment.ID + `","name":"bad","provider":"sealed-secrets","deliveries":[{"sourceKey":"key","kind":"environment","environmentName":"KEY"}],"values":{"key":"`)
	raw = append(raw, 0xff)
	raw = append(raw, []byte(`"}}`)...)
	req, _ = http.NewRequest(http.MethodPost, f.server.URL+path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "invalid-utf8-0001")
	req.Header.Set("X-CSRF-Token", f.csrf)
	r, err = f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	assertSecretResponseSafe(t, r, http.StatusBadRequest)

	duplicateValue := `{"environmentId":"` + f.environment.ID + `","name":"duplicate","provider":"sealed-secrets","deliveries":[{"sourceKey":"key","kind":"environment","environmentName":"KEY"}],"values":{"key":"first private","key":"second private"}}`
	req, _ = http.NewRequest(http.MethodPost, f.server.URL+path, strings.NewReader(duplicateValue))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "duplicate-value-0001")
	req.Header.Set("X-CSRF-Token", f.csrf)
	r, err = f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	assertSecretResponseSafe(t, r, http.StatusUnprocessableEntity, "first private", "second private")

	duplicateTopLevel := `{"environmentId":"` + f.environment.ID + `","name":"first-name","name":"second-name","provider":"sealed-secrets","deliveries":[{"sourceKey":"key","kind":"environment","environmentName":"KEY"}],"values":{"key":"top-level private"}}`
	req, _ = http.NewRequest(http.MethodPost, f.server.URL+path, strings.NewReader(duplicateTopLevel))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "duplicate-field-0001")
	req.Header.Set("X-CSRF-Token", f.csrf)
	r, err = f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	assertSecretResponseSafe(t, r, http.StatusBadRequest, "top-level private")

	req, _ = http.NewRequest(http.MethodPost, f.server.URL+path, strings.NewReader("not-json"))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Idempotency-Key", "wrong-media-type-01")
	req.Header.Set("X-CSRF-Token", f.csrf)
	r, err = f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	assertSecretResponseSafe(t, r, http.StatusUnsupportedMediaType)
}

func TestRuntimeSecretHTTPMetadataAllowsScopedBearerButMutationsRequireCookie(t *testing.T) {
	f := newSecretAPI(t, true)
	r := f.request(http.MethodPost, "/v1/applications/"+f.application.ID+"/secret-bindings", "bearer-fixture-0001", secretPayload(f.environment.ID, "bearer fixture private"))
	body := assertSecretResponseSafe(t, r, http.StatusCreated, "bearer fixture private")
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	r = f.request(http.MethodPost, "/v1/projects/"+f.project.ID+"/service-accounts", "secret-service-account", map[string]string{"name": "Secret metadata reader", "role": "project-admin"})
	account := decode[domain.ServiceAccount](t, r)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("service account status=%d", r.StatusCode)
	}
	r = f.request(http.MethodPost, "/v1/service-accounts/"+account.ID+"/tokens", "secret-service-token", map[string]any{
		"name": "Runtime secret API", "scopes": []string{"app.read", "app.edit"}, "expiresAt": time.Now().UTC().Add(time.Hour),
	})
	issue := decode[struct {
		Token string `json:"token"`
	}](t, r)
	if r.StatusCode != http.StatusCreated || issue.Token == "" {
		t.Fatalf("token issue status=%d body=%#v", r.StatusCode, issue)
	}
	bearerClient := &http.Client{}
	req, _ := http.NewRequest(http.MethodGet, f.server.URL+"/v1/secret-bindings/"+created.ID, nil)
	req.Header.Set("Authorization", "Bearer "+issue.Token)
	r, err := bearerClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	assertSecretResponseSafe(t, r, http.StatusOK, "bearer fixture private", issue.Token)

	rotation, _ := json.Marshal(map[string]any{"expectedActiveVersion": 1, "values": map[string]string{"key": "bearer must not ingest"},
		"deliveries": []map[string]any{{"sourceKey": "key", "kind": "environment", "environmentName": "KEY"}}})
	req, _ = http.NewRequest(http.MethodPost, f.server.URL+"/v1/secret-bindings/"+created.ID+"/versions", bytes.NewReader(rotation))
	req.Header.Set("Authorization", "Bearer "+issue.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "bearer-rotate-0001")
	r, err = bearerClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	problemBody := assertSecretResponseSafe(t, r, http.StatusForbidden, "bearer must not ingest", issue.Token)
	if !bytes.Contains(problemBody, []byte(`"code":"HumanSessionRequired"`)) {
		t.Fatalf("bearer write was not rejected by the cookie-only boundary: %s", problemBody)
	}
}

func TestGenericRuntimeSecretAPIDoesNotExposeOrMutateTLSCertificateBindings(t *testing.T) {
	f := newSecretAPI(t, true)
	material, err := secrets.NewMaterial(map[string][]byte{"tls.crt": []byte("certificate"), "tls.key": []byte("private-key")})
	if err != nil {
		t.Fatal(err)
	}
	created, err := f.service.Create(context.Background(), secrets.CreateRequest{
		ActorID: f.admin.ID,
		Scope: secrets.Scope{
			OrganizationID: f.team.ID, ProjectID: f.project.ID, EnvironmentID: f.environment.ID,
			ApplicationID: f.application.ID, Namespace: f.environment.Namespace,
		},
		Name: "edge-certificate", Provider: secrets.ProviderSealedSecrets, Purpose: secrets.PurposeTLSCertificate,
		TargetSecretType: secrets.TargetSecretTLS,
		Deliveries: []secrets.Delivery{
			{SourceKey: "tls.crt", Kind: secrets.DeliveryFile, FilePath: "/var/run/secrets/kuberploy/certificates/tls.crt", FileMode: 0o400},
			{SourceKey: "tls.key", Kind: secrets.DeliveryFile, FilePath: "/var/run/secrets/kuberploy/certificates/tls.key", FileMode: 0o400},
		},
		IdempotencyKey: "generic-api-certificate-1", RequestID: "generic-api-certificate", Material: material,
	})
	if err != nil {
		t.Fatal(err)
	}
	response := f.request(http.MethodGet,
		"/v1/applications/"+f.application.ID+"/secret-bindings?environmentId="+f.environment.ID, "", nil)
	body := assertSecretResponseSafe(t, response, http.StatusOK, created.Binding.ID, "tls.crt", "tls.key")
	if bytes.Contains(body, []byte(created.Binding.ID)) {
		t.Fatal("generic runtime-secret collection exposed a TLS certificate binding")
	}
	response = f.request(http.MethodGet, "/v1/secret-bindings/"+created.Binding.ID, "", nil)
	assertSecretResponseSafe(t, response, http.StatusNotFound, "tls.crt", "tls.key")
	response = f.request(http.MethodDelete, "/v1/secret-bindings/"+created.Binding.ID, "delete-certificate-generic", nil)
	assertSecretResponseSafe(t, response, http.StatusNotFound, "tls.crt", "tls.key")
}

func TestRuntimeSecretHTTPStaysUnavailableWithoutExplicitBackend(t *testing.T) {
	f := newSecretAPI(t, false)
	r := f.request(http.MethodPost, "/v1/applications/00000000-0000-4000-8000-000000000001/secret-bindings", "backend-disabled-01", secretPayload("00000000-0000-4000-8000-000000000002", "discarded"))
	body := assertSecretResponseSafe(t, r, http.StatusServiceUnavailable, "discarded")
	if !bytes.Contains(body, []byte(`"code":"SecretBindingsUnavailable"`)) {
		t.Fatalf("unexpected unavailable problem: %s", body)
	}
	r = f.request(http.MethodGet, "/v1/secret-bindings/00000000-0000-4000-8000-000000000003", "", nil)
	assertSecretResponseSafe(t, r, http.StatusServiceUnavailable)
	r = f.request(http.MethodGet, "/v1/capabilities", "", nil)
	capabilities := decode[struct {
		Features map[string]bool `json:"features"`
	}](t, r)
	if capabilities.Features["secretBindings"] {
		t.Fatal("backend-only seam falsely advertised controller-complete secret bindings")
	}
}

func TestRuntimeSecretBackendRejectsIncompleteConfiguration(t *testing.T) {
	config := httpRuntimeSecretConfig(t)
	if _, err := httpapi.NewRuntimeSecretBackend(secrets.Service{}, config); err == nil {
		t.Fatal("empty runtime secret service was accepted")
	}
	if _, err := httpapi.NewRuntimeSecretBackend(secrets.Service{Store: secrets.NewMemoryStore(), Keys: httpSecretKeys{}}, config); err == nil {
		t.Fatal("provider-free runtime secret service was accepted")
	}
	provider := &httpSecretProvider{}
	if _, err := httpapi.NewRuntimeSecretBackend(secrets.Service{Store: secrets.NewMemoryStore(), Keys: httpSecretKeys{}, ExternalSecrets: provider, SealedSecrets: provider}, config); err == nil {
		t.Fatal("External Secrets production writer was accepted")
	}
	backend, err := httpapi.NewRuntimeSecretBackend(secrets.Service{Store: secrets.NewMemoryStore(), Keys: httpSecretKeys{}, SealedSecrets: provider}, config)
	if err != nil || backend.ProviderAvailable(secrets.ProviderExternalSecrets) || !backend.ProviderAvailable(secrets.ProviderSealedSecrets) {
		t.Fatalf("strict provider boundary backend=%#v err=%v", backend, err)
	}
}
