package httpapi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/appconfigpreview"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/helmapps"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	"github.com/kuberploy/kuberploy/internal/releases"
	"github.com/kuberploy/kuberploy/internal/store/memory"
	"github.com/kuberploy/kuberploy/migrations"
)

type apiFixture struct {
	t      *testing.T
	server *httptest.Server
	client *http.Client
	csrf   string
	store  *memory.Store
}

func newAPI(t *testing.T) *apiFixture {
	t.Helper()
	st := memory.New()
	srv := httptest.NewServer(httpapi.New(httpapi.Options{Store: st, BootstrapToken: "one-time-secret", Version: "test", AppConfigRenderedPreviews: staticAppConfigRenderer{}, HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000)}))
	jar, _ := cookiejar.New(nil)
	f := &apiFixture{t: t, server: srv, client: &http.Client{Jar: jar}, store: st}
	t.Cleanup(srv.Close)
	return f
}

type staticAppConfigRenderer struct{ identityDigest string }

func (staticAppConfigRenderer) Identity() (appconfigpreview.Identity, string, error) {
	identity := appconfigpreview.Identity{Contract: appconfigpreview.Contract, ChartName: "kuberploy-runtime", ChartVersion: "1.2.3",
		ChartDigest: "sha256:" + strings.Repeat("a", 64), RendererImage: appconfigpreview.RendererImage,
		RendererVersion: appconfigpreview.RendererVersion, PolicyVersion: helmapps.PolicyVersion}
	digest, err := identity.Digest()
	return identity, digest, err
}

func (renderer staticAppConfigRenderer) Render(_ context.Context, request appconfigpreview.Request) (appconfigpreview.Result, error) {
	identity, digest, err := renderer.Identity()
	if renderer.identityDigest != "" {
		digest = renderer.identityDigest
	}
	return appconfigpreview.Result{RenderedDiff: "--- a/rendered-manifests.yaml\n+++ b/rendered-manifests.yaml\n@@ bounded full-document diff @@\n-replicas: 1\n+replicas: 2\n", Identity: identity, IdentityDigest: digest}, err
}

type staticReleaseService struct {
	snapshot releases.Snapshot
	err      error
}

func (s staticReleaseService) Latest(context.Context) (releases.Snapshot, error) {
	return s.snapshot, s.err
}
func newUpgradeAPI(t *testing.T, snapshot releases.Snapshot) *apiFixture {
	t.Helper()
	st := memory.New()
	srv := httptest.NewServer(httpapi.New(httpapi.Options{Store: st, BootstrapToken: "one-time-secret", Version: "1.0.0", Releases: staticReleaseService{snapshot: snapshot}, HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000)}))
	jar, _ := cookiejar.New(nil)
	f := &apiFixture{t: t, server: srv, client: &http.Client{Jar: jar}, store: st}
	t.Cleanup(srv.Close)
	return f
}
func (f *apiFixture) request(method, path, key string, body any) *http.Response {
	f.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			f.t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, f.server.URL+path, r)
	if err != nil {
		f.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if f.csrf != "" && method != "GET" {
		req.Header.Set("X-CSRF-Token", f.csrf)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	return resp
}
func decode[T any](t *testing.T, r *http.Response) T {
	t.Helper()
	defer r.Body.Close()
	var v T
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}
func (f *apiFixture) bootstrap() domain.User {
	r := f.request("POST", "/v1/auth/bootstrap", "", map[string]any{"token": "one-time-secret", "email": "admin@example.com", "displayName": "Local Admin", "password": "correct horse battery staple"})
	if r.StatusCode != 201 {
		f.t.Fatalf("bootstrap status %d", r.StatusCode)
	}
	f.csrf = r.Header.Get("X-CSRF-Token")
	if f.csrf == "" {
		f.t.Fatal("missing CSRF token")
	}
	var sessionOK bool
	for _, c := range r.Cookies() {
		if c.Name == "kuberploy_session" {
			sessionOK = c.HttpOnly && c.SameSite == http.SameSiteStrictMode
		}
	}
	if !sessionOK {
		f.t.Fatal("session cookie is not HttpOnly SameSite=Strict")
	}
	return decode[domain.User](f.t, r)
}

func TestBootstrapIsOneTimeAndSessionIsServerSide(t *testing.T) {
	f := newAPI(t)
	u := f.bootstrap()
	if u.Role != "platform-admin" || u.Issuer != "kuberploy:bootstrap" || u.Subject == "" || u.GrantRevision != 1 {
		t.Fatalf("unexpected identity: %#v", u)
	}
	r := f.request("GET", "/v1/me", "", nil)
	got := decode[struct {
		domain.User
		Authentication struct {
			Kind string `json:"kind"`
		} `json:"authentication"`
	}](t, r)
	if r.StatusCode != 200 || r.Header.Get("Cache-Control") != "private, no-store" || got.ID != u.ID || got.Authentication.Kind != "session" {
		t.Fatalf("me=%#v status=%d", got, r.StatusCode)
	}
	r = f.request("POST", "/v1/auth/bootstrap", "", map[string]string{"token": "one-time-secret", "email": "second@example.com", "displayName": "Second", "password": "another correct horse battery staple"})
	p := decode[httpapi.Problem](t, r)
	if r.StatusCode != 409 || p.Code != "BootstrapConsumed" || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/problem+json") {
		t.Fatalf("unexpected second bootstrap: %d %#v", r.StatusCode, p)
	}
}

