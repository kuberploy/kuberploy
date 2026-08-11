package observability

import (
	"context"
	"errors"
	"testing"
)

const managedChartVersionForTest = "0.1.0-rc.61"

type fixedManagedObserver struct {
	snapshot ManagedMonitoringSnapshot
	err      error
}

func (o fixedManagedObserver) ObserveManagedMonitoring(context.Context) (ManagedMonitoringSnapshot, error) {
	return o.snapshot, o.err
}

type fixedManagedPrometheus struct {
	probeErr error
	rulesErr error
	probes   int
	rules    int
}

func (p *fixedManagedPrometheus) Probe(context.Context) error {
	p.probes++
	return p.probeErr
}

func (p *fixedManagedPrometheus) ProbeManagedRules(context.Context) error {
	p.rules++
	return p.rulesErr
}

func validManagedSnapshotForTest() ManagedMonitoringSnapshot {
	return ManagedMonitoringSnapshot{
		ProfileData: expectedManagedProfile(managedChartVersionForTest), ProfileImmutable: true,
		OperatorName: ManagedMonitoringOperatorName, OperatorContainer: ManagedMonitoringOperatorContainer,
		OperatorImage: ManagedMonitoringOperatorImage, OperatorArgumentsSHA256: ManagedMonitoringOperatorArgsSHA256,
		OperatorGeneration: 4, OperatorObservedGeneration: 4, OperatorDesiredReplicas: 1, OperatorAvailableReplicas: 1,
		RuleName: ManagedMonitoringRuleName, RuleGeneration: 7, RuleSpecSHA256: ManagedMonitoringRuleSpecSHA256,
	}
}

func TestManagedReadinessRequiresAttestationProbeAndRules(t *testing.T) {
	t.Parallel()
	prometheus := &fixedManagedPrometheus{}
	probe := &ManagedReadinessProbe{Prometheus: prometheus, Observer: fixedManagedObserver{snapshot: validManagedSnapshotForTest()}, ExpectedChartVersion: managedChartVersionForTest}
	if err := probe.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if prometheus.probes != 1 || prometheus.rules != 1 {
		t.Fatalf("probe calls=%d rule calls=%d", prometheus.probes, prometheus.rules)
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
	for _, invalid := range []string{"", "dev", "v0.1.0-rc.61", "0.1.0+build", "0.1.0-rc.61-extra"} {
		if validManagedChartVersion(invalid) {
			t.Fatalf("invalid managed chart version accepted: %q", invalid)
		}
	}
}
