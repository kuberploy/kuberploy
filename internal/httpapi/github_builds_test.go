package httpapi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/builder"
	"github.com/kuberploy/kuberploy/internal/builds"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/githubapp"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

type githubSetupHTTPBackend struct {
	mu            sync.Mutex
	beginCalls    int
	continueCalls int
	completeCalls int
	linkCalls     int
	beginInput    builds.BeginSetupRequest
	continueInput builds.ContinueSetupRequest
	completeInput builds.CompleteSetupRequest
}

func (b *githubSetupHTTPBackend) Begin(_ context.Context, request builds.BeginSetupRequest) (builds.BeginSetupResult, error) {
	b.mu.Lock()
	b.beginCalls++
	b.beginInput = request
	b.mu.Unlock()
	return builds.BeginSetupResult{AuthorizationURL: "https://github.com/apps/kuberploy/installations/new?state=signed-state", State: "signed-state", ExpiresAt: time.Now().UTC().Add(5 * time.Minute)}, nil
}
func (b *githubSetupHTTPBackend) Continue(_ context.Context, request builds.ContinueSetupRequest) (builds.ContinueSetupResult, error) {
	b.mu.Lock()
	b.continueCalls++
	b.continueInput = request
	b.mu.Unlock()
	return builds.ContinueSetupResult{AuthorizationURL: "https://github.com/login/oauth/authorize?client_id=Iv1_test&redirect_uri=https%3A%2F%2Fkuberploy.example.test%2Fv1%2Fgithub%2Finstallations%2Fcallback&state=oauth-state",
		State: "oauth-state", ExpiresAt: time.Now().UTC().Add(5 * time.Minute)}, nil
}
func (b *githubSetupHTTPBackend) Complete(_ context.Context, request builds.CompleteSetupRequest) (builds.CompleteSetupResult, error) {
	b.mu.Lock()
	b.completeCalls++
	b.completeInput = request
	b.mu.Unlock()
	return builds.CompleteSetupResult{Handoff: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32)), ExpiresAt: time.Now().UTC().Add(5 * time.Minute),
		GitHubUser: githubapp.AccountIdentity{ID: 11, Login: "alice", Type: "User"},
		Installation: githubapp.Installation{ID: 4242, AppID: 123, Account: githubapp.AccountIdentity{ID: 22, Login: "example", Type: "Organization"},
			RepositorySelection: "selected", Permissions: githubapp.Permissions{"metadata": githubapp.PermissionRead, "contents": githubapp.PermissionRead}},
		Repositories: []githubapp.RepositoryIdentity{{ID: 33, OwnerID: 22, OwnerLogin: "example", Name: "api"}}}, nil
}
func (b *githubSetupHTTPBackend) Link(_ context.Context, request builds.LinkSetupRequest) (builds.LinkSetupResult, error) {
	b.mu.Lock()
	b.linkCalls++
	b.mu.Unlock()
	now := time.Now().UTC()
	installationID := "22222222-2222-4222-8222-222222222222"
	return builds.LinkSetupResult{Installation: domain.GitHubInstallation{ID: installationID, GitHubInstallationID: 4242, AccountLogin: "example", AccountType: "Organization",
		OwnerUserID: request.ActorID, Visibility: "private", RepositorySelection: "selected", RepositoryCount: 1, CreatedAt: now, UpdatedAt: now},
		Repositories: []builds.Repository{{ID: "33333333-3333-4333-8333-333333333333", InstallationID: installationID,
			Identity: githubapp.RepositoryIdentity{ID: 33, OwnerID: 22, OwnerLogin: "example", Name: "api"}, Lifecycle: builds.RepositoryActive, LastVerifiedAt: now, CreatedAt: now, UpdatedAt: now}}}, nil
}

type githubWebhookHTTPBackend struct {
	mu      sync.Mutex
	calls   int
	headers http.Header
	body    []byte
}

