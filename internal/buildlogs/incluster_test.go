package buildlogs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kuberploy/kuberploy/internal/builds"
)

func testInClusterClient(t *testing.T, server *httptest.Server) *InClusterClient {
	t.Helper()
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("service-account.header.signature"), 0o600); err != nil {
		t.Fatal(err)
	}
	httpClient := server.Client()
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }
	return &InClusterClient{baseURL: server.URL, http: httpClient, tokenPath: tokenPath}
}

func TestInClusterClientUsesOnlyExactGETJobPodAndAgentLogPaths(t *testing.T) {
	authorized, job, pod := buildLogFixture(t)
	jobName := authorized.Attempt.JobName
	var logCalls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.Header.Get("Authorization") != "Bearer service-account.header.signature" || request.Header.Get("Accept-Encoding") != "identity" {
			t.Errorf("unsafe Kubernetes request: method=%s headers=%v", request.Method, request.Header)
		}
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/apis/batch/v1/namespaces/kuberploy-build-dind/jobs/" + jobName:
			_ = json.NewEncoder(response).Encode(job)
		case "/api/v1/namespaces/kuberploy-build-dind/pods":
			if request.URL.Query().Get("limit") != "2" || request.URL.Query().Get("labelSelector") != "kuberploy.io/build-operation=11111111111141118111111111111111,kuberploy.io/build-generation=2" {
				t.Errorf("unbounded selector: %s", request.URL.RawQuery)
			}
			listedPod := cloneLogObject(t, pod)
			delete(listedPod, "apiVersion")
			delete(listedPod, "kind")
			_ = json.NewEncoder(response).Encode(map[string]any{"apiVersion": "v1", "kind": "PodList", "metadata": map[string]any{"continue": ""}, "items": []any{listedPod}})
		case "/api/v1/namespaces/kuberploy-build-dind/pods/build-pod-aaaaaaaa":
			_ = json.NewEncoder(response).Encode(pod)
		case "/api/v1/namespaces/kuberploy-build-dind/pods/build-pod-aaaaaaaa/log":
			logCalls.Add(1)
			if request.URL.Query().Get("container") != "agent" || request.URL.Query().Get("tailLines") != "200" || request.URL.Query().Get("limitBytes") != "1024" || request.URL.Query().Get("follow") != "false" {
				t.Errorf("unsafe log query: %s", request.URL.RawQuery)
			}
			response.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(response, "bounded logs\n")
		default:
			t.Errorf("unexpected Kubernetes path: %s", request.URL.Path)
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	client := testInClusterClient(t, server)
	observedJob, err := client.GetBuildJob(t.Context(), authorized.Attempt.JobNamespace, jobName)
	if err != nil || objectIdentity(observedJob, "uid") == "" {
		t.Fatalf("job=%#v err=%v", observedJob, err)
	}
	verified, err := builds.VerifyObservedBuildJob(authorized.Attempt, observedJob)
	if err != nil {
		t.Fatal(err)
	}
	pods, err := client.ListBuildJobPods(t.Context(), JobPodQuery{Namespace: verified.Namespace, JobName: verified.Name, JobUID: verified.UID, OperationLabel: verified.OperationLabel, GenerationLabel: verified.GenerationLabel})
	if err != nil || len(pods) != 1 {
		t.Fatalf("pods=%#v err=%v", pods, err)
	}
	reader, err := client.OpenBuilderAgentLogs(t.Context(), ExactPodRef{Namespace: verified.Namespace, Name: "build-pod-aaaaaaaa", UID: "55555555-5555-4555-8555-555555555555"}, PodLogOptions{TailLines: 200, LimitBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || string(content) != "bounded logs\n" || logCalls.Load() != 1 {
		t.Fatalf("content=%q logCalls=%d err=%v", content, logCalls.Load(), err)
	}
}

func cloneLogObject(t *testing.T, object map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	var cloned map[string]any
	if err = json.Unmarshal(encoded, &cloned); err != nil {
		t.Fatal(err)
	}
	return cloned
}

func TestInClusterClientProbeUsesOnlyFixedDiscovery(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.Method != http.MethodGet || request.URL.Path != "/api" || request.URL.RawQuery != "" {
			t.Errorf("unsafe readiness request: %s %s", request.Method, request.URL.String())
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"kind":"APIVersions","versions":["v1"]}`)
	}))
	defer server.Close()
	client := testInClusterClient(t, server)
	if err := client.Probe(t.Context()); err != nil || requests.Load() != 1 {
		t.Fatalf("probe requests=%d err=%v", requests.Load(), err)
	}
}

func TestInClusterClientRejectsPaginationReplacementRedirectsAndForbiddenPaths(t *testing.T) {
	t.Run("partial or substituted Pod TypeMeta", func(t *testing.T) {
		_, _, fixture := buildLogFixture(t)
		for name, mutate := range map[string]func(map[string]any){
			"partial": func(pod map[string]any) { delete(pod, "kind") },
			"wrong":   func(pod map[string]any) { pod["kind"] = "Secret" },
		} {
			t.Run(name, func(t *testing.T) {
				pod := cloneLogObject(t, fixture)
				mutate(pod)
				server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
					response.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(response).Encode(map[string]any{"apiVersion": "v1", "kind": "PodList", "metadata": map[string]any{}, "items": []any{pod}})
				}))
				defer server.Close()
				client := testInClusterClient(t, server)
				_, err := client.ListBuildJobPods(t.Context(), JobPodQuery{Namespace: "kuberploy-build-dind", JobName: "job", JobUID: "44444444-4444-4444-8444-444444444444", OperationLabel: strings.Repeat("1", 32), GenerationLabel: "2"})
				if !errors.Is(err, ErrScopeViolation) {
					t.Fatalf("invalid TypeMeta accepted: %v", err)
				}
			})
		}
	})
	t.Run("pagination", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			response.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(response, `{"apiVersion":"v1","kind":"PodList","metadata":{"continue":"opaque"},"items":[]}`)
		}))
		defer server.Close()
		client := testInClusterClient(t, server)
		_, err := client.ListBuildJobPods(t.Context(), JobPodQuery{Namespace: "kuberploy-build-dind", JobName: "job", JobUID: "44444444-4444-4444-8444-444444444444", OperationLabel: strings.Repeat("1", 32), GenerationLabel: "2"})
		if !errors.Is(err, ErrScopeViolation) {
			t.Fatalf("pagination accepted: %v", err)
		}
	})
	t.Run("Pod UID replacement", func(t *testing.T) {
		_, _, pod := buildLogFixture(t)
		pod["metadata"].(map[string]any)["uid"] = "99999999-9999-4999-8999-999999999999"
		var logCalls atomic.Int32
		server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			response.Header().Set("Content-Type", "application/json")
			if strings.HasSuffix(request.URL.Path, "/log") {
				logCalls.Add(1)
			}
			_ = json.NewEncoder(response).Encode(pod)
		}))
		defer server.Close()
		client := testInClusterClient(t, server)
		_, err := client.OpenBuilderAgentLogs(t.Context(), ExactPodRef{Namespace: "kuberploy-build-dind", Name: "build-pod-aaaaaaaa", UID: "55555555-5555-4555-8555-555555555555"}, PodLogOptions{TailLines: 1, LimitBytes: 1})
		if !errors.Is(err, ErrGone) || logCalls.Load() != 0 {
			t.Fatalf("replacement race reached logs: err=%v calls=%d", err, logCalls.Load())
		}
	})
	t.Run("Pod replaced while opening logs", func(t *testing.T) {
		_, _, oldPod := buildLogFixture(t)
		newPod := map[string]any{}
		encoded, _ := json.Marshal(oldPod)
		_ = json.Unmarshal(encoded, &newPod)
		newPod["metadata"].(map[string]any)["uid"] = "99999999-9999-4999-8999-999999999999"
		var podReads atomic.Int32
		server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			if strings.HasSuffix(request.URL.Path, "/log") {
				response.Header().Set("Content-Type", "text/plain")
				_, _ = io.WriteString(response, "replacement log must stay unread\n")
				return
			}
			response.Header().Set("Content-Type", "application/json")
			if podReads.Add(1) == 1 {
				_ = json.NewEncoder(response).Encode(oldPod)
				return
			}
			_ = json.NewEncoder(response).Encode(newPod)
		}))
		defer server.Close()
		client := testInClusterClient(t, server)
		reader, err := client.OpenBuilderAgentLogs(t.Context(), ExactPodRef{Namespace: "kuberploy-build-dind", Name: "build-pod-aaaaaaaa", UID: "55555555-5555-4555-8555-555555555555"}, PodLogOptions{TailLines: 1, LimitBytes: 1024})
		if !errors.Is(err, ErrGone) || reader != nil || podReads.Load() != 2 {
			t.Fatalf("replacement response escaped: reader=%T reads=%d err=%v", reader, podReads.Load(), err)
		}
	})
	t.Run("redirect", func(t *testing.T) {
		var destinationCalls atomic.Int32
		destination := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { destinationCalls.Add(1) }))
		defer destination.Close()
		server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			http.Redirect(response, request, destination.URL, http.StatusFound)
		}))
		defer server.Close()
		client := testInClusterClient(t, server)
		_, err := client.GetBuildPod(t.Context(), "kuberploy-build-dind", "build-pod-aaaaaaaa")
		if err == nil || destinationCalls.Load() != 0 {
			t.Fatalf("redirect followed: err=%v calls=%d", err, destinationCalls.Load())
		}
	})
	for _, path := range []string{
		"/api/v1/namespaces/kuberploy-build-dind/secrets/registry-credentials",
		"/api/v1/namespaces/kuberploy-build-dind/pods/build-pod-aaaaaaaa/exec",
		"/api/v1/namespaces/kuberploy-build-dind/pods/build-pod-aaaaaaaa/proxy",
		"/apis/apps/v1/namespaces/kuberploy-build-dind/deployments/x",
		"/api/v1/namespaces/kuberploy-build-dind/pods/build-pod-aaaaaaaa/portforward",
	} {
		if allowedBuildLogPath(path) {
			t.Fatalf("forbidden path accepted: %s", path)
		}
	}
	if !allowedBuildLogPath("/api") || allowedBuildLogPath("/apis") || allowedBuildLogPath("/api/v1/secrets") {
		t.Fatal("readiness path allowlist is not exact")
	}
}

func TestInClusterClientBoundsJSONAndRejectsInvalidToken(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, strings.Repeat("x", int(buildLogMaxKubernetesJSON+1)))
	}))
	defer server.Close()
	client := testInClusterClient(t, server)
	if _, err := client.GetBuildPod(context.Background(), "kuberploy-build-dind", "build-pod-aaaaaaaa"); !errors.Is(err, ErrResponseLimitReached) {
		t.Fatalf("oversized JSON accepted: %v", err)
	}
	if err := os.WriteFile(client.tokenPath, []byte("bad token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetBuildPod(context.Background(), "kuberploy-build-dind", "build-pod-aaaaaaaa"); err == nil {
		t.Fatal("token containing whitespace was accepted")
	}
}
