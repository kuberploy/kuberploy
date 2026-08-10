package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/gitpublication"
	"github.com/kuberploy/kuberploy/internal/id"
	base "github.com/kuberploy/kuberploy/internal/store"
	"github.com/kuberploy/kuberploy/internal/variables"
)

func memoryVariableTarget(plan gitprojection.WritePlan) domain.AccessTarget {
	if plan.VariableScope == "project" {
		return domain.AccessTarget{Type: "project", ID: plan.ProjectID}
	}
	return domain.AccessTarget{Type: "environment", ID: plan.EnvironmentID}
}

func (s *Store) validateVariablePlanLocked(plan gitprojection.WritePlan) (gitprojection.Binding, error) {
	binding, exists := s.gitBindings[plan.EnvironmentID]
	if !exists || plan.VariableScope == "" || plan.Validate(binding) != nil {
		return gitprojection.Binding{}, base.ErrPreconditionFailed
	}
	document, present := s.gitDocuments[memoryGitDocumentKey(binding.ID, plan.VariablePath)]
	switch plan.Precondition {
	case gitprojection.MutationCreateIfAbsent:
		if present {
			return gitprojection.Binding{}, base.ErrPreconditionFailed
		}
	case gitprojection.MutationMatchETag:
		if !present || !document.Valid || `"`+document.ContentSHA256+`"` != plan.ExpectedETag {
			return gitprojection.Binding{}, base.ErrPreconditionFailed
		}
	default:
		return gitprojection.Binding{}, base.ErrPreconditionFailed
	}
	return binding, nil
}

