package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/argo"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/environmentfoundation"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

func newPlatformGitBindingAPI(t *testing.T, resolver httpapi.GitBindingRepositoryResolver, config httpapi.PlatformGitBindingConfig) *apiFixture {
	t.Helper()
	st := memory.New()
	srv := httptest.NewServer(httpapi.New(httpapi.Options{Store: st, BootstrapToken: "one-time-secret", Version: "test",
		GitBindingRepositories: resolver, PlatformGitBinding: config, HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000)}))
	jar, _ := cookiejar.New(nil)
	fixture := &apiFixture{t: t, server: srv, client: &http.Client{Jar: jar}, store: st}
	t.Cleanup(srv.Close)
	return fixture
}

func TestPlatformArgoGitBindingHTTPIsHumanAdminOnlyAndServerDerived(t *testing.T) {
	backend := &buildHTTPBackend{}
	clusterID := id.New()
	f := newPlatformGitBindingAPI(t, backend, httpapi.PlatformGitBindingConfig{Enabled: true, ClusterID: clusterID, GitHubAppID: 123})
	admin := f.bootstrap()
	installation, err := f.store.CreateGitHubInstallation(t.Context(), admin.ID, "platform-binding-install", "platform-binding-install", "platform-binding-install", domain.CreateGitHubInstallation{
		GitHubInstallationID: 5242, AccountLogin: "example", AccountType: "Organization", RepositorySelection: "selected", RepositoryCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	providerRepositoryID := int64(59001)
	repositoryID := deterministicRepositoryCatalogID(installation.Value.ID, providerRepositoryID)
	backend.gitBinding = httpapi.GitBindingRepositoryResolution{GitHubAppID: 123, Repository: gitprojection.RepositoryIdentity{
		Provider: "github", InstallationID: 5242, RepositoryID: providerRepositoryID, Owner: "example", Name: "platform-gitops",
	}}

	unsafeBody := map[string]string{"installationId": installation.Value.ID, "repositoryId": repositoryID,
		"targetRef": "refs/heads/platform", "clusterId": id.New()}
	response := f.request(http.MethodPost, "/v1/platform/argo/git-binding", "platform-binding-unsafe", unsafeBody)
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusBadRequest || problem.Code != "InvalidJSON" || backend.gitBindCalls != 0 {
		t.Fatalf("caller cluster authority status=%d problem=%#v resolverCalls=%d", response.StatusCode, problem, backend.gitBindCalls)
	}

	input := map[string]string{"installationId": installation.Value.ID, "repositoryId": repositoryID, "targetRef": "refs/heads/platform"}
	response = f.request(http.MethodPost, "/v1/platform/argo/git-binding", "platform-binding-create", input)
	raw, _ := io.ReadAll(response.Body)
	response.Body.Close()
	var binding struct {
		ID         string `json:"id"`
		ClusterID  string `json:"clusterId"`
		TargetRef  string `json:"targetRef"`
		PathPrefix string `json:"pathPrefix"`
		State      string `json:"state"`
		Repository struct {
			Provider       string `json:"provider"`
			InstallationID int64  `json:"installationId"`
			RepositoryID   int64  `json:"repositoryId"`
			Owner          string `json:"owner"`
			Name           string `json:"name"`
		} `json:"repository"`
	}
	if err = json.Unmarshal(raw, &binding); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"clone", "remoteUrl", "credential", "secret", "token", "password", "githubAppId"} {
		if bytes.Contains(bytes.ToLower(raw), []byte(strings.ToLower(forbidden))) {
			t.Fatalf("platform binding response leaked %q: %s", forbidden, raw)
		}
	}
	if response.StatusCode != http.StatusCreated || response.Header.Get("Cache-Control") != "no-store" ||
		response.Header.Get("Location") != "/v1/platform/argo/git-binding" || binding.ClusterID != clusterID ||
		binding.PathPrefix != gitprojection.PlatformPrefix(clusterID) || binding.TargetRef != "refs/heads/platform" ||
		binding.State != "waiting-for-git" || binding.Repository.Provider != "github" ||
		binding.Repository.InstallationID != 5242 || binding.Repository.RepositoryID != providerRepositoryID ||
		binding.Repository.Owner != "example" || binding.Repository.Name != "platform-gitops" {
		t.Fatalf("unsafe or underived binding status=%d headers=%v body=%s", response.StatusCode, response.Header, raw)
	}

	// Stage two consumes only the server-returned binding ID and the same
	// operator-owned cluster identity. This is the exact handoff that could not
	// be configured before the stage-one route existed.
	stageTwo := map[string]string{
		argo.ProductionEnabledEnv:                         "true",
		argo.ProductionPlatformBindingIDEnv:               binding.ID,
		argo.ProductionClusterIDEnv:                       clusterID,
		argo.ProductionNamespaceEnv:                       "argocd",
		argo.ProductionChartRepositoryEnv:                 "oci://ghcr.io/kuberploy/charts",
		argo.ProductionChartVersionEnv:                    "1.2.3",
		argo.ProductionChartDigestEnv:                     "sha256:" + strings.Repeat("c", 64),
		argo.ProductionRendererImageEnv:                   "ghcr.io/kuberploy/worker@sha256:" + strings.Repeat("d", 64),
		argo.ProductionPollIntervalSecondsEnv:             "2",
		argo.ProductionCatalogMaxAgeSecondsEnv:            "300",
		"KUBERPLOY_GITHUB_APP_ID":                         "123",
		"KUBERPLOY_GITHUB_APP_CLIENT_ID":                  "Iv1_KuberployClient",
		environmentfoundation.RuntimeEnabledEnv:           "true",
		environmentfoundation.RuntimePlatformBindingIDEnv: binding.ID,
		environmentfoundation.RuntimePSAVersionEnv:        "v1.31",
		environmentfoundation.RuntimePollSecondsEnv:       "2",
	}
	stageTwoLookup := func(name string) (string, bool) { value, found := stageTwo[name]; return value, found }
	argoConfig, configErr := argo.ProductionRuntimeConfigFromLookup(stageTwoLookup)
	if configErr != nil || argoConfig.DesiredState.PlatformBindingID != binding.ID || argoConfig.DesiredState.ClusterID != clusterID {
		t.Fatalf("returned binding did not configure stage-two Argo: config=%+v err=%v", argoConfig, configErr)
	}
	foundationConfig, configErr := environmentfoundation.RuntimeConfigFromLookup(stageTwoLookup)
	if configErr != nil || foundationConfig.PlatformBindingID != binding.ID || foundationConfig.Profile.ClusterID != clusterID {
		t.Fatalf("returned binding did not configure stage-two foundation: config=%+v err=%v", foundationConfig, configErr)
	}

	response = f.request(http.MethodGet, "/v1/platform/argo/git-binding", "", nil)
	read := decode[map[string]any](t, response)
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" || read["id"] != binding.ID {
		t.Fatalf("binding read status=%d cache=%q body=%#v", response.StatusCode, response.Header.Get("Cache-Control"), read)
	}
	response = f.request(http.MethodPost, "/v1/platform/argo/git-binding", "platform-binding-create", input)
	if response.StatusCode != http.StatusCreated || response.Header.Get("Idempotent-Replay") != "true" {
		response.Body.Close()
		t.Fatalf("binding replay status=%d replay=%q", response.StatusCode, response.Header.Get("Idempotent-Replay"))
	}
	response.Body.Close()

	response = f.request(http.MethodPost, "/v1/projects", "platform-binding-automation-project", map[string]string{"name": "Automation boundary"})
	project := decode[domain.Project](t, response)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("automation project status=%d", response.StatusCode)
	}
	response = f.request(http.MethodPost, "/v1/projects/"+project.ID+"/service-accounts", "platform-binding-automation-account", map[string]string{"name": "Argo bot", "role": "developer"})
	account := decode[domain.ServiceAccount](t, response)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("automation account status=%d", response.StatusCode)
	}
	response = f.request(http.MethodPost, "/v1/service-accounts/"+account.ID+"/tokens", "platform-binding-automation-token", map[string]any{
		"name": "Read token", "scopes": []string{"app.read"}, "expiresAt": time.Now().UTC().Add(time.Hour).Truncate(time.Second).Format(time.RFC3339),
	})
	issued := decode[tokenIssueWire](t, response)
	if response.StatusCode != http.StatusCreated || issued.Token == "" {
		t.Fatalf("automation token status=%d", response.StatusCode)
	}
	response = bearerRequest(t, &http.Client{}, f.server.URL, http.MethodGet, "/v1/platform/argo/git-binding", issued.Token, "", nil)
	problem = decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusForbidden || problem.Code != "HumanSessionRequired" {
		t.Fatalf("automation reached platform authority status=%d problem=%#v", response.StatusCode, problem)
	}
}

