package builds

import (
	"context"
	"sort"
	"strings"
	"time"
)

func (s *MemoryStore) AcceptSourceDeployment(_ context.Context, command SourceDeploymentCommand) (SourceDeploymentAcceptance, error) {
	if command.validate() != nil {
		return SourceDeploymentAcceptance{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	idempotencyIdentity := apiMemoryKey(command.ActorID, APICommandSourceDeployment, command.DeploymentID, command.IdempotencyKey)
	if receipt, ok := s.apiIdempotency[idempotencyIdentity]; ok {
		if receipt.fingerprint != command.Fingerprint {
			return SourceDeploymentAcceptance{}, ErrConflict
		}
		attempt, attemptOK := s.attempts[receipt.resourceID]
		intent, intentOK := s.sourceDeploymentIntents[SourceDeploymentIntentID(receipt.resourceID, command.DeploymentID)]
		if !attemptOK || !intentOK {
			return SourceDeploymentAcceptance{}, ErrConflict
		}
		return SourceDeploymentAcceptance{Attempt: cloneAttempt(attempt), Intent: cloneSourceDeploymentIntent(intent), Replay: true}, nil
	}

	definition, repository, err := s.sourceDeploymentDefinitionLocked(command)
	if err != nil {
		return SourceDeploymentAcceptance{}, err
	}
	claimKey := APICommandClaimKey(command.ActorID, APICommandSourceDeployment, command.DeploymentID, command.IdempotencyKey)
	attemptID := ManualAttemptID(claimKey, definition.ID)
	if _, exists := s.attempts[attemptID]; exists {
		return SourceDeploymentAcceptance{}, ErrConflict
	}
	generationKey := serviceKey(definition.ProjectID, definition.ServiceID)
	generation := s.serviceGeneration[generationKey] + 1
	attempt, err := newAttemptWithExecution(definition, command.Execution, repository, EnqueuePush{
		ClaimKey: claimKey, CommitSHA: command.CommitSHA, GitRef: definition.TriggerRef, ResolvedAt: command.AcceptedAt.UTC(),
	}, generation, s.cacheImportsLocked(definition, generation), command.AcceptedAt)
	if err != nil || attempt.ID != attemptID {
		return SourceDeploymentAcceptance{}, ErrInvalid
	}
	attempt.DeliveryClaimKey = ""
	if command.Mode == SourceDeploymentRebuild {
		attempt.TriggerKind = "retry"
	} else {
		attempt.TriggerKind = "manual"
	}
	attempt.TriggerKey = claimKey
	if validateStoredAttempt(attempt) != nil {
		return SourceDeploymentAcceptance{}, ErrInvalid
	}

	now := command.AcceptedAt.UTC()
	sequence := s.sourceDeploymentLatest[command.DeploymentID] + 1
	for id, current := range s.sourceDeploymentIntents {
		if current.DeploymentID != command.DeploymentID || current.State != SourceDeploymentPending && current.State != SourceDeploymentProcessing {
			continue
		}
		completed := now
		current.State, current.FailureCode, current.CompletedAt = SourceDeploymentSuperseded, "newer-source-deploy", &completed
		current.LeaseOwner, current.LeaseUntil, current.UpdatedAt = "", time.Time{}, now
		s.sourceDeploymentIntents[id] = current
	}
	intent := SourceDeploymentIntent{ID: SourceDeploymentIntentID(attempt.ID, command.DeploymentID), AttemptID: attempt.ID,
		ActorID: command.ActorID, ProjectID: command.ProjectID, ApplicationID: command.ApplicationID,
		EnvironmentID: command.EnvironmentID, DeploymentID: command.DeploymentID, Mode: command.Mode,
		SourceAttemptID: command.SourceAttemptID, Sequence: sequence, DefinitionID: definition.ID,
		DefinitionDigest: definition.DefinitionDigest, SourceDeploymentGeneration: command.SourceDeploymentGeneration,
		SourceConfigETag: command.SourceConfigETag, ConfigIntent: append([]byte{}, command.ConfigIntent...),
		TemplateDigest: command.TemplateDigest, RequestID: command.RequestID, State: SourceDeploymentPending,
		AvailableAt: now, CreatedAt: now, UpdatedAt: now, StartDraft: command.StartDraft}
	if intent.validate() != nil {
		return SourceDeploymentAcceptance{}, ErrInvalid
	}
	s.serviceGeneration[generationKey] = generation
	s.attempts[attempt.ID] = cloneAttempt(attempt)
	s.sourceDeploymentIntents[intent.ID] = cloneSourceDeploymentIntent(intent)
	s.sourceDeploymentLatest[command.DeploymentID] = sequence
	s.apiIdempotency[idempotencyIdentity] = memoryAPIIdempotency{fingerprint: command.Fingerprint, resourceID: attempt.ID}
	return SourceDeploymentAcceptance{Attempt: attempt, Intent: intent}, nil
}

func (s *MemoryStore) sourceDeploymentDefinitionLocked(command SourceDeploymentCommand) (BuildDefinition, Repository, error) {
	definition, ok := s.definitions[command.DefinitionID]
	if command.Mode == SourceDeploymentRebuild {
		source, sourceOK := s.attempts[command.SourceAttemptID]
		if !sourceOK {
			return BuildDefinition{}, Repository{}, ErrNotFound
		}
		if source.State != AttemptSucceeded || source.ProjectID != command.ProjectID || source.ServiceID != command.ApplicationID ||
			source.DefinitionID != command.DefinitionID || source.DefinitionDigest != command.ExpectedDefinitionDigest ||
			source.CommitSHA != command.CommitSHA {
			return BuildDefinition{}, Repository{}, ErrConflict
		}
		definition, ok = source.SourceSnapshot, true
	}
	if !ok {
		return BuildDefinition{}, Repository{}, ErrNotFound
	}
	if !definition.Enabled || definition.validate() != nil || definition.ProjectID != command.ProjectID || definition.ServiceID != command.ApplicationID ||
		definition.DefinitionDigest != command.ExpectedDefinitionDigest {
		return BuildDefinition{}, Repository{}, ErrUnauthorized
	}
	var repository Repository
	switch definition.SourceKind {
	case SourceGitHub:
		installation, installationOK := s.installations[definition.InstallationID]
		var repositoryOK bool
		repository, repositoryOK = s.repositories[definition.RepositoryID]
		if !installationOK || !repositoryOK || installation.Lifecycle != InstallationActive || repository.Lifecycle != RepositoryActive ||
			repository.InstallationID != installation.ID || repository.Identity.OwnerID != installation.Account.ID ||
			!strings.EqualFold(repository.Identity.OwnerLogin, installation.Account.Login) {
			return BuildDefinition{}, Repository{}, ErrUnauthorized
		}
	case SourceGitSSH:
		if definition.GitSSH == nil {
			return BuildDefinition{}, Repository{}, ErrUnauthorized
		}
	default:
		return BuildDefinition{}, Repository{}, ErrUnauthorized
	}
	return definition, repository, nil
}

func (s *MemoryStore) ClaimNextSourceDeployment(_ context.Context, owner string, now time.Time, duration time.Duration) (SourceDeploymentWork, error) {
	if !validOwnerLease(owner, duration) || now.IsZero() {
		return SourceDeploymentWork{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current := now.UTC()
	ids := make([]string, 0, len(s.sourceDeploymentIntents))
	for id, intent := range s.sourceDeploymentIntents {
		if intent.Sequence != s.sourceDeploymentLatest[intent.DeploymentID] && (intent.State == SourceDeploymentPending || intent.State == SourceDeploymentProcessing) {
			completed := current
			intent.State, intent.FailureCode, intent.CompletedAt = SourceDeploymentSuperseded, "newer-source-deploy", &completed
			intent.LeaseOwner, intent.LeaseUntil, intent.UpdatedAt = "", time.Time{}, current
			s.sourceDeploymentIntents[id] = intent
			continue
		}
		if intent.State != SourceDeploymentPending && (intent.State != SourceDeploymentProcessing || intent.LeaseUntil.After(current)) || intent.AvailableAt.After(current) {
			continue
		}
		attempt, ok := s.attempts[intent.AttemptID]
		projection := s.releaseProjections[intent.AttemptID]
		if !ok || attempt.State == AttemptFailed || attempt.State == AttemptCancelled || projection.state == ReleaseProjectionFailed || intent.Attempts >= 20 {
			completed := current
			intent.State, intent.FailureCode, intent.CompletedAt = SourceDeploymentFailed, "source-build-failed", &completed
			intent.LeaseOwner, intent.LeaseUntil, intent.UpdatedAt = "", time.Time{}, current
			s.sourceDeploymentIntents[id] = intent
			continue
		}
		if attempt.State != AttemptSucceeded || projection.state != ReleaseProjectionSucceeded {
			continue
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		left, right := s.sourceDeploymentIntents[ids[i]], s.sourceDeploymentIntents[ids[j]]
		if left.AvailableAt.Equal(right.AvailableAt) {
			return left.ID < right.ID
		}
		return left.AvailableAt.Before(right.AvailableAt)
	})
	if len(ids) == 0 {
		return SourceDeploymentWork{}, ErrNotFound
	}
	intent := s.sourceDeploymentIntents[ids[0]]
	intent.State, intent.LeaseOwner, intent.LeaseUntil = SourceDeploymentProcessing, owner, current.Add(duration)
	intent.LeaseEpoch++
	intent.Attempts++
	intent.FailureCode, intent.UpdatedAt = "", current
	s.sourceDeploymentIntents[intent.ID] = intent
	lease := SourceDeploymentLeaseToken{IntentID: intent.ID, Owner: owner, Epoch: intent.LeaseEpoch, Until: intent.LeaseUntil}
	return SourceDeploymentWork{Intent: cloneSourceDeploymentIntent(intent), Lease: lease}, nil
}

func (s *MemoryStore) HeartbeatSourceDeployment(_ context.Context, lease SourceDeploymentLeaseToken, now time.Time, duration time.Duration) (SourceDeploymentLeaseToken, error) {
	if !validSourceDeploymentLease(lease) || !validOwnerLease(lease.Owner, duration) || now.IsZero() {
		return SourceDeploymentLeaseToken{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	intent, ok := s.sourceDeploymentIntents[lease.IntentID]
	if !ok || !sourceDeploymentLeaseMatches(intent, lease, now) || intent.Sequence != s.sourceDeploymentLatest[intent.DeploymentID] {
		return SourceDeploymentLeaseToken{}, ErrLeaseLost
	}
	intent.LeaseUntil, intent.UpdatedAt = now.UTC().Add(duration), now.UTC()
	s.sourceDeploymentIntents[intent.ID] = intent
	lease.Until = intent.LeaseUntil
	return lease, nil
}

func (s *MemoryStore) RetrySourceDeployment(_ context.Context, lease SourceDeploymentLeaseToken, code string, now, availableAt time.Time) error {
	if !validSourceDeploymentLease(lease) || validateFailureCode(code) != nil || now.IsZero() || availableAt.Before(now.UTC()) {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	intent, ok := s.sourceDeploymentIntents[lease.IntentID]
	if !ok || !sourceDeploymentLeaseMatches(intent, lease, now) {
		return ErrLeaseLost
	}
	intent.LeaseOwner, intent.LeaseUntil, intent.FailureCode, intent.UpdatedAt = "", time.Time{}, code, now.UTC()
	if intent.Sequence != s.sourceDeploymentLatest[intent.DeploymentID] {
		completed := now.UTC()
		intent.State, intent.FailureCode, intent.CompletedAt = SourceDeploymentSuperseded, "newer-source-deploy", &completed
	} else if intent.Attempts >= 20 {
		completed := now.UTC()
		intent.State, intent.CompletedAt = SourceDeploymentFailed, &completed
	} else {
		intent.State, intent.AvailableAt = SourceDeploymentPending, availableAt.UTC()
	}
	s.sourceDeploymentIntents[intent.ID] = intent
	return nil
}

func (s *MemoryStore) FailSourceDeployment(_ context.Context, lease SourceDeploymentLeaseToken, code string, now time.Time) error {
	if !validSourceDeploymentLease(lease) || validateFailureCode(code) != nil || now.IsZero() {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	intent, ok := s.sourceDeploymentIntents[lease.IntentID]
	if !ok || !sourceDeploymentLeaseMatches(intent, lease, now) {
		return ErrLeaseLost
	}
	completed := now.UTC()
	intent.State, intent.FailureCode, intent.CompletedAt = SourceDeploymentFailed, code, &completed
	intent.LeaseOwner, intent.LeaseUntil, intent.UpdatedAt = "", time.Time{}, completed
	s.sourceDeploymentIntents[intent.ID] = intent
	return nil
}

func (s *MemoryStore) CompleteSourceDeployment(_ context.Context, lease SourceDeploymentLeaseToken, receipt SourceDeploymentSubmissionReceipt, now time.Time) error {
	if !validSourceDeploymentLease(lease) || !uuidRE.MatchString(receipt.OperationID) || !uuidRE.MatchString(receipt.DeploymentID) || now.IsZero() {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	intent, ok := s.sourceDeploymentIntents[lease.IntentID]
	if !ok || !sourceDeploymentLeaseMatches(intent, lease, now) || intent.Sequence != s.sourceDeploymentLatest[intent.DeploymentID] || receipt.DeploymentID != intent.DeploymentID {
		return ErrLeaseLost
	}
	completed := now.UTC()
	intent.State, intent.OperationID, intent.CompletedAt = SourceDeploymentSubmitted, receipt.OperationID, &completed
	intent.LeaseOwner, intent.LeaseUntil, intent.FailureCode, intent.UpdatedAt = "", time.Time{}, "", completed
	s.sourceDeploymentIntents[intent.ID] = intent
	return nil
}

func validSourceDeploymentLease(lease SourceDeploymentLeaseToken) bool {
	return uuidRE.MatchString(lease.IntentID) && lease.Owner != "" && len(lease.Owner) <= 128 && lease.Epoch > 0 && !lease.Until.IsZero()
}

func sourceDeploymentLeaseMatches(intent SourceDeploymentIntent, lease SourceDeploymentLeaseToken, now time.Time) bool {
	return intent.State == SourceDeploymentProcessing && intent.LeaseOwner == lease.Owner && intent.LeaseEpoch == lease.Epoch &&
		intent.LeaseUntil.Equal(lease.Until) && intent.LeaseUntil.After(now.UTC())
}

func cloneSourceDeploymentIntent(intent SourceDeploymentIntent) SourceDeploymentIntent {
	intent.ConfigIntent = append([]byte(nil), intent.ConfigIntent...)
	if intent.CompletedAt != nil {
		completed := *intent.CompletedAt
		intent.CompletedAt = &completed
	}
	return intent
}

var _ SourceDeploymentStore = (*MemoryStore)(nil)
