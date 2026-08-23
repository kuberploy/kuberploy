package environmentfoundation

import (
	"context"
	"errors"
	"testing"
	"time"
)

type memoryEnvironmentCatalog struct{ ids []string }

func (c memoryEnvironmentCatalog) EnvironmentIDs(context.Context) ([]string, error) {
	return append([]string(nil), c.ids...), nil
}

type unavailableFoundationStore struct{ Store }

func (s unavailableFoundationStore) EnsureIntent(context.Context, EnsureRequest) (Intent, error) {
	return Intent{}, ErrNotFound
}

func foundationRuntimeConfig(t *testing.T) RuntimeConfig {
	t.Helper()
	values := map[string]string{RuntimeEnabledEnv: "true", RuntimePlatformBindingIDEnv: testBindingID,
		RuntimePSAVersionEnv: "v1.31", RuntimePollSecondsEnv: "1",
		RuntimeControlPlaneNamespaceEnv: "kuberploy-system", RuntimeObserverServiceAccountEnv: "kuberploy-api"}
	config, err := RuntimeConfigFromLookup(func(name string) (string, bool) { value, ok := values[name]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func TestRuntimeConfigIsStrictDefaultOffAndBindingScoped(t *testing.T) {
	if config, err := RuntimeConfigFromLookup(func(string) (string, bool) { return "", false }); err != nil || config != (RuntimeConfig{}) {
		t.Fatalf("default-off config changed: %#v %v", config, err)
	}
	for name, value := range map[string]string{
		RuntimePlatformBindingIDEnv: testBindingID,
		RuntimePSAVersionEnv:        "v1.31", RuntimePollSecondsEnv: "01",
		RuntimeControlPlaneNamespaceEnv: "kuberploy-system", RuntimeObserverServiceAccountEnv: "kuberploy-api",
	} {
		t.Run(name, func(t *testing.T) {
			values := map[string]string{RuntimeEnabledEnv: "false", name: value}
			if config, err := RuntimeConfigFromLookup(func(key string) (string, bool) { result, ok := values[key]; return result, ok }); err != nil || config != (RuntimeConfig{}) {
				t.Fatal("disabled runtime did not ignore dormant configuration")
			}
		})
	}
	if _, err := RuntimeConfigFromLookup(func(key string) (string, bool) {
		values := map[string]string{RuntimeEnabledEnv: "yes"}
		result, ok := values[key]
		return result, ok
	}); err == nil {
		t.Fatal("invalid enabled flag accepted")
	}
	config := foundationRuntimeConfig(t)
	if config.Profile.PlatformBindingID != testBindingID || config.Profile.ControlPlaneNamespace != "kuberploy-system" ||
		config.Profile.ObserverServiceAccount != "kuberploy-api" || config.Publisher.ConfigDigest != config.Profile.PublisherConfigDigest {
		t.Fatalf("binding authority was not digested into config: %#v", config)
	}
	changed := map[string]string{RuntimeEnabledEnv: "true", RuntimePlatformBindingIDEnv: testIntentID,
		RuntimePSAVersionEnv: "v1.31", RuntimePollSecondsEnv: "1",
		RuntimeControlPlaneNamespaceEnv: "kuberploy-system", RuntimeObserverServiceAccountEnv: "kuberploy-api"}
	other, err := RuntimeConfigFromLookup(func(name string) (string, bool) { value, ok := changed[name]; return value, ok })
	if err != nil || other.Publisher.ConfigDigest == config.Publisher.ConfigDigest {
		t.Fatal("platform binding substitution did not change publisher identity")
	}
	for name, value := range map[string]string{
		RuntimeControlPlaneNamespaceEnv:  "Other_Namespace",
		RuntimeObserverServiceAccountEnv: "other/service-account",
	} {
		t.Run("invalid-"+name, func(t *testing.T) {
			values := map[string]string{RuntimeEnabledEnv: "true", RuntimePlatformBindingIDEnv: testBindingID,
				RuntimePSAVersionEnv: "v1.31", RuntimePollSecondsEnv: "1",
				RuntimeControlPlaneNamespaceEnv: "kuberploy-system", RuntimeObserverServiceAccountEnv: "kuberploy-api"}
			values[name] = value
			if _, configErr := RuntimeConfigFromLookup(func(key string) (string, bool) { result, ok := values[key]; return result, ok }); configErr == nil {
				t.Fatal("invalid observer identity accepted")
			}
		})
	}
}

func TestRuntimeScansAllEnvironmentsAndFailsClosedOnCatalogTOCTOU(t *testing.T) {
	now := time.Date(2026, 8, 9, 14, 0, 0, 0, time.UTC)
	config := foundationRuntimeConfig(t)
	authority := testAuthority()
	second := secondPublisherIdentity()
	store, err := NewMemoryStore([]AuthorityRecord{{testIdentity(), authority}, {second, authority}})
	if err != nil {
		t.Fatal(err)
	}
	publisher := &fakePublisher{identity: config.Publisher, store: store, now: now}
	controller := &Controller{Store: store, Publisher: publisher, Profile: config.Profile,
		WorkerID: testWorker1, WorkerEpoch: 1, WorkLease: time.Minute,
		MinimumBackoff: time.Second, MaximumBackoff: time.Minute, Now: func() time.Time { return now }}
	runtime := &Runtime{Store: store, Catalog: memoryEnvironmentCatalog{[]string{testEnvironmentID, second.EnvironmentID}},
		Controller: controller, Config: config, WorkerEpoch: 1, StartedAt: now, Now: func() time.Time { return now }}
	runtime.WorkerEpoch = 2
	if err = runtime.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("runtime accepted readiness epoch different from controller lease epoch: %v", err)
	}
	runtime.WorkerEpoch = 1
	if err = runtime.RunOnce(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("partially reconciled catalog became ready: %v", err)
	}
	if err = runtime.RunOnce(context.Background()); err != nil {
		t.Fatalf("exact catalog did not become ready: %v", err)
	}
	probe := &RuntimeReadinessProbe{Store: store, Catalog: runtime.Catalog, Config: config, Now: func() time.Time { return now }}
	if err = probe.Probe(context.Background()); err != nil {
		t.Fatalf("exact readiness rejected: %v", err)
	}
	// Simulate a stale catalog list racing a newly inserted authoritative
	// environment. ExactReady independently compares the environment count in
	// its own snapshot, so the old list cannot keep Argo ready.
	probe.Catalog = memoryEnvironmentCatalog{[]string{testEnvironmentID}}
	if err = probe.Probe(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("catalog/count TOCTOU remained ready: %v", err)
	}
}

func TestEnsureIntentRejectsConfiguredBindingSubstitution(t *testing.T) {
	profile := testProfile()
	profile.PlatformBindingID = testIntentID
	store, err := NewMemoryStore([]AuthorityRecord{{testIdentity(), testAuthority()}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.EnsureIntent(context.Background(), EnsureRequest{testIntentID, testEnvironmentID, profile, time.Now().UTC()}); !errors.Is(err, ErrConflict) {
		t.Fatalf("configured platform binding substitution accepted: %v", err)
	}
}

func TestRuntimeKeepsInitialUnreadyPlatformBindingRetryable(t *testing.T) {
	now := time.Date(2026, 8, 11, 1, 0, 0, 0, time.UTC)
	config := foundationRuntimeConfig(t)
	base, err := NewMemoryStore([]AuthorityRecord{{testIdentity(), testAuthority()}})
	if err != nil {
		t.Fatal(err)
	}
	store := unavailableFoundationStore{Store: base}
	publisher := &fakePublisher{identity: config.Publisher, store: store, now: now}
	controller := &Controller{Store: store, Publisher: publisher, Profile: config.Profile,
		WorkerID: testWorker1, WorkerEpoch: 1, WorkLease: time.Minute,
		MinimumBackoff: time.Second, MaximumBackoff: time.Minute, Now: func() time.Time { return now }}
	runtime := &Runtime{Store: store, Catalog: memoryEnvironmentCatalog{[]string{testEnvironmentID}},
		Controller: controller, Config: config, WorkerEpoch: 1, StartedAt: now, Now: func() time.Time { return now }}
	if err = runtime.RunOnce(context.Background()); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrNotFound) {
		t.Fatalf("initial unready binding was terminal: %v", err)
	}
}

func TestRuntimeReportsRetryableReconciliationFailure(t *testing.T) {
	now := time.Date(2026, 8, 11, 1, 0, 0, 0, time.UTC)
	config := foundationRuntimeConfig(t)
	base, err := NewMemoryStore([]AuthorityRecord{{testIdentity(), testAuthority()}})
	if err != nil {
		t.Fatal(err)
	}
	store := unavailableFoundationStore{Store: base}
	publisher := &fakePublisher{identity: config.Publisher, store: store, now: now}
	controller := &Controller{Store: store, Publisher: publisher, Profile: config.Profile,
		WorkerID: testWorker1, WorkerEpoch: 1, WorkLease: time.Minute,
		MinimumBackoff: time.Second, MaximumBackoff: time.Minute, Now: func() time.Time { return now }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reported := make(chan error, 1)
	runtime := &Runtime{Store: store, Catalog: memoryEnvironmentCatalog{[]string{testEnvironmentID}},
		Controller: controller, Config: config, WorkerEpoch: 1, StartedAt: now, Now: func() time.Time { return now },
		ReportError: func(err error) {
			reported <- err
			cancel()
		}}
	if err = runtime.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("runtime did not stop after reporter cancelled context: %v", err)
	}
	select {
	case err = <-reported:
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("reported error lost retryable classification: %v", err)
		}
	default:
		t.Fatal("retryable reconciliation failure was not reported")
	}
}
