package certissuers

import (
	"context"

	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

// ProtectedController is the trusted profile-store-to-Git bridge. In
// particular, deletion is not exposed on ProtectedGitPublisher: only profiles
// returned by Store.PendingDematerialization (which is DB-fenced on live
// application references) can reach the exact-match delete path.
type ProtectedController struct {
	Store     Store
	Publisher *ProtectedGitPublisher
}

type ProtectedControllerResult struct {
	Materialized   []PublicationReceipt
	Dematerialized []PublicationReceipt
}

func (c *ProtectedController) Reconcile(ctx context.Context, limit int) (ProtectedControllerResult, error) {
	if ctx == nil || c == nil || c.Store == nil || c.Publisher == nil || limit < 1 || limit > 500 {
		return ProtectedControllerResult{}, ErrInvalid
	}
	active, err := c.Store.PendingMaterialization(ctx, limit)
	if err != nil {
		return ProtectedControllerResult{}, err
	}
	removed, err := c.Store.PendingDematerialization(ctx, limit)
	if err != nil {
		return ProtectedControllerResult{}, err
	}
	result := ProtectedControllerResult{Materialized: make([]PublicationReceipt, 0, len(active)), Dematerialized: make([]PublicationReceipt, 0, len(removed))}
	for _, desired := range active {
		receipt, publishErr := c.Publisher.Materialize(ctx, desired)
		if publishErr != nil {
			return result, publishErr
		}
		result.Materialized = append(result.Materialized, receipt)
	}
	for _, desired := range removed {
		receipt, publishErr := c.Publisher.publish(ctx, desired, gitprojection.MutationDelete)
		if publishErr != nil {
			return result, publishErr
		}
		result.Dematerialized = append(result.Dematerialized, receipt)
	}
	return result, nil
}
