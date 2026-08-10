package helmapps

import (
	"context"
	"sort"
	"sync"
	"time"
)

// MemoryStore mirrors PostgreSQL semantics for unit tests and single-process
// development. It is not presented as a production capability.
type MemoryStore struct {
	mu                   sync.Mutex
	operatorConfigDigest string
	approvals            map[ApprovalKey]Approval
	approvalIdem         map[string]ApprovalKey
	approvalDocuments    map[ApprovalKey]ApprovalDocument
	commands             map[string]RenderCommand
	commandIdem          map[string]string
	results              map[string]RenderResult
	readiness            map[string]Readiness
}

func NewMemoryStore(operatorConfigDigest ...string) *MemoryStore {
	digest := digestBytes([]byte("external-helm-memory-operator-config.v1"))
	if len(operatorConfigDigest) == 1 && validDigest(operatorConfigDigest[0]) {
		digest = operatorConfigDigest[0]
	}
	return &MemoryStore{
		operatorConfigDigest: digest,
		approvals:            make(map[ApprovalKey]Approval), approvalIdem: make(map[string]ApprovalKey),
		approvalDocuments: make(map[ApprovalKey]ApprovalDocument),
		commands:          make(map[string]RenderCommand), commandIdem: make(map[string]string),
		results: make(map[string]RenderResult), readiness: make(map[string]Readiness),
	}
}

