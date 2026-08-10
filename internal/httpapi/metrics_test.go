package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/observability"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

type fakeMetricsService struct {
	mu       sync.Mutex
	probeErr error
	queryErr error
	scope    observability.Scope
	metric   observability.MetricKey
	rangeArg observability.Range
}

func (f *fakeMetricsService) Probe(context.Context) error {
	return f.probeErr
}

func (f *fakeMetricsService) QueryRange(_ context.Context, scope observability.Scope, metric observability.MetricKey, rangeArg observability.Range) (observability.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scope, f.metric, f.rangeArg = scope, metric, rangeArg
	if f.queryErr != nil {
		return observability.Result{}, f.queryErr
	}
	return observability.Result{
		Metric:     metric,
		Scope:      scope.Type,
		Series:     []observability.Series{{Labels: map[string]string{"namespace": scope.Namespace}, Samples: []observability.Sample{{Timestamp: rangeArg.From, Value: 1.25}}}},
		ObservedAt: rangeArg.To,
	}, nil
}

func newMonitoringAPI(t *testing.T, service *fakeMetricsService) *apiFixture {
	t.Helper()
	st := memory.New()
	srv := httptest.NewServer(httpapi.New(httpapi.Options{Store: st, BootstrapToken: "one-time-secret", Version: "test", MonitoringMode: "managed", Metrics: service, HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000)}))
	jar, _ := cookiejar.New(nil)
	fixture := &apiFixture{t: t, server: srv, client: &http.Client{Jar: jar}, store: st}
	t.Cleanup(srv.Close)
	return fixture
}

func TestScopedMetricsQueryResolvesPlatformOwnedIdentity(t *testing.T) {
	metrics := &fakeMetricsService{}
	f := newMonitoringAPI(t, metrics)
	if response := f.request("GET", "/v1/metrics/query-range", "", nil); response.StatusCode != http.StatusUnauthorized {
		response.Body.Close()
		t.Fatalf("unauthenticated status=%d", response.StatusCode)
	} else {
		response.Body.Close()
	}
	f.bootstrap()
	response := f.request("GET", "/v1/monitoring/status", "", nil)
	status := decode[struct {
		Status    string `json:"status"`
		Available bool   `json:"available"`
	}](t, response)
	if response.StatusCode != http.StatusOK || status.Status != "available" || !status.Available {
		t.Fatalf("status=%d body=%#v", response.StatusCode, status)
	}
	response = f.request("GET", "/v1/capabilities", "", nil)
	capabilities := decode[struct {
		Features map[string]bool `json:"features"`
	}](t, response)
	if response.StatusCode != http.StatusOK || !capabilities.Features["monitoring"] || !capabilities.Features["metrics"] {
		t.Fatalf("configured monitoring capabilities status=%d body=%#v", response.StatusCode, capabilities)
	}

	response = f.request("POST", "/v1/projects", "metrics-project", map[string]string{"name": "Metrics"})
	project := decode[domain.Project](t, response)
	response = f.request("POST", "/v1/environments", "metrics-environment", map[string]string{"projectId": project.ID, "name": "Production"})
	environment := decode[domain.Environment](t, response)
	response = f.request("POST", "/v1/applications", "metrics-application", map[string]string{"projectId": project.ID, "name": "Web"})
	application := decode[domain.Application](t, response)
	response = f.request("POST", "/v1/deployments", "metrics-deployment", map[string]any{
		"environmentId": environment.ID,
		"applicationId": application.ID,
		"image":         "registry.example.test/web@sha256:" + strings.Repeat("a", 64),
		"port":          8080,
	})
	operation := decode[domain.Operation](t, response)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("deployment status=%d operation=%#v", response.StatusCode, operation)
	}

	from := "2026-08-09T00:00:00Z"
	to := "2026-08-09T00:05:00Z"
	path := "/v1/metrics/query-range?scopeType=service&scopeId=" + url.QueryEscape(operation.TargetID) + "&metric=cpu-usage&from=" + url.QueryEscape(from) + "&to=" + url.QueryEscape(to) + "&step=60s"
	response = f.request("GET", path, "", nil)
	result := decode[observability.Result](t, response)
	if response.StatusCode != http.StatusOK || result.Metric != observability.MetricCPUUsage || response.Header.Get("Cache-Control") != "private, max-age=15" {
		t.Fatalf("query status=%d result=%#v", response.StatusCode, result)
	}
	metrics.mu.Lock()
	gotScope, gotMetric, gotRange := metrics.scope, metrics.metric, metrics.rangeArg
	metrics.mu.Unlock()
	if gotMetric != observability.MetricCPUUsage || gotScope.Type != observability.ScopeService || gotScope.Namespace != environment.Namespace || gotScope.Project != project.ID || gotScope.Environment != environment.ID || gotScope.Application != application.ID || gotScope.Service != application.ID {
		t.Fatalf("resolved scope=%#v metric=%q", gotScope, gotMetric)
	}
	if gotRange.Step != time.Minute || gotRange.From.Format(time.RFC3339) != from || gotRange.To.Format(time.RFC3339) != to {
		t.Fatalf("resolved range=%#v", gotRange)
	}

	globalPath := "/v1/metrics/query-range?scopeType=global&scopeId=platform&metric=replicas-ready&from=" + url.QueryEscape(from) + "&to=" + url.QueryEscape(to) + "&step=300s"
	response = f.request("GET", globalPath, "", nil)
	if response.StatusCode != http.StatusOK {
		problem := decode[httpapi.Problem](t, response)
		t.Fatalf("global query status=%d problem=%#v", response.StatusCode, problem)
	}
	response.Body.Close()
}

