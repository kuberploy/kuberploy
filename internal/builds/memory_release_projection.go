package builds

import (
	"context"
	"sort"
	"time"
)

func (s *MemoryStore) ClaimNextReleaseProjection(_ context.Context, owner string, now time.Time, leaseDuration time.Duration) (ReleaseProjectionWork, error) {
	if !validOwnerLease(owner, leaseDuration) || now.IsZero() {
		return ReleaseProjectionWork{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.releaseProjections))
	for attemptID, projection := range s.releaseProjections {
		if projection.availableAt.After(now.UTC()) || projection.state != ReleaseProjectionPending &&
			(projection.state != ReleaseProjectionProcessing || projection.leaseUntil.After(now.UTC())) {
			continue
		}
		ids = append(ids, attemptID)
	}
	sort.Slice(ids, func(i, j int) bool {
		left, right := s.releaseProjections[ids[i]], s.releaseProjections[ids[j]]
		if left.availableAt.Equal(right.availableAt) {
			if left.createdAt.Equal(right.createdAt) {
				return ids[i] < ids[j]
			}
			return left.createdAt.Before(right.createdAt)
		}
		return left.availableAt.Before(right.availableAt)
	})
	for _, attemptID := range ids {
		projection := s.releaseProjections[attemptID]
		if projection.attempts >= 20 {
			completed := now.UTC()
			projection.state, projection.failureCode, projection.completedAt = ReleaseProjectionFailed, "projection-attempts-exhausted", &completed
			projection.leaseOwner, projection.leaseUntil, projection.updatedAt = "", time.Time{}, completed
			s.releaseProjections[attemptID] = projection
			continue
		}
		attempt, exists := s.attempts[attemptID]
		if !exists || attempt.State != AttemptSucceeded || attempt.Result == nil {
			completed := now.UTC()
			projection.state, projection.failureCode, projection.completedAt = ReleaseProjectionFailed, "build-release-input-invalid", &completed
			projection.leaseOwner, projection.leaseUntil, projection.updatedAt = "", time.Time{}, completed
			s.releaseProjections[attemptID] = projection
			continue
		}
		definition := attempt.SourceSnapshot
		if definition.validate() != nil || definition.ID != attempt.DefinitionID {
			completed := now.UTC()
			projection.state, projection.failureCode, projection.completedAt = ReleaseProjectionFailed, "build-source-snapshot-invalid", &completed
			projection.leaseOwner, projection.leaseUntil, projection.updatedAt = "", time.Time{}, completed
			s.releaseProjections[attemptID] = projection
			continue
		}
		projection.state, projection.leaseOwner = ReleaseProjectionProcessing, owner
		projection.leaseUntil = now.UTC().Add(leaseDuration)
		projection.leaseEpoch++
		projection.attempts++
		projection.failureCode, projection.updatedAt = "", now.UTC()
		s.releaseProjections[attemptID] = projection
		return ReleaseProjectionWork{
			Attempt: cloneAttempt(attempt), Definition: cloneDefinition(definition), Attempts: projection.attempts,
			Lease: ReleaseProjectionLease{AttemptID: attemptID, Owner: owner, Epoch: projection.leaseEpoch, Until: projection.leaseUntil},
		}, nil
	}
	return ReleaseProjectionWork{}, ErrNotFound
}

func (s *MemoryStore) HeartbeatReleaseProjection(_ context.Context, lease ReleaseProjectionLease, now time.Time, duration time.Duration) (ReleaseProjectionLease, error) {
	if !validProjectionLease(lease) || !validOwnerLease(lease.Owner, duration) || now.IsZero() {
		return ReleaseProjectionLease{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	projection, ok := s.releaseProjections[lease.AttemptID]
	if !ok || !projectionLeaseMatches(projection, lease, now) {
		return ReleaseProjectionLease{}, projectionLeaseError(lease)
	}
	projection.leaseUntil, projection.updatedAt = now.UTC().Add(duration), now.UTC()
	s.releaseProjections[lease.AttemptID] = projection
	lease.Until = projection.leaseUntil
	return lease, nil
}

func (s *MemoryStore) RetryReleaseProjection(_ context.Context, lease ReleaseProjectionLease, code string, now, availableAt time.Time) (bool, error) {
	if !validProjectionLease(lease) || validateFailureCode(code) != nil || now.IsZero() || availableAt.Before(now.UTC()) {
		return false, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	projection, ok := s.releaseProjections[lease.AttemptID]
	if !ok || !projectionLeaseMatches(projection, lease, now) {
		return false, projectionLeaseError(lease)
	}
	projection.leaseOwner, projection.leaseUntil = "", time.Time{}
	projection.failureCode, projection.updatedAt = code, now.UTC()
	if projection.attempts >= 20 {
		completed := now.UTC()
		projection.state, projection.completedAt = ReleaseProjectionFailed, &completed
		s.releaseProjections[lease.AttemptID] = projection
		return false, nil
	}
	projection.state, projection.availableAt = ReleaseProjectionPending, availableAt.UTC()
	s.releaseProjections[lease.AttemptID] = projection
	return true, nil
}

func (s *MemoryStore) FailReleaseProjection(_ context.Context, lease ReleaseProjectionLease, code string, now time.Time) error {
	if !validProjectionLease(lease) || validateFailureCode(code) != nil || now.IsZero() {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	projection, ok := s.releaseProjections[lease.AttemptID]
	if !ok || !projectionLeaseMatches(projection, lease, now) {
		return projectionLeaseError(lease)
	}
	completed := now.UTC()
	projection.state, projection.failureCode, projection.completedAt = ReleaseProjectionFailed, code, &completed
	projection.leaseOwner, projection.leaseUntil, projection.updatedAt = "", time.Time{}, completed
	s.releaseProjections[lease.AttemptID] = projection
	return nil
}

func (s *MemoryStore) CompleteReleaseProjection(_ context.Context, lease ReleaseProjectionLease, releaseID, cacheGenerationID string, now time.Time) error {
	if !validProjectionLease(lease) || !uuidRE.MatchString(releaseID) || cacheGenerationID != "" && !uuidRE.MatchString(cacheGenerationID) || now.IsZero() {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	projection, ok := s.releaseProjections[lease.AttemptID]
	if !ok || !projectionLeaseMatches(projection, lease, now) {
		return projectionLeaseError(lease)
	}
	completed := now.UTC()
	projection.state, projection.failureCode, projection.completedAt = ReleaseProjectionSucceeded, "", &completed
	projection.releaseID, projection.cacheGenerationID = releaseID, cacheGenerationID
	projection.leaseOwner, projection.leaseUntil, projection.updatedAt = "", time.Time{}, completed
	s.releaseProjections[lease.AttemptID] = projection
	return nil
}

func validProjectionLease(lease ReleaseProjectionLease) bool {
	return uuidRE.MatchString(lease.AttemptID) && lease.Owner != "" && len(lease.Owner) <= 128 && lease.Epoch > 0 && !lease.Until.IsZero()
}

func projectionLeaseMatches(projection memoryReleaseProjection, lease ReleaseProjectionLease, now time.Time) bool {
	return projection.state == ReleaseProjectionProcessing && projection.leaseOwner == lease.Owner && projection.leaseEpoch == lease.Epoch &&
		projection.leaseUntil.Equal(lease.Until) && projection.leaseUntil.After(now.UTC())
}
