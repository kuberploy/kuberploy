package gitpublication

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Store persists immutable intent plus version-fenced provider observations.
// CompareAndSwap is the only mutation so duplicate workers cannot replace a
// candidate, pull request, or verified merge identity.
type Store interface {
	CreatePublication(context.Context, Publication) error
	Publication(context.Context, string) (Publication, error)
	CompareAndSwapPublication(context.Context, Publication, Publication) error
}

// ReconcileStore supplies only protected publications that already have a
// provider pull-request identity and still need observation. Duplicate
// observers are safe because every transition remains version-fenced.
type ReconcileStore interface {
	Store
	RecoverVerifiedPublications(context.Context, int) (int, error)
	PendingPublications(context.Context, int) ([]Publication, error)
}

type MemoryStore struct {
	mu           sync.Mutex
	publications map[string]Publication
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{publications: make(map[string]Publication)}
}

func (s *MemoryStore) CreatePublication(_ context.Context, publication Publication) error {
	if publication.Validate() != nil || publication.State != StatePendingCandidate || publication.Version != 1 {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, exists := s.publications[publication.OperationID]; exists {
		if samePublication(current, publication) {
			return nil
		}
		return ErrConflict
	}
	s.publications[publication.OperationID] = clonePublication(publication)
	return nil
}

func (s *MemoryStore) Publication(_ context.Context, operationID string) (Publication, error) {
	if !uuidPattern.MatchString(operationID) {
		return Publication{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	publication, exists := s.publications[operationID]
	if !exists {
		return Publication{}, ErrNotFound
	}
	if publication.Validate() != nil {
		return Publication{}, ErrInvalid
	}
	return clonePublication(publication), nil
}

func (s *MemoryStore) CompareAndSwapPublication(_ context.Context, previous, next Publication) error {
	if ValidateTransition(previous, next) != nil {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.publications[previous.OperationID]
	if !exists {
		return ErrNotFound
	}
	if !samePublication(current, previous) {
		return ErrConflict
	}
	s.publications[next.OperationID] = clonePublication(next)
	return nil
}

func (s *MemoryStore) PendingPublications(_ context.Context, limit int) ([]Publication, error) {
	if limit <= 0 || limit > 100 {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	values := make([]Publication, 0, limit)
	for _, publication := range s.publications {
		if publication.State == StatePullRequestOpen || publication.State == StatePullRequestClosed || publication.State == StateMergePending {
			values = append(values, clonePublication(publication))
		}
	}
	sort.Slice(values, func(i, j int) bool {
		if values[i].UpdatedAt.Equal(values[j].UpdatedAt) {
			return values[i].OperationID < values[j].OperationID
		}
		return values[i].UpdatedAt.Before(values[j].UpdatedAt)
	})
	if len(values) > limit {
		values = values[:limit]
	}
	return values, nil
}

func (s *MemoryStore) RecoverVerifiedPublications(_ context.Context, limit int) (int, error) {
	if limit <= 0 || limit > 100 {
		return 0, ErrInvalid
	}
	return 0, nil
}

func samePublication(left, right Publication) bool {
	return left.OperationID == right.OperationID && left.BindingID == right.BindingID && left.Repository == right.Repository &&
		left.TargetRef == right.TargetRef && left.BaseRevision == right.BaseRevision && left.WriteBaseRevision == right.WriteBaseRevision && left.CandidateRef == right.CandidateRef &&
		left.CandidateRevision == right.CandidateRevision && left.PullRequestNumber == right.PullRequestNumber &&
		left.PullRequestURL == right.PullRequestURL && left.PullRequestState == right.PullRequestState &&
		left.MergeRevision == right.MergeRevision && left.TargetRevision == right.TargetRevision && left.State == right.State &&
		timeEqual(left.ProviderObservedAt, right.ProviderObservedAt) && left.CreatedAt.Equal(right.CreatedAt) &&
		left.UpdatedAt.Equal(right.UpdatedAt) && left.Version == right.Version
}

func timeEqual(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.Equal(*right)
}

func clonePublication(value Publication) Publication {
	if value.ProviderObservedAt != nil {
		copy := *value.ProviderObservedAt
		value.ProviderObservedAt = &copy
	}
	return value
}
