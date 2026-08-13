package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	"github.com/kuberploy/kuberploy/internal/runtimeview"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

type fakeRuntimeView struct {
	snapshot runtimeview.LogSnapshot
	events   runtimeview.EventSnapshot
	rollout  runtimeview.RolloutStatus
	err      error
	requests []any
}

func (f *fakeRuntimeView) Rollout(_ context.Context, deploymentID string) (runtimeview.RolloutStatus, error) {
	f.requests = append(f.requests, deploymentID)
	return f.rollout, f.err
}

type fakeRuntimeStream struct {
	events <-chan runtimeview.StreamEvent
}

type runtimeHTTPReadiness struct{ err error }

func (p *runtimeHTTPReadiness) Probe(context.Context) error { return p.err }

func (s fakeRuntimeStream) Channel() <-chan runtimeview.StreamEvent { return s.events }
func (s fakeRuntimeStream) Close()                                  {}

func (f *fakeRuntimeView) Snapshot(_ context.Context, request runtimeview.SnapshotRequest) (runtimeview.LogSnapshot, error) {
	f.requests = append(f.requests, request)
	return f.snapshot, f.err
}

func (f *fakeRuntimeView) Events(_ context.Context, request runtimeview.EventRequest) (runtimeview.EventSnapshot, error) {
	f.requests = append(f.requests, request)
	return f.events, f.err
}

func (f *fakeRuntimeView) Follow(_ context.Context, request runtimeview.FollowRequest) (httpapi.RuntimeLogStream, error) {
	f.requests = append(f.requests, request)
	if f.err != nil {
		return nil, f.err
	}
	events := make(chan runtimeview.StreamEvent, 2)
	events <- runtimeview.StreamEvent{Type: runtimeview.StreamHeartbeat, At: time.Date(2026, 8, 9, 1, 2, 3, 0, time.UTC)}
	events <- runtimeview.StreamEvent{Type: runtimeview.StreamTerminal, Terminal: &runtimeview.StreamTerminalPayload{Code: "SessionExpired", Detail: "The bounded log session ended."}, At: time.Date(2026, 8, 9, 1, 17, 3, 0, time.UTC)}
	close(events)
	return fakeRuntimeStream{events: events}, nil
}

func newRuntimeAPI(t *testing.T, runtime httpapi.RuntimeViewService) *apiFixture {
	t.Helper()
	st := memory.New()
	options := httpapi.Options{Store: st, BootstrapToken: "one-time-secret", Version: "test", Runtime: runtime, HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000)}
	if runtime != nil {
		options.RuntimeReadiness = &runtimeHTTPReadiness{}
	}
	srv := httptest.NewServer(httpapi.New(options))
	jar, _ := cookiejar.New(nil)
	fixture := &apiFixture{t: t, server: srv, client: &http.Client{Jar: jar}, store: st}
	t.Cleanup(srv.Close)
	return fixture
}

func TestRuntimeCapabilityFailsClosedWithoutRemovingAPIReadiness(t *testing.T) {
	probe := &runtimeHTTPReadiness{err: errors.New("Kubernetes API unavailable")}
	runtime := &fakeRuntimeView{}
	st := memory.New()
	server := httptest.NewServer(httpapi.New(httpapi.Options{Store: st, BootstrapToken: "one-time-secret", Runtime: runtime, RuntimeReadiness: probe, HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000)}))
	defer server.Close()
	jar, _ := cookiejar.New(nil)
	client := server.Client()
	client.Jar = jar
	fixture := &apiFixture{t: t, server: server, client: client, store: st}
	fixture.bootstrap()
	response := fixture.request(http.MethodGet, "/v1/capabilities", "", nil)
	capabilities := decode[struct {
		Features      map[string]bool   `json:"features"`
		FeatureStates map[string]string `json:"featureStates"`
	}](t, response)
	if response.StatusCode != http.StatusOK || capabilities.Features["logs"] || capabilities.FeatureStates["git"] != "disabled" {
		t.Fatalf("stale runtime capability status=%d features=%#v", response.StatusCode, capabilities.Features)
	}
	response = fixture.request(http.MethodGet, "/readyz", "", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("stale optional runtime removed API readiness: status=%d", response.StatusCode)
	}
	response.Body.Close()
}

