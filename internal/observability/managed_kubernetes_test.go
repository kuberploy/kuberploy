package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestManagedKubernetesObserverUsesOnlyExactGetsAndDigestsSpecs(t *testing.T) {
	t.Parallel()
	profile, _ := json.Marshal(map[string]any{
		"metadata":  map[string]any{"name": ManagedMonitoringProfileName, "namespace": ManagedMonitoringNamespace, "uid": "profile-uid", "resourceVersion": "1"},
		"immutable": true, "data": expectedManagedProfile(managedChartVersionForTest),
	})
	operator, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"name": ManagedMonitoringOperatorName, "namespace": ManagedMonitoringNamespace, "uid": "operator-uid", "resourceVersion": "2", "generation": 5},
		"spec": map[string]any{"replicas": 1, "template": map[string]any{"spec": map[string]any{"containers": []any{
			map[string]any{"name": ManagedMonitoringOperatorContainer, "image": ManagedMonitoringOperatorImage, "args": []string{"--one"}},
			map[string]any{"name": "unrelated-sidecar", "image": "example.invalid/sidecar@sha256:deadbeef"},
		}}}},
		"status": map[string]any{"observedGeneration": 5, "availableReplicas": 1},
	})
	ruleSpec := map[string]any{"groups": []any{map[string]any{"name": "kuberploy.service.metrics", "rules": []any{map[string]any{"record": managedMetricSeries[0], "expr": "vector(1)"}}}}}
	rule, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"name": ManagedMonitoringRuleName, "namespace": ManagedMonitoringNamespace, "uid": "rule-uid", "resourceVersion": "3", "generation": 8},
		"spec":     ruleSpec,
	})
	prometheus, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"name": ManagedMonitoringPrometheusName, "namespace": ManagedMonitoringNamespace, "uid": "prometheus-uid", "resourceVersion": "4", "generation": 2},
		"spec": map[string]any{
			"overrideHonorLabels":         true,
			"arbitraryFSAccessThroughSMs": map[string]any{"deny": false},
		},
	})
	bodies := map[string][]byte{managedKubernetesPaths[0]: profile, managedKubernetesPaths[1]: operator, managedKubernetesPaths[2]: rule, managedKubernetesPaths[3]: prometheus}
	seen := map[string]int{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer observer-token-123" || r.Header.Get("Accept") != "application/json" {
			t.Errorf("unsafe request method=%s authorization=%q accept=%q", r.Method, r.Header.Get("Authorization"), r.Header.Get("Accept"))
		}
		body, ok := bodies[r.URL.Path]
		if !ok || r.URL.RawQuery != "" {
			t.Errorf("unexpected Kubernetes path %q?%s", r.URL.Path, r.URL.RawQuery)
			http.NotFound(w, r)
			return
		}
		seen[r.URL.Path]++
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(body)
	}))
	defer server.Close()
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("observer-token-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	observer := &InClusterManagedMonitoringObserver{baseURL: server.URL, http: server.Client(), tokenPath: tokenPath}
	snapshot, err := observer.ObserveManagedMonitoring(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range managedKubernetesPaths {
		if seen[path] != 1 {
			t.Fatalf("path %q calls=%d", path, seen[path])
		}
	}
	if snapshot.OperatorArgumentsSHA256 != digestJSON([]string{"--one"}) || snapshot.RuleSpecSHA256 != digestJSON(ruleSpec) {
		t.Fatalf("unexpected digests operator=%q rule=%q", snapshot.OperatorArgumentsSHA256, snapshot.RuleSpecSHA256)
	}
	if !snapshot.ProfileImmutable || snapshot.OperatorGeneration != 5 || snapshot.RuleGeneration != 8 || snapshot.PrometheusGeneration != 2 || !snapshot.PrometheusOverrideHonorLabels || snapshot.PrometheusIgnoreNamespaceSelectors || snapshot.PrometheusArbitraryFSAccessDeny {
		t.Fatalf("snapshot=%#v", snapshot)
	}
}

func TestManagedKubernetesObserverRejectsUnapprovedPathsAndUnsafeResponses(t *testing.T) {
	t.Parallel()
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("observer-token-123"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	observer := &InClusterManagedMonitoringObserver{baseURL: server.URL, http: server.Client(), tokenPath: tokenPath}
	var destination map[string]any
	if err := observer.getJSON(context.Background(), "/api/v1/secrets", &destination); err != ErrUnavailable {
		t.Fatalf("unapproved path error=%v", err)
	}
	if err := observer.getJSON(context.Background(), managedKubernetesPaths[0], &destination); err != ErrUnsafeResponse {
		t.Fatalf("unsafe content type error=%v", err)
	}
}
