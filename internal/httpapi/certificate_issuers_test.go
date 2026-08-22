package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

type certificateIssuerCatalogStub struct {
	items    []httpapi.CertificateIssuerCatalogItem
	err      error
	hostname string
	now      time.Time
}

func (c *certificateIssuerCatalogStub) ApprovedCertificateIssuers(_ context.Context, hostname string, now time.Time) ([]httpapi.CertificateIssuerCatalogItem, error) {
	c.hostname, c.now = hostname, now
	return c.items, c.err
}

func newCertificateIssuerAPI(t *testing.T, catalog *certificateIssuerCatalogStub, readiness *edgeHTTPReadiness) (*apiFixture, domain.Application, domain.Environment) {
	t.Helper()
	central := memory.New()
	server := httptest.NewServer(httpapi.New(httpapi.Options{Store: central, BootstrapToken: "one-time-secret", Version: "test",
		CertificateIssuers: catalog, EdgeReadiness: readiness, EdgeFeatures: httpapi.EdgeRuntimeFeatures{CertManager: true},
		AppConfigRenderedPreviews: staticAppConfigRenderer{},
		HighRiskLimiter:           ratelimit.NewMemoryLimiter(10_000)}))
	jar, _ := cookiejar.New(nil)
	fixture := &apiFixture{t: t, server: server, client: &http.Client{Jar: jar}, store: central}
	t.Cleanup(server.Close)
	fixture.bootstrap()
	team := decode[domain.Team](t, fixture.request(http.MethodPost, "/v1/teams", "issuer-team", map[string]string{"name": "issuer", "slug": "issuer"}))
	project := decode[domain.Project](t, fixture.request(http.MethodPost, "/v1/projects", "issuer-project", map[string]string{"name": "issuer", "slug": "issuer", "teamId": team.ID}))
	environment := decode[domain.Environment](t, fixture.request(http.MethodPost, "/v1/environments", "issuer-environment", map[string]string{"projectId": project.ID, "name": "Production", "slug": "production"}))
	application := decode[domain.Application](t, fixture.request(http.MethodPost, "/v1/applications", "issuer-application", map[string]string{"projectId": project.ID, "name": "API", "slug": "api"}))
	return fixture, application, environment
}

func TestCertificateIssuerCatalogIsExactScopedAndFresh(t *testing.T) {
	catalog := &certificateIssuerCatalogStub{items: []httpapi.CertificateIssuerCatalogItem{
		{Name: "kuberploy-letsencrypt-production", Environment: "production", SolverTypes: []string{"dns01", "http01"}, Source: "bootstrap"},
		{Name: "team-production", Environment: "production", SolverTypes: []string{"dns01-cloudflare"}, Source: "managed", Revision: 2},
	}}
	readiness := &edgeHTTPReadiness{}
	fixture, application, environment := newCertificateIssuerAPI(t, catalog, readiness)
	path := "/v1/applications/" + application.ID + "/certificate-issuers?environmentId=" + environment.ID + "&hostname=api.example.com"
	response := fixture.request(http.MethodGet, path, "", nil)
	result := decode[struct {
		Items []httpapi.CertificateIssuerCatalogItem `json:"items"`
	}](t, response)
	if response.StatusCode != http.StatusOK || len(result.Items) != 2 || catalog.hostname != "api.example.com" || catalog.now.IsZero() || readiness.called != 1 {
		t.Fatalf("status=%d items=%#v hostname=%q now=%s readiness=%d", response.StatusCode, result.Items, catalog.hostname, catalog.now, readiness.called)
	}
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("unsafe cache policy %q", response.Header.Get("Cache-Control"))
	}

	for _, suffix := range []string{
		"?environmentId=" + environment.ID,
		"?environmentId=" + environment.ID + "&hostname=API.example.com",
		"?environmentId=" + environment.ID + "&hostname=api.example.com&extra=true",
		"?environmentId=" + environment.ID + "&hostname=*.example.com",
	} {
		response = fixture.request(http.MethodGet, "/v1/applications/"+application.ID+"/certificate-issuers"+suffix, "", nil)
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid query %q status=%d", suffix, response.StatusCode)
		}
		response.Body.Close()
	}
	readiness.err = errors.New("stale")
	response = fixture.request(http.MethodGet, path, "", nil)
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusServiceUnavailable || problem.Code != "CertificateIssuerCatalogUnavailable" {
		t.Fatalf("stale status=%d problem=%#v", response.StatusCode, problem)
	}
}