func (b *githubWebhookHTTPBackend) Accept(_ context.Context, headers http.Header, body io.Reader) (builds.WebhookOutcome, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return builds.WebhookOutcome{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	b.headers = headers.Clone()
	b.body = append([]byte(nil), raw...)
	return builds.WebhookOutcome{ClaimKey: strings.Repeat("a", 64), State: builds.DeliveryClaimed}, nil
}

type buildHTTPBackend struct {
	mu             sync.Mutex
	definition     builds.BuildDefinition
	attempt        builds.BuildAttempt
	repositories   []builds.Repository
	creates        int
	mutation       httpapi.BuildDefinitionMutation
	gitBinding     httpapi.GitBindingRepositoryResolution
	gitBindErr     error
	gitBindCalls   int
	profileCatalog builds.BuildSecretProfileCatalog
	buildCommit    string
	buildCalls     int
}

func (b *buildHTTPBackend) ResolveGitBindingRepository(_ context.Context, _, _ string) (httpapi.GitBindingRepositoryResolution, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gitBindCalls++
	return b.gitBinding, b.gitBindErr
}

func (b *buildHTTPBackend) SecretProfileCatalog(context.Context, string) (builds.BuildSecretProfileCatalog, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.profileCatalog, nil
}

type buildReadinessHTTPProbe struct{ err error }

func (p *buildReadinessHTTPProbe) Probe(context.Context) error { return p.err }

func (b *buildHTTPBackend) CreateDefinition(_ context.Context, mutation httpapi.BuildDefinitionMutation) (builds.BuildDefinition, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.creates++
	b.mutation = mutation
	definition := b.definition
	definition.ProjectID, definition.ServiceID, definition.SourceKind = mutation.ProjectID, mutation.ApplicationID, mutation.SourceKind
	definition.InstallationID, definition.RepositoryID = mutation.InstallationID, mutation.RepositoryID
	return definition, false, nil
}
func (b *buildHTTPBackend) Definition(context.Context, string) (builds.BuildDefinition, error) {
	return b.definition, nil
}
func (b *buildHTTPBackend) Definitions(context.Context, string) ([]builds.BuildDefinition, error) {
	return []builds.BuildDefinition{b.definition}, nil
}
func (b *buildHTTPBackend) Repositories(context.Context, string) ([]builds.Repository, error) {
	return append([]builds.Repository(nil), b.repositories...), nil
}
func (b *buildHTTPBackend) Attempt(context.Context, string) (builds.BuildAttempt, error) {
	return b.attempt, nil
}
func (b *buildHTTPBackend) Attempts(context.Context, string, int) ([]builds.BuildAttempt, error) {
	return []builds.BuildAttempt{b.attempt}, nil
}
func (b *buildHTTPBackend) Cancel(_ context.Context, _, _, _, _ string) (builds.BuildAttempt, bool, error) {
	result := b.attempt
	result.State = builds.AttemptCancelling
	return result, false, nil
}
func (b *buildHTTPBackend) Retry(_ context.Context, _, _, _, _ string) (builds.BuildAttempt, bool, error) {
	result := b.attempt
	result.ID = "77777777-7777-4777-8777-777777777777"
	result.Generation++
	result.State = builds.AttemptQueued
	return result, false, nil
}
func (b *buildHTTPBackend) Build(_ context.Context, _, _, commitSHA, _, _ string) (builds.BuildAttempt, bool, error) {
	b.mu.Lock()
	b.buildCommit = commitSHA
	b.buildCalls++
	b.mu.Unlock()
	result := b.attempt
	result.CommitSHA = commitSHA
	result.State = builds.AttemptQueued
	return result, false, nil
}

func TestGitSSHDefinitionAndManualBuildHTTP(t *testing.T) {
	backend := &buildHTTPBackend{definition: builds.BuildDefinition{ID: "55555555-5555-4555-8555-555555555555", SourceKind: builds.SourceGitSSH},
		attempt: builds.BuildAttempt{ID: "66666666-6666-4666-8666-666666666666", DefinitionID: "55555555-5555-4555-8555-555555555555", Generation: 1, MaxAttempts: 3}}
	f := newGitHubBuildHTTP(t, nil, nil, backend, ratelimit.NewMemoryLimiter(10_000))
	f.bootstrap()
	response := f.request(http.MethodPost, "/v1/projects", "git-ssh-http-project", map[string]string{"name": "Git SSH"})
	project := decode[domain.Project](t, response)
	response = f.request(http.MethodPost, "/v1/applications", "git-ssh-http-app", map[string]string{"projectId": project.ID, "name": "App"})
	application := decode[domain.Application](t, response)
	backend.definition.ProjectID, backend.definition.ServiceID = project.ID, application.ID
	backend.attempt.ProjectID, backend.attempt.ServiceID = project.ID, application.ID

	create := map[string]any{
		"sourceKind": "git_ssh", "repositoryUrl": "ssh://git@git.example.test/team/repository.git", "gitSSHKeyScope": "app", "gitSSHKeyRevision": 1,
		"hostKeyPins":      []map[string]string{{"endpoint": "git.example.test:22", "publicKey": "ssh-ed25519 AAAAFixture"}},
		"registryTargetId": "44444444-4444-4444-8444-444444444444", "triggerRef": "refs/heads/main", "contextPath": ".", "dockerfilePath": "Dockerfile",
		"platforms": []string{"linux/amd64"}, "cacheTrustLane": "protected", "cacheImports": 2,
		"profile": map[string]any{"resource": "standard", "timeoutSeconds": 900, "egress": "registry-and-source"}, "maxAttempts": 3,
	}
	response = f.request(http.MethodPost, "/v1/applications/"+application.ID+"/build-definitions", "git-ssh-definition-http", create)
	if response.StatusCode != http.StatusCreated {
		problem := decode[httpapi.Problem](t, response)
		t.Fatalf("definition status=%d problem=%#v", response.StatusCode, problem)
	}
	response.Body.Close()
	backend.mu.Lock()
	mutation := backend.mutation
	backend.mu.Unlock()
	if mutation.SourceKind != builds.SourceGitSSH || mutation.GitSSHKeyScope != "app" || mutation.GitSSHKeyRevision != 1 || len(mutation.HostKeyPins) != 1 {
		t.Fatalf("Git SSH mutation=%#v", mutation)
	}

	commit := strings.Repeat("a", 40)
	response = f.request(http.MethodPost, "/v1/build-definitions/"+backend.definition.ID+"/builds", "git-ssh-manual-build", map[string]string{"commitSha": commit})
	if response.StatusCode != http.StatusAccepted {
		problem := decode[httpapi.Problem](t, response)
		t.Fatalf("build status=%d problem=%#v", response.StatusCode, problem)
	}
	response.Body.Close()
	backend.mu.Lock()
	buildCommit, buildCalls := backend.buildCommit, backend.buildCalls
	backend.mu.Unlock()
	if buildCommit != commit || buildCalls != 1 {
		t.Fatalf("manual build commit=%q calls=%d", buildCommit, buildCalls)
	}

	response = f.request(http.MethodPost, "/v1/build-definitions/"+backend.definition.ID+"/builds", "git-ssh-manual-bad", map[string]string{"commitSha": "main"})
	problem := decode[httpapi.Problem](t, response)
	backend.mu.Lock()
	buildCalls = backend.buildCalls
	backend.mu.Unlock()
	if response.StatusCode != http.StatusUnprocessableEntity || problem.Code != "ValidationFailed" || buildCalls != 1 {
		t.Fatalf("invalid commit status=%d problem=%#v calls=%d", response.StatusCode, problem, buildCalls)
	}
}

func newGitHubBuildHTTP(t *testing.T, setup httpapi.GitHubSetupBackend, webhook httpapi.GitHubWebhookBackend, build httpapi.BuildBackend, limiter ratelimit.Limiter) *apiFixture {
	t.Helper()
	st := memory.New()
	srv := httptest.NewServer(httpapi.New(httpapi.Options{Store: st, BootstrapToken: "one-time-secret", Version: "test", GitHubSetup: setup,
		GitHubWebhook: webhook, Builds: build, HighRiskLimiter: limiter}))
	jar, _ := cookiejar.New(nil)
	fixture := &apiFixture{t: t, server: srv, client: &http.Client{Jar: jar}, store: st}
	t.Cleanup(srv.Close)
	return fixture
}

func deterministicRepositoryCatalogID(installationID string, providerRepositoryID int64) string {
	hash := sha256.New()
	for _, part := range []string{"github-repository-v1", installationID, strconv.FormatInt(providerRepositoryID, 10)} {
		_, _ = hash.Write([]byte(part))
		_, _ = hash.Write([]byte{0})
	}
	value := hash.Sum(nil)[:16]
	value[6] = (value[6] & 0x0f) | 0x80
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value)
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}