func TestLocalLoginIsEnumerationResistantRotatesAndRestoresExpiredSession(t *testing.T) {
	st := memory.New()
	srv := httptest.NewServer(httpapi.New(httpapi.Options{Store: st, BootstrapToken: "one-time-secret", Version: "test", SessionTTL: 20 * time.Millisecond, HighRiskLimiter: ratelimit.NewMemoryLimiter(100)}))
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	f := &apiFixture{t: t, server: srv, client: &http.Client{Jar: jar}, store: st}
	f.bootstrap()

	legacyLogin := f.request("POST", "/v1/auth/login", "", map[string]string{"login": "admin@example.com", "password": "correct horse battery staple"})
	legacyProblem := decode[httpapi.Problem](t, legacyLogin)
	if legacyLogin.StatusCode != http.StatusBadRequest || legacyProblem.Code != "InvalidJSON" {
		t.Fatalf("legacy login field was accepted: status=%d problem=%#v", legacyLogin.StatusCode, legacyProblem)
	}

	wrongExisting := f.request("POST", "/v1/auth/login", "", map[string]string{"email": "admin@example.com", "password": "incorrect password value"})
	existingProblem := decode[httpapi.Problem](t, wrongExisting)
	displayNameLogin := f.request("POST", "/v1/auth/login", "", map[string]string{"email": "Local Admin", "password": "correct horse battery staple"})
	displayNameProblem := decode[httpapi.Problem](t, displayNameLogin)
	wrongMissing := f.request("POST", "/v1/auth/login", "", map[string]string{"email": "missing@example.com", "password": "incorrect password value"})
	missingProblem := decode[httpapi.Problem](t, wrongMissing)
	if wrongExisting.StatusCode != http.StatusUnauthorized || displayNameLogin.StatusCode != http.StatusUnauthorized || wrongMissing.StatusCode != http.StatusUnauthorized || existingProblem.Code != displayNameProblem.Code || existingProblem.Code != missingProblem.Code || existingProblem.Title != displayNameProblem.Title || existingProblem.Title != missingProblem.Title || existingProblem.Detail != displayNameProblem.Detail || existingProblem.Detail != missingProblem.Detail {
		t.Fatalf("credential enumeration or display-name login leak: existing=%d %#v display-name=%d %#v missing=%d %#v", wrongExisting.StatusCode, existingProblem, displayNameLogin.StatusCode, displayNameProblem, wrongMissing.StatusCode, missingProblem)
	}

	time.Sleep(30 * time.Millisecond)
	expired := f.request("GET", "/v1/me", "", nil)
	if expired.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired session status=%d", expired.StatusCode)
	}
	expired.Body.Close()
	loggedIn := f.request("POST", "/v1/auth/login", "", map[string]string{"email": "ADMIN@example.com", "password": "correct horse battery staple"})
	user := decode[domain.User](t, loggedIn)
	if loggedIn.StatusCode != http.StatusOK || loggedIn.Header.Get("Cache-Control") != "no-store" || loggedIn.Header.Get("X-CSRF-Token") == "" || user.DisplayName != "Local Admin" || user.Email != "admin@example.com" {
		t.Fatalf("login status=%d user=%#v", loggedIn.StatusCode, user)
	}
	restored := f.request("GET", "/v1/me", "", nil)
	if restored.StatusCode != http.StatusOK {
		t.Fatalf("restored session status=%d", restored.StatusCode)
	}
	restored.Body.Close()
}

func TestCapabilitiesAndMonitoringAreAuthenticatedAndTruthful(t *testing.T) {
	f := newAPI(t)
	r := f.request("GET", "/v1/capabilities", "", nil)
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated capabilities status=%d", r.StatusCode)
	}
	r.Body.Close()
	f.bootstrap()
	r = f.request("GET", "/v1/capabilities", "", nil)
	capabilities := decode[struct {
		Features     map[string]bool           `json:"features"`
		Actions      []string                  `json:"actions"`
		Capabilities []domain.AccessCapability `json:"capabilities"`
	}](t, r)
	if r.StatusCode != http.StatusOK || len(capabilities.Actions) == 0 {
		t.Fatalf("capabilities are not truthful: %#v", capabilities)
	}
	for _, disabled := range []string{"gitops", "git", "argoCD", "argo", "traefik", "edge", "monitoring", "metrics", "logs", "builder", "builds", "buildLogs", "registry", "managedRegistry", "helmDeployments", "rollbacks", "deploymentRollbacks", "imageTagResolution", "variableSets", "certManager", "customCertificates", "sslip", "externalDNS", "traefikMiddlewares", "secretBindings"} {
		if capabilities.Features[disabled] {
			t.Fatalf("unwired runtime feature %q was advertised", disabled)
		}
	}
	if !capabilities.Features["serviceAccounts"] {
		t.Fatal("implemented service-account management and bearer authentication were not advertised")
	}
	foundBootstrapPlatformGrant := false
	for _, capability := range capabilities.Capabilities {
		if capability.Role == domain.RolePlatformAdmin && capability.ScopeType == domain.ScopePlatform && capability.ScopeID == "platform" && capability.Source == "bootstrap" {
			foundBootstrapPlatformGrant = true
			break
		}
	}
	if !foundBootstrapPlatformGrant {
		t.Fatalf("bootstrap administrator capability is not an explicit scoped grant: %#v", capabilities.Capabilities)
	}
	r = f.request("GET", "/v1/monitoring/status", "", nil)
	monitoring := decode[struct {
		Mode      string `json:"mode"`
		Status    string `json:"status"`
		Available bool   `json:"available"`
	}](t, r)
	if r.StatusCode != http.StatusOK || monitoring.Mode != "disabled" || monitoring.Status != "disabled" || monitoring.Available {
		t.Fatalf("monitoring status is not explicit: %#v", monitoring)
	}
}

