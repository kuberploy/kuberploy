package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	"github.com/kuberploy/kuberploy/internal/store"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

type sslipHTTPResolver struct {
	preview  httpapi.SSLIPHostnamePreview
	err      error
	probeErr error
	request  httpapi.SSLIPHostnameRequest
}

func (r *sslipHTTPResolver) Probe(context.Context) error { return r.probeErr }

func (r *sslipHTTPResolver) ResolveSSLIPHostname(_ context.Context, request httpapi.SSLIPHostnameRequest) (httpapi.SSLIPHostnamePreview, error) {
	r.request = request
	return r.preview, r.err
}

type sslipAPIFixture struct {
	*apiFixture
	resolver    *sslipHTTPResolver
	project     domain.Project
	environment domain.Environment
	application domain.Application
}

func newSSLIPAPI(t *testing.T, resolver *sslipHTTPResolver, readiness *edgeHTTPReadiness, traefik bool) *sslipAPIFixture {
	t.Helper()
	central := memory.New()
	options := httpapi.Options{
		Store: central, BootstrapToken: "one-time-secret", Version: "test",
		EdgeFeatures: httpapi.EdgeRuntimeFeatures{Traefik: traefik}, HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000),
	}
	if resolver != nil {
		options.SSLIP = resolver
	}
	if readiness != nil {
		options.EdgeReadiness = readiness
	}
	server := httptest.NewServer(httpapi.New(options))
	jar, _ := cookiejar.New(nil)
	base := &apiFixture{t: t, server: server, client: &http.Client{Jar: jar}, store: central}
	t.Cleanup(server.Close)
	fixture := &sslipAPIFixture{apiFixture: base, resolver: resolver}
	fixture.bootstrap()
	teamResponse := fixture.request(http.MethodPost, "/v1/teams", "sslip-team", map[string]string{"name": "sslip", "slug": "sslip"})
	team := decode[domain.Team](t, teamResponse)
	projectResponse := fixture.request(http.MethodPost, "/v1/projects", "sslip-project", map[string]string{"name": "sslip", "slug": "sslip", "teamId": team.ID})
	fixture.project = decode[domain.Project](t, projectResponse)
	environmentResponse := fixture.request(http.MethodPost, "/v1/environments", "sslip-environment", map[string]string{"projectId": fixture.project.ID, "name": "Production", "slug": "production"})
	fixture.environment = decode[domain.Environment](t, environmentResponse)
	applicationResponse := fixture.request(http.MethodPost, "/v1/applications", "sslip-application", map[string]string{"projectId": fixture.project.ID, "name": "API", "slug": "api"})
	fixture.application = decode[domain.Application](t, applicationResponse)
	return fixture
}

func validSSLIPPreview() httpapi.SSLIPHostnamePreview {
	return httpapi.SSLIPHostnamePreview{
		Mode: "sslip", Hostname: "kp-32f2a03ad59c10f96a1a.8-8-8-8.sslip.io", Source: httpapi.SSLIPSourceServiceIP,
		ObservedAt: time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC),
	}
}