func TestPlatformArgoGitBindingHTTPFailsClosedOnOperatorAppOrConfigMismatch(t *testing.T) {
	backend := &buildHTTPBackend{gitBinding: httpapi.GitBindingRepositoryResolution{GitHubAppID: 99, Repository: gitprojection.RepositoryIdentity{
		Provider: "github", InstallationID: 6242, RepositoryID: 69001, Owner: "example", Name: "platform-gitops"}}}
	f := newPlatformGitBindingAPI(t, backend, httpapi.PlatformGitBindingConfig{Enabled: true, ClusterID: id.New(), GitHubAppID: 123})
	f.bootstrap()
	response := f.request(http.MethodPost, "/v1/platform/argo/git-binding", "platform-binding-app-mismatch", map[string]string{
		"installationId": id.New(), "repositoryId": id.New(), "targetRef": "refs/heads/platform"})
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusServiceUnavailable || problem.Code != "GitHubBuildUnavailable" {
		t.Fatalf("operator App mismatch status=%d problem=%#v", response.StatusCode, problem)
	}

	disabled := newPlatformGitBindingAPI(t, backend, httpapi.PlatformGitBindingConfig{})
	disabled.bootstrap()
	response = disabled.request(http.MethodGet, "/v1/platform/argo/git-binding", "", nil)
	problem = decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusServiceUnavailable || problem.Code != "GitHubBuildUnavailable" {
		t.Fatalf("disabled workflow status=%d problem=%#v", response.StatusCode, problem)
	}
}

