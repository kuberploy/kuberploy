package httpapi_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/deploymentrollback"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

func newDeploymentRollbackAPI(t *testing.T) *apiFixture {
	t.Helper()
	st := memory.New()
	resolver := &deploymentrollback.Resolver{History: st, Artifacts: st, Publications: st}
	srv := httptest.NewServer(httpapi.New(httpapi.Options{Store: st, DeploymentRollbacks: resolver,
		BootstrapToken: "one-time-secret", Version: "test", AppConfigRenderedPreviews: staticAppConfigRenderer{}, HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000)}))
	jar, _ := cookiejar.New(nil)
	fixture := &apiFixture{t: t, server: srv, client: &http.Client{Jar: jar}, store: st}
	t.Cleanup(srv.Close)
	return fixture
}

func rollbackResources(t *testing.T, fixture *apiFixture, suffix string) (domain.User, domain.Project, domain.Environment, domain.Application) {
	t.Helper()
	admin := fixture.bootstrap()
	response := fixture.request(http.MethodPost, "/v1/projects", "rollback-project-"+suffix, map[string]string{"name": "Rollback " + suffix})
	project := decode[domain.Project](t, response)
	response = fixture.request(http.MethodPost, "/v1/environments", "rollback-environment-"+suffix, map[string]string{"projectId": project.ID, "name": "Production " + suffix, "protectionPolicy": "development"})
	environment := decode[domain.Environment](t, response)
	response = fixture.request(http.MethodPost, "/v1/applications", "rollback-application-"+suffix, map[string]string{"projectId": project.ID, "name": "API " + suffix})
	application := decode[domain.Application](t, response)
	return admin, project, environment, application
}

func submitRollbackFixtureDeployment(t *testing.T, fixture *apiFixture, environmentID, applicationID, key, character string) domain.Operation {
	t.Helper()
	response := fixture.request(http.MethodPost, "/v1/deployments", key, map[string]any{
		"environmentId": environmentID, "applicationId": applicationID,
		"image":   "registry.external.test/team/api@sha256:" + strings.Repeat(character, 64),
		"runtime": map[string]any{"replicas": 1, "ports": []map[string]any{{"name": "http", "containerPort": 8080}}, "resources": map[string]any{"requests": map[string]string{"cpu": "50m", "memory": "100Mi"}}},
	})
	operation := decode[domain.Operation](t, response)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("deployment operation=%#v status=%d", operation, response.StatusCode)
	}
	return operation
}

func completeRollbackSource(t *testing.T, fixture *apiFixture, operation domain.Operation) {
	t.Helper()
	if _, started, err := fixture.store.StartOperation(t.Context(), operation.ID, operation.Generation, "rollback-worker", time.Minute); err != nil || !started {
		t.Fatalf("start source started=%v err=%v", started, err)
	}
	if err := fixture.store.CompleteGitOperation(t.Context(), operation.ID, operation.Generation, "rollback-worker",
		domain.GitPublicationResult{Mode: "direct", Revision: strings.Repeat("c", 40)}); err != nil {
		t.Fatal(err)
	}
}

func TestDeploymentRollbackCatalogAndMutationUseOnlyExactSourceOperation(t *testing.T) {
	fixture := newDeploymentRollbackAPI(t)
	_, _, environment, application := rollbackResources(t, fixture, "main")
	source := submitRollbackFixtureDeployment(t, fixture, environment.ID, application.ID, "deployment-source-main", "a")
	completeRollbackSource(t, fixture, source)
	current := submitRollbackFixtureDeployment(t, fixture, environment.ID, application.ID, "deployment-current-main", "b")

	response := fixture.request(http.MethodGet, "/v1/deployments/"+current.TargetID+"/rollback-sources?limit=10", "", nil)
	catalog := decode[struct {
		Items []deploymentrollback.Candidate `json:"items"`
	}](t, response)
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "private, no-store" || len(catalog.Items) != 1 ||
		catalog.Items[0].SourceOperationID != source.ID || catalog.Items[0].ArtifactAssurance != deploymentrollback.ArtifactExternalDigestUnverified {
		t.Fatalf("catalog=%#v status=%d", catalog, response.StatusCode)
	}

	response = fixture.request(http.MethodPost, "/v1/deployments/"+current.TargetID+"/rollback", "deployment-rollback-stable-key",
		map[string]string{"sourceOperationId": source.ID})
	rollback := decode[domain.Operation](t, response)
	if response.StatusCode != http.StatusAccepted || rollback.Generation != current.Generation+1 || rollback.TargetID != current.TargetID || rollback.Kind != "deployment.git-write" {
		t.Fatalf("rollback=%#v status=%d", rollback, response.StatusCode)
	}
	snapshot, err := fixture.store.GetDeploymentForOperation(t.Context(), rollback.ID)
	if err != nil || snapshot.Image != "registry.external.test/team/api@sha256:"+strings.Repeat("a", 64) || snapshot.EnvironmentID != environment.ID || snapshot.ApplicationID != application.ID {
		t.Fatalf("rollback snapshot=%#v err=%v", snapshot, err)
	}
	response = fixture.request(http.MethodPost, "/v1/deployments/"+current.TargetID+"/rollback", "deployment-rollback-stable-key",
		map[string]string{"sourceOperationId": source.ID})
	replay := decode[domain.Operation](t, response)
	if response.StatusCode != http.StatusAccepted || response.Header.Get("Idempotent-Replay") != "true" || replay.ID != rollback.ID {
		t.Fatalf("replay=%#v status=%d", replay, response.StatusCode)
	}
}

