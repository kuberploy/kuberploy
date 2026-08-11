package environmentfoundation

import (
	"context"
	"sort"
	"sync"
	"time"
)

type AuthorityRecord struct {
	Identity  EnvironmentIdentity
	Authority GitAuthority
}

type MemoryStore struct {
	mu          sync.Mutex
	authorities map[string]AuthorityRecord
	intents     map[string]Intent
	readiness   map[string]Readiness
}

func NewMemoryStore(records []AuthorityRecord) (*MemoryStore, error) {
	s := &MemoryStore{authorities: map[string]AuthorityRecord{}, intents: map[string]Intent{}, readiness: map[string]Readiness{}}
	for _, record := range records {
		if record.Identity.Validate() != nil || record.Authority.Validate() != nil {
			return nil, ErrInvalid
		}
		if _, exists := s.authorities[record.Identity.EnvironmentID]; exists {
			return nil, ErrConflict
		}
		s.authorities[record.Identity.EnvironmentID] = record
	}
	return s, nil
}

func (s *MemoryStore) EnsureIntent(ctx context.Context, request EnsureRequest) (Intent, error) {
	if err := ctx.Err(); err != nil {
		return Intent{}, err
	}
	request.Now = request.Now.UTC()
	if !uuidRE.MatchString(request.IntentID) || !uuidRE.MatchString(request.EnvironmentID) || request.Profile.Validate() != nil || request.Now.IsZero() {
		return Intent{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, found := s.authorities[request.EnvironmentID]
	if !found {
		return Intent{}, ErrNotFound
	}
	if record.Authority.ClusterID != request.Profile.ClusterID || record.Authority.BindingID != request.Profile.PlatformBindingID {
		return Intent{}, ErrConflict
	}
	value, err := buildIntent(request.IntentID, record.Identity, record.Authority, request.Profile, request.Now)
	if err != nil {
		return Intent{}, err
	}
	if existing, exists := s.intents[request.IntentID]; exists {
		if existing.IntentDigest != value.IntentDigest || existing.Authority.BindingID != request.Profile.PlatformBindingID {
			return Intent{}, ErrConflict
		}
		return cloneIntent(existing), nil
	}
	for id, existing := range s.intents {
		if existing.EnvironmentID == request.EnvironmentID && existing.Active {
			if existing.IntentDigest == value.IntentDigest {
				return cloneIntent(existing), nil
			}
			// Finish or recover the exact durable predecessor before rotating its
			// material. This closes the push-before-receipt boundary across a
			// rolling worker/profile upgrade.
			if existing.State == StatePending || existing.State == StateClaimed {
				return cloneIntent(existing), nil
			}
			now := request.Now
			existing.Active = false
			existing.State = StateSuperseded
			existing.CompletedAt = &now
			existing.UpdatedAt = now
			clearLease(&existing)
			s.intents[id] = existing
		}
	}
	s.intents[value.ID] = value
	return cloneIntent(value), nil
}

func (s *MemoryStore) Intent(ctx context.Context, id string) (Intent, error) {
	if err := ctx.Err(); err != nil {
		return Intent{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.intents[id]
	if !ok {
		return Intent{}, ErrNotFound
	}
	return cloneIntent(v), nil
}

func (s *MemoryStore) ExpectedPreimage(ctx context.Context, id string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	if !uuidRE.MatchString(id) {
		return "", false, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.intents[id]
	if !ok {
		return "", false, ErrNotFound
	}
	var selected Intent
	found := false
	for otherID, candidate := range s.intents {
		if otherID == id || candidate.EnvironmentID != current.EnvironmentID || candidate.Path != current.Path ||
			candidate.PublishedAt == nil || candidate.CommittedRevision == "" {
			continue
		}
		if !found || candidate.PublishedAt.After(*selected.PublishedAt) ||
			(candidate.PublishedAt.Equal(*selected.PublishedAt) && candidate.ID > selected.ID) {
			selected, found = candidate, true
		}
	}
	if !found {
		return "", false, nil
	}
	if selected.Validate() != nil || !digestRE.MatchString(selected.ManifestDigest) {
		return "", false, ErrConflict
	}
	return selected.ManifestDigest, true, nil
}

func (s *MemoryStore) ClaimIntent(ctx context.Context, owner, profileDigest, publisherDigest string, now time.Time, duration time.Duration) (Lease, bool, error) {
	if err := ctx.Err(); err != nil {
		return Lease{}, false, err
	}
	now = now.UTC()
	if !workerIDRE.MatchString(owner) || !digestRE.MatchString(profileDigest) || !digestRE.MatchString(publisherDigest) || now.IsZero() || duration < MinimumLease || duration > MaximumLease {
		return Lease{}, false, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0)
	for id, v := range s.intents {
		bindingClaimed := false
		for otherID, other := range s.intents {
			if otherID != id && other.Authority.BindingID == v.Authority.BindingID && other.Active && other.State == StateClaimed &&
				other.LeaseUntil != nil && other.LeaseUntil.After(now) {
				bindingClaimed = true
				break
			}
		}
		if v.Active && v.PublisherConfigDigest == publisherDigest &&
			!bindingClaimed && (v.State == StatePending || v.State == StateClaimed) && !v.NextAttemptAt.After(now) && (v.LeaseUntil == nil || !v.LeaseUntil.After(now)) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return Lease{}, false, nil
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := s.intents[ids[i]], s.intents[ids[j]]
		if (a.ProfileDigest == profileDigest) != (b.ProfileDigest == profileDigest) {
			return a.ProfileDigest != profileDigest
		}
		if !a.NextAttemptAt.Equal(b.NextAttemptAt) {
			return a.NextAttemptAt.Before(b.NextAttemptAt)
		}
		return a.ID < b.ID
	})
	v := s.intents[ids[0]]
	v.State = StateClaimed
	v.LeaseOwner = owner
	v.LeaseEpoch++
	until := now.Add(duration)
	v.LeaseUntil = &until
	v.Attempts++
	v.UpdatedAt = now
	s.intents[v.ID] = v
	l := Lease{Intent: cloneIntent(v), Owner: owner, Epoch: v.LeaseEpoch, Until: until}
	if l.Validate(now) != nil {
		return Lease{}, false, ErrInvalid
	}
	return l, true, nil
}

func (s *MemoryStore) HeartbeatIntent(ctx context.Context, lease Lease, now time.Time, duration time.Duration) (Lease, error) {
	if err := ctx.Err(); err != nil {
		return Lease{}, err
	}
	now = now.UTC()
	if duration < MinimumLease || duration > MaximumLease || lease.Validate(now.Add(-time.Nanosecond)) != nil {
		return Lease{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.leased(lease, now)
	if !ok {
		return Lease{}, ErrLeaseLost
	}
	until := now.Add(duration)
	v.LeaseUntil = &until
	v.UpdatedAt = now
	s.intents[v.ID] = v
	return Lease{Intent: cloneIntent(v), Owner: lease.Owner, Epoch: lease.Epoch, Until: until}, nil
}

func (s *MemoryStore) BindWriteBase(ctx context.Context, lease Lease, revision string, observedAt, now time.Time) (Intent, error) {
	if err := ctx.Err(); err != nil {
		return Intent{}, err
	}
	now, observedAt = now.UTC(), observedAt.UTC()
	if !gitCommitRE.MatchString(revision) || observedAt.IsZero() || now.IsZero() || observedAt.After(now) {
		return Intent{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.intents[lease.Intent.ID]
	if !ok || !v.Active || v.State != StateClaimed || v.LeaseOwner != lease.Owner || v.LeaseEpoch != lease.Epoch || v.LeaseUntil == nil || !v.LeaseUntil.After(now) {
		return Intent{}, ErrLeaseLost
	}
	if v.WriteBaseRevision != "" {
		if v.WriteBaseRevision == revision && v.WriteBaseObservedAt != nil && v.WriteBaseObservedAt.Equal(observedAt) {
			return cloneIntent(v), nil
		}
		return Intent{}, ErrConflict
	}
	v.WriteBaseRevision, v.WriteBaseObservedAt, v.UpdatedAt = revision, &observedAt, now
	if v.Validate() != nil {
		return Intent{}, ErrInvalid
	}
	s.intents[v.ID] = v
	return cloneIntent(v), nil
}

func (s *MemoryStore) RecordReady(ctx context.Context, lease Lease, receipt PublicationReceipt, now time.Time) (Intent, error) {
	if err := ctx.Err(); err != nil {
		return Intent{}, err
	}
	now = now.UTC()
	if receipt.Validate(lease.Intent) != nil || receipt.ObservedAt.After(now) {
		return Intent{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.leased(lease, now)
	if !ok {
		return Intent{}, ErrLeaseLost
	}
	if receipt.Validate(v) != nil {
		return Intent{}, ErrConflict
	}
	v.State = StateReady
	v.ConsecutiveFailures = 0
	v.LastFailureCode = ""
	v.CommittedRevision = receipt.CommittedRevision
	v.CommittedParentRevision = receipt.ParentRevision
	v.ProviderRequest = receipt.ProviderRequest
	published := receipt.ObservedAt.UTC()
	v.PublishedAt = &published
	v.CompletedAt = &published
	v.UpdatedAt = now
	clearLease(&v)
	if v.Validate() != nil {
		return Intent{}, ErrInvalid
	}
	s.intents[v.ID] = v
	return cloneIntent(v), nil
}

func (s *MemoryStore) RecordRetry(ctx context.Context, lease Lease, code string, permanent bool, next, now time.Time) (Intent, error) {
	if err := ctx.Err(); err != nil {
		return Intent{}, err
	}
	now, next = now.UTC(), next.UTC()
	if !failureRE.MatchString(code) || next.Before(now) {
		return Intent{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.leased(lease, now)
	if !ok {
		return Intent{}, ErrLeaseLost
	}
	v.ConsecutiveFailures++
	if v.ConsecutiveFailures > MaximumAttempts {
		v.ConsecutiveFailures = MaximumAttempts
	}
	v.LastFailureCode = code
	v.NextAttemptAt = next
	v.UpdatedAt = now
	clearLease(&v)
	if permanent || v.Attempts >= MaximumAttempts {
		v.State = StateFailed
		v.Active = false
		v.CompletedAt = &now
	} else {
		v.State = StatePending
	}
	if v.Validate() != nil {
		return Intent{}, ErrInvalid
	}
	s.intents[v.ID] = v
	return cloneIntent(v), nil
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
	if old, ok := s.readiness[next.WorkerID]; ok {
		if next.WorkerEpoch < old.WorkerEpoch || next.WorkerEpoch > old.WorkerEpoch+1 {
			return ErrConflict
		}
		if next.WorkerEpoch == old.WorkerEpoch && (next.Contract != old.Contract || next.ProfileDigest != old.ProfileDigest || next.PublisherConfigDigest != old.PublisherConfigDigest || !next.StartedAt.Equal(old.StartedAt) || next.ObservedAt.Before(old.ObservedAt)) {
			return ErrConflict
		}
	}
	s.readiness[next.WorkerID] = next
	return nil
}

func (s *MemoryStore) ExactReady(ctx context.Context, profile, publisher string, count int, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !digestRE.MatchString(profile) || !digestRE.MatchString(publisher) || count < 0 || count > 10000 || now.IsZero() {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.authorities) != count {
		return ErrUnavailable
	}
	active := 0
	for _, v := range s.intents {
		if v.Active && v.ProfileDigest == profile && v.PublisherConfigDigest == publisher {
			active++
			if v.State != StateReady {
				return ErrUnavailable
			}
		}
	}
	if active != count {
		return ErrUnavailable
	}
	for _, r := range s.readiness {
		if r.Contract == Contract && r.ProfileDigest == profile && r.PublisherConfigDigest == publisher && r.ActiveIntentCount == count && !r.ObservedAt.After(now) && r.LeaseUntil.After(now) {
			return nil
		}
	}
	return ErrUnavailable
}

func (s *MemoryStore) leased(lease Lease, now time.Time) (Intent, bool) {
	v, ok := s.intents[lease.Intent.ID]
	return v, ok && v.Active && v.State == StateClaimed && v.LeaseOwner == lease.Owner && v.LeaseEpoch == lease.Epoch && v.LeaseUntil != nil && v.LeaseUntil.Equal(lease.Until) && v.LeaseUntil.After(now)
}
func clearLease(v *Intent) { v.LeaseOwner = ""; v.LeaseUntil = nil }
func cloneIntent(v Intent) Intent {
	v.Manifest = append([]byte(nil), v.Manifest...)
	if v.LeaseUntil != nil {
		x := *v.LeaseUntil
		v.LeaseUntil = &x
	}
	if v.WriteBaseObservedAt != nil {
		x := *v.WriteBaseObservedAt
		v.WriteBaseObservedAt = &x
	}
	if v.PublishedAt != nil {
		x := *v.PublishedAt
		v.PublishedAt = &x
	}
	if v.CompletedAt != nil {
		x := *v.CompletedAt
		v.CompletedAt = &x
	}
	return v
}

var _ Store = (*MemoryStore)(nil)