func TestCreateDeploymentSSLIPRouteUsesOnlyServerDerivedHostname(t *testing.T) {
	resolver := &sslipHTTPResolver{preview: validSSLIPPreview()}
	fixture := newSSLIPAPI(t, resolver, &edgeHTTPReadiness{}, true)
	body := map[string]any{
		"environmentId": fixture.environment.ID,
		"applicationId": fixture.application.ID,
		"image":         "registry.example.test/team/api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"replicas":      1,
		"port":          8080,
		"route": map[string]any{
			"dnsMode": "sslip", "pathPrefix": "/", "tlsMode": "httpOnly",
		},
	}
	response := fixture.request(http.MethodPost, "/v1/deployments", "sslip-create-0001", body)
	operation := decode[domain.Operation](t, response)
	if response.StatusCode != http.StatusAccepted || operation.TargetID == "" {
		t.Fatalf("status=%d operation=%#v", response.StatusCode, operation)
	}
	response = fixture.request(http.MethodGet, "/v1/deployments/"+operation.TargetID, "", nil)
	deployment := decode[domain.Deployment](t, response)
	if deployment.Route == nil || deployment.Route.DNSMode != "sslip" || deployment.Route.Hostname != resolver.preview.Hostname {
		t.Fatalf("deployment route=%#v", deployment.Route)
	}
	if resolver.request.ApplicationID != fixture.application.ID || resolver.request.EnvironmentID != fixture.environment.ID ||
		resolver.request.ProjectID != fixture.project.ID || resolver.request.Namespace != fixture.environment.Namespace {
		t.Fatalf("create resolver request was not exact server-derived scope: %#v", resolver.request)
	}

	for key, hostname := range map[string]string{"caller-host": "caller.8-8-8-8.sslip.io", "caller-ip-host": resolver.preview.Hostname} {
		bad := map[string]any{
			"environmentId": fixture.environment.ID, "applicationId": fixture.application.ID,
			"image": body["image"], "replicas": 1, "port": 8080,
			"route": map[string]any{"dnsMode": "sslip", "hostname": hostname, "pathPrefix": "/", "tlsMode": "httpOnly"},
		}
		response = fixture.request(http.MethodPost, "/v1/deployments", "sslip-"+key+"-0001", bad)
		if response.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("caller hostname %q status=%d", hostname, response.StatusCode)
		}
		response.Body.Close()
	}
	callerIP := map[string]any{
		"environmentId": fixture.environment.ID, "applicationId": fixture.application.ID,
		"image": body["image"], "replicas": 1, "port": 8080,
		"route": map[string]any{"dnsMode": "sslip", "ip": "8.8.8.8", "pathPrefix": "/", "tlsMode": "httpOnly"},
	}
	response = fixture.request(http.MethodPost, "/v1/deployments", "sslip-caller-ip-0001", callerIP)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("caller IP field status=%d", response.StatusCode)
	}
	response.Body.Close()
}

func TestCreateDeploymentManualRouteKeepsOptionalDNSModeCompatibility(t *testing.T) {
	fixture := newSSLIPAPI(t, nil, nil, false)
	body := map[string]any{
		"environmentId": fixture.environment.ID,
		"applicationId": fixture.application.ID,
		"image":         "registry.example.test/team/api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"replicas":      1,
		"port":          8080,
		"route":         map[string]any{"hostname": "api.example.test"},
	}
	response := fixture.request(http.MethodPost, "/v1/deployments", "manual-create-0001", body)
	operation := decode[domain.Operation](t, response)
	if response.StatusCode != http.StatusAccepted || operation.TargetID == "" {
		t.Fatalf("status=%d operation=%#v", response.StatusCode, operation)
	}
	response = fixture.request(http.MethodGet, "/v1/deployments/"+operation.TargetID, "", nil)
	deployment := decode[domain.Deployment](t, response)
	if deployment.Route == nil || deployment.Route.DNSMode != "manual" || deployment.Route.Hostname != "api.example.test" ||
		deployment.Route.PathPrefix != "/" || deployment.Route.TLSMode != "httpOnly" {
		t.Fatalf("manual deployment route=%#v", deployment.Route)
	}
}

func TestSSLIPHostnameHTTPReturnsOnlyServerDerivedFreshMetadata(t *testing.T) {
	resolver := &sslipHTTPResolver{preview: validSSLIPPreview()}
	fixture := newSSLIPAPI(t, resolver, &edgeHTTPReadiness{}, true)
	response := fixture.request(http.MethodGet, "/v1/applications/"+fixture.application.ID+"/sslip-hostname?environmentId="+fixture.environment.ID, "", nil)
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "private, no-store" || response.Header.Get("Pragma") != "no-cache" {
		t.Fatalf("status=%d headers=%#v", response.StatusCode, response.Header)
	}
	preview := decode[map[string]any](t, response)
	if len(preview) != 4 || preview["mode"] != "sslip" || preview["hostname"] != resolver.preview.Hostname || preview["source"] != "service-ip" || preview["observedAt"] == "" {
		t.Fatalf("preview=%#v", preview)
	}
	for _, forbidden := range []string{"ip", "address", "namespace", "projectId", "applicationId", "environmentId"} {
		if _, exposed := preview[forbidden]; exposed {
			t.Fatalf("preview exposed %q: %#v", forbidden, preview)
		}
	}
	if resolver.request.ApplicationID != fixture.application.ID || resolver.request.EnvironmentID != fixture.environment.ID ||
		resolver.request.ProjectID != fixture.project.ID || resolver.request.Namespace != fixture.environment.Namespace {
		t.Fatalf("resolver request was not exact server-derived scope: %#v", resolver.request)
	}
	features := edgeFeatures(t, fixture.request(http.MethodGet, "/v1/capabilities", "", nil))
	if !features["sslip"] {
		t.Fatalf("fresh exact sslip backend was not advertised: %#v", features)
	}
}

