package observability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingTokenSource struct {
	mu   sync.Mutex
	last []byte
	err  error
}

func managedRulesResponse(mutator func([]map[string]string) []map[string]string) string {
	rules := make([]map[string]string, 0, len(managedMetricSeries))
	for _, name := range managedMetricSeries {
		rules = append(rules, map[string]string{"name": name, "type": "recording", "health": "ok", "lastError": ""})
	}
	if mutator != nil {
		rules = mutator(rules)
	}
	body, _ := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"groups": []any{map[string]any{"name": "kuberploy.service.metrics", "rules": rules}}}})
	return string(body)
}

func TestProbeManagedRulesRequiresEveryUniqueHealthyRecordingRule(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func([]map[string]string) []map[string]string
		wantErr error
	}{
		{name: "healthy"},
		{name: "missing", mutate: func(rules []map[string]string) []map[string]string { return rules[:len(rules)-1] }, wantErr: ErrUnavailable},
		{name: "duplicate", mutate: func(rules []map[string]string) []map[string]string { return append(rules, rules[0]) }, wantErr: ErrUnsafeResponse},
		{name: "unhealthy", mutate: func(rules []map[string]string) []map[string]string { rules[0]["health"] = "err"; return rules }, wantErr: ErrUnsafeResponse},
		{name: "last error", mutate: func(rules []map[string]string) []map[string]string {
			rules[0]["lastError"] = "evaluation failed"
			return rules
		}, wantErr: ErrUnsafeResponse},
		{name: "not recording", mutate: func(rules []map[string]string) []map[string]string { rules[0]["type"] = "alerting"; return rules }, wantErr: ErrUnsafeResponse},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/rules" || r.URL.Query().Get("type") != "record" || len(r.URL.Query()) != 1 {
					t.Errorf("unexpected rules request %s?%s", r.URL.Path, r.URL.RawQuery)
				}
				fmt.Fprint(w, managedRulesResponse(test.mutate))
			}))
			defer server.Close()
			client, err := NewClient(Options{BaseURL: server.URL, HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			err = client.ProbeManagedRules(context.Background())
			if test.wantErr == nil && err != nil {
				t.Fatal(err)
			}
			if test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Fatalf("error=%v want=%v", err, test.wantErr)
			}
		})
	}
}

func managedTargetsResponse(targets []map[string]string) string {
	body, _ := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"activeTargets": targets}})
	return string(body)
}

func TestProbeManagedTargetsRequiresEveryHealthyProtectedSource(t *testing.T) {
	t.Parallel()
	healthy := func() []map[string]string {
		targets := make([]map[string]string, 0, len(managedRequiredScrapePools))
		for _, pool := range managedRequiredScrapePools {
			targets = append(targets, map[string]string{"scrapePool": pool, "health": "up"})
		}
		return targets
	}
	tests := []struct {
		name    string
		mutate  func([]map[string]string) []map[string]string
		wantErr error
	}{
		{name: "healthy"},
		{name: "unrelated target ignored", mutate: func(targets []map[string]string) []map[string]string {
			return append(targets, map[string]string{"scrapePool": "serviceMonitor/other/other/0", "health": "down"})
		}},
		{name: "missing source", mutate: func(targets []map[string]string) []map[string]string {
			return targets[:len(targets)-1]
		}, wantErr: ErrUnavailable},
		{name: "source down", mutate: func(targets []map[string]string) []map[string]string {
			targets[0]["health"] = "down"
			return targets
		}, wantErr: ErrUnavailable},
		{name: "oversized identity", mutate: func(targets []map[string]string) []map[string]string {
			targets[0]["scrapePool"] = strings.Repeat("x", 254)
			return targets
		}, wantErr: ErrUnsafeResponse},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			targets := healthy()
			if test.mutate != nil {
				targets = test.mutate(targets)
			}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/targets" || r.URL.Query().Get("state") != "active" || len(r.URL.Query()) != 1 {
					t.Errorf("unexpected targets request %s?%s", r.URL.Path, r.URL.RawQuery)
				}
				fmt.Fprint(w, managedTargetsResponse(targets))
			}))
			defer server.Close()
			client, err := NewClient(Options{BaseURL: server.URL, HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			err = client.ProbeManagedTargets(context.Background())
			if test.wantErr == nil && err != nil {
				t.Fatal(err)
			}
			if test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Fatalf("error=%v want=%v", err, test.wantErr)
			}
		})
	}
}

func (s *recordingTokenSource) ReadToken(context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = []byte("test-token-123")
	return s.last, s.err
}

func (s *recordingTokenSource) erased() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.last) == 0 {
		return false
	}
	for _, value := range s.last {
		if value != 0 {
			return false
		}
	}
	return true
}

