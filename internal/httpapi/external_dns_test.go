package httpapi_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/deploymentrollback"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/externaldns"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

type externalDNSAPIFixture struct {
	*apiFixture
	project     domain.Project
	environment domain.Environment
	application domain.Application
	edgeProbe   *edgeHTTPReadiness
}

func newExternalDNSAPI(t *testing.T, observed ...bool) *externalDNSAPIFixture {
	t.Helper()
	central := memory.New()
	management := externaldns.NewManagement(central)
	var edgeProbe *edgeHTTPReadiness
	features := httpapi.EdgeRuntimeFeatures{}
	if len(observed) != 0 && observed[0] {
		edgeProbe = &edgeHTTPReadiness{}
		features.ExternalDNS = true
	}
	server := httptest.NewServer(httpapi.New(httpapi.Options{
		Store: central, BootstrapToken: "one-time-secret", Version: "test", ExternalDNS: management,
		DeploymentRollbacks: &deploymentrollback.Resolver{History: central, Artifacts: central, Publications: central},
		EdgeReadiness:       edgeProbe, EdgeFeatures: features, AppConfigRenderedPreviews: staticAppConfigRenderer{},
		HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000),
	}))
	jar, _ := cookiejar.New(nil)
	base := &apiFixture{t: t, server: server, client: &http.Client{Jar: jar}, store: central}
	t.Cleanup(server.Close)
	fixture := &externalDNSAPIFixture{apiFixture: base, edgeProbe: edgeProbe}
	fixture.bootstrap()
	response := fixture.request(http.MethodPost, "/v1/projects", "external-dns-project", map[string]string{"name": "DNS project", "slug": "dns-project"})
	fixture.project = decode[domain.Project](t, response)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("project status=%d", response.StatusCode)
	}
	response = fixture.request(http.MethodPost, "/v1/environments", "external-dns-environment", map[string]string{"projectId": fixture.project.ID, "name": "Production", "slug": "production"})
	fixture.environment = decode[domain.Environment](t, response)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("environment status=%d", response.StatusCode)
	}
	response = fixture.request(http.MethodPost, "/v1/applications", "external-dns-application", map[string]string{"projectId": fixture.project.ID, "name": "API", "slug": "api"})
	fixture.application = decode[domain.Application](t, response)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("application status=%d", response.StatusCode)
	}
	return fixture
}

func externalDNSPayload(environmentID string) map[string]any {
	return map[string]any{
		"slug": "public-dns", "name": "Public DNS", "mode": "managed", "providerKind": "cloudflare",
		"txtOwnerId": "kuberploy.production", "allowedDomainSuffixes": []string{"example.com"},
		"syncPolicy": "upsert-only", "credentialSecretRef": "external-dns-credentials",
		"providerConfigRef": "cloudflare-provider", "egressConfigRef": "internet-egress",
		"environmentIds": []string{environmentID},
	}
}

func readExternalDNSBody(t *testing.T, response *http.Response, status int, forbidden ...string) []byte {
	t.Helper()
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != status {
		t.Fatalf("status=%d want=%d body=%s", response.StatusCode, status, body)
	}
	if response.Header.Get("Cache-Control") != "private, no-store" || response.Header.Get("Pragma") != "no-cache" {
		t.Fatalf("external-dns response was cacheable: %#v", response.Header)
	}
	for _, value := range forbidden {
		if bytes.Contains(body, []byte(value)) {
			t.Fatalf("external-dns response disclosed forbidden value %q: %s", value, body)
		}
	}
	return body
}

func TestExternalDNSConfigurationCapabilityDefaultsOffWithoutOperationalManagement(t *testing.T) {
	fixture := newAPI(t)
	fixture.bootstrap()
	response := fixture.request(http.MethodGet, "/v1/capabilities", "", nil)
	capabilities := decode[struct {
		Features map[string]bool `json:"features"`
	}](t, response)
	if response.StatusCode != http.StatusOK || capabilities.Features["externalDNSConfiguration"] || capabilities.Features["externalDNS"] {
		t.Fatalf("disabled External DNS was advertised: status=%d %#v", response.StatusCode, capabilities.Features)
	}
	response = fixture.request(http.MethodGet, "/v1/external-dns/integrations", "", nil)
	readExternalDNSBody(t, response, http.StatusServiceUnavailable)
}

