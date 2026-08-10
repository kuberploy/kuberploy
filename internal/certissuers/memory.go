package certissuers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/kuberploy/kuberploy/internal/id"
)

type memoryCommand struct {
	digest string
	result MutationResult
}
type MemoryStore struct {
	mu           sync.Mutex
	profiles     map[string]Profile
	revisions    map[string]map[int64]Revision
	commands     map[string]memoryCommand
	observations map[string]Observation
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{profiles: map[string]Profile{}, revisions: map[string]map[int64]Revision{}, commands: map[string]memoryCommand{}, observations: map[string]Observation{}}
}
func obsKey(id string, rev int64) string { return fmt.Sprintf("%s/%d", id, rev) }
func (s *MemoryStore) Create(_ context.Context, c Command, name string, spec Spec) (MutationResult, error) {
	return s.mutate(c, "create", name, Ref{}, spec)
}
func (s *MemoryStore) Revise(_ context.Context, c Command, ref Ref, spec Spec) (MutationResult, error) {
	return s.mutate(c, "revise", "", ref, spec)
}
func (s *MemoryStore) mutate(c Command, action, name string, ref Ref, spec Spec) (MutationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validateCommand(c) || action == "create" && !dnsLabelRE.MatchString(name) || action == "revise" && (!uuidRE.MatchString(ref.ProfileID) || ref.Revision < 1) {
		return MutationResult{}, ErrInvalid
	}
	d, err := commandDigest(action, ref.ProfileID, name, ref.Revision, spec)
	if err != nil {
		return MutationResult{}, err
	}
	key := c.ActorID + "\x00" + c.IdempotencyKey
	if old, ok := s.commands[key]; ok {
		if old.digest != d {
			return MutationResult{}, ErrConflict
		}
		out := old.result
		out.Replay = true
		return out, nil
	}
	clean, solver, sd, _ := normalizeSpec(spec)
	var p Profile
	var rev int64
	if action == "create" {
		for _, x := range s.profiles {
			if x.Name == name {
				return MutationResult{}, ErrConflict
			}
		}
		p = Profile{ID: id.New(), Name: name, Lifecycle: Active, CurrentRevision: 1, CreatedBy: c.ActorID, CreatedAt: c.Now.UTC()}
		rev = 1
		s.revisions[p.ID] = map[int64]Revision{}
	} else {
		var ok bool
		p, ok = s.profiles[ref.ProfileID]
		if !ok {
			return MutationResult{}, ErrNotFound
		}
		if p.Lifecycle != Active {
			return MutationResult{}, ErrInactive
		}
		if p.CurrentRevision != ref.Revision {
			return MutationResult{}, ErrConflict
		}
		rev = p.CurrentRevision + 1
		p.CurrentRevision = rev
	}
	r := Revision{ProfileID: p.ID, Revision: rev, Solver: solver, Spec: clean, SpecDigest: sd, CreatedBy: c.ActorID, CreatedAt: c.Now.UTC()}
	s.profiles[p.ID] = p
	s.revisions[p.ID][rev] = r
	s.observations[obsKey(p.ID, rev)] = Observation{ProfileID: p.ID, Revision: rev, State: Pending, UpdatedAt: c.Now.UTC()}
	out := MutationResult{Profile: p, Revision: r}
	s.commands[key] = memoryCommand{d, out}
	return out, nil
}
func (s *MemoryStore) Deactivate(_ context.Context, c Command, ref Ref) (MutationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validateCommand(c) || !uuidRE.MatchString(ref.ProfileID) || ref.Revision < 1 {
		return MutationResult{}, ErrInvalid
	}
	d := digestText(fmt.Sprintf("%s\x00deactivate\x00%s\x00%d", Contract, ref.ProfileID, ref.Revision))
	key := c.ActorID + "\x00" + c.IdempotencyKey
	if old, ok := s.commands[key]; ok {
		if old.digest != d {
			return MutationResult{}, ErrConflict
		}
		out := old.result
		out.Replay = true
		return out, nil
	}
	p, ok := s.profiles[ref.ProfileID]
	if !ok {
		return MutationResult{}, ErrNotFound
	}
	if p.Lifecycle != Active {
		return MutationResult{}, ErrInactive
	}
	if p.CurrentRevision != ref.Revision {
		return MutationResult{}, ErrConflict
	}
	now := c.Now.UTC()
	p.Lifecycle = Deactivated
	p.DeactivatedBy = c.ActorID
	p.DeactivatedAt = &now
	s.profiles[p.ID] = p
	r := s.revisions[p.ID][ref.Revision]
	out := MutationResult{Profile: p, Revision: r}
	s.commands[key] = memoryCommand{d, out}
	return out, nil
}
func digestText(v string) string { sum := sha256Sum([]byte(v)); return "sha256:" + sum }
func sha256Sum(v []byte) string {
	h := sha256.New()
	_, _ = h.Write(v)
	return hex.EncodeToString(h.Sum(nil))
}
func (s *MemoryStore) Current(_ context.Context, id string) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.profiles[id]
	if !ok {
		return Entry{}, ErrNotFound
	}
	r := s.revisions[id][p.CurrentRevision]
	r.Spec = cloneSpec(r.Spec)
	return Entry{p, r}, nil
}
func (s *MemoryStore) List(_ context.Context, limit int) ([]Entry, error) {
	if limit < 1 || limit > 500 {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Entry{}
	for _, p := range s.profiles {
		r := s.revisions[p.ID][p.CurrentRevision]
		r.Spec = cloneSpec(r.Spec)
		out = append(out, Entry{p, r})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Profile.Name < out[j].Profile.Name })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (s *MemoryStore) PendingMaterialization(_ context.Context, limit int) ([]Desired, error) {
	if limit < 1 || limit > 500 {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Desired{}
	for _, p := range s.profiles {
		if p.Lifecycle != Active {
			continue
		}
		r := s.revisions[p.ID][p.CurrentRevision]
		o := s.observations[obsKey(p.ID, r.Revision)]
		if o.State != Ready || o.ObservedSpecDigest != r.SpecDigest {
			out = append(out, Desired{p.ID, p.Name, r.SpecDigest, r.Revision, r.Solver, cloneSpec(r.Spec)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (s *MemoryStore) PendingDematerialization(_ context.Context, limit int) ([]Desired, error) {
	if limit < 1 || limit > 500 {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Desired{}
	for _, p := range s.profiles {
		if p.Lifecycle == Deactivated {
			r := s.revisions[p.ID][p.CurrentRevision]
			out = append(out, Desired{p.ID, p.Name, r.SpecDigest, r.Revision, r.Solver, cloneSpec(r.Spec)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (s *MemoryStore) RecordObservation(_ context.Context, o Observation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.profiles[o.ProfileID]
	if !ok {
		return ErrNotFound
	}
	r, ok := s.revisions[p.ID][o.Revision]
	if !ok {
		return ErrNotFound
	}
	if !validObservation(o, r.SpecDigest) {
		return ErrInvalid
	}
	s.observations[obsKey(o.ProfileID, o.Revision)] = o
	return nil
}
func (s *MemoryStore) Observation(_ context.Context, profileID string, revision int64) (Observation, error) {
	if !uuidRE.MatchString(profileID) || revision < 1 {
		return Observation{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.observations[obsKey(profileID, revision)]
	if !ok {
		return Observation{}, ErrNotFound
	}
	if o.ObservedAt != nil {
		v := *o.ObservedAt
		o.ObservedAt = &v
	}
	return o, nil
}
func validObservation(o Observation, desired string) bool {
	if !uuidRE.MatchString(o.ProfileID) || o.Revision < 1 || o.UpdatedAt.IsZero() || o.UpdatedAt.Location() != time.UTC || len(o.Reason) > 1024 {
		return false
	}
	if o.State == Pending {
		return o.ObservedSpecDigest == "" && o.ObservedGeneration == 0 && o.ObservedAt == nil
	}
	return (o.State == Ready || o.State == Degraded) && digestRE.MatchString(o.ObservedSpecDigest) && o.ObservedGeneration > 0 && o.ObservedAt != nil && o.ObservedAt.Location() == time.UTC && !o.ObservedAt.After(o.UpdatedAt) && (o.State != Ready || o.ObservedSpecDigest == desired)
}
func (s *MemoryStore) ReadyForHostname(_ context.Context, host string, now time.Time, maxAge time.Duration, limit int) ([]TenantIdentity, error) {
	if !validHostname(host, true) || !validFreshness(now, maxAge) || limit < 1 || limit > 500 {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []TenantIdentity{}
	for _, p := range s.profiles {
		if p.Lifecycle != Active {
			continue
		}
		r := s.revisions[p.ID][p.CurrentRevision]
		o := s.observations[obsKey(p.ID, r.Revision)]
		if o.State == Ready && o.ObservedSpecDigest == r.SpecDigest && o.ObservedAt != nil && !o.ObservedAt.Before(now.Add(-maxAge)) && !o.ObservedAt.After(now.Add(30*time.Second)) && coversHostname(r.Spec, r.Solver, host) {
			out = append(out, TenantIdentity{ProfileID: p.ID, Name: p.Name, Revision: r.Revision, Solver: r.Solver, Environment: environmentForServer(r.Spec.ACME.Server)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
