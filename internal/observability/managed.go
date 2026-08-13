package observability

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"reflect"
	"regexp"
)

const (
	ManagedMonitoringNamespace          = "kuberploy-monitoring"
	ManagedMonitoringProfileName        = "monitoring-monitoring-profile"
	ManagedMonitoringOperatorName       = "kuberploy-prometheus-operator"
	ManagedMonitoringOperatorContainer  = "kube-prometheus-stack"
	ManagedMonitoringOperatorImage      = "quay.io/prometheus-operator/prometheus-operator:v0.93.0"
	ManagedMonitoringOperatorArgsSHA256 = "sha256:ad7ee73da3828389d76d5f6102dde3c3c6cde35f0345bf8d7cad220a5c6df7a6"
	ManagedMonitoringRuleName           = "monitoring-service-recording-rules"
	ManagedMonitoringRuleSpecSHA256     = "sha256:0058f63c0c000cc9e491f3775c830554fa7a1bf10d0b86de7e3f8d61e9b09879"
	ManagedMonitoringPrometheusName     = "monitoring-kube-prometheus-prometheus"
	ManagedMonitoringQueryURL           = "http://prometheus-operated.kuberploy-monitoring.svc:9090"
)

var managedRequiredScrapePools = []string{
	// The installer deliberately fixes the independently owned edge Helm
	// release name to "edge". Bind readiness to that exact live identity;
	// using the Argo Application name ("kuberploy-edge") here would reject a
	// healthy installation even though the protected ServiceMonitor is up.
	"serviceMonitor/kuberploy-monitoring/edge-traefik/0",
	"serviceMonitor/kuberploy-monitoring/kuberploy-kube-state-metrics/0",
	"serviceMonitor/kuberploy-monitoring/monitoring-kube-prometheus-kubelet/0",
}

var managedMetricSeries = []string{
	"kuberploy:service:container_restarts_total",
	"kuberploy:service:cpu_usage_cores",
	"kuberploy:service:http_5xx_ratio",
	"kuberploy:service:http_latency_seconds:p95",
	"kuberploy:service:http_requests_per_second",
	"kuberploy:service:memory_working_set_bytes",
	"kuberploy:service:replicas_ready",
}

// ManagedMonitoringSnapshot is the closed set of live Kubernetes state used
// to attest the independently owned managed-monitoring release.
type ManagedMonitoringSnapshot struct {
	ProfileData                        map[string]string
	ProfileImmutable                   bool
	OperatorName                       string
	OperatorContainer                  string
	OperatorImage                      string
	OperatorArgumentsSHA256            string
	OperatorGeneration                 int64
	OperatorObservedGeneration         int64
	OperatorDesiredReplicas            int32
	OperatorAvailableReplicas          int32
	RuleName                           string
	RuleGeneration                     int64
	RuleSpecSHA256                     string
	PrometheusName                     string
	PrometheusGeneration               int64
	PrometheusOverrideHonorLabels      bool
	PrometheusIgnoreNamespaceSelectors bool
	PrometheusArbitraryFSAccessDeny    bool
}

type ManagedMonitoringObserver interface {
	ObserveManagedMonitoring(context.Context) (ManagedMonitoringSnapshot, error)
}

type ManagedPrometheusProbe interface {
	Probe(context.Context) error
	ProbeManagedRules(context.Context) error
	ProbeManagedTargets(context.Context) error
}

// ManagedReadinessProbe requires both exact Kubernetes release attestation and
// live Prometheus rule health. A generic Prometheus vector probe alone can
// never make managed monitoring ready.
type ManagedReadinessProbe struct {
	Prometheus           ManagedPrometheusProbe
	Observer             ManagedMonitoringObserver
	ExpectedChartVersion string
}

func (p *ManagedReadinessProbe) Probe(ctx context.Context) error {
	if p == nil || p.Prometheus == nil || p.Observer == nil {
		return ErrUnavailable
	}
	snapshot, err := p.Observer.ObserveManagedMonitoring(ctx)
	if err != nil {
		return ErrUnavailable
	}
	if err = validateManagedSnapshot(snapshot, p.ExpectedChartVersion); err != nil {
		return err
	}
	if err = p.Prometheus.Probe(ctx); err != nil {
		return err
	}
	if err = p.Prometheus.ProbeManagedRules(ctx); err != nil {
		return err
	}
	return p.Prometheus.ProbeManagedTargets(ctx)
}

