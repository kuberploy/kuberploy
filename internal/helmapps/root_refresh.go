package helmapps

import (
	"context"
	"time"

	"github.com/kuberploy/kuberploy/internal/argo"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

// ProtectedRootRefresher asks Argo to re-read the exact verified platform Git
// head after a protected Helm Application mutation. Argo's automated sync
// policy remains the only workload sync executor.
type ProtectedRootRefresher interface {
	Validate() error
	RefreshProtectedRoot(context.Context, gitprojection.Binding, gitprojection.VerifiedHead, time.Time) error
}

// ProductionProtectedRootRefresher adapts the same admission-fenced,
// metadata-only root refresh used by ordinary desired-state publication.
type ProductionProtectedRootRefresher struct {
	Identity  argo.DesiredStateRuntimeIdentity
	Refresher argo.PlatformRootRefresher
}

func (r *ProductionProtectedRootRefresher) Validate() error {
	if r == nil || r.Identity.Validate() != nil || r.Refresher == nil {
		return ErrInvalid
	}
	return nil
}

func (r *ProductionProtectedRootRefresher) RefreshProtectedRoot(ctx context.Context,
	binding gitprojection.Binding, head gitprojection.VerifiedHead, now time.Time) error {
	if r.Validate() != nil || ctx == nil || now.IsZero() || binding.Validate() != nil ||
		head.ValidateFor(binding) != nil || now.Before(head.ObservedAt) {
		return ErrInvalid
	}
	expectation, err := argo.NewPlatformRootApplicationExpectation(r.Identity, binding, head)
	if err != nil {
		return err
	}
	return r.Refresher.RefreshPlatformRootApplication(ctx, expectation, now.UTC())
}
