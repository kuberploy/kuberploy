package helmdirect

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestInClusterApplicationAPIObserveParsesOwnershipAndReconcileTime(t *testing.T) {
	wantTime := time.Date(2026, 9, 11, 8, 9, 10, 123456789, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			t.Fatalf("method=%s", request.Method)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"metadata":{"labels":{"kuberploy.io/environment-id":"33333333-3333-4333-8333-333333333333"}},"status":{"sync":{"status":"Synced"},"health":{"status":"Healthy"},"operationState":{"phase":"Succeeded"},"reconciledAt":"2026-09-11T08:09:10.123456789Z"}}`))
	}))
	t.Cleanup(server.Close)
	tokenPath := t.TempDir() + "/token"
	if err := os.WriteFile(tokenPath, []byte("test-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	api := &InClusterApplicationAPI{baseURL: server.URL, tokenPath: tokenPath, http: server.Client()}
	state, err := api.Observe(t.Context(), ArgoNamespace, "kp-h-test")
	if err != nil {
		t.Fatal(err)
	}
	if !state.Exists || state.EnvironmentID != "33333333-3333-4333-8333-333333333333" || state.ReconciledAt == nil || !state.ReconciledAt.Equal(wantTime) {
		t.Fatalf("state=%+v", state)
	}
}

func TestInClusterApplicationAPIObserveParsesGeneration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"metadata":{"annotations":{"kuberploy.io/helm-generation":"7"}},"status":{"sync":{"status":"Synced"},"health":{"status":"Healthy"}}}`))
	}))
	t.Cleanup(server.Close)
	tokenPath := t.TempDir() + "/token"
	if err := os.WriteFile(tokenPath, []byte("test-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	api := &InClusterApplicationAPI{baseURL: server.URL, tokenPath: tokenPath, http: server.Client()}
	state, err := api.Observe(t.Context(), ArgoNamespace, "kp-h-test")
	if err != nil {
		t.Fatal(err)
	}
	if !state.Exists || state.Generation != 7 {
		t.Fatalf("state=%+v", state)
	}
}

func TestInClusterApplicationAPIObserveLeavesInvalidReconcileTimePending(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"metadata":{"labels":{}},"status":{"sync":{"status":"Synced"},"health":{"status":"Healthy"},"reconciledAt":"invalid"}}`))
	}))
	t.Cleanup(server.Close)
	tokenPath := t.TempDir() + "/token"
	if err := os.WriteFile(tokenPath, []byte("test-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	api := &InClusterApplicationAPI{baseURL: server.URL, tokenPath: tokenPath, http: server.Client()}
	state, err := api.Observe(t.Context(), ArgoNamespace, "kp-h-test")
	if err != nil {
		t.Fatal(err)
	}
	if !state.Exists || state.ReconciledAt != nil {
		t.Fatalf("state=%+v", state)
	}
}

func TestInClusterApplicationAPIObserveKeepsExactComparedSource(t *testing.T) {
	revision := renderFixture(SourceHelmRepository)
	want := observedRevisionState(revision)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(map[string]any{
			"metadata": map[string]any{"labels": map[string]string{
				"kuberploy.io/environment-id": revision.Target.EnvironmentID,
				"kuberploy.io/application-id": revision.Target.ApplicationID,
				"kuberploy.io/project-id":     revision.Target.ProjectID}},
			"spec": map[string]any{"project": HelmAppProject, "source": map[string]any{"repoURL": "https://unobserved.example.com"}},
			"status": map[string]any{
				"sync":         map[string]any{"status": "Synced", "comparedTo": map[string]any{"source": want.ObservedSource, "destination": want.ObservedDestination}},
				"health":       map[string]any{"status": "Healthy"},
				"reconciledAt": revision.UpdatedAt.Add(-time.Minute).Format(time.RFC3339),
			},
		})
	}))
	t.Cleanup(server.Close)
	tokenPath := t.TempDir() + "/token"
	if err := os.WriteFile(tokenPath, []byte("test-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	api := &InClusterApplicationAPI{baseURL: server.URL, tokenPath: tokenPath, http: server.Client()}
	state, err := api.Observe(t.Context(), ArgoNamespace, "kp-h-test")
	if err != nil {
		t.Fatal(err)
	}
	manifest, renderErr := RenderApplication(revision, ArgoNamespace)
	if renderErr != nil {
		t.Fatal(renderErr)
	}
	if !state.observes(revision, manifest) {
		t.Fatal("exact compared source/destination/ownership were not parsed")
	}
	if state.ObservedSource["repoURL"] == "https://unobserved.example.com" {
		t.Fatal("current spec substituted for Argo's observed source")
	}
}