func validateManagedSnapshot(snapshot ManagedMonitoringSnapshot, chartVersion string) error {
	if !validManagedChartVersion(chartVersion) || !snapshot.ProfileImmutable || !reflect.DeepEqual(snapshot.ProfileData, expectedManagedProfile(chartVersion)) {
		return ErrUnsafeResponse
	}
	if snapshot.OperatorName != ManagedMonitoringOperatorName || snapshot.OperatorContainer != ManagedMonitoringOperatorContainer ||
		snapshot.OperatorImage != ManagedMonitoringOperatorImage || snapshot.OperatorGeneration < 1 ||
		snapshot.OperatorObservedGeneration != snapshot.OperatorGeneration || snapshot.OperatorDesiredReplicas != 1 ||
		snapshot.OperatorAvailableReplicas != 1 || snapshot.OperatorArgumentsSHA256 != ManagedMonitoringOperatorArgsSHA256 {
		return ErrUnsafeResponse
	}
	if snapshot.RuleName != ManagedMonitoringRuleName || snapshot.RuleGeneration < 1 || snapshot.RuleSpecSHA256 != ManagedMonitoringRuleSpecSHA256 {
		return ErrUnsafeResponse
	}
	if snapshot.PrometheusName != ManagedMonitoringPrometheusName || snapshot.PrometheusGeneration < 1 || !snapshot.PrometheusOverrideHonorLabels ||
		snapshot.PrometheusIgnoreNamespaceSelectors || snapshot.PrometheusArbitraryFSAccessDeny {
		return ErrUnsafeResponse
	}
	return nil
}

// ManagedService keeps tenant queries on the ordinary closed metric catalog,
// but strengthens Probe with the independently owned release attestation. Both
// capabilities and the monitoring status endpoint use Probe, so managed mode
// cannot be advertised from endpoint reachability alone.
type ManagedService struct {
	client    *Client
	readiness *ManagedReadinessProbe
}

func NewManagedService(client *Client, observer ManagedMonitoringObserver, chartVersion string) (*ManagedService, error) {
	if client == nil || observer == nil || !validManagedChartVersion(chartVersion) {
		return nil, ErrUnavailable
	}
	return &ManagedService{client: client, readiness: &ManagedReadinessProbe{Prometheus: client, Observer: observer, ExpectedChartVersion: chartVersion}}, nil
}

func (s *ManagedService) Probe(ctx context.Context) error {
	if s == nil || s.readiness == nil {
		return ErrUnavailable
	}
	return s.readiness.Probe(ctx)
}

func (s *ManagedService) QueryRange(ctx context.Context, scope Scope, metric MetricKey, queryRange Range) (Result, error) {
	if s == nil || s.client == nil {
		return Result{}, ErrUnavailable
	}
	// Managed mode must not serve queries when the exact installed release,
	// recording rules, or protected scrape targets are stale. Capability probes
	// are advisory and may race a later query, so enforce the same boundary here.
	if err := s.Probe(ctx); err != nil {
		return Result{}, err
	}
	return s.client.QueryRange(ctx, scope, metric, queryRange)
}

var managedChartVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-rc\.[0-9]+)?$`)

func validManagedChartVersion(version string) bool {
	return managedChartVersionPattern.MatchString(version)
}

func expectedManagedProfile(chartVersion string) map[string]string {
	return map[string]string{
		"contract":                       "kuberploy-managed-monitoring/v1",
		"management":                     "managed",
		"chartName":                      "kuberploy-monitoring",
		"chartVersion":                   chartVersion,
		"upstreamChartSHA256":            "sha256:b558a852552f809ccce66d5677ca1a55c8010470c44a01dbdc4ab3f678bcdc90",
		"releaseName":                    "monitoring",
		"namespace":                      ManagedMonitoringNamespace,
		"queryURL":                       ManagedMonitoringQueryURL,
		"operatorDeploymentName":         ManagedMonitoringOperatorName,
		"operatorContainerName":          ManagedMonitoringOperatorContainer,
		"operatorImage":                  ManagedMonitoringOperatorImage,
		"operatorArgumentsSHA256":        ManagedMonitoringOperatorArgsSHA256,
		"recordingRuleName":              ManagedMonitoringRuleName,
		"recordingRuleSpecSHA256":        ManagedMonitoringRuleSpecSHA256,
		"readinessContract":              "profile+operator+rule-spec+prometheus-scrape-policy+prometheus-rules+required-targets",
		"queryClientNamespaceLabel":      "kuberploy.io/control-plane-namespace=true",
		"queryClientPodLabels":           "app.kubernetes.io/name=kuberploy,app.kubernetes.io/component=api",
		"monitorNamespaceLabel":          "kuberploy.io/monitoring-namespace=true",
		"monitorSelectorLabel":           "kuberploy.io/monitoring-source=protected",
		"ruleSelectorLabel":              "kuberploy.io/monitoring-rule=protected",
		"ignoreNamespaceSelectors":       "false",
		"serviceMonitorFilesystemAccess": "kubelet-service-account-token+cluster-ca",
		"grafanaEnabled":                 "false",
		"metricSeries":                   "kuberploy:service:cpu_usage_cores,kuberploy:service:memory_working_set_bytes,kuberploy:service:replicas_ready,kuberploy:service:container_restarts_total,kuberploy:service:http_requests_per_second,kuberploy:service:http_5xx_ratio,kuberploy:service:http_latency_seconds:p95",
	}
}

func canonicalRawJSONDigest(raw json.RawMessage) string {
	if len(raw) == 0 || len(raw) > int(defaultMaxBody) {
		return ""
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return ""
	}
	return digestJSON(value)
}

func digestJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}