func TestEnvironmentGitBindingHTTPUsesOnlyVerifiedCatalogIDs(t *testing.T) {
	backend := &buildHTTPBackend{}
	f := newGitHubBuildHTTP(t, nil, nil, backend, ratelimit.NewMemoryLimiter(10_000))
	admin := f.bootstrap()
	response := f.request(http.MethodPost, "/v1/projects", "git-binding-project", map[string]string{"name": "Git authority", "slug": "git-authority"})
	project := decode[domain.Project](t, response)
	response = f.request(http.MethodPost, "/v1/environments", "git-binding-environment", map[string]string{"projectId": project.ID, "name": "Production", "slug": "production"})
	environment := decode[domain.Environment](t, response)
	installation, err := f.store.CreateGitHubInstallation(t.Context(), admin.ID, "git-binding-install", "git-binding-install", "git-binding-install-request", domain.CreateGitHubInstallation{
		GitHubInstallationID: 4242, AccountLogin: "example", AccountType: "Organization", RepositorySelection: "selected", RepositoryCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	providerRepositoryID := int64(9001)
	repositoryID := deterministicRepositoryCatalogID(installation.Value.ID, providerRepositoryID)
	backend.gitBinding = httpapi.GitBindingRepositoryResolution{GitHubAppID: 123, Repository: gitprojection.RepositoryIdentity{
		Provider: "github", InstallationID: 4242, RepositoryID: providerRepositoryID, Owner: "example", Name: "platform-config",
	}}

	duplicate := `{"installationId":"` + installation.Value.ID + `","repositoryId":"` + repositoryID + `","targetRef":"refs/heads/main","targetRef":"refs/heads/other"}`
	request, _ := http.NewRequest(http.MethodPost, f.server.URL+"/v1/environments/"+environment.ID+"/git-binding", strings.NewReader(duplicate))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "git-binding-duplicate-0001")
	request.Header.Set("X-CSRF-Token", f.csrf)
	response, err = f.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusBadRequest || problem.Code != "InvalidJSON" || backend.gitBindCalls != 0 {
		t.Fatalf("duplicate binding request status=%d problem=%#v resolverCalls=%d", response.StatusCode, problem, backend.gitBindCalls)
	}

	requestBody := map[string]string{"installationId": installation.Value.ID, "repositoryId": repositoryID, "targetRef": "refs/heads/main"}
	response = f.request(http.MethodPost, "/v1/environments/"+environment.ID+"/git-binding", "git-binding-create-0001", requestBody)
	raw, _ := io.ReadAll(response.Body)
	response.Body.Close()
	var binding struct {
		ID             string                           `json:"id"`
		EnvironmentID  string                           `json:"environmentId"`
		Repository     gitprojection.RepositoryIdentity `json:"repository"`
		CredentialMode string                           `json:"credentialMode"`
		PathPrefix     string                           `json:"pathPrefix"`
		TargetRef      string                           `json:"targetRef"`
	}
	if err = json.Unmarshal(raw, &binding); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"credentialSecret", "cloneUrl", "remoteUrl", "token", "password"} {
		if bytes.Contains(bytes.ToLower(raw), []byte(strings.ToLower(forbidden))) {
			t.Fatalf("safe Git binding response leaked %q: %s", forbidden, raw)
		}
	}
	if response.StatusCode != http.StatusCreated || response.Header.Get("Cache-Control") != "no-store" ||
		binding.EnvironmentID != environment.ID || binding.Repository.RepositoryID != providerRepositoryID || binding.CredentialMode != "github-app" ||
		binding.TargetRef != "refs/heads/main" || !strings.Contains(binding.PathPrefix, project.ID) || !strings.Contains(binding.PathPrefix, environment.ID) {
		t.Fatalf("binding=%#v status=%d cache=%q raw=%s", binding, response.StatusCode, response.Header.Get("Cache-Control"), raw)
	}
	response = f.request(http.MethodGet, "/v1/environments/"+environment.ID+"/git-binding", "", nil)
	read := decode[map[string]any](t, response)
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" || read["id"] != binding.ID {
		t.Fatalf("binding read=%#v status=%d cache=%q", read, response.StatusCode, response.Header.Get("Cache-Control"))
	}
	response = f.request(http.MethodPost, "/v1/environments/"+environment.ID+"/git-binding", "git-binding-create-0001", requestBody)
	if response.StatusCode != http.StatusCreated || response.Header.Get("Idempotent-Replay") != "true" {
		response.Body.Close()
		t.Fatalf("binding replay status=%d replay=%q", response.StatusCode, response.Header.Get("Idempotent-Replay"))
	}
	response.Body.Close()
}

