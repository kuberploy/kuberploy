package registry

import (
	"testing"
	"time"
)

func validRegistryRuntimeConfig(t *testing.T) RuntimeConfig {
	t.Helper()
	values := validRuntimeEnvironment()
	config, err := RuntimeConfigFromLookup(func(name string) (string, bool) { value, ok := values[name]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func TestRuntimeDigestBindsEveryRuntimeSetting(t *testing.T) {
	config := validRegistryRuntimeConfig(t)
	baseline, err := config.RuntimeDigest()
	if err != nil {
		t.Fatal(err)
	}
	if identity, identityErr := RuntimeIdentityForConfig(config); identityErr != nil || identity.ConfigDigest != baseline || identity.ContractVersion != ManagedRegistryRuntimeContract {
		t.Fatalf("identity=%+v err=%v", identity, identityErr)
	}
	mutations := []func(*RuntimeConfig){
		func(value *RuntimeConfig) { value.TargetID = "22222222-2222-4222-8222-222222222222" },
		func(value *RuntimeConfig) { value.TargetName = "Other registry" },
		func(value *RuntimeConfig) {
			value.Endpoint = "http://other-registry.kuberploy-registry.svc.cluster.local:5000"
		},
		func(value *RuntimeConfig) { value.RepositoryPrefix = "other" },
		func(value *RuntimeConfig) { value.PullCredentialRef = "other-pull" },
		func(value *RuntimeConfig) { value.PushCredentialRef = "other-push" },
		func(value *RuntimeConfig) { value.CacheCredentialRef = "other-cache" },
		func(value *RuntimeConfig) { value.CredentialRef = "operator/other-registry" },
		func(value *RuntimeConfig) {
			value.AllowPlainHTTP = false
			value.Endpoint = "https://kuberploy-registry.example.test"
		},
		func(value *RuntimeConfig) { value.Namespace = "other-registry" },
		func(value *RuntimeConfig) { value.Deployment = "other-registry" },
		func(value *RuntimeConfig) { value.PersistentVolumeClaim = "other-registry" },
		func(value *RuntimeConfig) { value.RegistryConfigMap = "other-registry-config" },
		func(value *RuntimeConfig) { value.HelperServiceAccount = "other-maintenance" },
		func(value *RuntimeConfig) {
			value.HelperImage = "ghcr.io/kuberploy/kuberploy-worker@sha256:" + repeatHex("b", 64)
		},
		func(value *RuntimeConfig) { value.ObservationInterval = 301 * time.Second },
	}
	for index, mutate := range mutations {
		changed := config
		mutate(&changed)
		digest, digestErr := changed.RuntimeDigest()
		if digestErr != nil {
			t.Fatalf("mutation %d invalid: %v", index, digestErr)
		}
		if digest == baseline {
			t.Fatalf("mutation %d did not change digest: %+v", index, changed)
		}
	}
}

func TestRuntimeReadinessLeaseAndObservationValidation(t *testing.T) {
	config := validRegistryRuntimeConfig(t)
	identity, err := RuntimeIdentityForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 9, 9, 0, 0, 0, time.UTC)
	observation := RuntimeWorkerObservation{WorkerID: "worker-registry-ready-01234567", RuntimeIdentity: identity, StartedAt: now, ObservedAt: now}
	if err = observation.Validate(); err != nil {
		t.Fatal(err)
	}
	lease := RuntimeReadinessLease{RuntimeWorkerObservation: observation, Epoch: 1, Until: now.Add(ManagedRegistryReadinessLease)}
	if err = lease.Validate(); err != nil {
		t.Fatal(err)
	}
	lease.Epoch = 0
	if lease.Validate() == nil {
		t.Fatal("zero epoch was accepted")
	}
}