func TestInvitationTeamAndGitHubAccessContract(t *testing.T) {
	f := newAPI(t)
	r := f.request("GET", "/v1/meta", "", nil)
	meta := decode[map[string]any](t, r)
	if meta["bootstrapRequired"] != true {
		t.Fatalf("bootstrap state before bootstrap=%#v", meta)
	}
	admin := f.bootstrap()
	r = f.request("GET", "/v1/meta", "", nil)
	meta = decode[map[string]any](t, r)
	if meta["bootstrapRequired"] != false {
		t.Fatalf("bootstrap state after bootstrap=%#v", meta)
	}
	r = f.request("POST", "/v1/users/invitations", "ignored-by-token-endpoint", map[string]string{"email": "developer@example.com"})
	invitation := decode[domain.UserInvitation](t, r)
	if r.StatusCode != http.StatusCreated || invitation.ID == "" || invitation.Email != "developer@example.com" || invitation.Token == "" || !invitation.ExpiresAt.After(time.Now()) || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("invitation=%#v status=%d", invitation, r.StatusCode)
	}
	developerJar, _ := cookiejar.New(nil)
	developerClient := &http.Client{Jar: developerJar}
	body, _ := json.Marshal(map[string]string{"token": invitation.Token, "displayName": "Invited Developer", "password": "developer correct horse battery staple"})
	req, err := http.NewRequest(http.MethodPost, f.server.URL+"/v1/auth/invitations/accept", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	r, err = developerClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	developerCSRF := r.Header.Get("X-CSRF-Token")
	developer := decode[domain.User](t, r)
	if r.StatusCode != http.StatusCreated || developer.Role != "developer" || developer.ID == admin.ID || developerCSRF == "" {
		t.Fatalf("accepted user=%#v status=%d", developer, r.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodGet, f.server.URL+"/v1/me", nil)
	r, err = developerClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if me := decode[domain.User](t, r); r.StatusCode != http.StatusOK || me.ID != developer.ID {
		t.Fatalf("developer session user=%#v status=%d", me, r.StatusCode)
	}
	setupBody, _ := json.Marshal(map[string]string{"returnKey": "application-source"})
	setupRequest, _ := http.NewRequest(http.MethodPost, f.server.URL+"/v1/github/installations/authorize", bytes.NewReader(setupBody))
	setupRequest.Header.Set("Content-Type", "application/json")
	setupRequest.Header.Set("Idempotency-Key", "developer-github-setup-denied")
	setupRequest.Header.Set("X-CSRF-Token", developerCSRF)
	r, err = developerClient.Do(setupRequest)
	if err != nil {
		t.Fatal(err)
	}
	if problem := decode[httpapi.Problem](t, r); r.StatusCode != http.StatusForbidden || problem.Code != "Forbidden" {
		t.Fatalf("developer GitHub setup status=%d problem=%#v", r.StatusCode, problem)
	}
	// A token is strictly single use, even from a new client.
	replayReq, _ := http.NewRequest(http.MethodPost, f.server.URL+"/v1/auth/invitations/accept", bytes.NewReader(body))
	replayReq.Header.Set("Content-Type", "application/json")
	replay, err := (&http.Client{}).Do(replayReq)
	if err != nil {
		t.Fatal(err)
	}
	if replay.StatusCode != http.StatusUnauthorized {
		t.Fatalf("invitation replay status=%d", replay.StatusCode)
	}
	replay.Body.Close()
	// The safe directory omits issuer, subject, session, and invitation data.
	r = f.request("GET", "/v1/users", "", nil)
	usersBody, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != http.StatusOK || bytes.Contains(usersBody, []byte(`"issuer"`)) || bytes.Contains(usersBody, []byte(`"subject"`)) || bytes.Contains(usersBody, []byte(invitation.Token)) {
		t.Fatalf("unsafe users response status=%d body=%s", r.StatusCode, usersBody)
	}
	r = f.request("POST", "/v1/teams", "team-1", map[string]string{"name": "Product", "slug": "product"})
	team := decode[domain.Team](t, r)
	if r.StatusCode != http.StatusCreated || team.ID == "" {
		t.Fatalf("team=%#v status=%d", team, r.StatusCode)
	}
	unauthorizedRemove, _ := http.NewRequest(http.MethodDelete, f.server.URL+"/v1/teams/"+team.ID+"/members/"+admin.ID, nil)
	unauthorizedRemove.Header.Set("X-CSRF-Token", developerCSRF)
	r, err = developerClient.Do(unauthorizedRemove)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("ordinary user removal status=%d", r.StatusCode)
	}
	r.Body.Close()
	r = f.request("DELETE", "/v1/teams/"+team.ID+"/members/"+admin.ID, "", nil)
	p := decode[httpapi.Problem](t, r)
	if r.StatusCode != http.StatusConflict || p.Code != "Conflict" {
		t.Fatalf("sole-owner removal status=%d problem=%#v", r.StatusCode, p)
	}
	r = f.request("POST", "/v1/teams/"+team.ID+"/members", "member-1", map[string]string{"userId": developer.ID, "role": "member"})
	member := decode[map[string]any](t, r)
	if r.StatusCode != http.StatusCreated || member["userId"] != developer.ID {
		t.Fatalf("member=%#v status=%d", member, r.StatusCode)
	}
	// The membership grant revision invalidates the previously issued session.
	req, _ = http.NewRequest(http.MethodGet, f.server.URL+"/v1/me", nil)
	r, err = developerClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("stale developer session status=%d", r.StatusCode)
	}
	r.Body.Close()
	r = f.request("DELETE", "/v1/teams/"+team.ID+"/members/"+developer.ID, "", nil)
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("member removal status=%d", r.StatusCode)
	}
	r.Body.Close()
	r = f.request("DELETE", "/v1/teams/"+team.ID+"/members/"+developer.ID, "", nil)
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("repeated member removal status=%d", r.StatusCode)
	}
	r.Body.Close()
	r = f.request("POST", "/v1/github/installations", "installation-1", map[string]any{"githubInstallationId": 42, "accountLogin": "kuberploy", "accountType": "Organization", "repositorySelection": "selected", "repositoryCount": 3})
	installation := decode[domain.GitHubInstallation](t, r)
	if r.StatusCode != http.StatusCreated || installation.Visibility != "private" || installation.OwnerUserID != admin.ID {
		t.Fatalf("installation=%#v status=%d", installation, r.StatusCode)
	}
	r = f.request("PATCH", "/v1/github/installations/"+installation.ID+"/sharing", "", map[string]string{"visibility": "team", "teamId": team.ID})
	p = decode[httpapi.Problem](t, r)
	if r.StatusCode != http.StatusBadRequest || p.Code != "IdempotencyKeyRequired" {
		t.Fatalf("sharing missing key status=%d problem=%#v", r.StatusCode, p)
	}
	r = f.request("PATCH", "/v1/github/installations/"+installation.ID+"/sharing", "installation-sharing", map[string]string{"visibility": "team", "teamId": team.ID})
	installation = decode[domain.GitHubInstallation](t, r)
	if r.StatusCode != http.StatusOK || installation.Visibility != "team" || installation.TeamID != team.ID {
		t.Fatalf("shared installation=%#v status=%d", installation, r.StatusCode)
	}
	r = f.request("PATCH", "/v1/github/installations/"+installation.ID+"/sharing", "installation-sharing", map[string]string{"visibility": "team", "teamId": team.ID})
	replayedInstallation := decode[domain.GitHubInstallation](t, r)
	if r.StatusCode != http.StatusOK || r.Header.Get("Idempotent-Replay") != "true" || replayedInstallation.ID != installation.ID {
		t.Fatalf("sharing replay=%#v status=%d replay=%q", replayedInstallation, r.StatusCode, r.Header.Get("Idempotent-Replay"))
	}
	r = f.request("PATCH", "/v1/github/installations/"+installation.ID+"/sharing", "installation-sharing", map[string]string{"visibility": "private"})
	p = decode[httpapi.Problem](t, r)
	if r.StatusCode != http.StatusConflict || p.Code != "IdempotencyConflict" {
		t.Fatalf("sharing fingerprint conflict status=%d problem=%#v", r.StatusCode, p)
	}
	r = f.request("POST", "/v1/projects", "team-project", map[string]string{"name": "Team app", "slug": "team-app", "teamId": team.ID})
	project := decode[domain.Project](t, r)
	if r.StatusCode != http.StatusCreated || project.TeamID != team.ID {
		t.Fatalf("team project=%#v status=%d", project, r.StatusCode)
	}
}