func TestExternalDNSManagementIsReplaySafeAndProjectsExactReadiness(t *testing.T) {
	fixture := newExternalDNSAPI(t, true)
	response := fixture.request(http.MethodGet, "/v1/capabilities", "", nil)
	capabilities := decode[struct {
		Features map[string]bool `json:"features"`
	}](t, response)
	if capabilities.Features["externalDNS"] || !capabilities.Features["externalDNSConfiguration"] {
		t.Fatalf("capability confused configured metadata with runtime readiness: %#v", capabilities.Features)
	}

	payload := externalDNSPayload(fixture.environment.ID)
	auditsBefore := fixture.store.AuditCount()
	response = fixture.request(http.MethodPost, "/v1/external-dns/integrations", "external-dns-create-0001", payload)
	body := readExternalDNSBody(t, response, http.StatusCreated, `"credential" :`, `"token"`, `"endpoint"`, `"providerJson"`)
	var created domain.ExternalDNSIntegration
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.SyncPolicy != "upsert-only" || created.DestructiveSyncConfirmed || fixture.store.AuditCount() != auditsBefore+1 {
		t.Fatalf("created=%#v audits=%d", created, fixture.store.AuditCount())
	}
	response = fixture.request(http.MethodPost, "/v1/external-dns/integrations", "external-dns-create-0001", payload)
	replayBody := readExternalDNSBody(t, response, http.StatusCreated)
	if response.Header.Get("Idempotent-Replay") != "true" || !bytes.Contains(replayBody, []byte(created.ID)) || fixture.store.AuditCount() != auditsBefore+1 {
		t.Fatalf("replay header=%q audits=%d", response.Header.Get("Idempotent-Replay"), fixture.store.AuditCount())
	}
	duplicateSlug := externalDNSPayload(fixture.environment.ID)
	duplicateSlug["txtOwnerId"] = "kuberploy.other"
	response = fixture.request(http.MethodPost, "/v1/external-dns/integrations", "external-dns-duplicate-slug-0001", duplicateSlug)
	readExternalDNSBody(t, response, http.StatusConflict, "external-dns-credentials")
	duplicateOwner := externalDNSPayload(fixture.environment.ID)
	duplicateOwner["slug"] = "other-dns"
	response = fixture.request(http.MethodPost, "/v1/external-dns/integrations", "external-dns-duplicate-owner-0001", duplicateOwner)
	readExternalDNSBody(t, response, http.StatusConflict, "external-dns-credentials")
	if fixture.store.AuditCount() != auditsBefore+1 {
		t.Fatalf("failed identity conflicts emitted audits: %d", fixture.store.AuditCount())
	}

	response = fixture.request(http.MethodGet, "/v1/external-dns/status", "", nil)
	statusBody := readExternalDNSBody(t, response, http.StatusOK)
	var status struct {
		ConfigurationState  string `json:"configurationState"`
		ControllerReadiness string `json:"controllerReadiness"`
		RuntimeAvailable    bool   `json:"runtimeAvailable"`
	}
	if err := json.Unmarshal(statusBody, &status); err != nil {
		t.Fatal(err)
	}
	if status.ConfigurationState != "configured" || status.ControllerReadiness != "ready" || !status.RuntimeAvailable || fixture.edgeProbe.called == 0 {
		t.Fatalf("status did not project fresh controller availability: %#v calls=%d", status, fixture.edgeProbe.called)
	}

	response = fixture.request(http.MethodGet, "/v1/applications/"+fixture.application.ID+"/external-dns-integrations?environmentId="+fixture.environment.ID, "", nil)
	catalogBody := readExternalDNSBody(t, response, http.StatusOK, "external-dns-credentials", "cloudflare-provider", "internet-egress", "kuberploy.production", `"createdBy"`)
	if !bytes.Contains(catalogBody, []byte(`"slug":"public-dns"`)) || !bytes.Contains(catalogBody, []byte(`"runtimeRevision":1,"runtimeAvailable":true`)) || !bytes.Contains(catalogBody, []byte(`"controllerReadiness":"ready"`)) || !bytes.Contains(catalogBody, []byte(`"runtimeAvailable":true`)) {
		t.Fatalf("catalog missing safe identity/readiness: %s", catalogBody)
	}
	fixture.edgeProbe.err = errors.New("stale external-dns observation")
	response = fixture.request(http.MethodGet, "/v1/external-dns/status", "", nil)
	statusBody = readExternalDNSBody(t, response, http.StatusOK)
	if err := json.Unmarshal(statusBody, &status); err != nil {
		t.Fatal(err)
	}
	if status.ControllerReadiness != externaldns.ReadinessUnobserved || status.RuntimeAvailable {
		t.Fatalf("stale controller observation did not fail closed: %#v", status)
	}
	response = fixture.request(http.MethodGet, "/v1/applications/"+fixture.application.ID+"/external-dns-integrations?environmentId="+fixture.environment.ID, "", nil)
	catalogBody = readExternalDNSBody(t, response, http.StatusOK)
	if !bytes.Contains(catalogBody, []byte(`"runtimeRevision":1,"runtimeAvailable":false`)) {
		t.Fatalf("stale per-integration catalog readiness did not fail closed: %s", catalogBody)
	}

	changed := externalDNSPayload(fixture.environment.ID)
	changed["slug"] = "renamed-dns"
	response = fixture.request(http.MethodPut, "/v1/external-dns/integrations/"+created.ID, "external-dns-update-0001", changed)
	problemBody := readExternalDNSBody(t, response, http.StatusConflict)
	if bytes.Contains(problemBody, []byte("external-dns-credentials")) {
		t.Fatalf("conflict response leaked configuration metadata: %s", problemBody)
	}

	unsafe := externalDNSPayload(fixture.environment.ID)
	unsafe["syncPolicy"] = "sync"
	response = fixture.request(http.MethodPost, "/v1/external-dns/integrations", "external-dns-unsafe-0001", unsafe)
	readExternalDNSBody(t, response, http.StatusUnprocessableEntity)
}

