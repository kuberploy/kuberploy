package gitprojection

import (
	"context"
	"sort"
	"sync"
	"time"
)

// GitHubPushWake is constructed only from an HMAC-verified, durably claimed
// GitHub push receipt. AfterCommit is retained for audit and replay collapse;
// it is never promoted to TargetHeadRevision or passed to the indexer.
type GitHubPushWake struct {
	GitHubAppID    int64     `json:"githubAppId"`
	InstallationID int64     `json:"installationId"`
	RepositoryID   int64     `json:"repositoryId"`
	TargetRef      string    `json:"targetRef"`
	AfterCommit    string    `json:"afterCommit"`
	DeliveryHash   string    `json:"deliveryHash"`
	ReceivedAt     time.Time `json:"receivedAt"`
}

func (w GitHubPushWake) Validate() error {
	if w.GitHubAppID <= 0 || w.InstallationID <= 0 || w.RepositoryID <= 0 || !validTargetRef(w.TargetRef) ||
		!commitRE.MatchString(w.AfterCommit) || !digestRE.MatchString(w.DeliveryHash) || w.ReceivedAt.IsZero() {
		return ErrInvalid
	}
	return nil
}

type BindingWake struct {
	BindingID      string `json:"bindingId"`
	WakeGeneration int64  `json:"wakeGeneration"`
}

type GitHubPushWakeResult struct {
	Replay   bool          `json:"replay"`
	Bindings []BindingWake `json:"bindings"`
}

// GitHubPushWakeStore atomically tombstones the verified delivery and bumps a
// monotonic generation only for exact App/installation/repository/ref matches.
// The normal coordinator remains the sole HeadVerifier consumer.
type GitHubPushWakeStore interface {
	WakeGitHubPush(context.Context, GitHubPushWake) (GitHubPushWakeResult, error)
}

type GitHubPushWaker struct{ Store GitHubPushWakeStore }

func (w GitHubPushWaker) Wake(ctx context.Context, push GitHubPushWake) (GitHubPushWakeResult, error) {
	if w.Store == nil || push.Validate() != nil {
		return GitHubPushWakeResult{}, ErrInvalid
	}
	result, err := w.Store.WakeGitHubPush(ctx, push)
	if err != nil {
		return GitHubPushWakeResult{}, err
	}
	for index, binding := range result.Bindings {
		if !uuidRE.MatchString(binding.BindingID) || binding.WakeGeneration < 1 || index > 0 && result.Bindings[index-1].BindingID >= binding.BindingID {
			return GitHubPushWakeResult{}, ErrConflict
		}
	}
	return result, nil
}

// MemoryGitHubPushWakeStore is an isolated reference implementation used by
// unit tests and development. PostgreSQL folds the same counters into the
// reconciliation cursor so wake completion and lease fencing are atomic.
type MemoryGitHubPushWakeStore struct {
	mu       sync.Mutex
	bindings map[string]memoryWakeBinding
	receipts map[string]GitHubPushWake
	cursors  map[string]memoryWakeCursor
}

type memoryWakeBinding struct {
	Binding Binding
	AppID   int64
	Active  bool
}

type memoryWakeCursor struct {
	WakeGeneration       int64
	ReconciledGeneration int64
	NextPollAt           time.Time
}

type WakeSnapshot struct {
	BindingID            string
	WakeGeneration       int64
	ReconciledGeneration int64
	NextPollAt           time.Time
}

func NewMemoryGitHubPushWakeStore() *MemoryGitHubPushWakeStore {
	return &MemoryGitHubPushWakeStore{bindings: map[string]memoryWakeBinding{}, receipts: map[string]GitHubPushWake{}, cursors: map[string]memoryWakeCursor{}}
}

func (s *MemoryGitHubPushWakeStore) PutGitHubBinding(binding Binding, appID int64, active bool) error {
	if s == nil || binding.Validate() != nil || binding.CredentialMode != CredentialGitHubApp || appID <= 0 {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bindings[binding.ID] = memoryWakeBinding{Binding: cloneBinding(binding), AppID: appID, Active: active}
	return nil
}

func (s *MemoryGitHubPushWakeStore) WakeGitHubPush(_ context.Context, wake GitHubPushWake) (GitHubPushWakeResult, error) {
	if s == nil || wake.Validate() != nil {
		return GitHubPushWakeResult{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.receipts[wake.DeliveryHash]; ok {
		if existing != wake {
			return GitHubPushWakeResult{}, ErrConflict
		}
		return GitHubPushWakeResult{Replay: true}, nil
	}
	s.receipts[wake.DeliveryHash] = wake
	ids := make([]string, 0)
	for id, candidate := range s.bindings {
		binding := candidate.Binding
		if candidate.Active && candidate.AppID == wake.GitHubAppID && binding.Repository.InstallationID == wake.InstallationID &&
			binding.Repository.RepositoryID == wake.RepositoryID && binding.TargetRef == wake.TargetRef {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	result := GitHubPushWakeResult{Bindings: make([]BindingWake, 0, len(ids))}
	for _, id := range ids {
		cursor := s.cursors[id]
		cursor.WakeGeneration++
		if cursor.NextPollAt.IsZero() || wake.ReceivedAt.Before(cursor.NextPollAt) {
			cursor.NextPollAt = wake.ReceivedAt.UTC()
		}
		s.cursors[id] = cursor
		result.Bindings = append(result.Bindings, BindingWake{BindingID: id, WakeGeneration: cursor.WakeGeneration})
	}
	return result, nil
}

func (s *MemoryGitHubPushWakeStore) Snapshot(bindingID string) (WakeSnapshot, error) {
	if s == nil || !uuidRE.MatchString(bindingID) {
		return WakeSnapshot{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cursor, ok := s.cursors[bindingID]
	if !ok {
		return WakeSnapshot{}, ErrNotFound
	}
	return WakeSnapshot{BindingID: bindingID, WakeGeneration: cursor.WakeGeneration,
		ReconciledGeneration: cursor.ReconciledGeneration, NextPollAt: cursor.NextPollAt}, nil
}

// FinishWakeSnapshot acknowledges only the generation a coordinator actually
// claimed. If another push arrived during work, its higher generation and due
// poll remain visible instead of being overwritten by the older completion.
func (s *MemoryGitHubPushWakeStore) FinishWakeSnapshot(bindingID string, claimedGeneration int64, nextSafetyPoll time.Time) error {
	if s == nil || !uuidRE.MatchString(bindingID) || claimedGeneration < 1 || nextSafetyPoll.IsZero() {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cursor, ok := s.cursors[bindingID]
	if !ok || claimedGeneration > cursor.WakeGeneration || claimedGeneration <= cursor.ReconciledGeneration {
		return ErrConflict
	}
	cursor.ReconciledGeneration = claimedGeneration
	if cursor.WakeGeneration == claimedGeneration {
		cursor.NextPollAt = nextSafetyPoll.UTC()
	}
	s.cursors[bindingID] = cursor
	return nil
}

var _ GitHubPushWakeStore = (*MemoryGitHubPushWakeStore)(nil)
