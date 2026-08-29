package gitpublication

import (
	"context"
	"errors"
	"time"
)

// VerifiedMergeRefresher completes the provider-to-runtime handoff before a
// protected publication becomes terminal. Failures leave the publication in
// merge-pending so the reconciler retries the same idempotent refresh.
type VerifiedMergeRefresher interface {
	RefreshVerifiedMerge(context.Context, Publication, TargetHeadObservation) error
}

type Service struct {
	Store         Store
	Provider      Provider
	VerifiedMerge VerifiedMergeRefresher
	Now           func() time.Time
}

func (s Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s Service) validate() error {
	if s.Store == nil || s.Provider == nil {
		return ErrInvalid
	}
	return nil
}

func (s Service) RecordCandidate(ctx context.Context, operationID, revision string) (Publication, error) {
	if err := s.validate(); err != nil {
		return Publication{}, err
	}
	current, err := s.Store.Publication(ctx, operationID)
	if err != nil {
		return Publication{}, err
	}
	next, err := current.WithCandidate(revision, s.now())
	if err != nil {
		return Publication{}, err
	}
	if next.Version == current.Version {
		return current, nil
	}
	if err = s.Store.CompareAndSwapPublication(ctx, current, next); err != nil {
		return Publication{}, err
	}
	return next, nil
}

func (s Service) RecordWriteBase(ctx context.Context, operationID, revision string) (Publication, error) {
	if err := s.validate(); err != nil {
		return Publication{}, err
	}
	current, err := s.Store.Publication(ctx, operationID)
	if err != nil {
		return Publication{}, err
	}
	next, err := current.WithWriteBase(revision, s.now())
	if err != nil {
		return Publication{}, err
	}
	if next.Version == current.Version {
		return current, nil
	}
	if err = s.Store.CompareAndSwapPublication(ctx, current, next); err != nil {
		return Publication{}, err
	}
	return next, nil
}

// EnsurePullRequest returns one exact provider pull request. If creation has
// an ambiguous result, the deterministic head/base lookup is repeated before
// the error is returned, allowing a lost 201 response to recover without a
// duplicate PR.
func (s Service) EnsurePullRequest(ctx context.Context, operationID string) (Publication, error) {
	if err := s.validate(); err != nil {
		return Publication{}, err
	}
	current, err := s.Store.Publication(ctx, operationID)
	if err != nil {
		return Publication{}, err
	}
	if current.State == StatePendingCandidate || current.State == StateWriteBaseReady {
		return Publication{}, ErrConflict
	}
	if current.State == StateMergeVerified {
		return current, nil
	}
	var observation PullRequestObservation
	if current.PullRequestNumber > 0 {
		observation, err = s.Provider.GetPullRequest(ctx, GetPullRequestRequest{Repository: current.Repository, Number: current.PullRequestNumber})
	} else {
		find := FindPullRequestRequest{Repository: current.Repository, TargetRef: current.TargetRef, HeadRef: current.CandidateRef, HeadSHA: current.CandidateRevision}
		var found bool
		observation, found, err = s.Provider.FindPullRequest(ctx, find)
		if err == nil && !found {
			var title, body string
			title, err = PullRequestTitle(current.OperationID)
			if err == nil {
				body, err = PullRequestBody(current)
			}
			if err == nil {
				observation, err = s.Provider.CreatePullRequest(ctx, CreatePullRequestRequest{
					Repository: current.Repository, TargetRef: current.TargetRef, HeadRef: current.CandidateRef,
					HeadSHA: current.CandidateRevision, Title: title, Body: body,
				})
			}
			if err != nil {
				createErr := err
				observation, found, err = s.Provider.FindPullRequest(ctx, find)
				if err != nil {
					return Publication{}, errors.Join(createErr, err)
				}
				if !found {
					return Publication{}, createErr
				}
			}
		}
	}
	if err != nil {
		return Publication{}, err
	}
	return s.applyObservation(ctx, current, observation)
}

func (s Service) Observe(ctx context.Context, operationID string) (Publication, error) {
	if err := s.validate(); err != nil {
		return Publication{}, err
	}
	current, err := s.Store.Publication(ctx, operationID)
	if err != nil {
		return Publication{}, err
	}
	if current.PullRequestNumber == 0 {
		return Publication{}, ErrConflict
	}
	observation, err := s.Provider.GetPullRequest(ctx, GetPullRequestRequest{Repository: current.Repository, Number: current.PullRequestNumber})
	if err != nil {
		return Publication{}, err
	}
	return s.applyObservation(ctx, current, observation)
}

func (s Service) applyObservation(ctx context.Context, current Publication, observation PullRequestObservation) (Publication, error) {
	now := s.now()
	if observation.ObservedAt.After(now) {
		return Publication{}, ErrProviderMismatch
	}
	next, err := current.WithPullRequest(observation, now)
	if err != nil {
		return Publication{}, err
	}
	if err = s.Store.CompareAndSwapPublication(ctx, current, next); err != nil {
		return Publication{}, err
	}
	if next.State != StateMergePending {
		return next, nil
	}
	return s.verifyMerge(ctx, next)
}

func (s Service) verifyMerge(ctx context.Context, current Publication) (Publication, error) {
	head, err := s.Provider.ResolveTargetHead(ctx, current.Repository, current.TargetRef)
	if err != nil {
		return current, err
	}
	if head.ValidateFor(current) != nil || head.ObservedAt.After(s.now()) {
		return current, ErrProviderMismatch
	}
	present, err := s.Provider.IsAncestor(ctx, current.Repository, current.MergeRevision, head.Revision)
	if err != nil {
		return current, err
	}
	if !present {
		return current, ErrMergeNotVisible
	}
	next, err := current.WithVerifiedMerge(head.Revision, s.now())
	if err != nil {
		return current, err
	}
	if s.VerifiedMerge != nil {
		if err = s.VerifiedMerge.RefreshVerifiedMerge(ctx, next, head); err != nil {
			return current, err
		}
	}
	if err = s.Store.CompareAndSwapPublication(ctx, current, next); err != nil {
		return current, err
	}
	return next, nil
}
