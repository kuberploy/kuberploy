package gitprojection

import (
	"context"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"
)

// MemoryStore is a concurrency-safe contract implementation for tests and the
// local in-memory profile. It deliberately retains old generations so tests can
// prove that readers never observe a partially built shadow generation.
type MemoryStore struct {
	mu             sync.Mutex
	bindings       map[string]Binding
	observations   map[string][]VerifiedHead
	webhooks       map[string]WebhookTombstone
	webhookEvents  map[string]string
	polls          map[string]PollCursor
	reconciliation map[string]memoryReconciliation
	generations    map[string]map[int64]Generation
	documents      map[string]map[int64]map[string]Document
	reservations   map[string]PathReservation
	runtimeLeases  map[string]RuntimeLease
	writeCommands  map[string]WriteCommand
}

var _ Store = (*MemoryStore)(nil)
var _ BindingCatalog = (*MemoryStore)(nil)

type memoryReconciliation struct {
	lease        *ReconciliationLease
	epoch        int64
	reconciledAt time.Time
	lastError    string
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		bindings: map[string]Binding{}, observations: map[string][]VerifiedHead{}, webhooks: map[string]WebhookTombstone{}, webhookEvents: map[string]string{}, polls: map[string]PollCursor{},
		reconciliation: map[string]memoryReconciliation{}, generations: map[string]map[int64]Generation{}, documents: map[string]map[int64]map[string]Document{}, reservations: map[string]PathReservation{}, runtimeLeases: map[string]RuntimeLease{}, writeCommands: map[string]WriteCommand{},
	}
}

func (s *MemoryStore) PutBinding(_ context.Context, binding Binding) error {
	if err := binding.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, exists := s.bindings[binding.ID]; exists {
		if immutableBinding(current) != immutableBinding(binding) {
			return ErrConflict
		}
		return nil
	}
	for _, current := range s.bindings {
		if current.Kind == binding.Kind && current.ScopeID == binding.ScopeID {
			return ErrConflict
		}
	}
	s.bindings[binding.ID] = cloneBinding(binding)
	return nil
}

type bindingIdentity struct {
	Kind                              BindingKind
	ScopeID, ProjectID, EnvironmentID string
	Repository                        RepositoryIdentity
	TargetRef, Prefix                 string
	CredentialMode                    CredentialMode
	CredentialSecretName              string
}

func immutableBinding(b Binding) bindingIdentity {
	return bindingIdentity{b.Kind, b.ScopeID, b.ProjectID, b.EnvironmentID, b.Repository, b.TargetRef, b.Prefix, b.CredentialMode, b.CredentialSecretName}
}

func (s *MemoryStore) Binding(_ context.Context, id string) (Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	binding, exists := s.bindings[id]
	if !exists {
		return Binding{}, ErrNotFound
	}
	return cloneBinding(binding), nil
}

