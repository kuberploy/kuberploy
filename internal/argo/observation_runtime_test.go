package argo

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMemoryObservationRuntimeFencesReclaimedWorkers(t *testing.T) {
	store := NewMemoryObservationStore()
	base := time.Date(2026, 8, 9, 2, 0, 0, 0, time.UTC)
	first, err := store.ClaimObservation(t.Context(), "argocd", "observer-owner-a", base, 30*time.Second)
	if err != nil || first.Lease.Epoch != 1 {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	if _, err = store.ClaimObservation(t.Context(), "argocd", "observer-owner-b", base.Add(time.Second), 30*time.Second); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("expected held lease, got %v", err)
	}
	refreshed, err := store.HeartbeatObservation(t.Context(), first.Lease, base.Add(10*time.Second), 30*time.Second)
	if err != nil || !refreshed.Until.Equal(base.Add(40*time.Second)) {
		t.Fatalf("heartbeat=%#v err=%v", refreshed, err)
	}
	observation := runtimeObservation(base.Add(11 * time.Second))
	if err = store.PutObservationFenced(t.Context(), first.Lease, observation, base.Add(11*time.Second)); err != nil {
		t.Fatal(err)
	}
	second, err := store.ClaimObservation(t.Context(), "argocd", "observer-owner-b", base.Add(41*time.Second), 30*time.Second)
	if err != nil || second.Lease.Epoch != 2 {
		t.Fatalf("second=%#v err=%v", second, err)
	}
	if err = store.PutObservationFenced(t.Context(), first.Lease, observation, base.Add(42*time.Second)); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale worker wrote observation: %v", err)
	}
	if err = store.FinishObservation(t.Context(), first.Lease, ObservationOutcome{SnapshotVersion: "50", NextPollAt: base.Add(time.Minute)}, base.Add(42*time.Second)); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale worker finished lease: %v", err)
	}
	if err = store.FinishObservation(t.Context(), second.Lease, ObservationOutcome{SnapshotVersion: "51", NextPollAt: base.Add(2 * time.Minute)}, base.Add(43*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ClaimObservation(t.Context(), "argocd", "observer-owner-a", base.Add(time.Minute), 30*time.Second); !errors.Is(err, ErrNotFound) {
		t.Fatalf("observer ignored next-poll gate: %v", err)
	}
	third, err := store.ClaimObservation(t.Context(), "argocd", "observer-owner-a", base.Add(2*time.Minute), 30*time.Second)
	if err != nil || third.Lease.Epoch != 3 || third.ConsecutiveFailures != 0 {
		t.Fatalf("third=%#v err=%v", third, err)
	}
	if err = store.FinishObservation(t.Context(), third.Lease, ObservationOutcome{ConsecutiveFailures: 1, FailureCode: "observation-unavailable", NextPollAt: base.Add(3 * time.Minute)}, base.Add(2*time.Minute+time.Second)); err != nil {
		t.Fatal(err)
	}
	fourth, err := store.ClaimObservation(t.Context(), "argocd", "observer-owner-b", base.Add(3*time.Minute), 30*time.Second)
	if err != nil || fourth.ConsecutiveFailures != 1 || fourth.Lease.Epoch != 4 {
		t.Fatalf("fourth=%#v err=%v", fourth, err)
	}
}

func runtimeObservation(at time.Time) Observation {
	target := observerTarget()
	return Observation{DeploymentID: target.DeploymentID, ApplicationID: target.ApplicationID, ProjectID: target.ProjectID, EnvironmentID: target.EnvironmentID,
		ArgoUID: observerUID, ArgoNamespace: "argocd", ArgoName: ApplicationName(target.DeploymentID), DestinationNamespace: target.DestinationNamespace,
		DesiredRevision: target.DesiredRevision, ObservedRevision: strings.Repeat("b", 40), Sync: SyncSynced, Health: HealthHealthy,
		OperationPhase: "succeeded", Resources: []ResourceIdentity{}, ObservedAt: at.UTC(), UpdatedAt: at.UTC()}
}

type failingApplicationSource struct{ err error }

func (s failingApplicationSource) ListKuberployApplications(context.Context, string, string, int) (KubernetesApplicationPage, error) {
	return KubernetesApplicationPage{}, s.err
}

func TestObservationCoordinatorCompletesAndDurablyBacksOffFailures(t *testing.T) {
	base := time.Date(2026, 8, 9, 3, 0, 0, 0, time.UTC)
	clock := base
	now := func() time.Time { return clock }
	store := NewMemoryObservationStore()
	source := &observerSource{pages: []KubernetesApplicationPage{{ResourceVersion: "500", Applications: []KubernetesApplication{observerApplication(base)}}}}
	coordinator := &ObservationCoordinator{Store: store, Source: source, Resolver: observerResolver{targets: map[string]ObservationTarget{observerDeploymentID: observerTarget()}},
		Namespace: "argocd", Owner: "observer-owner-a", LeaseDuration: 30 * time.Second, HeartbeatInterval: 10 * time.Second,
		WorkTimeout: time.Minute, PollInterval: time.Minute, MinimumBackoff: 5 * time.Second, MaximumBackoff: time.Minute, IdleDelay: time.Second, Now: now}
	worked, err := coordinator.RunOnce(t.Context())
	if err != nil || !worked {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	if _, err = store.Observation(t.Context(), observerDeploymentID); err != nil {
		t.Fatal(err)
	}
	if worked, err = coordinator.RunOnce(t.Context()); err != nil || worked {
		t.Fatalf("not-due worked=%v err=%v", worked, err)
	}

	clock = base.Add(time.Minute)
	providerErr := errors.New("provider unavailable")
	coordinator.Source = failingApplicationSource{err: providerErr}
	worked, err = coordinator.RunOnce(t.Context())
	if !worked || !errors.Is(err, providerErr) {
		t.Fatalf("failure worked=%v err=%v", worked, err)
	}
	clock = clock.Add(time.Second)
	if worked, err = coordinator.RunOnce(t.Context()); err != nil || worked {
		t.Fatalf("failure backoff was not durable: worked=%v err=%v", worked, err)
	}
}

func TestObservationOutcomeRejectsAmbiguousSuccessAndFailure(t *testing.T) {
	now := time.Now().UTC()
	invalid := []ObservationOutcome{
		{NextPollAt: now},
		{SnapshotVersion: "1", FailureCode: "failure", NextPollAt: now},
		{SnapshotVersion: "1", ConsecutiveFailures: 1, FailureCode: "failure", NextPollAt: now},
		{ConsecutiveFailures: 1, FailureCode: "", NextPollAt: now},
		{ConsecutiveFailures: 33, FailureCode: "failure", NextPollAt: now},
	}
	for index, outcome := range invalid {
		if err := outcome.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("outcome %d unexpectedly valid: %#v err=%v", index, outcome, err)
		}
	}
}
