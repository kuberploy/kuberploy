package runtimeview

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestInClusterClientUsesExactReadOnlyPathsUIDBindingAndBoundedQueries(t *testing.T) {
	const (
		namespace     = "kp-production"
		deploymentUID = "deployment-uid-1"
		replicaSetUID = "replicaset-uid-1"
		podUID        = "pod-uid-1"
	)
	var podGets atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer projected-token" {
			t.Errorf("missing projected bearer on %s", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("unexpected mutation method %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api":
			_, _ = io.WriteString(w, `{"kind":"APIVersions","versions":["v1"]}`)
		case "/apis/apps/v1/namespaces/" + namespace + "/deployments/kp-a-runtime":
			_, _ = io.WriteString(w, `{"metadata":{"name":"kp-a-runtime","namespace":"kp-production","uid":"deployment-uid-1"},"spec":{"selector":{"matchLabels":{"kuberploy.io/application":"33333333-3333-4333-8333-333333333333","app.kubernetes.io/name":"kuberploy-runtime"}}}}`)
		case "/apis/apps/v1/namespaces/" + namespace + "/replicasets":
			assertRuntimeSelector(t, r.URL.Query())
			_, _ = io.WriteString(w, `{"metadata":{},"items":[{"metadata":{"name":"kp-a-runtime-abc","namespace":"kp-production","uid":"replicaset-uid-1","annotations":{"deployment.kubernetes.io/revision":"7"},"ownerReferences":[{"uid":"deployment-uid-1","kind":"Deployment","controller":true}]},"status":{"readyReplicas":1}}]}`)
		case "/api/v1/namespaces/" + namespace + "/pods":
			assertRuntimeSelector(t, r.URL.Query())
			_, _ = io.WriteString(w, `{"metadata":{},"items":[`+testPodJSON+`]}`)
		case "/api/v1/namespaces/" + namespace + "/pods/kp-a-runtime-abc-1":
			podGets.Add(1)
			_, _ = io.WriteString(w, testPodJSON)
		case "/api/v1/namespaces/" + namespace + "/pods/kp-a-runtime-abc-1/log":
			if r.URL.Query().Get("container") != "application" || r.URL.Query().Get("tailLines") != "25" || r.URL.Query().Get("limitBytes") != "4096" || r.URL.Query().Get("timestamps") != "true" || r.URL.Query().Get("follow") != "false" {
				t.Errorf("unsafe log query: %s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "2026-08-09T01:02:03Z ready\n")
		case "/api/v1/namespaces/" + namespace + "/events":
			uid := strings.TrimPrefix(r.URL.Query().Get("fieldSelector"), "involvedObject.uid=")
			if uid == "" || r.URL.Query().Get("limit") != "2" {
				t.Errorf("unsafe event query: %s", r.URL.RawQuery)
			}
			_, _ = io.WriteString(w, `{"metadata":{},"items":[{"metadata":{"name":"event-1","namespace":"kp-production","uid":"event-uid-1"},"involvedObject":{"uid":"`+uid+`","kind":"Pod","name":"kp-a-runtime-abc-1"},"type":"Normal","reason":"Ready","message":"ready","count":1,"eventTime":"2026-08-09T01:02:03Z"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := testInClusterClient(t, server)
	if security := client.Security(); !security.TLSVerified || security.InsecureSkipTLSVerify {
		t.Fatalf("insecure client metadata: %#v", security)
	}
	if err := client.Probe(t.Context()); err != nil {
		t.Fatalf("probe: %v", err)
	}

	deployment, err := client.GetDeployment(t.Context(), namespace, "kp-a-runtime")
	if err != nil || deployment.UID != deploymentUID || len(deployment.Selector.MatchLabels) != 2 {
		t.Fatalf("deployment=%#v err=%v", deployment, err)
	}
	replicaSets, err := client.ListReplicaSets(t.Context(), namespace, deployment.Selector)
	if err != nil || len(replicaSets) != 1 || replicaSets[0].UID != replicaSetUID || replicaSets[0].Revision != "7" || !replicaSets[0].Ready {
		t.Fatalf("replicaSets=%#v err=%v", replicaSets, err)
	}
	pods, err := client.ListPods(t.Context(), namespace, deployment.Selector)
	if err != nil || len(pods) != 1 || pods[0].UID != podUID || !pods[0].Ready || len(pods[0].Containers) != 2 || pods[0].Containers[1].RestartCount != 2 {
		t.Fatalf("pods=%#v err=%v", pods, err)
	}
	reader, err := client.OpenPodLogs(t.Context(), PodLogRequest{Namespace: namespace, PodName: pods[0].Name, PodUID: podUID, Options: PodLogOptions{Container: "application", TailLines: 25, Timestamps: true, LimitBytes: 4096}})
	if err != nil {
		t.Fatal(err)
	}
	logBody, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || string(logBody) != "2026-08-09T01:02:03Z ready\n" || podGets.Load() != 1 {
		t.Fatalf("logs=%q podGets=%d err=%v", logBody, podGets.Load(), err)
	}
	events, err := client.ListEvents(t.Context(), namespace, EventQuery{InvolvedUIDs: []string{podUID}, Limit: 2})
	if err != nil || len(events) != 1 || events[0].InvolvedUID != podUID || events[0].UID != "event-uid-1" {
		t.Fatalf("events=%#v err=%v", events, err)
	}
}

func TestInClusterClientProbeFailsClosed(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"missing core version": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"kind":"APIVersions","versions":["v2"]}`)
		},
		"unauthorized": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "denied", http.StatusUnauthorized)
		},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewTLSServer(handler)
			defer server.Close()
			if err := testInClusterClient(t, server).Probe(t.Context()); err == nil {
				t.Fatal("unavailable Kubernetes discovery passed readiness")
			}
		})
	}
}

