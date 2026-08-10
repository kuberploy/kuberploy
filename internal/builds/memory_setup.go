package builds

import (
	"context"
	"crypto/sha256"
	"time"

	"github.com/kuberploy/kuberploy/internal/githubapp"
)

func (s *MemoryStore) PutSetupAuthorization(_ context.Context, authorization SetupAuthorization) (SetupAuthorization, bool, error) {
	if err := validateSetupAuthorization(authorization); err != nil {
		return SetupAuthorization{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := authorization.ActorID + "\x00" + authorization.IdempotencyKey
	if existing, ok := s.setupAuthorizations[key]; ok {
		if existing.RequestFingerprint != authorization.RequestFingerprint {
			return SetupAuthorization{}, false, ErrConflict
		}
		return existing, true, nil
	}
	s.setupAuthorizations[key] = authorization
	return authorization, false, nil
}

func (s *MemoryStore) GitHubUserBinding(_ context.Context, actorID string) (githubapp.AccountIdentity, error) {
	if !uuidRE.MatchString(actorID) {
		return githubapp.AccountIdentity{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	binding, ok := s.githubUserBindings[actorID]
	if !ok {
		return githubapp.AccountIdentity{}, ErrNotFound
	}
	return binding, nil
}

func (s *MemoryStore) BindGitHubUser(_ context.Context, actorID string, identity githubapp.AccountIdentity, _ time.Time) error {
	if !uuidRE.MatchString(actorID) || identity.ID <= 0 || identity.Type != "User" || !loginRE.MatchString(identity.Login) {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if owner, exists := s.githubUserOwners[identity.ID]; exists && owner != actorID {
		return ErrUnauthorized
	}
	if current, exists := s.githubUserBindings[actorID]; exists && (current.ID != identity.ID || current.Type != identity.Type) {
		return ErrUnauthorized
	}
	s.githubUserBindings[actorID] = identity
	s.githubUserOwners[identity.ID] = actorID
	return nil
}

func (s *MemoryStore) PutSetupHandoff(_ context.Context, handoff SetupHandoff) error {
	if err := validateSetupHandoff(handoff); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.setupHandoffs[handoff.Digest]; exists {
		return ErrConflict
	}
	s.setupHandoffs[handoff.Digest] = memorySetupHandoff{record: cloneSetupHandoff(handoff)}
	return nil
}

func (s *MemoryStore) ConsumeSetupHandoff(_ context.Context, digest [sha256.Size]byte, actorID, idempotencyKey, fingerprint string, now time.Time) (ConsumedSetupHandoff, bool, error) {
	if !uuidRE.MatchString(actorID) || !setupIdempotencyRE.MatchString(idempotencyKey) || !setupFingerprintRE.MatchString(fingerprint) || now.IsZero() {
		return ConsumedSetupHandoff{}, false, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.setupHandoffs[digest]
	if !ok || stored.record.ActorID != actorID || !now.UTC().Before(stored.record.ExpiresAt) {
		return ConsumedSetupHandoff{}, false, ErrUnauthorized
	}
	if stored.consumedAt != nil {
		if stored.linkIdempotencyKey != idempotencyKey || stored.linkFingerprint != fingerprint {
			return ConsumedSetupHandoff{}, false, ErrConflict
		}
		return consumedMemoryHandoff(stored, true), true, nil
	}
	at := now.UTC()
	stored.consumedAt, stored.linkIdempotencyKey, stored.linkFingerprint = &at, idempotencyKey, fingerprint
	s.setupHandoffs[digest] = stored
	return consumedMemoryHandoff(stored, false), false, nil
}

func (s *MemoryStore) CompleteSetupHandoff(_ context.Context, digest [sha256.Size]byte, installationID string, _ time.Time) error {
	if !uuidRE.MatchString(installationID) {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.setupHandoffs[digest]
	if !ok || stored.consumedAt == nil {
		return ErrNotFound
	}
	if stored.linkedInstallation != "" && stored.linkedInstallation != installationID {
		return ErrConflict
	}
	stored.linkedInstallation = installationID
	s.setupHandoffs[digest] = stored
	return nil
}

func consumedMemoryHandoff(stored memorySetupHandoff, replay bool) ConsumedSetupHandoff {
	return ConsumedSetupHandoff{SetupHandoff: cloneSetupHandoff(stored.record), LinkedInstallationID: stored.linkedInstallation, Replay: replay}
}

func cloneSetupHandoff(input SetupHandoff) SetupHandoff {
	input.Installation.Permissions = clonePermissions(input.Installation.Permissions)
	input.Repositories = cloneRepositoryIdentities(input.Repositories)
	return input
}

func validateSetupAuthorization(authorization SetupAuthorization) error {
	if !uuidRE.MatchString(authorization.ActorID) || !setupIdempotencyRE.MatchString(authorization.IdempotencyKey) ||
		!setupFingerprintRE.MatchString(authorization.RequestFingerprint) || len(authorization.State) < 64 || len(authorization.State) > 4096 ||
		authorization.ExpiresAt.IsZero() || authorization.CreatedAt.IsZero() || !authorization.ExpiresAt.After(authorization.CreatedAt) {
		return ErrInvalid
	}
	return nil
}

func validateSetupHandoff(handoff SetupHandoff) error {
	zero := [sha256.Size]byte{}
	if handoff.Digest == zero || !uuidRE.MatchString(handoff.ActorID) || handoff.GitHubUser.ID <= 0 || handoff.GitHubUser.Type != "User" ||
		!loginRE.MatchString(handoff.GitHubUser.Login) || handoff.ExpiresAt.IsZero() || handoff.CreatedAt.IsZero() || !handoff.ExpiresAt.After(handoff.CreatedAt) ||
		validateSetupProviderResult(handoff.Installation.AppID, handoff.Installation.ID, handoff.GitHubUser.ID, handoff.Installation.Account.ID,
			SetupProviderResult{Verification: githubapp.SetupVerification{User: handoff.GitHubUser, Installation: handoff.Installation}, Repositories: handoff.Repositories}) != nil {
		return ErrInvalid
	}
	return nil
}
