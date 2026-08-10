package buildlogs

import (
	"context"
	"errors"

	"github.com/kuberploy/kuberploy/internal/builds"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/store"
)

type AttemptCatalog interface {
	Attempt(context.Context, string) (builds.BuildAttempt, error)
}

type ResourceCatalog interface {
	GetApplication(context.Context, string) (domain.Application, error)
	GetProject(context.Context, string) (domain.Project, error)
	Authorize(context.Context, string, domain.Permission, domain.AccessTarget) error
}

// RecordResolver is the production authorization seam. It follows only the
// persisted attempt -> application -> project ownership chain and performs
// both build visibility and log authorization against that exact chain.
type RecordResolver struct {
	attempts  AttemptCatalog
	resources ResourceCatalog
}

// AttemptOwnershipCatalog adapts the separate in-memory build store to the
// central memory store's audit seam without duplicating build attempts. The
// PostgreSQL central store queries build_attempts directly and does not use it.
type AttemptOwnershipCatalog struct{ attempts AttemptCatalog }

func NewAttemptOwnershipCatalog(attempts AttemptCatalog) (*AttemptOwnershipCatalog, error) {
	if attempts == nil {
		return nil, ErrInvalidRequest
	}
	return &AttemptOwnershipCatalog{attempts: attempts}, nil
}

func (c *AttemptOwnershipCatalog) BuildLogAttemptOwnership(ctx context.Context, attemptID string) (store.BuildLogAttemptOwnership, error) {
	if c == nil || c.attempts == nil || !uuidPattern.MatchString(attemptID) {
		return store.BuildLogAttemptOwnership{}, ErrInvalidRequest
	}
	attempt, err := c.attempts.Attempt(ctx, attemptID)
	if errors.Is(err, builds.ErrNotFound) || errors.Is(err, builds.ErrUnauthorized) {
		return store.BuildLogAttemptOwnership{}, store.ErrNotFound
	}
	if err != nil {
		return store.BuildLogAttemptOwnership{}, err
	}
	if attempt.ID != attemptID || !uuidPattern.MatchString(attempt.ProjectID) || !uuidPattern.MatchString(attempt.ServiceID) {
		return store.BuildLogAttemptOwnership{}, store.ErrNotFound
	}
	return store.BuildLogAttemptOwnership{AttemptID: attempt.ID, ProjectID: attempt.ProjectID, ApplicationID: attempt.ServiceID}, nil
}

func NewRecordResolver(attempts AttemptCatalog, resources ResourceCatalog) (*RecordResolver, error) {
	if attempts == nil || resources == nil {
		return nil, ErrInvalidRequest
	}
	return &RecordResolver{attempts: attempts, resources: resources}, nil
}

func (r *RecordResolver) Resolve(ctx context.Context, access AccessRequest) (AuthorizedAttempt, error) {
	if r == nil || r.attempts == nil || r.resources == nil || !validAccess(access) {
		return AuthorizedAttempt{}, ErrInvalidRequest
	}
	attempt, err := r.attempts.Attempt(ctx, access.AttemptID)
	if err != nil {
		return AuthorizedAttempt{}, catalogError(err)
	}
	application, err := r.resources.GetApplication(ctx, attempt.ServiceID)
	if err != nil || application.ID != attempt.ServiceID || application.ProjectID != attempt.ProjectID {
		if err == nil {
			err = store.ErrNotFound
		}
		return AuthorizedAttempt{}, catalogError(err)
	}
	project, err := r.resources.GetProject(ctx, application.ProjectID)
	if err != nil || project.ID != application.ProjectID {
		if err == nil {
			err = store.ErrNotFound
		}
		return AuthorizedAttempt{}, catalogError(err)
	}
	target := domain.AccessTarget{Type: "application", ID: application.ID, TeamID: project.TeamID, ProjectID: project.ID, ApplicationID: application.ID}
	for _, permission := range []domain.Permission{domain.PermissionBuildsRead, domain.PermissionLogsRead} {
		if err = r.resources.Authorize(ctx, access.ActorID, permission, target); err != nil {
			return AuthorizedAttempt{}, catalogError(err)
		}
	}
	return AuthorizedAttempt{Access: access, Attempt: attempt, ApplicationID: application.ID, ProjectID: project.ID}, nil
}

func (r *RecordResolver) Revalidate(ctx context.Context, access AccessRequest) error {
	_, err := r.Resolve(ctx, access)
	return err
}

func catalogError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, builds.ErrNotFound) || errors.Is(err, builds.ErrUnauthorized) || errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrForbidden) {
		return ErrNotFound
	}
	return err
}

var _ Resolver = (*RecordResolver)(nil)
var _ store.BuildLogAttemptCatalog = (*AttemptOwnershipCatalog)(nil)
