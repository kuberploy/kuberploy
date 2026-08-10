package buildlogs

import (
	"context"
	"errors"

	"github.com/kuberploy/kuberploy/internal/store"
)

type DurableAuditStore interface {
	AuditBuildLogAccess(context.Context, string, string, string, string) error
}

// StoreAuditor adapts the central store's atomic authorization/audit method to
// the log service. It forwards only opaque actor/attempt/request identities and
// a closed action; Kubernetes identities and log bodies cannot cross it.
type StoreAuditor struct{ store DurableAuditStore }

func NewStoreAuditor(store DurableAuditStore) (*StoreAuditor, error) {
	if store == nil {
		return nil, ErrInvalidRequest
	}
	return &StoreAuditor{store: store}, nil
}

func (a *StoreAuditor) AuditBuildLogAccess(ctx context.Context, event AuditEvent) error {
	if a == nil || a.store == nil || !validAccess(AccessRequest{ActorID: event.ActorID, AttemptID: event.AttemptID}) ||
		!requestIDPattern.MatchString(event.RequestID) || event.At.IsZero() || event.Action != "build.logs.snapshot" && event.Action != "build.logs.follow" {
		return ErrInvalidRequest
	}
	err := a.store.AuditBuildLogAccess(ctx, event.ActorID, event.AttemptID, event.Action, event.RequestID)
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrUnauthorized) || errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrForbidden) {
		return ErrNotFound
	}
	return err
}

var _ Auditor = (*StoreAuditor)(nil)
