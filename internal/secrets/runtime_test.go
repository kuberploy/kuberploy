package secrets

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/id"
)

type runtimeTestCertificateSource struct {
	fingerprint string
	err         error
	calls       int
}

func (s *runtimeTestCertificateSource) ActivePublicKey(context.Context, time.Time) (sealingPublicKey, error) {
	s.calls++
	if s.err != nil {
		return sealingPublicKey{}, s.err
	}
	return sealingPublicKey{fingerprint: s.fingerprint}, nil
}

func testRuntimeConfig() RuntimeConfig {
	config := DefaultRuntimeConfig()
	config.Enabled = true
	config.Namespaces = []string{"runtime-test"}
	config.FingerprintSecretRef = "runtime-secret-fingerprint"
	config.SealingCertificateSecretRef = "sealed-secrets-key"
	config.WorkLease = 30 * time.Second
	config.HeartbeatInterval = 5 * time.Second
	return config
}

func testRuntimeIdentity(t *testing.T, config RuntimeConfig) RuntimeIdentity {
	t.Helper()
	identity, err := RuntimeIdentityForConfig(config, "sha256:"+strings.Repeat("d", 64))
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func createAwaitingSealed(t *testing.T, store Store, provider *fakeProviders, name, idempotency string) MutationResult {
	t.Helper()
	request := createRequest(t, ProviderSealedSecrets, "write-only-value", idempotency)
	request.Name = name
	result, err := testService(store, provider).Create(context.Background(), request)
	if err != nil || result.Version.State != VersionAwaitingReadiness {
		t.Fatalf("create awaiting result=%#v err=%v", result, err)
	}
	return result
}

func TestRuntimeConfigIdentityIsExactAndNamespaceAllowlisted(t *testing.T) {
	config := testRuntimeConfig()
	identity := testRuntimeIdentity(t, config)
	if config.Validate() != nil || identity.Validate() != nil || !config.AllowsNamespace("runtime-test") || config.AllowsNamespace("other") {
		t.Fatalf("config=%#v identity=%#v", config, identity)
	}
	mutations := []func(*RuntimeConfig){
		func(value *RuntimeConfig) { value.Namespaces = []string{"other"} },
		func(value *RuntimeConfig) { value.NamespacePrefixes = []string{"kp-"} },
		func(value *RuntimeConfig) { value.FingerprintSecretRef = "other-hmac" },
		func(value *RuntimeConfig) { value.FingerprintSecretKey = "other.key" },
		func(value *RuntimeConfig) { value.FingerprintKeyID = "other-key" },
		func(value *RuntimeConfig) { value.SealingCertificateSecretRef = "other-cert" },
		func(value *RuntimeConfig) { value.SealingCertificateSecretKey = "other.crt" },
		func(value *RuntimeConfig) { value.PollInterval++ },
		func(value *RuntimeConfig) { value.WorkLease++ },
		func(value *RuntimeConfig) { value.HeartbeatInterval++ },
		func(value *RuntimeConfig) { value.IdleDelay++ },
		func(value *RuntimeConfig) { value.MinimumBackoff++ },
		func(value *RuntimeConfig) { value.MaximumBackoff++ },
	}
	for index, mutate := range mutations {
		changed := config
		changed.Namespaces = append([]string(nil), config.Namespaces...)
		changed.NamespacePrefixes = append([]string(nil), config.NamespacePrefixes...)
		mutate(&changed)
		actual := testRuntimeIdentity(t, changed)
		if actual.ConfigDigest == identity.ConfigDigest {
			t.Fatalf("mutation %d did not change config digest", index)
		}
	}
	invalid := config
	invalid.Namespaces = []string{"runtime-test", "runtime-test"}
	if invalid.Validate() == nil {
		t.Fatal("duplicate namespaces accepted")
	}
	invalid = config
	invalid.Namespaces = []string{"z-runtime", "a-runtime"}
	if invalid.Validate() == nil {
		t.Fatal("unsorted namespaces accepted")
	}
	prefixOnly := config
	prefixOnly.Namespaces = nil
	prefixOnly.NamespacePrefixes = []string{"kp-"}
	if prefixOnly.Validate() != nil || !prefixOnly.AllowsNamespace("kp-project-production") || prefixOnly.AllowsNamespace("kube-system") || prefixOnly.AllowsNamespace("kp-") {
		t.Fatalf("managed Environment prefix policy failed: %#v", prefixOnly)
	}
	if _, err := RuntimeIdentityForConfig(config, "sha256:"+strings.Repeat("e", 63)); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("invalid public key fingerprint: %v", err)
	}
}

