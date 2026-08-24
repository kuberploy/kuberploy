package argo

import (
	"context"
	"slices"
	"sync"
	"time"
)

type ObservationStore interface {
	PutObservation(context.Context, Observation) error
	Observation(context.Context, string) (Observation, error)
}

// MemoryObservationStore provides concurrency-safe parity for the observed
// Argo state projection. Newer observations cannot be overwritten by an
// out-of-order watch event.
type MemoryObservationStore struct {
	mu       sync.Mutex
	values   map[string]Observation
	runtimes map[string]memoryObservationRuntime
}

func NewMemoryObservationStore() *MemoryObservationStore {
	return &MemoryObservationStore{values: map[string]Observation{}, runtimes: map[string]memoryObservationRuntime{}}
}

func (s *MemoryObservationStore) PutObservation(_ context.Context, value Observation) error {
	if err := value.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putObservationLocked(value)
}

func (s *MemoryObservationStore) putObservationLocked(value Observation) error {
	if current, exists := s.values[value.DeploymentID]; exists {
		if value.ObservedAt.Before(current.ObservedAt) || value.UpdatedAt.Before(current.UpdatedAt) {
			return ErrConflict
		}
		if observationIdentityConflict(current, value) {
			return ErrConflict
		}
		if value.ObservedAt.Equal(current.ObservedAt) && value.UpdatedAt.Equal(current.UpdatedAt) {
			if !sameObservation(current, value) {
				return ErrConflict
			}
			return nil
		}
	}
	s.values[value.DeploymentID] = cloneObservation(value)
	return nil
}

// A Kubernetes Application receives a new UID when Argo recreates it. The
// observer has already resolved every stable identity from PostgreSQL and
// validated the exact name, project, destination, labels, and desired Git
// revision. Permit that UID rollover only once the replacement is synced to
// the server-owned desired revision; an untrusted or stale replacement cannot
// establish a new durable identity.
func observationIdentityConflict(current, replacement Observation) bool {
	if current.ApplicationID != replacement.ApplicationID || current.ProjectID != replacement.ProjectID ||
		current.EnvironmentID != replacement.EnvironmentID || current.ArgoNamespace != replacement.ArgoNamespace ||
		current.ArgoName != replacement.ArgoName || current.DestinationNamespace != replacement.DestinationNamespace {
		return true
	}
	return current.ArgoUID != replacement.ArgoUID &&
		(replacement.Sync != SyncSynced || replacement.ObservedRevision != replacement.DesiredRevision)
}

func (s *MemoryObservationStore) Observation(_ context.Context, deploymentID string) (Observation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, exists := s.values[deploymentID]
	if !exists {
		return Observation{}, ErrNotFound
	}
	return cloneObservation(value), nil
}

func cloneObservation(value Observation) Observation {
	value.Resources = slices.Clone(value.Resources)
	return value
}

type memoryObservationRuntime struct {
	lease               ObservationLease
	snapshotVersion     string
	consecutiveFailures int
	failureCode         string
	nextPollAt          time.Time
	lastCompletedAt     time.Time
	updatedAt           time.Time
}

func (s *MemoryObservationStore) ClaimObservation(_ context.Context, namespace, owner string, now time.Time, leaseDuration time.Duration) (ObservationWork, error) {
	if !kubeRE.MatchString(namespace) || !observationOwnerRE.MatchString(owner) || now.IsZero() || leaseDuration < minimumObservationLease || leaseDuration > maximumObservationLease {
		return ObservationWork{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, exists := s.runtimes[namespace]
	if !exists {
		state.nextPollAt, state.updatedAt = now.UTC(), now.UTC()
	}
	if state.lease.Owner != "" && state.lease.Until.After(now) {
		return ObservationWork{}, ErrLeaseHeld
	}
	if state.nextPollAt.After(now) {
		return ObservationWork{}, ErrNotFound
	}
	state.lease = ObservationLease{Namespace: namespace, Owner: owner, Epoch: state.lease.Epoch + 1, Until: now.UTC().Add(leaseDuration)}
	// While work is active, nextPollAt tracks the lease boundary. A concurrent
	// wake moves it back to now; FinishObservation preserves that wake.
	state.nextPollAt = state.lease.Until
	state.updatedAt = now.UTC()
	s.runtimes[namespace] = state
	return ObservationWork{Lease: state.lease, ConsecutiveFailures: state.consecutiveFailures}, nil
}

func (s *MemoryObservationStore) HeartbeatObservation(_ context.Context, lease ObservationLease, now time.Time, leaseDuration time.Duration) (ObservationLease, error) {
	if lease.Validate() != nil || now.IsZero() || leaseDuration < minimumObservationLease || leaseDuration > maximumObservationLease {
		return ObservationLease{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, exists := s.runtimes[lease.Namespace]
	if !exists || !sameActiveObservationLease(state.lease, lease, now) {
		return ObservationLease{}, ErrLeaseLost
	}
	previousUntil := state.lease.Until
	state.lease.Until = now.UTC().Add(leaseDuration)
	if state.nextPollAt.Equal(previousUntil) {
		state.nextPollAt = state.lease.Until
	}
	state.updatedAt = now.UTC()
	s.runtimes[lease.Namespace] = state
	return state.lease, nil
}

func (s *MemoryObservationStore) PutObservationFenced(_ context.Context, lease ObservationLease, value Observation, now time.Time) error {
	if lease.Validate() != nil || value.Validate() != nil || now.IsZero() || value.ArgoNamespace != lease.Namespace {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, exists := s.runtimes[lease.Namespace]
	if !exists || !sameActiveObservationLease(state.lease, lease, now) {
		return ErrLeaseLost
	}
	return s.putObservationLocked(value)
}

func (s *MemoryObservationStore) FinishObservation(_ context.Context, lease ObservationLease, outcome ObservationOutcome, now time.Time) error {
	if lease.Validate() != nil || outcome.Validate() != nil || now.IsZero() || outcome.NextPollAt.Before(now) {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, exists := s.runtimes[lease.Namespace]
	if !exists || !sameActiveObservationLease(state.lease, lease, now) {
		return ErrLeaseLost
	}
	nextPollAt := outcome.NextPollAt.UTC()
	if !state.nextPollAt.After(now) {
		nextPollAt = now.UTC()
	}
	state.lease.Owner, state.lease.Until = "", time.Time{}
	if outcome.ConsecutiveFailures == 0 {
		state.snapshotVersion = outcome.SnapshotVersion
		state.lastCompletedAt = now.UTC()
	}
	state.consecutiveFailures, state.failureCode = outcome.ConsecutiveFailures, outcome.FailureCode
	state.nextPollAt, state.updatedAt = nextPollAt, now.UTC()
	s.runtimes[lease.Namespace] = state
	return nil
}

func (s *MemoryObservationStore) WakeObservation(_ context.Context, namespace string, now time.Time) error {
	if !kubeRE.MatchString(namespace) || now.IsZero() {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, exists := s.runtimes[namespace]
	if !exists {
		state.nextPollAt = now.UTC()
	} else if state.nextPollAt.After(now) {
		state.nextPollAt = now.UTC()
	}
	state.updatedAt = now.UTC()
	s.runtimes[namespace] = state
	return nil
}

func sameActiveObservationLease(current, candidate ObservationLease, now time.Time) bool {
	return current.Namespace == candidate.Namespace && current.Owner == candidate.Owner && current.Epoch == candidate.Epoch &&
		current.Owner != "" && current.Until.After(now)
}

var _ ObservationRuntimeStore = (*MemoryObservationStore)(nil)
