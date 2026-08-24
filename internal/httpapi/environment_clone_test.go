package httpapi_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/httpapi"
)

func TestEnvironmentCloneHTTPIsAuthorizedIdempotentAndSideEffectFree(t *testing.T) {
	f := newAPI(t)
	unauthenticated := f.request(http.MethodPost, "/v1/environments/11111111-1111-4111-8111-111111111111/clone", "unauthenticated-clone", map[string]string{"name": "Production"})
	if unauthenticated.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", unauthenticated.StatusCode)
	}
	unauthenticated.Body.Close()
	f.bootstrap()

	response := f.request(http.MethodPost, "/v1/projects", "clone-http-project", map[string]string{"name": "Clone HTTP", "slug": "clone-http"})
	project := decode[domain.Project](t, response)
	response = f.request(http.MethodPost, "/v1/environments", "clone-http-source", map[string]string{
		"projectId": project.ID, "name": "Development", "slug": "development", "protectionPolicy": "development",
	})
	source := decode[domain.Environment](t, response)
	response = f.request(http.MethodPost, "/v1/applications", "clone-http-app", map[string]string{"projectId": project.ID, "name": "API", "slug": "api"})
	application := decode[domain.Application](t, response)
	response = f.request(http.MethodPost, "/v1/deployments", "clone-http-deployment", map[string]any{
		"environmentId": source.ID, "applicationId": application.ID,
		"image": "registry.example.test/api@sha256:" + strings.Repeat("a", 64), "port": 8080,
	})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("source deployment status=%d", response.StatusCode)
	}
	response.Body.Close()
	outboxBefore := f.store.OutboxCount()
	auditsBefore := f.store.AuditCount()

	missingKey := f.request(http.MethodPost, "/v1/environments/"+source.ID+"/clone", "", map[string]string{"name": "Production"})
	problem := decode[httpapi.Problem](t, missingKey)
	if missingKey.StatusCode != http.StatusBadRequest || problem.Code != "IdempotencyKeyRequired" {
		t.Fatalf("missing key status=%d problem=%#v", missingKey.StatusCode, problem)
	}

	cloneBody := map[string]string{"name": "Production", "slug": "production"}
	response = f.request(http.MethodPost, "/v1/environments/"+source.ID+"/clone", "clone-http-environment", cloneBody)
	cloned := decode[domain.EnvironmentCloneResult](t, response)
	if response.StatusCode != http.StatusCreated || response.Header.Get("Location") != "/v1/environments/"+cloned.Environment.ID ||
		cloned.Environment.ProjectID != project.ID || cloned.Environment.ProtectionPolicy != domain.EnvironmentDevelopment || len(cloned.AppPlacements) != 1 {
		t.Fatalf("clone status=%d result=%#v", response.StatusCode, cloned)
	}
	placement := cloned.AppPlacements[0]
	if placement.ApplicationID != application.ID || placement.EnvironmentID != cloned.Environment.ID ||
		placement.State != domain.EnvironmentAppPlacementDraft || placement.DesiredState != domain.EnvironmentAppPlacementStopped {
		t.Fatalf("placement=%#v", placement)
	}
	if f.store.OutboxCount() != outboxBefore || f.store.AuditCount() != auditsBefore+1 {
		t.Fatalf("clone side effects outbox=%d audit=%d", f.store.OutboxCount(), f.store.AuditCount())
	}
	response = f.request(http.MethodGet, "/v1/deployments", "", nil)
	deployments := decode[struct {
		Items []domain.Deployment `json:"items"`
	}](t, response)
	if response.StatusCode != http.StatusOK || len(deployments.Items) != 1 || deployments.Items[0].EnvironmentID != source.ID {
		t.Fatalf("clone created deployment: %#v", deployments.Items)
	}

	response = f.request(http.MethodPost, "/v1/environments/"+source.ID+"/clone", "clone-http-environment", cloneBody)
	replay := decode[domain.EnvironmentCloneResult](t, response)
	if response.StatusCode != http.StatusCreated || response.Header.Get("Idempotent-Replay") != "true" || replay.Environment.ID != cloned.Environment.ID ||
		f.store.OutboxCount() != outboxBefore || f.store.AuditCount() != auditsBefore+1 {
		t.Fatalf("replay status=%d result=%#v", response.StatusCode, replay)
	}
	response = f.request(http.MethodPost, "/v1/environments/"+source.ID+"/clone", "clone-http-environment", map[string]string{"name": "Different", "slug": "different"})
	problem = decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusConflict || problem.Code != "IdempotencyConflict" {
		t.Fatalf("conflict status=%d problem=%#v", response.StatusCode, problem)
	}
}

