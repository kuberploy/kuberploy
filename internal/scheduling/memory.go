package scheduling

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/kuberploy/kuberploy/internal/id"
)

type memoryCommand struct {
	digest string
	result MutationResult
}
type MemoryStore struct {
	mu        sync.Mutex
	profiles  map[string]Profile
	revisions map[string]map[int64]Revision
	commands  map[string]memoryCommand
	audits    []AuditEvent
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{profiles: map[string]Profile{}, revisions: map[string]map[int64]Revision{}, commands: map[string]memoryCommand{}}
}
func (s *MemoryStore) replay(c Command, d string) (MutationResult, bool, error) {
	x, ok := s.commands[c.ActorID+"\x00"+c.IdempotencyKey]
	if !ok {
		return MutationResult{}, false, nil
	}
	if x.digest != d {
		return MutationResult{}, true, ErrConflict
	}
	x.result.Replay = true
	return x.result, true, nil
}
func (s *MemoryStore) save(c Command, action, d string, p Profile, v Revision) MutationResult {
	r := MutationResult{Profile: p, Revision: v}
	s.commands[c.ActorID+"\x00"+c.IdempotencyKey] = memoryCommand{d, r}
	s.audits = append(s.audits, AuditEvent{c.ActorID, action, p.ID, c.RequestID, c.IdempotencyKey, v.SpecDigest, v.AssignmentsDigest, v.Revision, c.Now.UTC()})
	return r
}
func (s *MemoryStore) Create(_ context.Context, c Command, name string, spec Spec, a []Assignment) (MutationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if validateCommand(c) != nil || !safeName(name) {
		return MutationResult{}, ErrInvalid
	}
	d, e := requestDigest("create", "", name, 1, spec, a)
	if e != nil {
		return MutationResult{}, e
	}
	if r, ok, e := s.replay(c, d); ok {
		return r, e
	}
	for _, p := range s.profiles {
		if p.Name == name {
			return MutationResult{}, ErrConflict
		}
	}
	spec, a, sd, ad, _ := canonical(spec, a)
	p := Profile{ID: id.New(), Name: name, Lifecycle: Active, CurrentRevision: 1, CreatedBy: c.ActorID, CreatedAt: c.Now.UTC()}
	v := Revision{p.ID, 1, spec, sd, ad, c.ActorID, a, c.Now.UTC()}
	s.profiles[p.ID] = p
	s.revisions[p.ID] = map[int64]Revision{1: v}
	return s.save(c, "create", d, p, v), nil
}
func (s *MemoryStore) Revise(_ context.Context, c Command, ref Ref, spec Spec, a []Assignment) (MutationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if validateCommand(c) != nil {
		return MutationResult{}, ErrInvalid
	}
	d, e := requestDigest("revise", ref.ProfileID, "", ref.Revision, spec, a)
	if e != nil {
		return MutationResult{}, e
	}
	if r, ok, e := s.replay(c, d); ok {
		return r, e
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
	spec, a, sd, ad, _ := canonical(spec, a)
	p.CurrentRevision++
	v := Revision{p.ID, p.CurrentRevision, spec, sd, ad, c.ActorID, a, c.Now.UTC()}
	s.profiles[p.ID] = p
	s.revisions[p.ID][v.Revision] = v
	return s.save(c, "revise", d, p, v), nil
}
func (s *MemoryStore) Deactivate(_ context.Context, c Command, ref Ref) (MutationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if validateCommand(c) != nil {
		return MutationResult{}, ErrInvalid
	}
	p, ok := s.profiles[ref.ProfileID]
	if !ok {
		return MutationResult{}, ErrNotFound
	}
	v := s.revisions[p.ID][p.CurrentRevision]
	d := digest([]byte(fmt.Sprintf("%s\x00deactivate\x00%s\x00%d", Contract, ref.ProfileID, ref.Revision)))
	if r, ok, e := s.replay(c, d); ok {
		return r, e
	}
	if p.Lifecycle != Active {
		return MutationResult{}, ErrInactive
	}
	if p.CurrentRevision != ref.Revision {
		return MutationResult{}, ErrConflict
	}
	at := c.Now.UTC()
	p.Lifecycle = Deactivated
	p.DeactivatedBy = c.ActorID
	p.DeactivatedAt = &at
	s.profiles[p.ID] = p
	return s.save(c, "deactivate", d, p, v), nil
}
func (s *MemoryStore) Current(ctx context.Context, id string) (Profile, Revision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.profiles[id]
	if !ok {
		return Profile{}, Revision{}, ErrNotFound
	}
	return p, s.revisions[id][p.CurrentRevision], nil
}
func (s *MemoryStore) Revision(ctx context.Context, ref Ref) (Profile, Revision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.profiles[ref.ProfileID]
	if !ok {
		return Profile{}, Revision{}, ErrNotFound
	}
	v, ok := s.revisions[ref.ProfileID][ref.Revision]
	if !ok {
		return Profile{}, Revision{}, ErrNotFound
	}
	return p, v, nil
}
func (s *MemoryStore) AuditEvents() []AuditEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]AuditEvent(nil), s.audits...)
}

func (s *MemoryStore) Catalog(_ context.Context, limit int) ([]Entry, error) {
	if limit < 1 || limit > 500 {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]Entry, 0, len(s.profiles))
	for _, profile := range s.profiles {
		items = append(items, Entry{Profile: profile, Revision: s.revisions[profile.ID][profile.CurrentRevision]})
	}
	sortEntries(items)
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (s *MemoryStore) Assigned(_ context.Context, target Target, limit int) ([]Entry, error) {
	if validateTarget(target) != nil || limit < 1 || limit > 500 {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	items := []Entry{}
	for _, profile := range s.profiles {
		if profile.Lifecycle != Active {
			continue
		}
		revision := s.revisions[profile.ID][profile.CurrentRevision]
		for _, assignment := range revision.Assignments {
			if match(assignment, target) {
				items = append(items, Entry{Profile: profile, Revision: revision})
				break
			}
		}
	}
	sortEntries(items)
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func sortEntries(items []Entry) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Profile.Name == items[j].Profile.Name {
			return items[i].Profile.ID < items[j].Profile.ID
		}
		return items[i].Profile.Name < items[j].Profile.Name
	})
}
