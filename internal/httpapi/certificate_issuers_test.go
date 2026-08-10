package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
	"time"

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
		HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000)}))
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
