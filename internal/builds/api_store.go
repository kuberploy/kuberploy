package builds

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

const (
	APICommandDefinitionCreate = "definition.create"
	APICommandAttemptCancel    = "attempt.cancel"
	APICommandAttemptRetry     = "attempt.retry"
)

// APIStore adds bounded metadata and idempotent command operations without
// widening the worker-facing Store contract.
type APIStore interface {
	Store
	Installation(context.Context, string) (Installation, error)
	Repository(context.Context, string) (Repository, error)
	Definition(context.Context, string) (BuildDefinition, error)
	ListRepositories(context.Context, string) ([]Repository, error)
	DefinitionsForService(context.Context, string) ([]BuildDefinition, error)
	// HistoricalAttempt returns the bounded, credential-free attempt projection
	// used by read-only detail and log authorization. It deliberately does not
	// validate private execution inputs that may have changed across releases.
	HistoricalAttempt(context.Context, string) (BuildAttempt, error)
	AttemptsForService(context.Context, string, int) ([]BuildAttempt, error)
	ClaimAPICommand(context.Context, string, string, string, string, string, string, time.Time) (string, bool, error)
	RetryAttempt(context.Context, string, string, string, ExecutionSettings, time.Time) (BuildAttempt, bool, error)
}

func APICommandClaimKey(actorID, operation, scopeID, idempotencyKey string) string {
	digest := sha256.Sum256([]byte("kuberploy-build-api-v1\x00" + actorID + "\x00" + operation + "\x00" + scopeID + "\x00" + idempotencyKey))
	return hex.EncodeToString(digest[:])
}

func RetryAttemptID(claimKey, definitionID string) string {
	return deterministicUUID("build-attempt-v1", claimKey, definitionID)
}

func validAPICommand(operation, actorID, scopeID, key, fingerprint, resourceID string, now time.Time) bool {
	return (operation == APICommandDefinitionCreate || operation == APICommandAttemptCancel || operation == APICommandAttemptRetry) &&
		uuidRE.MatchString(actorID) && uuidRE.MatchString(scopeID) && setupIdempotencyRE.MatchString(key) && setupFingerprintRE.MatchString(fingerprint) &&
		uuidRE.MatchString(resourceID) && !now.IsZero()
}
