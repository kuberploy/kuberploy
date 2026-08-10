package certificates

import (
	"context"
	"crypto/subtle"
	"slices"
	"sync"

	"github.com/kuberploy/kuberploy/internal/secrets"
)

type Store interface {
	Record(context.Context, Version, secrets.Binding, secrets.Version) (Version, bool, error)
	Version(context.Context, string) (Version, error)
	Versions(context.Context, string) ([]Version, error)
}

type MemoryStore struct {
	mu        sync.Mutex
	versions  map[string]Version
	byBinding map[string][]string
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{versions: map[string]Version{}, byBinding: map[string][]string{}}
}

func (s *MemoryStore) Record(_ context.Context, value Version, binding secrets.Binding, secretVersion secrets.Version) (Version, bool, error) {
	if s == nil || value.ValidateFor(binding, secretVersion) != nil {
		return Version{}, false, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.versions[value.SecretVersionID]; ok {
		if !sameVersion(existing, value) {
			return Version{}, false, ErrConflict
		}
		return cloneVersion(existing), true, nil
	}
	for _, id := range s.byBinding[value.BindingID] {
		if s.versions[id].Number == value.Number {
			return Version{}, false, ErrConflict
		}
	}
	s.versions[value.SecretVersionID] = cloneVersion(value)
	s.byBinding[value.BindingID] = append(s.byBinding[value.BindingID], value.SecretVersionID)
	return cloneVersion(value), false, nil
}

func (s *MemoryStore) Version(_ context.Context, secretVersionID string) (Version, error) {
	if s == nil || !uuidRE.MatchString(secretVersionID) {
		return Version{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.versions[secretVersionID]
	if !ok {
		return Version{}, ErrNotFound
	}
	return cloneVersion(value), nil
}

func (s *MemoryStore) Versions(_ context.Context, bindingID string) ([]Version, error) {
	if s == nil || !uuidRE.MatchString(bindingID) {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ids, ok := s.byBinding[bindingID]
	if !ok {
		return []Version{}, nil
	}
	result := make([]Version, 0, len(ids))
	for _, id := range ids {
		result = append(result, cloneVersion(s.versions[id]))
	}
	slices.SortFunc(result, func(left, right Version) int {
		switch {
		case left.Number < right.Number:
			return -1
		case left.Number > right.Number:
			return 1
		default:
			return 0
		}
	})
	return result, nil
}

func sameVersion(left, right Version) bool {
	return left.BindingID == right.BindingID && left.SecretVersionID == right.SecretVersionID && left.Number == right.Number &&
		left.LeafFingerprint == right.LeafFingerprint && left.PublicKeyFingerprint == right.PublicKeyFingerprint &&
		slices.Equal(left.DNSNames, right.DNSNames) && slices.Equal(left.IPAddresses, right.IPAddresses) &&
		left.NotBefore.Equal(right.NotBefore) && left.NotAfter.Equal(right.NotAfter) && left.CreatedBy == right.CreatedBy &&
		left.CreatedAt.Equal(right.CreatedAt) &&
		subtle.ConstantTimeCompare(left.SecretContentFingerprint[:], right.SecretContentFingerprint[:]) == 1
}

var _ Store = (*MemoryStore)(nil)
