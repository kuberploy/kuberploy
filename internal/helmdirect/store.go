package helmdirect

import (
	"context"
	"time"
)

type Actor struct {
	ID, IdempotencyKey, RequestID string
}

func (a Actor) Validate() error {
	if !uuidRE.MatchString(a.ID) || len(a.IdempotencyKey) < 16 || len(a.IdempotencyKey) > 128 || len(a.RequestID) < 1 || len(a.RequestID) > 128 || stringsContainControl(a.IdempotencyKey+a.RequestID) {
		return ErrInvalid
	}
	return nil
}

func stringsContainControl(value string) bool {
	for _, current := range value {
		if current < 0x20 || current == 0x7f {
			return true
		}
	}
	return false
}

type DeployRequest struct {
	Target Target
	Actor  Actor
	Source Source
	Values []byte
}

type MutationRequest struct {
	Target           Target
	Actor            Actor
	RollbackSourceID string
}

type Store interface {
	Deploy(context.Context, DeployRequest, time.Time) (Revision, bool, error)
	Retry(context.Context, MutationRequest, time.Time) (Revision, bool, error)
	Disable(context.Context, MutationRequest, time.Time) (Revision, bool, error)
	Rollback(context.Context, MutationRequest, time.Time) (Revision, bool, error)
	Head(context.Context, Target) (Revision, error)
	History(context.Context, Target, int) ([]Revision, error)
	Pending(context.Context, int) ([]Revision, error)
	MarkApplied(context.Context, string, time.Time) error
	MarkFailed(context.Context, string, string, time.Time) error
}

type Reconciler interface {
	Reconcile(context.Context, Revision) error
}

type Service struct {
	Store      Store
	Reconciler Reconciler
}

type Capabilities struct {
	HelmDeployments bool
	HelmRollbacks   bool
}

func (s *Service) Capabilities(context.Context) (Capabilities, error) {
	if s == nil || s.Store == nil || s.Reconciler == nil {
		return Capabilities{}, ErrUnavailable
	}
	return Capabilities{HelmDeployments: true, HelmRollbacks: true}, nil
}

func (s *Service) Head(ctx context.Context, target Target) (Revision, error) {
	if s == nil || s.Store == nil {
		return Revision{}, ErrUnavailable
	}
	return s.Store.Head(ctx, target)
}

func (s *Service) History(ctx context.Context, target Target, limit int) ([]Revision, error) {
	if s == nil || s.Store == nil {
		return nil, ErrUnavailable
	}
	return s.Store.History(ctx, target, limit)
}

func (s *Service) Deploy(ctx context.Context, request DeployRequest, now time.Time) (Revision, bool, error) {
	if s == nil || s.Store == nil || s.Reconciler == nil {
		return Revision{}, false, ErrUnavailable
	}
	revision, replay, err := s.Store.Deploy(ctx, request, now)
	if err != nil || replay {
		return revision, replay, err
	}
	return s.finish(ctx, revision, now)
}

func (s *Service) Retry(ctx context.Context, request MutationRequest, now time.Time) (Revision, bool, error) {
	if s == nil || s.Store == nil || s.Reconciler == nil {
		return Revision{}, false, ErrUnavailable
	}
	return s.mutate(ctx, request, now, s.Store.Retry)
}

func (s *Service) Disable(ctx context.Context, request MutationRequest, now time.Time) (Revision, bool, error) {
	if s == nil || s.Store == nil || s.Reconciler == nil {
		return Revision{}, false, ErrUnavailable
	}
	return s.mutate(ctx, request, now, s.Store.Disable)
}

func (s *Service) Rollback(ctx context.Context, request MutationRequest, now time.Time) (Revision, bool, error) {
	if s == nil || s.Store == nil || s.Reconciler == nil {
		return Revision{}, false, ErrUnavailable
	}
	return s.mutate(ctx, request, now, s.Store.Rollback)
}

func (s *Service) mutate(ctx context.Context, request MutationRequest, now time.Time,
	mutation func(context.Context, MutationRequest, time.Time) (Revision, bool, error)) (Revision, bool, error) {
	revision, replay, err := mutation(ctx, request, now)
	if err != nil || replay {
		return revision, replay, err
	}
	return s.finish(ctx, revision, now)
}

func (s *Service) finish(ctx context.Context, revision Revision, now time.Time) (Revision, bool, error) {
	if err := s.reconcile(ctx, revision, now); err != nil {
		if markErr := s.Store.MarkFailed(ctx, revision.ID, "argo-apply-failed", now); markErr != nil {
			return Revision{}, false, markErr
		}
		revision.State, revision.FailureCode, revision.UpdatedAt = StateFailed, "argo-apply-failed", now.UTC()
		return revision, false, nil
	}
	revision.State, revision.FailureCode, revision.UpdatedAt = StateApplied, "", now.UTC()
	return revision, false, nil
}

func (s *Service) reconcile(ctx context.Context, revision Revision, now time.Time) error {
	if err := s.Reconciler.Reconcile(ctx, revision); err != nil {
		return err
	}
	return s.Store.MarkApplied(ctx, revision.ID, now)
}

func (s *Service) ReconcilePending(ctx context.Context, limit int, now time.Time) error {
	items, err := s.Store.Pending(ctx, limit)
	if err != nil {
		return err
	}
	for _, item := range items {
		if _, _, err = s.finish(ctx, item, now); err != nil {
			return err
		}
	}
	return nil
}