func createRuntimeDeployment(t *testing.T, fixture *apiFixture) (domain.Application, domain.Deployment) {
	t.Helper()
	fixture.bootstrap()
	response := fixture.request(http.MethodPost, "/v1/projects", "runtime-project", map[string]string{"name": "Runtime"})
	project := decode[domain.Project](t, response)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("project status=%d", response.StatusCode)
	}
	response = fixture.request(http.MethodPost, "/v1/environments", "runtime-environment", map[string]string{"projectId": project.ID, "name": "Production"})
	environment := decode[domain.Environment](t, response)
	response = fixture.request(http.MethodPost, "/v1/applications", "runtime-application", map[string]string{"projectId": project.ID, "name": "API"})
	application := decode[domain.Application](t, response)
	response = fixture.request(http.MethodPost, "/v1/deployments", "runtime-deployment", map[string]any{
		"environmentId": environment.ID,
		"applicationId": application.ID,
		"image":         "registry.example.test/api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"runtime": map[string]any{
			"replicas":  1,
			"ports":     []map[string]any{{"name": "http", "containerPort": 8080, "protocol": "TCP"}},
			"resources": map[string]any{"requests": map[string]string{"cpu": "50m", "memory": "100Mi"}},
		},
	})
	operation := decode[domain.Operation](t, response)
	if response.StatusCode != http.StatusAccepted || operation.TargetID == "" {
		t.Fatalf("deployment operation status=%d body=%#v", response.StatusCode, operation)
	}
	response = fixture.request(http.MethodGet, "/v1/deployments", "", nil)
	deployments := decode[struct {
		Items []domain.Deployment `json:"items"`
	}](t, response)
	if len(deployments.Items) != 1 {
		t.Fatalf("deployments=%#v", deployments.Items)
	}
	return application, deployments.Items[0]
}

func TestRuntimeSnapshotEventsAndWorkloadsAreScopedAuditedAndNoStore(t *testing.T) {
	now := time.Date(2026, 8, 9, 1, 2, 3, 0, time.UTC)
	runtime := &fakeRuntimeView{
		snapshot: runtimeview.LogSnapshot{Lines: []runtimeview.LogLine{{Type: "line", Message: "ready"}}, Sources: []runtimeview.LogSource{}, Bytes: 5, ObservedAt: now},
		events:   runtimeview.EventSnapshot{Items: []runtimeview.RuntimeEvent{{ID: "event-1", Type: "Normal", Reason: "Ready", Message: "ready", ObjectKind: "Pod", ObjectName: "api-1", Count: 1, FirstSeen: now, LastSeen: now}}, ObservedAt: now},
	}
	fixture := newRuntimeAPI(t, runtime)
	application, deployment := createRuntimeDeployment(t, fixture)
	baselineAudits := fixture.store.AuditCount()

	response := fixture.request(http.MethodGet, "/v1/capabilities", "", nil)
	capabilities := decode[struct {
		Features map[string]bool `json:"features"`
	}](t, response)
	if !capabilities.Features["logs"] {
		t.Fatal("configured runtime view was not advertised")
	}

	response = fixture.request(http.MethodGet, "/v1/applications/"+application.ID+"/workloads", "", nil)
	workloads := decode[struct {
		Items []struct {
			ID, Kind, Namespace string
		} `json:"items"`
	}](t, response)
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "private, no-store" || len(workloads.Items) != 1 || workloads.Items[0].ID != deployment.ID || workloads.Items[0].Kind != "Deployment" || workloads.Items[0].Namespace == "" {
		t.Fatalf("workloads status=%d cache=%q body=%#v", response.StatusCode, response.Header.Get("Cache-Control"), workloads)
	}

	response = fixture.request(http.MethodGet, "/v1/workloads/"+deployment.ID+"/logs?tailLines=25&pod=api-7f9-abc&revision=42&container=application&limitBytes=4096", "", nil)
	snapshot := decode[runtimeview.LogSnapshot](t, response)
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "private, no-store" || len(snapshot.Lines) != 1 {
		t.Fatalf("logs status=%d cache=%q body=%#v", response.StatusCode, response.Header.Get("Cache-Control"), snapshot)
	}
	request, ok := runtime.requests[0].(runtimeview.SnapshotRequest)
	if !ok || request.Target.ID != deployment.ID || request.Target.Kind != runtimeview.TargetDeployment || request.Options.TailLines != 25 || request.Options.Pod != "api-7f9-abc" || request.Options.Revision != "42" || request.Options.Container != "application" || request.Options.LimitBytes != 4096 || !request.Options.Timestamps || request.Options.Follow {
		t.Fatalf("unsafe runtime request=%#v", runtime.requests[0])
	}

	response = fixture.request(http.MethodGet, "/v1/workloads/"+deployment.ID+"/events?limit=10", "", nil)
	events := decode[runtimeview.EventSnapshot](t, response)
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "private, no-store" || len(events.Items) != 1 {
		t.Fatalf("events status=%d cache=%q body=%#v", response.StatusCode, response.Header.Get("Cache-Control"), events)
	}
	if fixture.store.AuditCount() != baselineAudits+2 {
		t.Fatalf("runtime reads were not audited exactly once: before=%d after=%d", baselineAudits, fixture.store.AuditCount())
	}

	response = fixture.request(http.MethodGet, "/v1/workloads/"+deployment.ID+"/logs?namespace=attacker", "", nil)
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusUnprocessableEntity || problem.Code != "ValidationFailed" || fixture.store.AuditCount() != baselineAudits+2 {
		t.Fatalf("invalid query reached audit/runtime: status=%d problem=%#v", response.StatusCode, problem)
	}
}

