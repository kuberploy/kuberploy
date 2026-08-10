// Package observability contains the provider boundaries used by Kuberploy's
// scoped monitoring API. It deliberately does not expose a raw PromQL seam to
// tenant-facing callers.
package observability

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultTimeout    = 8 * time.Second
	defaultMaxBody    = int64(4 << 20)
	defaultMaxSeries  = 200
	defaultMaxSamples = 20_000
	defaultMaxRange   = 30 * 24 * time.Hour
)

// ScopeType is intentionally smaller than the authorization scope model. The
// API resolves a caller's grants first and supplies one exact, safe metric
// scope here.
type ScopeType string

const (
	ScopeService   ScopeType = "service"
	ScopeNamespace ScopeType = "namespace"
	ScopeGlobal    ScopeType = "global"
)

// Scope holds platform-resolved metric identity. Callers never supply these
// label values directly to Prometheus.
type Scope struct {
	Type        ScopeType
	Namespace   string
	Project     string
	Environment string
	Application string
	Service     string
}

// MetricKey is a closed catalog of recording-rule outputs. The recording
// rules attach only bounded Kuberploy identity dimensions.
type MetricKey string

const (
	MetricCPUUsage          MetricKey = "cpu-usage"
	MetricMemoryUsage       MetricKey = "memory-working-set"
	MetricReplicasReady     MetricKey = "replicas-ready"
	MetricContainerRestarts MetricKey = "container-restarts"
	MetricHTTPRequestRate   MetricKey = "http-request-rate"
	MetricHTTPErrorRatio    MetricKey = "http-error-ratio"
	MetricHTTPLatencyP95    MetricKey = "http-latency-p95"
)

var metricSeries = map[MetricKey]string{
	MetricCPUUsage:          "kuberploy:service:cpu_usage_cores",
	MetricMemoryUsage:       "kuberploy:service:memory_working_set_bytes",
	MetricReplicasReady:     "kuberploy:service:replicas_ready",
	MetricContainerRestarts: "kuberploy:service:container_restarts_total",
	MetricHTTPRequestRate:   "kuberploy:service:http_requests_per_second",
	MetricHTTPErrorRatio:    "kuberploy:service:http_5xx_ratio",
	MetricHTTPLatencyP95:    "kuberploy:service:http_latency_seconds:p95",
}

func ValidMetric(key MetricKey) bool {
	_, ok := metricSeries[key]
	return ok
}

var returnedLabelAllowlist = map[string]struct{}{
	"__name__":              {},
	"cluster":               {},
	"namespace":             {},
	"kuberploy_project":     {},
	"kuberploy_environment": {},
	"kuberploy_application": {},
	"kuberploy_service":     {},
}

// Range is a bounded Prometheus range query. Times are normalized to UTC and
// step is expressed exactly as seconds on the provider request.
type Range struct {
	From time.Time
	To   time.Time
	Step time.Duration
}

type Sample struct {
	Timestamp time.Time `json:"timestamp"`
	Value     float64   `json:"value"`
}

type Series struct {
	Labels  map[string]string `json:"labels"`
	Samples []Sample          `json:"samples"`
}

type Result struct {
	Metric     MetricKey `json:"metric"`
	Scope      ScopeType `json:"scope"`
	Series     []Series  `json:"series"`
	ObservedAt time.Time `json:"observedAt"`
}

// BearerTokenSource returns a fresh caller-owned buffer. Client.Query erases
// the buffer immediately after constructing the request header.
type BearerTokenSource interface {
	ReadToken(context.Context) ([]byte, error)
}

type Options struct {
	BaseURL          string
	HTTPClient       *http.Client
	TokenSource      BearerTokenSource
	Timeout          time.Duration
	MaxResponseBytes int64
	MaxSeries        int
	MaxSamples       int
	MaxRange         time.Duration
	// AllowHTTPForTests exists only for hermetic httptest servers.
	AllowHTTPForTests bool
	// AllowHTTPForClusterService permits managed-mode cleartext only for an
	// exact Kubernetes Service DNS name. NetworkPolicy and namespace ownership
	// are the transport boundary for this in-cluster hop.
	AllowHTTPForClusterService bool
}

type Client struct {
	base       *url.URL
	http       *http.Client
	token      BearerTokenSource
	timeout    time.Duration
	maxBody    int64
	maxSeries  int
	maxSamples int
	maxRange   time.Duration
}

var (
	ErrUnavailable    = errors.New("monitoring backend unavailable")
	ErrRateLimited    = errors.New("monitoring backend rate limited")
	ErrInvalidScope   = errors.New("invalid monitoring scope")
	ErrInvalidMetric  = errors.New("unsupported metric")
	ErrInvalidRange   = errors.New("invalid monitoring range")
	ErrUnsafeResponse = errors.New("monitoring backend returned an unsafe response")
)

