package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"

	"github.com/kuberploy/kuberploy/internal/externaldns"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

type edgeHTTPReadiness struct {
	err    error
	called int
}

func (p *edgeHTTPReadiness) Probe(context.Context) error {
	p.called++
	return p.err
}

func newEdgeHTTP(t *testing.T, edgeProbe *edgeHTTPReadiness, gitProbe *projectionHTTPReadiness, features httpapi.EdgeRuntimeFeatures, withExternalDNS bool) *apiFixture {
	t.Helper()
	st := memory.New()
	options := httpapi.Options{Store: st, BootstrapToken: "one-time-secret", Version: "test",
		EdgeReadiness: edgeProbe, EdgeFeatures: features, HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000)}
	if gitProbe != nil {
		options.GitProjection = &projectionHTTPBackend{}
		options.GitProjectionReadiness = gitProbe
	}
	if withExternalDNS {
		options.ExternalDNS = externaldns.NewManagement(st)
	}
	server := httptest.NewServer(httpapi.New(options))
	jar, _ := cookiejar.New(nil)
	fixture := &apiFixture{t: t, server: server, client: &http.Client{Jar: jar}, store: st}
	t.Cleanup(server.Close)
	fixture.bootstrap()
	return fixture
}

func edgeFeatures(t *testing.T, response *http.Response) map[string]bool {
	t.Helper()
	return decode[struct {
		Features map[string]bool `json:"features"`
	}](t, response).Features
}

func TestEdgeCapabilitiesRequireFreshExactProfilesWithoutClaimingArgoServingPaths(t *testing.T) {
	edgeProbe := &edgeHTTPReadiness{}
	gitProbe := &projectionHTTPReadiness{}
	fixture := newEdgeHTTP(t, edgeProbe, gitProbe, httpapi.EdgeRuntimeFeatures{
		Traefik: true, CertManager: true, ExternalDNS: true,
	}, true)
	features := edgeFeatures(t, fixture.request(http.MethodGet, "/v1/capabilities", "", nil))
	for _, name := range []string{"edge", "traefik", "certManager"} {
		if !features[name] {
			t.Fatalf("fresh exact edge infrastructure %q was not advertised: %#v", name, features)
		}
	}
	for _, name := range []string{"customCertificates", "externalDNS", "traefikMiddlewares"} {
		if features[name] {
			t.Fatalf("unimplemented Argo-dependent serving path %q was advertised: %#v", name, features)
		}
	}
	response := fixture.request(http.MethodGet, "/readyz", "", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("fresh edge readyz status=%d", response.StatusCode)
	}
	response.Body.Close()

	gitProbe.err = errors.New("stale Git projection")
	features = edgeFeatures(t, fixture.request(http.MethodGet, "/v1/capabilities", "", nil))
	if !features["edge"] || !features["traefik"] || !features["certManager"] || features["externalDNS"] || features["traefikMiddlewares"] {
		t.Fatalf("edge infrastructure readiness incorrectly depended on Git or advertised an Argo serving path: %#v", features)
	}

	gitProbe.err = nil
	edgeProbe.err = errors.New("stale edge observation")
	features = edgeFeatures(t, fixture.request(http.MethodGet, "/v1/capabilities", "", nil))
	for _, name := range []string{"edge", "traefik", "certManager", "externalDNS", "traefikMiddlewares"} {
		if features[name] {
			t.Fatalf("stale aggregate edge runtime advertised %q: %#v", name, features)
		}
	}
	response = fixture.request(http.MethodGet, "/readyz", "", nil)
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusServiceUnavailable || problem.Code != "EdgeRuntimeUnavailable" {
		t.Fatalf("stale edge readyz status=%d problem=%#v", response.StatusCode, problem)
	}
}

func TestExternalDNSCapabilityRequiresManagementService(t *testing.T) {
	fixture := newEdgeHTTP(t, &edgeHTTPReadiness{}, &projectionHTTPReadiness{},
		httpapi.EdgeRuntimeFeatures{ExternalDNS: true}, false)
	features := edgeFeatures(t, fixture.request(http.MethodGet, "/v1/capabilities", "", nil))
	if !features["edge"] || features["externalDNS"] {
		t.Fatalf("external-DNS runtime was advertised without its management service: %#v", features)
	}
}