func TestExternalDNSRejectsRawProviderFieldsAndUnsafeIdempotencyKeys(t *testing.T) {
	fixture := newExternalDNSAPI(t)
	payload := externalDNSPayload(fixture.environment.ID)
	payload["providerEndpoint"] = "https://api.provider.invalid/raw-secret-value"
	response := fixture.request(http.MethodPost, "/v1/external-dns/integrations", "external-dns-invalid-0001", payload)
	readExternalDNSBody(t, response, http.StatusBadRequest, "raw-secret-value")
	response = fixture.request(http.MethodPost, "/v1/external-dns/integrations", "short", externalDNSPayload(fixture.environment.ID))
	readExternalDNSBody(t, response, http.StatusBadRequest, "external-dns-credentials")
}

func TestExternalDNSRejectsNonUUIDScopesBeforeStoreAccess(t *testing.T) {
	fixture := newExternalDNSAPI(t)
	response := fixture.request(http.MethodPut, "/v1/external-dns/integrations/not-a-uuid", "external-dns-invalid-id-0001", externalDNSPayload(fixture.environment.ID))
	readExternalDNSBody(t, response, http.StatusNotFound, "external-dns-credentials")

	response = fixture.request(http.MethodGet, "/v1/environments/not-a-uuid/external-dns-integrations", "", nil)
	readExternalDNSBody(t, response, http.StatusNotFound)
	response = fixture.request(http.MethodGet, "/v1/applications/not-a-uuid/external-dns-integrations?environmentId="+fixture.environment.ID, "", nil)
	readExternalDNSBody(t, response, http.StatusNotFound)
	response = fixture.request(http.MethodGet, "/v1/applications/"+fixture.application.ID+"/external-dns-integrations?environmentId=not-a-uuid", "", nil)
	readExternalDNSBody(t, response, http.StatusBadRequest)
}

