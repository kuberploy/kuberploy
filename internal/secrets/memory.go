package secrets

import (
	"context"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

type MemoryStore struct {
	mu          sync.Mutex
	bindings    map[string]Binding
	versions    map[string]Version
	byBinding   map[string][]string
	idempotency map[string]Idempotency
	references  map[string]Reference
	events      map[string]Event
	runtime     map[string]memoryRuntimeReconciliation
	readiness   map[string]RuntimeReadinessLease
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		bindings: map[string]Binding{}, versions: map[string]Version{}, byBinding: map[string][]string{},
		idempotency: map[string]Idempotency{}, references: map[string]Reference{}, events: map[string]Event{},
		runtime: map[string]memoryRuntimeReconciliation{}, readiness: map[string]RuntimeReadinessLease{},
	}
}

func idempotencyMapKey(value Idempotency) string {
	return value.ActorID + "\x00" + value.Operation + "\x00" + value.ApplicationID + "\x00" + value.Key
}

func referenceMapKey(bindingID string, kind ReferenceKind, reference string) string {
	return bindingID + "\x00" + string(kind) + "\x00" + reference
}

func (s *MemoryStore) BeginCreate(_ context.Context, command BeginCreate) (Binding, Version, bool, error) {
	if err := validateBeginCreate(command); err != nil {
		return Binding{}, Version{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if binding, version, replay, err := s.replay(command.Idempotency); replay || err != nil {
		return binding, version, replay, err
	}
	for _, existing := range s.bindings {
		if existing.Scope.ProjectID == command.Binding.Scope.ProjectID && existing.Scope.EnvironmentID == command.Binding.Scope.EnvironmentID &&
			existing.Scope.ApplicationID == command.Binding.Scope.ApplicationID && existing.Name == command.Binding.Name {
			return Binding{}, Version{}, false, ErrConflict
		}
	}
	s.bindings[command.Binding.ID] = command.Binding
	s.versions[command.Version.ID] = cloneVersion(command.Version)
	s.byBinding[command.Binding.ID] = []string{command.Version.ID}
	s.idempotency[idempotencyMapKey(command.Idempotency)] = command.Idempotency
	s.events[command.Event.ID] = command.Event
	return command.Binding, cloneVersion(command.Version), false, nil
}

func (s *MemoryStore) BeginRotation(_ context.Context, command BeginRotation) (Binding, Version, bool, error) {
	if !uuidRE.MatchString(command.BindingID) || command.ExpectedActiveVersion <= 0 || command.Version.Number != 0 ||
		command.Idempotency.validate() != nil || command.Event.Validate() != nil || command.Event.Kind != EventVersionStaging ||
		command.Version.BindingID != command.BindingID || command.Idempotency.BindingID != command.BindingID ||
		command.Idempotency.VersionID != command.Version.ID || command.Event.BindingID != command.BindingID || command.Event.VersionID != command.Version.ID ||
		!sameFingerprint(command.Idempotency.RequestFingerprint, command.Version.RequestFingerprint) {
		return Binding{}, Version{}, false, ErrInvalid
	}
	draft := cloneVersion(command.Version)
	draft.Number = 1
	if draft.Validate() != nil {
		return Binding{}, Version{}, false, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if binding, version, replay, err := s.replay(command.Idempotency); replay || err != nil {
		return binding, version, replay, err
	}
	binding, ok := s.bindings[command.BindingID]
	if !ok {
		return Binding{}, Version{}, false, ErrNotFound
	}
	if binding.State != BindingReady || binding.ActiveVersion != command.ExpectedActiveVersion || binding.Provider != command.Version.Provider ||
		binding.Scope.ApplicationID != command.Idempotency.ApplicationID || command.Version.CreatedAt.Before(binding.UpdatedAt) {
		return Binding{}, Version{}, false, ErrConflict
	}
	if !validPurposeTarget(binding.Purpose, binding.Provider, command.Version.TargetSecretType) {
		return Binding{}, Version{}, false, ErrConflict
	}
	activeType := TargetSecretType("")
	for _, versionID := range s.byBinding[binding.ID] {
		existing := s.versions[versionID]
		state := existing.State
		if state == VersionActive {
			activeType = existing.TargetSecretType
		}
		if state == VersionStaging || state == VersionAwaitingReadiness {
			return Binding{}, Version{}, false, ErrConflict
		}
	}
	if activeType != command.Version.TargetSecretType {
		return Binding{}, Version{}, false, ErrConflict
	}
	var maxVersion int64
	for _, versionID := range s.byBinding[binding.ID] {
		maxVersion = max(maxVersion, s.versions[versionID].Number)
	}
	command.Version.Number = maxVersion + 1
	if command.Version.Validate() != nil {
		return Binding{}, Version{}, false, ErrInvalid
	}
	s.versions[command.Version.ID] = cloneVersion(command.Version)
	s.byBinding[binding.ID] = append(s.byBinding[binding.ID], command.Version.ID)
	s.idempotency[idempotencyMapKey(command.Idempotency)] = command.Idempotency
	s.events[command.Event.ID] = command.Event
	return binding, cloneVersion(command.Version), false, nil
}

func (s *MemoryStore) CompleteStage(_ context.Context, versionID string, artifact Artifact, event Event, now time.Time) (Version, error) {
	if !uuidRE.MatchString(versionID) || now.IsZero() || event.Validate() != nil || event.Kind != EventVersionAwaitingReadiness || event.VersionID != versionID {
		return Version{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	version, ok := s.versions[versionID]
	if !ok {
		return Version{}, ErrNotFound
	}
	binding := s.bindings[version.BindingID]
	if event.BindingID != binding.ID {
		return Version{}, ErrInvalid
	}
	if now.Before(version.UpdatedAt) {
		return Version{}, ErrInvalid
	}
	if version.State == VersionAwaitingReadiness && version.Artifact != nil && *version.Artifact == artifact {
		return cloneVersion(version), nil
	}
	if version.State != VersionStaging || artifact.ValidateFor(binding, version.Number) != nil || event.BindingID != binding.ID {
		return Version{}, ErrConflict
	}
	copyArtifact := artifact
	version.Artifact, version.State, version.StagedAt, version.UpdatedAt = &copyArtifact, VersionAwaitingReadiness, now.UTC(), now.UTC()
	s.versions[versionID] = version
	if version.Provider == ProviderSealedSecrets {
		s.runtime[versionID] = memoryRuntimeReconciliation{VersionID: versionID, BindingID: binding.ID,
			NextAt: now.UTC(), CreatedAt: now.UTC(), UpdatedAt: now.UTC()}
	}
	s.events[event.ID] = event
	return cloneVersion(version), nil
}

func (s *MemoryStore) FailVersion(_ context.Context, versionID, code string, event Event, now time.Time) (Version, error) {
	if !uuidRE.MatchString(versionID) || !safeCodeRE.MatchString(code) || now.IsZero() || event.Validate() != nil || event.Kind != EventVersionFailed || event.VersionID != versionID {
		return Version{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	version, ok := s.versions[versionID]
	if !ok {
		return Version{}, ErrNotFound
	}
	binding := s.bindings[version.BindingID]
	if event.BindingID != binding.ID {
		return Version{}, ErrInvalid
	}
	if now.Before(version.UpdatedAt) || now.Before(binding.UpdatedAt) {
		return Version{}, ErrInvalid
	}
	if version.State == VersionFailed && version.FailureCode == code {
		return cloneVersion(version), nil
	}
	if version.State != VersionStaging && version.State != VersionAwaitingReadiness {
		return Version{}, ErrConflict
	}
	version.State, version.FailureCode, version.ReadinessObservedAt, version.UpdatedAt = VersionFailed, code, now.UTC(), now.UTC()
	s.versions[versionID] = version
	if runtime, exists := s.runtime[versionID]; exists {
		runtime.State, runtime.CompletedAt, runtime.UpdatedAt = "failed", now.UTC(), now.UTC()
		runtime.Lease = RuntimeLease{}
		s.runtime[versionID] = runtime
	}
	if binding.State == BindingProvisioning {
		binding.State, binding.UpdatedAt = BindingFailed, now.UTC()
		s.bindings[binding.ID] = binding
	}
	s.events[event.ID] = event
	return cloneVersion(version), nil
}

func (s *MemoryStore) ActivateVersion(_ context.Context, versionID string, now time.Time, event Event) (Binding, Version, error) {
	if !uuidRE.MatchString(versionID) || now.IsZero() || event.Validate() != nil || event.Kind != EventVersionActive || event.VersionID != versionID {
		return Binding{}, Version{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	version, ok := s.versions[versionID]
	if !ok {
		return Binding{}, Version{}, ErrNotFound
	}
	binding := s.bindings[version.BindingID]
	if event.BindingID != binding.ID {
		return Binding{}, Version{}, ErrInvalid
	}
	if now.Before(version.UpdatedAt) || now.Before(binding.UpdatedAt) {
		return Binding{}, Version{}, ErrInvalid
	}
	if version.State == VersionActive && binding.ActiveVersion == version.Number {
		return binding, cloneVersion(version), nil
	}
	if version.State != VersionAwaitingReadiness || (binding.State != BindingProvisioning && binding.State != BindingReady) || event.BindingID != binding.ID {
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
	s.versions[versionID], s.bindings[binding.ID], s.events[event.ID] = version, binding, event
	if runtime, exists := s.runtime[versionID]; exists {
		runtime.State, runtime.CompletedAt, runtime.UpdatedAt = "ready", now.UTC(), now.UTC()
		runtime.Lease = RuntimeLease{}
		s.runtime[versionID] = runtime
	}
	return binding, cloneVersion(version), nil
}

func (s *MemoryStore) Binding(_ context.Context, id string) (Binding, error) {
	if !uuidRE.MatchString(id) {
		return Binding{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	binding, ok := s.bindings[id]
	if !ok {
		return Binding{}, ErrNotFound
	}
	return binding, nil
}

func (s *MemoryStore) ListBindings(_ context.Context, applicationID, environmentID string) ([]Binding, error) {
	if !uuidRE.MatchString(applicationID) || environmentID != "" && !uuidRE.MatchString(environmentID) {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]Binding, 0)
	for _, binding := range s.bindings {
		if binding.Scope.ApplicationID == applicationID && (environmentID == "" || binding.Scope.EnvironmentID == environmentID) {
			result = append(result, binding)
		}
	}
	slices.SortFunc(result, func(a, b Binding) int {
		if compared := a.CreatedAt.Compare(b.CreatedAt); compared != 0 {
			return compared
		}
		return strings.Compare(a.ID, b.ID)
	})
	return result, nil
}

func (s *MemoryStore) Version(_ context.Context, id string) (Version, error) {
	if !uuidRE.MatchString(id) {
		return Version{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	version, ok := s.versions[id]
	if !ok {
		return Version{}, ErrNotFound
	}
	return cloneVersion(version), nil
}

func (s *MemoryStore) Versions(_ context.Context, bindingID string) ([]Version, error) {
	if !uuidRE.MatchString(bindingID) {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.bindings[bindingID]; !ok {
		return nil, ErrNotFound
	}
	result := make([]Version, 0, len(s.byBinding[bindingID]))
	for _, id := range s.byBinding[bindingID] {
		result = append(result, cloneVersion(s.versions[id]))
	}
	slices.SortFunc(result, func(a, b Version) int { return int(a.Number - b.Number) })
	return result, nil
}

func (s *MemoryStore) AddReference(_ context.Context, reference Reference, event Event) error {
	if reference.Validate() != nil || event.Validate() != nil || event.Kind != EventReferenceAdded || event.BindingID != reference.BindingID || event.VersionID != reference.VersionID {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	binding, bindingOK := s.bindings[reference.BindingID]
	version, versionOK := s.versions[reference.VersionID]
	if !bindingOK || !versionOK {
		return ErrNotFound
	}
	if binding.State != BindingReady || version.BindingID != binding.ID || (version.State != VersionActive && version.State != VersionRetained) {
		return ErrConflict
	}
	key := referenceMapKey(reference.BindingID, reference.Kind, reference.Reference)
	if current, exists := s.references[key]; exists {
		if current.BindingID == reference.BindingID && current.VersionID == reference.VersionID && current.Kind == reference.Kind &&
			current.Reference == reference.Reference && current.Revision == reference.Revision {
			return nil
		}
		return ErrConflict
	}
	s.references[key], s.events[event.ID] = reference, event
	return nil
}

func (s *MemoryStore) RemoveReference(_ context.Context, bindingID string, kind ReferenceKind, referenceID string, event Event) error {
	if !uuidRE.MatchString(bindingID) || !kind.valid() || !safeOpaque(referenceID, 256) || event.Validate() != nil || event.Kind != EventReferenceRemoved || event.BindingID != bindingID {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := referenceMapKey(bindingID, kind, referenceID)
	reference, ok := s.references[key]
	if !ok {
		return ErrNotFound
	}
	if event.VersionID != reference.VersionID {
		return ErrInvalid
	}
	delete(s.references, key)
	s.events[event.ID] = event
	return nil
}

func (s *MemoryStore) References(_ context.Context, bindingID string) ([]Reference, error) {
	if !uuidRE.MatchString(bindingID) {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.bindings[bindingID]; !ok {
		return nil, ErrNotFound
	}
	result := []Reference{}
	for _, reference := range s.references {
		if reference.BindingID == bindingID {
			result = append(result, reference)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return string(result[i].Kind)+"\x00"+result[i].Reference < string(result[j].Kind)+"\x00"+result[j].Reference
	})
	return result, nil
}

func (s *MemoryStore) PrepareDelete(_ context.Context, bindingID, actorID string, event Event, now time.Time) (Binding, []Version, error) {
	if !uuidRE.MatchString(bindingID) || !uuidRE.MatchString(actorID) || now.IsZero() || event.Validate() != nil || event.Kind != EventBindingDeleting || event.BindingID != bindingID || event.ActorID != actorID {
		return Binding{}, nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	binding, ok := s.bindings[bindingID]
	if !ok {
		return Binding{}, nil, ErrNotFound
	}
	versions := s.versionsLocked(bindingID)
	if binding.State == BindingDeleting {
		return binding, versions, nil
	}
	if binding.State == BindingDeleted {
		return binding, nil, nil
	}
	if now.Before(binding.UpdatedAt) {
		return Binding{}, nil, ErrInvalid
	}
	for _, reference := range s.references {
		if reference.BindingID == bindingID {
			return Binding{}, nil, ErrReferenced
		}
	}
	for _, version := range versions {
		if version.State == VersionStaging || version.State == VersionAwaitingReadiness {
			return Binding{}, nil, ErrConflict
		}
	}
	binding.State, binding.DeleteStarted, binding.UpdatedAt = BindingDeleting, now.UTC(), now.UTC()
	s.bindings[bindingID], s.events[event.ID] = binding, event
	return binding, versions, nil
}

func (s *MemoryStore) CompleteDelete(_ context.Context, bindingID string, event Event, now time.Time) (Binding, error) {
	if !uuidRE.MatchString(bindingID) || now.IsZero() || event.Validate() != nil || event.Kind != EventBindingDeleted || event.BindingID != bindingID {
		return Binding{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	binding, ok := s.bindings[bindingID]
	if !ok {
		return Binding{}, ErrNotFound
	}
	if binding.State == BindingDeleted {
		return binding, nil
	}
	if binding.State != BindingDeleting {
		return Binding{}, ErrConflict
	}
	if now.Before(binding.UpdatedAt) {
		return Binding{}, ErrInvalid
	}
	for _, reference := range s.references {
		if reference.BindingID == bindingID {
			return Binding{}, ErrReferenced
		}
	}
	for _, versionID := range s.byBinding[bindingID] {
		version := s.versions[versionID]
		if now.Before(version.UpdatedAt) {
			return Binding{}, ErrInvalid
		}
		version.State, version.UpdatedAt = VersionDeleted, now.UTC()
		s.versions[versionID] = version
	}
	binding.State, binding.ActiveVersion, binding.DeletedAt, binding.UpdatedAt = BindingDeleted, 0, now.UTC(), now.UTC()
	s.bindings[bindingID], s.events[event.ID] = binding, event
	return binding, nil
}

func (s *MemoryStore) PendingEvents(_ context.Context, limit int) ([]Event, error) {
	if limit < 1 || limit > 1000 {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]Event, 0, limit)
	for _, event := range s.events {
		if event.PublishedAt.IsZero() {
			result = append(result, event)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].OccurredAt.Equal(result[j].OccurredAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].OccurredAt.Before(result[j].OccurredAt)
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (s *MemoryStore) MarkEventPublished(_ context.Context, id string, at time.Time) error {
	if !uuidRE.MatchString(id) || at.IsZero() {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	event, ok := s.events[id]
	if !ok {
		return ErrNotFound
	}
	if at.Before(event.OccurredAt) {
		return ErrInvalid
	}
	if event.PublishedAt.IsZero() {
		event.PublishedAt = at.UTC()
		s.events[id] = event
	}
	return nil
}

func (s *MemoryStore) replay(input Idempotency) (Binding, Version, bool, error) {
	existing, ok := s.idempotency[idempotencyMapKey(input)]
	if !ok {
		return Binding{}, Version{}, false, nil
	}
	if !sameFingerprint(existing.RequestFingerprint, input.RequestFingerprint) {
		return Binding{}, Version{}, false, ErrConflict
	}
	binding, bindingOK := s.bindings[existing.BindingID]
	version, versionOK := s.versions[existing.VersionID]
	if !bindingOK || !versionOK {
		return Binding{}, Version{}, false, ErrConflict
	}
	return binding, cloneVersion(version), true, nil
}

func (s *MemoryStore) versionsLocked(bindingID string) []Version {
	result := make([]Version, 0, len(s.byBinding[bindingID]))
	for _, id := range s.byBinding[bindingID] {
		result = append(result, cloneVersion(s.versions[id]))
	}
	slices.SortFunc(result, func(a, b Version) int { return int(a.Number - b.Number) })
	return result
}

func validateBeginCreate(command BeginCreate) error {
	if command.Binding.Validate() != nil || command.Binding.State != BindingProvisioning || command.Binding.ActiveVersion != 0 ||
		command.Version.Validate() != nil || command.Version.Number != 1 || command.Version.BindingID != command.Binding.ID || command.Version.Provider != command.Binding.Provider ||
		!validPurposeTarget(command.Binding.Purpose, command.Binding.Provider, command.Version.TargetSecretType) ||
		command.Idempotency.validate() != nil || command.Idempotency.Operation != "create" || command.Idempotency.ApplicationID != command.Binding.Scope.ApplicationID ||
		command.Idempotency.BindingID != command.Binding.ID || command.Idempotency.VersionID != command.Version.ID ||
		!sameFingerprint(command.Idempotency.RequestFingerprint, command.Version.RequestFingerprint) ||
		command.Event.Validate() != nil || command.Event.Kind != EventVersionStaging || command.Event.BindingID != command.Binding.ID || command.Event.VersionID != command.Version.ID {
		return ErrInvalid
	}
	return nil
}

var _ Store = (*MemoryStore)(nil)

func scopeKey(scope Scope) string {
	return strings.Join([]string{scope.OrganizationID, scope.ProjectID, scope.EnvironmentID, scope.ApplicationID, scope.Namespace}, "\x00")
}
