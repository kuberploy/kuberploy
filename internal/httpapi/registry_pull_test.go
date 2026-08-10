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

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/imagepull"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	"github.com/kuberploy/kuberploy/internal/registry"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

type registryPullHTTPResolver struct {
	reference  domain.RegistryPullReference
	present    bool
	err        error
	calls      int
	repository string
}

func (r *registryPullHTTPResolver) ResolveRegistryPull(_ context.Context, _ imagepull.RuntimeConfig, _, _, repository string) (domain.RegistryPullReference, bool, error) {
	r.calls++
	r.repository = repository
	return r.reference, r.present, r.err
}

type registryPullHTTPReadiness struct {
	err   error
	calls int
}

func (r *registryPullHTTPReadiness) Probe(context.Context) error {
	r.calls++
	return r.err
}

func newRegistryPullAPI(
	t *testing.T,
	projection *projectionHTTPBackend,
	gitReadiness *projectionHTTPReadiness,
	resolver *registryPullHTTPResolver,
	pullReadiness *registryPullHTTPReadiness,
) *apiFixture {
	t.Helper()
	central := memory.New()
	server := httptest.NewServer(httpapi.New(httpapi.Options{
		Store: central, BootstrapToken: "one-time-secret", Version: "test",
		GitProjection: projection, GitProjectionReadiness: gitReadiness, ArgoReadiness: &projectionHTTPReadiness{},
		RegistryPulls: resolver, RegistryPullReadiness: pullReadiness,
		Registry:        registry.NewManagement(central, nil),
		HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000),
	}))
	jar, _ := cookiejar.New(nil)
	fixture := &apiFixture{t: t, server: server, client: &http.Client{Jar: jar}, store: central}
	t.Cleanup(server.Close)
	return fixture
}

