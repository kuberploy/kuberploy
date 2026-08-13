package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/kuberploy/kuberploy/internal/domain"
)

func TestProjectGrantAPIRequiresExactScopeAndIdempotentDelete(t *testing.T) {
	f := newAPI(t)
	admin := f.bootstrap()
	response := f.request("POST", "/v1/teams", "grant-team", map[string]string{"name": "Access", "slug": "access-team"})
	team := decode[domain.Team](t, response)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("team status=%d", response.StatusCode)
	}
	response = f.request("POST", "/v1/projects", "grant-project", map[string]string{"name": "Payments", "slug": "access-payments", "teamId": team.ID})
	project := decode[domain.Project](t, response)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("project status=%d", response.StatusCode)
	}
	response = f.request("POST", "/v1/teams", "grant-subject-team", map[string]string{"name": "Backend", "slug": "access-backend"})
	subjectTeam := decode[domain.Team](t, response)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("subject team status=%d", response.StatusCode)
	}
	input := map[string]any{"subjectUserId": admin.ID, "role": "viewer", "scopeType": "project", "scopeId": project.ID, "permissions": []string{"logs.read"}}
	response = f.request("POST", "/v1/projects/"+project.ID+"/grants", "grant-create", input)
	grant := decode[domain.AccessGrant](t, response)
	if response.StatusCode != http.StatusCreated || grant.SubjectUserID != admin.ID || grant.ScopeID != project.ID || len(grant.Permissions) != 1 {
		t.Fatalf("grant=%#v status=%d", grant, response.StatusCode)
	}
	response = f.request("GET", "/v1/projects/"+project.ID+"/grants", "", nil)
	listed := decode[struct {
		Items []domain.AccessGrant `json:"items"`
	}](t, response)
	if response.StatusCode != http.StatusOK || len(listed.Items) != 1 || listed.Items[0].ID != grant.ID {
		t.Fatalf("listed=%#v status=%d", listed, response.StatusCode)
	}
	emptyPermissionsInput := map[string]any{"subjectUserId": admin.ID, "role": "developer", "scopeType": "project", "scopeId": project.ID}
	response = f.request("POST", "/v1/projects/"+project.ID+"/grants", "grant-create-empty-permissions", emptyPermissionsInput)
	emptyPermissionsGrant := decode[domain.AccessGrant](t, response)
	if response.StatusCode != http.StatusCreated || emptyPermissionsGrant.Permissions == nil || len(emptyPermissionsGrant.Permissions) != 0 {
		t.Fatalf("empty permissions must serialize as an array: grant=%#v status=%d", emptyPermissionsGrant, response.StatusCode)
	}
	teamInput := map[string]any{"subjectTeamId": subjectTeam.ID, "role": "viewer", "scopeType": "project", "scopeId": project.ID}
	response = f.request("POST", "/v1/projects/"+project.ID+"/grants", "grant-create-team", teamInput)
	teamGrant := decode[domain.AccessGrant](t, response)
	if response.StatusCode != http.StatusCreated || teamGrant.SubjectTeamID != subjectTeam.ID || teamGrant.SubjectUserID != "" {
		t.Fatalf("team grant=%#v status=%d", teamGrant, response.StatusCode)
	}
	bothSubjects := map[string]any{"subjectUserId": admin.ID, "subjectTeamId": subjectTeam.ID, "role": "viewer", "scopeType": "project", "scopeId": project.ID}
	response = f.request("POST", "/v1/projects/"+project.ID+"/grants", "grant-create-both-subjects", bothSubjects)
	problem := decode[struct {
		Code string `json:"code"`
	}](t, response)
	if response.StatusCode != http.StatusUnprocessableEntity || problem.Code != "ValidationFailed" {
		t.Fatalf("both-subject grant status=%d problem=%#v", response.StatusCode, problem)
	}
	response = f.request("DELETE", "/v1/projects/"+project.ID+"/grants/"+grant.ID, "grant-delete", nil)
	if response.StatusCode != http.StatusNoContent || response.Header.Get("Idempotent-Replay") != "" {
		t.Fatalf("first delete status=%d replay=%q", response.StatusCode, response.Header.Get("Idempotent-Replay"))
	}
	response.Body.Close()
	response = f.request("DELETE", "/v1/projects/"+project.ID+"/grants/"+grant.ID, "grant-delete", nil)
	if response.StatusCode != http.StatusNoContent || response.Header.Get("Idempotent-Replay") != "true" {
		t.Fatalf("replayed delete status=%d replay=%q", response.StatusCode, response.Header.Get("Idempotent-Replay"))
	}
	response.Body.Close()
	invalid := map[string]any{"subjectUserId": admin.ID, "role": "platform-admin", "scopeType": "platform", "scopeId": "platform"}
	response = f.request("POST", "/v1/projects/"+project.ID+"/grants", "invalid-platform-grant", invalid)
	problem = decode[struct {
		Code string `json:"code"`
	}](t, response)
	if response.StatusCode != http.StatusUnprocessableEntity || problem.Code != "ValidationFailed" {
		t.Fatalf("invalid platform grant status=%d problem=%#v", response.StatusCode, problem)
	}
	invalid = map[string]any{"subjectUserId": "not-a-user-id", "role": "viewer", "scopeType": "application", "scopeId": "not-an-application-id"}
	response = f.request("POST", "/v1/projects/"+project.ID+"/grants", "invalid-grant-identifiers", invalid)
	validationProblem := decode[struct {
		Code   string           `json:"code"`
		Errors []map[string]any `json:"errors"`
	}](t, response)
	if response.StatusCode != http.StatusUnprocessableEntity || validationProblem.Code != "ValidationFailed" || len(validationProblem.Errors) != 2 {
		t.Fatalf("invalid grant identifiers status=%d problem=%#v", response.StatusCode, validationProblem)
	}
	response = f.request("GET", "/v1/projects/not-a-project-id/grants", "", nil)
	problem = decode[struct {
		Code string `json:"code"`
	}](t, response)
	if response.StatusCode != http.StatusNotFound || problem.Code != "NotFound" {
		t.Fatalf("invalid project path status=%d problem=%#v", response.StatusCode, problem)
	}
}
