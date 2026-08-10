package imagepull

import (
	"errors"
	"testing"
	"time"
)

func readinessTestConfig(t *testing.T) RuntimeConfig {
	t.Helper()
	config := DefaultRuntimeConfig()
	config.Enabled = true
	config.Namespaces = []string{"runtime-a"}
	config.Profiles = []Profile{{Name: "registry-a", TargetID: "11111111-1111-4111-8111-111111111111",
		RegistryServer: "registry.example.test", CredentialRef: "pull/registry-a", Revision: 1,
		SourceSecretRef: "registry-a-pull", SourceSecretKey: ".dockerconfigjson"}}
	if config.Validate() != nil {
		t.Fatal("invalid test configuration")
	}
	return config
}

func TestReadinessProbeRequiresExactFreshWorkerIdentity(t *testing.T) {
	config := readinessTestConfig(t)
	store := NewMemoryStore()
	now := time.Date(2026, 8, 9, 4, 0, 0, 0, time.UTC)
	probe := &ReadinessProbe{Store: store, Config: config, Now: func() time.Time { return now }}
	if err := probe.Probe(t.Context()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("probe without worker = %v", err)
	}
	digest, err := config.Digest()
	if err != nil {
		t.Fatal(err)
	}
	readiness := Readiness{WorkerID: "runtime-pull-worker:test", WorkerEpoch: 1, Contract: RuntimeContract,
		ConfigDigest: digest, ProfileCount: len(config.Profiles), StartedAt: now, ObservedAt: now,
		LeaseUntil: now.Add(config.ReadinessMaxAge)}
	if err = store.RecordReadiness(t.Context(), readiness); err != nil {
		t.Fatal(err)
	}
	if err = probe.Probe(t.Context()); err != nil {
		t.Fatalf("fresh exact worker = %v", err)
	}

	mismatch := config
	mismatch.Profiles = append([]Profile(nil), config.Profiles...)
	mismatch.Profiles[0].Revision++
	mismatchProbe := &ReadinessProbe{Store: store, Config: mismatch, Now: probe.Now}
	if err = mismatchProbe.Probe(t.Context()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("mismatched profile revision = %v", err)
	}

	probe.Now = func() time.Time { return readiness.LeaseUntil.Add(time.Nanosecond) }
	if err = probe.Probe(t.Context()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expired worker = %v", err)
	}
}

func TestReadinessProbeRejectsDisabledAndPartialConfiguration(t *testing.T) {
	store := NewMemoryStore()
	for name, probe := range map[string]*ReadinessProbe{
		"nil":      nil,
		"disabled": {Store: store},
		"no-store": {Config: readinessTestConfig(t)},
	} {
		t.Run(name, func(t *testing.T) {
			if err := probe.Probe(t.Context()); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("Probe() = %v", err)
			}
		})
	}
}
