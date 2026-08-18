package observability

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestManagedQueryFailsClosedBeforeProviderQueryWhenAttestationIsUnavailable(t *testing.T) {
	t.Parallel()
	providerCalls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		providerCalls++
	}))
	defer server.Close()
	client, err := NewClient(Options{BaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewManagedService(client, fixedManagedObserver{err: ErrUnavailable}, managedChartVersionForTest)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.QueryRange(context.Background(), Scope{Type: ScopeGlobal}, MetricReplicasReady, Range{From: time.Now().Add(-time.Minute), To: time.Now(), Step: 15 * time.Second})
	if !errors.Is(err, ErrUnavailable) || providerCalls != 0 {
		t.Fatalf("error=%v providerCalls=%d", err, providerCalls)
	}
}

const managedChartVersionForTest = "0.1.0-rc.208"

type fixedManagedObserver struct {
	snapshot ManagedMonitoringSnapshot
	err      error
}

func (o fixedManagedObserver) ObserveManagedMonitoring(context.Context) (ManagedMonitoringSnapshot, error) {
	return o.snapshot, o.err
}

type fixedManagedPrometheus struct {
	probeErr   error
	rulesErr   error
	targetsErr error
	probes     int
	rules      int
	targets    int
}

func (p *fixedManagedPrometheus) Probe(context.Context) error {
	p.probes++
	return p.probeErr
}

func (p *fixedManagedPrometheus) ProbeManagedRules(context.Context) error {
	p.rules++
	return p.rulesErr
}

func (p *fixedManagedPrometheus) ProbeManagedTargets(context.Context) error {
	p.targets++
	return p.targetsErr
}

func validManagedSnapshotForTest() ManagedMonitoringSnapshot {
	return ManagedMonitoringSnapshot{
		ProfileData: expectedManagedProfile(managedChartVersionForTest), ProfileImmutable: true,
		OperatorName: ManagedMonitoringOperatorName, OperatorContainer: ManagedMonitoringOperatorContainer,
		OperatorImage: ManagedMonitoringOperatorImage, OperatorArgumentsSHA256: ManagedMonitoringOperatorArgsSHA256,
		OperatorGeneration: 4, OperatorObservedGeneration: 4, OperatorDesiredReplicas: 1, OperatorAvailableReplicas: 1,
		RuleName: ManagedMonitoringRuleName, RuleGeneration: 7, RuleSpecSHA256: ManagedMonitoringRuleSpecSHA256,
		PrometheusName: ManagedMonitoringPrometheusName, PrometheusGeneration: 3, PrometheusOverrideHonorLabels: true,
	}
}

func TestManagedReadinessRequiresAttestationProbeAndRules(t *testing.T) {
	t.Parallel()
	prometheus := &fixedManagedPrometheus{}
	probe := &ManagedReadinessProbe{Prometheus: prometheus, Observer: fixedManagedObserver{snapshot: validManagedSnapshotForTest()}, ExpectedChartVersion: managedChartVersionForTest}
	if err := probe.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if prometheus.probes != 1 || prometheus.rules != 1 || prometheus.targets != 1 {
		t.Fatalf("probe calls=%d rule calls=%d target calls=%d", prometheus.probes, prometheus.rules, prometheus.targets)
	}

	prometheus = &fixedManagedPrometheus{probeErr: ErrUnavailable}
	probe.Prometheus = prometheus
	if err := probe.Probe(context.Background()); !errors.Is(err, ErrUnavailable) || prometheus.rules != 0 {
		t.Fatalf("probe failure=%v rule calls=%d", err, prometheus.rules)
	}

	prometheus = &fixedManagedPrometheus{rulesErr: ErrUnsafeResponse}
	probe.Prometheus = prometheus
	if err := probe.Probe(context.Background()); !errors.Is(err, ErrUnsafeResponse) {
		t.Fatalf("rules failure=%v", err)
	}

	prometheus = &fixedManagedPrometheus{targetsErr: ErrUnavailable}
	probe.Prometheus = prometheus
	if err := probe.Probe(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("targets failure=%v", err)
	}
}

func TestManagedSnapshotAttestationFailsClosedOnEveryProtectedIdentity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*ManagedMonitoringSnapshot)
	}{
		{name: "mutable profile", mutate: func(s *ManagedMonitoringSnapshot) { s.ProfileImmutable = false }},
		{name: "profile drift", mutate: func(s *ManagedMonitoringSnapshot) { s.ProfileData["releaseName"] = "attacker" }},
		{name: "operator name", mutate: func(s *ManagedMonitoringSnapshot) { s.OperatorName = "attacker" }},
		{name: "operator container", mutate: func(s *ManagedMonitoringSnapshot) { s.OperatorContainer = "attacker" }},
		{name: "operator image", mutate: func(s *ManagedMonitoringSnapshot) { s.OperatorImage = "attacker:latest" }},
		{name: "operator args", mutate: func(s *ManagedMonitoringSnapshot) { s.OperatorArgumentsSHA256 = "sha256:" + string(make([]byte, 64)) }},
		{name: "operator stale", mutate: func(s *ManagedMonitoringSnapshot) { s.OperatorObservedGeneration-- }},
		{name: "operator replicas", mutate: func(s *ManagedMonitoringSnapshot) { s.OperatorAvailableReplicas = 2 }},
		{name: "rule name", mutate: func(s *ManagedMonitoringSnapshot) { s.RuleName = "attacker" }},
		{name: "rule digest", mutate: func(s *ManagedMonitoringSnapshot) { s.RuleSpecSHA256 = "" }},
		{name: "prometheus name", mutate: func(s *ManagedMonitoringSnapshot) { s.PrometheusName = "attacker" }},
		{name: "prometheus generation", mutate: func(s *ManagedMonitoringSnapshot) { s.PrometheusGeneration = 0 }},
		{name: "prometheus label override", mutate: func(s *ManagedMonitoringSnapshot) { s.PrometheusOverrideHonorLabels = false }},
		{name: "prometheus namespace override", mutate: func(s *ManagedMonitoringSnapshot) { s.PrometheusIgnoreNamespaceSelectors = true }},
		{name: "prometheus filesystem guard", mutate: func(s *ManagedMonitoringSnapshot) { s.PrometheusArbitraryFSAccessDeny = true }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			snapshot := validManagedSnapshotForTest()
			test.mutate(&snapshot)
			if err := validateManagedSnapshot(snapshot, managedChartVersionForTest); !errors.Is(err, ErrUnsafeResponse) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestManagedSnapshotAttestationBindsRunningReleaseVersion(t *testing.T) {
	t.Parallel()
	snapshot := validManagedSnapshotForTest()
	if err := validateManagedSnapshot(snapshot, managedChartVersionForTest); err != nil {
		t.Fatal(err)
	}
	if err := validateManagedSnapshot(snapshot, "0.1.0-rc.10"); !errors.Is(err, ErrUnsafeResponse) {
		t.Fatalf("substituted release version error=%v", err)
	}
	for _, invalid := range []string{"", "dev", "v0.1.0-rc.208", "0.1.0+build", "0.1.0-rc.208-extra"} {
		if validManagedChartVersion(invalid) {
			t.Fatalf("invalid managed chart version accepted: %q", invalid)
		}
	}
}
