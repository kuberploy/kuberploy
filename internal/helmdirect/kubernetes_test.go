package helmdirect

import (
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