func TestDeploymentConfigExternalDNSRequiresCatalogSlugAllowedSuffixAndRuntimeReadiness(t *testing.T) {
	fixture := newExternalDNSAPI(t, true)
	response := fixture.request(http.MethodPost, "/v1/external-dns/integrations", "external-dns-config-0001", externalDNSPayload(fixture.environment.ID))
	readExternalDNSBody(t, response, http.StatusCreated)
	image := "registry.example/api@sha256:" + strings.Repeat("a", 64)
	response = fixture.request(http.MethodPost, "/v1/deployments", "external-dns-deploy-0001", map[string]any{
		"environmentId": fixture.environment.ID, "applicationId": fixture.application.ID,
		"image": image, "runtime": domain.DefaultWorkloadRuntime(8080, nil),
	})
	operation := decode[domain.Operation](t, response)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("deployment status=%d", response.StatusCode)
	}
	path := "/v1/deployments/" + operation.TargetID + "/config"
	response = fixture.request(http.MethodGet, path, "", nil)
	bundle := decode[configBundleWire](t, response)

	route := func(host, integration string) appconfig.Change {
		return appconfig.Change{Mode: "jsonPatch", Patch: []appconfig.PatchOperation{{Op: "add", Path: "/spec/routes", Value: []any{map[string]any{
			"host": host, "path": "/", "port": "http", "dns": map[string]any{"mode": "externalDns", "integrationRef": integration},
			"tls": map[string]any{"mode": "httpOnly"},
		}}}}}
	}
	response = configRequest(t, fixture.apiFixture, http.MethodPost, path+"/validate", "", route("api.example.com", "public-dns"), nil)
	valid := decode[validationWire](t, response)
	if response.StatusCode != http.StatusOK || !valid.Valid {
		t.Fatalf("authorized route validation=%#v status=%d", valid, response.StatusCode)
	}
	response = configRequest(t, fixture.apiFixture, http.MethodPost, path+"/validate", "", route("api.example.net", "public-dns"), nil)
	wrongSuffix := decode[validationWire](t, response)
	if wrongSuffix.Valid || !hasConfigDiagnostic(wrongSuffix.Diagnostics, "ExternalDNSHostnameNotAllowed", "/spec/routes/0/host") {
		t.Fatalf("suffix escape accepted: %#v", wrongSuffix)
	}
	response = configRequest(t, fixture.apiFixture, http.MethodPost, path+"/validate", "", route("api.example.com", "other-dns"), nil)
	unknown := decode[validationWire](t, response)
	if unknown.Valid || !hasConfigDiagnostic(unknown.Diagnostics, "ExternalDNSIntegrationUnavailable", "/spec/routes/0/dns/integrationRef") {
		t.Fatalf("unknown integration accepted: %#v", unknown)
	}
	fixture.edgeProbe.err = errors.New("stale exact External DNS observation")
	response = configRequest(t, fixture.apiFixture, http.MethodPost, path+"/validate", "", route("api.example.com", "public-dns"), nil)
	stale := decode[validationWire](t, response)
	if stale.Valid || !hasConfigDiagnostic(stale.Diagnostics, "ExternalDNSRuntimeUnavailable", "/spec/routes/0/dns/integrationRef") {
		t.Fatalf("stale integration runtime accepted: %#v", stale)
	}
	response = configRequest(t, fixture.apiFixture, http.MethodPost, path+"/preview", "", route("api.example.com", "public-dns"), map[string]string{"If-Match": bundle.ETag})
	stalePreview := decode[validationWire](t, response)
	if response.StatusCode != http.StatusUnprocessableEntity || stalePreview.Valid || !hasConfigDiagnostic(stalePreview.Diagnostics, "ExternalDNSRuntimeUnavailable", "/spec/routes/0/dns/integrationRef") {
		t.Fatalf("stale integration preview accepted: status=%d %#v", response.StatusCode, stalePreview)
	}
	fixture.edgeProbe.err = nil
	response = configRequest(t, fixture.apiFixture, http.MethodPost, path+"/preview", "", route("api.example.com", "public-dns"), map[string]string{"If-Match": bundle.ETag})
	preview := decode[previewWire](t, response)
	if response.StatusCode != http.StatusOK || preview.PreviewToken == "" || len(preview.Warnings) == 0 || !strings.Contains(strings.Join(preview.Warnings, " "), "freshly observed ready") {
		t.Fatalf("ready preview omitted exact readiness evidence: status=%d %#v", response.StatusCode, preview)
	}
	fixture.edgeProbe.err = errors.New("runtime became stale after preview")
	response = configRequest(t, fixture.apiFixture, http.MethodPut, path, "external-dns-stale-save-0001", route("api.example.com", "public-dns"), map[string]string{"If-Match": bundle.ETag, "Preview-Token": preview.PreviewToken})
	staleSave := decode[validationWire](t, response)
	if response.StatusCode != http.StatusUnprocessableEntity || staleSave.Valid || !hasConfigDiagnostic(staleSave.Diagnostics, "ExternalDNSRuntimeUnavailable", "/spec/routes/0/dns/integrationRef") {
		t.Fatalf("stale integration save accepted: status=%d %#v", response.StatusCode, staleSave)
	}
}