func TestEnvironmentCloneHTTPRequiresCreatePermissionAtSourceProject(t *testing.T) {
	f := newAPI(t)
	f.bootstrap()
	response := f.request(http.MethodPost, "/v1/projects", "clone-auth-http-project", map[string]string{"name": "Clone Auth", "slug": "clone-auth-http"})
	project := decode[domain.Project](t, response)
	response = f.request(http.MethodPost, "/v1/environments", "clone-auth-http-source", map[string]string{"projectId": project.ID, "name": "Development", "slug": "development"})
	source := decode[domain.Environment](t, response)

	response = f.request(http.MethodPost, "/v1/users/invitations", "clone-auth-http-invite", map[string]string{"email": "clone-environment@example.test"})
	invitation := decode[domain.UserInvitation](t, response)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}
	const password = "clone environment correct horse battery staple"
	accepted := cloneRequestAs(t, client, f.server.URL, http.MethodPost, "/v1/auth/invitations/accept", "", map[string]string{
		"token": invitation.Token, "displayName": "Environment developer", "password": password,
	})
	member := decode[domain.User](t, accepted)
	if accepted.StatusCode != http.StatusCreated {
		t.Fatalf("accept status=%d", accepted.StatusCode)
	}
	response = f.request(http.MethodPost, "/v1/projects/"+project.ID+"/grants", "clone-auth-http-grant", map[string]any{
		"subjectUserId": member.ID, "role": "developer", "scopeType": "environment", "scopeId": source.ID,
	})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("grant status=%d", response.StatusCode)
	}
	response.Body.Close()
	login := cloneRequestAs(t, client, f.server.URL, http.MethodPost, "/v1/auth/login", "", map[string]string{
		"email": "clone-environment@example.test", "password": password,
	})
	if login.StatusCode != http.StatusOK {
		t.Fatalf("login status=%d", login.StatusCode)
	}
	csrf := login.Header.Get("X-CSRF-Token")
	login.Body.Close()

	denied := cloneRequestAs(t, client, f.server.URL, http.MethodPost, "/v1/environments/"+source.ID+"/clone", csrf, map[string]string{"name": "Production", "slug": "production"})
	problem := decode[httpapi.Problem](t, denied)
	if denied.StatusCode != http.StatusForbidden || problem.Code != "Forbidden" {
		t.Fatalf("environment-scoped clone status=%d problem=%#v", denied.StatusCode, problem)
	}
}

func TestCreateApplicationAtomicallyCreatesStoppedEnvironmentPlacement(t *testing.T) {
	f := newAPI(t)
	f.bootstrap()
	response := f.request(http.MethodPost, "/v1/projects", "create-app-placement-project", map[string]string{
		"name": "Placement", "slug": "placement",
	})
	project := decode[domain.Project](t, response)
	response = f.request(http.MethodPost, "/v1/environments", "create-app-placement-environment", map[string]string{
		"projectId": project.ID, "name": "Production", "slug": "production",
	})
	environment := decode[domain.Environment](t, response)
	outboxBefore := f.store.OutboxCount()

	body := map[string]string{
		"projectId": project.ID, "environmentId": environment.ID, "name": "API", "slug": "api", "sourceKind": "git-ssh",
	}
	response = f.request(http.MethodPost, "/v1/applications", "create-app-placement", body)
	application := decode[domain.Application](t, response)
	if response.StatusCode != http.StatusCreated || application.ProjectID != project.ID || application.SourceKind != domain.ApplicationSourceGitSSH {
		t.Fatalf("create status=%d application=%#v", response.StatusCode, application)
	}
	response = f.request(http.MethodGet, "/v1/environments/"+environment.ID+"/apps", "", nil)
	placements := decode[struct {
		Items []domain.EnvironmentAppPlacement `json:"items"`
	}](t, response)
	if response.StatusCode != http.StatusOK || len(placements.Items) != 1 {
		t.Fatalf("placements status=%d items=%#v", response.StatusCode, placements.Items)
	}
	placement := placements.Items[0]
	if placement.ApplicationID != application.ID || placement.State != domain.EnvironmentAppPlacementDraft ||
		placement.DesiredState != domain.EnvironmentAppPlacementStopped || f.store.OutboxCount() != outboxBefore {
		t.Fatalf("placement=%#v outbox=%d", placement, f.store.OutboxCount())
	}

	response = f.request(http.MethodPost, "/v1/applications", "create-app-placement", body)
	replay := decode[domain.Application](t, response)
	if response.StatusCode != http.StatusCreated || response.Header.Get("Idempotent-Replay") != "true" || replay.ID != application.ID {
		t.Fatalf("replay status=%d application=%#v", response.StatusCode, replay)
	}
	response = f.request(http.MethodGet, "/v1/environments/"+environment.ID+"/apps", "", nil)
	placements = decode[struct {
		Items []domain.EnvironmentAppPlacement `json:"items"`
	}](t, response)
	if len(placements.Items) != 1 {
		t.Fatalf("replay duplicated placement: %#v", placements.Items)
	}
}

func cloneRequestAs(t *testing.T, client *http.Client, baseURL, method, path, csrf string, body any) *http.Response {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(method, baseURL+path, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if csrf != "" {
		request.Header.Set("X-CSRF-Token", csrf)
	}
	if strings.HasSuffix(path, "/clone") {
		request.Header.Set("Idempotency-Key", "clone-auth-http-denied")
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