func (s *MemoryStore) BindingsForScope(_ context.Context, kind BindingKind, scopeID string) ([]Binding, error) {
	if (kind != BindingEnvironment && kind != BindingPlatform) || !uuidRE.MatchString(scopeID) {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	values := make([]Binding, 0, 1)
	for _, binding := range s.bindings {
		if binding.Kind == kind && binding.ScopeID == scopeID {
			values = append(values, cloneBinding(binding))
		}
	}
	sort.Slice(values, func(left, right int) bool { return values[left].ID < values[right].ID })
	return values, nil
}

func (s *MemoryStore) SetBindingState(_ context.Context, bindingID, expectedHead string, state BindingState, now time.Time) error {
	if now.IsZero() || expectedHead != "" && !commitRE.MatchString(expectedHead) || state != BindingDiverged && state != BindingMissingRef && state != BindingWaiting && state != BindingIndexing {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	binding, exists := s.bindings[bindingID]
	if !exists {
		return ErrNotFound
	}
	if binding.TargetHeadRevision != expectedHead {
		return ErrConflict
	}
	if now.Before(binding.UpdatedAt) {
		return ErrConflict
	}
	binding.State, binding.UpdatedAt = state, now.UTC()
	s.bindings[bindingID] = binding
	return nil
}

func (s *MemoryStore) RecordVerifiedHead(_ context.Context, head VerifiedHead) (Binding, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	binding, exists := s.bindings[head.BindingID]
	if !exists {
		return Binding{}, false, ErrNotFound
	}
	if err := head.ValidateFor(binding); err != nil {
		return Binding{}, false, err
	}
	if head.ObservedAt.Before(binding.CreatedAt) || !binding.TargetHeadObservedAt.IsZero() && (head.ObservedAt.Before(binding.TargetHeadObservedAt) || head.ObservedAt.Equal(binding.TargetHeadObservedAt) && head.Commit != binding.TargetHeadRevision) {
		return Binding{}, false, ErrConflict
	}
	replay := false
	for _, old := range s.observations[binding.ID] {
		if old.Commit == head.Commit && old.Source == head.Source && old.ProviderRequest == head.ProviderRequest {
			replay = true
			break
		}
	}
	if !replay {
		s.observations[binding.ID] = append(s.observations[binding.ID], head)
	}
	binding.TargetHeadRevision = head.Commit
	binding.TargetHeadObservedAt = head.ObservedAt.UTC()
	// BindingIndexing can also mean metadata policy changed while the Git head
	// stayed constant. Preserve that durable revalidation request across the
	// provider observation instead of incorrectly turning it back into ready.
	if binding.IndexedRevision == head.Commit && binding.State != BindingIndexing {
		binding.State = BindingReady
	} else if binding.State != BindingDiverged {
		binding.State = BindingIndexing
	}
	updatedAt := head.ObservedAt.UTC()
	if !updatedAt.After(binding.UpdatedAt) {
		updatedAt = binding.UpdatedAt.Add(time.Microsecond)
	}
	binding.UpdatedAt = updatedAt
	s.bindings[binding.ID] = binding
	return cloneBinding(binding), replay, nil
}

func webhookKey(value WebhookTombstone) string {
	return value.Provider + "\x00" + value.DeliveryHash
}

func webhookEventKey(value WebhookTombstone) string {
	return value.Provider + "\x00" + strconv.FormatInt(value.RepositoryID, 10) + "\x00" + value.TargetRef + "\x00" + value.AfterCommit
}

func (s *MemoryStore) ClaimWebhook(_ context.Context, value WebhookTombstone) (bool, error) {
	if err := value.Validate(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := webhookKey(value)
	if _, exists := s.webhooks[key]; exists {
		return false, nil
	}
	eventKey := webhookEventKey(value)
	if _, exists := s.webhookEvents[eventKey]; exists {
		return false, nil
	}
	s.webhooks[key] = value
	s.webhookEvents[eventKey] = key
	return true, nil
}

func (s *MemoryStore) PollCursor(_ context.Context, bindingID string) (PollCursor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, exists := s.polls[bindingID]
	if !exists {
		return PollCursor{}, ErrNotFound
	}
	return value, nil
}

func (s *MemoryStore) PutPollCursor(_ context.Context, value PollCursor) error {
	if err := value.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.bindings[value.BindingID]; !exists {
		return ErrNotFound
	}
	if current, exists := s.polls[value.BindingID]; exists {
		state := s.reconciliation[value.BindingID]
		if state.lease != nil && state.lease.Until.After(value.UpdatedAt) {
			return ErrLeaseHeld
		}
		if value.UpdatedAt.Before(current.UpdatedAt) || value.UpdatedAt.Equal(current.UpdatedAt) && value != current {
			return ErrConflict
		}
	}
	s.polls[value.BindingID] = value
	state := s.reconciliation[value.BindingID]
	state.reconciledAt = s.bindings[value.BindingID].UpdatedAt
	state.lastError = ""
	s.reconciliation[value.BindingID] = state
	return nil
}

func (s *MemoryStore) ClaimReconciliation(_ context.Context, owner string, now time.Time, duration time.Duration) (ReconciliationWork, error) {
	if !ownerRE.MatchString(owner) || now.IsZero() || !validReconciliationLeaseDuration(duration) {
		return ReconciliationWork{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.bindings))
	for id := range s.bindings {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		binding := s.bindings[id]
		if binding.CredentialMode != CredentialGitHubApp {
			continue
		}
		cursor, hasCursor := s.polls[id]
		if binding.UpdatedAt.After(now) || hasCursor && cursor.UpdatedAt.After(now) {
			continue
		}
		state := s.reconciliation[id]
		bindingChanged := state.reconciledAt.IsZero() || !state.reconciledAt.Equal(binding.UpdatedAt)
		active := state.lease != nil && state.lease.Until.After(now)
		if active {
			continue
		}
		reclaimed := state.lease != nil
		if !reclaimed && hasCursor && cursor.NextPollAt.After(now) && state.reconciledAt.Equal(binding.UpdatedAt) {
			continue
		}
		if !hasCursor {
			cursor = PollCursor{BindingID: id, NextPollAt: now.UTC(), UpdatedAt: now.UTC()}
		}
		state.epoch++
		lease := ReconciliationLease{BindingID: id, Owner: owner, Epoch: state.epoch, Until: now.UTC().Add(duration)}
		state.lease = &lease
		cursor.UpdatedAt = now.UTC()
		s.polls[id] = cursor
		s.reconciliation[id] = state
		return ReconciliationWork{Binding: cloneBinding(binding), Lease: lease, ConsecutiveFailure: cursor.ConsecutiveFail, Reclaimed: reclaimed, BindingChanged: bindingChanged}, nil
	}
	return ReconciliationWork{}, ErrNotFound
}

func (s *MemoryStore) HeartbeatReconciliation(_ context.Context, lease ReconciliationLease, now time.Time, duration time.Duration) (ReconciliationLease, error) {
	if lease.Validate() != nil || now.IsZero() || !validReconciliationLeaseDuration(duration) {
		return ReconciliationLease{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.activeReconciliationLocked(lease, now) {
		return ReconciliationLease{}, ErrLeaseLost
	}
	state := s.reconciliation[lease.BindingID]
	if !now.UTC().Add(duration).After(state.lease.Until) {
		return ReconciliationLease{}, ErrLeaseLost
	}
	if s.polls[lease.BindingID].UpdatedAt.After(now) {
		return ReconciliationLease{}, ErrLeaseLost
	}
	lease.Until = now.UTC().Add(duration)
	state.lease = &lease
	s.reconciliation[lease.BindingID] = state
	cursor := s.polls[lease.BindingID]
	cursor.UpdatedAt = now.UTC()
	s.polls[lease.BindingID] = cursor
	return lease, nil
}

func (s *MemoryStore) FinishReconciliation(_ context.Context, lease ReconciliationLease, outcome ReconciliationOutcome, now time.Time) error {
	if lease.Validate() != nil || outcome.Validate() != nil || now.IsZero() || !outcome.NextPollAt.After(now) {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.activeReconciliationLocked(lease, now) {
		return ErrLeaseLost
	}
	binding, exists := s.bindings[lease.BindingID]
	if !exists {
		return ErrNotFound
	}
	if binding.UpdatedAt.After(now) || s.polls[lease.BindingID].UpdatedAt.After(now) {
		return ErrConflict
	}
	if outcome.ConsecutiveFailure == 0 && (binding.State != BindingReady || binding.TargetHeadRevision != outcome.LastCommit || binding.IndexedRevision != outcome.LastCommit) ||
		outcome.ConsecutiveFailure > 0 && outcome.LastCommit != "" && binding.TargetHeadRevision != outcome.LastCommit {
		return ErrConflict
	}
	cursor := s.polls[lease.BindingID]
	cursor.LastCommit, cursor.ConsecutiveFail = outcome.LastCommit, outcome.ConsecutiveFailure
	cursor.NextPollAt, cursor.UpdatedAt = outcome.NextPollAt.UTC(), now.UTC()
	s.polls[lease.BindingID] = cursor
	state := s.reconciliation[lease.BindingID]
	state.lease, state.reconciledAt, state.lastError = nil, binding.UpdatedAt, outcome.FailureCode
	s.reconciliation[lease.BindingID] = state
	return nil
}

func (s *MemoryStore) ReleaseReconciliation(_ context.Context, lease ReconciliationLease, now time.Time) error {
	if lease.Validate() != nil || now.IsZero() {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.activeReconciliationLocked(lease, now) {
		return ErrLeaseLost
	}
	cursor := s.polls[lease.BindingID]
	cursor.NextPollAt, cursor.UpdatedAt = now.UTC(), now.UTC()
	s.polls[lease.BindingID] = cursor
	state := s.reconciliation[lease.BindingID]
	state.lease = nil
	s.reconciliation[lease.BindingID] = state
	return nil
}

func (s *MemoryStore) activeReconciliationLocked(lease ReconciliationLease, now time.Time) bool {
	state, exists := s.reconciliation[lease.BindingID]
	return exists && state.lease != nil && state.lease.Owner == lease.Owner && state.lease.Epoch == lease.Epoch && state.lease.Until.After(now)
}

func (s *MemoryStore) BeginGeneration(_ context.Context, lease ReconciliationLease, expectedHead, parser string, now time.Time) (Generation, error) {
	if lease.Validate() != nil || now.IsZero() || !commitRE.MatchString(expectedHead) || parser == "" || len(parser) > 64 {
		return Generation{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.activeReconciliationLocked(lease, now) {
		return Generation{}, ErrLeaseLost
	}
	bindingID := lease.BindingID
	binding, exists := s.bindings[bindingID]
	if !exists {
		return Generation{}, ErrNotFound
	}
	if binding.State == BindingDiverged {
		return Generation{}, ErrDiverged
	}
	if binding.State == BindingMissingRef {
		return Generation{}, ErrMissingRef
	}
	if binding.TargetHeadRevision != expectedHead || binding.ParserVersion != parser {
		return Generation{}, ErrConflict
	}
	if now.Before(binding.UpdatedAt) {
		return Generation{}, ErrConflict
	}
	number := binding.ProjectionGeneration + 1
	if byNumber := s.generations[bindingID]; byNumber != nil {
		for existing, old := range byNumber {
			if old.State == ProjectionStaging {
				old.State = ProjectionFailed
				byNumber[existing] = old
				delete(s.documents[bindingID], existing)
			}
			if existing >= number {
				number = existing + 1
			}
		}
	} else {
		s.generations[bindingID] = map[int64]Generation{}
	}
	generation := Generation{BindingID: bindingID, Number: number, HeadRevision: expectedHead, ParserVersion: parser, State: ProjectionStaging, StartedAt: now.UTC()}
	s.generations[bindingID][number] = generation
	if s.documents[bindingID] == nil {
		s.documents[bindingID] = map[int64]map[string]Document{}
	}
	s.documents[bindingID][number] = map[string]Document{}
	return generation, nil
}

func (s *MemoryStore) PutDocuments(_ context.Context, generation Generation, documents []Document) error {
	if generation.State != ProjectionStaging || len(documents) > 1000 {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	binding, exists := s.bindings[generation.BindingID]
	if !exists {
		return ErrNotFound
	}
	current, exists := s.generations[generation.BindingID][generation.Number]
	if !exists || current != generation || current.State != ProjectionStaging {
		return ErrConflict
	}
	staged := s.documents[generation.BindingID][generation.Number]
	accepted := make([]Document, 0, len(documents))
	pending := map[string]Document{}
	for _, document := range documents {
		if err := document.Validate(binding); err != nil || document.Generation != generation.Number || document.SourceRevision != generation.HeadRevision || document.IndexedAt.Before(generation.StartedAt) {
			return ErrInvalid
		}
		if existing, duplicate := staged[document.Path]; duplicate && (existing.BlobID != document.BlobID || existing.ContentSHA256 != document.ContentSHA256) {
			return ErrConflict
		}
		if existing, duplicate := pending[document.Path]; duplicate && (existing.BlobID != document.BlobID || existing.ContentSHA256 != document.ContentSHA256) {
			return ErrConflict
		}
		pending[document.Path] = document
		accepted = append(accepted, cloneDocument(document))
	}
	for _, document := range accepted {
		staged[document.Path] = document
	}
	return nil
}

func (s *MemoryStore) ActivateGeneration(ctx context.Context, lease ReconciliationLease, generation Generation, policy AppConfigPolicyValidator, now time.Time) (Binding, error) {
	if lease.Validate() != nil || now.IsZero() || generation.State != ProjectionStaging || lease.BindingID != generation.BindingID || policy == nil {
		return Binding{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.activeReconciliationLocked(lease, now) {
		return Binding{}, ErrLeaseLost
	}
	binding, exists := s.bindings[generation.BindingID]
	if !exists {
		return Binding{}, ErrNotFound
	}
	current, exists := s.generations[generation.BindingID][generation.Number]
	if !exists || current != generation || binding.TargetHeadRevision != generation.HeadRevision || binding.ParserVersion != generation.ParserVersion || now.Before(generation.StartedAt) || now.Before(binding.UpdatedAt) {
		return Binding{}, ErrConflict
	}
	currentDocuments := policyDocuments(s.documents[generation.BindingID][generation.Number])
	previousDocuments := []Document{}
	if binding.ProjectionGeneration > 0 {
		previousDocuments = policyDocuments(s.documents[generation.BindingID][binding.ProjectionGeneration])
	}
	input := AppConfigPolicyInput{Binding: cloneBinding(binding), Generation: generation, Current: currentDocuments, Previous: previousDocuments}
	validation := AppConfigPolicyValidation{Diagnostics: map[string][]Diagnostic{}}
	var err error
	if binding.Kind == BindingPlatform {
		if len(currentDocuments) != 0 || len(previousDocuments) != 0 {
			return Binding{}, ErrConflict
		}
	} else if validation, err = policy.ValidateAppConfigs(ctx, input); err != nil {
		return Binding{}, err
	}
	if validation.ValidateFor(input) != nil {
		return Binding{}, ErrInvalid
	}
	validatedDocuments := []Document{}
	if binding.Kind != BindingPlatform {
		validatedDocuments, err = applyPolicyValidation(binding, currentDocuments, validation)
		if err != nil {
			return Binding{}, err
		}
		validatedDocuments, err = applyEffectiveConfigRevisions(validatedDocuments, previousDocuments, generation.HeadRevision)
		if err != nil {
			return Binding{}, err
		}
	}
	for _, document := range validatedDocuments {
		s.documents[generation.BindingID][generation.Number][document.Path] = document
	}
	activated := now.UTC()
	current.State, current.ActivatedAt = ProjectionActive, &activated
	s.generations[generation.BindingID][generation.Number] = current
	binding.IndexedRevision = generation.HeadRevision
	binding.ProjectionGeneration = generation.Number
	binding.IndexedAt, binding.UpdatedAt = activated, activated
	binding.State = BindingReady
	s.bindings[binding.ID] = binding
	// Activation is convergence through the exact current provider head, not
	// merely equality with one operation commit. A provider may expose a later
	// fast-forward before an earlier successful writer has finalized its DB
	// receipt. Every committed reservation that was visible before this
	// binding-locked activation is therefore covered by this generation.
	for key, reservation := range s.reservations {
		if reservation.BindingID != binding.ID || reservation.State != ReservationCommittedPendingIndex {
			continue
		}
		if command, exists := s.writeCommands[reservation.OperationID]; exists && command.Plan.BindingID == binding.ID &&
			command.TargetRef == reservation.TargetRef && command.Path == reservation.Path && command.State == WriteCommandGitCommitted {
			indexedAt := now.UTC()
			command.State, command.IndexedGeneration, command.IndexedAt, command.UpdatedAt = WriteCommandIndexed, generation.Number, &indexedAt, indexedAt
			s.writeCommands[reservation.OperationID] = command
		}
		delete(s.reservations, key)
	}
	// Retain the exact-commit receipt path for migration adapters and isolated
	// stores that persist a committed command without a path reservation.
	for operationID, command := range s.writeCommands {
		if command.Plan.BindingID == binding.ID && command.State == WriteCommandGitCommitted && command.CommittedRevision == generation.HeadRevision {
			indexedAt := now.UTC()
			command.State, command.IndexedGeneration, command.IndexedAt, command.UpdatedAt = WriteCommandIndexed, generation.Number, &indexedAt, indexedAt
			s.writeCommands[operationID] = command
		}
	}
	return cloneBinding(binding), nil
}

func (s *MemoryStore) FailGeneration(_ context.Context, lease ReconciliationLease, generation Generation, now time.Time) error {
	if lease.Validate() != nil || now.IsZero() || lease.BindingID != generation.BindingID || generation.State != ProjectionStaging {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.activeReconciliationLocked(lease, now) {
		return ErrLeaseLost
	}
	current, exists := s.generations[generation.BindingID][generation.Number]
	if !exists || current != generation || current.State != ProjectionStaging {
		return ErrConflict
	}
	current.State = ProjectionFailed
	s.generations[generation.BindingID][generation.Number] = current
	delete(s.documents[generation.BindingID], generation.Number)
	return nil
}

func (s *MemoryStore) Document(_ context.Context, bindingID, documentPath string) (Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	binding, exists := s.bindings[bindingID]
	if !exists || binding.ProjectionGeneration == 0 {
		return Document{}, ErrNotFound
	}
	document, exists := s.documents[bindingID][binding.ProjectionGeneration][documentPath]
	if !exists {
		return Document{}, ErrNotFound
	}
	return cloneDocument(document), nil
}

func (s *MemoryStore) Bundle(_ context.Context, bindingID, documentPath string, dependencies []string, chartDigest, policyVersion string) (Bundle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	binding, exists := s.bindings[bindingID]
	if !exists || binding.ProjectionGeneration == 0 {
		return Bundle{}, ErrNotFound
	}
	active := s.documents[bindingID][binding.ProjectionGeneration]
	document, exists := active[documentPath]
	if !exists {
		return Bundle{}, ErrNotFound
	}
	dependencyDocuments := make([]Document, 0, len(dependencies))
	dependencyStates := make([]DependencyState, 0, len(dependencies))
	for _, dependencyPath := range dependencies {
		dependency, ok := active[dependencyPath]
		if !ok {
			dependencyStates = append(dependencyStates, DependencyState{Path: dependencyPath})
			continue
		}
		dependencyDocuments = append(dependencyDocuments, dependency)
		dependencyStates = append(dependencyStates, DependencyState{Path: dependencyPath, Present: true, BlobID: dependency.BlobID})
	}
	var etag string
	var err error
	if len(dependencies) == 0 {
		etag, err = StrongETag(binding, []Document{document}, nil, chartDigest, policyVersion)
	} else {
		etag, err = StrongETagWithDependencies(binding, []Document{document}, dependencies, dependencyDocuments, chartDigest, policyVersion)
	}
	if err != nil {
		return Bundle{}, err
	}
	all := append([]Document{cloneDocument(document)}, cloneDocuments(dependencyDocuments)...)
	return Bundle{BindingID: binding.ID, TargetRef: binding.TargetRef, TargetHeadRevision: binding.TargetHeadRevision, IndexedRevision: binding.IndexedRevision,
		ConfigRevision: document.ConfigRevision, ETag: etag, Stale: binding.TargetHeadRevision == "" || binding.TargetHeadRevision != binding.IndexedRevision,
		Documents: all, Dependencies: dependencyStates, IndexedAt: binding.IndexedAt}, nil
}

func reservationKey(bindingID, targetRef, documentPath string) string {
	return bindingID + "\x00" + targetRef + "\x00" + documentPath
}

func (s *MemoryStore) PathReservation(_ context.Context, bindingID, targetRef, documentPath string) (PathReservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	binding, exists := s.bindings[bindingID]
	if !exists {
		return PathReservation{}, ErrNotFound
	}
	reservation, exists := s.reservations[reservationKey(bindingID, targetRef, documentPath)]
	if !exists {
		return PathReservation{}, ErrNotFound
	}
	if reservation.Validate(binding) != nil {
		return PathReservation{}, ErrInvalid
	}
	return reservation, nil
}

func (s *MemoryStore) AcquirePath(_ context.Context, candidate PathReservation, now time.Time, lease time.Duration) (PathReservation, bool, error) {
	if now.IsZero() || lease <= 0 || lease > 2*time.Minute || candidate.State != ReservationCandidate || candidate.LeaseUntil == nil {
		return PathReservation{}, false, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	binding, exists := s.bindings[candidate.BindingID]
	if !exists {
		return PathReservation{}, false, ErrNotFound
	}
	if err := candidate.Validate(binding); err != nil || !candidate.CreatedAt.Equal(now) || !candidate.UpdatedAt.Equal(now) || !candidate.LeaseUntil.Equal(now.Add(lease)) {
		return PathReservation{}, false, ErrInvalid
	}
	if binding.State != BindingReady || binding.TargetHeadRevision == "" || binding.TargetHeadRevision != binding.IndexedRevision || binding.TargetHeadRevision != candidate.BaseRevision {
		return PathReservation{}, false, ErrStale
	}
	key := reservationKey(candidate.BindingID, candidate.TargetRef, candidate.Path)
	if current, occupied := s.reservations[key]; occupied {
		if current.OperationID == candidate.OperationID && current.Owner == candidate.Owner && current.BaseRevision == candidate.BaseRevision {
			return current, true, nil
		}
		if current.OperationID == candidate.OperationID && current.BaseRevision == candidate.BaseRevision &&
			current.State == ReservationCandidate && current.LeaseUntil != nil && !current.LeaseUntil.After(now) {
			current.Owner = candidate.Owner
			current.LeaseUntil = candidate.LeaseUntil
			current.UpdatedAt = now.UTC()
			s.reservations[key] = current
			return current, true, nil
		}
		// Expiry alone never permits stealing. A repair worker must verify that
		// the previous operation commit is absent from authoritative history.
		return PathReservation{}, false, ErrLeaseHeld
	}
	s.reservations[key] = candidate
	return candidate, false, nil
}

func (s *MemoryStore) FinalizePath(_ context.Context, bindingID, targetRef, documentPath, operationID, committedRevision string, now time.Time) (PathReservation, error) {
	if now.IsZero() || !commitRE.MatchString(committedRevision) {
		return PathReservation{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	binding, exists := s.bindings[bindingID]
	if !exists {
		return PathReservation{}, ErrNotFound
	}
	key := reservationKey(bindingID, targetRef, documentPath)
	reservation, exists := s.reservations[key]
	if !exists {
		return PathReservation{}, ErrNotFound
	}
	if reservation.OperationID != operationID {
		return PathReservation{}, ErrLeaseLost
	}
	if now.Before(reservation.CreatedAt) {
		return PathReservation{}, ErrInvalid
	}
	if reservation.State == ReservationCommittedPendingIndex {
		if reservation.CommittedRevision == committedRevision {
			return reservation, nil
		}
		return PathReservation{}, ErrConflict
	}
	if reservation.LeaseUntil == nil || !reservation.LeaseUntil.After(now) || binding.TargetHeadRevision != reservation.BaseRevision {
		// If an independently verified write observation already advanced to
		// exactly our commit, finalization remains safely replayable.
		if binding.TargetHeadRevision != committedRevision {
			return PathReservation{}, ErrLeaseLost
		}
	}
	reservation.State, reservation.CommittedRevision, reservation.LeaseUntil = ReservationCommittedPendingIndex, committedRevision, nil
	reservation.UpdatedAt = now.UTC()
	s.reservations[key] = reservation
	return reservation, nil
}

func (s *MemoryStore) FinalizeVerifiedPath(_ context.Context, bindingID, targetRef, documentPath, operationID, committedRevision string, head VerifiedHead, now time.Time) (PathReservation, error) {
	if now.IsZero() || !commitRE.MatchString(committedRevision) || head.Source != ObservationWrite || head.ObservedAt.After(now) {
		return PathReservation{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	binding, exists := s.bindings[bindingID]
	if !exists {
		return PathReservation{}, ErrNotFound
	}
	if head.BindingID != bindingID || head.TargetRef != targetRef || head.ValidateFor(binding) != nil || head.ObservedAt.Before(binding.CreatedAt) ||
		!binding.TargetHeadObservedAt.IsZero() && (head.ObservedAt.Before(binding.TargetHeadObservedAt) || head.ObservedAt.Equal(binding.TargetHeadObservedAt) && head.Commit != binding.TargetHeadRevision) {
		return PathReservation{}, ErrConflict
	}
	key := reservationKey(bindingID, targetRef, documentPath)
	reservation, exists := s.reservations[key]
	if !exists {
		return PathReservation{}, ErrNotFound
	}
	if reservation.OperationID != operationID {
		return PathReservation{}, ErrLeaseLost
	}
	if reservation.State == ReservationCommittedPendingIndex && reservation.CommittedRevision != committedRevision {
		return PathReservation{}, ErrConflict
	}
	command, exists := s.writeCommands[operationID]
	if !exists || validatePersistedWriteCommand(command, binding) != nil || command.Plan.BindingID != bindingID || command.Path != documentPath || command.TargetRef != targetRef {
		return PathReservation{}, ErrNotFound
	}
	if command.State != WriteCommandPending && command.CommittedRevision != committedRevision {
		return PathReservation{}, ErrConflict
	}
	replay := false
	for _, old := range s.observations[binding.ID] {
		if old.Commit == head.Commit && old.Source == head.Source && old.ProviderRequest == head.ProviderRequest {
			replay = true
			break
		}
	}
	if !replay {
		s.observations[binding.ID] = append(s.observations[binding.ID], head)
	}
	alreadyIndexed := binding.IndexedRevision == head.Commit && binding.ProjectionGeneration > 0
	updatedAt := head.ObservedAt.UTC()
	if !updatedAt.After(binding.UpdatedAt) {
		updatedAt = binding.UpdatedAt.Add(time.Microsecond)
	}
	binding.TargetHeadRevision, binding.TargetHeadObservedAt, binding.UpdatedAt = head.Commit, head.ObservedAt.UTC(), updatedAt
	if alreadyIndexed && binding.State != BindingIndexing {
		binding.State = BindingReady
	} else if binding.State != BindingDiverged {
		binding.State = BindingIndexing
	}
	s.bindings[binding.ID] = binding
	reservation.State, reservation.CommittedRevision, reservation.LeaseUntil, reservation.UpdatedAt = ReservationCommittedPendingIndex, committedRevision, nil, now.UTC()
	if alreadyIndexed {
		delete(s.reservations, key)
		indexedAt := now.UTC()
		if command.State == WriteCommandPending {
			committedAt := now.UTC()
			command.State, command.CommittedRevision, command.CommittedAt = WriteCommandIndexed, committedRevision, &committedAt
		} else if command.State == WriteCommandGitCommitted {
			command.State = WriteCommandIndexed
		}
		command.IndexedGeneration, command.IndexedAt, command.UpdatedAt = binding.ProjectionGeneration, &indexedAt, indexedAt
		s.writeCommands[operationID] = command
	} else if command.State == WriteCommandPending {
		committedAt := now.UTC()
		command.State, command.CommittedRevision, command.CommittedAt, command.UpdatedAt = WriteCommandGitCommitted, committedRevision, &committedAt, committedAt
		s.writeCommands[operationID] = command
		s.reservations[key] = reservation
	} else {
		s.reservations[key] = reservation
	}
	return reservation, nil
}

func (s *MemoryStore) RepairExpiredPath(_ context.Context, bindingID, targetRef, documentPath string, commitPresent bool, committedRevision string, now time.Time) error {
	if now.IsZero() || commitPresent && !commitRE.MatchString(committedRevision) {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := reservationKey(bindingID, targetRef, documentPath)
	reservation, exists := s.reservations[key]
	if !exists {
		return ErrNotFound
	}
	if reservation.State != ReservationCandidate || reservation.LeaseUntil == nil || reservation.LeaseUntil.After(now) {
		return ErrConflict
	}
	if commitPresent {
		reservation.State, reservation.CommittedRevision, reservation.LeaseUntil = ReservationCommittedPendingIndex, committedRevision, nil
		reservation.UpdatedAt = now.UTC()
		s.reservations[key] = reservation
		return nil
	}
	delete(s.reservations, key)
	return nil
}

func cloneBinding(value Binding) Binding { return value }

func cloneDocument(value Document) Document {
	value.Raw = append([]byte(nil), value.Raw...)
	value.Diagnostics = slices.Clone(value.Diagnostics)
	value.Parsed = cloneMap(value.Parsed)
	return value
}

func cloneDocuments(values []Document) []Document {
	result := make([]Document, len(values))
	for index, value := range values {
		result[index] = cloneDocument(value)
	}
	return result
}
