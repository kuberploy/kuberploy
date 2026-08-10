package httpapi_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/imageresolution"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	"github.com/kuberploy/kuberploy/internal/registry"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

type imageResolutionHTTPProvider struct {
	digest string
	err    error
	calls  int
}

func (p *imageResolutionHTTPProvider) ResolveTag(_ context.Context, _ imageresolution.AuthorizedSource, _ imageresolution.TagReference, _ *imageresolution.ProviderAuthority, _ imageresolution.Platform) (string, error) {
	p.calls++
	return p.digest, p.err
}

type imageResolutionFixture struct {
	*apiFixture
	provider    *imageResolutionHTTPProvider
	resolver    *imageresolution.Resolver
	sslip       *sslipHTTPResolver
	environment domain.Environment
	application domain.Application
	target      domain.RegistryTarget
}

func newImageResolutionAPI(t *testing.T, configured bool) *imageResolutionFixture {
	t.Helper()
	central := memory.New()
	provider := &imageResolutionHTTPProvider{digest: "sha256:" + strings.Repeat("d", 64)}
	sslip := &sslipHTTPResolver{preview: validSSLIPPreview()}
	options := httpapi.Options{Store: central, BootstrapToken: "one-time-secret", Version: "test", SSLIP: sslip,
		EdgeFeatures: httpapi.EdgeRuntimeFeatures{Traefik: true}, EdgeReadiness: &edgeHTTPReadiness{},
		HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000)}
	if configured {
		targetID := "44444444-4444-4444-8444-444444444444"
		options.ImageResolution = &imageresolution.Resolver{Catalog: central, Provider: provider,
			Config: imageresolution.RuntimeConfig{AnonymousTargetIDs: []string{targetID}, Platform: imageresolution.DefaultPlatform()}}
	}
	server := httptest.NewServer(httpapi.New(options))
	jar, _ := cookiejar.New(nil)
	base := &apiFixture{t: t, server: server, client: &http.Client{Jar: jar}, store: central}
	t.Cleanup(server.Close)
	fixture := &imageResolutionFixture{apiFixture: base, provider: provider, resolver: options.ImageResolution, sslip: sslip}
	fixture.bootstrap()
	projectResponse := fixture.request(http.MethodPost, "/v1/projects", "image-resolution-project", map[string]string{"name": "Image resolution"})
	project := decode[domain.Project](t, projectResponse)
	environmentResponse := fixture.request(http.MethodPost, "/v1/environments", "image-resolution-environment", map[string]string{"projectId": project.ID, "name": "Development"})
	fixture.environment = decode[domain.Environment](t, environmentResponse)
	applicationResponse := fixture.request(http.MethodPost, "/v1/applications", "image-resolution-application", map[string]string{"projectId": project.ID, "name": "API"})
	fixture.application = decode[domain.Application](t, applicationResponse)
	if configured {
		fixture.target = domain.RegistryTarget{ID: "44444444-4444-4444-8444-444444444444", Name: "public", Mode: domain.RegistryTargetExternal,
			Endpoint: "https://registry.example.test", RepositoryPrefix: "tenant"}
		var err error
		fixture.target, err = central.PutRegistryTarget(t.Context(), fixture.target)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = central.PutServiceRegistryPolicy(t.Context(), registry.DefaultPolicy(fixture.target.ID, fixture.application.ID, "tenant/api", fixture.application.CreatedAt)); err != nil {
			t.Fatal(err)
		}
	}
	return fixture
}

func TestImageResolutionPreviewIsAuthorizedNoStoreAndSafe(t *testing.T) {
	fixture := newImageResolutionAPI(t, true)
	capabilityResponse := fixture.request(http.MethodGet, "/v1/capabilities", "", nil)
	capabilities := decode[struct {
		Features map[string]bool `json:"features"`
	}](t, capabilityResponse)
	if !capabilities.Features["imageTagResolution"] {
		t.Fatal("configured image-tag resolver was not advertised")
	}
	response := fixture.request(http.MethodPost, "/v1/deployments/image-resolution-preview", "", map[string]any{
		"environmentId": fixture.environment.ID, "applicationId": fixture.application.ID, "image": "registry.example.test/tenant/api:stable",
	})
	body := decode[map[string]any](t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(response.Header.Get("Cache-Control"), "no-store") || fixture.provider.calls != 1 ||
		body["requestedImage"] != "registry.example.test/tenant/api:stable" || body["immutableImage"] != "registry.example.test/tenant/api@"+fixture.provider.digest || body["resolved"] != true || len(body) != 3 {
		t.Fatalf("status=%d body=%#v calls=%d", response.StatusCode, body, fixture.provider.calls)
	}
	for _, forbidden := range []string{"registryTargetId", "profile", "realm", "service", "credential", "authorization", "challenge"} {
		if _, present := body[forbidden]; present {
			t.Fatalf("unsafe field %q was projected", forbidden)
		}
	}
	response = fixture.request(http.MethodPost, "/v1/deployments/image-resolution-preview", "", map[string]any{
		"environmentId": fixture.environment.ID, "applicationId": fixture.application.ID, "image": "registry.example.test/tenant/api:stable", "registryTargetId": fixture.target.ID,
	})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("caller authority field status=%d", response.StatusCode)
	}
	response.Body.Close()
}

