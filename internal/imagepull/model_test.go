package imagepull

import (
	"strings"
	"testing"
	"time"
)

const (
	testEnvironmentID = "11111111-1111-4111-8111-111111111111"
	testTargetID      = "22222222-2222-4222-8222-222222222222"
)

func testRuntimeConfig() RuntimeConfig {
	config := DefaultRuntimeConfig()
	config.Enabled = true
	config.Namespaces = []string{"tenant-a-dev", "tenant-a-prod"}
	config.Profiles = []Profile{{
		Name: "managed-main", TargetID: testTargetID, RegistryServer: "registry.example.test:5000",
		CredentialRef: "runtime-pull/main", Revision: 3,
		SourceSecretRef: "registry-pull-main", SourceSecretKey: ".dockerconfigjson",
	}}
	return config
}

func TestRuntimeConfigIsCanonicalBoundedAndSecretFree(t *testing.T) {
	config := testRuntimeConfig()
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	digest, err := config.Digest()
	if err != nil || !digestPattern(digest) {
		t.Fatalf("digest=%q err=%v", digest, err)
	}
	if path := config.Profiles[0].SourcePath(); path != SourceRoot+"/managed-main/dockerconfigjson" {
		t.Fatalf("source path=%q", path)
	}
	if strings.Contains(digest, "registry-pull-main") {
		t.Fatalf("digest unexpectedly contains serialized config: %q", digest)
	}

	for name, mutate := range map[string]func(*RuntimeConfig){
		"unsorted namespaces":      func(value *RuntimeConfig) { value.Namespaces = []string{"z", "a"} },
		"invalid namespace prefix": func(value *RuntimeConfig) { value.NamespacePrefixes = []string{"tenant"} },
		"duplicate target": func(value *RuntimeConfig) {
			other := value.Profiles[0]
			other.Name = "second"
			other.CredentialRef = "runtime-pull/second"
			other.SourceSecretRef = "registry-pull-second"
			value.Profiles = append(value.Profiles, other)
		},
		"shared source secret": func(value *RuntimeConfig) {
			other := value.Profiles[0]
			other.Name = "second"
			other.TargetID = "33333333-3333-4333-8333-333333333333"
			other.CredentialRef = "runtime-pull/second"
			value.Profiles = append(value.Profiles, other)
		},
		"URL server":                  func(value *RuntimeConfig) { value.Profiles[0].RegistryServer = "https://registry.example.test" },
		"uppercase server":            func(value *RuntimeConfig) { value.Profiles[0].RegistryServer = "Registry.example.test" },
		"port out of range":           func(value *RuntimeConfig) { value.Profiles[0].RegistryServer = "registry.example.test:65536" },
		"zero revision":               func(value *RuntimeConfig) { value.Profiles[0].Revision = 0 },
		"unsafe secret key":           func(value *RuntimeConfig) { value.Profiles[0].SourceSecretKey = "../token" },
		"heartbeat at lease boundary": func(value *RuntimeConfig) { value.HeartbeatInterval = value.WorkLease / 2 },
	} {
		t.Run(name, func(t *testing.T) {
			value := testRuntimeConfig()
			mutate(&value)
			if err := value.Validate(); err == nil {
				t.Fatalf("invalid config accepted: %#v", value)
			}
		})
	}
}