func TestGitHubSetupHTTPIsHumanBoundNoStoreAndRejectsAmbiguity(t *testing.T) {
	setup := &githubSetupHTTPBackend{}
	f := newGitHubBuildHTTP(t, setup, nil, nil, ratelimit.NewMemoryLimiter(10_000))
	response := f.request(http.MethodPost, "/v1/github/installations/authorize", "setup-http-authorize", map[string]any{"expectedAccountId": 22, "returnKey": "application-source"})
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated setup status=%d", response.StatusCode)
	}
	response.Body.Close()
	f.bootstrap()

	request, _ := http.NewRequest(http.MethodPost, f.server.URL+"/v1/github/installations/authorize", strings.NewReader(`{"returnKey":"application-source"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "setup-http-no-csrf")
	response, err := f.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusForbidden || problem.Code != "CSRFRejected" {
		t.Fatalf("missing CSRF status=%d problem=%#v", response.StatusCode, problem)
	}

	response = f.request(http.MethodPost, "/v1/github/installations/authorize", "setup-http-authorize", map[string]any{"expectedAccountId": 22, "returnKey": "application-source"})
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" || !bytes.Contains(body, []byte(`"authorizationUrl"`)) {
		t.Fatalf("authorize status=%d cache=%q body=%s", response.StatusCode, response.Header.Get("Cache-Control"), body)
	}
	var installFlowCookie *http.Cookie
	for _, cookie := range response.Cookies() {
		if cookie.Name == "kuberploy_github_setup_session" && cookie.Path == "/v1/github/installations/setup" {
			installFlowCookie = cookie
		}
	}
	if installFlowCookie == nil || installFlowCookie.Value == "" || !installFlowCookie.HttpOnly || installFlowCookie.SameSite != http.SameSiteLaxMode ||
		installFlowCookie.Path != "/v1/github/installations/setup" || installFlowCookie.MaxAge != 15*60 || !installFlowCookie.Expires.After(time.Now().UTC()) {
		t.Fatal("authorize did not issue the bounded path-scoped browser flow cookie")
	}
	response = f.request(http.MethodPost, "/v1/github/installations/authorize", "setup-http-zero-id", map[string]any{"expectedAccountId": 0, "returnKey": "application-source"})
	problem = decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusUnprocessableEntity || problem.Code != "ValidationFailed" {
		t.Fatalf("zero expected account status=%d problem=%#v", response.StatusCode, problem)
	}
	response = f.request(http.MethodPost, "/v1/github/installations/authorize", "setup-http-existing-install", map[string]any{"existingInstallationId": 4242, "returnKey": "application-source"})
	response.Body.Close()
	if response.StatusCode != http.StatusOK || setup.beginInput.ExistingInstallationID != 4242 {
		t.Fatalf("existing installation setup status=%d input=%#v", response.StatusCode, setup.beginInput)
	}
	response = f.request(http.MethodPost, "/v1/github/installations/authorize", "setup-http-existing-zero", map[string]any{"existingInstallationId": 0, "returnKey": "application-source"})
	problem = decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusUnprocessableEntity || problem.Code != "ValidationFailed" {
		t.Fatalf("zero existing installation status=%d problem=%#v", response.StatusCode, problem)
	}

	duplicate := `{"returnKey":"application-source","returnKey":"other"}`
	request, _ = http.NewRequest(http.MethodPost, f.server.URL+"/v1/github/installations/authorize", strings.NewReader(duplicate))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "setup-http-duplicate")
	request.Header.Set("X-CSRF-Token", f.csrf)
	response, err = f.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	problem = decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusBadRequest || problem.Code != "InvalidJSON" {
		t.Fatalf("duplicate JSON status=%d problem=%#v", response.StatusCode, problem)
	}

	setupReturn := "/v1/github/installations/setup?state=" + url.QueryEscape("signed-state") + "&installation_id=4242&setup_action=install"
	serverURL, _ := url.Parse(f.server.URL)
	var primarySessionCookie *http.Cookie
	for _, cookie := range f.client.Jar.Cookies(serverURL) {
		if cookie.Name == "kuberploy_session" {
			primarySessionCookie = cookie
		}
	}
	if primarySessionCookie == nil {
		t.Fatal("test browser lost the primary session cookie")
	}
	request, _ = http.NewRequest(http.MethodGet, f.server.URL+setupReturn, nil)
	request.AddCookie(primarySessionCookie)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	problem = decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusUnauthorized || problem.Code != "Unauthenticated" {
		t.Fatalf("setup return accepted a non-flow cookie: status=%d problem=%#v", response.StatusCode, problem)
	}
	request, _ = http.NewRequest(http.MethodGet, f.server.URL+setupReturn, nil)
	request.AddCookie(installFlowCookie)
	request.AddCookie(installFlowCookie)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	problem = decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusUnauthorized || problem.Code != "Unauthenticated" {
		t.Fatalf("duplicate browser-flow cookies accepted: status=%d problem=%#v", response.StatusCode, problem)
	}
	request, _ = http.NewRequest(http.MethodGet, f.server.URL+setupReturn, nil)
	request.AddCookie(installFlowCookie)
	redirectClient := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err = redirectClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	wantOAuthURL := "https://github.com/login/oauth/authorize?client_id=Iv1_test&redirect_uri=https%3A%2F%2Fkuberploy.example.test%2Fv1%2Fgithub%2Finstallations%2Fcallback&state=oauth-state"
	setup.mu.Lock()
	continueCalls, continueInput := setup.continueCalls, setup.continueInput
	setup.mu.Unlock()
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Referrer-Policy") != "no-referrer" || response.Header.Get("Location") != wantOAuthURL ||
		len(body) != 0 || continueCalls != 1 || continueInput.InstallationID != 4242 || continueInput.State != "signed-state" || continueInput.ActorID == "" {
		t.Fatalf("setup return status=%d cache=%q location-ok=%t body-bytes=%d calls=%d binding-ok=%t", response.StatusCode,
			response.Header.Get("Cache-Control"), response.Header.Get("Location") == wantOAuthURL, len(body), continueCalls,
			continueInput.InstallationID == 4242 && continueInput.State == "signed-state" && continueInput.ActorID != "")
	}
	var oauthFlowCookie *http.Cookie
	installCookieCleared := false
	for _, cookie := range response.Cookies() {
		if cookie.Name == "kuberploy_github_setup_session" && cookie.Path == "/v1/github/installations/callback" {
			oauthFlowCookie = cookie
		}
		if cookie.Name == "kuberploy_github_setup_session" && cookie.Path == "/v1/github/installations/setup" && cookie.MaxAge < 0 {
			installCookieCleared = true
		}
	}
	if oauthFlowCookie == nil || oauthFlowCookie.Value != installFlowCookie.Value || oauthFlowCookie.MaxAge != 15*60 || !installCookieCleared {
		t.Fatal("setup return did not rotate the same bounded browser session")
	}
	response = f.request(http.MethodGet, setupReturn+"&state=duplicate", "", nil)
	problem = decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusBadRequest || problem.Code != "InvalidGitHubSetupReturn" {
		t.Fatalf("duplicate setup query status=%d problem=%#v", response.StatusCode, problem)
	}

	callbackBase := "/v1/github/installations/callback?state=" + url.QueryEscape("oauth-state") + "&code=" + url.QueryEscape("oauth-code-1234567890")
	callback := callbackBase + "&iss=" + url.QueryEscape("https://github.com/login/oauth")
	request, _ = http.NewRequest(http.MethodGet, f.server.URL+callback, nil)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	problem = decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusUnauthorized || problem.Code != "Unauthenticated" {
		t.Fatalf("OAuth callback accepted without dedicated browser flow session: status=%d problem=%#v", response.StatusCode, problem)
	}
	for name, invalid := range map[string]string{
		"foreign issuer":   callbackBase + "&iss=" + url.QueryEscape("https://example.invalid/oauth"),
		"duplicate issuer": callback + "&iss=" + url.QueryEscape("https://github.com/login/oauth"),
	} {
		request, _ = http.NewRequest(http.MethodGet, f.server.URL+invalid, nil)
		request.AddCookie(oauthFlowCookie)
		response, err = http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		problem = decode[httpapi.Problem](t, response)
		if response.StatusCode != http.StatusBadRequest || problem.Code != "InvalidGitHubCallback" {
			t.Fatalf("%s callback accepted: status=%d problem=%#v", name, response.StatusCode, problem)
		}
	}
	request, _ = http.NewRequest(http.MethodGet, f.server.URL+callback, nil)
	request.AddCookie(oauthFlowCookie)
	response, err = redirectClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Referrer-Policy") != "no-referrer" || response.Header.Get("Location") != "/github/setup/complete" || len(body) != 0 {
		t.Fatalf("callback status=%d cache=%q location=%q body-bytes=%d", response.StatusCode,
			response.Header.Get("Cache-Control"), response.Header.Get("Location"), len(body))
	}
	setup.mu.Lock()
	completeCalls, completeInput := setup.completeCalls, setup.completeInput
	setup.mu.Unlock()
	if completeCalls != 1 || completeInput.State != "oauth-state" || completeInput.Code != "oauth-code-1234567890" || completeInput.ActorID == "" {
		t.Fatalf("callback was not actor/OAuth-state bound: calls=%d binding-ok=%t", completeCalls,
			completeInput.State == "oauth-state" && completeInput.Code == "oauth-code-1234567890" && completeInput.ActorID != "")
	}
	cleared := false
	var handoffCookie *http.Cookie
	for _, cookie := range response.Cookies() {
		if cookie.Name == "kuberploy_github_setup_session" && cookie.MaxAge < 0 && cookie.Path == "/v1/github/installations/callback" {
			cleared = true
		}
		if cookie.Name == "kuberploy_github_setup_handoff" && cookie.Path == "/v1/github/installations/link" {
			handoffCookie = cookie
		}
	}
	if !cleared || handoffCookie == nil || handoffCookie.Value == "" || !handoffCookie.HttpOnly || handoffCookie.SameSite != http.SameSiteStrictMode || handoffCookie.MaxAge < 1 || handoffCookie.MaxAge > 15*60 {
		t.Fatal("successful OAuth callback did not rotate into a bounded HttpOnly link handoff")
	}
	request, _ = http.NewRequest(http.MethodGet, f.server.URL+callback+"&state=duplicate", nil)
	request.AddCookie(oauthFlowCookie)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	problem = decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusBadRequest || problem.Code != "InvalidGitHubCallback" {
		t.Fatalf("duplicate callback query status=%d problem=%#v", response.StatusCode, problem)
	}
	request, _ = http.NewRequest(http.MethodGet, f.server.URL+callback+"&installation_id=4242", nil)
	request.AddCookie(oauthFlowCookie)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	problem = decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusBadRequest || problem.Code != "InvalidGitHubCallback" {
		t.Fatalf("caller installation id reached OAuth callback: status=%d problem=%#v", response.StatusCode, problem)
	}

	f.client.Jar.SetCookies(serverURL, []*http.Cookie{handoffCookie})
	response = f.request(http.MethodPost, "/v1/github/installations/link", "setup-http-link-01", nil)
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusCreated || response.Header.Get("Cache-Control") != "no-store" || bytes.Contains(body, []byte(handoffCookie.Value)) {
		t.Fatalf("link leaked handoff: status=%d body=%s", response.StatusCode, body)
	}
	handoffCleared := false
	for _, cookie := range response.Cookies() {
		if cookie.Name == "kuberploy_github_setup_handoff" && cookie.Path == "/v1/github/installations/link" && cookie.MaxAge < 0 {
			handoffCleared = true
		}
	}
	if !handoffCleared {
		t.Fatal("successful link did not clear the one-time handoff cookie")
	}
}

func TestGitHubSetupBrowserFlowCookieIsSecureOnHTTPS(t *testing.T) {
	setup := &githubSetupHTTPBackend{}
	st := memory.New()
	srv := httptest.NewTLSServer(httpapi.New(httpapi.Options{Store: st, BootstrapToken: "one-time-secret", Version: "test",
		SecureCookie: true, GitHubSetup: setup, HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000)}))
	jar, _ := cookiejar.New(nil)
	client := srv.Client()
	client.Jar = jar
	fixture := &apiFixture{t: t, server: srv, client: client, store: st}
	t.Cleanup(srv.Close)
	fixture.bootstrap()
	response := fixture.request(http.MethodPost, "/v1/github/installations/authorize", "setup-secure-cookie", map[string]string{"returnKey": "application-source"})
	defer response.Body.Close()
	secure := false
	for _, cookie := range response.Cookies() {
		if cookie.Name == "kuberploy_github_setup_session" && cookie.Path == "/v1/github/installations/setup" {
			secure = cookie.Secure && cookie.HttpOnly && cookie.SameSite == http.SameSiteLaxMode && cookie.Path == "/v1/github/installations/setup"
		}
	}
	if response.StatusCode != http.StatusOK || !secure {
		t.Fatalf("HTTPS setup flow cookie is not Secure/HttpOnly/path-scoped: status=%d", response.StatusCode)
	}
}

func TestBuildCapabilitiesAndReadyzRequireObservedMatchingWorker(t *testing.T) {
	probe := &buildReadinessHTTPProbe{}
	setup, webhook, backend := &githubSetupHTTPBackend{}, &githubWebhookHTTPBackend{}, &buildHTTPBackend{}
	st := memory.New()
	srv := httptest.NewServer(httpapi.New(httpapi.Options{Store: st, BootstrapToken: "one-time-secret", Version: "test",
		GitHubSetup: setup, GitHubWebhook: webhook, Builds: backend, BuildReadiness: probe, HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000)}))
	jar, _ := cookiejar.New(nil)
	fixture := &apiFixture{t: t, server: srv, client: &http.Client{Jar: jar}, store: st}
	t.Cleanup(srv.Close)
	fixture.bootstrap()
	response := fixture.request(http.MethodGet, "/v1/capabilities", "", nil)
	capabilities := decode[struct {
		Features map[string]bool `json:"features"`
	}](t, response)
	if !capabilities.Features["builder"] || !capabilities.Features["builds"] || !capabilities.Features["githubAppSetup"] {
		t.Fatalf("healthy runtime not advertised: %#v", capabilities.Features)
	}
	response = fixture.request(http.MethodGet, "/readyz", "", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("ready status=%d", response.StatusCode)
	}
	response.Body.Close()

	probe.err = errors.New("stale worker")
	response = fixture.request(http.MethodGet, "/v1/capabilities", "", nil)
	capabilities = decode[struct {
		Features map[string]bool `json:"features"`
	}](t, response)
	if capabilities.Features["builder"] || capabilities.Features["builds"] || !capabilities.Features["githubAppSetup"] {
		t.Fatalf("stale runtime advertised: %#v", capabilities.Features)
	}
	response = fixture.request(http.MethodGet, "/readyz", "", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("stale optional build runtime removed API readiness: status=%d", response.StatusCode)
	}
	response.Body.Close()
}

func TestGitHubWebhookPreservesRawBodyAndBypassesTransportLimiter(t *testing.T) {
	webhook := &githubWebhookHTTPBackend{}
	f := newGitHubBuildHTTP(t, nil, webhook, nil, nil)
	raw := []byte("{\n  \"ref\": \"refs/heads/main\", \"after\": \"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\"\n}")
	request, _ := http.NewRequest(http.MethodPost, f.server.URL+"/v1/webhooks/github", bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("a", 64))
	request.Header.Set("X-GitHub-Delivery", "11111111-1111-4111-8111-111111111111")
	request.Header.Set("X-GitHub-Event", "push")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	webhook.mu.Lock()
	calls, captured := webhook.calls, append([]byte(nil), webhook.body...)
	webhook.mu.Unlock()
	if response.StatusCode != http.StatusAccepted || response.Header.Get("Cache-Control") != "no-store" || calls != 1 || !bytes.Equal(captured, raw) {
		t.Fatalf("webhook status=%d cache=%q calls=%d raw=%q", response.StatusCode, response.Header.Get("Cache-Control"), calls, captured)
	}

	request, _ = http.NewRequest(http.MethodPost, f.server.URL+"/v1/webhooks/github", bytes.NewReader(raw))
	request.Header.Set("Content-Type", "text/plain")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusUnsupportedMediaType || problem.Code != "UnsupportedMediaType" {
		t.Fatalf("webhook media type status=%d problem=%#v", response.StatusCode, problem)
	}
}

func TestBuildHTTPUsesServerScopeAndReturnsOnlySafeMetadata(t *testing.T) {
	now := time.Now().UTC()
	backend := &buildHTTPBackend{}
	f := newGitHubBuildHTTP(t, nil, nil, backend, ratelimit.NewMemoryLimiter(10_000))
	admin := f.bootstrap()
	response := f.request(http.MethodPost, "/v1/projects", "build-http-project", map[string]string{"name": "Builds"})
	project := decode[domain.Project](t, response)
	response = f.request(http.MethodPost, "/v1/applications", "build-http-application", map[string]string{"projectId": project.ID, "name": "API"})
	application := decode[domain.Application](t, response)
	backend.profileCatalog = builds.BuildSecretProfileCatalog{
		Build: []builds.BuildSecretProfile{{ID: "npmrc", Label: "Private npm registry", File: builder.FileReference{ID: "npmrc", Path: "/var/run/secrets/kuberploy/build/npmrc"}}},
		SSH:   []builds.BuildSecretProfile{{ID: "github", Label: "GitHub deploy key", File: builder.FileReference{ID: "github", Path: "/var/run/secrets/kuberploy/ssh/id_ed25519"}}},
	}
	response = f.request(http.MethodGet, "/v1/applications/"+application.ID+"/build-secret-profiles", "", nil)
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || bytes.Contains(body, []byte("/var/run")) || bytes.Contains(body, []byte("Secret")) {
		t.Fatalf("profile catalog status=%d body=%s", response.StatusCode, body)
	}
	if !bytes.Contains(body, []byte(`"id":"npmrc"`)) || !bytes.Contains(body, []byte(`"id":"github"`)) {
		t.Fatalf("profile catalog missing approved IDs: %s", body)
	}
	response = f.request(http.MethodPost, "/v1/github/installations", "build-http-install", map[string]any{"githubInstallationId": 4242, "accountLogin": "example", "accountType": "Organization", "repositorySelection": "selected", "repositoryCount": 1})
	installation := decode[domain.GitHubInstallation](t, response)
	repositoryID := "33333333-3333-4333-8333-333333333333"
	registryID := "44444444-4444-4444-8444-444444444444"
	definitionID := "55555555-5555-4555-8555-555555555555"
	attemptID := "66666666-6666-4666-8666-666666666666"
	backend.definition = builds.BuildDefinition{ID: definitionID, ProjectID: project.ID, ServiceID: application.ID, InstallationID: installation.ID, RepositoryID: repositoryID,
		TriggerRef: "refs/heads/main", DefinitionDigest: "sha256:" + strings.Repeat("d", 64), DefinitionGeneration: 1, Enabled: true, CreatedAt: now, UpdatedAt: now,
		Spec: builds.DefinitionSpec{ContextPath: ".", DockerfilePath: "Dockerfile", Platforms: []string{"linux/amd64"},
			Registry: builds.RegistryBinding{TargetID: registryID, Mode: builds.RegistryManaged, Server: "registry.example.test", RepositoryPrefix: "apps",
				PushCredentialSecret: "do-not-leak-push", CacheCredentialSecret: "do-not-leak-cache"},
			BuildArgs: []builder.BuildArg{{Name: "PUBLIC_MODE", Value: "production"}}, CacheTrustLane: "trusted", CacheImports: 2,
			Profile: builder.BuildProfile{Resource: "small", TimeoutSeconds: 600, Egress: "default"}, Execution: builds.ExecutionSettings{Namespace: "do-not-leak", BuilderAgentImage: "do-not-leak"}, MaxAttempts: 3}}
	backend.attempt = builds.BuildAttempt{ID: attemptID, DefinitionID: definitionID, ProjectID: project.ID, ServiceID: application.ID, CommitSHA: strings.Repeat("a", 40),
		GitRef: "refs/heads/main", Generation: 1, State: builds.AttemptRunning, ExecutionAttempts: 1, MaxAttempts: 3, JobNamespace: "do-not-leak", JobName: "do-not-leak",
		LogReference: "k8s://do-not-leak/pods/do-not-leak/containers/agent", CreatedAt: now, UpdatedAt: now}
	backend.repositories = []builds.Repository{{ID: repositoryID, InstallationID: installation.ID, Identity: githubapp.RepositoryIdentity{ID: 33, OwnerID: 22, OwnerLogin: "example", Name: "api"}, Lifecycle: builds.RepositoryActive, LastVerifiedAt: now, CreatedAt: now, UpdatedAt: now}}

	response = f.request(http.MethodGet, "/v1/applications/"+application.ID+"/build-definitions", "", nil)
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	for _, forbidden := range []string{"credentialSecret", "execution", "builderAgentImage", "do-not-leak"} {
		if bytes.Contains(body, []byte(forbidden)) {
			t.Fatalf("definition response leaked %q: %s", forbidden, body)
		}
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("definition list status=%d cache=%q body=%s", response.StatusCode, response.Header.Get("Cache-Control"), body)
	}
	if !bytes.Contains(body, []byte(`"secretFiles":[]`)) || !bytes.Contains(body, []byte(`"sshFiles":[]`)) {
		t.Fatalf("required empty definition arrays encoded as null or omitted: %s", body)
	}
	if !bytes.Contains(body, []byte(`"name":"PUBLIC_MODE","value":""`)) || bytes.Contains(body, []byte(`"value":"production"`)) {
		t.Fatalf("build argument value was not safely redacted in the schema-compatible response: %s", body)
	}

	create := map[string]any{"installationId": installation.ID, "repositoryId": repositoryID, "registryTargetId": registryID, "triggerRef": "refs/heads/main",
		"contextPath": ".", "dockerfilePath": "Dockerfile", "platforms": []string{"linux/amd64"}, "cacheTrustLane": "trusted", "cacheImports": 2,
		"profile": map[string]any{"resource": "small", "timeoutSeconds": 600, "egress": "default"}, "maxAttempts": 3}
	response = f.request(http.MethodPost, "/v1/applications/"+application.ID+"/build-definitions", "build-http-create-01", create)
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	backend.mu.Lock()
	mutation, creates := backend.mutation, backend.creates
	backend.mu.Unlock()
	if response.StatusCode != http.StatusCreated || mutation.ApplicationID != application.ID || mutation.ProjectID != project.ID || mutation.ActorID != admin.ID || creates != 1 || bytes.Contains(body, []byte("do-not-leak")) {
		t.Fatalf("create status=%d mutation=%#v creates=%d body=%s", response.StatusCode, mutation, creates, body)
	}

	duplicate := strings.Replace(string(mustJSONBytes(t, create)), `"resource":"small"`, `"resource":"small","resource":"large"`, 1)
	request, _ := http.NewRequest(http.MethodPost, f.server.URL+"/v1/applications/"+application.ID+"/build-definitions", strings.NewReader(duplicate))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "build-http-duplicate")
	request.Header.Set("X-CSRF-Token", f.csrf)
	response, err := f.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	problem := decode[httpapi.Problem](t, response)
	backend.mu.Lock()
	creates = backend.creates
	backend.mu.Unlock()
	if response.StatusCode != http.StatusBadRequest || problem.Code != "InvalidJSON" || creates != 1 {
		t.Fatalf("duplicate nested JSON status=%d problem=%#v creates=%d", response.StatusCode, problem, creates)
	}

	response = f.request(http.MethodGet, "/v1/builds/"+attemptID, "", nil)
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	for _, forbidden := range []string{"planRequest", "checkoutRequest", "jobNamespace", "jobName", "logReference", "do-not-leak"} {
		if bytes.Contains(body, []byte(forbidden)) {
			t.Fatalf("attempt response leaked %q: %s", forbidden, body)
		}
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("attempt status=%d body=%s", response.StatusCode, body)
	}
}

func TestBuildHTTPServiceAccountRequiresBuildScopeAndTeamSharedInstallation(t *testing.T) {
	now := time.Now().UTC()
	setup := &githubSetupHTTPBackend{}
	backend := &buildHTTPBackend{definition: builds.BuildDefinition{
		ID: "55555555-5555-4555-8555-555555555555", TriggerRef: "refs/heads/main",
		DefinitionDigest: "sha256:" + strings.Repeat("d", 64), DefinitionGeneration: 1, Enabled: true, CreatedAt: now, UpdatedAt: now,
		Spec: builds.DefinitionSpec{ContextPath: ".", DockerfilePath: "Dockerfile", Platforms: []string{"linux/amd64"},
			Registry: builds.RegistryBinding{TargetID: "44444444-4444-4444-8444-444444444444", Mode: builds.RegistryManaged,
				Server: "registry.example.test", RepositoryPrefix: "apps"}, CacheTrustLane: "trusted", CacheImports: 2,
			Profile: builder.BuildProfile{Resource: "small", TimeoutSeconds: 600, Egress: "default"}, MaxAttempts: 3}}}
	f := newGitHubBuildHTTP(t, setup, nil, backend, ratelimit.NewMemoryLimiter(10_000))
	f.bootstrap()

	response := f.request(http.MethodPost, "/v1/teams", "build-bearer-team", map[string]string{"name": "Build team"})
	team := decode[domain.Team](t, response)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("team status=%d body=%#v", response.StatusCode, team)
	}
	response = f.request(http.MethodPost, "/v1/projects", "build-bearer-project", map[string]string{"name": "Builds", "teamId": team.ID})
	project := decode[domain.Project](t, response)
	response = f.request(http.MethodPost, "/v1/applications", "build-bearer-application", map[string]string{"projectId": project.ID, "name": "API"})
	application := decode[domain.Application](t, response)
	response = f.request(http.MethodPost, "/v1/github/installations", "build-bearer-install", map[string]any{
		"githubInstallationId": 4242, "accountLogin": "example", "accountType": "Organization", "repositorySelection": "selected", "repositoryCount": 1,
	})
	installation := decode[domain.GitHubInstallation](t, response)
	response = f.request(http.MethodPatch, "/v1/github/installations/"+installation.ID+"/sharing", "build-bearer-sharing", map[string]string{"visibility": "team", "teamId": team.ID})
	if response.StatusCode != http.StatusOK {
		problem := decode[httpapi.Problem](t, response)
		t.Fatalf("share status=%d problem=%#v", response.StatusCode, problem)
	}

	response = f.request(http.MethodPost, "/v1/projects/"+project.ID+"/service-accounts", "build-bearer-account", map[string]string{"name": "Build bot", "role": "project-admin"})
	account := decode[domain.ServiceAccount](t, response)
	expiresAt := now.Add(time.Hour).Truncate(time.Second).Format(time.RFC3339)
	issue := func(key string, scopes []string) string {
		response = f.request(http.MethodPost, "/v1/service-accounts/"+account.ID+"/tokens", key,
			map[string]any{"name": key, "scopes": scopes, "expiresAt": expiresAt})
		issued := decode[tokenIssueWire](t, response)
		if response.StatusCode != http.StatusCreated || issued.Token == "" {
			t.Fatalf("token %s status=%d body=%#v", key, response.StatusCode, issued)
		}
		return issued.Token
	}
	editToken := issue("build-bearer-edit-token", []string{"app.edit"})
	buildToken := issue("build-bearer-build-token", []string{"build.create"})

	create := map[string]any{"installationId": installation.ID, "repositoryId": "33333333-3333-4333-8333-333333333333",
		"registryTargetId": "44444444-4444-4444-8444-444444444444", "triggerRef": "refs/heads/main", "contextPath": ".",
		"dockerfilePath": "Dockerfile", "platforms": []string{"linux/amd64"}, "cacheTrustLane": "trusted", "cacheImports": 2,
		"profile": map[string]any{"resource": "small", "timeoutSeconds": 600, "egress": "default"}, "maxAttempts": 3}
	bearerClient := &http.Client{}
	response = bearerRequest(t, bearerClient, f.server.URL, http.MethodPost, "/v1/applications/"+application.ID+"/build-definitions",
		editToken, "build-bearer-edit-denied", create)
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusForbidden || problem.Code != "InsufficientTokenScope" {
		t.Fatalf("app.edit escaped build scope: status=%d problem=%#v", response.StatusCode, problem)
	}

	response = bearerRequest(t, bearerClient, f.server.URL, http.MethodPost, "/v1/applications/"+application.ID+"/build-definitions",
		buildToken, "build-bearer-create-ok", create)
	created := decode[map[string]any](t, response)
	if response.StatusCode != http.StatusCreated || created["applicationId"] != application.ID {
		t.Fatalf("build token status=%d body=%#v", response.StatusCode, created)
	}

	response = bearerRequest(t, bearerClient, f.server.URL, http.MethodPost, "/v1/github/installations/authorize",
		buildToken, "build-bearer-setup-denied", map[string]string{"returnKey": "application-source"})
	problem = decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusForbidden || problem.Code != "HumanSessionRequired" {
		t.Fatalf("service account reached setup: status=%d problem=%#v", response.StatusCode, problem)
	}
}

func mustJSONBytes(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
