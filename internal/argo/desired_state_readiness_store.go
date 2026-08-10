package argo

import (
	"context"
	"time"
)

func (s *MemoryDesiredStateStore) AcquireDesiredStateReadiness(_ context.Context, observation DesiredStateRuntimeWorkerObservation, duration time.Duration) (DesiredStateRuntimeLease, error) {
	if observation.Validate() != nil || !validDesiredStateReadinessLease(duration) {
		return DesiredStateRuntimeLease{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	epoch := int64(1)
	if current, exists := s.ready[observation.WorkerID]; exists {
		epoch = current.Epoch + 1
	}
	lease := DesiredStateRuntimeLease{DesiredStateRuntimeWorkerObservation: observation, Epoch: epoch, Until: observation.ObservedAt.Add(duration)}
	s.ready[observation.WorkerID] = lease
	return lease, nil
}

func (s *MemoryDesiredStateStore) HeartbeatDesiredStateReadiness(_ context.Context, lease DesiredStateRuntimeLease, observedAt time.Time, duration time.Duration) (DesiredStateRuntimeLease, error) {
	if lease.Validate() != nil || observedAt.IsZero() || observedAt.Before(lease.ObservedAt) || !validDesiredStateReadinessLease(duration) {
		return DesiredStateRuntimeLease{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.ready[lease.WorkerID]
	if !exists || current != lease || !current.Until.After(observedAt) {
		return DesiredStateRuntimeLease{}, ErrLeaseLost
	}
	current.ObservedAt, current.Until = observedAt.UTC(), observedAt.UTC().Add(duration)
	s.ready[lease.WorkerID] = current
	return current, nil
}

func (s *MemoryDesiredStateStore) DesiredStateRuntimeReady(_ context.Context, identity DesiredStateRuntimeIdentity, now time.Time, maximumAge time.Duration) error {
	if identity.Validate() != nil || now.IsZero() || maximumAge < 2*DesiredStateHeartbeatInterval || maximumAge > 5*time.Minute {
		return ErrDesiredStateNotReady
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, lease := range s.ready {
		if lease.DesiredStateRuntimeIdentity == identity && lease.Until.After(now) && !lease.ObservedAt.Before(now.Add(-maximumAge)) && !lease.ObservedAt.After(now) {
			return nil
		}
	}
	return ErrDesiredStateNotReady
}

var _ DesiredStateReadinessStore = (*MemoryDesiredStateStore)(nil)
