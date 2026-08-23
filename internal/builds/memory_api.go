package builds

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"time"

	"github.com/kuberploy/kuberploy/internal/githubapp"
)

func (s *MemoryStore) Installation(_ context.Context, installationID string) (Installation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	installation, ok := s.installations[installationID]
	if !ok {
		return Installation{}, ErrNotFound
	}
	return cloneInstallation(installation), nil
}

func (s *MemoryStore) Repository(_ context.Context, repositoryID string) (Repository, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	repository, ok := s.repositories[repositoryID]
	if !ok {
		return Repository{}, ErrNotFound
	}
	return cloneRepository(repository), nil
}

func (s *MemoryStore) Definition(_ context.Context, definitionID string) (BuildDefinition, error) {
	if !uuidRE.MatchString(definitionID) {
		return BuildDefinition{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	definition, ok := s.definitions[definitionID]
	if !ok {
		return BuildDefinition{}, ErrNotFound
	}
	return cloneDefinition(definition), nil
}

func (s *MemoryStore) ListRepositories(_ context.Context, installationID string) ([]Repository, error) {
	if !uuidRE.MatchString(installationID) {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.installations[installationID]; !ok {
		return nil, ErrNotFound
	}
	result := make([]Repository, 0)
	for _, repository := range s.repositories {
		if repository.InstallationID == installationID {
			result = append(result, cloneRepository(repository))
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Identity.ID < result[j].Identity.ID })
	return result, nil
}

func (s *MemoryStore) DefinitionsForService(_ context.Context, serviceID string) ([]BuildDefinition, error) {
	if !uuidRE.MatchString(serviceID) {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]BuildDefinition, 0)
	for _, definition := range s.definitions {
		if definition.ServiceID == serviceID {
			result = append(result, cloneDefinition(definition))
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].UpdatedAt.Equal(result[j].UpdatedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].UpdatedAt.After(result[j].UpdatedAt)
	})
	return result, nil
}

func (s *MemoryStore) AttemptsForService(_ context.Context, serviceID string, limit int) ([]BuildAttempt, error) {
	if !uuidRE.MatchString(serviceID) || limit < 1 || limit > 100 {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]BuildAttempt, 0)
	for _, attempt := range s.attempts {
		if attempt.ServiceID == serviceID {
			result = append(result, cloneAttempt(attempt))
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].ID > result[j].ID
		}
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func apiMemoryKey(actorID, operation, scopeID, key string) string {
	return actorID + "\x00" + operation + "\x00" + scopeID + "\x00" + key
}

func (s *MemoryStore) ClaimAPICommand(_ context.Context, actorID, operation, scopeID, key, fingerprint, resourceID string, now time.Time) (string, bool, error) {
	if !validAPICommand(operation, actorID, scopeID, key, fingerprint, resourceID, now) {
		return "", false, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	idemKey := apiMemoryKey(actorID, operation, scopeID, key)
	if existing, ok := s.apiIdempotency[idemKey]; ok {
		if existing.fingerprint != fingerprint {
			return "", false, ErrConflict
		}
		return existing.resourceID, true, nil
	}
	s.apiIdempotency[idemKey] = memoryAPIIdempotency{fingerprint: fingerprint, resourceID: resourceID}
	return resourceID, false, nil
}

func (s *MemoryStore) RetryAttempt(_ context.Context, sourceAttemptID, retryAttemptID, claimKey string, execution ExecutionSettings, now time.Time) (BuildAttempt, bool, error) {
	if !uuidRE.MatchString(sourceAttemptID) || !uuidRE.MatchString(retryAttemptID) || !regexpHex64(claimKey) || now.IsZero() {
		return BuildAttempt{}, false, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.attempts[retryAttemptID]; ok {
		if existing.TriggerKey != claimKey {
			return BuildAttempt{}, false, ErrConflict
		}
		return cloneAttempt(existing), true, nil
	}
	source, ok := s.attempts[sourceAttemptID]
	if !ok {
		return BuildAttempt{}, false, ErrNotFound
	}
	if source.State != AttemptFailed && source.State != AttemptCancelled {
		return BuildAttempt{}, false, ErrConflict
	}
	definition, ok := s.definitions[source.DefinitionID]
	if !ok || !definition.Enabled || definition.DefinitionDigest != source.DefinitionDigest {
		return BuildAttempt{}, false, ErrUnauthorized
	}
	var installation Installation
	var repository Repository
	if definition.SourceKind == SourceGitHub {
		var installationOK, repositoryOK bool
		installation, installationOK = s.installations[definition.InstallationID]
		repository, repositoryOK = s.repositories[definition.RepositoryID]
		if !installationOK || !repositoryOK || installation.Lifecycle != InstallationActive || repository.Lifecycle != RepositoryActive || repository.InstallationID != installation.ID {
			return BuildAttempt{}, false, ErrUnauthorized
		}
	} else if definition.GitSSH == nil {
		return BuildAttempt{}, false, ErrUnauthorized
	}
	generationKey := serviceKey(definition.ProjectID, definition.ServiceID)
	generation := s.serviceGeneration[generationKey] + 1
	imports := s.cacheImportsLocked(definition, generation)
	attempt, err := newAttemptWithExecution(definition, execution, repository, EnqueuePush{ClaimKey: claimKey, CommitSHA: source.CommitSHA, GitRef: source.GitRef, ResolvedAt: now.UTC()}, generation, imports, now)
	if err != nil {
		return BuildAttempt{}, false, err
	}
	if attempt.ID != retryAttemptID {
		return BuildAttempt{}, false, ErrInvalid
	}
	if definition.SourceKind == SourceGitHub {
		bodyDigest := sha256.Sum256([]byte("kuberploy-manual-build-retry-v1\x00" + sourceAttemptID))
		deliveryID := deterministicUUID("build-retry-delivery-v1", claimKey)
		completed := now.UTC()
		claim := githubapp.OneTimeClaim{Kind: "github-delivery", ClaimKey: claimKey, RetainUntil: now.UTC().Add(24 * time.Hour), Permanent: true}
		s.claims[claimMapKey(claim.Kind, claim.ClaimKey)] = memoryClaim{claim: claim, createdAt: now.UTC()}
		s.deliveries[claimKey] = DeliveryReceipt{ClaimKey: claimKey, AppID: installation.AppID, GitHubInstallationID: installation.GitHubInstallationID,
			DeliveryID: deliveryID, Event: "push", BodySHA256: "sha256:" + hex.EncodeToString(bodyDigest[:]), RepositoryID: repository.Identity.ID,
			GitRef: source.GitRef, State: DeliveryEnqueued, AvailableAt: now.UTC(), ReceivedAt: now.UTC(), CompletedAt: &completed, UpdatedAt: now.UTC()}
	} else {
		attempt.DeliveryClaimKey = ""
		attempt.TriggerKind, attempt.TriggerKey = "retry", claimKey
		if err = validateStoredAttempt(attempt); err != nil {
			return BuildAttempt{}, false, err
		}
	}
	s.serviceGeneration[generationKey] = generation
	s.attempts[attempt.ID] = cloneAttempt(attempt)
	return attempt, false, nil
}

func (s *MemoryStore) EnqueueManualAttempt(_ context.Context, definitionID, commitSHA, claimKey string, execution ExecutionSettings, now time.Time) (BuildAttempt, bool, error) {
	if !uuidRE.MatchString(definitionID) || !commitRE.MatchString(commitSHA) || !regexpHex64(claimKey) || now.IsZero() {
		return BuildAttempt{}, false, ErrInvalid
	}
	attemptID := ManualAttemptID(claimKey, definitionID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.attempts[attemptID]; ok {
		if existing.TriggerKind != "manual" || existing.TriggerKey != claimKey || existing.DefinitionID != definitionID || existing.CommitSHA != commitSHA {
			return BuildAttempt{}, false, ErrConflict
		}
		return cloneAttempt(existing), true, nil
	}
	definition, ok := s.definitions[definitionID]
	if !ok {
		return BuildAttempt{}, false, ErrNotFound
	}
	if definition.SourceKind != SourceGitSSH || definition.GitSSH == nil || !definition.Enabled {
		return BuildAttempt{}, false, ErrUnauthorized
	}
	generationKey := serviceKey(definition.ProjectID, definition.ServiceID)
	generation := s.serviceGeneration[generationKey] + 1
	imports := s.cacheImportsLocked(definition, generation)
	attempt, err := newAttemptWithExecution(definition, execution, Repository{}, EnqueuePush{
		ClaimKey: claimKey, CommitSHA: commitSHA, GitRef: definition.TriggerRef, ResolvedAt: now.UTC(),
	}, generation, imports, now)
	if err != nil || attempt.ID != attemptID {
		return BuildAttempt{}, false, ErrInvalid
	}
	attempt.DeliveryClaimKey = ""
	attempt.TriggerKind, attempt.TriggerKey = "manual", claimKey
	if err = validateStoredAttempt(attempt); err != nil {
		return BuildAttempt{}, false, err
	}
	s.serviceGeneration[generationKey] = generation
	s.attempts[attempt.ID] = cloneAttempt(attempt)
	return attempt, false, nil
}
