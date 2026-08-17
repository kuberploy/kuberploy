package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/buildlogs"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

type fakeBuildLogService struct {
	snapshot         buildlogs.Snapshot
	snapshotRequest  buildlogs.SnapshotRequest
	followRequest    buildlogs.FollowRequest
	followEvents     []buildlogs.StreamEvent
	err              error
	snapshotRequests int
	followRequests   int
}

type fakeBuildLogStream struct{ events <-chan buildlogs.StreamEvent }

func (s fakeBuildLogStream) Channel() <-chan buildlogs.StreamEvent { return s.events }
func (s fakeBuildLogStream) Close()                                {}

func (f *fakeBuildLogService) Snapshot(_ context.Context, request buildlogs.SnapshotRequest) (buildlogs.Snapshot, error) {
	f.snapshotRequests++
	f.snapshotRequest = request
	return f.snapshot, f.err
}

func (f *fakeBuildLogService) Follow(_ context.Context, request buildlogs.FollowRequest) (httpapi.BuildLogStream, error) {
	f.followRequests++
	f.followRequest = request
	if f.err != nil {
		return nil, f.err
	}
	events := make(chan buildlogs.StreamEvent, len(f.followEvents))
	for _, event := range f.followEvents {
		events <- event
	}
	close(events)
	return fakeBuildLogStream{events: events}, nil
}

type buildLogHTTPReadiness struct{ err error }

func (p *buildLogHTTPReadiness) Probe(context.Context) error { return p.err }

func newBuildLogAPI(t *testing.T, service httpapi.BuildLogService, readiness httpapi.ReadinessProbe) *apiFixture {
	t.Helper()
	st := memory.New()
	server := httptest.NewServer(httpapi.New(httpapi.Options{
		Store: st, BootstrapToken: "one-time-secret", Version: "test", Builds: &buildHTTPBackend{},
		BuildLogs: service, BuildLogReadiness: readiness, HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000),
	}))
	jar, _ := cookiejar.New(nil)
	fixture := &apiFixture{t: t, server: server, client: &http.Client{Jar: jar}, store: st}
	t.Cleanup(server.Close)
	return fixture
}