func TestDeploymentRollbackCatalogOmitsSourceWithUnavailableExternalDNSDependency(t *testing.T) {
	fixture := newExternalDNSAPI(t, true)
	response := fixture.request(http.MethodPost, "/v1/external-dns/integrations", "rollback-external-dns-create", externalDNSPayload(fixture.environment.ID))
	readExternalDNSBody(t, response, http.StatusCreated)

	image := "registry.example/api@sha256:" + strings.Repeat("a", 64)
	response = fixture.request(http.MethodPost, "/v1/deployments", "rollback-external-dns-initial", map[string]any{
		"environmentId": fixture.environment.ID, "applicationId": fixture.application.ID,
		"image": image, "runtime": domain.DefaultWorkloadRuntime(8080, nil),
	})
	initial := decode[domain.Operation](t, response)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("initial deployment status=%d", response.StatusCode)
	}
	completeRollbackSource(t, fixture.apiFixture, initial)

	configPath := "/v1/deployments/" + initial.TargetID + "/config"
	response = fixture.request(http.MethodGet, configPath, "", nil)
	bundle := decode[configBundleWire](t, response)
	change := appconfig.Change{Mode: "jsonPatch", Patch: []appconfig.PatchOperation{{Op: "add", Path: "/spec/routes", Value: []any{map[string]any{
		"host": "api.example.com", "path": "/", "port": "http",
		"dns": map[string]any{"mode": "externalDns", "integrationRef": "public-dns"},
		"tls": map[string]any{"mode": "httpOnly"},
	}}}}}
	response = configRequest(t, fixture.apiFixture, http.MethodPost, configPath+"/preview", "", change, map[string]string{"If-Match": bundle.ETag})
	preview := decode[previewWire](t, response)
	if response.StatusCode != http.StatusOK || preview.PreviewToken == "" {
		t.Fatalf("preview status=%d body=%#v", response.StatusCode, preview)
	}
	response = configRequest(t, fixture.apiFixture, http.MethodPut, configPath, "rollback-external-dns-source", change,
		map[string]string{"If-Match": bundle.ETag, "Preview-Token": preview.PreviewToken})
	source := decode[domain.Operation](t, response)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("source status=%d operation=%#v", response.StatusCode, source)
	}
	completeRollbackSource(t, fixture.apiFixture, source)

	response = fixture.request(http.MethodPost, "/v1/deployments", "rollback-external-dns-current", map[string]any{
		"environmentId": fixture.environment.ID, "applicationId": fixture.application.ID,
		"image": image, "runtime": domain.DefaultWorkloadRuntime(8080, nil),
	})
	current := decode[domain.Operation](t, response)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("current status=%d operation=%#v", response.StatusCode, current)
	}
	completeRollbackSource(t, fixture.apiFixture, current)

	fixture.edgeProbe.err = errors.New("external DNS observation became stale")
	response = fixture.request(http.MethodGet, "/v1/deployments/"+initial.TargetID+"/rollback-sources?limit=10", "", nil)
	catalog := decode[struct {
		Items []deploymentrollback.Candidate `json:"items"`
	}](t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("catalog status=%d", response.StatusCode)
	}
	for _, candidate := range catalog.Items {
		if candidate.SourceOperationID == source.ID {
			t.Fatalf("catalog exposed source with unavailable dependency: %#v", candidate)
		}
	}
	foundInitial := false
	for _, candidate := range catalog.Items {
		foundInitial = foundInitial || candidate.SourceOperationID == initial.ID
	}
	if !foundInitial {
		t.Fatalf("catalog removed dependency-free source: %#v", catalog.Items)
	}
}

func TestExternalDNSDeactivateIsIdempotentAndRemovesTenantCatalog(t *testing.T) {
	fixture := newExternalDNSAPI(t, true)
	response := fixture.request(http.MethodPost, "/v1/external-dns/integrations", "external-dns-deactivate-create-0001", externalDNSPayload(fixture.environment.ID))
	created := decode[domain.ExternalDNSIntegration](t, response)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d", response.StatusCode)
	}
	response = fixture.request(http.MethodDelete, "/v1/external-dns/integrations/"+created.ID, "external-dns-deactivate-0001", nil)
	removed := decode[domain.ExternalDNSIntegration](t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("deactivate: %d", response.StatusCode)
	}
	if removed.Lifecycle != "deactivated" || removed.DeactivatedAt == nil {
		t.Fatalf("unexpected deactivation %#v", removed)
	}
	resurrection := externalDNSPayload(fixture.environment.ID)
	resurrection["name"] = "Resurrection attempt"
	response = fixture.request(http.MethodPut, "/v1/external-dns/integrations/"+created.ID, "external-dns-resurrection-0001", resurrection)
	readExternalDNSBody(t, response, http.StatusConflict)
	response = fixture.request(http.MethodDelete, "/v1/external-dns/integrations/"+created.ID, "external-dns-deactivate-0001", nil)
	if response.StatusCode != http.StatusOK || response.Header.Get("Idempotent-Replay") != "true" {
		t.Fatalf("replay: %d %#v", response.StatusCode, response.Header)
	}
	_ = decode[domain.ExternalDNSIntegration](t, response)
	response = fixture.request(http.MethodGet, "/v1/environments/"+fixture.environment.ID+"/external-dns-integrations?limit=50", "", nil)
	body := readExternalDNSBody(t, response, http.StatusOK)
	if strings.Contains(string(body), created.Slug) {
		t.Fatalf("deactivated integration leaked: %s", body)
	}
}
