package memory

import (
	"context"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/registry"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func registryReadinessKey(targetID, workerID string) string {
	return targetID + "\x00" + workerID
}

func validRegistryReadinessDuration(duration time.Duration) bool {
	return duration >= 10*time.Second && duration <= time.Hour
}

func (s *Store) AcquireManagedRegistryReadiness(_ context.Context, observation registry.RuntimeWorkerObservation, duration time.Duration) (registry.RuntimeReadinessLease, error) {
	if observation.Validate() != nil || !validRegistryReadinessDuration(duration) {
		return registry.RuntimeReadinessLease{}, registry.ErrRegistryRuntimeNotReady
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	target, exists := s.registryTargets[observation.TargetID]
	if !exists || target.Mode != domain.RegistryTargetManaged {
		return registry.RuntimeReadinessLease{}, base.ErrNotFound
	}
	key := registryReadinessKey(observation.TargetID, observation.WorkerID)
	epoch := int64(1)
	if current, ok := s.registryRuntimeReadiness[key]; ok {
		epoch = current.Epoch + 1
	}
	lease := registry.RuntimeReadinessLease{RuntimeWorkerObservation: observation, Epoch: epoch, Until: observation.ObservedAt.Add(duration)}
	s.registryRuntimeReadiness[key] = lease
	return lease, nil
}

func (s *Store) HeartbeatManagedRegistryReadiness(_ context.Context, lease registry.RuntimeReadinessLease, observedAt time.Time, duration time.Duration) (registry.RuntimeReadinessLease, error) {
	if lease.Validate() != nil || observedAt.IsZero() || observedAt.Before(lease.ObservedAt) || !validRegistryReadinessDuration(duration) {
		return registry.RuntimeReadinessLease{}, registry.ErrRegistryRuntimeNotReady
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.registryRuntimeReadiness[registryReadinessKey(lease.TargetID, lease.WorkerID)]
	if !exists || current.Epoch != lease.Epoch || current.RuntimeIdentity != lease.RuntimeIdentity ||
		!current.StartedAt.Equal(lease.StartedAt) || !current.Until.After(observedAt) || current.ObservedAt.After(observedAt) {
		return registry.RuntimeReadinessLease{}, base.ErrRegistryLeaseLost
	}
	current.ObservedAt = observedAt
	current.Until = observedAt.Add(duration)
	s.registryRuntimeReadiness[registryReadinessKey(lease.TargetID, lease.WorkerID)] = current
	return current, nil
}

func (s *Store) ManagedRegistryRuntimeReady(_ context.Context, identity registry.RuntimeIdentity, now time.Time, maximumAge time.Duration) error {
	if identity.Validate() != nil || now.IsZero() || maximumAge < time.Second || maximumAge > 5*time.Minute {
		return registry.ErrRegistryRuntimeNotReady
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, lease := range s.registryRuntimeReadiness {
		if lease.RuntimeIdentity == identity && !lease.ObservedAt.Before(now.Add(-maximumAge)) &&
			!lease.ObservedAt.After(now.Add(registry.ManagedRegistryReadinessClockSkew)) && lease.Until.After(now) {
			return nil
		}
	}
	return registry.ErrRegistryRuntimeNotReady
}
