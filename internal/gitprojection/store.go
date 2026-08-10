package gitprojection

import (
	"context"
	"errors"
	"time"
)

// Store is the durable boundary for verified ref observations, shadow
// projection generations, webhook tombstones and short path/ref write lanes.
// ActivateGeneration must atomically swap the active generation and checkpoint.
type Store interface {
	PutBinding(context.Context, Binding) error
	Binding(context.Context, string) (Binding, error)
	SetBindingState(context.Context, string, string, BindingState, time.Time) error
	RecordVerifiedHead(context.Context, VerifiedHead) (Binding, bool, error)
	ClaimWebhook(context.Context, WebhookTombstone) (bool, error)
	PollCursor(context.Context, string) (PollCursor, error)
	PutPollCursor(context.Context, PollCursor) error
	ClaimReconciliation(context.Context, string, time.Time, time.Duration) (ReconciliationWork, error)
	HeartbeatReconciliation(context.Context, ReconciliationLease, time.Time, time.Duration) (ReconciliationLease, error)
	FinishReconciliation(context.Context, ReconciliationLease, ReconciliationOutcome, time.Time) error
	ReleaseReconciliation(context.Context, ReconciliationLease, time.Time) error

	BeginGeneration(context.Context, ReconciliationLease, string, string, time.Time) (Generation, error)
	PutDocuments(context.Context, Generation, []Document) error
	ActivateGeneration(context.Context, ReconciliationLease, Generation, AppConfigPolicyValidator, time.Time) (Binding, error)
	FailGeneration(context.Context, ReconciliationLease, Generation, time.Time) error
	Document(context.Context, string, string) (Document, error)
	Bundle(context.Context, string, string, []string, string, string) (Bundle, error)

	AcquirePath(context.Context, PathReservation, time.Time, time.Duration) (PathReservation, bool, error)
	PathReservation(context.Context, string, string, string) (PathReservation, error)
	FinalizePath(context.Context, string, string, string, string, string, time.Time) (PathReservation, error)
	FinalizeVerifiedPath(context.Context, string, string, string, string, string, VerifiedHead, time.Time) (PathReservation, error)
	RepairExpiredPath(context.Context, string, string, string, bool, string, time.Time) error
}

// BindingCatalog is the narrow control-plane management/read surface. It is
// intentionally separate from Store so API wiring does not gain access to
// projection leases, raw documents, reservations, or mutation finalization.
type BindingCatalog interface {
	PutBinding(context.Context, Binding) error
	Binding(context.Context, string) (Binding, error)
	BindingsForScope(context.Context, BindingKind, string) ([]Binding, error)
}

// WriteCommandStore owns immutable accepted Git bytes and their durable
// commit/index receipts. Command creation by the production control-plane is
// performed inside its deployment-operation transaction; PutWriteCommand is
// retained for isolated contract tests and adapters with an equivalent atomic
// outer transaction.
type WriteCommandStore interface {
	PutWriteCommand(context.Context, WriteCommand) error
	WriteCommand(context.Context, string) (WriteCommand, error)
	MarkWriteCommandCommitted(context.Context, string, string, time.Time) (WriteCommand, error)
}

// HeadVerifier must make an authenticated provider API request for the exact
// installation/repository/ref in the Binding. It must not trust webhook payload
// identity or after-SHA as proof of the target head.
type HeadVerifier interface {
	VerifyTargetHead(context.Context, Binding, ObservationSource) (VerifiedHead, error)
}

type Service struct {
	Store    Store
	Provider HeadVerifier
	Now      func() time.Time
}

func (s Service) ObserveTargetHead(ctx context.Context, bindingID string, source ObservationSource) (Binding, bool, error) {
	if s.Store == nil || s.Provider == nil {
		return Binding{}, false, ErrInvalid
	}
	binding, err := s.Store.Binding(ctx, bindingID)
	if err != nil {
		return Binding{}, false, err
	}
	observation, err := s.Provider.VerifyTargetHead(ctx, binding, source)
	if err != nil {
		if errors.Is(err, ErrMissingRef) {
			now := time.Now().UTC()
			if s.Now != nil {
				now = s.Now().UTC()
			}
			if stateErr := s.Store.SetBindingState(ctx, binding.ID, binding.TargetHeadRevision, BindingMissingRef, now); stateErr != nil {
				return Binding{}, false, stateErr
			}
			binding.State, binding.UpdatedAt = BindingMissingRef, now
			return binding, false, ErrMissingRef
		}
		return Binding{}, false, err
	}
	if err = observation.ValidateFor(binding); err != nil {
		return Binding{}, false, err
	}
	return s.Store.RecordVerifiedHead(ctx, observation)
}

// GetBundle serves only the indexed projection. It reports staleness in the
// result instead of synchronously calling Git or the provider on the read path.
func (s Service) GetBundle(ctx context.Context, bindingID, applicationPath string, dependencies []string, chartDigest, policyVersion string) (Bundle, error) {
	if s.Store == nil {
		return Bundle{}, ErrInvalid
	}
	return s.Store.Bundle(ctx, bindingID, applicationPath, dependencies, chartDigest, policyVersion)
}

func (s Service) RequireFreshBundle(ctx context.Context, bindingID, applicationPath string, dependencies []string, chartDigest, policyVersion string) (Bundle, error) {
	bundle, err := s.GetBundle(ctx, bindingID, applicationPath, dependencies, chartDigest, policyVersion)
	if err != nil {
		return Bundle{}, err
	}
	if bundle.Stale {
		return bundle, ErrStale
	}
	return bundle, nil
}