func serviceScope() Scope {
	return Scope{
		Type:        ScopeService,
		Namespace:   "kp-team-project-production",
		Project:     "project-123",
		Environment: "environment-456",
		Application: "application-789",
		Service:     "service-web",
	}
}

func queryWindow() Range {
	return Range{
		From: time.Unix(1_800_000_000, 0).UTC(),
		To:   time.Unix(1_800_000_300, 0).UTC(),
		Step: time.Minute,
	}
}

func TestQueryRangeBuildsClosedScopedQueryAndRedactsLabels(t *testing.T) {
	t.Parallel()
	token := &recordingTokenSource{}
	var received url.Values
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prometheus/api/v1/query_range" {
			t.Errorf("path=%q", r.URL.Path)
		}
		if authorization := r.Header.Get("Authorization"); authorization != "Bearer test-token-123" {
			t.Errorf("authorization=%q", authorization)
		}
		received = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"namespace":"kp-team-project-production","kuberploy_project":"project-123","kuberploy_environment":"environment-456","kuberploy_application":"application-789","kuberploy_service":"service-web","pod":"must-not-leak","token":"must-not-leak"},"values":[[1800000000,"0"],[1800000060.25,"1.5"]]}]}}`)
	}))
	defer server.Close()

	client, err := NewClient(Options{BaseURL: server.URL + "/prometheus", HTTPClient: server.Client(), TokenSource: token})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.QueryRange(context.Background(), serviceScope(), MetricCPUUsage, queryWindow())
	if err != nil {
		t.Fatal(err)
	}
	expectedQuery := `kuberploy:service:cpu_usage_cores{kuberploy_application="application-789",kuberploy_environment="environment-456",kuberploy_project="project-123",kuberploy_service="service-web",namespace="kp-team-project-production"}`
	if received.Get("query") != expectedQuery {
		t.Fatalf("query=%q", received.Get("query"))
	}
	if received.Get("step") != "60" || received.Get("start") != "1800000000.000" || received.Get("end") != "1800000300.000" {
		t.Fatalf("range parameters=%v", received)
	}
	if !token.erased() {
		t.Fatal("token source buffer was not erased")
	}
	if result.Metric != MetricCPUUsage || result.Scope != ScopeService || len(result.Series) != 1 || len(result.Series[0].Samples) != 2 {
		t.Fatalf("result=%#v", result)
	}
	if _, exists := result.Series[0].Labels["pod"]; exists {
		t.Fatal("provider pod label escaped the allowlist")
	}
	if _, exists := result.Series[0].Labels["token"]; exists {
		t.Fatal("provider token label escaped the allowlist")
	}
	if result.Series[0].Samples[1].Value != 1.5 || !result.Series[0].Samples[1].Timestamp.Equal(time.Unix(1_800_000_060, 250_000_000).UTC()) {
		t.Fatalf("sample=%#v", result.Series[0].Samples[1])
	}
}

func TestClientRejectsUnsafeOriginsRedirectsAndProviderBodies(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"http://prometheus.example.test",
		"https://user:password@prometheus.example.test",
		"https://prometheus.example.test/base?token=secret",
		"https://prometheus.example.test/base#fragment",
		"https://prometheus.example.test/base/../admin",
		"https://prometheus.example.test/base/%2e%2e/admin",
	} {
		if _, err := NewClient(Options{BaseURL: raw}); err == nil {
			t.Fatalf("unsafe base URL accepted: %s", raw)
		}
	}

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/redirected") {
			t.Fatal("redirect was followed")
		}
		http.Redirect(w, r, "/redirected?credential=provider-secret", http.StatusFound)
	}))
	defer server.Close()
	client, err := NewClient(Options{BaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.QueryRange(context.Background(), Scope{Type: ScopeNamespace, Namespace: "tenant-one"}, MetricMemoryUsage, queryWindow())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("redirect error=%v", err)
	}
	if message := ErrorMessage(err); strings.Contains(message, "provider-secret") || strings.Contains(message, server.URL) {
		t.Fatalf("unsafe provider detail in error message: %q", message)
	}
}

func TestManagedModeMayUseOnlyKubernetesServiceHTTP(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		url     string
		allowed bool
	}{
		{url: "http://prometheus-operated.kuberploy-monitoring.svc:9090", allowed: true},
		{url: "http://prometheus-operated.kuberploy-monitoring.svc.cluster.local:9090", allowed: true},
		{url: "http://prometheus-operated.kuberploy-monitoring.svc.attacker.example", allowed: false},
		{url: "http://127.0.0.1:9090", allowed: false},
		{url: "http://prometheus.example.test", allowed: false},
	} {
		_, err := NewClient(Options{BaseURL: test.url, AllowHTTPForClusterService: true})
		if (err == nil) != test.allowed {
			t.Fatalf("url=%q allowed=%v err=%v", test.url, test.allowed, err)
		}
	}
}

func TestTokenBufferIsErasedWhenTheSourceFails(t *testing.T) {
	t.Parallel()
	token := &recordingTokenSource{err: errors.New("provider secret lookup failed")}
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("request must not be sent when token lookup fails")
	}))
	defer server.Close()
	client, err := NewClient(Options{BaseURL: server.URL, HTTPClient: server.Client(), TokenSource: token})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.QueryRange(context.Background(), Scope{Type: ScopeNamespace, Namespace: "tenant"}, MetricCPUUsage, queryWindow())
	if !errors.Is(err, ErrUnavailable) || !token.erased() {
		t.Fatalf("error=%v erased=%v", err, token.erased())
	}
}

func TestQueryRangeFailsClosedForScopeMetricRangeAndCrossScopeData(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"namespace":"another-tenant"},"values":[[1800000000,"1"]]}]}}`)
	}))
	defer server.Close()
	client, err := NewClient(Options{BaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.QueryRange(context.Background(), Scope{Type: "arbitrary", Namespace: "tenant"}, MetricCPUUsage, queryWindow()); !errors.Is(err, ErrInvalidScope) {
		t.Fatalf("invalid scope error=%v", err)
	}
	if _, err = client.QueryRange(context.Background(), Scope{Type: ScopeNamespace, Namespace: "tenant"}, "raw-promql", queryWindow()); !errors.Is(err, ErrInvalidMetric) {
		t.Fatalf("invalid metric error=%v", err)
	}
	badRange := queryWindow()
	badRange.Step = time.Second
	if _, err = client.QueryRange(context.Background(), Scope{Type: ScopeNamespace, Namespace: "tenant"}, MetricCPUUsage, badRange); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("invalid range error=%v", err)
	}
	if _, err = client.QueryRange(context.Background(), Scope{Type: ScopeNamespace, Namespace: "tenant"}, MetricCPUUsage, queryWindow()); !errors.Is(err, ErrUnsafeResponse) {
		t.Fatalf("cross-scope response error=%v", err)
	}
}