func TestExistingImageTagPersistsOnlyDigestAndReplaysBeforeTagAndSSLIPResolution(t *testing.T) {
	fixture := newImageResolutionAPI(t, true)
	body := map[string]any{
		"environmentId": fixture.environment.ID, "applicationId": fixture.application.ID, "image": "registry.example.test/tenant/api:stable",
		"expectedImmutableImage": "registry.example.test/tenant/api@" + fixture.provider.digest,
		"runtime":                map[string]any{"replicas": 1, "ports": []map[string]any{{"name": "http", "containerPort": 8080}}, "resources": map[string]any{"requests": map[string]string{"cpu": "50m", "memory": "100Mi"}}},
		"route":                  map[string]any{"dnsMode": "sslip", "pathPrefix": "/", "tlsMode": "httpOnly"},
	}
	response := fixture.request(http.MethodPost, "/v1/deployments", "image-tag-stable-key", body)
	if response.StatusCode != http.StatusAccepted {
		problemBody, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("status=%d body=%s calls=%d", response.StatusCode, problemBody, fixture.provider.calls)
	}
	operation := decode[domain.Operation](t, response)
	if response.StatusCode != http.StatusAccepted || operation.ID == "" || fixture.provider.calls != 1 {
		t.Fatalf("status=%d operation=%+v calls=%d", response.StatusCode, operation, fixture.provider.calls)
	}
	snapshot, err := fixture.store.GetDeploymentForOperation(t.Context(), operation.ID)
	if err != nil || snapshot.Image != "registry.example.test/tenant/api@"+fixture.provider.digest || strings.Contains(snapshot.Image, ":stable") || snapshot.Route == nil || snapshot.Route.Hostname != fixture.sslip.preview.Hostname {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	fixture.provider.err = errors.New("provider should not be called on replay")
	fixture.resolver.Config = imageresolution.RuntimeConfig{}
	fixture.sslip.err = errors.New("sslip should not be called on replay")
	response = fixture.request(http.MethodPost, "/v1/deployments", "image-tag-stable-key", body)
	replay := decode[domain.Operation](t, response)
	if response.StatusCode != http.StatusAccepted || response.Header.Get("Idempotent-Replay") != "true" || replay.ID != operation.ID || fixture.provider.calls != 1 {
		t.Fatalf("status=%d replay=%+v calls=%d", response.StatusCode, replay, fixture.provider.calls)
	}
	body["expectedImmutableImage"] = "registry.example.test/tenant/api@sha256:" + strings.Repeat("e", 64)
	response = fixture.request(http.MethodPost, "/v1/deployments", "image-tag-stable-key", body)
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusConflict || problem.Code != "IdempotencyConflict" || fixture.provider.calls != 1 {
		t.Fatalf("changed precondition status=%d problem=%+v calls=%d", response.StatusCode, problem, fixture.provider.calls)
	}
}

func TestExistingImageTagRejectsMissingOrMovedPreviewPrecondition(t *testing.T) {
	fixture := newImageResolutionAPI(t, true)
	body := map[string]any{
		"environmentId": fixture.environment.ID,
		"applicationId": fixture.application.ID,
		"image":         "registry.example.test/tenant/api:stable",
		"runtime": map[string]any{
			"replicas": 1,
			"ports":    []map[string]any{{"name": "http", "containerPort": 8080}},
			"resources": map[string]any{"requests": map[string]string{
				"cpu": "50m", "memory": "100Mi",
			}},
		},
	}
	response := fixture.request(http.MethodPost, "/v1/deployments", "image-tag-missing-preview", body)
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusUnprocessableEntity || problem.Code != "ValidationFailed" || fixture.provider.calls != 0 {
		t.Fatalf("missing status=%d problem=%+v calls=%d", response.StatusCode, problem, fixture.provider.calls)
	}

	body["expectedImmutableImage"] = "registry.example.test/tenant/api@sha256:" + strings.Repeat("e", 64)
	response = fixture.request(http.MethodPost, "/v1/deployments", "image-tag-moved-preview", body)
	problem = decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusConflict || problem.Code != "ImageTagMoved" || fixture.provider.calls != 1 {
		t.Fatalf("moved status=%d problem=%+v calls=%d", response.StatusCode, problem, fixture.provider.calls)
	}

	body["image"] = "registry.example.test/tenant/api@" + fixture.provider.digest
	response = fixture.request(http.MethodPost, "/v1/deployments", "image-digest-forbids-preview", body)
	problem = decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusUnprocessableEntity || problem.Code != "ValidationFailed" || fixture.provider.calls != 1 {
		t.Fatalf("digest status=%d problem=%+v calls=%d", response.StatusCode, problem, fixture.provider.calls)
	}
}

func TestImageResolutionPreviewFailsBeforeDecodingWhenUnconfigured(t *testing.T) {
	fixture := newImageResolutionAPI(t, false)
	request, err := http.NewRequest(http.MethodPost, fixture.server.URL+"/v1/deployments/image-resolution-preview", bytes.NewBufferString(`{"image":`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", fixture.csrf)
	response, err := fixture.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusServiceUnavailable || problem.Code != "ImageResolutionUnavailable" {
		t.Fatalf("status=%d problem=%+v", response.StatusCode, problem)
	}
}
