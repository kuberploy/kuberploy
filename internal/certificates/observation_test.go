package certificates

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/secrets"
)

const observationWorkerOne = "certificate-worker-alpha"
const observationWorkerTwo = "certificate-worker-bravo"

func activeCertificateObservationTarget(t *testing.T) (secrets.Binding, secrets.Version, Version) {
	t.Helper()
	service, secretService, _, certificateStore := testCertificateService()
	created, err := service.Create(context.Background(), CreateRequest{
		ActorID: testActor, Scope: testScope, Name: "observed-edge", IdempotencyKey: "observe-certificate-0001",
		RequestID: "certificate-observe-create", Material: newCertificateMaterial(t, "api.example.test"),
	})
	if err != nil {
		t.Fatal(err)
	}
	active, err := secretService.ReconcileVersion(context.Background(), created.Version.ID, "certificate-observe-active")
	if err != nil {
		t.Fatal(err)
	}
	attestation, err := certificateStore.Version(context.Background(), created.Version.ID)
	if err != nil {
		t.Fatal(err)
	}
	if validateActiveCertificateTarget(active.Binding, active.Version, attestation) != nil {
		t.Fatal("fixture is not an exact active TLS certificate")
	}
	return active.Binding, active.Version, attestation
}

func testObservationConfig() ObservationConfig {
	config := DefaultObservationConfig()
	config.Enabled = true
	config.Namespaces = []string{testScope.Namespace}
	config.PollInterval = 5 * time.Second
	config.WorkLease = 20 * time.Second
	config.HeartbeatInterval = time.Second
	config.IdleDelay = 100 * time.Millisecond
	config.MinimumBackoff = time.Second
	config.MaximumBackoff = 8 * time.Second
	config.MaximumObservationAge = 15 * time.Second
	return config
}

