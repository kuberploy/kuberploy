package certissuers

import (
	"context"
	"time"
)

type Store interface {
	Create(context.Context, Command, string, Spec) (MutationResult, error)
	Revise(context.Context, Command, Ref, Spec) (MutationResult, error)
	Deactivate(context.Context, Command, Ref) (MutationResult, error)
	ReplayCreate(context.Context, Command, string, Spec) (MutationResult, bool, error)
	ReplayRevise(context.Context, Command, Ref, Spec) (MutationResult, bool, error)
	ReplayDeactivate(context.Context, Command, Ref) (MutationResult, bool, error)
	Current(context.Context, string) (Entry, error)
	List(context.Context, int) ([]Entry, error)
	PendingMaterialization(context.Context, int) ([]Desired, error)
	PendingDematerialization(context.Context, int) ([]Desired, error)
	RecordObservation(context.Context, Observation) error
	Observation(context.Context, string, int64) (Observation, error)
	ReadyForHostname(context.Context, string, time.Time, time.Duration, int) ([]TenantIdentity, error)
}

// Catalog is the tenant-safe read service. Its output type cannot carry the
// stored ACME or provider configuration.
type Catalog struct{ store Store }

func NewCatalog(store Store) (*Catalog, error) {
	if store == nil {
		return nil, ErrInvalid
	}
	return &Catalog{store}, nil
}
func (c *Catalog) ForHostname(ctx context.Context, hostname string, now time.Time, maxAge time.Duration, limit int) ([]TenantIdentity, error) {
	if !validHostname(hostname, true) || !validFreshness(now, maxAge) {
		return nil, ErrInvalid
	}
	return c.store.ReadyForHostname(ctx, hostname, now, maxAge, limit)
}

// Materializer is the protected-Git seam. Implementations must publish only
// exact Desired records to a protected platform repository and may report
// Ready only after cert-manager observes the same spec digest. This package
// intentionally has no Kubernetes write client: ordinary API credentials must
// never gain cluster-scoped ClusterIssuer mutation.
type Materializer interface {
	Reconcile(context.Context, Desired) (Observation, error)
}