func configureRegistryPullProjection(
	t *testing.T,
	fixture *apiFixture,
	backend *projectionHTTPBackend,
	actor domain.User,
) (domain.Environment, domain.Application) {
	t.Helper()
	project, err := fixture.store.CreateProject(t.Context(), actor.ID, "registry-pull-project", "registry-pull-project", domain.CreateProject{Name: "Registry pull", Slug: "registry-pull"})
	if err != nil {
		t.Fatal(err)
	}
	environment, err := fixture.store.CreateEnvironment(t.Context(), actor.ID, "registry-pull-environment", "registry-pull-environment", domain.CreateEnvironment{ProjectID: project.Value.ID, Name: "Production", Slug: "production"})
	if err != nil {
		t.Fatal(err)
	}
	application, err := fixture.store.CreateApplication(t.Context(), actor.ID, "registry-pull-application", "registry-pull-application", domain.CreateApplication{ProjectID: project.Value.ID, Name: "API", Slug: "api"})
	if err != nil {
		t.Fatal(err)
	}
	binding := projectedHTTPBinding(t, project.Value.ID, environment.Value.ID, time.Now().UTC().Add(-time.Minute))
	if err = fixture.store.PutBinding(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	backend.plan = gitprojection.WritePlan{
		BindingID: binding.ID, ProjectID: project.Value.ID, EnvironmentID: environment.Value.ID,
		ApplicationID: application.Value.ID, BaseRevision: binding.IndexedRevision,
		Precondition: gitprojection.MutationCreateIfAbsent,
		ChartDigest:  "sha256:" + strings.Repeat("d", 64), PolicyVersion: "appconfig-v1alpha1",
	}
	return environment.Value, application.Value
}

func registryPullDeploymentBody(environmentID, applicationID, repository string) map[string]any {
	return map[string]any{
		"environmentId": environmentID,
		"applicationId": applicationID,
		"image":         repository + "@sha256:" + strings.Repeat("a", 64),
		"runtime":       domain.DefaultWorkloadRuntime(8080, nil),
	}
}

func TestPrivateRegistryPullCreateIsServerDerivedAndReplaySafe(t *testing.T) {
	backend := &projectionHTTPBackend{}
	resolver := &registryPullHTTPResolver{present: true, reference: domain.RegistryPullReference{
		TargetID: "77777777-7777-4777-8777-777777777777", ProfileName: "managed-main", ProfileRevision: 4,
	}}
	pullReadiness := &registryPullHTTPReadiness{}
	fixture := newRegistryPullAPI(t, backend, &projectionHTTPReadiness{}, resolver, pullReadiness)
	admin := fixture.bootstrap()
	environment, application := configureRegistryPullProjection(t, fixture, backend, admin)
	body := registryPullDeploymentBody(environment.ID, application.ID, "registry.example.test/team/api")

	response := fixture.request(http.MethodPost, "/v1/deployments", "private-pull-create", body)
	operation := decode[domain.Operation](t, response)
	if response.StatusCode != http.StatusAccepted || resolver.repository != "registry.example.test/team/api" || pullReadiness.calls != 1 {
		t.Fatalf("private create status=%d resolver=%q readinessCalls=%d operation=%#v", response.StatusCode, resolver.repository, pullReadiness.calls, operation)
	}
	deployment, err := fixture.store.GetDeploymentForOperation(t.Context(), operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if deployment.RegistryPull == nil || *deployment.RegistryPull != resolver.reference {
		t.Fatalf("server-derived pull reference not retained in accepted snapshot: %#v", deployment.RegistryPull)
	}
	config := string(deployment.ConfigRaw)
	for _, expected := range []string{
		"    registryPull:\n",
		`      targetId: "77777777-7777-4777-8777-777777777777"`,
		`      profileName: "managed-main"`,
		"      profileRevision: 4\n",
	} {
		if !strings.Contains(config, expected) {
			t.Fatalf("server-derived pull metadata %q missing:\n%s", expected, config)
		}
	}
	for _, forbidden := range []string{"credentialRef", "sourceSecret", "secretName", ".dockerconfigjson"} {
		if strings.Contains(config, forbidden) {
			t.Fatalf("credential metadata %q entered AppConfig:\n%s", forbidden, config)
		}
	}

	resolver.err = imagepull.ErrUnavailable
	response = fixture.request(http.MethodPost, "/v1/deployments", "private-pull-create", body)
	replay := decode[domain.Operation](t, response)
	if response.StatusCode != http.StatusAccepted || replay.ID != operation.ID || response.Header.Get("Idempotent-Replay") != "true" || fixture.store.OutboxCount() != 1 {
		t.Fatalf("lost-response replay status=%d operation=%#v outbox=%d", response.StatusCode, replay, fixture.store.OutboxCount())
	}
}

func TestPrivateRegistryPullCreateFailsClosedButPublicImageOmitsMetadata(t *testing.T) {
	t.Run("private runtime unavailable", func(t *testing.T) {
		backend := &projectionHTTPBackend{}
		resolver := &registryPullHTTPResolver{present: true, reference: domain.RegistryPullReference{
			TargetID: "77777777-7777-4777-8777-777777777777", ProfileName: "managed-main", ProfileRevision: 4,
		}}
		pullReadiness := &registryPullHTTPReadiness{err: errors.New("stale worker")}
		fixture := newRegistryPullAPI(t, backend, &projectionHTTPReadiness{}, resolver, pullReadiness)
		admin := fixture.bootstrap()
		environment, application := configureRegistryPullProjection(t, fixture, backend, admin)
		response := fixture.request(http.MethodPost, "/v1/deployments", "private-pull-stale", registryPullDeploymentBody(environment.ID, application.ID, "registry.example.test/team/api"))
		problem := decode[httpapi.Problem](t, response)
		if response.StatusCode != http.StatusServiceUnavailable || problem.Code != "RegistryPullRuntimeUnavailable" || fixture.store.OutboxCount() != 0 {
			t.Fatalf("stale runtime status=%d problem=%#v outbox=%d", response.StatusCode, problem, fixture.store.OutboxCount())
		}
	})

	t.Run("public image", func(t *testing.T) {
		backend := &projectionHTTPBackend{}
		resolver := &registryPullHTTPResolver{}
		pullReadiness := &registryPullHTTPReadiness{err: errors.New("must not be called")}
		fixture := newRegistryPullAPI(t, backend, &projectionHTTPReadiness{}, resolver, pullReadiness)
		admin := fixture.bootstrap()
		environment, application := configureRegistryPullProjection(t, fixture, backend, admin)
		response := fixture.request(http.MethodPost, "/v1/deployments", "public-pull-create", registryPullDeploymentBody(environment.ID, application.ID, "docker.io/library/nginx"))
		operation := decode[domain.Operation](t, response)
		if response.StatusCode != http.StatusAccepted || pullReadiness.calls != 0 {
			t.Fatalf("public create status=%d readinessCalls=%d operation=%#v", response.StatusCode, pullReadiness.calls, operation)
		}
		deployment, err := fixture.store.GetDeploymentForOperation(t.Context(), operation.ID)
		if err != nil {
			t.Fatal(err)
		}
		if deployment.RegistryPull != nil || strings.Contains(string(deployment.ConfigRaw), "registryPull:") {
			t.Fatalf("public image gained private pull metadata: %#v\n%s", deployment.RegistryPull, deployment.ConfigRaw)
		}
	})
}

func TestPrivateRegistryPullReadinessAndCapabilityAreExact(t *testing.T) {
	backend := &projectionHTTPBackend{}
	gitReadiness := &projectionHTTPReadiness{}
	pullReadiness := &registryPullHTTPReadiness{}
	fixture := newRegistryPullAPI(t, backend, gitReadiness, &registryPullHTTPResolver{}, pullReadiness)
	fixture.bootstrap()

	response := fixture.request(http.MethodGet, "/v1/capabilities", "", nil)
	capabilities := decode[struct {
		Features map[string]bool `json:"features"`
	}](t, response)
	if response.StatusCode != http.StatusOK || capabilities.Features["privateRegistryPulls"] {
		t.Fatalf("private pulls were advertised before Argo readiness: %#v", capabilities.Features)
	}

	pullReadiness.err = errors.New("stale private pull worker")
	response = fixture.request(http.MethodGet, "/v1/capabilities", "", nil)
	capabilities = decode[struct {
		Features map[string]bool `json:"features"`
	}](t, response)
	if response.StatusCode != http.StatusOK || capabilities.Features["privateRegistryPulls"] {
		t.Fatalf("stale private pull worker was advertised: %#v", capabilities.Features)
	}
	response = fixture.request(http.MethodGet, "/readyz", "", nil)
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusServiceUnavailable || problem.Code != "RegistryPullRuntimeUnavailable" {
		t.Fatalf("stale private pull readiness status=%d problem=%#v", response.StatusCode, problem)
	}
}
