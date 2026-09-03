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
	"github.com/kuberploy/kuberploy/internal/middlewareprofiles"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	"github.com/kuberploy/kuberploy/internal/secrets"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

type middlewareSecretBackend struct {
	httpapi.RuntimeSecretBackend
	binding secrets.Binding
}

func (b middlewareSecretBackend) Binding(_ context.Context, id string) (secrets.Binding, error) {
	if id != b.binding.ID {
		return secrets.Binding{}, secrets.ErrNotFound
	}
	return b.binding, nil
}

func newMiddlewareProfileAPI(t *testing.T, backend *middlewareSecretBackend) *apiFixture {
	return newMiddlewareProfileAPIWithReadiness(t, backend, nil)
}

func newMiddlewareProfileAPIWithReadiness(t *testing.T, backend *middlewareSecretBackend, readinessErr error) *apiFixture {
	t.Helper()
	central := memory.New()
	server := httptest.NewServer(httpapi.New(httpapi.Options{
		Store: central, BootstrapToken: "one-time-secret", Version: "test",
		MiddlewareProfiles:     middlewareprofiles.NewMemoryStore(),
		RuntimeSecrets:         backend,
		GitProjection:          &projectionHTTPBackend{},
		GitProjectionReadiness: &projectionHTTPReadiness{err: readinessErr},
		ArgoReadiness:          &projectionHTTPReadiness{err: readinessErr},
		EdgeReadiness:          &edgeHTTPReadiness{err: readinessErr},
		EdgeFeatures:           httpapi.EdgeRuntimeFeatures{Traefik: true},
		HighRiskLimiter:        ratelimit.NewMemoryLimiter(10_000),
	}))
	jar, _ := cookiejar.New(nil)
	fixture := &apiFixture{t: t, server: server, client: &http.Client{Jar: jar}, store: central}
	t.Cleanup(server.Close)
	return fixture
}

func TestMiddlewareProfilesRemainUsableWhileDeliveryRuntimesConverge(t *testing.T) {
	f := newMiddlewareProfileAPIWithReadiness(t, nil, errors.New("delivery runtime converging"))
	f.bootstrap()

	response := f.request(http.MethodPost, "/v1/middlewares/validate", "", map[string]any{
		"name": "security-headers",
		"spec": map[string]any{"headers": map[string]any{"frameDeny": true}},
	})
	result := decode[struct {
		Valid bool `json:"valid"`
	}](t, response)
	if response.StatusCode != http.StatusOK || !result.Valid {
		t.Fatalf("middleware validation during delivery convergence: status=%d result=%#v", response.StatusCode, result)
	}
}

