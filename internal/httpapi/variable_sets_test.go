package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/gitpublication"
	"github.com/kuberploy/kuberploy/internal/httpapi"
)

func TestVariableSetHTTPUsesExactGitAuthorityAndReplaysBeforeReadiness(t *testing.T) {
	backend := &projectionHTTPBackend{}
	readiness := &projectionHTTPReadiness{}
	fixture := newProjectionAPI(t, backend, readiness)
	admin := fixture.bootstrap()
	capabilitiesResponse := fixture.request(http.MethodGet, "/v1/capabilities", "", nil)
	capabilities := decode[struct {
		Features map[string]bool `json:"features"`
	}](t, capabilitiesResponse)
	if capabilitiesResponse.StatusCode != http.StatusOK || !capabilities.Features["variableSets"] {
		t.Fatalf("variableSets capability was not advertised for the exact projection backend: %#v", capabilities.Features)
	}
	project, err := fixture.store.CreateProject(t.Context(), admin.ID, "variables-http-project", "variables-http-project",
		domain.CreateProject{Name: "Variables", Slug: "variables"})
	if err != nil {
		t.Fatal(err)
	}
	environment, err := fixture.store.CreateEnvironment(t.Context(), admin.ID, "variables-http-environment", "variables-http-environment",
		domain.CreateEnvironment{ProjectID: project.Value.ID, Name: "Protected", Slug: "protected", ProtectionPolicy: domain.EnvironmentProtected})
	if err != nil {
		t.Fatal(err)
	}
	binding := projectedHTTPBinding(t, project.Value.ID, environment.Value.ID, time.Now().UTC().Add(-time.Minute))
	if err = fixture.store.PutBinding(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	paths, err := gitprojection.DependencyPaths(binding)
	if err != nil {
		t.Fatal(err)
	}
	backend.variableSets = []gitprojection.VariableSetSnapshot{
		{Scope: "project", BindingID: binding.ID, ProjectID: project.Value.ID, EnvironmentID: environment.Value.ID, Path: paths[0], IndexedRevision: binding.IndexedRevision},
		{Scope: "environment", BindingID: binding.ID, ProjectID: project.Value.ID, EnvironmentID: environment.Value.ID, Path: paths[1], IndexedRevision: binding.IndexedRevision},
	}
	backend.variablePlan = gitprojection.WritePlan{BindingID: binding.ID, ProjectID: project.Value.ID, EnvironmentID: environment.Value.ID,
		BaseRevision: binding.IndexedRevision, Precondition: gitprojection.MutationCreateIfAbsent, PolicyVersion: binding.ParserVersion,
		VariableScope: "project", VariablePath: paths[0]}
	basePath := "/v1/environments/" + environment.Value.ID + "/variable-sets"
	response := fixture.request(http.MethodGet, basePath, "", nil)
	var listed struct {
		Items []gitprojection.VariableSetSnapshot `json:"items"`
	}
	if decodeJSONErr := json.NewDecoder(response.Body).Decode(&listed); decodeJSONErr != nil {
		t.Fatal(decodeJSONErr)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || len(listed.Items) != 2 || listed.Items[0].Present || listed.Items[0].Path != paths[0] {
		t.Fatalf("list status=%d body=%#v", response.StatusCode, listed)
	}
	raw := "apiVersion: variables.kuberploy.io/v1alpha1\nkind: VariableSet\nvalues:\n  REGION: ap-southeast-1\n  FEATURE_FLAG: \"true\"\n"
	previewResponse := configRequest(t, fixture, http.MethodPost, basePath+"/project/preview", "", map[string]string{"rawYaml": raw}, nil)
	var preview struct {
		PreviewToken string         `json:"previewToken"`
		Path         string         `json:"path"`
		Document     map[string]any `json:"document"`
		GitDiff      string         `json:"gitDiff"`
	}
	if err = json.NewDecoder(previewResponse.Body).Decode(&preview); err != nil {
		t.Fatal(err)
	}
	previewResponse.Body.Close()
	if previewResponse.StatusCode != http.StatusOK || preview.Path != paths[0] || preview.PreviewToken == "" ||
		!strings.Contains(preview.GitDiff, "FEATURE_FLAG") || preview.Document["kind"] != "VariableSet" {
		t.Fatalf("preview status=%d body=%#v", previewResponse.StatusCode, preview)
	}
	saveHeaders := map[string]string{"Preview-Token": preview.PreviewToken}
	saveResponse := configRequest(t, fixture, http.MethodPut, basePath+"/project", "variables-http-save", map[string]string{"rawYaml": raw}, saveHeaders)
	operation := decode[domain.Operation](t, saveResponse)
	if saveResponse.StatusCode != http.StatusAccepted || operation.Kind != "variable-set.git-write" {
		t.Fatalf("save status=%d operation=%#v", saveResponse.StatusCode, operation)
	}
	command, err := fixture.store.AcceptedGitWriteCommand(operation.ID)
	mode, modeErr := fixture.store.AcceptedGitPublicationMode(operation.ID)
	if err != nil || modeErr != nil || command.Path != paths[0] || command.Plan.VariableScope != "project" ||
		command.RequestDigest == command.ContentSHA256 || mode != gitpublication.ModePullRequest {
		t.Fatalf("command=%#v mode=%q err=%v modeErr=%v", command, mode, err, modeErr)
	}

	readiness.err = errUnavailable
	replayResponse := configRequest(t, fixture, http.MethodPut, basePath+"/project", "variables-http-save", map[string]string{"rawYaml": raw}, saveHeaders)
	replayed := decode[domain.Operation](t, replayResponse)
	if replayResponse.StatusCode != http.StatusAccepted || replayResponse.Header.Get("Idempotent-Replay") != "true" || replayed.ID != operation.ID {
		t.Fatalf("replay status=%d operation=%#v", replayResponse.StatusCode, replayed)
	}
}

func TestVariableSetHTTPRejectsCrossScopeBackendSubstitution(t *testing.T) {
	backend := &projectionHTTPBackend{}
	fixture := newProjectionAPI(t, backend, &projectionHTTPReadiness{})
	admin := fixture.bootstrap()
	project, _ := fixture.store.CreateProject(t.Context(), admin.ID, "variables-cross-project", "variables-cross-project", domain.CreateProject{Name: "Cross", Slug: "cross"})
	environment, _ := fixture.store.CreateEnvironment(t.Context(), admin.ID, "variables-cross-environment", "variables-cross-environment", domain.CreateEnvironment{ProjectID: project.Value.ID, Name: "Dev", Slug: "dev"})
	binding := projectedHTTPBinding(t, project.Value.ID, environment.Value.ID, time.Now().UTC().Add(-time.Minute))
	if err := fixture.store.PutBinding(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	paths, _ := gitprojection.DependencyPaths(binding)
	backend.variablePlan = gitprojection.WritePlan{BindingID: binding.ID, ProjectID: project.Value.ID, EnvironmentID: environment.Value.ID,
		BaseRevision: binding.IndexedRevision, Precondition: gitprojection.MutationCreateIfAbsent, PolicyVersion: binding.ParserVersion,
		VariableScope: "project", VariablePath: paths[0]}
	raw := "apiVersion: variables.kuberploy.io/v1alpha1\nkind: VariableSet\nvalues: {}\n"
	response := configRequest(t, fixture, http.MethodPost, "/v1/environments/"+environment.Value.ID+"/variable-sets/environment/preview", "", map[string]string{"rawYaml": raw}, nil)
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusServiceUnavailable || problem.Code != "GitProjectionUnavailable" {
		t.Fatalf("cross-scope status=%d problem=%#v", response.StatusCode, problem)
	}
}

func TestVariableSetHTTPRejectsIndexedSnapshotDriftWithoutReturningToken(t *testing.T) {
	backend := &projectionHTTPBackend{}
	fixture := newProjectionAPI(t, backend, &projectionHTTPReadiness{})
	admin := fixture.bootstrap()
	project, _ := fixture.store.CreateProject(t.Context(), admin.ID, "variables-race-project", "variables-race-project", domain.CreateProject{Name: "Race", Slug: "race"})
	environment, _ := fixture.store.CreateEnvironment(t.Context(), admin.ID, "variables-race-environment", "variables-race-environment", domain.CreateEnvironment{ProjectID: project.Value.ID, Name: "Dev", Slug: "dev"})
	binding := projectedHTTPBinding(t, project.Value.ID, environment.Value.ID, time.Now().UTC().Add(-time.Minute))
	if err := fixture.store.PutBinding(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	paths, _ := gitprojection.DependencyPaths(binding)
	backend.variablePlan = gitprojection.WritePlan{BindingID: binding.ID, ProjectID: project.Value.ID, EnvironmentID: environment.Value.ID,
		BaseRevision: binding.IndexedRevision, Precondition: gitprojection.MutationCreateIfAbsent, PolicyVersion: binding.ParserVersion,
		VariableScope: "project", VariablePath: paths[0]}
	backend.variableSets = []gitprojection.VariableSetSnapshot{{Scope: "project", BindingID: binding.ID, ProjectID: project.Value.ID,
		EnvironmentID: environment.Value.ID, Path: paths[0], IndexedRevision: strings.Repeat("d", 40)}}
	raw := "apiVersion: variables.kuberploy.io/v1alpha1\nkind: VariableSet\nvalues: {}\n"
	response := configRequest(t, fixture, http.MethodPost, "/v1/environments/"+environment.Value.ID+"/variable-sets/project/preview", "", map[string]string{"rawYaml": raw}, nil)
	var problem struct {
		httpapi.Problem
		PreviewToken string `json:"previewToken"`
	}
	if err := json.NewDecoder(response.Body).Decode(&problem); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusConflict || problem.Code != "Conflict" || problem.PreviewToken != "" {
		t.Fatalf("snapshot drift status=%d problem=%#v", response.StatusCode, problem)
	}
}

var errUnavailable = &temporaryVariableError{}

type temporaryVariableError struct{}

func (*temporaryVariableError) Error() string { return "temporarily unavailable" }