func (s *Store) CreateVariableSetPreview(_ context.Context, actor string, plan gitprojection.WritePlan, tokenHash, candidateHash []byte, expires time.Time) error {
	if len(tokenHash) != sha256.Size || len(candidateHash) != sha256.Size || !expires.After(time.Now().UTC()) {
		return base.ErrPreviewInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorizeLocked(actor, domain.PermissionConfigWrite, memoryVariableTarget(plan)); err != nil {
		return err
	}
	if _, err := s.validateVariablePlanLocked(plan); err != nil {
		return err
	}
	key := hex.EncodeToString(tokenHash)
	if _, exists := s.variableSetPreviews[key]; exists {
		return base.ErrConflict
	}
	s.variableSetPreviews[key] = variableSetPreviewRecord{
		actorID: actor, bindingID: plan.BindingID, projectID: plan.ProjectID, environmentID: plan.EnvironmentID,
		scope: plan.VariableScope, path: plan.VariablePath, baseRevision: plan.BaseRevision, baseETag: plan.ExpectedETag, policyVersion: plan.PolicyVersion,
		candidateHash: append([]byte(nil), candidateHash...), expiresAt: expires.UTC(),
	}
	return nil
}

func (s *Store) VariableSetPreviewAuthority(_ context.Context, actor string, tokenHash []byte) (gitprojection.WritePlan, []byte, error) {
	if len(tokenHash) != sha256.Size {
		return gitprojection.WritePlan{}, nil, base.ErrPreviewInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	preview, exists := s.variableSetPreviews[hex.EncodeToString(tokenHash)]
	if !exists || preview.actorID != actor {
		return gitprojection.WritePlan{}, nil, base.ErrPreviewInvalid
	}
	precondition := gitprojection.MutationCreateIfAbsent
	if preview.baseETag != "" {
		precondition = gitprojection.MutationMatchETag
	}
	plan := gitprojection.WritePlan{BindingID: preview.bindingID, ProjectID: preview.projectID, EnvironmentID: preview.environmentID,
		BaseRevision: preview.baseRevision, Precondition: precondition, ExpectedETag: preview.baseETag, PolicyVersion: preview.policyVersion,
		VariableScope: preview.scope, VariablePath: preview.path}
	return plan, append([]byte(nil), preview.candidateHash...), nil
}

func (s *Store) SaveVariableSet(_ context.Context, actor, key, fingerprint, requestID string, plan gitprojection.WritePlan, tokenHash, candidateHash, raw []byte) (base.Result[domain.Operation], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idemKey := ik(actor, "variable-sets.save", key)
	if previous, exists := s.idempotency[idemKey]; exists {
		if err := check(previous, true, fingerprint); err != nil {
			return base.Result[domain.Operation]{}, err
		}
		operation, exists := s.operations[previous.operationID]
		if !exists {
			return base.Result[domain.Operation]{}, base.ErrConflict
		}
		return base.Result[domain.Operation]{Value: operation, Replay: true}, nil
	}
	if len(tokenHash) != sha256.Size || len(candidateHash) != sha256.Size || len(raw) == 0 {
		return base.Result[domain.Operation]{}, base.ErrPreviewInvalid
	}
	parsed, diagnostics := variables.ParseAndValidate(raw)
	if len(diagnostics) != 0 || parsed.Values == nil {
		return base.Result[domain.Operation]{}, gitprojection.ErrInvalid
	}
	digest := sha256.Sum256(raw)
	if !bytes.Equal(digest[:], candidateHash) {
		return base.Result[domain.Operation]{}, base.ErrPreviewInvalid
	}
	if err := s.authorizeLocked(actor, domain.PermissionConfigWrite, memoryVariableTarget(plan)); err != nil {
		return base.Result[domain.Operation]{}, err
	}
	binding, err := s.validateVariablePlanLocked(plan)
	if err != nil {
		return base.Result[domain.Operation]{}, err
	}
	previewKey := hex.EncodeToString(tokenHash)
	preview, exists := s.variableSetPreviews[previewKey]
	if !exists || preview.actorID != actor || preview.bindingID != plan.BindingID || preview.projectID != plan.ProjectID ||
		preview.environmentID != plan.EnvironmentID || preview.scope != plan.VariableScope || preview.path != plan.VariablePath ||
		preview.baseRevision != plan.BaseRevision || preview.baseETag != plan.ExpectedETag || preview.policyVersion != plan.PolicyVersion || !bytes.Equal(preview.candidateHash, candidateHash) {
		return base.Result[domain.Operation]{}, base.ErrPreviewInvalid
	}
	if preview.consumedAt != nil {
		return base.Result[domain.Operation]{}, base.ErrPreviewConsumed
	}
	now := time.Now().UTC()
	if !preview.expiresAt.After(now) {
		return base.Result[domain.Operation]{}, base.ErrPreviewExpired
	}
	targetID := plan.EnvironmentID
	if plan.VariableScope == "project" {
		targetID = plan.ProjectID
	}
	operation := domain.Operation{ID: id.New(), Kind: "variable-set.git-write", Status: "queued", TargetType: plan.VariableScope,
		TargetID: targetID, RequestID: requestID, Generation: 1, Progress: []domain.ProgressStep{{Name: "git-write", Status: "pending"}},
		CreatedAt: now, UpdatedAt: now}
	command, err := gitprojection.NewVariableWriteCommand(operation.ID, actor, plan, binding, raw, "variables("+plan.VariableScope+"): save VariableSet", fingerprint, now)
	if err != nil {
		return base.Result[domain.Operation]{}, err
	}
	environment, exists := s.environments[plan.EnvironmentID]
	if !exists {
		return base.Result[domain.Operation]{}, base.ErrNotFound
	}
	mode := gitpublication.ModePullRequest
	if environment.ProtectionPolicy == domain.EnvironmentDevelopment {
		mode = gitpublication.ModeDirect
	} else if environment.ProtectionPolicy != domain.EnvironmentProtected {
		return base.Result[domain.Operation]{}, base.ErrConflict
	}
	command.PublicationMode = gitprojection.PublicationMode(mode)
	if command.Validate(binding) != nil {
		return base.Result[domain.Operation]{}, gitprojection.ErrInvalid
	}
	if existing, exists := s.gitWriteCommands[operation.ID]; exists && (!reflect.DeepEqual(existing, command) || s.gitPublicationModes[operation.ID] != mode) {
		return base.Result[domain.Operation]{}, base.ErrConflict
	}
	s.gitWriteCommands[operation.ID], s.gitPublicationModes[operation.ID] = command, mode
	if mode == gitpublication.ModePullRequest {
		publication, publicationErr := gitpublication.NewPublication(operation.ID, binding.ID, gitpublication.Repository{
			InstallationID: binding.Repository.InstallationID, ID: binding.Repository.RepositoryID,
			Owner: binding.Repository.Owner, Name: binding.Repository.Name,
		}, binding.TargetRef, plan.BaseRevision, now)
		if publicationErr != nil {
			return base.Result[domain.Operation]{}, publicationErr
		}
		s.gitPublications[operation.ID] = publication
	}
	// Marshal here to keep memory parity with the PostgreSQL operation shape.
	if _, err = json.Marshal(operation.Progress); err != nil {
		return base.Result[domain.Operation]{}, err
	}
	s.operations[operation.ID] = operation
	s.outbox[operation.ID] = &outboxRecord{message: domain.WorkMessage{OperationID: operation.ID, Kind: operation.Kind, ScopeID: plan.EnvironmentID, Generation: 1, TraceID: requestID}}
	preview.consumedAt = &now
	s.variableSetPreviews[previewKey] = preview
	s.idempotency[idemKey] = idemRecord{fingerprint: fingerprint, typ: plan.VariableScope, resourceID: targetID, operationID: operation.ID}
	s.audits++
	return base.Result[domain.Operation]{Value: operation}, nil
}