func TestPlatformGitBindingOperatorConfigIsCanonicalAndDefaultOff(t *testing.T) {
	if err := (httpapi.PlatformGitBindingConfig{}).Validate(); err != nil {
		t.Fatalf("zero/default-off config invalid: %v", err)
	}
	if err := (httpapi.PlatformGitBindingConfig{Enabled: true, ClusterID: id.New(), GitHubAppID: 123}).Validate(); err != nil {
		t.Fatalf("canonical enabled config invalid: %v", err)
	}
	for name, config := range map[string]httpapi.PlatformGitBindingConfig{
		"partial disabled": {ClusterID: id.New()},
		"missing cluster":  {Enabled: true, GitHubAppID: 123},
		"nil variant":      {Enabled: true, ClusterID: "01900000-0000-0000-8000-000000000001", GitHubAppID: 123},
		"bad variant":      {Enabled: true, ClusterID: "01900000-0000-7000-7000-000000000001", GitHubAppID: 123},
		"uppercase":        {Enabled: true, ClusterID: "01900000-0000-7000-8000-00000000000A", GitHubAppID: 123},
		"whitespace":       {Enabled: true, ClusterID: " 01900000-0000-7000-8000-000000000001", GitHubAppID: 123},
		"missing app":      {Enabled: true, ClusterID: id.New()},
	} {
		t.Run(name, func(t *testing.T) {
			if err := config.Validate(); err == nil {
				t.Fatalf("unsafe config accepted: %#v", config)
			}
		})
	}
}