func TestSSLIPHostnameRequiresApplicationAndEnvironmentAuthorization(t *testing.T) {
	resolver := &sslipHTTPResolver{preview: validSSLIPPreview()}
	fixture := newSSLIPAPI(t, resolver, &edgeHTTPReadiness{}, true)

	invitationResponse := fixture.request(http.MethodPost, "/v1/users/invitations", "sslip-authz-invite", map[string]string{"email": "scoped.reader@example.com"})
	invitation := decode[domain.UserInvitation](t, invitationResponse)
	if invitationResponse.StatusCode != http.StatusCreated {
		t.Fatalf("invitation status=%d", invitationResponse.StatusCode)
	}
	const password = "scoped reader correct horse battery staple"
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	developerClient := &http.Client{Jar: jar}
	acceptBody, _ := json.Marshal(map[string]string{"token": invitation.Token, "displayName": "Scoped reader", "password": password})
	acceptRequest, err := http.NewRequest(http.MethodPost, fixture.server.URL+"/v1/auth/invitations/accept", bytes.NewReader(acceptBody))
	if err != nil {
		t.Fatal(err)
	}
	acceptRequest.Header.Set("Content-Type", "application/json")
	acceptResponse, err := developerClient.Do(acceptRequest)
	if err != nil {
		t.Fatal(err)
	}
	developer := decode[domain.User](t, acceptResponse)
	if acceptResponse.StatusCode != http.StatusCreated || developer.ID == "" {
		t.Fatalf("accept status=%d developer=%#v", acceptResponse.StatusCode, developer)
	}

	grantResponse := fixture.request(http.MethodPost, "/v1/projects/"+fixture.project.ID+"/grants", "sslip-authz-grant", map[string]any{
		"subjectUserId": developer.ID, "role": "viewer", "scopeType": "application", "scopeId": fixture.application.ID,
	})
	grant := decode[domain.AccessGrant](t, grantResponse)
	if grantResponse.StatusCode != http.StatusCreated || grant.SubjectUserID != developer.ID {
		t.Fatalf("grant status=%d grant=%#v", grantResponse.StatusCode, grant)
	}
	if err := fixture.store.Authorize(t.Context(), developer.ID, domain.PermissionResourcesRead, domain.AccessTarget{Type: "application", ID: fixture.application.ID}); err != nil {
		t.Fatalf("application grant was not durable: %v", err)
	}
	if err := fixture.store.Authorize(t.Context(), developer.ID, domain.PermissionResourcesRead, domain.AccessTarget{Type: "environment", ID: fixture.environment.ID}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unexpected environment authorization before request: %v", err)
	}

	loginBody, _ := json.Marshal(map[string]string{"email": "scoped.reader@example.com", "password": password})
	loginRequest, err := http.NewRequest(http.MethodPost, fixture.server.URL+"/v1/auth/login", bytes.NewReader(loginBody))
	if err != nil {
		t.Fatal(err)
	}
	loginRequest.Header.Set("Content-Type", "application/json")
	loginResponse, err := developerClient.Do(loginRequest)
	if err != nil {
		t.Fatal(err)
	}
	if loginResponse.StatusCode != http.StatusOK {
		problem := decode[httpapi.Problem](t, loginResponse)
		t.Fatalf("developer login status=%d problem=%#v", loginResponse.StatusCode, problem)
	}
	loginResponse.Body.Close()

	request, err := http.NewRequest(http.MethodGet, fixture.server.URL+"/v1/applications/"+fixture.application.ID+"/sslip-hostname?environmentId="+fixture.environment.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := developerClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	problem := decode[httpapi.Problem](t, response)
	if (response.StatusCode != http.StatusForbidden && response.StatusCode != http.StatusNotFound) || (problem.Code != "Forbidden" && problem.Code != "NotFound") || resolver.request.ApplicationID != "" {
		t.Fatalf("application-only grant leaked sslip hostname: status=%d problem=%#v", response.StatusCode, problem)
	}
}

func TestSSLIPHostnameHTTPRejectsCallerFieldsAncestryAndInvalidObservation(t *testing.T) {
	resolver := &sslipHTTPResolver{preview: validSSLIPPreview()}
	fixture := newSSLIPAPI(t, resolver, &edgeHTTPReadiness{}, true)
	base := "/v1/applications/" + fixture.application.ID + "/sslip-hostname"
	for _, query := range []string{"", "?environmentId=", "?environmentId=not-a-uuid", "?environmentId=" + fixture.environment.ID + "&environmentId=" + fixture.environment.ID, "?environmentId=" + fixture.environment.ID + "&ip=203.0.113.10", "?hostname=caller.sslip.io"} {
		response := fixture.request(http.MethodGet, base+query, "", nil)
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("query %q status=%d", query, response.StatusCode)
		}
		response.Body.Close()
	}

	otherProjectResponse := fixture.request(http.MethodPost, "/v1/projects", "sslip-other-project", map[string]string{"name": "Other", "slug": "sslip-other"})
	otherProject := decode[domain.Project](t, otherProjectResponse)
	otherEnvironmentResponse := fixture.request(http.MethodPost, "/v1/environments", "sslip-other-environment", map[string]string{"projectId": otherProject.ID, "name": "Other", "slug": "other"})
	otherEnvironment := decode[domain.Environment](t, otherEnvironmentResponse)
	response := fixture.request(http.MethodGet, base+"?environmentId="+otherEnvironment.ID, "", nil)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-project environment status=%d", response.StatusCode)
	}
	response.Body.Close()

	resolver.preview.Hostname = "caller.example.com"
	response = fixture.request(http.MethodGet, base+"?environmentId="+fixture.environment.ID, "", nil)
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusServiceUnavailable || problem.Code != "SSLIPHostnameUnavailable" {
		t.Fatalf("invalid resolver output status=%d problem=%#v", response.StatusCode, problem)
	}
	resolver.preview = validSSLIPPreview()
	resolver.err = errors.New("stale provider detail must not escape")
	response = fixture.request(http.MethodGet, base+"?environmentId="+fixture.environment.ID, "", nil)
	problem = decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusServiceUnavailable || problem.Code != "SSLIPHostnameUnavailable" {
		t.Fatalf("resolver failure status=%d problem=%#v", response.StatusCode, problem)
	}
}