func TestBuildLogSnapshotIsOpaqueBoundedNoStoreAndSeparatelyAdvertised(t *testing.T) {
	now := time.Date(2026, 8, 9, 3, 4, 5, 0, time.UTC)
	service := &fakeBuildLogService{snapshot: buildlogs.Snapshot{
		Source: buildlogs.Source{ID: "build_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Ready: true},
		Lines:  []buildlogs.LogLine{{Type: "line", Message: "build ready", Source: buildlogs.Source{ID: "build_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Ready: true}}},
		Bytes:  11, ObservedAt: now,
	}}
	fixture := newBuildLogAPI(t, service, &buildLogHTTPReadiness{})
	actor := fixture.bootstrap()

	response := fixture.request(http.MethodGet, "/v1/capabilities", "", nil)
	capabilities := decode[struct {
		Features map[string]bool `json:"features"`
	}](t, response)
	if !capabilities.Features["buildLogs"] || capabilities.Features["logs"] {
		t.Fatalf("build/runtime log capabilities were conflated: %#v", capabilities.Features)
	}

	attemptID := "11111111-1111-4111-8111-111111111111"
	response = fixture.request(http.MethodGet, "/v1/builds/"+attemptID+"/logs?tailLines=25&limitBytes=4096&previous=true", "", nil)
	snapshot := decode[buildlogs.Snapshot](t, response)
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "private, no-store" || response.Header.Get("X-Content-Type-Options") != "nosniff" || snapshot.Source.ID != service.snapshot.Source.ID {
		t.Fatalf("snapshot status=%d headers=%v body=%#v", response.StatusCode, response.Header, snapshot)
	}
	request := service.snapshotRequest
	if service.snapshotRequests != 1 || request.Access.ActorID != actor.ID || request.Access.AttemptID != attemptID || request.RequestID == "" || request.Options.TailLines != 25 || request.Options.LimitBytes != 4096 || !request.Options.Previous || !request.Options.Timestamps || request.Options.Follow {
		t.Fatalf("unsafe snapshot request=%#v calls=%d", request, service.snapshotRequests)
	}

	for _, query := range []string{"namespace=attacker", "pod=attacker", "selector=x", "container=agent", "logRef=secret", "tailLines=020", "follow=true&previous=true"} {
		response = fixture.request(http.MethodGet, "/v1/builds/"+attemptID+"/logs?"+query, "", nil)
		problem := decode[httpapi.Problem](t, response)
		if response.StatusCode != http.StatusUnprocessableEntity || problem.Code != "ValidationFailed" {
			t.Fatalf("query %q status=%d problem=%#v", query, response.StatusCode, problem)
		}
	}
	if service.snapshotRequests != 1 || service.followRequests != 0 {
		t.Fatalf("invalid caller selectors reached backend: snapshot=%d follow=%d", service.snapshotRequests, service.followRequests)
	}
}

func TestBuildLogSSECarriesOpaqueReconnectCursorAndAcceptsLastEventID(t *testing.T) {
	now := time.Date(2026, 8, 9, 3, 4, 5, 0, time.UTC)
	source := buildlogs.Source{ID: "build_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Ready: true}
	cursor := &buildlogs.LineCursor{SourceID: source.ID, Timestamp: now, Fingerprint: strings.Repeat("b", 64)}
	service := &fakeBuildLogService{followEvents: []buildlogs.StreamEvent{
		{Type: buildlogs.StreamStatus, Status: &buildlogs.StatusPayload{Source: source, State: "active"}, At: now},
		{Type: buildlogs.StreamLine, Line: &buildlogs.LogLine{Type: "line", Source: source, Message: "step complete", Cursor: cursor}, At: now},
		{Type: buildlogs.StreamTerminal, Terminal: &buildlogs.TerminalPayload{Code: "BuildCompleted", Detail: "The build log stream has ended."}, At: now},
	}}
	fixture := newBuildLogAPI(t, service, &buildLogHTTPReadiness{})
	fixture.bootstrap()
	attemptID := "11111111-1111-4111-8111-111111111111"
	response := fixture.request(http.MethodGet, "/v1/builds/"+attemptID+"/logs?follow=true&tailLines=10&limitBytes=4096", "", nil)
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" || response.Header.Get("Cache-Control") != "private, no-store" || response.Header.Get("X-Accel-Buffering") != "no" || !strings.Contains(text, "event: status\n") || !strings.Contains(text, "event: line\n") || !strings.Contains(text, "event: terminal\n") || strings.Contains(text, "namespace") || strings.Contains(text, "podName") || strings.Contains(text, "container") {
		t.Fatalf("SSE status=%d headers=%v body=%q", response.StatusCode, response.Header, text)
	}
	var eventID string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "id: ") {
			eventID = strings.TrimPrefix(line, "id: ")
		}
	}
	if eventID == "" {
		t.Fatalf("line reconnect cursor missing: %q", text)
	}

	request, err := http.NewRequest(http.MethodGet, fixture.server.URL+"/v1/builds/"+attemptID+"/logs?follow=true", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Last-Event-ID", eventID)
	response, err = fixture.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || service.followRequests != 2 || service.followRequest.Cursor == nil || service.followRequest.Cursor.SourceID != source.ID || !service.followRequest.Cursor.Timestamp.Equal(now) || service.followRequest.Cursor.Fingerprint != cursor.Fingerprint {
		t.Fatalf("reconnect status=%d request=%#v calls=%d", response.StatusCode, service.followRequest, service.followRequests)
	}
}

func TestBuildLogReadinessFailsClosedWithoutChangingRuntimeLogs(t *testing.T) {
	probe := &buildLogHTTPReadiness{err: errors.New("Kubernetes API unavailable")}
	service := &fakeBuildLogService{}
	fixture := newBuildLogAPI(t, service, probe)
	fixture.bootstrap()
	attemptID := "11111111-1111-4111-8111-111111111111"

	response := fixture.request(http.MethodGet, "/v1/capabilities", "", nil)
	capabilities := decode[struct {
		Features map[string]bool `json:"features"`
	}](t, response)
	if capabilities.Features["buildLogs"] || capabilities.Features["logs"] {
		t.Fatalf("stale build log runtime advertised: %#v", capabilities.Features)
	}
	response = fixture.request(http.MethodGet, "/readyz", "", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("stale optional build-log runtime removed API readiness: status=%d", response.StatusCode)
	}
	response.Body.Close()
	response = fixture.request(http.MethodGet, "/v1/builds/"+attemptID+"/logs", "", nil)
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusServiceUnavailable || problem.Code != "BuildLogRuntimeUnavailable" || service.snapshotRequests != 0 || !strings.Contains(problem.Detail, "Build metadata remains available.") || strings.Contains(problem.Detail, "continues running") {
		t.Fatalf("unready request status=%d problem=%#v calls=%d", response.StatusCode, problem, service.snapshotRequests)
	}
}

func TestBuildLogRouteRequiresExactAutomationLogsReadScope(t *testing.T) {
	service := &fakeBuildLogService{snapshot: buildlogs.Snapshot{
		Source: buildlogs.Source{ID: "build_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Ready: true},
	}}
	fixture := newBuildLogAPI(t, service, &buildLogHTTPReadiness{})
	fixture.bootstrap()
	response := fixture.request(http.MethodPost, "/v1/projects", "build-log-scope-project", map[string]string{"name": "Build log scope"})
	project := decode[struct {
		ID string `json:"id"`
	}](t, response)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("project status=%d", response.StatusCode)
	}
	response = fixture.request(http.MethodPost, "/v1/projects/"+project.ID+"/service-accounts", "build-log-scope-account", map[string]string{"name": "Build log reader", "role": "developer"})
	account := decode[struct {
		ID string `json:"id"`
	}](t, response)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("account status=%d", response.StatusCode)
	}
	expiresAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second).Format(time.RFC3339)
	issue := func(key, scope string) string {
		t.Helper()
		issuedResponse := fixture.request(http.MethodPost, "/v1/service-accounts/"+account.ID+"/tokens", key, map[string]any{
			"name": key, "scopes": []string{scope}, "expiresAt": expiresAt,
		})
		issued := decode[tokenIssueWire](t, issuedResponse)
		if issuedResponse.StatusCode != http.StatusCreated || issued.Token == "" {
			t.Fatalf("issue %s status=%d", scope, issuedResponse.StatusCode)
		}
		return issued.Token
	}
	attemptID := "11111111-1111-4111-8111-111111111111"
	client := &http.Client{}
	response = bearerRequest(t, client, fixture.server.URL, http.MethodGet, "/v1/builds/"+attemptID+"/logs", issue("wrong-scope", "app.read"), "", nil)
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusForbidden || problem.Code != "InsufficientTokenScope" || service.snapshotRequests != 0 {
		t.Fatalf("wrong scope status=%d problem=%#v calls=%d", response.StatusCode, problem, service.snapshotRequests)
	}
	response = bearerRequest(t, client, fixture.server.URL, http.MethodGet, "/v1/builds/"+attemptID+"/logs", issue("log-scope", "logs.read"), "", nil)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || service.snapshotRequests != 1 {
		t.Fatalf("logs.read status=%d calls=%d", response.StatusCode, service.snapshotRequests)
	}
}

func TestBuildLogErrorsAreSafeAndDoNotExposeKubernetesIdentity(t *testing.T) {
	service := &fakeBuildLogService{err: buildlogs.ErrScopeViolation}
	fixture := newBuildLogAPI(t, service, &buildLogHTTPReadiness{})
	fixture.bootstrap()
	response := fixture.request(http.MethodGet, "/v1/builds/11111111-1111-4111-8111-111111111111/logs", "", nil)
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var problem httpapi.Problem
	if err = json.Unmarshal(body, &problem); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusBadGateway || problem.Code != "BuildLogResponseRejected" || strings.Contains(string(body), "kuberploy-build-dind") || strings.Contains(string(body), "builder-agent") {
		t.Fatalf("unsafe error status=%d body=%s", response.StatusCode, body)
	}
}
