package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/httpapi"
)

type tokenIssueWire struct {
	TokenRecord domain.ServiceAccountToken `json:"tokenRecord"`
	Token       string                     `json:"token"`
}

func bearerRequest(t *testing.T, client *http.Client, serverURL, method, path, token, key string, body any) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, serverURL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestServiceAccountOneTimeTokenAndScopedBearer(t *testing.T) {
	f := newAPI(t)
	f.bootstrap()

	response := f.request(http.MethodPost, "/v1/projects", "automation-project", map[string]string{"name": "Automation"})
	project := decode[domain.Project](t, response)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("project status=%d body=%#v", response.StatusCode, project)
	}
	response = f.request(http.MethodPost, "/v1/projects/"+project.ID+"/service-accounts", "automation-account", map[string]string{"name": "Release bot", "role": "developer"})
	account := decode[domain.ServiceAccount](t, response)
	if response.StatusCode != http.StatusCreated || account.ID == "" || account.ProjectID != project.ID || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("account status=%d body=%#v cache=%q", response.StatusCode, account, response.Header.Get("Cache-Control"))
	}

	expiresAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second).Format(time.RFC3339)
	tokenRequest := map[string]any{"name": "Read API", "scopes": []string{"app.read"}, "expiresAt": expiresAt}
	response = f.request(http.MethodPost, "/v1/service-accounts/"+account.ID+"/tokens", "read-token", tokenRequest)
	issued := decode[tokenIssueWire](t, response)
	if response.StatusCode != http.StatusCreated || !strings.HasPrefix(issued.Token, "kp_sa_") || len(issued.Token) != len("kp_sa_")+43 || issued.TokenRecord.Prefix != issued.Token[:len("kp_sa_")+8] || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("token issue status=%d body=%#v", response.StatusCode, issued)
	}
	rawReadToken := issued.Token

	response = f.request(http.MethodPost, "/v1/service-accounts/"+account.ID+"/tokens", "read-token", tokenRequest)
	replayed := decode[tokenIssueWire](t, response)
	if response.StatusCode != http.StatusCreated || replayed.Token != "" || replayed.TokenRecord.ID != issued.TokenRecord.ID || response.Header.Get("Idempotent-Replay") != "true" {
		t.Fatalf("token replay redisclosed or changed credential: status=%d body=%#v", response.StatusCode, replayed)
	}

	response = f.request(http.MethodGet, "/v1/service-accounts/"+account.ID+"/tokens", "", nil)
	tokenListBody, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || bytes.Contains(tokenListBody, []byte(rawReadToken)) || bytes.Contains(tokenListBody, []byte(`"tokenHash"`)) {
		t.Fatalf("token list leaked credential: status=%d body=%s", response.StatusCode, tokenListBody)
	}

	bearerClient := &http.Client{}
	response = bearerRequest(t, bearerClient, f.server.URL, http.MethodGet, "/v1/projects/"+project.ID, rawReadToken, "", nil)
	gotProject := decode[domain.Project](t, response)
	if response.StatusCode != http.StatusOK || gotProject.ID != project.ID {
		t.Fatalf("read bearer status=%d body=%#v", response.StatusCode, gotProject)
	}
	response = bearerRequest(t, bearerClient, f.server.URL, http.MethodGet, "/v1/me", rawReadToken, "", nil)
	principalBody, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var principal struct {
		ID             string `json:"id"`
		Authentication struct {
			Kind             string                   `json:"kind"`
			ServiceAccountID string                   `json:"serviceAccountId"`
			TokenID          string                   `json:"tokenId"`
			Scopes           []domain.AutomationScope `json:"scopes"`
			ExpiresAt        time.Time                `json:"expiresAt"`
		} `json:"authentication"`
	}
	if err = json.Unmarshal(principalBody, &principal); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "private, no-store" || principal.ID != account.ID || principal.Authentication.Kind != "service-account" || principal.Authentication.ServiceAccountID != account.ID || principal.Authentication.TokenID != issued.TokenRecord.ID || len(principal.Authentication.Scopes) != 1 || principal.Authentication.Scopes[0] != domain.AutomationScopeAppRead || !principal.Authentication.ExpiresAt.Equal(issued.TokenRecord.ExpiresAt) || bytes.Contains(principalBody, []byte(rawReadToken)) || bytes.Contains(principalBody, []byte(`"tokenHash"`)) {
		t.Fatalf("unsafe or incomplete bearer principal: status=%d cache=%q body=%s", response.StatusCode, response.Header.Get("Cache-Control"), principalBody)
	}
	response = bearerRequest(t, bearerClient, f.server.URL, http.MethodGet, "/v1/capabilities", rawReadToken, "", nil)
	capabilities := decode[struct {
		Actions  []string        `json:"actions"`
		Features map[string]bool `json:"features"`
	}](t, response)
	if response.StatusCode != http.StatusOK || !capabilities.Features["serviceAccounts"] || !containsString(capabilities.Actions, "projects:read") || containsString(capabilities.Actions, "applications:create") || containsString(capabilities.Actions, "access-grants:read") || containsString(capabilities.Actions, "teams:read") {
		t.Fatalf("bearer capabilities escaped coarse scopes: status=%d body=%#v", response.StatusCode, capabilities)
	}
	response = bearerRequest(t, bearerClient, f.server.URL, http.MethodPost, "/v1/applications", rawReadToken, "read-token-mutation", map[string]string{"projectId": project.ID, "name": "Denied"})
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusForbidden || problem.Code != "InsufficientTokenScope" || !strings.Contains(response.Header.Get("WWW-Authenticate"), "insufficient_scope") {
		t.Fatalf("coarse scope bypass status=%d problem=%#v", response.StatusCode, problem)
	}
	response = bearerRequest(t, bearerClient, f.server.URL, http.MethodGet, "/v1/users", rawReadToken, "", nil)
	problem = decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusForbidden || problem.Code != "HumanSessionRequired" {
		t.Fatalf("bearer reached human-only endpoint status=%d problem=%#v", response.StatusCode, problem)
	}
	logTokenRequest := map[string]any{"name": "Log API", "scopes": []string{"logs.read"}, "expiresAt": expiresAt}
	response = f.request(http.MethodPost, "/v1/service-accounts/"+account.ID+"/tokens", "log-token", logTokenRequest)
	logIssued := decode[tokenIssueWire](t, response)
	if response.StatusCode != http.StatusCreated || logIssued.Token == "" {
		t.Fatalf("log token status=%d body=%#v", response.StatusCode, logIssued)
	}
	response = bearerRequest(t, bearerClient, f.server.URL, http.MethodGet, "/v1/workloads/00000000-0000-4000-8000-000000000001/logs/follow", logIssued.Token, "", nil)
	problem = decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusForbidden || problem.Code != "HumanSessionRequired" {
		t.Fatalf("bearer reached human-only log follow status=%d problem=%#v", response.StatusCode, problem)
	}

	editTokenRequest := map[string]any{"name": "Edit API", "scopes": []string{"app.edit", "app.read"}, "expiresAt": expiresAt}
	response = f.request(http.MethodPost, "/v1/service-accounts/"+account.ID+"/tokens", "edit-token", editTokenRequest)
	editIssued := decode[tokenIssueWire](t, response)
	if response.StatusCode != http.StatusCreated || editIssued.Token == "" {
		t.Fatalf("edit token status=%d body=%#v", response.StatusCode, editIssued)
	}
	response = bearerRequest(t, bearerClient, f.server.URL, http.MethodGet, "/v1/capabilities", editIssued.Token, "", nil)
	editCapabilities := decode[struct {
		Actions []string `json:"actions"`
	}](t, response)
	if response.StatusCode != http.StatusOK || !containsString(editCapabilities.Actions, "applications:create") || containsString(editCapabilities.Actions, "secret-bindings:bind") {
		t.Fatalf("edit bearer capabilities advertised unsupported secret mutation: status=%d actions=%#v", response.StatusCode, editCapabilities.Actions)
	}
	response = bearerRequest(t, bearerClient, f.server.URL, http.MethodPost, "/v1/applications", editIssued.Token, "bearer-application", map[string]string{"projectId": project.ID, "name": "Bearer app"})
	application := decode[domain.Application](t, response)
	if response.StatusCode != http.StatusCreated || application.ProjectID != project.ID {
		t.Fatalf("bearer mutation status=%d body=%#v", response.StatusCode, application)
	}

	// Sending a session cookie and bearer together is rejected instead of
	// silently choosing the more privileged identity.
	response = bearerRequest(t, f.client, f.server.URL, http.MethodGet, "/v1/me", rawReadToken, "", nil)
	problem = decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusUnauthorized || problem.Code != "Unauthenticated" {
		t.Fatalf("ambiguous authentication status=%d problem=%#v", response.StatusCode, problem)
	}

	request, err := http.NewRequest(http.MethodGet, f.server.URL+"/v1/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Add("Authorization", "Bearer "+rawReadToken)
	request.Header.Add("Authorization", "Bearer "+editIssued.Token)
	response, err = bearerClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	problem = decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusUnauthorized || problem.Code != "Unauthenticated" {
		t.Fatalf("multiple bearer headers status=%d problem=%#v", response.StatusCode, problem)
	}

	response = f.request(http.MethodDelete, "/v1/service-accounts/"+account.ID+"/tokens/"+issued.TokenRecord.ID, "revoke-read-token", nil)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke status=%d", response.StatusCode)
	}
	response.Body.Close()
	response = bearerRequest(t, bearerClient, f.server.URL, http.MethodGet, "/v1/me", rawReadToken, "", nil)
	problem = decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusUnauthorized || problem.Code != "Unauthenticated" {
		t.Fatalf("revoked token status=%d problem=%#v", response.StatusCode, problem)
	}

	response = f.request(http.MethodDelete, "/v1/service-accounts/"+account.ID, "disable-account", nil)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("disable status=%d", response.StatusCode)
	}
	response.Body.Close()
	response = bearerRequest(t, bearerClient, f.server.URL, http.MethodGet, "/v1/me", editIssued.Token, "", nil)
	problem = decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusUnauthorized || problem.Code != "Unauthenticated" {
		t.Fatalf("disabled account token status=%d problem=%#v", response.StatusCode, problem)
	}
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func TestServiceAccountValidationAndMalformedBearerFailClosed(t *testing.T) {
	f := newAPI(t)
	f.bootstrap()
	response := f.request(http.MethodPost, "/v1/projects", "validation-project", map[string]string{"name": "Validation"})
	project := decode[domain.Project](t, response)
	response = f.request(http.MethodPost, "/v1/projects/"+project.ID+"/service-accounts", "invalid-role", map[string]string{"name": "Root bot", "role": "platform-admin"})
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusUnprocessableEntity || problem.Code != "ValidationFailed" {
		t.Fatalf("platform role status=%d problem=%#v", response.StatusCode, problem)
	}

	for _, authorization := range []string{"Bearer", "Basic abc", "Bearer short", "bearer kp_sa_" + strings.Repeat("a", 43)} {
		request, err := http.NewRequest(http.MethodGet, f.server.URL+"/v1/me", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", authorization)
		response, err = (&http.Client{}).Do(request)
		if err != nil {
			t.Fatal(err)
		}
		problem = decode[httpapi.Problem](t, response)
		if response.StatusCode != http.StatusUnauthorized || problem.Code != "Unauthenticated" {
			t.Fatalf("authorization=%q status=%d problem=%#v", authorization, response.StatusCode, problem)
		}
	}
}