func TestQueryRangeEnforcesBodySeriesSampleAndNumberLimits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		body    string
		options Options
	}{
		{name: "body", body: `{"status":"success","data":{"resultType":"matrix","result":[]}}` + strings.Repeat(" ", 128), options: Options{MaxResponseBytes: 64}},
		{name: "series", body: `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"namespace":"tenant"},"values":[]},{"metric":{"namespace":"tenant"},"values":[]}]}}`, options: Options{MaxSeries: 1}},
		{name: "samples", body: `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"namespace":"tenant"},"values":[[1800000000,"1"],[1800000060,"2"],[1800000120,"3"],[1800000180,"4"],[1800000240,"5"],[1800000300,"6"],[1800000360,"7"]]}]}}`, options: Options{MaxSamples: 6}},
		{name: "nan", body: `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"namespace":"tenant"},"values":[[1800000000,"NaN"]]}]}}`},
		{name: "trailing", body: `{"status":"success","data":{"resultType":"matrix","result":[]}} {}`},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			options := test.options
			options.BaseURL = server.URL
			options.HTTPClient = server.Client()
			client, err := NewClient(options)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.QueryRange(context.Background(), Scope{Type: ScopeNamespace, Namespace: "tenant"}, MetricCPUUsage, queryWindow())
			if !errors.Is(err, ErrUnsafeResponse) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestGlobalScopeDoesNotAddMatchers(t *testing.T) {
	t.Parallel()
	var expression string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expression = r.URL.Query().Get("query")
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	}))
	defer server.Close()
	client, err := NewClient(Options{BaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.QueryRange(context.Background(), Scope{Type: ScopeGlobal}, MetricReplicasReady, queryWindow()); err != nil {
		t.Fatal(err)
	}
	if expression != "kuberploy:service:replicas_ready" {
		t.Fatalf("global query=%q", expression)
	}
	if _, err = client.QueryRange(context.Background(), Scope{Type: ScopeGlobal, Namespace: "smuggled"}, MetricReplicasReady, queryWindow()); !errors.Is(err, ErrInvalidScope) {
		t.Fatalf("global scope with labels error=%v", err)
	}
}

func TestProbeUsesFixedExpressionAndStrictVectorResponse(t *testing.T) {
	t.Parallel()
	var query string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query().Get("query")
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1800000000,"1"]}]}}`)
	}))
	defer server.Close()
	client, err := NewClient(Options{BaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if query != "vector(1)" {
		t.Fatalf("probe query=%q", query)
	}

	unsafe := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"namespace":"tenant"},"value":[1800000000,"1"]}]}}`)
	}))
	defer unsafe.Close()
	client, err = NewClient(Options{BaseURL: unsafe.URL, HTTPClient: unsafe.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Probe(context.Background()); !errors.Is(err, ErrUnsafeResponse) {
		t.Fatalf("unsafe probe error=%v", err)
	}
}