func TestDesiredArtifactIsServerDerivedAndRevisioned(t *testing.T) {
	config := testRuntimeConfig()
	desired, err := Desired(config, testEnvironmentID, "tenant-a-dev", testTargetID)
	if err != nil {
		t.Fatal(err)
	}
	if desired.SecretName != SecretName("tenant-a-dev", testTargetID, 3) || desired.PullCredentialRef != "runtime-pull/main" ||
		desired.ProfileName != "managed-main" || len(desired.SecretName) != len("kuberploy-pull-")+24 {
		t.Fatalf("desired=%#v", desired)
	}
	rotated := config
	rotated.Profiles = append([]Profile(nil), config.Profiles...)
	rotated.Profiles[0].Revision = 4
	next, err := Desired(rotated, testEnvironmentID, "tenant-a-dev", testTargetID)
	if err != nil || next.SecretName == desired.SecretName {
		t.Fatalf("rotation did not produce a distinct exact name: next=%#v err=%v", next, err)
	}
	otherNamespace, err := Desired(config, testEnvironmentID, "tenant-a-prod", testTargetID)
	if err != nil || otherNamespace.SecretName != desired.SecretName {
		t.Fatalf("namespaced copies did not retain one resourceNames-safe identity: next=%#v err=%v", otherNamespace, err)
	}
	prefixConfig := config
	prefixConfig.Namespaces = nil
	prefixConfig.NamespacePrefixes = []string{"kp-"}
	if prefixed, prefixErr := Desired(prefixConfig, testEnvironmentID, "kp-project-prod", testTargetID); prefixErr != nil || prefixed.SecretName != desired.SecretName {
		t.Fatalf("managed namespace prefix was not accepted exactly: desired=%#v err=%v", prefixed, prefixErr)
	}
	for _, input := range []struct{ environment, namespace, target string }{
		{"invalid", "tenant-a-dev", testTargetID},
		{testEnvironmentID, "unmanaged", testTargetID},
		{testEnvironmentID, "tenant-a-dev", "33333333-3333-4333-8333-333333333333"},
	} {
		if _, err = Desired(config, input.environment, input.namespace, input.target); err == nil {
			t.Fatalf("unsafe desired artifact accepted: %#v", input)
		}
	}
	if SecretName("not/a-namespace", testTargetID, 3) != "" || SecretName("Tenant-A", testTargetID, 3) != "" {
		t.Fatal("non-canonical namespaces must not derive RBAC resource names")
	}
}

func TestRuntimePullIdentityAcceptsPlatformUUIDv7(t *testing.T) {
	config := testRuntimeConfig()
	config.Profiles[0].TargetID = "0198a7f4-3b22-7a10-8f42-0123456789ab"
	if err := config.Validate(); err != nil {
		t.Fatalf("platform UUIDv7 target rejected: %v", err)
	}
	desired, err := Desired(config, "0198a7f4-3b22-7a10-8f42-ba9876543210", "tenant-a-dev", config.Profiles[0].TargetID)
	if err != nil {
		t.Fatalf("platform UUIDv7 environment rejected: %v", err)
	}
	if desired.EnvironmentID != "0198a7f4-3b22-7a10-8f42-ba9876543210" || desired.RegistryTargetID != config.Profiles[0].TargetID || desired.SecretName == "" {
		t.Fatalf("UUIDv7 identity was not preserved exactly: %#v", desired)
	}
}

func TestArtifactAndReadinessRejectPartialMetadata(t *testing.T) {
	desired, err := Desired(testRuntimeConfig(), testEnvironmentID, "tenant-a-dev", testTargetID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 9, 1, 2, 3, 0, time.UTC)
	artifact := Artifact{DesiredArtifact: desired, Active: true, State: StateAwaiting, NextObservationAt: now, CreatedAt: now, UpdatedAt: now}
	if err = artifact.Validate(); err != nil {
		t.Fatal(err)
	}
	observed := now.Add(time.Minute)
	artifact.State = StateReady
	artifact.LastObservedAt = &observed
	artifact.ObservedUID = "44444444-4444-4444-8444-444444444444"
	artifact.ObservedResourceVersion = "123"
	artifact.UpdatedAt = observed
	artifact.NextObservationAt = observed.Add(time.Minute)
	if err = artifact.Validate(); err != nil {
		t.Fatal(err)
	}
	artifact.ObservedResourceVersion = ""
	if err = artifact.Validate(); err == nil {
		t.Fatal("partial Kubernetes observation accepted")
	}
	artifact.ObservedResourceVersion = "123"
	artifact.ObservedUID = "0198a7f4-3b22-7a10-8f42-0123456789ab"
	if err = artifact.Validate(); err == nil {
		t.Fatal("non-Kubernetes UUID version was accepted as an observed Secret UID")
	}

	digest, _ := testRuntimeConfig().Digest()
	readiness := Readiness{WorkerID: "registry-pull-worker:one", WorkerEpoch: 1, Contract: RuntimeContract,
		ConfigDigest: digest, ProfileCount: 1, StartedAt: now, ObservedAt: now, LeaseUntil: now.Add(time.Minute)}
	if err = readiness.Validate(); err != nil {
		t.Fatal(err)
	}
	readiness.ProfileCount = 0
	if err = readiness.Validate(); err == nil {
		t.Fatal("empty profile readiness accepted")
	}
}