func NewClient(options Options) (*Client, error) {
	base, err := url.Parse(strings.TrimSpace(options.BaseURL))
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("Prometheus base URL must be an absolute credential-free URL")
	}
	clusterHTTP := options.AllowHTTPForClusterService && base.Scheme == "http" && clusterServiceHost(base.Hostname())
	if base.Scheme != "https" && !(options.AllowHTTPForTests && base.Scheme == "http") && !clusterHTTP {
		return nil, errors.New("Prometheus base URL must use HTTPS")
	}
	if !canonicalBasePath(base) {
		return nil, errors.New("Prometheus base URL path is not canonical")
	}
	base.Path = strings.TrimSuffix(base.Path, "/")
	base.RawPath = ""

	timeout := options.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if timeout > 30*time.Second {
		return nil, errors.New("Prometheus timeout exceeds the hard limit")
	}
	maxBody := options.MaxResponseBytes
	if maxBody <= 0 {
		maxBody = defaultMaxBody
	}
	if maxBody > 16<<20 {
		return nil, errors.New("Prometheus response limit exceeds the hard limit")
	}
	maxSeries := options.MaxSeries
	if maxSeries <= 0 {
		maxSeries = defaultMaxSeries
	}
	if maxSeries > 1_000 {
		return nil, errors.New("Prometheus series limit exceeds the hard limit")
	}
	maxSamples := options.MaxSamples
	if maxSamples <= 0 {
		maxSamples = defaultMaxSamples
	}
	if maxSamples > 100_000 {
		return nil, errors.New("Prometheus sample limit exceeds the hard limit")
	}
	maxRange := options.MaxRange
	if maxRange <= 0 {
		maxRange = defaultMaxRange
	}
	if maxRange > 90*24*time.Hour {
		return nil, errors.New("Prometheus range limit exceeds the hard limit")
	}

	var transport http.RoundTripper
	if options.HTTPClient != nil {
		transport = options.HTTPClient.Transport
	}
	client := &http.Client{Transport: transport, Timeout: timeout}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{base: base, http: client, token: options.TokenSource, timeout: timeout, maxBody: maxBody, maxSeries: maxSeries, maxSamples: maxSamples, maxRange: maxRange}, nil
}

func clusterServiceHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" || net.ParseIP(host) != nil {
		return false
	}
	return strings.HasSuffix(host, ".svc") || strings.HasSuffix(host, ".svc.cluster.local")
}

func canonicalBasePath(base *url.URL) bool {
	pathSegments := strings.Split(base.Path, "/")
	escapedSegments := strings.Split(strings.ToLower(base.EscapedPath()), "/")
	if len(pathSegments) != len(escapedSegments) {
		return false
	}
	for index, segment := range pathSegments {
		escaped := escapedSegments[index]
		if segment == "." || segment == ".." || escaped == "%2e" || escaped == "%2e%2e" {
			return false
		}
	}
	return true
}

// QueryRange expands one catalog metric and exact authorized scope. It never
// accepts PromQL, matchers, a backend URL, or arbitrary provider parameters.
func (c *Client) QueryRange(ctx context.Context, scope Scope, metric MetricKey, queryRange Range) (Result, error) {
	name, ok := metricSeries[metric]
	if !ok {
		return Result{}, ErrInvalidMetric
	}
	expected, selector, err := validateScope(scope)
	if err != nil {
		return Result{}, err
	}
	if err = c.validateRange(queryRange); err != nil {
		return Result{}, err
	}
	expression := name
	if selector != "" {
		expression += "{" + selector + "}"
	}
	parameters := url.Values{
		"query": {expression},
		"start": {formatPrometheusTime(queryRange.From)},
		"end":   {formatPrometheusTime(queryRange.To)},
		"step":  {strconv.FormatInt(int64(queryRange.Step/time.Second), 10)},
	}
	endpoint := *c.base
	endpoint.Path = strings.TrimSuffix(c.base.Path, "/") + "/api/v1/query_range"
	endpoint.RawQuery = parameters.Encode()
	body, err := c.get(ctx, endpoint)
	if err != nil {
		return Result{}, err
	}
	series, err := c.decodeMatrix(body, expected)
	if err != nil {
		return Result{}, err
	}
	return Result{Metric: metric, Scope: scope.Type, Series: series, ObservedAt: time.Now().UTC()}, nil
}

