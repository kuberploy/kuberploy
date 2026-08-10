package buildpromotion

import (
	"context"
	"errors"

	"github.com/kuberploy/kuberploy/internal/domain"
	platformstore "github.com/kuberploy/kuberploy/internal/store"
)

type ProjectionCatalog interface {
	SuccessfulReleaseProjection(context.Context, string) (ProjectedBuild, error)
}
type ReleaseCatalog interface {
	RegistryRelease(context.Context, string) (domain.RegistryRelease, error)
}
type ResourceCatalog interface {
	GetEnvironment(context.Context, string) (domain.Environment, error)
	GetApplication(context.Context, string) (domain.Application, error)
}
type Authorizer interface {
	Authorize(context.Context, string, domain.Permission, domain.AccessTarget) error
	AuthorizePromotion(context.Context, string, string, string) error
}

type Resolver struct {
	Projections ProjectionCatalog
	Releases    ReleaseCatalog
	Resources   ResourceCatalog
	Access      Authorizer
}

// ResolveAuthorized resolves immutable build and resource identity and proves
// both required permissions without consulting mutable registry availability.
// It is exposed separately so an HTTP transport can recover an already
// committed deployment response before readiness probes that may later fail.
func (r *Resolver) ResolveAuthorized(ctx context.Context, request Request) (Source, error) {
	if r == nil || r.Projections == nil || r.Resources == nil || r.Access == nil || request.Validate() != nil {
		return Source{}, ErrInvalid
	}
	projected, err := r.Projections.SuccessfulReleaseProjection(ctx, request.AttemptID)
	if err != nil {
		return Source{}, classify(err)
	}
	if projected.Validate() != nil || projected.AttemptID != request.AttemptID {
		return Source{}, ErrConflict
	}
	application, err := r.Resources.GetApplication(ctx, projected.ApplicationID)
	if err != nil {
		return Source{}, classify(err)
	}
	if application.ID != projected.ApplicationID || application.ProjectID != projected.ProjectID {
		return Source{}, ErrConflict
	}
	if err = r.Access.Authorize(ctx, request.ActorID, domain.PermissionBuildsRead, domain.AccessTarget{Type: "application", ID: application.ID}); err != nil {
		return Source{}, err
	}
	environment, err := r.Resources.GetEnvironment(ctx, request.EnvironmentID)
	if err != nil {
		return Source{}, classify(err)
	}
	if environment.ID != request.EnvironmentID || environment.ProjectID != projected.ProjectID {
		return Source{}, ErrNotFound
	}
	if err = r.Access.AuthorizePromotion(ctx, request.ActorID, environment.ID, application.ID); err != nil {
		return Source{}, err
	}
	return Source{ProjectedBuild: projected, EnvironmentID: environment.ID, Namespace: environment.Namespace}, nil
}

func (r *Resolver) Resolve(ctx context.Context, request Request) (Source, error) {
	if r == nil || r.Releases == nil {
		return Source{}, ErrInvalid
	}
	source, err := r.ResolveAuthorized(ctx, request)
	if err != nil {
		return Source{}, err
	}
	release, err := r.Releases.RegistryRelease(ctx, source.ReleaseID)
	if err != nil {
		// A completed projection without its exact independent release row is an
		// unavailable projection, not a caller-visible missing resource.
		if errors.Is(err, ErrNotFound) || errors.Is(err, platformstore.ErrNotFound) {
			return Source{}, ErrNotReady
		}
		return Source{}, classify(err)
	}
	if release.Availability == domain.RegistryArtifactExpired || release.Availability == domain.RegistryArtifactMissing {
		return Source{}, ErrArtifactUnavailable
	}
	source.Release = release
	if source.Validate() != nil {
		return Source{}, ErrConflict
	}
	return source, nil
}

func classify(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrInvalid) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrNotReady) || errors.Is(err, ErrArtifactUnavailable) || errors.Is(err, ErrConflict) {
		return err
	}
	if errors.Is(err, platformstore.ErrNotFound) {
		return ErrNotFound
	}
	if errors.Is(err, platformstore.ErrConflict) {
		return ErrConflict
	}
	return err
}
