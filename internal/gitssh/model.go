package gitssh

import (
	"errors"
	"fmt"
	"strings"
)

type Scope string

const (
	ScopeApp     Scope = "app"
	ScopeProject Scope = "project"
)

func (s Scope) Validate() error {
	switch s {
	case ScopeApp, ScopeProject:
		return nil
	default:
		return fmt.Errorf("invalid Git SSH key scope %q: %w", s, ErrInvalidScope)
	}
}

type Status string

const (
	StatusActive  Status = "active"
	StatusRevoked Status = "revoked"
)

var (
	ErrInvalidScope        = errors.New("scope must be app or project")
	ErrInvalidOwner        = errors.New("owner ID is required")
	ErrActiveKeyExists     = errors.New("active Git SSH key already exists")
	ErrActiveKeyNotFound   = errors.New("active Git SSH key not found")
	ErrInvalidEnvelope     = errors.New("encrypted private-key envelope is invalid")
	ErrHostKeyNotPinned    = errors.New("SSH host key is not pinned")
	ErrHostKeyChanged      = errors.New("SSH host key does not match pin")
	ErrInvalidHostKeyPin   = errors.New("SSH host-key pin is invalid")
	ErrIdempotencyConflict = errors.New("Git SSH idempotency key was used with different input")
)

type MutationOperation string

const (
	OperationCreate MutationOperation = "create"
	OperationRotate MutationOperation = "rotate"
	OperationRevoke MutationOperation = "revoke"
)

type MutationRequest struct {
	Operation          MutationOperation
	ActorID            string
	IdempotencyKey     string
	RequestFingerprint string
	Scope              Scope
	OwnerID            string
}

type MutationResult struct {
	Value  KeyMetadata
	Replay bool
}

func (r MutationRequest) validate() error {
	if err := validateIdentity(r.Scope, r.OwnerID); err != nil {
		return err
	}
	if r.Operation != OperationCreate && r.Operation != OperationRotate && r.Operation != OperationRevoke {
		return errors.New("Git SSH mutation operation is invalid")
	}
	if strings.TrimSpace(r.ActorID) == "" || len(r.IdempotencyKey) < 1 || len(r.IdempotencyKey) > 128 || len(r.RequestFingerprint) != 64 {
		return errors.New("Git SSH mutation idempotency identity is invalid")
	}
	return nil
}

// KeyMetadata is the complete public view of a generated Git SSH key.
// Private key material and its encrypted envelope are deliberately absent.
type KeyMetadata struct {
	Scope       Scope  `json:"scope"`
	OwnerID     string `json:"ownerId"`
	Revision    uint64 `json:"revision"`
	Status      Status `json:"status"`
	PublicKey   string `json:"publicKey"`
	Fingerprint string `json:"fingerprint"`
}

type CreateRequest struct {
	Scope   Scope
	OwnerID string
}

func validateIdentity(scope Scope, ownerID string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(ownerID) == "" {
		return ErrInvalidOwner
	}
	return nil
}
