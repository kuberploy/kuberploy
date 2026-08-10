package secrets

import (
	"context"
	"slices"
	"time"
)

type memoryRuntimeReconciliation struct {
	VersionID           string
	BindingID           string
	State               string
	NextAt              time.Time
	ConsecutiveFailures int
	LastFailureCode     string
	LeaseEpoch          int64
	Lease               RuntimeLease
	CreatedAt           time.Time
	UpdatedAt           time.Time
	CompletedAt         time.Time
}

func (s *MemoryStore) ClaimRuntimeSecret(_ context.Context, identity RuntimeIdentity, owner string, namespaces []string, now time.Time, duration time.Duration) (RuntimeWork, error) {
	if identity.Validate() != nil || !runtimeSecretWorkerIDRE.MatchString(owner) || !exactRuntimeNamespaces(namespaces) ||
		now.IsZero() || duration < 20*time.Second || duration > time.Hour {
		return RuntimeWork{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var selected *memoryRuntimeReconciliation
	for versionID, cursor := range s.runtime {
		version, versionExists := s.versions[versionID]
		binding, bindingExists := s.bindings[cursor.BindingID]
		_, namespaceAllowed := slices.BinarySearch(namespaces, binding.Scope.Namespace)
		if !versionExists || !bindingExists || !namespaceAllowed || cursor.State != "" && cursor.State != "awaiting" ||
			version.State != VersionAwaitingReadiness || version.Provider != ProviderSealedSecrets || cursor.NextAt.After(now) ||
			!cursor.Lease.Until.IsZero() && cursor.Lease.Until.After(now) {
			continue
		}
		candidate := cursor
		if selected == nil || candidate.NextAt.Before(selected.NextAt) ||
			candidate.NextAt.Equal(selected.NextAt) && candidate.VersionID < selected.VersionID {
			selected = &candidate
		}
	}
	if selected == nil {
		return RuntimeWork{}, ErrNotFound
	}
	selected.State = "awaiting"
	selected.UpdatedAt = now.UTC()
	selected.LeaseEpoch++
	selected.Lease = RuntimeLease{VersionID: selected.VersionID, BindingID: selected.BindingID, Owner: owner,
		Epoch: selected.LeaseEpoch, Until: now.UTC().Add(duration), Identity: identity}
	s.runtime[selected.VersionID] = *selected
	work := RuntimeWork{Binding: s.bindings[selected.BindingID], Version: cloneVersion(s.versions[selected.VersionID]),
		Lease: selected.Lease, ConsecutiveFailures: selected.ConsecutiveFailures}
	if work.Validate() != nil {
		return RuntimeWork{}, ErrConflict
	}
	return work, nil
}

func (s *MemoryStore) HeartbeatRuntimeSecret(_ context.Context, lease RuntimeLease, now time.Time, duration time.Duration) (RuntimeLease, error) {
	if lease.Validate() != nil || now.IsZero() || duration < 20*time.Second || duration > time.Hour {
		return RuntimeLease{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cursor, ok := s.runtime[lease.VersionID]
	if !ok || cursor.State != "awaiting" || !sameRuntimeLease(cursor.Lease, lease) || !now.Before(cursor.Lease.Until) || now.Before(cursor.UpdatedAt) {
		return RuntimeLease{}, ErrRuntimeLeaseLost
	}
	cursor.Lease.Until, cursor.UpdatedAt = now.UTC().Add(duration), now.UTC()
	s.runtime[lease.VersionID] = cursor
	return cursor.Lease, nil
}

func (s *MemoryStore) ApplyRuntimeSecretPending(_ context.Context, lease RuntimeLease, outcome RuntimePendingOutcome, now time.Time) error {
	if lease.Validate() != nil || outcome.validate(now) != nil {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cursor, ok := s.runtime[lease.VersionID]
	version := s.versions[lease.VersionID]
	if !ok || cursor.State != "awaiting" || version.State != VersionAwaitingReadiness ||
		!sameRuntimeLease(cursor.Lease, lease) || !now.Before(cursor.Lease.Until) || now.Before(cursor.UpdatedAt) {
		return ErrRuntimeLeaseLost
	}
	if outcome.FailureCode == "" {
		cursor.ConsecutiveFailures, cursor.LastFailureCode = 0, ""
	} else {
		cursor.ConsecutiveFailures = min(30, cursor.ConsecutiveFailures+1)
		cursor.LastFailureCode = outcome.FailureCode
	}
	cursor.NextAt, cursor.UpdatedAt, cursor.Lease = outcome.NextAt.UTC(), now.UTC(), RuntimeLease{}
	s.runtime[lease.VersionID] = cursor
	return nil
}

func (s *MemoryStore) ApplyRuntimeSecretReady(_ context.Context, lease RuntimeLease, event Event, now time.Time) (Binding, Version, error) {
	if lease.Validate() != nil || now.IsZero() || event.Validate() != nil || event.Kind != EventVersionActive ||
		event.VersionID != lease.VersionID || event.BindingID != lease.BindingID || event.ActorID != "" || !event.OccurredAt.Equal(now) {
		return Binding{}, Version{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cursor, version, binding, err := s.lockedRuntimeApply(lease, now)
	if err != nil {
		return Binding{}, Version{}, err
	}
	if now.Before(version.UpdatedAt) || now.Before(binding.UpdatedAt) ||
		(binding.State != BindingProvisioning && binding.State != BindingReady) {
		return Binding{}, Version{}, ErrConflict
	}
	for _, otherID := range s.byBinding[binding.ID] {
		other := s.versions[otherID]
		if other.State == VersionActive {
			other.State, other.RetainedAt, other.UpdatedAt = VersionRetained, now.UTC(), now.UTC()
			s.versions[otherID] = other
		}
	}
	version.State, version.ReadinessObservedAt, version.ActivatedAt, version.UpdatedAt = VersionActive, now.UTC(), now.UTC(), now.UTC()
	binding.State, binding.ActiveVersion, binding.UpdatedAt = BindingReady, version.Number, now.UTC()
	cursor.State, cursor.CompletedAt, cursor.UpdatedAt, cursor.Lease = "ready", now.UTC(), now.UTC(), RuntimeLease{}
	s.versions[version.ID], s.bindings[binding.ID], s.runtime[version.ID], s.events[event.ID] = version, binding, cursor, event
	return binding, cloneVersion(version), nil
}

func (s *MemoryStore) ApplyRuntimeSecretFailed(_ context.Context, lease RuntimeLease, code string, event Event, now time.Time) (Version, error) {
	if lease.Validate() != nil || !safeCodeRE.MatchString(code) || now.IsZero() || event.Validate() != nil ||
		event.Kind != EventVersionFailed || event.VersionID != lease.VersionID || event.BindingID != lease.BindingID ||
		event.ActorID != "" || !event.OccurredAt.Equal(now) {
		return Version{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cursor, version, binding, err := s.lockedRuntimeApply(lease, now)
	if err != nil {
		return Version{}, err
	}
	if now.Before(version.UpdatedAt) || now.Before(binding.UpdatedAt) {
		return Version{}, ErrConflict
	}
	version.State, version.FailureCode, version.ReadinessObservedAt, version.UpdatedAt = VersionFailed, code, now.UTC(), now.UTC()
	if binding.State == BindingProvisioning {
		binding.State, binding.UpdatedAt = BindingFailed, now.UTC()
		s.bindings[binding.ID] = binding
	}
	cursor.State, cursor.CompletedAt, cursor.UpdatedAt, cursor.Lease = "failed", now.UTC(), now.UTC(), RuntimeLease{}
	s.versions[version.ID], s.runtime[version.ID], s.events[event.ID] = version, cursor, event
	return cloneVersion(version), nil
}

func (s *MemoryStore) lockedRuntimeApply(lease RuntimeLease, now time.Time) (memoryRuntimeReconciliation, Version, Binding, error) {
	cursor, ok := s.runtime[lease.VersionID]
	if !ok || cursor.State != "awaiting" || !sameRuntimeLease(cursor.Lease, lease) || !now.Before(cursor.Lease.Until) || now.Before(cursor.UpdatedAt) {
		return memoryRuntimeReconciliation{}, Version{}, Binding{}, ErrRuntimeLeaseLost
	}
	version, versionOK := s.versions[lease.VersionID]
	binding, bindingOK := s.bindings[lease.BindingID]
	if !versionOK || !bindingOK || version.BindingID != binding.ID || version.State != VersionAwaitingReadiness ||
		version.Provider != ProviderSealedSecrets || version.Artifact == nil {
		return memoryRuntimeReconciliation{}, Version{}, Binding{}, ErrRuntimeLeaseLost
	}
	return cursor, version, binding, nil
}

func sameRuntimeLease(left, right RuntimeLease) bool {
	return left.VersionID == right.VersionID && left.BindingID == right.BindingID && left.Owner == right.Owner &&
		left.Epoch == right.Epoch && left.Until.Equal(right.Until) && runtimeIdentityEqual(left.Identity, right.Identity)
}

func (s *MemoryStore) AcquireRuntimeSecretReadiness(_ context.Context, observation RuntimeWorkerObservation, duration time.Duration) (RuntimeReadinessLease, error) {
	if observation.Validate() != nil || duration < 20*time.Second || duration > time.Hour {
		return RuntimeReadinessLease{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	epoch := int64(1)
	if existing, ok := s.readiness[observation.WorkerID]; ok {
		epoch = existing.Epoch + 1
	}
	lease := RuntimeReadinessLease{RuntimeWorkerObservation: observation, Epoch: epoch, Until: observation.ObservedAt.UTC().Add(duration)}
	s.readiness[observation.WorkerID] = lease
	return lease, nil
}

func (s *MemoryStore) HeartbeatRuntimeSecretReadiness(_ context.Context, lease RuntimeReadinessLease, observedAt time.Time, duration time.Duration) (RuntimeReadinessLease, error) {
	if lease.Validate() != nil || observedAt.IsZero() || observedAt.Before(lease.ObservedAt) ||
		duration < 20*time.Second || duration > time.Hour {
		return RuntimeReadinessLease{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.readiness[lease.WorkerID]
	if !ok || current.Epoch != lease.Epoch || current.StartedAt != lease.StartedAt ||
		!runtimeIdentityEqual(current.Identity, lease.Identity) || current.Until != lease.Until || !observedAt.Before(current.Until) {
		return RuntimeReadinessLease{}, ErrRuntimeLeaseLost
	}
	current.ObservedAt, current.Until = observedAt.UTC(), observedAt.UTC().Add(duration)
	s.readiness[lease.WorkerID] = current
	return current, nil
}

func (s *MemoryStore) RuntimeSecretReady(_ context.Context, identity RuntimeIdentity, now time.Time, maximumAge time.Duration) error {
	if identity.Validate() != nil || now.IsZero() || maximumAge < 2*RuntimeSecretHeartbeatInterval || maximumAge > 5*time.Minute {
		return ErrRuntimeUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, lease := range s.readiness {
		if runtimeIdentityEqual(lease.Identity, identity) && lease.Until.After(now) &&
			!lease.ObservedAt.Before(now.Add(-maximumAge)) && !lease.ObservedAt.After(now.Add(RuntimeSecretReadinessSkew)) {
			return nil
		}
	}
	return ErrRuntimeUnavailable
}

var _ RuntimeStore = (*MemoryStore)(nil)