func TestInvitationAcceptanceSwitchesAndRevokesExistingSession(t *testing.T) {
	f := newAPI(t)
	f.bootstrap()
	r := f.request("POST", "/v1/users/invitations", "switch-invite", map[string]string{"email": "switch@example.com"})
	invitation := decode[domain.UserInvitation](t, r)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("invitation status=%d", r.StatusCode)
	}

	oldJar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	serverURL, err := url.Parse(f.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	oldJar.SetCookies(serverURL, f.client.Jar.Cookies(serverURL))
	oldClient := &http.Client{Jar: oldJar}

	r = f.request("POST", "/v1/auth/invitations/accept", "", map[string]string{
		"token":       invitation.Token,
		"displayName": "Switched user",
		"password":    "switched user password 123",
	})
	accepted := decode[domain.User](t, r)
	if r.StatusCode != http.StatusCreated || accepted.Email != invitation.Email {
		t.Fatalf("accepted user=%#v status=%d", accepted, r.StatusCode)
	}
	current := f.request("GET", "/v1/me", "", nil)
	currentUser := decode[domain.User](t, current)
	if current.StatusCode != http.StatusOK || currentUser.ID != accepted.ID {
		t.Fatalf("new invitation session was not active: status=%d", current.StatusCode)
	}
	oldRequest, err := http.NewRequest(http.MethodGet, f.server.URL+"/v1/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	oldResponse, err := oldClient.Do(oldRequest)
	if err != nil {
		t.Fatal(err)
	}
	if oldResponse.StatusCode != http.StatusUnauthorized {
		oldResponse.Body.Close()
		t.Fatalf("previous session remained valid: status=%d", oldResponse.StatusCode)
	}
	oldResponse.Body.Close()
}

func TestImageDeploymentWalkingSliceAndIdempotency(t *testing.T) {
	f := newAPI(t)
	f.bootstrap()
	r := f.request("POST", "/v1/projects", "project-1", map[string]string{"name": "Demo"})
	project := decode[domain.Project](t, r)
	if r.StatusCode != 201 {
		t.Fatalf("project: %d", r.StatusCode)
	}
	r = f.request("POST", "/v1/projects", "project-1", map[string]string{"name": "Demo"})
	replay := decode[domain.Project](t, r)
	if replay.ID != project.ID || r.Header.Get("Idempotent-Replay") != "true" {
		t.Fatal("project replay did not return original resource")
	}
	r = f.request("POST", "/v1/projects", "project-1", map[string]string{"name": "Different"})
	p := decode[httpapi.Problem](t, r)
	if r.StatusCode != 409 || p.Code != "IdempotencyConflict" {
		t.Fatalf("expected idempotency conflict: %d %#v", r.StatusCode, p)
	}
	r = f.request("POST", "/v1/environments", "env-1", map[string]string{"projectId": project.ID, "name": "Development"})
	environment := decode[domain.Environment](t, r)
	if r.StatusCode != 201 {
		t.Fatalf("environment: %d", r.StatusCode)
	}
	r = f.request("POST", "/v1/applications", "app-1", map[string]string{"projectId": project.ID, "name": "Hello"})
	application := decode[domain.Application](t, r)
	if r.StatusCode != 201 {
		t.Fatalf("application: %d", r.StatusCode)
	}
	image := "registry.example.test/hello@sha256:" + strings.Repeat("a", 64)
	request := map[string]any{"environmentId": environment.ID, "applicationId": application.ID, "image": image, "port": 8080, "environment": map[string]string{"LOG_LEVEL": "info"}, "route": map[string]string{"hostname": "hello.example.com"}}
	r = f.request("POST", "/v1/deployments", "deployment-1", request)
	op := decode[domain.Operation](t, r)
	if r.StatusCode != 202 || op.Status != "queued" || op.TargetID == "" {
		t.Fatalf("operation: %d %#v", r.StatusCode, op)
	}
	if f.store.OutboxCount() != 1 {
		t.Fatalf("outbox count=%d", f.store.OutboxCount())
	}
	if f.store.AuditCount() != 4 {
		t.Fatalf("audit count=%d", f.store.AuditCount())
	}
	r = f.request("GET", "/v1/deployments/"+op.TargetID+"/status", "", nil)
	status := decode[domain.DeploymentStatus](t, r)
	if status.OperationID != op.ID || status.OperationStatus != "queued" || status.ArgoSyncStatus != "unknown" || status.RolloutHealth != "unknown" || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("status %#v", status)
	}
	r = f.request("POST", "/v1/deployments", "deployment-1", request)
	again := decode[domain.Operation](t, r)
	if again.ID != op.ID || r.Header.Get("Idempotent-Replay") != "true" || f.store.OutboxCount() != 1 {
		t.Fatal("deployment replay duplicated durable work")
	}
	request["image"] = "registry.example.test/hello@sha256:" + strings.Repeat("b", 64)
	r = f.request("POST", "/v1/deployments", "deployment-2", request)
	second := decode[domain.Operation](t, r)
	if r.StatusCode != 202 || second.ID == op.ID || second.TargetID != op.TargetID || second.Generation != 2 {
		t.Fatalf("second release: %d %#v", r.StatusCode, second)
	}
	r = f.request("GET", "/v1/operations/"+op.ID, "", nil)
	superseded := decode[domain.Operation](t, r)
	if superseded.Status != "superseded" {
		t.Fatalf("old queued release not superseded: %#v", superseded)
	}
	r = f.request("POST", "/v1/deployments", "deployment-2", request)
	secondReplay := decode[domain.Operation](t, r)
	if secondReplay.ID != second.ID || r.Header.Get("Idempotent-Replay") != "true" || f.store.OutboxCount() != 2 {
		t.Fatal("second release retry duplicated work")
	}
	r = f.request("GET", "/v1/deployments/"+op.TargetID, "", nil)
	stable := decode[domain.Deployment](t, r)
	if stable.ID != op.TargetID || stable.Generation != 2 || stable.Image != request["image"] {
		t.Fatalf("stable deployment was replaced or stale: %#v", stable)
	}
}