func TestBasicAuthProfileNeverCrossesItsExactEnvironment(t *testing.T) {
	now := time.Now().UTC()
	binding := secrets.Binding{
		ID: "77777777-7777-4777-8777-777777777777",
		Scope: secrets.Scope{
			OrganizationID: "11111111-1111-4111-8111-111111111111",
			ProjectID:      "22222222-2222-4222-8222-222222222222",
			EnvironmentID:  "33333333-3333-4333-8333-333333333333",
			ApplicationID:  "55555555-5555-4555-8555-555555555555",
			Namespace:      "env-a",
		},
		Name: "auth-users", Provider: secrets.ProviderSealedSecrets,
		Purpose: secrets.PurposeRuntimeSecret, State: secrets.BindingReady,
		ActiveVersion: 3, CreatedBy: "99999999-9999-4999-8999-999999999999",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := binding.Validate(); err != nil {
		t.Fatal(err)
	}
	backend := &middlewareSecretBackend{binding: binding}
	f := newMiddlewareProfileAPI(t, backend)
	admin := f.bootstrap()
	_ = admin
	teamResponse := f.request(http.MethodPost, "/v1/teams", "middleware-team-http", map[string]any{"name": "Middleware", "slug": "middleware"})
	teamView := decode[domain.Team](t, teamResponse)
	projectResponse := f.request(http.MethodPost, "/v1/projects", "middleware-project-http", map[string]any{"name": "Middleware", "slug": "middleware", "teamId": teamView.ID})
	projectView := decode[domain.Project](t, projectResponse)
	envAResponse := f.request(http.MethodPost, "/v1/environments", "middleware-env-a-http", map[string]any{"projectId": projectView.ID, "name": "Env A", "slug": "env-a"})
	envAView := decode[domain.Environment](t, envAResponse)
	envBResponse := f.request(http.MethodPost, "/v1/environments", "middleware-env-b-http", map[string]any{"projectId": projectView.ID, "name": "Env B", "slug": "env-b"})
	envBView := decode[domain.Environment](t, envBResponse)
	appResponse := f.request(http.MethodPost, "/v1/applications", "middleware-app-http", map[string]any{"projectId": projectView.ID, "name": "API", "slug": "api"})
	appView := decode[domain.Application](t, appResponse)
	backend.binding.Scope.ProjectID = projectView.ID
	backend.binding.Scope.OrganizationID = teamView.ID
	backend.binding.Scope.EnvironmentID = envAView.ID
	backend.binding.Scope.ApplicationID = appView.ID
	backend.binding.Scope.Namespace = envAView.Namespace
	// Platform-owned projects do not have a team identity, while secret scopes
	// do. Retain the valid opaque organization UUID: ancestry authorization is
	// performed by the central store before this metadata is consulted.
	if err := backend.binding.Validate(); err != nil {
		t.Fatal(err)
	}

	spec := map[string]any{"basicAuth": map[string]any{"secretBindingRef": map[string]any{
		"bindingId": binding.ID, "name": binding.Name, "key": "users", "version": binding.ActiveVersion,
	}}}
	response := f.request(http.MethodPost, "/v1/middlewares", "basic-auth-env-a", map[string]any{
		"name": "login", "spec": spec,
		"assignments": []any{map[string]any{"scope": "application", "id": appView.ID}},
	})
	created := decode[middlewareprofiles.Entry](t, response)
	if response.StatusCode != http.StatusCreated || created.Profile.ID == "" {
		t.Fatalf("exact environment BasicAuth create status=%d entry=%#v", response.StatusCode, created)
	}
	for _, test := range []struct {
		environmentID string
		want          int
	}{{envAView.ID, 1}, {envBView.ID, 0}} {
		environmentID := test.environmentID
		response = f.request(http.MethodGet, "/v1/middlewares?environmentId="+environmentID+"&applicationId="+appView.ID, "", nil)
		catalog := decode[struct {
			Items []any `json:"items"`
		}](t, response)
		if response.StatusCode != http.StatusOK || len(catalog.Items) != test.want {
			t.Fatalf("BasicAuth catalog environment %s: status=%d items=%d want=%d", environmentID, response.StatusCode, len(catalog.Items), test.want)
		}
	}
	response = f.request(http.MethodPost, "/v1/middlewares", "basic-auth-project-scope", map[string]any{
		"name": "leaky-login", "spec": spec,
		"assignments": []any{map[string]any{"scope": "project", "id": projectView.ID}},
	})
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("project-wide BasicAuth status=%d, want privacy-preserving 404", response.StatusCode)
	}
	response.Body.Close()
}

func TestMiddlewareProfileReadsRejectMissingMalformedAndDuplicateTargets(t *testing.T) {
	f := newMiddlewareProfileAPI(t, nil)
	f.bootstrap()

	for _, path := range []string{
		"/v1/middlewares",
		"/v1/middlewares?environmentId=not-a-uuid&applicationId=55555555-5555-4555-8555-555555555555",
		"/v1/middlewares?environmentId=33333333-3333-4333-8333-333333333333&applicationId=55555555-5555-4555-8555-555555555555&applicationId=66666666-6666-4666-8666-666666666666",
		"/v1/middlewares/catalog",
		"/v1/middlewares/catalog?environmentId=33333333-3333-4333-8333-333333333333&applicationId=not-a-uuid",
	} {
		response := f.request(http.MethodGet, path, "", nil)
		if response.StatusCode != http.StatusUnprocessableEntity {
			response.Body.Close()
			t.Fatalf("GET %s status=%d, want 422", path, response.StatusCode)
		}
		response.Body.Close()
	}
}