func TestDeploymentStatusIncludesExactBoundedKubernetesRollout(t *testing.T) {
	now := time.Date(2026, 8, 13, 1, 2, 3, 0, time.UTC)
	runtime := &fakeRuntimeView{rollout: runtimeview.RolloutStatus{DesiredReplicas: 3, ReadyReplicas: 2, ObservedAt: now,
		Conditions: []runtimeview.DeploymentCondition{{Type: "Progressing", Status: "True", Reason: "ReplicaSetUpdated", LastTransitionTime: &now}}}}
	fixture := newRuntimeAPI(t, runtime)
	_, deployment := createRuntimeDeployment(t, fixture)

	response := fixture.request(http.MethodGet, "/v1/deployments/"+deployment.ID+"/status", "", nil)
	status := decode[domain.DeploymentStatus](t, response)
	if response.StatusCode != http.StatusOK || status.DesiredReplicas == nil || *status.DesiredReplicas != 3 ||
		status.ReadyReplicas == nil || *status.ReadyReplicas != 2 || status.RolloutObservedAt == nil ||
		len(status.RolloutConditions) != 1 || status.RolloutConditions[0].Type != "Progressing" || status.RolloutConditions[0].Reason != "ReplicaSetUpdated" {
		t.Fatalf("rollout status=%d body=%#v", response.StatusCode, status)
	}
}

func TestRuntimeFollowIsBoundedNDJSONAndScopeViolationsAreGeneric(t *testing.T) {
	runtime := &fakeRuntimeView{}
	fixture := newRuntimeAPI(t, runtime)
	_, deployment := createRuntimeDeployment(t, fixture)
	baselineAudits := fixture.store.AuditCount()

	response := fixture.request(http.MethodGet, "/v1/workloads/"+deployment.ID+"/logs/follow?tailLines=10&limitBytes=4096", "", nil)
	defer response.Body.Close()
	decoder := json.NewDecoder(response.Body)
	var heartbeat, terminal runtimeview.StreamEvent
	if err := decoder.Decode(&heartbeat); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&terminal); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "application/x-ndjson" || response.Header.Get("Cache-Control") != "private, no-store" || response.Header.Get("X-Accel-Buffering") != "no" || heartbeat.Type != runtimeview.StreamHeartbeat || terminal.Type != runtimeview.StreamTerminal || fixture.store.AuditCount() != baselineAudits+1 {
		t.Fatalf("follow response status=%d headers=%v events=%#v %#v", response.StatusCode, response.Header, heartbeat, terminal)
	}

	runtime.err = runtimeview.ErrScopeViolation
	response = fixture.request(http.MethodGet, "/v1/workloads/"+deployment.ID+"/logs", "", nil)
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusBadGateway || problem.Code != "RuntimeResponseRejected" || errors.Is(runtime.err, nil) {
		t.Fatalf("scope violation leaked or mapped incorrectly: status=%d problem=%#v", response.StatusCode, problem)
	}
}