func TestDeploymentStatusSeparatesExactArgoRolloutFromGitSuccess(t *testing.T) {
	f := newAPI(t)
	admin := f.bootstrap()
	project, err := f.store.CreateProject(t.Context(), admin.ID, "status-project", "status-project", domain.CreateProject{Name: "Status", Slug: "status"})
	if err != nil {
		t.Fatal(err)
	}
	environment, err := f.store.CreateEnvironment(t.Context(), admin.ID, "status-environment", "status-environment", domain.CreateEnvironment{ProjectID: project.Value.ID, Name: "Production", Slug: "production"})
	if err != nil {
		t.Fatal(err)
	}
	application, err := f.store.CreateApplication(t.Context(), admin.ID, "status-application", "status-application", domain.CreateApplication{ProjectID: project.Value.ID, Name: "API", Slug: "api"})
	if err != nil {
		t.Fatal(err)
	}
	created, operation, err := f.store.CreateDeployment(t.Context(), admin.ID, "status-deployment", "status-deployment", "request", domain.CreateDeployment{EnvironmentID: environment.Value.ID, ApplicationID: application.Value.ID, Image: "registry.test/api@sha256:" + strings.Repeat("7", 64), Runtime: domain.DefaultWorkloadRuntime(8080, nil)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.LeasePendingOperations(t.Context(), "status-worker", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, started, startErr := f.store.StartOperation(t.Context(), operation.ID, operation.Generation, "status-worker", time.Minute); startErr != nil || !started {
		t.Fatalf("start=%v err=%v", started, startErr)
	}
	revision := strings.Repeat("6", 40)
	if err = f.store.CompleteGitOperation(t.Context(), operation.ID, operation.Generation, "status-worker", domain.GitPublicationResult{Mode: "direct", Revision: revision}); err != nil {
		t.Fatal(err)
	}
	r := f.request(http.MethodGet, "/v1/deployments/"+created.Value.ID+"/status", "", nil)
	status := decode[domain.DeploymentStatus](t, r)
	if r.StatusCode != http.StatusOK || status.OperationStatus != "succeeded" || status.ArgoSyncStatus != "unknown" || status.RolloutHealth != "unknown" {
		t.Fatalf("Git success implied rollout status=%d %#v", r.StatusCode, status)
	}
	f.store.PutArgoRolloutObservation(t.Context(), domain.ArgoRolloutObservation{DeploymentID: created.Value.ID, ApplicationID: application.Value.ID, ProjectID: project.Value.ID,
		EnvironmentID: environment.Value.ID, DestinationNamespace: environment.Value.Namespace, DesiredRevision: revision, ObservedRevision: revision,
		SyncStatus: "synced", HealthStatus: "healthy", ObservedAt: time.Now().UTC()})
	r = f.request(http.MethodGet, "/v1/deployments/"+created.Value.ID+"/status", "", nil)
	status = decode[domain.DeploymentStatus](t, r)
	if r.StatusCode != http.StatusOK || status.ArgoSyncStatus != "synced" || status.RolloutHealth != "healthy" || status.ArgoObservedRevision != revision || status.ArgoObservedAt == nil {
		t.Fatalf("exact rollout status=%d %#v", r.StatusCode, status)
	}
	f.store.PutArgoRolloutObservation(t.Context(), domain.ArgoRolloutObservation{DeploymentID: created.Value.ID, ApplicationID: "11111111-1111-4111-8111-111111111111", ProjectID: project.Value.ID,
		EnvironmentID: environment.Value.ID, DestinationNamespace: environment.Value.Namespace, DesiredRevision: revision, ObservedRevision: revision,
		SyncStatus: "synced", HealthStatus: "healthy", ObservedAt: time.Now().UTC()})
	r = f.request(http.MethodGet, "/v1/deployments/"+created.Value.ID+"/status", "", nil)
	status = decode[domain.DeploymentStatus](t, r)
	if status.ArgoSyncStatus != "unknown" || status.RolloutHealth != "unknown" || status.ArgoObservedAt != nil {
		t.Fatalf("substituted observation leaked=%#v", status)
	}
}

func upgradeSnapshot() releases.Snapshot {
	artifactDigest := "sha256:" + strings.Repeat("c", 64)
	version := "1.1.0"
	manifest := domain.ReleaseManifest{
		Release: domain.ManifestRelease{Tag: "v" + version, Version: version, NotesURL: "https://github.com/kuberploy/kuberploy/releases/tag/v" + version, Summary: "Upgrade notes"},
		Source:  domain.ManifestSource{Repository: "kuberploy/kuberploy", Commit: strings.Repeat("d", 40)},
		Compatibility: domain.ReleaseCompatibility{
			SupportedUpgradeFrom: ">=1.0.0 <1.1.0",
			Kubernetes:           domain.KubernetesCompatibility{Constraint: ">=1.34.0-0 <1.37.0-0", TestedMinors: []string{"1.34", "1.35", "1.36"}},
			Database:             domain.DatabaseCompatibility{CurrentSchema: migrations.CurrentSchema, MinimumUpgradeableSchema: "001_initial"},
		},
		Artifacts: domain.ManifestArtifacts{
			Chart:           domain.ManifestChart{Name: "kuberploy", Version: version, OCIReference: "ghcr.io/kuberploy/charts/kuberploy:" + version, OCIDigest: artifactDigest},
			ComponentCharts: []domain.ManifestChart{{Name: "kuberploy-installer", Version: version, OCIReference: "ghcr.io/kuberploy/charts/kuberploy-installer:" + version, OCIDigest: artifactDigest}},
		},
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(manifestBytes)
	manifestDigest := "sha256:" + hex.EncodeToString(sum[:])
	return releases.Snapshot{Release: domain.ReleaseInfo{Tag: "v1.1.0", Version: "1.1.0", ManifestDigest: manifestDigest, Manifest: manifest, ManifestBytes: manifestBytes, PublishedAt: time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)}, LastCheckedAt: time.Date(2026, 8, 6, 0, 1, 0, 0, time.UTC)}
}

func TestReleaseCheckIsReadOnlyForInstallerManagedPlatform(t *testing.T) {
	snapshot := upgradeSnapshot()
	f := newUpgradeAPI(t, snapshot)
	f.bootstrap()
	r := f.request("GET", "/v1/platform/releases/latest", "", nil)
	if r.StatusCode != 200 {
		t.Fatalf("latest status=%d", r.StatusCode)
	}
	etag := r.Header.Get("ETag")
	var latest map[string]any
	latest = decode[map[string]any](t, r)
	if latest["currentVersion"] != "1.0.0" || etag == "" {
		t.Fatalf("latest=%#v etag=%q", latest, etag)
	}
	releaseView, ok := latest["release"].(map[string]any)
	if !ok {
		t.Fatalf("release=%#v", latest["release"])
	}
	chart, ok := releaseView["chart"].(map[string]any)
	if !ok || chart["name"] != "kuberploy-installer" || chart["ociReference"] != "ghcr.io/kuberploy/charts/kuberploy-installer:1.1.0" {
		t.Fatalf("operator chart=%#v", chart)
	}
	req, err := http.NewRequest("GET", f.server.URL+"/v1/platform/releases/latest", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("If-None-Match", etag)
	r, err = f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 304 {
		t.Fatalf("conditional status=%d", r.StatusCode)
	}
	body := map[string]string{"targetVersion": "1.1.0", "manifestDigest": snapshot.Release.ManifestDigest}
	r = f.request("POST", "/v1/platform/upgrades", "upgrade-1", body)
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("removed platform mutation route returned %d", r.StatusCode)
	}
	if f.store.OutboxCount() != 0 || f.store.AuditCount() != 0 {
		t.Fatalf("rejected child upgrade mutated durable state: outbox=%d audit=%d", f.store.OutboxCount(), f.store.AuditCount())
	}
}

func TestReleaseCheckFailsClosedWithoutInstallerChart(t *testing.T) {
	snapshot := upgradeSnapshot()
	snapshot.Release.Manifest.Artifacts.ComponentCharts = nil
	f := newUpgradeAPI(t, snapshot)
	f.bootstrap()
	r := f.request(http.MethodGet, "/v1/platform/releases/latest", "", nil)
	problem := decode[httpapi.Problem](t, r)
	if r.StatusCode != http.StatusServiceUnavailable || problem.Code != "ReleaseCheckUnavailable" {
		t.Fatalf("missing installer chart status=%d problem=%#v", r.StatusCode, problem)
	}
}

func TestReleaseCheckReportsMissingStableRelease(t *testing.T) {
	f := newUpgradeAPIWithReleaseError(t, releases.ErrNoStableRelease)
	f.bootstrap()
	r := f.request(http.MethodGet, "/v1/platform/releases/latest", "", nil)
	problem := decode[httpapi.Problem](t, r)
	if r.StatusCode != http.StatusServiceUnavailable || problem.Code != "NoStableRelease" {
		t.Fatalf("missing stable release status=%d problem=%#v", r.StatusCode, problem)
	}
}

func newUpgradeAPIWithReleaseError(t *testing.T, releaseErr error) *apiFixture {
	t.Helper()
	st := memory.New()
	srv := httptest.NewServer(httpapi.New(httpapi.Options{Store: st, BootstrapToken: "one-time-secret", Version: "0.1.0-rc.298", Releases: staticReleaseService{err: releaseErr}, HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000)}))
	jar, _ := cookiejar.New(nil)
	f := &apiFixture{t: t, server: srv, client: &http.Client{Jar: jar}, store: st}
	t.Cleanup(srv.Close)
	return f
}

func TestMutationRejectsMissingCSRFAndUnknownJSON(t *testing.T) {
	f := newAPI(t)
	f.bootstrap()
	saved := f.csrf
	f.csrf = ""
	r := f.request("POST", "/v1/projects", "p", map[string]string{"name": "Denied"})
	p := decode[httpapi.Problem](t, r)
	if r.StatusCode != 403 || p.Code != "CSRFRejected" {
		t.Fatalf("expected CSRF rejection, got %d %#v", r.StatusCode, p)
	}
	f.csrf = saved
	req, err := http.NewRequest("POST", f.server.URL+"/v1/projects", strings.NewReader(`{"name":"Demo","unknown":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "x")
	req.Header.Set("X-CSRF-Token", f.csrf)
	r, err = f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	p = decode[httpapi.Problem](t, r)
	if r.StatusCode != 400 || p.Code != "InvalidJSON" {
		t.Fatalf("expected closed JSON request, got %d %#v", r.StatusCode, p)
	}
}

func TestOpenAPIIsValidJSONAndDeclaresCurrentSpec(t *testing.T) {
	f := newAPI(t)
	r := f.request("GET", "/openapi.json", "", nil)
	defer r.Body.Close()
	var doc map[string]any
	if err := json.NewDecoder(r.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if doc["openapi"] != "3.2.0" {
		t.Fatalf("openapi=%v", doc["openapi"])
	}
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		t.Fatal("OpenAPI paths are missing")
	}
	membershipPath, ok := paths["/v1/teams/{teamId}/members/{userId}"].(map[string]any)
	if !ok {
		t.Fatal("OpenAPI team-member removal path is missing")
	}
	remove, ok := membershipPath["delete"].(map[string]any)
	if !ok || remove["operationId"] != "removeTeamMember" {
		t.Fatalf("OpenAPI team-member removal operation=%#v", remove)
	}
	grantCollection, ok := paths["/v1/projects/{id}/grants"].(map[string]any)
	if !ok {
		t.Fatal("OpenAPI project access-grant collection path is missing")
	}
	createGrant, ok := grantCollection["post"].(map[string]any)
	if !ok || createGrant["operationId"] != "createProjectAccessGrant" || createGrant["x-kuberploy-permission"] != "grants.manage" {
		t.Fatalf("OpenAPI project access-grant create operation=%#v", createGrant)
	}
	grantItem, ok := paths["/v1/projects/{projectId}/grants/{grantId}"].(map[string]any)
	if !ok {
		t.Fatal("OpenAPI exact access-grant deletion path is missing")
	}
	deleteGrant, ok := grantItem["delete"].(map[string]any)
	if !ok || deleteGrant["operationId"] != "deleteProjectAccessGrant" || deleteGrant["x-kuberploy-idempotency"] != "required" {
		t.Fatalf("OpenAPI exact access-grant deletion operation=%#v", deleteGrant)
	}
	metricsPath, ok := paths["/v1/metrics/query-range"].(map[string]any)
	if !ok {
		t.Fatal("OpenAPI scoped metrics query path is missing")
	}
	queryMetrics, ok := metricsPath["get"].(map[string]any)
	if !ok || queryMetrics["operationId"] != "queryMetricRange" || queryMetrics["x-kuberploy-permission"] != "metrics.read" {
		t.Fatalf("OpenAPI scoped metrics operation=%#v", queryMetrics)
	}
}

func TestMachineContractDocumentsArePublicValidatedAndCacheable(t *testing.T) {
	f := newAPI(t)
	tests := []struct {
		path        string
		contentType string
		identity    func(t *testing.T, body []byte)
	}{
		{path: "/openapi.json", contentType: "application/json", identity: func(t *testing.T, body []byte) {
			var document struct {
				OpenAPI string `json:"openapi"`
			}
			if err := json.Unmarshal(body, &document); err != nil || document.OpenAPI != "3.2.0" {
				t.Fatalf("unexpected OpenAPI document: version=%q err=%v", document.OpenAPI, err)
			}
		}},
		{path: "/openapi-agent.json", contentType: "application/json", identity: func(t *testing.T, body []byte) {
			var profile struct {
				SchemaVersion  string `json:"schemaVersion"`
				Authentication struct {
					ServiceAccountsAvailable bool `json:"serviceAccountsAvailable"`
				} `json:"authentication"`
				Operations []struct {
					OperationID string `json:"operationId"`
				} `json:"operations"`
			}
			if err := json.Unmarshal(body, &profile); err != nil {
				t.Fatal(err)
			}
			if profile.SchemaVersion != "1.0.0" || !profile.Authentication.ServiceAccountsAvailable || len(profile.Operations) < 20 {
				t.Fatalf("unexpected agent profile: version=%q serviceAccounts=%t operations=%d", profile.SchemaVersion, profile.Authentication.ServiceAccountsAvailable, len(profile.Operations))
			}
			for _, operation := range profile.Operations {
				lower := strings.ToLower(operation.OperationID)
				if strings.Contains(lower, "bootstrap") || strings.Contains(lower, "upgrade") || strings.Contains(lower, "grant") || strings.Contains(lower, "invitation") || strings.Contains(lower, "serviceaccount") {
					t.Fatalf("sensitive operation advertised to agents: %q", operation.OperationID)
				}
			}
		}},
		{path: "/arazzo.yaml", contentType: "application/vnd.oai.workflows+yaml;version=1.1.0", identity: func(t *testing.T, body []byte) {
			var document struct {
				Arazzo    string `json:"arazzo"`
				Workflows []any  `json:"workflows"`
			}
			if err := json.Unmarshal(body, &document); err != nil || document.Arazzo != "1.1.0" || len(document.Workflows) != 3 {
				t.Fatalf("unexpected Arazzo document: version=%q workflows=%d err=%v", document.Arazzo, len(document.Workflows), err)
			}
		}},
	}

	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			response, err := f.client.Get(f.server.URL + test.path)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != test.contentType {
				t.Fatalf("contract response: status=%d content-type=%q", response.StatusCode, response.Header.Get("Content-Type"))
			}
			etag := response.Header.Get("ETag")
			if !strings.HasPrefix(etag, `"sha256-`) || !strings.HasSuffix(etag, `"`) || response.Header.Get("Cache-Control") != "public, max-age=300, must-revalidate" || response.Header.Get("X-Content-Type-Options") != "nosniff" {
				t.Fatalf("contract validators: etag=%q cache=%q nosniff=%q", etag, response.Header.Get("Cache-Control"), response.Header.Get("X-Content-Type-Options"))
			}
			test.identity(t, body)

			request, err := http.NewRequest(http.MethodGet, f.server.URL+test.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("If-None-Match", etag)
			response, err = f.client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusNotModified {
				t.Fatalf("conditional response status=%d", response.StatusCode)
			}
		})
	}
}