func TestWorkerRuntimeIdentityUsesOnlyPublicCertificateAndExactMetadata(t *testing.T) {
	config := testRuntimeConfig()
	source := &runtimeTestCertificateSource{fingerprint: "sha256:" + strings.Repeat("f", 64)}
	identity, err := runtimeIdentityFromSealingCertificate(t.Context(), config, testTime, source)
	if err != nil || source.calls != 1 || identity.FingerprintKeyID != config.FingerprintKeyID ||
		identity.SealingKeyFingerprint != source.fingerprint {
		t.Fatalf("identity=%#v calls=%d err=%v", identity, source.calls, err)
	}
	changed := config
	changed.FingerprintSecretRef = "different-api-only-hmac"
	changedIdentity, err := runtimeIdentityFromSealingCertificate(t.Context(), changed, testTime, source)
	if err != nil || changedIdentity.ConfigDigest == identity.ConfigDigest {
		t.Fatalf("worker omitted exact HMAC metadata from identity: %#v err=%v", changedIdentity, err)
	}
	failing := &runtimeTestCertificateSource{err: errors.New("certificate unavailable")}
	if _, err = runtimeIdentityFromSealingCertificate(t.Context(), config, testTime, failing); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("certificate failure error=%v", err)
	}
}

func TestRuntimePolicyDigestIsCanonicalMetadataOnly(t *testing.T) {
	disabled, err := RuntimePolicyDigest(RuntimeConfig{})
	if err != nil || !digestRE.MatchString(disabled) {
		t.Fatalf("disabled digest=%q err=%v", disabled, err)
	}
	dormant := DefaultRuntimeConfig()
	if _, err = RuntimePolicyDigest(dormant); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("dormant disabled configuration error=%v", err)
	}
	config := testRuntimeConfig()
	baseline, err := RuntimePolicyDigest(config)
	if err != nil || !digestRE.MatchString(baseline) {
		t.Fatalf("enabled digest=%q err=%v", baseline, err)
	}
	for name, mutate := range map[string]func(*RuntimeConfig){
		"namespace":       func(c *RuntimeConfig) { c.Namespaces = []string{"other-runtime"} },
		"fingerprint key": func(c *RuntimeConfig) { c.FingerprintSecretKey = "other-key" },
		"certificate key": func(c *RuntimeConfig) { c.SealingCertificateSecretKey = "other-cert" },
		"poll interval":   func(c *RuntimeConfig) { c.PollInterval += time.Second },
	} {
		t.Run(name, func(t *testing.T) {
			changed := config
			changed.Namespaces = append([]string(nil), config.Namespaces...)
			mutate(&changed)
			digest, digestErr := RuntimePolicyDigest(changed)
			if digestErr != nil {
				t.Fatal(digestErr)
			}
			if digest == baseline {
				t.Fatalf("metadata mutation was omitted from policy digest: %s", name)
			}
		})
	}
}