// Probe executes a fixed scalar expression through the same authentication,
// origin, redirect, timeout, and response-size boundary as metric queries.
func (c *Client) Probe(ctx context.Context) error {
	endpoint := *c.base
	endpoint.Path = strings.TrimSuffix(c.base.Path, "/") + "/api/v1/query"
	endpoint.RawQuery = url.Values{"query": {"vector(1)"}}.Encode()
	body, err := c.get(ctx, endpoint)
	if err != nil {
		return err
	}
	var envelope prometheusVectorEnvelope
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	if err = decoder.Decode(&envelope); err != nil || decoder.Decode(&struct{}{}) != io.EOF || envelope.Status != "success" || envelope.Data.ResultType != "vector" || len(envelope.Data.Result) != 1 {
		return ErrUnsafeResponse
	}
	provider := envelope.Data.Result[0]
	if len(provider.Metric) != 0 || len(provider.Value) != 2 {
		return ErrUnsafeResponse
	}
	var timestamp float64
	var encoded string
	if json.Unmarshal(provider.Value[0], &timestamp) != nil || json.Unmarshal(provider.Value[1], &encoded) != nil || math.IsNaN(timestamp) || math.IsInf(timestamp, 0) || timestamp < 0 || timestamp > 253402300799 {
		return ErrUnsafeResponse
	}
	value, parseErr := strconv.ParseFloat(encoded, 64)
	if parseErr != nil || value != 1 {
		return ErrUnsafeResponse
	}
	return nil
}

// ProbeManagedRules verifies that Prometheus has loaded exactly one healthy
// recording rule for every series in Kuberploy's closed metric catalog. It is
// intentionally separate from Probe: vector(1) proves query reachability but
// says nothing about whether the managed rules were accepted and evaluated.
func (c *Client) ProbeManagedRules(ctx context.Context) error {
	endpoint := *c.base
	endpoint.Path = strings.TrimSuffix(c.base.Path, "/") + "/api/v1/rules"
	endpoint.RawQuery = url.Values{"type": {"record"}}.Encode()
	body, err := c.get(ctx, endpoint)
	if err != nil {
		return err
	}
	var envelope prometheusRulesEnvelope
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	if decoder.Decode(&envelope) != nil || decoder.Decode(&struct{}{}) != io.EOF || envelope.Status != "success" || len(envelope.Data.Groups) > 2_000 {
		return ErrUnsafeResponse
	}
	found := make(map[string]bool, len(managedMetricSeries))
	wanted := make(map[string]struct{}, len(managedMetricSeries))
	for _, name := range managedMetricSeries {
		wanted[name] = struct{}{}
	}
	for _, group := range envelope.Data.Groups {
		if len(group.Rules) > 1_000 {
			return ErrUnsafeResponse
		}
		for _, rule := range group.Rules {
			if _, ok := wanted[rule.Name]; !ok {
				continue
			}
			if found[rule.Name] || group.Name != "kuberploy.service.metrics" || rule.Type != "recording" || rule.Health != "ok" || rule.LastError != "" {
				return ErrUnsafeResponse
			}
			found[rule.Name] = true
		}
	}
	if len(found) != len(managedMetricSeries) {
		return ErrUnavailable
	}
	return nil
}

func (c *Client) get(ctx context.Context, endpoint url.URL) ([]byte, error) {

	requestContext, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, ErrUnavailable
	}
	request.Header.Set("Accept", "application/json")
	if c.token != nil {
		raw, tokenErr := c.token.ReadToken(requestContext)
		if tokenErr != nil {
			erase(raw)
			return nil, ErrUnavailable
		}
		valid := validBearerToken(raw)
		if valid {
			request.Header.Set("Authorization", "Bearer "+string(raw))
		}
		erase(raw)
		if !valid {
			return nil, ErrUnavailable
		}
	}

	response, err := c.http.Do(request)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusTooManyRequests {
		return nil, ErrRateLimited
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, ErrUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, c.maxBody+1))
	if err != nil || int64(len(body)) > c.maxBody {
		return nil, ErrUnsafeResponse
	}
	return body, nil
}

func (c *Client) validateRange(value Range) error {
	if value.From.IsZero() || value.To.IsZero() || !value.To.After(value.From) || value.To.Sub(value.From) > c.maxRange {
		return ErrInvalidRange
	}
	if value.Step < 10*time.Second || value.Step > time.Hour || value.Step%time.Second != 0 {
		return ErrInvalidRange
	}
	points := int(value.To.Sub(value.From)/value.Step) + 1
	if points <= 0 || points > c.maxSamples {
		return ErrInvalidRange
	}
	return nil
}