func TestInClusterClientRejectsCrossNamespaceObjectsAndReplacedPod(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/deployments/"):
			_, _ = io.WriteString(w, `{"metadata":{"name":"kp-a-runtime","namespace":"other","uid":"deployment-uid"},"spec":{"selector":{"matchLabels":{"kuberploy.io/application":"app"}}}}`)
		case strings.HasSuffix(r.URL.Path, "/pods/pod-1"):
			body := strings.Replace(testPodJSON, `"name":"kp-a-runtime-abc-1"`, `"name":"pod-1"`, 1)
			body = strings.Replace(body, `"uid":"pod-uid-1"`, `"uid":"replacement-uid"`, 1)
			_, _ = io.WriteString(w, body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := testInClusterClient(t, server)
	if _, err := client.GetDeployment(t.Context(), "kp-production", "kp-a-runtime"); !errors.Is(err, ErrScopeViolation) {
		t.Fatalf("cross-namespace deployment accepted: %v", err)
	}
	_, err := client.OpenPodLogs(t.Context(), PodLogRequest{Namespace: "kp-production", PodName: "pod-1", PodUID: "pod-uid-1", Options: PodLogOptions{Container: "application", TailLines: 1, LimitBytes: 64}})
	if !errors.Is(err, ErrGone) {
		t.Fatalf("replaced Pod accepted: %v", err)
	}
}

func testInClusterClient(t *testing.T, server *httptest.Server) *InClusterClient {
	t.Helper()
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("projected-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &InClusterClient{baseURL: server.URL, http: server.Client(), tokenPath: tokenPath}
}

func assertRuntimeSelector(t *testing.T, values url.Values) {
	t.Helper()
	if values.Get("labelSelector") != "app.kubernetes.io/name=kuberploy-runtime,kuberploy.io/application=33333333-3333-4333-8333-333333333333" || values.Get("limit") != "1001" {
		t.Errorf("unsafe selector query: %s", values.Encode())
	}
}

const testPodJSON = `{"metadata":{"name":"kp-a-runtime-abc-1","namespace":"kp-production","uid":"pod-uid-1","ownerReferences":[{"uid":"replicaset-uid-1","kind":"ReplicaSet","controller":true}]},"spec":{"initContainers":[{"name":"init"}],"containers":[{"name":"application"}]},"status":{"initContainerStatuses":[{"name":"init","restartCount":0}],"containerStatuses":[{"name":"application","restartCount":2}],"conditions":[{"type":"Ready","status":"True"}]}}`

func TestDecodeEventUsesSeriesTimestampAndCount(t *testing.T) {
	var object kubernetesEventObject
	if err := json.Unmarshal([]byte(`{"metadata":{"name":"e","namespace":"ns","uid":"event"},"involvedObject":{"uid":"pod","kind":"Pod","name":"pod"},"type":"Warning","reason":"BackOff","message":"retry","count":1,"firstTimestamp":"2026-08-09T00:00:00Z","series":{"count":4,"lastObservedTime":"2026-08-09T00:01:00Z"}}`), &object); err != nil {
		t.Fatal(err)
	}
	event, err := decodeEvent(object, "ns", "pod")
	if err != nil || event.Count != 4 || !event.LastSeen.Equal(time.Date(2026, 8, 9, 0, 1, 0, 0, time.UTC)) {
		t.Fatalf("event=%#v err=%v", event, err)
	}
}