func TestMetricsQueryRejectsOpenPromQLAndMapsProviderFailures(t *testing.T) {
	metrics := &fakeMetricsService{}
	f := newMonitoringAPI(t, metrics)
	f.bootstrap()
	base := "/v1/metrics/query-range?scopeType=global&scopeId=platform&from=2026-08-09T00%3A00%3A00Z&to=2026-08-09T00%3A05%3A00Z&step=60s"

	for _, suffix := range []string{
		"&metric=up%7Bjob%3D%22prometheus%22%7D",
		"&metric=cpu-usage&query=up",
		"&metric=cpu-usage&metric=memory-working-set",
		"&metric=cpu-usage&step=060s",
	} {
		response := f.request("GET", base+suffix, "", nil)
		problem := decode[httpapi.Problem](t, response)
		if response.StatusCode != http.StatusUnprocessableEntity || problem.Code != "ValidationFailed" {
			t.Fatalf("query %q status=%d problem=%#v", suffix, response.StatusCode, problem)
		}
	}

	metrics.queryErr = observability.ErrRateLimited
	response := f.request("GET", base+"&metric=cpu-usage", "", nil)
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusTooManyRequests || response.Header.Get("Retry-After") != "15" || problem.Code != "MonitoringRateLimited" || !problem.Retryable {
		t.Fatalf("rate limit status=%d problem=%#v", response.StatusCode, problem)
	}
	metrics.queryErr = errors.New("provider included secret body")
	response = f.request("GET", base+"&metric=cpu-usage", "", nil)
	problem = decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusServiceUnavailable || problem.Code != "MonitoringUnavailable" || strings.Contains(problem.Detail, "secret") {
		t.Fatalf("provider failure status=%d problem=%#v", response.StatusCode, problem)
	}

	response = f.request("GET", strings.Replace(base, "scopeId=platform", "scopeId=wrong", 1)+"&metric=cpu-usage", "", nil)
	problem = decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusNotFound || problem.Code != "NotFound" {
		t.Fatalf("non-canonical global scope status=%d problem=%#v", response.StatusCode, problem)
	}
}

func TestConfiguredMonitoringDoesNotClaimAvailabilityWhenProbeFails(t *testing.T) {
	f := newMonitoringAPI(t, &fakeMetricsService{probeErr: observability.ErrUnavailable})
	f.bootstrap()
	response := f.request("GET", "/v1/monitoring/status", "", nil)
	status := decode[struct {
		Status    string `json:"status"`
		Available bool   `json:"available"`
	}](t, response)
	if response.StatusCode != http.StatusOK || status.Status != "unavailable" || status.Available {
		t.Fatalf("status=%d body=%#v", response.StatusCode, status)
	}
	response = f.request("GET", "/v1/capabilities", "", nil)
	capabilities := decode[struct {
		Features map[string]bool `json:"features"`
	}](t, response)
	if response.StatusCode != http.StatusOK || capabilities.Features["monitoring"] || capabilities.Features["metrics"] {
		t.Fatalf("unavailable monitoring was advertised status=%d features=%#v", response.StatusCode, capabilities.Features)
	}
}
