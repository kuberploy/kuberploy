package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kuberploy/kuberploy/internal/certissuers"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

type certificateIssuerAdminReadiness struct{ err error }

func (p *certificateIssuerAdminReadiness) Probe(context.Context) error { return p.err }

func newCertificateIssuerAdminAPI(t *testing.T, readiness *certificateIssuerAdminReadiness) (*apiFixture, *certissuers.MemoryStore) {
	t.Helper()
	central := memory.New()
	issuers := certissuers.NewMemoryStore()
	server := httptest.NewServer(httpapi.New(httpapi.Options{
		Store: central, BootstrapToken: "one-time-secret", Version: "test",
		CertificateIssuerAdmin: issuers, CertificateIssuerRuntimeReadiness: readiness,
		HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000),
	}))
	jar, _ := cookiejar.New(nil)
	fixture := &apiFixture{t: t, server: server, client: &http.Client{Jar: jar}, store: central}
	t.Cleanup(server.Close)
	return fixture, issuers
}

func TestCertificateIssuerAdminLifecycleIsClosedAndServerDerivesACMEServer(t *testing.T) {
	readiness := &certificateIssuerAdminReadiness{}
	fixture, issuers := newCertificateIssuerAdminAPI(t, readiness)
	fixture.bootstrap()

	create := map[string]any{
		"name":                        "tenant-production",
		"environment":                 "production",
		"email":                       "ACME-Admin@Example.COM",
		"accountPrivateKeySecretName": "tenant-production-acme-account",
		"solver":                      map[string]any{"type": "http01"},
	}
	response := fixture.request(http.MethodPost, "/v1/platform/certificate-issuers", "create-tenant-production", create)
	created := decode[struct {
		ID              string `json:"id"`
		Lifecycle       string `json:"lifecycle"`
		CurrentRevision int64  `json:"currentRevision"`
		Revision        struct {
			Environment string `json:"environment"`
			Email       string `json:"email"`
			Solver      string `json:"solver"`
		} `json:"revision"`
		Observation struct {
			State string `json:"state"`
		} `json:"observation"`
	}](t, response)
	if response.StatusCode != http.StatusCreated || created.ID == "" || created.Lifecycle != "active" || created.CurrentRevision != 1 ||
		created.Revision.Environment != "production" || created.Revision.Email != "acme-admin@example.com" || created.Revision.Solver != "http01" || created.Observation.State != "pending" {
		t.Fatalf("unexpected create response status=%d body=%#v", response.StatusCode, created)
	}
	stored, err := issuers.Current(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Revision.Spec.ACME.Server != certissuers.LetsEncryptProduction || stored.Revision.Spec.HTTP01 == nil || stored.Revision.Spec.Cloudflare != nil {
		t.Fatalf("server did not derive the closed production HTTP-01 spec: %#v", stored.Revision.Spec)
	}

	replay := fixture.request(http.MethodPost, "/v1/platform/certificate-issuers", "create-tenant-production", create)
	if replay.StatusCode != http.StatusCreated || replay.Header.Get("Idempotent-Replay") != "true" {
		t.Fatalf("replay status=%d replay=%q", replay.StatusCode, replay.Header.Get("Idempotent-Replay"))
	}
	replay.Body.Close()
	readiness.err = errors.New("stale protected publisher")
	replay = fixture.request(http.MethodPost, "/v1/platform/certificate-issuers", "create-tenant-production", create)
	if replay.StatusCode != http.StatusCreated || replay.Header.Get("Idempotent-Replay") != "true" {
		t.Fatalf("stale-runtime create replay status=%d replay=%q", replay.StatusCode, replay.Header.Get("Idempotent-Replay"))
	}
	replay.Body.Close()
	blocked := fixture.request(http.MethodPost, "/v1/platform/certificate-issuers", "blocked-create", create)
	blockedProblem := decode[httpapi.Problem](t, blocked)
	if blocked.StatusCode != http.StatusServiceUnavailable || blockedProblem.Code != "CertificateIssuerManagementUnavailable" {
		t.Fatalf("stale-runtime new create status=%d problem=%#v", blocked.StatusCode, blockedProblem)
	}
	readiness.err = nil

	revise := map[string]any{
		"baseRevision":                1,
		"environment":                 "staging",
		"email":                       "acme-admin@example.com",
		"accountPrivateKeySecretName": "tenant-staging-acme-account",
		"solver": map[string]any{
			"type": "dns01-cloudflare", "dnsZones": []string{"dev.example.com", "example.com"},
			"apiTokenSecretName": "cloudflare-dns-token", "apiTokenSecretKey": "api-token",
		},
	}
	response = fixture.request(http.MethodPut, "/v1/platform/certificate-issuers/"+created.ID, "revise-tenant-production", revise)
	revised := decode[struct {
		CurrentRevision int64 `json:"currentRevision"`
		Revision        struct {
			Environment string   `json:"environment"`
			Solver      string   `json:"solver"`
			DNSZones    []string `json:"dnsZones"`
		} `json:"revision"`
	}](t, response)
	if response.StatusCode != http.StatusOK || revised.CurrentRevision != 2 || revised.Revision.Environment != "staging" || revised.Revision.Solver != "dns01-cloudflare" || strings.Join(revised.Revision.DNSZones, ",") != "dev.example.com,example.com" {
		t.Fatalf("unexpected revise status=%d body=%#v", response.StatusCode, revised)
	}
	stored, err = issuers.Current(context.Background(), created.ID)
	if err != nil || stored.Revision.Spec.ACME.Server != certissuers.LetsEncryptStaging || stored.Revision.Spec.Cloudflare == nil {
		t.Fatalf("server did not derive the closed staging DNS-01 spec: entry=%#v err=%v", stored, err)
	}
	readiness.err = errors.New("stale protected publisher")
	replay = fixture.request(http.MethodPut, "/v1/platform/certificate-issuers/"+created.ID, "revise-tenant-production", revise)
	if replay.StatusCode != http.StatusOK || replay.Header.Get("Idempotent-Replay") != "true" {
		t.Fatalf("stale-runtime revise replay status=%d replay=%q", replay.StatusCode, replay.Header.Get("Idempotent-Replay"))
	}
	replay.Body.Close()
	blocked = fixture.request(http.MethodPut, "/v1/platform/certificate-issuers/"+created.ID, "blocked-revise", revise)
	blockedProblem = decode[httpapi.Problem](t, blocked)
	if blocked.StatusCode != http.StatusServiceUnavailable || blockedProblem.Code != "CertificateIssuerManagementUnavailable" {
		t.Fatalf("stale-runtime new revise status=%d problem=%#v", blocked.StatusCode, blockedProblem)
	}
	readiness.err = errors.New("issuer made the protected runtime unhealthy")
	response = fixture.request(http.MethodPost, "/v1/platform/certificate-issuers/"+created.ID+"/deactivate", "deactivate-tenant-production", map[string]any{"revision": 2})
	deactivated := decode[struct {
		Lifecycle string `json:"lifecycle"`
	}](t, response)
	if response.StatusCode != http.StatusOK || deactivated.Lifecycle != "deactivated" {
		t.Fatalf("stale-runtime recovery deactivate status=%d body=%#v", response.StatusCode, deactivated)
	}
	replay = fixture.request(http.MethodPost, "/v1/platform/certificate-issuers/"+created.ID+"/deactivate", "deactivate-tenant-production", map[string]any{"revision": 2})
	if replay.StatusCode != http.StatusOK || replay.Header.Get("Idempotent-Replay") != "true" {
		t.Fatalf("stale-runtime deactivate replay status=%d replay=%q", replay.StatusCode, replay.Header.Get("Idempotent-Replay"))
	}
	replay.Body.Close()
	blocked = fixture.request(http.MethodPost, "/v1/platform/certificate-issuers/"+created.ID+"/deactivate", "blocked-deactivate", map[string]any{"revision": 2})
	blockedProblem = decode[httpapi.Problem](t, blocked)
	if blocked.StatusCode != http.StatusConflict || blockedProblem.Code != "CertificateIssuerConflict" {
		t.Fatalf("inactive issuer second deactivate status=%d problem=%#v", blocked.StatusCode, blockedProblem)
	}
}

func TestCertificateIssuerAdminFailsClosedWithoutRuntimeAndRejectsAuthorityInjection(t *testing.T) {
	readiness := &certificateIssuerAdminReadiness{err: errors.New("stale protected publisher")}
	fixture, _ := newCertificateIssuerAdminAPI(t, readiness)
	fixture.bootstrap()
	capabilitiesResponse := fixture.request(http.MethodGet, "/v1/capabilities", "", nil)
	capabilities := decode[struct {
		Features map[string]bool `json:"features"`
	}](t, capabilitiesResponse)
	if capabilities.Features["certificateIssuerManagement"] {
		t.Fatal("stale protected issuer runtime was advertised")
	}
	staleInput := map[string]any{
		"name": "unsafe", "environment": "production", "email": "admin@example.com",
		"accountPrivateKeySecretName": "acme-account",
		"solver":                      map[string]any{"type": "http01"},
	}
	response := fixture.request(http.MethodPost, "/v1/platform/certificate-issuers", "stale-runtime", staleInput)
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusServiceUnavailable || problem.Code != "CertificateIssuerManagementUnavailable" {
		t.Fatalf("stale runtime status=%d problem=%#v", response.StatusCode, problem)
	}

	input := map[string]any{
		"name": "unsafe", "environment": "production", "email": "admin@example.com",
		"accountPrivateKeySecretName": "acme-account", "server": certissuers.LetsEncryptStaging,
		"solver": map[string]any{"type": "http01"},
	}

	readiness.err = nil
	capabilitiesResponse = fixture.request(http.MethodGet, "/v1/capabilities", "", nil)
	capabilities = decode[struct {
		Features map[string]bool `json:"features"`
	}](t, capabilitiesResponse)
	if !capabilities.Features["certificateIssuerManagement"] {
		t.Fatal("fresh protected issuer runtime was not advertised")
	}
	response = fixture.request(http.MethodPost, "/v1/platform/certificate-issuers", "authority-injection", input)
	problem = decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusBadRequest || problem.Code != "InvalidJSON" {
		t.Fatalf("caller ACME-server injection status=%d problem=%#v", response.StatusCode, problem)
	}

	input = map[string]any{
		"name": "unsafe", "environment": "production", "email": "admin@example.com",
		"accountPrivateKeySecretName": "acme-account",
		"solver":                      map[string]any{"type": "http01", "apiToken": "plaintext-secret"},
	}
	response = fixture.request(http.MethodPost, "/v1/platform/certificate-issuers", "credential-injection", input)
	problem = decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusBadRequest || problem.Code != "InvalidJSON" {
		t.Fatalf("caller credential injection status=%d problem=%#v", response.StatusCode, problem)
	}
}
