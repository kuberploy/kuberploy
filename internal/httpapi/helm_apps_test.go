package httpapi_test

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/helmdirect"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

type helmHTTPBackend struct {
	capabilities helmdirect.Capabilities
	revision     helmdirect.Revision
	request      helmdirect.DeployRequest
	deployCalls  int
}

func (b *helmHTTPBackend) Capabilities(context.Context) (helmdirect.Capabilities, error) {
	return b.capabilities, nil
}
func (b *helmHTTPBackend) Deploy(_ context.Context, request helmdirect.DeployRequest, _ time.Time) (helmdirect.Revision, bool, error) {
	b.deployCalls++
	b.request = request
	return b.revision, false, nil
}
func (b *helmHTTPBackend) Retry(context.Context, helmdirect.MutationRequest, time.Time) (helmdirect.Revision, bool, error) {
	return b.revision, false, nil
}
func (b *helmHTTPBackend) Disable(context.Context, helmdirect.MutationRequest, time.Time) (helmdirect.Revision, bool, error) {
	return b.revision, false, nil
}
func (b *helmHTTPBackend) Rollback(context.Context, helmdirect.MutationRequest, time.Time) (helmdirect.Revision, bool, error) {
	return b.revision, false, nil
}
func (b *helmHTTPBackend) Head(context.Context, helmdirect.Target) (helmdirect.Revision, error) {
	return b.revision, nil
}
func (b *helmHTTPBackend) History(context.Context, helmdirect.Target, int) ([]helmdirect.Revision, error) {
	return []helmdirect.Revision{b.revision}, nil
}

type helmHTTPFixture struct {
	*apiFixture
	backend     *helmHTTPBackend
	project     domain.Project
	environment domain.Environment
	application domain.Application
}

func newHelmHTTPFixture(t *testing.T, backend *helmHTTPBackend) *helmHTTPFixture {
	t.Helper()
	central := memory.New()
	server := httptest.NewServer(httpapi.New(httpapi.Options{Store: central, BootstrapToken: "one-time-secret",
		Version: "test", HelmApplications: backend, HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000)}))
	jar, _ := cookiejar.New(nil)
	base := &apiFixture{t: t, server: server, client: &http.Client{Jar: jar}, store: central}
	t.Cleanup(server.Close)
	fixture := &helmHTTPFixture{apiFixture: base, backend: backend}
	fixture.bootstrap()
	team := decode[domain.Team](t, fixture.request(http.MethodPost, "/v1/teams", "helm-team-create", map[string]string{"name": "Helm", "slug": "helm"}))
	fixture.project = decode[domain.Project](t, fixture.request(http.MethodPost, "/v1/projects", "helm-project-create", map[string]string{"name": "Helm", "slug": "helm", "teamId": team.ID}))
	fixture.environment = decode[domain.Environment](t, fixture.request(http.MethodPost, "/v1/environments", "helm-environment-create", map[string]string{"projectId": fixture.project.ID, "name": "Production", "slug": "production"}))
	fixture.application = decode[domain.Application](t, fixture.request(http.MethodPost, "/v1/applications", "helm-application-create",
		map[string]string{"projectId": fixture.project.ID, "environmentId": fixture.environment.ID, "name": "Valkey", "slug": "valkey", "sourceKind": "helm"}))
	now := time.Now().UTC()
	values := []byte("replicaCount: 1\n")
	backend.revision = helmdirect.Revision{ID: "66666666-6666-4666-8666-666666666666", Generation: 1,
		Target:      helmdirect.Target{ProjectID: fixture.project.ID, EnvironmentID: fixture.environment.ID, ApplicationID: fixture.application.ID},
		ReleaseName: "valkey", DestinationNamespace: fixture.environment.Namespace, ArgoProject: fixture.environment.ArgoProject,
		Source:     helmdirect.Source{Kind: helmdirect.SourceGit, RepositoryURL: "https://github.com/valkey-io/valkey-helm.git", Path: "valkey", TargetRevision: "main"},
		ValuesYAML: values, ValuesDigest: helmdirect.Digest(values), Action: helmdirect.ActionDeploy, DesiredEnabled: true,
		State: helmdirect.StateApplied, ActorID: "11111111-1111-4111-8111-111111111111", IdempotencyKey: "helm-release-upsert-0001",
		RequestID: "request", CreatedAt: now, UpdatedAt: now}
	return fixture
}

func (f *helmHTTPFixture) basePath() string {
	return "/v1/applications/" + f.application.ID + "/environments/" + f.environment.ID + "/helm"
}

func TestHelmDeployForwardsSourceAndValuesWithoutApproval(t *testing.T) {
	backend := &helmHTTPBackend{capabilities: helmdirect.Capabilities{HelmDeployments: true, HelmRollbacks: true}}
	fixture := newHelmHTTPFixture(t, backend)
	response := fixture.request(http.MethodPut, fixture.basePath()+"/release", "helm-release-upsert-0001", map[string]any{
		"source":     map[string]string{"kind": "git", "repositoryUrl": "https://github.com/valkey-io/valkey-helm.git", "targetRevision": "main", "path": "valkey", "chart": ""},
		"valuesYaml": "replicaCount: 1\n",
	})
	if response.StatusCode != http.StatusAccepted || backend.deployCalls != 1 {
		t.Fatalf("status=%d calls=%d", response.StatusCode, backend.deployCalls)
	}
	if backend.request.Source.Kind != helmdirect.SourceGit || backend.request.Source.Path != "valkey" || string(backend.request.Values) != "replicaCount: 1\n" {
		t.Fatalf("forwarded request=%#v", backend.request)
	}
	view := decode[struct {
		Source struct {
			Kind string `json:"kind"`
		} `json:"source"`
		ValuesYAML string `json:"valuesYaml"`
	}](t, response)
	if view.Source.Kind != "git" || view.ValuesYAML != "replicaCount: 1\n" {
		t.Fatalf("response=%#v", view)
	}
}

func TestHelmApprovalAndRenderedPreviewRoutesAreRemoved(t *testing.T) {
	fixture := newHelmHTTPFixture(t, &helmHTTPBackend{capabilities: helmdirect.Capabilities{HelmDeployments: true}})
	for _, path := range []string{"/v1/platform/helm/approvals", fixture.basePath() + "/approvals", fixture.basePath() + "/rendered-preview"} {
		response := fixture.request(http.MethodGet, path, "", nil)
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("removed route %s status=%d", path, response.StatusCode)
		}
	}
}
