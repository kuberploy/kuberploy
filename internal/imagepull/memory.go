package imagepull

import (
	"context"
	"sort"
	"sync"
	"time"
)

type MemoryStore struct {
	mu        sync.Mutex
	artifacts map[ArtifactKey]Artifact
	readiness map[string]Readiness
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{artifacts: make(map[ArtifactKey]Artifact), readiness: make(map[string]Readiness)}
}

func (s *MemoryStore) EnsureArtifact(ctx context.Context, desired DesiredArtifact, now time.Time) (Artifact, error) {
	if err := ctx.Err(); err != nil {
		return Artifact{}, err
	}
	now = now.UTC()
	if desired.Validate() != nil || now.IsZero() {
		return Artifact{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, exists := s.artifacts[desired.ArtifactKey]; exists &&
		(existing.DesiredArtifact != desired || existing.State == StateFailed) {
		return Artifact{}, ErrConflict
	}
	for key, artifact := range s.artifacts {
		if key.EnvironmentID != desired.EnvironmentID || key.RegistryTargetID != desired.RegistryTargetID || !artifact.Active {
			continue
		}
		if key == desired.ArtifactKey {
			if artifact.DesiredArtifact != desired {
				return Artifact{}, ErrConflict
			}
			return cloneArtifact(artifact), nil
		}
		artifact.Active = false
		artifact.UpdatedAt = now
		clearArtifactLease(&artifact)
		s.artifacts[key] = artifact
	}
	if artifact, exists := s.artifacts[desired.ArtifactKey]; exists {
		artifact.Active = true
		artifact.NextObservationAt = now
		artifact.UpdatedAt = now
		clearArtifactLease(&artifact)
		s.artifacts[desired.ArtifactKey] = artifact
		return cloneArtifact(artifact), nil
	}
	artifact := Artifact{DesiredArtifact: desired, Active: true, State: StateAwaiting,
		NextObservationAt: now, CreatedAt: now, UpdatedAt: now}
	if artifact.Validate() != nil {
		return Artifact{}, ErrInvalid
	}
	s.artifacts[desired.ArtifactKey] = artifact
	return cloneArtifact(artifact), nil
}

func (s *MemoryStore) Artifact(ctx context.Context, key ArtifactKey) (Artifact, error) {
	if err := ctx.Err(); err != nil {
		return Artifact{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	artifact, exists := s.artifacts[key]
	if !exists {
		return Artifact{}, ErrNotFound
	}
	return cloneArtifact(artifact), nil
}

func (s *MemoryStore) ClaimArtifact(ctx context.Context, owner, contract, configDigest string, now time.Time, duration time.Duration) (Lease, bool, error) {
	if err := ctx.Err(); err != nil {
		return Lease{}, false, err
	}
	now = now.UTC()
	if !workerIDPattern.MatchString(owner) || contract != RuntimeContract || !digestPattern(configDigest) || now.IsZero() || duration < time.Second {
		return Lease{}, false, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]ArtifactKey, 0, len(s.artifacts))
	for key, artifact := range s.artifacts {
		if artifact.Active && artifact.State != StateFailed && !artifact.NextObservationAt.After(now) &&
			(artifact.LeaseUntil == nil || !artifact.LeaseUntil.After(now)) {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return Lease{}, false, nil
	}
	sort.Slice(keys, func(left, right int) bool {
		a, b := s.artifacts[keys[left]], s.artifacts[keys[right]]
		if !a.NextObservationAt.Equal(b.NextObservationAt) {
			return a.NextObservationAt.Before(b.NextObservationAt)
		}
		if keys[left].EnvironmentID != keys[right].EnvironmentID {
			return keys[left].EnvironmentID < keys[right].EnvironmentID
		}
		if keys[left].RegistryTargetID != keys[right].RegistryTargetID {
			return keys[left].RegistryTargetID < keys[right].RegistryTargetID
		}
		return keys[left].ProfileRevision < keys[right].ProfileRevision
	})
	key := keys[0]
	artifact := s.artifacts[key]
	artifact.LeaseOwner = owner
	artifact.LeaseEpoch++
	until := now.Add(duration)
	artifact.LeaseUntil = &until
	artifact.WorkerContract = contract
	artifact.WorkerConfigDigest = configDigest
	artifact.UpdatedAt = now
	s.artifacts[key] = artifact
	lease := Lease{Artifact: cloneArtifact(artifact), Owner: owner, Epoch: artifact.LeaseEpoch, Until: until}
	if lease.Validate(now) != nil {
		return Lease{}, false, ErrInvalid
	}
	return lease, true, nil
}

func (s *MemoryStore) HeartbeatArtifact(ctx context.Context, lease Lease, now time.Time, duration time.Duration) (Lease, error) {
	if err := ctx.Err(); err != nil {
		return Lease{}, err
	}
	now = now.UTC()
	if duration < time.Second || lease.Validate(now.Add(-time.Nanosecond)) != nil {
		return Lease{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	artifact, ok := s.leaseArtifact(lease, now)
	if !ok {
		return Lease{}, ErrLeaseLost
	}
	until := now.Add(duration)
	artifact.LeaseUntil = &until
	artifact.UpdatedAt = now
	s.artifacts[artifact.ArtifactKey] = artifact
	return Lease{Artifact: cloneArtifact(artifact), Owner: lease.Owner, Epoch: lease.Epoch, Until: until}, nil
}

func (s *MemoryStore) RecordArtifactReady(ctx context.Context, lease Lease, uid, resourceVersion string, observedAt, next time.Time) (Artifact, error) {
	if err := ctx.Err(); err != nil {
		return Artifact{}, err
	}
	observedAt, next = observedAt.UTC(), next.UTC()
	if !uuidPattern.MatchString(uid) || !resourceVersionPattern.MatchString(resourceVersion) || next.Before(observedAt) {
		return Artifact{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	artifact, ok := s.leaseArtifact(lease, observedAt)
	if !ok {
		return Artifact{}, ErrLeaseLost
	}
	artifact.State = StateReady
	artifact.LastObservedAt = &observedAt
	artifact.NextObservationAt = next
	artifact.ConsecutiveFailures = 0
	artifact.LastFailureCode = ""
	artifact.ObservedUID = uid
	artifact.ObservedResourceVersion = resourceVersion
	artifact.UpdatedAt = observedAt
	clearArtifactLease(&artifact)
	if artifact.Validate() != nil {
		return Artifact{}, ErrInvalid
	}
	s.artifacts[artifact.ArtifactKey] = artifact
	return cloneArtifact(artifact), nil
}

func (s *MemoryStore) RecordArtifactRetry(ctx context.Context, lease Lease, code string, permanent bool, next, now time.Time) (Artifact, error) {
	if err := ctx.Err(); err != nil {
		return Artifact{}, err
	}
	next, now = next.UTC(), now.UTC()
	if !failureCodePattern.MatchString(code) || next.Before(now) {
		return Artifact{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	artifact, ok := s.leaseArtifact(lease, now)
	if !ok {
		return Artifact{}, ErrLeaseLost
	}
	if artifact.ConsecutiveFailures < 30 {
		artifact.ConsecutiveFailures++
	}
	artifact.LastFailureCode = code
	artifact.NextObservationAt = next
	artifact.UpdatedAt = now
	if permanent || artifact.ConsecutiveFailures == 30 {
		artifact.State = StateFailed
	}
	clearArtifactLease(&artifact)
	if artifact.Validate() != nil {
		return Artifact{}, ErrInvalid
	}
	s.artifacts[artifact.ArtifactKey] = artifact
	return cloneArtifact(artifact), nil
}

func (s *MemoryStore) ActiveArtifactsHealthy(ctx context.Context, staleBefore time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	staleBefore = staleBefore.UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, artifact := range s.artifacts {
		if !artifact.Active {
			continue
		}
		if artifact.State != StateReady || artifact.LastObservedAt == nil || artifact.LastObservedAt.Before(staleBefore) {
			return false, nil
		}
	}
	return true, nil
}

func (s *MemoryStore) RecordReadiness(ctx context.Context, next Readiness) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if next.Validate() != nil {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, exists := s.readiness[next.WorkerID]; exists {
		if next.WorkerEpoch < current.WorkerEpoch || next.WorkerEpoch > current.WorkerEpoch+1 {
			return ErrConflict
		}
		if next.WorkerEpoch == current.WorkerEpoch && (next.Contract != current.Contract || next.ConfigDigest != current.ConfigDigest ||
			next.ProfileCount != current.ProfileCount || !next.StartedAt.Equal(current.StartedAt) || next.ObservedAt.Before(current.ObservedAt)) {
			return ErrConflict
		}
	}
	s.readiness[next.WorkerID] = next
	return nil
}

func (s *MemoryStore) RuntimeReady(ctx context.Context, contract, configDigest string, profileCount int, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if contract != RuntimeContract || !digestPattern(configDigest) || profileCount < 1 || profileCount > MaximumProfiles || now.IsZero() {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, readiness := range s.readiness {
		if readiness.Contract == contract && readiness.ConfigDigest == configDigest && readiness.ProfileCount == profileCount &&
			!readiness.ObservedAt.After(now) && readiness.LeaseUntil.After(now) {
			return nil
		}
	}
	return ErrUnavailable
}

func (s *MemoryStore) leaseArtifact(lease Lease, now time.Time) (Artifact, bool) {
	artifact, exists := s.artifacts[lease.Artifact.ArtifactKey]
	return artifact, exists && artifact.Active && artifact.LeaseOwner == lease.Owner && artifact.LeaseEpoch == lease.Epoch &&
		artifact.LeaseUntil != nil && artifact.LeaseUntil.Equal(lease.Until) && artifact.LeaseUntil.After(now)
}

func clearArtifactLease(artifact *Artifact) {
	artifact.LeaseOwner = ""
	artifact.LeaseUntil = nil
	artifact.WorkerContract = ""
	artifact.WorkerConfigDigest = ""
}

func cloneArtifact(artifact Artifact) Artifact {
	if artifact.LastObservedAt != nil {
		value := *artifact.LastObservedAt
		artifact.LastObservedAt = &value
	}
	if artifact.LeaseUntil != nil {
		value := *artifact.LeaseUntil
		artifact.LeaseUntil = &value
	}
	return artifact
}

var _ Store = (*MemoryStore)(nil)
