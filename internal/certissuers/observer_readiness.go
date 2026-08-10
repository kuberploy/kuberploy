package certissuers

import (
	"context"
	"errors"
	"sync"
	"time"
)

var ErrObserverLeaseLost = errors.New("cert-manager issuer observer readiness lease lost")

type ObserverWorkerObservation struct {
	WorkerID     string
	Identity     ObserverRuntimeIdentity
	TargetDigest string
	TargetCount  int
	StartedAt    time.Time
	ObservedAt   time.Time
}

func (o ObserverWorkerObservation) Validate() error {
	if !idemRE.MatchString(o.WorkerID) || o.Identity.Validate() != nil || !digestRE.MatchString(o.TargetDigest) ||
		o.TargetCount < 0 || o.TargetCount > MaximumObservedIssuers || o.StartedAt.IsZero() || o.StartedAt.Location() != time.UTC ||
		o.ObservedAt.IsZero() || o.ObservedAt.Location() != time.UTC || o.ObservedAt.Before(o.StartedAt) {
		return ErrObservationUnavailable
	}
	return nil
}

type ObserverReadinessLease struct {
	ObserverWorkerObservation
	Epoch int64
	Until time.Time
}

func (l ObserverReadinessLease) Validate() error {
	if l.ObserverWorkerObservation.Validate() != nil || l.Epoch < 1 || l.Until.IsZero() || l.Until.Location() != time.UTC || !l.Until.After(l.ObservedAt) {
		return ErrObservationUnavailable
	}
	return nil
}

type ObserverReadinessStore interface {
	AcquireObserverReadiness(context.Context, ObserverWorkerObservation, time.Duration) (ObserverReadinessLease, error)
	HeartbeatObserverReadiness(context.Context, ObserverReadinessLease, ObserverWorkerObservation, time.Duration) (ObserverReadinessLease, error)
	ObserverRuntimeReady(context.Context, ObserverRuntimeIdentity, string, int, time.Time, time.Duration) error
}

// MemoryObserverReadinessStore is the non-persistent implementation used by
// hermetic runtimes. The interface intentionally permits a later PostgreSQL
// implementation without changing the observer or broadening migration 039.
type MemoryObserverReadinessStore struct {
	mu      sync.Mutex
	records map[string]ObserverReadinessLease
}

func NewMemoryObserverReadinessStore() *MemoryObserverReadinessStore {
	return &MemoryObserverReadinessStore{records: map[string]ObserverReadinessLease{}}
}

func validObserverLeaseDuration(duration time.Duration) bool {
	return duration >= 30*time.Second && duration <= 15*time.Minute
}

func (s *MemoryObserverReadinessStore) AcquireObserverReadiness(_ context.Context, observation ObserverWorkerObservation, duration time.Duration) (ObserverReadinessLease, error) {
	if s == nil || observation.Validate() != nil || !validObserverLeaseDuration(duration) {
		return ObserverReadinessLease{}, ErrObservationUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	epoch := int64(1)
	if current, exists := s.records[observation.Identity.ConfigDigest]; exists {
		if current.Until.After(observation.ObservedAt) || observation.StartedAt.Before(current.StartedAt) {
			return ObserverReadinessLease{}, ErrObserverLeaseLost
		}
		epoch = current.Epoch + 1
	}
	lease := ObserverReadinessLease{ObserverWorkerObservation: observation, Epoch: epoch, Until: observation.ObservedAt.Add(duration)}
	s.records[observation.Identity.ConfigDigest] = lease
	return lease, nil
}

func (s *MemoryObserverReadinessStore) HeartbeatObserverReadiness(_ context.Context, lease ObserverReadinessLease, observation ObserverWorkerObservation, duration time.Duration) (ObserverReadinessLease, error) {
	if s == nil || lease.Validate() != nil || observation.Validate() != nil || !validObserverLeaseDuration(duration) ||
		observation.WorkerID != lease.WorkerID || !observerIdentityEqual(observation.Identity, lease.Identity) ||
		!observation.StartedAt.Equal(lease.StartedAt) || observation.ObservedAt.Before(lease.ObservedAt) {
		return ObserverReadinessLease{}, ErrObservationUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.records[lease.Identity.ConfigDigest]
	if !exists || current.Epoch != lease.Epoch || current.WorkerID != lease.WorkerID || !current.StartedAt.Equal(lease.StartedAt) ||
		!current.Until.Equal(lease.Until) || !observerIdentityEqual(current.Identity, lease.Identity) || !observation.ObservedAt.Before(current.Until) {
		return ObserverReadinessLease{}, ErrObserverLeaseLost
	}
	current.ObserverWorkerObservation = observation
	current.Until = observation.ObservedAt.Add(duration)
	s.records[lease.Identity.ConfigDigest] = current
	return current, nil
}

func (s *MemoryObserverReadinessStore) ObserverRuntimeReady(_ context.Context, identity ObserverRuntimeIdentity, targetDigest string, targetCount int, now time.Time, maximumAge time.Duration) error {
	if s == nil || identity.Validate() != nil || !digestRE.MatchString(targetDigest) || targetCount < 0 || targetCount > MaximumObservedIssuers ||
		now.IsZero() || now.Location() != time.UTC || maximumAge < 10*time.Second || maximumAge > 15*time.Minute {
		return ErrObservationUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.records[identity.ConfigDigest]
	if !exists || current.Validate() != nil || !observerIdentityEqual(current.Identity, identity) || current.TargetDigest != targetDigest ||
		current.TargetCount != targetCount || !current.Until.After(now) || current.ObservedAt.Before(now.Add(-maximumAge)) || current.ObservedAt.After(now) {
		return ErrObservationUnavailable
	}
	return nil
}

var _ ObserverReadinessStore = (*MemoryObserverReadinessStore)(nil)