func testObservationIdentity(t *testing.T, config ObservationConfig) ObservationIdentity {
	t.Helper()
	identity, err := ObservationIdentityForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func TestCertificateObservationIdentityAndTargetDigestAreExactAndReconstructible(t *testing.T) {
	config := testObservationConfig()
	identity := testObservationIdentity(t, config)
	if identity.ContractVersion != CertificateObservationContract || !digestRE.MatchString(identity.ConfigDigest) {
		t.Fatalf("identity=%#v", identity)
	}
	changed := config
	changed.MaximumObservationAge++
	if other := testObservationIdentity(t, changed); other == identity {
		t.Fatal("freshness contract did not enter worker identity")
	}
	changed = config
	changed.Namespaces = []string{"z-runtime", "a-runtime"}
	if changed.Validate() == nil {
		t.Fatal("unsorted namespace contract was accepted")
	}

	binding, secretVersion, attestation := activeCertificateObservationTarget(t)
	digest, err := CertificateObservationTargetDigest(binding, secretVersion, attestation)
	if err != nil || !digestRE.MatchString(digest) {
		t.Fatalf("digest=%q err=%v", digest, err)
	}
	reconstructed := cloneObservedSecretVersion(secretVersion)
	reconstructed.RequestFingerprint[0] ^= 0xff
	if same, sameErr := CertificateObservationTargetDigest(binding, reconstructed, attestation); sameErr != nil || same != digest {
		t.Fatalf("non-persisted request fingerprint entered target identity: digest=%q err=%v", same, sameErr)
	}
	corrupted := cloneObservedSecretVersion(secretVersion)
	corrupted.Artifact.ObjectName = "substituted-object"
	if changedDigest, changedErr := CertificateObservationTargetDigest(binding, corrupted, attestation); changedErr == nil || changedDigest != "" {
		t.Fatalf("substituted strict object accepted: digest=%q err=%v", changedDigest, changedErr)
	}
	wrongPurpose := binding
	wrongPurpose.Purpose = secrets.PurposeRuntimeSecret
	if _, err = CertificateObservationTargetDigest(wrongPurpose, secretVersion, attestation); !errors.Is(err, ErrInvalid) {
		t.Fatalf("ordinary secret accepted: %v", err)
	}
	wrongDelivery := cloneObservedSecretVersion(secretVersion)
	wrongDelivery.Deliveries[0].FileMode = 0o440
	if _, err = CertificateObservationTargetDigest(binding, wrongDelivery, attestation); !errors.Is(err, ErrInvalid) {
		t.Fatalf("non-certificate delivery contract accepted: %v", err)
	}
	wrongAttestation := cloneVersion(attestation)
	wrongAttestation.SecretVersionID = "20000000-0000-4000-8000-000000000001"
	if _, err = CertificateObservationTargetDigest(binding, secretVersion, wrongAttestation); !errors.Is(err, ErrInvalid) {
		t.Fatalf("attestation substitution accepted: %v", err)
	}
}

func TestObservationMemoryStoreFencesLeasesResultsAndFreshReadiness(t *testing.T) {
	binding, secretVersion, attestation := activeCertificateObservationTarget(t)
	config := testObservationConfig()
	identity := testObservationIdentity(t, config)
	now := testNow.Add(2 * time.Minute)
	store := NewObservationMemoryStore()
	if err := store.UpsertActiveCertificate(binding, secretVersion, attestation, now); err != nil {
		t.Fatal(err)
	}
	first, err := store.ClaimCertificateObservation(t.Context(), identity, observationWorkerOne, config.Namespaces, now, config.WorkLease)
	if err != nil || first.Validate() != nil {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	if _, err = store.ClaimCertificateObservation(t.Context(), identity, observationWorkerTwo, config.Namespaces, now.Add(time.Second), config.WorkLease); !errors.Is(err, ErrNotFound) {
		t.Fatalf("active lease was stolen: %v", err)
	}
	reclaimedAt := first.Lease.Until.Add(time.Millisecond)
	second, err := store.ClaimCertificateObservation(t.Context(), identity, observationWorkerTwo, config.Namespaces, reclaimedAt, config.WorkLease)
	if err != nil || second.Lease.Epoch != first.Lease.Epoch+1 {
		t.Fatalf("second=%#v err=%v", second, err)
	}
	if _, err = store.HeartbeatCertificateObservation(t.Context(), first.Lease, reclaimedAt, config.WorkLease); !errors.Is(err, ErrObservationLeaseLost) {
		t.Fatalf("expired owner heartbeat accepted: %v", err)
	}
	if err = store.ApplyCertificateObservationReady(t.Context(), first.Lease, ObservationReadyOutcome{ObservedAt: reclaimedAt, NextAt: reclaimedAt.Add(config.PollInterval)}, reclaimedAt); !errors.Is(err, ErrObservationLeaseLost) {
		t.Fatalf("expired owner result accepted: %v", err)
	}
	heartbeatAt := reclaimedAt.Add(time.Second)
	lease, err := store.HeartbeatCertificateObservation(t.Context(), second.Lease, heartbeatAt, config.WorkLease)
	if err != nil || !lease.Until.After(second.Lease.Until) {
		t.Fatalf("heartbeat=%#v err=%v", lease, err)
	}
	readyAt := heartbeatAt.Add(time.Second)
	if err = store.ApplyCertificateObservationReady(t.Context(), lease, ObservationReadyOutcome{ObservedAt: readyAt, NextAt: readyAt.Add(config.PollInterval)}, readyAt); err != nil {
		t.Fatal(err)
	}
	if err = store.ActiveCertificateReady(t.Context(), binding.ID, secretVersion.ID, identity, readyAt.Add(time.Second), config.MaximumObservationAge); err != nil {
		t.Fatalf("fresh exact certificate not ready: %v", err)
	}
	wrongIdentity := identity
	wrongIdentity.ConfigDigest = "sha256:" + strings.Repeat("e", 64)
	if err = store.ActiveCertificateReady(t.Context(), binding.ID, secretVersion.ID, wrongIdentity, readyAt.Add(time.Second), config.MaximumObservationAge); !errors.Is(err, ErrObservationUnavailable) {
		t.Fatalf("wrong config observed ready: %v", err)
	}
	if err = store.ActiveCertificateReady(t.Context(), binding.ID, secretVersion.ID, identity, readyAt.Add(config.MaximumObservationAge+time.Second), config.MaximumObservationAge); !errors.Is(err, ErrObservationUnavailable) {
		t.Fatalf("stale certificate observed ready: %v", err)
	}

	due := readyAt.Add(config.PollInterval)
	work, err := store.ClaimCertificateObservation(t.Context(), identity, observationWorkerOne, config.Namespaces, due, config.WorkLease)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.ApplyCertificateObservationDegraded(t.Context(), work.Lease, ObservationDegradedOutcome{
		FailureCode: ObservationSealedSecretNotReady, ObservedAt: due, NextAt: due.Add(config.MinimumBackoff),
	}, due); err != nil {
		t.Fatal(err)
	}
	if err = store.ActiveCertificateReady(t.Context(), binding.ID, secretVersion.ID, identity, due, config.MaximumObservationAge); !errors.Is(err, ErrObservationUnavailable) {
		t.Fatalf("degraded certificate remained ready: %v", err)
	}
	snapshot, err := store.Observation(secretVersion.ID)
	if err != nil || snapshot.State != ObservationDegraded || snapshot.FailureCode != ObservationSealedSecretNotReady || snapshot.ConsecutiveFailures != 1 || !snapshot.LeaseUntil.IsZero() {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
}

func TestObservationMemoryStorePublishesOnlyFreshExactWorkerReadiness(t *testing.T) {
	config := testObservationConfig()
	identity := testObservationIdentity(t, config)
	now := testNow.Add(2 * time.Minute)
	store := NewObservationMemoryStore()
	first, err := store.AcquireCertificateObservationReadiness(t.Context(), ObservationWorkerObservation{
		WorkerID: observationWorkerOne, Identity: identity, StartedAt: now, ObservedAt: now,
	}, CertificateObservationReadinessLease)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.CertificateObservationRuntimeReady(t.Context(), identity, now.Add(time.Second), CertificateObservationHeartbeatMaxAge); err != nil {
		t.Fatalf("fresh runtime unavailable: %v", err)
	}
	second, err := store.AcquireCertificateObservationReadiness(t.Context(), ObservationWorkerObservation{
		WorkerID: observationWorkerOne, Identity: identity, StartedAt: now.Add(2 * time.Second), ObservedAt: now.Add(2 * time.Second),
	}, CertificateObservationReadinessLease)
	if err != nil || second.Epoch != first.Epoch+1 {
		t.Fatalf("second=%#v err=%v", second, err)
	}
	if _, err = store.HeartbeatCertificateObservationReadiness(t.Context(), first, now.Add(3*time.Second), CertificateObservationReadinessLease); !errors.Is(err, ErrObservationLeaseLost) {
		t.Fatalf("superseded readiness heartbeat accepted: %v", err)
	}
	updated, err := store.HeartbeatCertificateObservationReadiness(t.Context(), second, now.Add(3*time.Second), CertificateObservationReadinessLease)
	if err != nil || !updated.ObservedAt.Equal(now.Add(3*time.Second)) {
		t.Fatalf("updated=%#v err=%v", updated, err)
	}
	wrong := identity
	wrong.ConfigDigest = "sha256:" + strings.Repeat("f", 64)
	if err = store.CertificateObservationRuntimeReady(t.Context(), wrong, now.Add(4*time.Second), CertificateObservationHeartbeatMaxAge); !errors.Is(err, ErrObservationUnavailable) {
		t.Fatalf("mismatched runtime observed ready: %v", err)
	}
	if err = store.CertificateObservationRuntimeReady(t.Context(), identity, updated.ObservedAt.Add(CertificateObservationHeartbeatMaxAge+time.Second), CertificateObservationHeartbeatMaxAge); !errors.Is(err, ErrObservationUnavailable) {
		t.Fatalf("stale runtime observed ready: %v", err)
	}
}

type scriptedObservation struct {
	observation secrets.ReadinessObservation
	err         error
}

type scriptedCertificateObserver struct {
	mu      sync.Mutex
	results []scriptedObservation
	calls   int
}

func (o *scriptedCertificateObserver) ObserveStrictSealedSecret(_ context.Context, artifact secrets.Artifact) (secrets.ReadinessObservation, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls++
	if len(o.results) == 0 {
		return secrets.ReadinessObservation{}, errors.New("no scripted observation")
	}
	result := o.results[0]
	o.results = o.results[1:]
	if result.observation.Artifact == (secrets.Artifact{}) {
		result.observation.Artifact = artifact
	}
	return result.observation, result.err
}

func (o *scriptedCertificateObserver) Calls() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.calls
}

func TestObservationControllerContinuouslyTransitionsReadyDegradedAndRequeue(t *testing.T) {
	binding, secretVersion, attestation := activeCertificateObservationTarget(t)
	config := testObservationConfig()
	identity := testObservationIdentity(t, config)
	now := testNow.Add(2 * time.Minute)
	store := NewObservationMemoryStore()
	if err := store.UpsertActiveCertificate(binding, secretVersion, attestation, now); err != nil {
		t.Fatal(err)
	}
	mismatch := *secretVersion.Artifact
	mismatch.ObjectName = "substituted-object"
	observer := &scriptedCertificateObserver{results: []scriptedObservation{
		{observation: secrets.ReadinessObservation{Status: secrets.ReadinessReady, ObservedAt: now}},
		{observation: secrets.ReadinessObservation{Artifact: mismatch, Status: secrets.ReadinessReady, ObservedAt: now.Add(config.PollInterval)}},
		{err: errors.New("provider transport failed")},
	}}
	controller := &ObservationController{Store: store, Observer: observer, Config: config, Identity: identity,
		WorkerID: observationWorkerOne, Now: func() time.Time { return now }}
	if worked, err := controller.Reconcile(t.Context()); err != nil || !worked {
		t.Fatalf("ready reconcile worked=%v err=%v", worked, err)
	}
	snapshot, _ := store.Observation(secretVersion.ID)
	if snapshot.State != ObservationReady || snapshot.FailureCode != "" {
		t.Fatalf("ready snapshot=%#v", snapshot)
	}
	now = snapshot.NextAt
	if worked, err := controller.Reconcile(t.Context()); err != nil || !worked {
		t.Fatalf("mismatch reconcile worked=%v err=%v", worked, err)
	}
	snapshot, _ = store.Observation(secretVersion.ID)
	if snapshot.State != ObservationDegraded || snapshot.FailureCode != ObservationProviderMismatch || snapshot.ConsecutiveFailures != 1 {
		t.Fatalf("mismatch snapshot=%#v", snapshot)
	}
	now = snapshot.NextAt
	if worked, err := controller.Reconcile(t.Context()); !errors.Is(err, ErrObservationUnavailable) || !worked {
		t.Fatalf("requeue worked=%v err=%v", worked, err)
	}
	snapshot, _ = store.Observation(secretVersion.ID)
	if snapshot.State != ObservationRequeue || snapshot.FailureCode != ObservationProviderUnavailable || snapshot.ConsecutiveFailures != 2 || observer.Calls() != 3 {
		t.Fatalf("requeue snapshot=%#v calls=%d", snapshot, observer.Calls())
	}
	if strings.Contains(string(snapshot.FailureCode), "transport") {
		t.Fatalf("provider error leaked into durable code: %q", snapshot.FailureCode)
	}
}

func TestObservationControllerMapsProviderStatesAndCertificateExpiryToSafeDegradation(t *testing.T) {
	for _, test := range []struct {
		name        string
		observation secrets.ReadinessObservation
		expected    ObservationFailureCode
	}{
		{name: "pending", observation: secrets.ReadinessObservation{Status: secrets.ReadinessPending}, expected: ObservationSealedSecretNotReady},
		{name: "failed", observation: secrets.ReadinessObservation{Status: secrets.ReadinessFailed, FailureCode: "provider-internal-detail"}, expected: ObservationSealedSecretSyncFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			binding, secretVersion, attestation := activeCertificateObservationTarget(t)
			config := testObservationConfig()
			identity := testObservationIdentity(t, config)
			now := testNow.Add(2 * time.Minute)
			store := NewObservationMemoryStore()
			if err := store.UpsertActiveCertificate(binding, secretVersion, attestation, now); err != nil {
				t.Fatal(err)
			}
			test.observation.ObservedAt = now
			observer := &scriptedCertificateObserver{results: []scriptedObservation{{observation: test.observation}}}
			controller := &ObservationController{Store: store, Observer: observer, Config: config, Identity: identity,
				WorkerID: observationWorkerOne, Now: func() time.Time { return now }}
			if worked, err := controller.Reconcile(t.Context()); err != nil || !worked {
				t.Fatalf("worked=%v err=%v", worked, err)
			}
			snapshot, _ := store.Observation(secretVersion.ID)
			if snapshot.State != ObservationDegraded || snapshot.FailureCode != test.expected {
				t.Fatalf("snapshot=%#v", snapshot)
			}
		})
	}

	binding, secretVersion, attestation := activeCertificateObservationTarget(t)
	config := testObservationConfig()
	identity := testObservationIdentity(t, config)
	availableAt := testNow.Add(2 * time.Minute)
	store := NewObservationMemoryStore()
	if err := store.UpsertActiveCertificate(binding, secretVersion, attestation, availableAt); err != nil {
		t.Fatal(err)
	}
	now := attestation.NotAfter
	observer := &scriptedCertificateObserver{}
	controller := &ObservationController{Store: store, Observer: observer, Config: config, Identity: identity,
		WorkerID: observationWorkerOne, Now: func() time.Time { return now }}
	if worked, err := controller.Reconcile(t.Context()); err != nil || !worked {
		t.Fatalf("expired worked=%v err=%v", worked, err)
	}
	snapshot, _ := store.Observation(secretVersion.ID)
	if snapshot.State != ObservationDegraded || snapshot.FailureCode != ObservationCertificateExpired || observer.Calls() != 0 {
		t.Fatalf("expired snapshot=%#v calls=%d", snapshot, observer.Calls())
	}
}

func TestObservationControllerPublishesAndExpiresExactRuntimeReadiness(t *testing.T) {
	config := testObservationConfig()
	identity := testObservationIdentity(t, config)
	now := testNow.Add(2 * time.Minute)
	store := NewObservationMemoryStore()
	observer := &scriptedCertificateObserver{}
	controller := &ObservationController{Store: store, Observer: observer, Config: config, Identity: identity,
		WorkerID: observationWorkerOne, Now: func() time.Time { return now }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- controller.Run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for {
		if err := store.CertificateObservationRuntimeReady(t.Context(), identity, now, CertificateObservationHeartbeatMaxAge); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("controller did not publish exact worker readiness")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("run error=%v", err)
	}
	probe := &ObservationReadinessProbe{Store: store, Config: config, Now: func() time.Time { return now }}
	if err := probe.Probe(t.Context()); err != nil {
		t.Fatalf("fresh probe=%v", err)
	}
	probe.Now = func() time.Time { return now.Add(CertificateObservationHeartbeatMaxAge + time.Second) }
	if err := probe.Probe(t.Context()); !errors.Is(err, ErrObservationUnavailable) {
		t.Fatalf("stale probe=%v", err)
	}
}

type heartbeatLeaseLossStore struct {
	ObservationStore
	heartbeats int
	mu         sync.Mutex
}

func (s *heartbeatLeaseLossStore) HeartbeatCertificateObservation(context.Context, ObservationLease, time.Time, time.Duration) (ObservationLease, error) {
	s.mu.Lock()
	s.heartbeats++
	s.mu.Unlock()
	return ObservationLease{}, ErrObservationLeaseLost
}

type cancellationObserver struct {
	started  chan struct{}
	canceled chan struct{}
	once     sync.Once
}

func (o *cancellationObserver) ObserveStrictSealedSecret(ctx context.Context, _ secrets.Artifact) (secrets.ReadinessObservation, error) {
	o.once.Do(func() { close(o.started) })
	<-ctx.Done()
	close(o.canceled)
	return secrets.ReadinessObservation{}, ctx.Err()
}

func TestObservationControllerCancelsProviderAndRejectsResultOnLeaseLoss(t *testing.T) {
	binding, secretVersion, attestation := activeCertificateObservationTarget(t)
	config := testObservationConfig()
	identity := testObservationIdentity(t, config)
	now := testNow.Add(2 * time.Minute)
	memory := NewObservationMemoryStore()
	if err := memory.UpsertActiveCertificate(binding, secretVersion, attestation, now); err != nil {
		t.Fatal(err)
	}
	store := &heartbeatLeaseLossStore{ObservationStore: memory}
	observer := &cancellationObserver{started: make(chan struct{}), canceled: make(chan struct{})}
	controller := &ObservationController{Store: store, Observer: observer, Config: config, Identity: identity,
		WorkerID: observationWorkerOne, Now: func() time.Time { return now }}
	startedAt := time.Now()
	worked, err := controller.Reconcile(t.Context())
	if !worked || !errors.Is(err, ErrObservationLeaseLost) {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	if time.Since(startedAt) > 3*time.Second {
		t.Fatal("lease loss did not bound the provider call")
	}
	select {
	case <-observer.started:
	default:
		t.Fatal("provider was not started")
	}
	select {
	case <-observer.canceled:
	default:
		t.Fatal("provider context was not canceled on lease loss")
	}
	store.mu.Lock()
	heartbeats := store.heartbeats
	store.mu.Unlock()
	if heartbeats != 1 {
		t.Fatalf("heartbeats=%d", heartbeats)
	}
	snapshot, snapshotErr := memory.Observation(secretVersion.ID)
	if snapshotErr != nil || snapshot.State != ObservationAwaiting || snapshot.LastObservedAt != (time.Time{}) {
		t.Fatalf("lease-lost result mutated readiness: snapshot=%#v err=%v", snapshot, snapshotErr)
	}
}
