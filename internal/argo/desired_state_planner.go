package argo

import (
	"context"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

const DesiredStateProjectionApprovalContract = "argo-projection-approval-v1"

// DesiredStateProjectionApproval is returned only by the trusted central
// projection/reference resolver. The resolver computes CatalogDigest over the
// exact active documents, dependency blobs, and resolved secret-reference
// receipts. RegistryReferencesResolved is output-only planner state: Plan
// clears any caller value and sets it only after the separate exact indexed
// AppConfig/artifact eligibility resolver succeeds.
type DesiredStateProjectionApproval struct {
	Contract                   string
	BindingID                  string
	IndexedRevision            string
	ProjectionGeneration       int64
	CatalogDigest              string
	Applications               []domain.Application
	Deployments                []domain.Deployment
	AppConfigsValid            bool
	DependenciesValid          bool
	SecretReferencesResolved   bool
	RegistryReferencesResolved bool
}

func (a DesiredStateProjectionApproval) ValidateFor(target DesiredStateTarget) error {
	return a.validateFor(target, true)
}

func (a DesiredStateProjectionApproval) validateFor(target DesiredStateTarget, requireRegistry bool) error {
	if target.Validate() != nil || a.Contract != DesiredStateProjectionApprovalContract || a.BindingID != target.Environment.Binding.ID ||
		a.IndexedRevision != target.Environment.Binding.IndexedRevision || a.ProjectionGeneration != target.Environment.Binding.ProjectionGeneration ||
		!digestRE.MatchString(a.CatalogDigest) || !a.AppConfigsValid || !a.DependenciesValid ||
		!a.SecretReferencesResolved || requireRegistry && !a.RegistryReferencesResolved {
		return ErrInvalid
	}
	return nil
}

type DesiredStateProjectionGate interface {
	ApproveDesiredStateProjection(context.Context, DesiredStateTarget) (DesiredStateProjectionApproval, error)
}

// DesiredStateRegistryEligibilityResolver derives registry resolution from the
// exact active indexed AppConfig policy documents. Implementations must ignore
// DesiredStateProjectionApproval.RegistryReferencesResolved as input. A
// private document is eligible only after its exact operator-owned pull
// artifact is active, ready, and freshly observed; public documents are
// eligible without an artifact.
type DesiredStateRegistryEligibilityResolver interface {
	ResolveRegistryReferences(context.Context, DesiredStateTarget, DesiredStateProjectionApproval, time.Time) (bool, error)
}

type DesiredStateClaimMode string

const (
	// DesiredStateClaimActive permits a new Git mutation only while the exact
	// approved environment generation remains active.
	DesiredStateClaimActive DesiredStateClaimMode = "active"
	// DesiredStateClaimRecovery permits only inspection/finalization of a Git
	// operation that may already have been pushed. The durable approval receipt
	// must still authenticate the command, but a newer active generation cannot
	// strand crash recovery.
	DesiredStateClaimRecovery DesiredStateClaimMode = "recovery"
)

// DesiredStateClaimGate revalidates the durable approval receipt immediately
// after a command is claimed and before any Git mutation. A production
// implementation must authenticate the command's exact environment revision,
// projection generation, catalog digest, and secret/registry resolution
// receipts. Active mode additionally requires that exact tuple to remain the
// active projection; recovery mode exists only to finalize an operation that
// may already be present in protected Git.
type DesiredStateClaimGate interface {
	ValidateDesiredStateClaim(context.Context, DesiredStateCommand, DesiredStateClaimMode) error
}

// ProductionDesiredStateClaimGate marks the stronger implementation accepted
// by the production runtime. It must reconstruct the immutable approval from
// the exact indexed generation and re-evaluate registry artifact eligibility
// at claim time. Test/static gates deliberately do not satisfy this boundary.
type ProductionDesiredStateClaimGate interface {
	DesiredStateClaimGate
	productionDesiredStateClaimGate()
}

type DesiredStatePlanner struct {
	Projection          DesiredStateProjectionGate
	RegistryEligibility DesiredStateRegistryEligibilityResolver
}

func (p DesiredStatePlanner) Plan(ctx context.Context, id string, target DesiredStateTarget, previous *DesiredStateCommand, now time.Time) (DesiredStateCommand, error) {
	if p.Projection == nil || p.RegistryEligibility == nil || target.Validate() != nil || now.IsZero() {
		return DesiredStateCommand{}, ErrInvalid
	}
	approval, err := p.Projection.ApproveDesiredStateProjection(ctx, target)
	if err != nil {
		return DesiredStateCommand{}, err
	}
	// A caller-provided true is never authority. Validate every other closed
	// projection prerequisite first, then derive this one from exact indexed
	// policy documents inside the resolver's transaction.
	approval.RegistryReferencesResolved = false
	if approval.validateFor(target, false) != nil {
		return DesiredStateCommand{}, ErrInvalid
	}
	resolved, err := p.RegistryEligibility.ResolveRegistryReferences(ctx, target, approval, now.UTC())
	if err != nil {
		return DesiredStateCommand{}, err
	}
	if !resolved {
		return DesiredStateCommand{}, ErrRegistryReferencesNotReady
	}
	approval.RegistryReferencesResolved = true
	if approval.ValidateFor(target) != nil {
		return DesiredStateCommand{}, ErrInvalid
	}
	return newDesiredStateCommand(id, target, approval, previous, now)
}
