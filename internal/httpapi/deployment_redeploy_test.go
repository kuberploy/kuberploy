package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

func reloadDocument(t *testing.T, raw []byte) (map[string]any, string) {
	t.Helper()
	parsed, _, diagnostics := appconfig.ParseAndValidate(raw)
	if len(diagnostics) != 0 {
		t.Fatalf("invalid saved configuration: %v", diagnostics)
	}
	runtime := parsed["spec"].(map[string]any)["runtime"].(map[string]any)
	marker, _ := runtime["configRevision"].(string)
	delete(runtime, "configRevision")
	return parsed, marker
}

func TestDeploymentReloadChangesOnlyRolloutMarkerAndReplaysExactly(t *testing.T) {
	f := newAPI(t)
	created, image := createConfigDeployment(t, f)
	configPath := "/v1/deployments/" + created.TargetID + "/config"
	bundle := decode[configBundleWire](t, f.request(http.MethodGet, configPath, "", nil))
	change := appconfig.Change{Mode: "jsonPatch", Patch: []appconfig.PatchOperation{
		{Op: "add", Path: "/spec/runtime/command", Value: []string{"/bin/sh"}},
		{Op: "add", Path: "/spec/runtime/args", Value: []string{"-c", "exec nginx -g 'daemon off;'"}},
		{Op: "add", Path: "/spec/runtime/workingDirectory", Value: "/tmp"},
		{Op: "replace", Path: "/spec/runtime/resources/requests/cpu", Value: "500m"},
		{Op: "add", Path: "/spec/runtime/probes", Value: map[string]any{"readiness": map[string]any{"httpGet": map[string]any{"path": "/ready", "port": "http"}, "initialDelaySeconds": 3}}},
	}}
	previewResponse := configRequest(t, f, http.MethodPost, configPath+"/preview", "", change, map[string]string{"If-Match": bundle.ETag})
	preview := decode[previewWire](t, previewResponse)
	if previewResponse.StatusCode != http.StatusOK {
		t.Fatalf("preview status=%d", previewResponse.StatusCode)
	}
	saveResponse := configRequest(t, f, http.MethodPut, configPath, "reload-saved-config", change, map[string]string{"If-Match": bundle.ETag, "Preview-Token": preview.PreviewToken})
	saved := decode[domain.Operation](t, saveResponse)
	if saveResponse.StatusCode != http.StatusAccepted {
		t.Fatalf("save status=%d", saveResponse.StatusCode)
	}
	before, err := f.store.GetDeploymentForOperation(t.Context(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	want, originalMarker := reloadDocument(t, before.ConfigRaw)
	outboxBefore := f.store.OutboxCount()
	reloadPath := "/v1/deployments/" + created.TargetID + "/redeploy"
	firstResponse := f.request(http.MethodPost, reloadPath, "reload-first-key", nil)
	first := decode[domain.Operation](t, firstResponse)
	if firstResponse.StatusCode != http.StatusAccepted || first.ID == saved.ID || first.TargetID != before.ID || first.Generation != before.Generation+1 {
		t.Fatalf("reload status=%d operation=%+v", firstResponse.StatusCode, first)
	}
	firstSnapshot, err := f.store.GetDeploymentForOperation(t.Context(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	actual, firstMarker := reloadDocument(t, firstSnapshot.ConfigRaw)
	if firstMarker == "" || firstMarker == originalMarker || strings.Contains(firstMarker, "reload-first-key") {
		t.Fatal("reload did not create a new opaque rollout marker")
	}
	if !reflect.DeepEqual(actual, want) || firstSnapshot.Image != image || !reflect.DeepEqual(firstSnapshot.Runtime, before.Runtime) ||
		!reflect.DeepEqual(firstSnapshot.Route, before.Route) || !reflect.DeepEqual(firstSnapshot.Environment, before.Environment) {
		t.Fatal("reload changed semantic configuration, release image, runtime, route, or variables")
	}
	if f.store.OutboxCount() != outboxBefore+1 {
		t.Fatal("reload did not enqueue exactly one operation")
	}
	assertReplay := func() {
		t.Helper()
		response := f.request(http.MethodPost, reloadPath, "reload-first-key", nil)
		replayed := decode[domain.Operation](t, response)
		if response.StatusCode != http.StatusAccepted || response.Header.Get("Idempotent-Replay") != "true" || replayed.ID != first.ID {
			t.Fatalf("reload replay status=%d operation=%+v", response.StatusCode, replayed)
		}
		snapshot, snapshotErr := f.store.GetDeploymentForOperation(t.Context(), replayed.ID)
		if snapshotErr != nil || !reflect.DeepEqual(snapshot.ConfigRaw, firstSnapshot.ConfigRaw) {
			t.Fatal("reload replay replaced its immutable marker or configuration snapshot")
		}
	}
	assertReplay()
	if f.store.OutboxCount() != outboxBefore+1 {
		t.Fatal("replayed reload created another operation")
	}
	secondResponse := f.request(http.MethodPost, reloadPath, "reload-second-key", nil)
	second := decode[domain.Operation](t, secondResponse)
	secondSnapshot, err := f.store.GetDeploymentForOperation(t.Context(), second.ID)
	if secondResponse.StatusCode != http.StatusAccepted || err != nil || second.ID == first.ID || second.Generation != first.Generation+1 {
		t.Fatalf("second reload status=%d operation=%+v err=%v", secondResponse.StatusCode, second, err)
	}
	secondConfig, secondMarker := reloadDocument(t, secondSnapshot.ConfigRaw)
	if secondMarker == firstMarker || !reflect.DeepEqual(secondConfig, want) || !reflect.DeepEqual(secondSnapshot.Runtime, before.Runtime) || secondSnapshot.Image != image {
		t.Fatal("a new reload did not change only the rollout marker")
	}
	assertReplay()
	if f.store.OutboxCount() != outboxBefore+2 {
		t.Fatal("late replay created another operation")
	}
}

type invalidReloadConfigStore struct {
	*memory.Store
	deploymentID string
	raw          []byte
}

func (s *invalidReloadConfigStore) GetDeploymentForActor(ctx context.Context, actor, id string) (domain.Deployment, error) {
	deployment, err := s.Store.GetDeploymentForActor(ctx, actor, id)
	if err == nil && id == s.deploymentID {
		deployment.ConfigRaw = append([]byte(nil), s.raw...)
	}
	return deployment, err
}

func TestDeploymentReloadRejectsInvalidSavedConfiguration(t *testing.T) {
	for _, raw := range []string{"[invalid", `{"apiVersion":"config.kuberploy.io/v1alpha1","kind":"AppConfig","spec":{}}`} {
		t.Run(raw, func(t *testing.T) {
			f := newAPI(t)
			created, _ := createConfigDeployment(t, f)
			outboxBefore := f.store.OutboxCount()
			f.server.Close()
			f.server = httptest.NewServer(httpapi.New(httpapi.Options{Store: &invalidReloadConfigStore{Store: f.store, deploymentID: created.TargetID, raw: []byte(raw)}, HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000)}))
			t.Cleanup(f.server.Close)
			response := f.request(http.MethodPost, "/v1/deployments/"+created.TargetID+"/redeploy", "invalid-reload", nil)
			problem := decode[httpapi.Problem](t, response)
			if response.StatusCode != http.StatusInternalServerError || problem.Code != "StoredConfigInvalid" || f.store.OutboxCount() != outboxBefore {
				t.Fatalf("invalid reload status=%d problem=%+v outbox=%d", response.StatusCode, problem, f.store.OutboxCount())
			}
		})
	}
}