func (s *MemoryStore) AdmitApproval(_ context.Context, document ApprovalDocument) (ApprovalDocument, bool, error) {
	if document.Validate() != nil {
		return ApprovalDocument{}, false, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	idem := document.Approval.CreatedBy + "\x00" + document.Approval.IdempotencyKey
	if key, exists := s.approvalIdem[idem]; exists {
		stored, hasDocument := s.approvalDocuments[key]
		if !hasDocument || !approvalDocumentReplayEqual(stored, document) {
			return ApprovalDocument{}, false, ErrConflict
		}
		return cloneApprovalDocument(stored), true, nil
	}
	if _, exists := s.approvals[document.Approval.ApprovalKey]; exists {
		return ApprovalDocument{}, false, ErrConflict
	}
	stored := cloneApprovalDocument(document)
	s.approvals[stored.Approval.ApprovalKey] = stored.Approval
	s.approvalDocuments[stored.Approval.ApprovalKey] = stored
	s.approvalIdem[idem] = stored.Approval.ApprovalKey
	return cloneApprovalDocument(stored), false, nil
}

func (s *MemoryStore) ApprovalAdmission(_ context.Context, actorID, idempotencyKey string) (ApprovalDocument, error) {
	if !uuidRE.MatchString(actorID) || !idempotencyRE.MatchString(idempotencyKey) {
		return ApprovalDocument{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key, exists := s.approvalIdem[actorID+"\x00"+idempotencyKey]
	if !exists {
		return ApprovalDocument{}, ErrNotFound
	}
	document, exists := s.approvalDocuments[key]
	if !exists || document.Validate() != nil {
		return ApprovalDocument{}, ErrConflict
	}
	return cloneApprovalDocument(document), nil
}

func (s *MemoryStore) ApprovalAdmissionCatalog(_ context.Context, limit int) ([]ApprovalDocument, error) {
	if limit < 1 || limit > 100 {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	documents := make([]ApprovalDocument, 0, len(s.approvalDocuments))
	for _, document := range s.approvalDocuments {
		if document.Validate() != nil {
			return nil, ErrConflict
		}
		documents = append(documents, cloneApprovalDocument(document))
	}
	sort.Slice(documents, func(i, j int) bool {
		left, right := documents[i], documents[j]
		if !left.Approval.CreatedAt.Equal(right.Approval.CreatedAt) {
			return left.Approval.CreatedAt.After(right.Approval.CreatedAt)
		}
		if left.Approval.ID != right.Approval.ID {
			return left.Approval.ID < right.Approval.ID
		}
		return left.Approval.Revision > right.Approval.Revision
	})
	if len(documents) > limit {
		documents = documents[:limit]
	}
	return documents, nil
}

func (s *MemoryStore) PutApproval(_ context.Context, approval Approval) (Approval, bool, error) {
	if approval.Validate() != nil {
		return Approval{}, false, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	idem := approval.CreatedBy + "\x00" + approval.IdempotencyKey
	if key, exists := s.approvalIdem[idem]; exists {
		stored := s.approvals[key]
		if !stored.replayEqual(approval) {
			return Approval{}, false, ErrConflict
		}
		return stored, true, nil
	}
	if stored, exists := s.approvals[approval.ApprovalKey]; exists {
		if !stored.replayEqual(approval) {
			return Approval{}, false, ErrConflict
		}
		return stored, true, nil
	}
	approval = cloneApproval(approval)
	s.approvals[approval.ApprovalKey] = approval
	s.approvalIdem[idem] = approval.ApprovalKey
	return cloneApproval(approval), false, nil
}

func (s *MemoryStore) Approval(_ context.Context, key ApprovalKey) (Approval, error) {
	if key.Validate() != nil {
		return Approval{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	approval, exists := s.approvals[key]
	if !exists {
		return Approval{}, ErrNotFound
	}
	return cloneApproval(approval), nil
}

func (s *MemoryStore) Submit(_ context.Context, desired DesiredRender, now time.Time) (RenderCommand, bool, error) {
	if desired.Validate() != nil || now.IsZero() {
		return RenderCommand{}, false, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	approval, exists := s.approvals[desired.Approval]
	if !exists {
		return RenderCommand{}, false, ErrNotFound
	}
	if !desiredMatchesApproval(desired, approval) {
		return RenderCommand{}, false, ErrConflict
	}
	idem := desired.IdempotencyScope + "\x00" + desired.IdempotencyKey
	if id, replay := s.commandIdem[idem]; replay {
		stored := s.commands[id]
		if !desiredReplayEqual(stored.DesiredRender, desired) || stored.OperatorConfigDigest != s.operatorConfigDigest {
			return RenderCommand{}, false, ErrConflict
		}
		return cloneCommand(stored), true, nil
	}
	if _, duplicate := s.commands[desired.ID]; duplicate {
		return RenderCommand{}, false, ErrConflict
	}
	command := RenderCommand{DesiredRender: cloneDesired(desired), OperatorConfigDigest: s.operatorConfigDigest,
		State: StateQueued, AvailableAt: now,
		CreatedAt: now, UpdatedAt: now}
	if command.Validate() != nil {
		return RenderCommand{}, false, ErrInvalid
	}
	s.commands[command.ID] = command
	s.commandIdem[idem] = command.ID
	return cloneCommand(command), false, nil
}

func (s *MemoryStore) Command(_ context.Context, id string) (RenderCommand, error) {
	if !uuidRE.MatchString(id) {
		return RenderCommand{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	command, exists := s.commands[id]
	if !exists {
		return RenderCommand{}, ErrNotFound
	}
	return cloneCommand(command), nil
}

func (s *MemoryStore) Result(_ context.Context, commandID string) (RenderResult, error) {
	if !uuidRE.MatchString(commandID) {
		return RenderResult{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result, exists := s.results[commandID]
	if !exists {
		return RenderResult{}, ErrNotFound
	}
	return cloneResult(result), nil
}

func (s *MemoryStore) Claim(_ context.Context, owner string, runtime RenderWorkerIdentity, now time.Time, duration time.Duration) (RenderLease, error) {
	if !validLeaseRequest(owner, runtime, now, duration) || runtime.OperatorConfigDigest != s.operatorConfigDigest {
		return RenderLease{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.commands))
	for id := range s.commands {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		left, right := s.commands[ids[i]], s.commands[ids[j]]
		if !left.AvailableAt.Equal(right.AvailableAt) {
			return left.AvailableAt.Before(right.AvailableAt)
		}
		if !left.CreatedAt.Equal(right.CreatedAt) {
			return left.CreatedAt.Before(right.CreatedAt)
		}
		return left.ID < right.ID
	})
	for _, id := range ids {
		command := s.commands[id]
		if command.OperatorConfigDigest != runtime.OperatorConfigDigest {
			continue
		}
		dueQueued := command.State == StateQueued && !command.AvailableAt.After(now)
		expired := command.State == StateProcessing && command.LeaseUntil != nil && !command.LeaseUntil.After(now)
		if !dueQueued && !expired {
			continue
		}
		if expired {
			if command.ConsecutiveFailures < MaximumAttempts {
				command.ConsecutiveFailures++
			}
			command.LastFailureCode = "renderer-lease-expired"
		}
		if command.Attempts >= MaximumAttempts {
			completed := now
			command.State, command.CompletedAt, command.UpdatedAt = StateFailed, &completed, now
			command.LeaseOwner, command.LeaseUntil, command.WorkerIdentity = "", nil, RenderWorkerIdentity{}
			s.commands[id] = command
			continue
		}
		until := now.Add(duration)
		command.State, command.LeaseOwner, command.LeaseUntil = StateProcessing, owner, &until
		command.LeaseEpoch++
		command.Attempts++
		command.WorkerIdentity = runtime
		command.UpdatedAt = now
		s.commands[id] = command
		return RenderLease{Command: cloneCommand(command), Owner: owner, Epoch: command.LeaseEpoch, Until: until}, nil
	}
	return RenderLease{}, ErrNotFound
}

func (s *MemoryStore) Heartbeat(_ context.Context, lease RenderLease, now time.Time, duration time.Duration) (RenderLease, error) {
	if !validLeaseRequest(lease.Owner, lease.Command.WorkerIdentity, now, duration) || lease.Epoch <= 0 {
		return RenderLease{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	command, exists := s.commands[lease.Command.ID]
	if !exists {
		return RenderLease{}, ErrNotFound
	}
	if !leaseMatches(command, lease, now) {
		return RenderLease{}, ErrLeaseLost
	}
	until := now.Add(duration)
	command.LeaseUntil, command.UpdatedAt = &until, now
	s.commands[command.ID] = command
	return RenderLease{Command: cloneCommand(command), Owner: lease.Owner, Epoch: lease.Epoch, Until: until}, nil
}

func (s *MemoryStore) Complete(_ context.Context, lease RenderLease, manifests ValidatedManifests, now time.Time) (RenderResult, error) {
	if now.IsZero() || manifests.ResourceCount < 1 || manifests.ResourceCount > MaximumResources ||
		manifests.ManifestDigest != digestBytes(manifests.Raw) || !validDigest(manifests.InventoryDigest) {
		return RenderResult{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	command, exists := s.commands[lease.Command.ID]
	if !exists {
		return RenderResult{}, ErrNotFound
	}
	if !leaseMatches(command, lease, now) {
		return RenderResult{}, ErrLeaseLost
	}
	verified, verifyErr := ValidateRenderedManifests(manifests.Raw, command.Descriptor)
	if verifyErr != nil || !validatedReplayEqual(verified, manifests) {
		return RenderResult{}, ErrInvalid
	}
	result := RenderResult{CommandID: command.ID, InputDigest: command.InputDigest,
		ManifestDigest: manifests.ManifestDigest, InventoryDigest: manifests.InventoryDigest,
		RenderedManifests: append([]byte(nil), manifests.Raw...), ResourceCount: manifests.ResourceCount,
		OutputBytes: len(manifests.Raw), OperatorConfigDigest: command.OperatorConfigDigest,
		RendererImage: RendererImage, RendererVersion: HelmVersion,
		PolicyVersion: PolicyVersion, LimitsDigest: LimitsDigest(), CompletedAt: now}
	if result.Validate(command) != nil {
		return RenderResult{}, ErrInvalid
	}
	if _, duplicate := s.results[command.ID]; duplicate {
		return RenderResult{}, ErrConflict
	}
	completed := now
	command.State, command.CompletedAt, command.UpdatedAt = StateSucceeded, &completed, now
	command.LeaseOwner, command.LeaseUntil, command.WorkerIdentity = "", nil, RenderWorkerIdentity{}
	s.commands[command.ID], s.results[command.ID] = command, cloneResult(result)
	return cloneResult(result), nil
}

func (s *MemoryStore) Fail(_ context.Context, lease RenderLease, code string, retryable bool, now time.Time) (RenderCommand, error) {
	if now.IsZero() || !failureCodeRE.MatchString(code) {
		return RenderCommand{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	command, exists := s.commands[lease.Command.ID]
	if !exists {
		return RenderCommand{}, ErrNotFound
	}
	if !leaseMatches(command, lease, now) {
		return RenderCommand{}, ErrLeaseLost
	}
	command.ConsecutiveFailures++
	command.LastFailureCode = code
	command.LeaseOwner, command.LeaseUntil, command.WorkerIdentity = "", nil, RenderWorkerIdentity{}
	command.UpdatedAt = now
	if retryable && command.Attempts < MaximumAttempts {
		command.State = StateQueued
		command.AvailableAt = now.Add(RetryDelay(command.Attempts))
	} else {
		completed := now
		command.State, command.CompletedAt = StateFailed, &completed
	}
	if command.Validate() != nil {
		return RenderCommand{}, ErrConflict
	}
	s.commands[command.ID] = command
	return cloneCommand(command), nil
}

func (s *MemoryStore) PutReadiness(_ context.Context, readiness Readiness) error {
	if readiness.Validate() != nil || readiness.RenderWorkerIdentity != ExpectedRenderWorkerIdentity(s.operatorConfigDigest) {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, exists := s.readiness[readiness.WorkerID]; exists {
		if readiness.WorkerEpoch < current.WorkerEpoch || readiness.WorkerEpoch > current.WorkerEpoch+1 {
			return ErrConflict
		}
		if readiness.WorkerEpoch == current.WorkerEpoch && (readiness.RenderWorkerIdentity != current.RenderWorkerIdentity ||
			!readiness.StartedAt.Equal(current.StartedAt) || readiness.ObservedAt.Before(current.ObservedAt)) {
			return ErrConflict
		}
	}
	s.readiness[readiness.WorkerID] = readiness
	return nil
}

func (s *MemoryStore) RuntimeReady(_ context.Context, now time.Time) (bool, error) {
	if now.IsZero() {
		return false, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, readiness := range s.readiness {
		if readiness.RenderWorkerIdentity == ExpectedRenderWorkerIdentity(s.operatorConfigDigest) && readiness.LeaseUntil.After(now) {
			return true, nil
		}
	}
	return false, nil
}

func leaseMatches(command RenderCommand, lease RenderLease, now time.Time) bool {
	return command.State == StateProcessing && command.LeaseOwner == lease.Owner && command.LeaseEpoch == lease.Epoch &&
		command.WorkerIdentity == lease.Command.WorkerIdentity && command.LeaseUntil != nil && command.LeaseUntil.After(now)
}

func cloneApproval(value Approval) Approval { return value }

func cloneDesired(value DesiredRender) DesiredRender {
	value.DescriptorYAML = append([]byte(nil), value.DescriptorYAML...)
	value.ValuesYAML = append([]byte(nil), value.ValuesYAML...)
	return value
}

func cloneCommand(value RenderCommand) RenderCommand {
	value.DesiredRender = cloneDesired(value.DesiredRender)
	if value.LeaseUntil != nil {
		copied := *value.LeaseUntil
		value.LeaseUntil = &copied
	}
	if value.CompletedAt != nil {
		copied := *value.CompletedAt
		value.CompletedAt = &copied
	}
	return value
}

func cloneResult(value RenderResult) RenderResult {
	value.RenderedManifests = append([]byte(nil), value.RenderedManifests...)
	return value
}