func TestSSLIPCapabilityAndRouteRequireResolverTraefikAndFreshTargetReadiness(t *testing.T) {
	for _, test := range []struct {
		name      string
		resolver  *sslipHTTPResolver
		readiness *edgeHTTPReadiness
		traefik   bool
	}{
		{name: "no resolver", readiness: &edgeHTTPReadiness{}, traefik: true},
		{name: "stale target readiness", resolver: &sslipHTTPResolver{preview: validSSLIPPreview(), probeErr: errors.New("stale")}, readiness: &edgeHTTPReadiness{}, traefik: true},
		{name: "no traefik", resolver: &sslipHTTPResolver{preview: validSSLIPPreview()}, readiness: &edgeHTTPReadiness{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSSLIPAPI(t, test.resolver, test.readiness, test.traefik)
			features := edgeFeatures(t, fixture.request(http.MethodGet, "/v1/capabilities", "", nil))
			if features["sslip"] {
				t.Fatalf("unready sslip advertised: %#v", features)
			}
			response := fixture.request(http.MethodGet, "/v1/applications/"+fixture.application.ID+"/sslip-hostname?environmentId="+fixture.environment.ID, "", nil)
			problem := decode[httpapi.Problem](t, response)
			if response.StatusCode != http.StatusServiceUnavailable || problem.Code != "SSLIPHostnameUnavailable" {
				t.Fatalf("status=%d problem=%#v", response.StatusCode, problem)
			}
		})
	}
	t.Run("unrelated global digest is stale", func(t *testing.T) {
		fixture := newSSLIPAPI(t, &sslipHTTPResolver{preview: validSSLIPPreview()}, &edgeHTTPReadiness{err: errors.New("stale global digest")}, true)
		features := edgeFeatures(t, fixture.request(http.MethodGet, "/v1/capabilities", "", nil))
		if !features["sslip"] {
			t.Fatalf("healthy target-scoped sslip was not advertised: %#v", features)
		}
		response := fixture.request(http.MethodGet, "/v1/applications/"+fixture.application.ID+"/sslip-hostname?environmentId="+fixture.environment.ID, "", nil)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status=%d", response.StatusCode)
		}
	})
}