func validateScope(scope Scope) (map[string]string, string, error) {
	labels := map[string]string{}
	switch scope.Type {
	case ScopeService:
		labels = map[string]string{
			"namespace":             scope.Namespace,
			"kuberploy_project":     scope.Project,
			"kuberploy_environment": scope.Environment,
			"kuberploy_application": scope.Application,
			"kuberploy_service":     scope.Service,
		}
	case ScopeNamespace:
		labels = map[string]string{"namespace": scope.Namespace}
	case ScopeGlobal:
		if scope.Namespace != "" || scope.Project != "" || scope.Environment != "" || scope.Application != "" || scope.Service != "" {
			return nil, "", ErrInvalidScope
		}
	default:
		return nil, "", ErrInvalidScope
	}
	keys := make([]string, 0, len(labels))
	for key, value := range labels {
		if !validIdentity(value) {
			return nil, "", ErrInvalidScope
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	selectors := make([]string, 0, len(keys))
	for _, key := range keys {
		selectors = append(selectors, key+"="+strconv.Quote(labels[key]))
	}
	return labels, strings.Join(selectors, ","), nil
}

func validIdentity(value string) bool {
	return value != "" && len(value) <= 128 && utf8.ValidString(value) && strings.TrimSpace(value) == value &&
		strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) < 0
}

func validBearerToken(raw []byte) bool {
	if len(raw) < 8 || len(raw) > 8192 || !utf8.Valid(raw) {
		return false
	}
	for _, value := range raw {
		if value <= 0x20 || value >= 0x7f {
			return false
		}
	}
	return true
}

func erase(raw []byte) {
	for index := range raw {
		raw[index] = 0
	}
}

func formatPrometheusTime(value time.Time) string {
	value = value.UTC()
	seconds := float64(value.Unix()) + float64(value.Nanosecond())/float64(time.Second)
	return strconv.FormatFloat(seconds, 'f', 3, 64)
}

type prometheusEnvelope struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string             `json:"resultType"`
		Result     []prometheusSeries `json:"result"`
	} `json:"data"`
}

type prometheusSeries struct {
	Metric map[string]string   `json:"metric"`
	Values [][]json.RawMessage `json:"values"`
}

type prometheusVectorEnvelope struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

type prometheusRulesEnvelope struct {
	Status string `json:"status"`
	Data   struct {
		Groups []struct {
			Name  string `json:"name"`
			Rules []struct {
				Name      string `json:"name"`
				Type      string `json:"type"`
				Health    string `json:"health"`
				LastError string `json:"lastError"`
			} `json:"rules"`
		} `json:"groups"`
	} `json:"data"`
}

func (c *Client) decodeMatrix(body []byte, expected map[string]string) ([]Series, error) {
	var envelope prometheusEnvelope
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	if err := decoder.Decode(&envelope); err != nil {
		return nil, ErrUnsafeResponse
	}
	if decoder.Decode(&struct{}{}) != io.EOF || envelope.Status != "success" || envelope.Data.ResultType != "matrix" || len(envelope.Data.Result) > c.maxSeries {
		return nil, ErrUnsafeResponse
	}
	result := make([]Series, 0, len(envelope.Data.Result))
	totalSamples := 0
	for _, providerSeries := range envelope.Data.Result {
		for key, value := range expected {
			if providerSeries.Metric[key] != value {
				return nil, ErrUnsafeResponse
			}
		}
		labels := make(map[string]string)
		for key, value := range providerSeries.Metric {
			if _, allowed := returnedLabelAllowlist[key]; allowed && validReturnedLabel(value) {
				labels[key] = value
			}
		}
		samples := make([]Sample, 0, len(providerSeries.Values))
		for _, pair := range providerSeries.Values {
			if len(pair) != 2 {
				return nil, ErrUnsafeResponse
			}
			var timestamp float64
			var encoded string
			if json.Unmarshal(pair[0], &timestamp) != nil || json.Unmarshal(pair[1], &encoded) != nil || math.IsNaN(timestamp) || math.IsInf(timestamp, 0) || timestamp < 0 || timestamp > 253402300799 {
				return nil, ErrUnsafeResponse
			}
			value, parseErr := strconv.ParseFloat(encoded, 64)
			if parseErr != nil || math.IsNaN(value) || math.IsInf(value, 0) {
				return nil, ErrUnsafeResponse
			}
			seconds, fraction := math.Modf(timestamp)
			nanos := int64(math.Round(fraction * float64(time.Second)))
			if nanos >= int64(time.Second) {
				seconds++
				nanos -= int64(time.Second)
			}
			samples = append(samples, Sample{Timestamp: time.Unix(int64(seconds), nanos).UTC(), Value: value})
			totalSamples++
			if totalSamples > c.maxSamples {
				return nil, ErrUnsafeResponse
			}
		}
		result = append(result, Series{Labels: labels, Samples: samples})
	}
	return result, nil
}

func validReturnedLabel(value string) bool {
	return len(value) <= 256 && utf8.ValidString(value) && strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) < 0
}

// ErrorMessage is deliberately generic so provider bodies, queries, URLs and
// credentials are never reflected to callers or audit records.
func ErrorMessage(err error) string {
	switch {
	case errors.Is(err, ErrRateLimited):
		return "The monitoring backend is rate limited."
	case errors.Is(err, ErrInvalidScope), errors.Is(err, ErrInvalidMetric), errors.Is(err, ErrInvalidRange):
		return err.Error()
	default:
		return ErrUnavailable.Error()
	}
}
