package builds

import (
	"context"
	"time"
)

func (s *MemoryStore) ObserveSourceBuildWorker(_ context.Context, observation SourceBuildWorkerObservation) error {
	if observation.validate() != nil {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, exists := s.runtimeReadiness[observation.WorkerID]; exists && current.ObservedAt.After(observation.ObservedAt) {
		return ErrConflict
	}
	s.runtimeReadiness[observation.WorkerID] = observation
	return nil
}

func (s *MemoryStore) SourceBuildRuntimeReady(_ context.Context, identity SourceBuildRuntimeIdentity, now time.Time, maximumAge time.Duration) error {
	if identity.validate() != nil || now.IsZero() || maximumAge < time.Second || maximumAge > 5*time.Minute {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, observation := range s.runtimeReadiness {
		if observation.SourceBuildRuntimeIdentity == identity && !observation.ObservedAt.Before(now.Add(-maximumAge)) &&
			!observation.ObservedAt.After(now.Add(5*time.Second)) {
			return nil
		}
	}
	return ErrRuntimeNotReady
}