func TestCertificateIssuerCatalogRejectsUnsafeBackendProjection(t *testing.T) {
	catalog := &certificateIssuerCatalogStub{items: []httpapi.CertificateIssuerCatalogItem{{Name: "issuer", Environment: "production", SolverTypes: []string{"http01"}, Source: "managed"}}}
	fixture, application, environment := newCertificateIssuerAPI(t, catalog, &edgeHTTPReadiness{})
	response := fixture.request(http.MethodGet, "/v1/applications/"+application.ID+"/certificate-issuers?environmentId="+environment.ID+"&hostname=api.example.com", "", nil)
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusServiceUnavailable || problem.Code != "CertificateIssuerCatalogUnavailable" {
		t.Fatalf("unsafe backend status=%d problem=%#v", response.StatusCode, problem)
	}
}

func TestDeploymentConfigPreviewRequiresExactApprovedCertificateIssuer(t *testing.T) {
	catalog := &certificateIssuerCatalogStub{items: []httpapi.CertificateIssuerCatalogItem{{
		Name: "kuberploy-letsencrypt-production", Environment: "production", SolverTypes: []string{"http01"}, Source: "bootstrap",
	}}}
	readiness := &edgeHTTPReadiness{}
	fixture, application, environment := newCertificateIssuerAPI(t, catalog, readiness)
	response := fixture.request(http.MethodPost, "/v1/deployments", "issuer-deployment", map[string]any{
		"environmentId": environment.ID, "applicationId": application.ID,
		"image": "registry.example/api@sha256:" + strings.Repeat("a", 64), "runtime": domain.DefaultWorkloadRuntime(8080, nil),
	})
	operation := decode[domain.Operation](t, response)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("deployment status=%d", response.StatusCode)
	}
	path := "/v1/deployments/" + operation.TargetID + "/config"
	response = fixture.request(http.MethodGet, path, "", nil)
	bundle := decode[configBundleWire](t, response)
	route := func(issuer string) appconfig.Change {
		return appconfig.Change{Mode: "jsonPatch", Patch: []appconfig.PatchOperation{{Op: "add", Path: "/spec/routes", Value: []any{map[string]any{
			"host": "api.example.com", "path": "/", "port": "http", "dns": map[string]any{"mode": "manual"},
			"tls": map[string]any{"mode": "letsencrypt", "issuerRef": issuer, "redirectHttp": true},
		}}}}}
	}

	response = configRequest(t, fixture, http.MethodPost, path+"/validate", "", route("unapproved"), nil)
	invalid := decode[validationWire](t, response)
	if response.StatusCode != http.StatusOK || invalid.Valid || !hasConfigDiagnostic(invalid.Diagnostics, "CertificateIssuerNotApproved", "/spec/routes/0/tls/issuerRef") {
		t.Fatalf("unapproved issuer validated: status=%d %#v", response.StatusCode, invalid)
	}
	response = configRequest(t, fixture, http.MethodPost, path+"/preview", "", route("unapproved"), map[string]string{"If-Match": bundle.ETag})
	invalid = decode[validationWire](t, response)
	if response.StatusCode != http.StatusUnprocessableEntity || invalid.Valid || !hasConfigDiagnostic(invalid.Diagnostics, "CertificateIssuerNotApproved", "/spec/routes/0/tls/issuerRef") {
		t.Fatalf("unapproved issuer received preview authority: status=%d %#v", response.StatusCode, invalid)
	}

	response = configRequest(t, fixture, http.MethodPost, path+"/preview", "", route("kuberploy-letsencrypt-production"), map[string]string{"If-Match": bundle.ETag})
	preview := decode[previewWire](t, response)
	if response.StatusCode != http.StatusOK || preview.PreviewToken == "" {
		t.Fatalf("approved issuer rejected: status=%d %#v", response.StatusCode, preview)
	}

	readiness.err = errors.New("stale issuer observation")
	response = configRequest(t, fixture, http.MethodPut, path, "issuer-stale-save", route("kuberploy-letsencrypt-production"), map[string]string{
		"If-Match": bundle.ETag, "Preview-Token": preview.PreviewToken,
	})
	stale := decode[validationWire](t, response)
	if response.StatusCode != http.StatusUnprocessableEntity || stale.Valid || !hasConfigDiagnostic(stale.Diagnostics, "CertificateIssuerCatalogUnavailable", "/spec/routes/0/tls/issuerRef") {
		t.Fatalf("stale issuer catalog accepted save: status=%d %#v", response.StatusCode, stale)
	}
	response = configRequest(t, fixture, http.MethodPost, path+"/preview", "", route("kuberploy-letsencrypt-production"), map[string]string{"If-Match": bundle.ETag})
	stale = decode[validationWire](t, response)
	if response.StatusCode != http.StatusUnprocessableEntity || stale.Valid || !hasConfigDiagnostic(stale.Diagnostics, "CertificateIssuerCatalogUnavailable", "/spec/routes/0/tls/issuerRef") {
		t.Fatalf("stale issuer catalog received preview authority: status=%d %#v", response.StatusCode, stale)
	}
}
