package middlewareprofiles

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
	mu         sync.Mutex
	profiles   map[string]Profile
	revisions  map[string]map[int64]Revision
	commands   map[string]memoryCommand
	references map[string][]Reference
	audits     []AuditEvent
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{profiles: map[string]Profile{}, revisions: map[string]map[int64]Revision{}, commands: map[string]memoryCommand{}, references: map[string][]Reference{}}
}
func (s *MemoryStore) replay(c Command, digest string) (MutationResult, bool, error) {
	value, ok := s.commands[c.ActorID+"\x00"+c.IdempotencyKey]
	if !ok {
		return MutationResult{}, false, nil
	}
	if value.digest != digest {
		return MutationResult{}, true, ErrConflict
	}
	value.result.Replay = true
	return value.result, true, nil
}
func (s *MemoryStore) save(c Command, action, digest string, p Profile, r Revision) MutationResult {
	result := MutationResult{Profile: p, Revision: r}
	s.commands[c.ActorID+"\x00"+c.IdempotencyKey] = memoryCommand{digest, result}
	s.audits = append(s.audits, AuditEvent{c.ActorID, action, p.ID, c.RequestID, c.IdempotencyKey, r.SpecDigest, r.AssignmentsDigest, r.Revision, c.Now.UTC()})
	return result
}
func (s *MemoryStore) Create(_ context.Context, c Command, name string, spec Spec, assignments []Assignment) (MutationResult, error) {
	return s.create(c, name, spec, assignments, nil, "create")
}
func (s *MemoryStore) create(c Command, name string, spec Spec, assignments []Assignment, source *Ref, action string) (MutationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if validateCommand(c) != nil || !dnsLabelRE.MatchString(name) {
		return MutationResult{}, ErrInvalid
	}
	d, e := commandDigest(action, "", name, 1, spec, assignments, source)
	if e != nil {
		return MutationResult{}, e
	}
	if r, ok, e := s.replay(c, d); ok {
		return r, e
	}
	clean, a, sd, ad, _ := canonical(spec, assignments)
	p := Profile{ID: id.New(), Name: name, Lifecycle: Active, CurrentRevision: 1, CreatedBy: c.ActorID, CreatedAt: c.Now.UTC()}
	r := Revision{ProfileID: p.ID, Revision: 1, Spec: clean, SpecDigest: sd, AssignmentsDigest: ad, CreatedBy: c.ActorID, Assignments: a, CreatedAt: c.Now.UTC(), ClonedFrom: source}
	s.profiles[p.ID] = p
	s.revisions[p.ID] = map[int64]Revision{1: r}
	return s.save(c, action, d, p, r), nil
}
func (s *MemoryStore) Clone(ctx context.Context, c Command, source Ref, name string, assignments []Assignment) (MutationResult, error) {
	if validateRef(source) != nil {
		return MutationResult{}, ErrInvalid
	}
	_, revision, e := s.Revision(ctx, source)
	if e != nil {
		return MutationResult{}, e
	}
	exact := Ref{source.ProfileID, source.Revision, revision.SpecDigest, revision.AssignmentsDigest}
	return s.create(c, name, revision.Spec, assignments, &exact, "clone")
}
func (s *MemoryStore) Revise(_ context.Context, c Command, ref Ref, spec Spec, assignments []Assignment) (MutationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if validateCommand(c) != nil || validateRef(ref) != nil {
		return MutationResult{}, ErrInvalid
	}
	d, e := commandDigest("revise", ref.ProfileID, "", ref.Revision, spec, assignments, nil)
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
	clean, a, sd, ad, _ := canonical(spec, assignments)
	p.CurrentRevision++
	r := Revision{ProfileID: p.ID, Revision: p.CurrentRevision, Spec: clean, SpecDigest: sd, AssignmentsDigest: ad, CreatedBy: c.ActorID, Assignments: a, CreatedAt: c.Now.UTC()}
	s.profiles[p.ID] = p
	s.revisions[p.ID][r.Revision] = r
	return s.save(c, "revise", d, p, r), nil
}
func (s *MemoryStore) Deactivate(_ context.Context, c Command, ref Ref) (MutationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if validateCommand(c) != nil || validateRef(ref) != nil {
		return MutationResult{}, ErrInvalid
	}
	p, ok := s.profiles[ref.ProfileID]
	if !ok {
		return MutationResult{}, ErrNotFound
	}
	r := s.revisions[p.ID][p.CurrentRevision]
	d := digest([]byte(fmt.Sprintf("%s\x00deactivate\x00%s\x00%d", Contract, ref.ProfileID, ref.Revision)))
	if out, ok, e := s.replay(c, d); ok {
		return out, e
	}
	if p.Lifecycle != Active {
		return MutationResult{}, ErrInactive
	}
	if p.CurrentRevision != ref.Revision {
		return MutationResult{}, ErrConflict
	}
	if len(s.references[p.ID]) > 0 {
		return MutationResult{}, ErrReferenced
	}
	now := c.Now.UTC()
	p.Lifecycle = Deactivated
	p.DeactivatedBy = c.ActorID
	p.DeactivatedAt = &now
	s.profiles[p.ID] = p
	return s.save(c, "deactivate", d, p, r), nil
}
func (s *MemoryStore) Current(ctx context.Context, profileID string) (Profile, Revision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.profiles[profileID]
	if !ok {
		return Profile{}, Revision{}, ErrNotFound
	}
	return cloneEntry(p, s.revisions[p.ID][p.CurrentRevision])
}
func (s *MemoryStore) Revision(_ context.Context, ref Ref) (Profile, Revision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.profiles[ref.ProfileID]
	if !ok {
		return Profile{}, Revision{}, ErrNotFound
	}
	r, ok := s.revisions[p.ID][ref.Revision]
	if !ok {
		return Profile{}, Revision{}, ErrNotFound
	}
	return cloneEntry(p, r)
}
func cloneEntry(p Profile, r Revision) (Profile, Revision, error) {
	r.Spec = cloneSpec(r.Spec)
	r.Assignments = append([]Assignment(nil), r.Assignments...)
	return p, r, nil
}
func (s *MemoryStore) Catalog(_ context.Context, limit int) ([]Entry, error) {
	if limit < 1 || limit > 500 {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := []Entry{}
	for _, p := range s.profiles {
		_, r, _ := cloneEntry(p, s.revisions[p.ID][p.CurrentRevision])
		entries = append(entries, Entry{p, r})
	}
	sortEntries(entries)
	if len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, nil
}
func (s *MemoryStore) Assigned(_ context.Context, target Target, limit int) ([]Entry, error) {
	if validateTarget(target) != nil || limit < 1 || limit > 500 {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := []Entry{}
	for _, p := range s.profiles {
		if p.Lifecycle != Active {
			continue
		}
		r := s.revisions[p.ID][p.CurrentRevision]
		for _, a := range r.Assignments {
			if assigned(a, target) {
				_, copy, _ := cloneEntry(p, r)
				entries = append(entries, Entry{p, copy})
				break
			}
		}
	}
	sortEntries(entries)
	if len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, nil
}
func sortEntries(entries []Entry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Profile.Name == entries[j].Profile.Name {
			return entries[i].Profile.ID < entries[j].Profile.ID
		}
		return entries[i].Profile.Name < entries[j].Profile.Name
	})
}
func (s *MemoryStore) References(_ context.Context, profileID string, limit int) ([]Reference, error) {
	if !uuidRE.MatchString(profileID) || limit < 1 || limit > 500 {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	refs := append([]Reference(nil), s.references[profileID]...)
	if len(refs) > limit {
		refs = refs[:limit]
	}
	return refs, nil
}
func (s *MemoryStore) ReplaceReferences(applicationID, environmentID, gitPath string, refs []Reference) error {
	if !uuidRE.MatchString(applicationID) || !uuidRE.MatchString(environmentID) || gitPath == "" || len(refs) > 32 {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for profileID, items := range s.references {
		out := items[:0]
		for _, item := range items {
			if item.GitPath != gitPath {
				out = append(out, item)
			}
		}
		s.references[profileID] = out
	}
	seen := map[string]struct{}{}
	for _, ref := range refs {
		if !uuidRE.MatchString(ref.ProfileID) || ref.Revision < 1 || ref.ApplicationID != applicationID || ref.EnvironmentID != environmentID || ref.GitPath != gitPath || !dnsLabelRE.MatchString(ref.LogicalName) {
			return ErrInvalid
		}
		key := ref.ProfileID + "\x00" + ref.LogicalName
		if _, ok := seen[key]; ok {
			return ErrInvalid
		}
		seen[key] = struct{}{}
		s.references[ref.ProfileID] = append(s.references[ref.ProfileID], ref)
	}
	return nil
}
func (s *MemoryStore) AuditEvents() []AuditEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]AuditEvent(nil), s.audits...)
}