func TestDeploymentRollbackRestoresExactRetainedAppConfig(t *testing.T) {
	fixture := newDeploymentRollbackAPI(t)
	_, _, environment, application := rollbackResources(t, fixture, "exact-config")
	initial := submitRollbackFixtureDeployment(t, fixture, environment.ID, application.ID, "deployment-exact-initial", "a")
	completeRollbackSource(t, fixture, initial)

	path := "/v1/deployments/" + initial.TargetID + "/config"
	response := fixture.request(http.MethodGet, path, "", nil)
	bundle := decode[configBundleWire](t, response)
	change := appconfig.Change{Mode: "jsonPatch", Patch: []appconfig.PatchOperation{
		{Op: "add", Path: "/spec/runtime/env", Value: []any{map[string]any{"name": "ROLLBACK_EXACT", "value": "retained"}}},
		{Op: "add", Path: "/spec/middlewares", Value: []any{map[string]any{"name": "force-https", "spec": map[string]any{"redirectScheme": map[string]any{"scheme": "https", "permanent": true}}}}},
	}}
	response = configRequest(t, fixture, http.MethodPost, path+"/preview", "", change, map[string]string{"If-Match": bundle.ETag})
	preview := decode[previewWire](t, response)
	if response.StatusCode != http.StatusOK || preview.PreviewToken == "" {
		t.Fatalf("preview status=%d body=%#v", response.StatusCode, preview)
	}
	response = configRequest(t, fixture, http.MethodPut, path, "save-exact-rollback-source", change,
		map[string]string{"If-Match": bundle.ETag, "Preview-Token": preview.PreviewToken})
	source := decode[domain.Operation](t, response)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("save source status=%d operation=%#v", response.StatusCode, source)
	}
	completeRollbackSource(t, fixture, source)
	sourceSnapshot, err := fixture.store.GetDeploymentForOperation(t.Context(), source.ID)
	if err != nil || !bytes.Contains(sourceSnapshot.ConfigRaw, []byte("ROLLBACK_EXACT")) || !bytes.Contains(sourceSnapshot.ConfigRaw, []byte("force-https")) {
		t.Fatalf("source snapshot incomplete: err=%v raw=%s", err, sourceSnapshot.ConfigRaw)
	}

	current := submitRollbackFixtureDeployment(t, fixture, environment.ID, application.ID, "deployment-exact-current", "b")
	response = fixture.request(http.MethodPost, "/v1/deployments/"+current.TargetID+"/rollback", "rollback-exact-config",
		map[string]string{"sourceOperationId": source.ID})
	rollback := decode[domain.Operation](t, response)
	if response.StatusCode != http.StatusAccepted || rollback.Generation != current.Generation+1 {
		t.Fatalf("rollback status=%d operation=%#v", response.StatusCode, rollback)
	}
	restored, err := fixture.store.GetDeploymentForOperation(t.Context(), rollback.ID)
	if err != nil || !bytes.Equal(restored.ConfigRaw, sourceSnapshot.ConfigRaw) || restored.Image != sourceSnapshot.Image {
		t.Fatalf("rollback regenerated source snapshot: err=%v image=%q raw=%s", err, restored.Image, restored.ConfigRaw)
	}
}

func TestDeploymentRollbackRejectsClosedFieldCurrentAndCrossDeploymentSources(t *testing.T) {
	fixture := newDeploymentRollbackAPI(t)
	admin, project, environment, application := rollbackResources(t, fixture, "adversarial")
	source := submitRollbackFixtureDeployment(t, fixture, environment.ID, application.ID, "deployment-source-adversarial", "a")
	completeRollbackSource(t, fixture, source)
	current := submitRollbackFixtureDeployment(t, fixture, environment.ID, application.ID, "deployment-current-adversarial", "b")

	response := fixture.request(http.MethodPost, "/v1/deployments/"+current.TargetID+"/rollback", "deployment-rollback-closed-fields",
		map[string]any{"sourceOperationId": source.ID, "image": "attacker.invalid/root@sha256:" + strings.Repeat("f", 64), "environmentId": environment.ID})
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest || !bytes.Contains(body, []byte(`"code":"InvalidJSON"`)) ||
		bytes.Contains(body, []byte("attacker.invalid")) {
		t.Fatalf("closed fields status=%d body=%s", response.StatusCode, body)
	}

	response = fixture.request(http.MethodPost, "/v1/deployments/"+current.TargetID+"/rollback", "deployment-rollback-current-source",
		map[string]string{"sourceOperationId": current.ID})
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusConflict || problem.Code != "DeploymentRollbackSourceNotEligible" {
		t.Fatalf("current source status=%d problem=%#v", response.StatusCode, problem)
	}

	otherApplicationResult, err := fixture.store.CreateApplication(t.Context(), admin.ID, "other-app", "other-app", domain.CreateApplication{ProjectID: project.ID, Name: "Other", Slug: "other"})
	if err != nil {
		t.Fatal(err)
	}
	other := submitRollbackFixtureDeployment(t, fixture, environment.ID, otherApplicationResult.Value.ID, "deployment-other", "d")
	completeRollbackSource(t, fixture, other)
	response = fixture.request(http.MethodPost, "/v1/deployments/"+current.TargetID+"/rollback", "deployment-rollback-cross-target",
		map[string]string{"sourceOperationId": other.ID})
	problem = decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusNotFound || problem.Code != "NotFound" {
		t.Fatalf("cross target status=%d problem=%#v", response.StatusCode, problem)
	}
}
