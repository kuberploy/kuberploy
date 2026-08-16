package httpapi_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/httpapi"
)

func TestAuditTimelineIsBoundedSafeAndNoStore(t *testing.T) {
	f := newAPI(t)
	admin := f.bootstrap()
	targetID := "11111111-1111-4111-8111-111111111111"
	f.store.AddAuditEvent(domain.AuditEvent{ID: "22222222-2222-4222-8222-222222222222",
		ActorID: admin.ID, Action: "deployment.config.accepted", TargetType: "deployment",
		TargetID: targetID, Outcome: "accepted", RequestID: "request-safe", CreatedAt: time.Now().UTC()})
	response := f.request(http.MethodGet, "/v1/audit-events?targetType=deployment&targetId="+targetID+"&limit=1", "", nil)
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d cache=%q", response.StatusCode, response.Header.Get("Cache-Control"))
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if !strings.Contains(string(body), `"outcome":"accepted"`) ||
		strings.Contains(strings.ToLower(string(body)), "detail") ||
		strings.Contains(strings.ToLower(string(body)), "secret") {
		t.Fatalf("unsafe audit response: %s", body)
	}
	response = f.request(http.MethodGet, "/v1/audit-events?targetType=deployment", "", nil)
	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unpaired target status=%d", response.StatusCode)
	}
}

func TestAuditTimelineRequiresAppReadAutomationScope(t *testing.T) {
	f := newAPI(t)
	f.bootstrap()
	response := f.request(http.MethodPost, "/v1/projects", "audit-scope-project", map[string]string{"name": "Audit scope"})
	project := decode[domain.Project](t, response)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("project status=%d", response.StatusCode)
	}
	response = f.request(http.MethodPost, "/v1/projects/"+project.ID+"/service-accounts", "audit-scope-account", map[string]string{"name": "Audit reader", "role": "developer"})
	account := decode[domain.ServiceAccount](t, response)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("service account status=%d", response.StatusCode)
	}
	expiresAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second).Format(time.RFC3339)
	issue := func(key string, scope string) string {
		response := f.request(http.MethodPost, "/v1/service-accounts/"+account.ID+"/tokens", key,
			map[string]any{"name": key, "scopes": []string{scope}, "expiresAt": expiresAt})
		issued := decode[tokenIssueWire](t, response)
		if response.StatusCode != http.StatusCreated || issued.Token == "" {
			t.Fatalf("token %s status=%d", scope, response.StatusCode)
		}
		return issued.Token
	}
	logsToken := issue("audit-logs-scope", "logs.read")
	appReadToken := issue("audit-app-read-scope", "app.read")
	targetID := project.ID
	f.store.AddAuditEvent(domain.AuditEvent{ID: "33333333-3333-4333-8333-333333333333", ActorID: account.ID,
		Action: "project.create", TargetType: "project", TargetID: targetID, Outcome: "accepted",
		RequestID: "audit-scope-request", CreatedAt: time.Now().UTC()})

	response = bearerRequest(t, &http.Client{}, f.server.URL, http.MethodGet,
		"/v1/audit-events?targetType=project&targetId="+targetID+"&limit=1", logsToken, "", nil)
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusForbidden || problem.Code != "InsufficientTokenScope" {
		t.Fatalf("logs-only audit status=%d problem=%#v", response.StatusCode, problem)
	}
	response.Body.Close()
	response = bearerRequest(t, &http.Client{}, f.server.URL, http.MethodGet,
		"/v1/audit-events?targetType=project&targetId="+targetID+"&limit=1", appReadToken, "", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("app-read audit status=%d", response.StatusCode)
	}
	response.Body.Close()
}
