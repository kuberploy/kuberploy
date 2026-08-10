package gitprojection

import (
	"context"
	"errors"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

const (
	DefaultBundleWait  = 5 * time.Second
	MaximumBundleWait  = 10 * time.Second
	bundlePollInterval = 100 * time.Millisecond
)

var ErrPreconditionRequired = errors.New("a strong Git projection precondition is required")

// DeploymentCatalog is the authorization-aware central catalog used by the
// Git projection control-plane. It deliberately exposes no raw Git document,
// credential, arbitrary repository URL, or caller-selected path.
type DeploymentCatalog interface {
	GetEnvironmentForActor(context.Context, string, string) (domain.Environment, error)
	GetApplicationForActor(context.Context, string, string) (domain.Application, error)
	GetEnvironmentGitBindingForActor(context.Context, string, string) (Binding, error)
}

// ControlPlane derives immutable write plans from an authorized, indexed Git
// snapshot. The production store revalidates the complete plan in the same
// transaction that accepts the deployment operation; this object is not an
// authorization or compare-and-swap substitute.
type ControlPlane struct {
	Catalog       DeploymentCatalog
	Store         Store
	ChartDigest   string
	PolicyVersion string
}

func (c *ControlPlane) validate() error {
	if c == nil || c.Catalog == nil || c.Store == nil ||
		!digestRE.MatchString(c.ChartDigest) || c.PolicyVersion == "" || len(c.PolicyVersion) > 128 {
		return ErrInvalid
	}
	return nil
}

func (c *ControlPlane) authorizedBinding(ctx context.Context, actor, environmentID, applicationID string) (Binding, error) {
	if err := c.validate(); err != nil || !uuidRE.MatchString(actor) || !uuidRE.MatchString(environmentID) || !uuidRE.MatchString(applicationID) {
		return Binding{}, ErrInvalid
	}
	environment, err := c.Catalog.GetEnvironmentForActor(ctx, actor, environmentID)
	if err != nil {
		return Binding{}, err
	}
	application, err := c.Catalog.GetApplicationForActor(ctx, actor, applicationID)
	if err != nil {
		return Binding{}, err
	}
	if environment.ProjectID == "" || application.ProjectID != environment.ProjectID {
		return Binding{}, ErrNotFound
	}
	binding, err := c.Catalog.GetEnvironmentGitBindingForActor(ctx, actor, environmentID)
	if err != nil {
		return Binding{}, err
	}
	if binding.Validate() != nil || binding.Kind != BindingEnvironment || binding.ProjectID != environment.ProjectID ||
		binding.EnvironmentID != environment.ID {
		return Binding{}, ErrProviderMismatch
	}
	return binding, nil
}

// PlanMutation distinguishes an absent path from an optimistic update. A
// caller cannot create with a fake/empty ETag and cannot update without the
// exact strong ETag derived from the active indexed tree.
func (c *ControlPlane) PlanMutation(ctx context.Context, actor, environmentID, applicationID, expectedETag string) (WritePlan, error) {
	if err := c.validate(); err != nil {
		return WritePlan{}, err
	}
	binding, err := c.authorizedBinding(ctx, actor, environmentID, applicationID)
	if err != nil {
		return WritePlan{}, err
	}
	applicationPath, err := ApplicationPath(binding, applicationID)
	if err != nil {
		return WritePlan{}, err
	}
	dependencies, err := DependencyPaths(binding)
	if err != nil {
		return WritePlan{}, err
	}
	bundle, bundleErr := c.Store.Bundle(ctx, binding.ID, applicationPath, dependencies, c.ChartDigest, c.PolicyVersion)
	plan := WritePlan{BindingID: binding.ID, ProjectID: binding.ProjectID, EnvironmentID: binding.EnvironmentID,
		ApplicationID: applicationID, ChartDigest: c.ChartDigest, PolicyVersion: c.PolicyVersion}
	switch {
	case errors.Is(bundleErr, ErrNotFound):
		if expectedETag != "" {
			return WritePlan{}, ErrConflict
		}
		plan.BaseRevision = binding.IndexedRevision
		plan.Precondition = MutationCreateIfAbsent
	case bundleErr != nil:
		return WritePlan{}, bundleErr
	default:
		if bundle.BindingID != binding.ID || bundle.IndexedRevision == "" {
			return WritePlan{}, ErrProviderMismatch
		}
		if expectedETag == "" {
			return WritePlan{}, ErrPreconditionRequired
		}
		if !validStrongETag(expectedETag) || expectedETag != bundle.ETag {
			return WritePlan{}, ErrConflict
		}
		plan.BaseRevision = bundle.IndexedRevision
		plan.Precondition = MutationMatchETag
		plan.ExpectedETag = expectedETag
	}
	// The central transaction performs the authoritative ready/head/index
	// validation after its idempotency replay check. Keeping planning itself
	// side-effect free means a retry can still recover its exact first response
	// while the binding is temporarily indexing.
	if plan.validateIdentity(binding) != nil {
		return WritePlan{}, ErrStale
	}
	return plan, nil
}

// Bundle returns only the active indexed projection. atLeastRevision is a
// fail-closed convergence fence: because the database does not infer Git
// ancestry after a force-push/full-reindex, only the exact indexed revision
// satisfies it. The bounded wait avoids turning API reads into long-held
// database or provider calls.
func (c *ControlPlane) Bundle(ctx context.Context, actor string, deployment domain.Deployment, atLeastRevision string, wait time.Duration) (Bundle, error) {
	if err := c.validate(); err != nil {
		return Bundle{}, err
	}
	if atLeastRevision != "" && !commitRE.MatchString(atLeastRevision) || wait < 0 || wait > MaximumBundleWait || atLeastRevision == "" && wait != 0 {
		return Bundle{}, ErrInvalid
	}
	binding, err := c.authorizedBinding(ctx, actor, deployment.EnvironmentID, deployment.ApplicationID)
	if err != nil {
		return Bundle{}, err
	}
	applicationPath, err := ApplicationPath(binding, deployment.ApplicationID)
	if err != nil {
		return Bundle{}, err
	}
	dependencies, err := DependencyPaths(binding)
	if err != nil {
		return Bundle{}, err
	}
	deadline := time.Now().UTC().Add(wait)
	for {
		bundle, bundleErr := c.Store.Bundle(ctx, binding.ID, applicationPath, dependencies, c.ChartDigest, c.PolicyVersion)
		if bundleErr != nil {
			return Bundle{}, bundleErr
		}
		if atLeastRevision == "" || bundle.IndexedRevision == atLeastRevision {
			return bundle, nil
		}
		if wait == 0 || !time.Now().UTC().Before(deadline) {
			return bundle, ErrStale
		}
		delay := bundlePollInterval
		if remaining := time.Until(deadline); remaining < delay {
			delay = remaining
		}
		if delay <= 0 {
			return bundle, ErrStale
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return Bundle{}, ctx.Err()
		case <-timer.C:
		}
	}
}

var _ interface {
	PlanMutation(context.Context, string, string, string, string) (WritePlan, error)
	Bundle(context.Context, string, domain.Deployment, string, time.Duration) (Bundle, error)
} = (*ControlPlane)(nil)