func TestMemoryRuntimeCrashReclaimAndEpochFence(t *testing.T) {
	store := NewMemoryStore()
	provider := &fakeProviders{}
	created := createAwaitingSealed(t, store, provider, "database", "runtime-crash-0001")
	config, now := testRuntimeConfig(), testTime.Add(time.Second)
	identity := testRuntimeIdentity(t, config)
	first, err := store.ClaimRuntimeSecret(context.Background(), identity, "runtime-worker-alpha", config.Namespaces, config.NamespacePrefixes, now, config.WorkLease)
	if err != nil || first.Lease.Epoch != 1 || first.Version.ID != created.Version.ID {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	if _, err = store.ClaimRuntimeSecret(context.Background(), identity, "runtime-worker-beta1", config.Namespaces, config.NamespacePrefixes, now.Add(time.Second), config.WorkLease); !errors.Is(err, ErrNotFound) {
		t.Fatalf("concurrent claim: %v", err)
	}
	reclaimedAt := first.Lease.Until.Add(time.Second)
	second, err := store.ClaimRuntimeSecret(context.Background(), identity, "runtime-worker-beta1", config.Namespaces, config.NamespacePrefixes, reclaimedAt, config.WorkLease)
	if err != nil || second.Lease.Epoch != first.Lease.Epoch+1 || second.Lease.Owner == first.Lease.Owner {
		t.Fatalf("reclaimed=%#v err=%v", second, err)
	}
	staleEvent := Event{ID: id.New(), BindingID: first.Binding.ID, VersionID: first.Version.ID,
		Kind: EventVersionActive, RequestID: "runtime-stale-apply", OccurredAt: reclaimedAt.Add(time.Second)}
	if _, _, err = store.ApplyRuntimeSecretReady(context.Background(), first.Lease, staleEvent, staleEvent.OccurredAt); !errors.Is(err, ErrRuntimeLeaseLost) {
		t.Fatalf("stale apply: %v", err)
	}
	readyAt := reclaimedAt.Add(2 * time.Second)
	readyEvent := Event{ID: id.New(), BindingID: second.Binding.ID, VersionID: second.Version.ID,
		Kind: EventVersionActive, RequestID: "runtime-ready-apply", OccurredAt: readyAt}
	binding, version, err := store.ApplyRuntimeSecretReady(context.Background(), second.Lease, readyEvent, readyAt)
	if err != nil || binding.State != BindingReady || version.State != VersionActive {
		t.Fatalf("binding=%#v version=%#v err=%v", binding, version, err)
	}
	events, err := store.PendingEvents(context.Background(), 100)
	if err != nil || !eventExists(events, readyEvent.ID) || eventExists(events, staleEvent.ID) {
		t.Fatalf("events=%#v err=%v", events, err)
	}
}

func TestMemoryRuntimePendingBackoffReleaseAndStaleHeartbeat(t *testing.T) {
	store := NewMemoryStore()
	created := createAwaitingSealed(t, store, &fakeProviders{}, "queue", "runtime-backoff-001")
	config, now := testRuntimeConfig(), testTime.Add(time.Second)
	identity := testRuntimeIdentity(t, config)
	first, err := store.ClaimRuntimeSecret(context.Background(), identity, "runtime-worker-alpha", config.Namespaces, config.NamespacePrefixes, now, config.WorkLease)
	if err != nil {
		t.Fatal(err)
	}
	nextAt := now.Add(4 * time.Second)
	if err = store.ApplyRuntimeSecretPending(context.Background(), first.Lease,
		RuntimePendingOutcome{FailureCode: "provider-observe-failed", NextAt: nextAt}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ClaimRuntimeSecret(context.Background(), identity, "runtime-worker-beta1", config.Namespaces, config.NamespacePrefixes, nextAt.Add(-time.Millisecond), config.WorkLease); !errors.Is(err, ErrNotFound) {
		t.Fatalf("claimed before backoff: %v", err)
	}
	second, err := store.ClaimRuntimeSecret(context.Background(), identity, "runtime-worker-beta1", config.Namespaces, config.NamespacePrefixes, nextAt, config.WorkLease)
	if err != nil || second.Lease.Epoch != 2 || second.ConsecutiveFailures != 1 || second.Version.ID != created.Version.ID {
		t.Fatalf("second=%#v err=%v", second, err)
	}
	if _, err = store.HeartbeatRuntimeSecret(context.Background(), first.Lease, now.Add(2*time.Second), config.WorkLease); !errors.Is(err, ErrRuntimeLeaseLost) {
		t.Fatalf("stale heartbeat: %v", err)
	}
	if err = store.ApplyRuntimeSecretPending(context.Background(), second.Lease,
		RuntimePendingOutcome{NextAt: nextAt.Add(config.PollInterval)}, nextAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	cursor := store.runtime[created.Version.ID]
	store.mu.Unlock()
	if cursor.ConsecutiveFailures != 0 || cursor.LastFailureCode != "" {
		t.Fatalf("healthy pending did not reset failure backoff: %#v", cursor)
	}
}

func TestMemoryRuntimeClaimsOnlyStrictSealedSecretsInAllowedNamespace(t *testing.T) {
	store := NewMemoryStore()
	provider := &fakeProviders{}
	_ = createAwaitingSealed(t, store, provider, "sealed", "runtime-strict-0001")
	externalRequest := createRequest(t, ProviderExternalSecrets, "external-value", "runtime-external-1")
	externalRequest.Name = "external"
	external, err := testService(store, provider).Create(context.Background(), externalRequest)
	if err != nil || external.Version.State != VersionAwaitingReadiness {
		t.Fatal(err)
	}
	config, now := testRuntimeConfig(), testTime.Add(time.Second)
	identity := testRuntimeIdentity(t, config)
	if _, err = store.ClaimRuntimeSecret(context.Background(), identity, "runtime-worker-alpha", []string{"other-namespace"}, nil, now, config.WorkLease); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross namespace claim: %v", err)
	}
	work, err := store.ClaimRuntimeSecret(context.Background(), identity, "runtime-worker-alpha", config.Namespaces, config.NamespacePrefixes, now, config.WorkLease)
	if err != nil || work.Version.Provider != ProviderSealedSecrets {
		t.Fatalf("work=%#v err=%v", work, err)
	}
	failedAt := now.Add(time.Second)
	failure := Event{ID: id.New(), BindingID: work.Binding.ID, VersionID: work.Version.ID,
		Kind: EventVersionFailed, RequestID: "runtime-terminal-fail", OccurredAt: failedAt}
	version, err := store.ApplyRuntimeSecretFailed(context.Background(), work.Lease, "sealed-secret-sync-failed", failure, failedAt)
	if err != nil || version.State != VersionFailed || version.FailureCode != "sealed-secret-sync-failed" {
		t.Fatalf("version=%#v err=%v", version, err)
	}
	if _, err = store.ClaimRuntimeSecret(context.Background(), identity, "runtime-worker-alpha", config.Namespaces, config.NamespacePrefixes, failedAt.Add(time.Second), config.WorkLease); !errors.Is(err, ErrNotFound) {
		t.Fatalf("external secret entered strict runtime: %v", err)
	}
}

func TestMemoryRuntimeReadinessIsFreshExactAndEpochFenced(t *testing.T) {
	store := NewMemoryStore()
	config, now := testRuntimeConfig(), testTime.Add(time.Minute)
	identity := testRuntimeIdentity(t, config)
	observation := RuntimeWorkerObservation{WorkerID: "runtime-worker-alpha", Identity: identity, StartedAt: now, ObservedAt: now}
	first, err := store.AcquireRuntimeSecretReadiness(context.Background(), observation, RuntimeSecretReadinessLease)
	if err != nil || first.Epoch != 1 {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	second, err := store.AcquireRuntimeSecretReadiness(context.Background(), observation, RuntimeSecretReadinessLease)
	if err != nil || second.Epoch != 2 {
		t.Fatalf("second=%#v err=%v", second, err)
	}
	if _, err = store.HeartbeatRuntimeSecretReadiness(context.Background(), first, now.Add(time.Second), RuntimeSecretReadinessLease); !errors.Is(err, ErrRuntimeLeaseLost) {
		t.Fatalf("stale readiness heartbeat: %v", err)
	}
	second, err = store.HeartbeatRuntimeSecretReadiness(context.Background(), second, now.Add(2*time.Second), RuntimeSecretReadinessLease)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.RuntimeSecretReady(context.Background(), identity, now.Add(3*time.Second), RuntimeSecretHeartbeatMaxAge); err != nil {
		t.Fatalf("exact readiness: %v", err)
	}
	mismatch := identity
	mismatch.ConfigDigest = "sha256:" + strings.Repeat("f", 64)
	if err = store.RuntimeSecretReady(context.Background(), mismatch, now.Add(3*time.Second), RuntimeSecretHeartbeatMaxAge); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("mismatched readiness: %v", err)
	}
	if err = store.RuntimeSecretReady(context.Background(), identity, second.ObservedAt.Add(RuntimeSecretHeartbeatMaxAge+time.Second), RuntimeSecretHeartbeatMaxAge); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("stale readiness: %v", err)
	}
}

func TestRuntimeControllerAppliesOnlySafeObservation(t *testing.T) {
	store := NewMemoryStore()
	provider := &fakeProviders{}
	created := createAwaitingSealed(t, store, provider, "controller", "runtime-control-001")
	config := testRuntimeConfig()
	controller := RuntimeController{Store: store, Observer: provider, Config: config, Identity: testRuntimeIdentity(t, config),
		WorkerID: "runtime-worker-alpha", Now: func() time.Time { return testTime.Add(time.Second) },
		ResolveIdentity: func(_ context.Context, _ RuntimeConfig, _ time.Time) (RuntimeIdentity, error) {
			return testRuntimeIdentity(t, config), nil
		}}
	worked, err := controller.Reconcile(context.Background())
	if err != nil || !worked {
		t.Fatalf("worked=%t err=%v", worked, err)
	}
	version, err := store.Version(context.Background(), created.Version.ID)
	if err != nil || version.State != VersionActive {
		t.Fatalf("version=%#v err=%v", version, err)
	}

	store = NewMemoryStore()
	provider = &fakeProviders{observeErr: errors.New("provider transport included-sensitive-detail")}
	created = createAwaitingSealed(t, store, provider, "deferred", "runtime-control-002")
	controller.Store, controller.Observer = store, provider
	worked, err = controller.Reconcile(context.Background())
	if !worked || !errors.Is(err, ErrProviderOperation) {
		t.Fatalf("worked=%t err=%v", worked, err)
	}
	store.mu.Lock()
	cursor := store.runtime[created.Version.ID]
	store.mu.Unlock()
	if cursor.LastFailureCode != "provider-observe-failed" || cursor.ConsecutiveFailures != 1 ||
		strings.Contains(cursor.LastFailureCode, "sensitive") {
		t.Fatalf("unsafe or missing durable failure: %#v", cursor)
	}
}

func eventExists(events []Event, eventID string) bool {
	for _, event := range events {
		if event.ID == eventID {
			return true
		}
	}
	return false
}
