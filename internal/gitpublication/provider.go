package gitpublication

import (
	"context"
	"time"
)

// CreatePullRequestRequest contains only server-derived repository and ref
// identity. Provider adapters must not accept a caller-provided URL or body.
type CreatePullRequestRequest struct {
	Repository Repository
	TargetRef  string
	HeadRef    string
	HeadSHA    string
	Title      string
	Body       string
}

type FindPullRequestRequest struct {
	Repository Repository
	TargetRef  string
	HeadRef    string
	HeadSHA    string
}

type GetPullRequestRequest struct {
	Repository Repository
	Number     int64
}

type PullRequestObservation struct {
	Repository    Repository
	Number        int64
	URL           string
	TargetRef     string
	HeadRef       string
	HeadRevision  string
	State         PullRequestState
	Merged        bool
	MergeRevision string
	ObservedAt    time.Time
}

func (o PullRequestObservation) ValidateFor(publication Publication) error {
	if publication.Validate() != nil || o.Repository != publication.Repository || o.Number <= 0 ||
		!validPullRequestURL(o.Repository, o.Number, o.URL) || o.TargetRef != publication.TargetRef ||
		o.HeadRef != publication.CandidateRef || o.HeadRevision != publication.CandidateRevision || o.ObservedAt.IsZero() ||
		o.ObservedAt.Before(publication.CreatedAt) || (o.State != PullRequestOpen && o.State != PullRequestClosed) {
		return ErrProviderMismatch
	}
	if o.Merged {
		if o.State != PullRequestClosed || !commitPattern.MatchString(o.MergeRevision) {
			return ErrProviderMismatch
		}
	} else if o.MergeRevision != "" {
		return ErrProviderMismatch
	}
	if publication.PullRequestNumber > 0 && (publication.PullRequestNumber != o.Number || publication.PullRequestURL != o.URL) {
		return ErrProviderMismatch
	}
	return nil
}

type TargetHeadObservation struct {
	Repository Repository
	TargetRef  string
	Revision   string
	ObservedAt time.Time
}

func (o TargetHeadObservation) ValidateFor(publication Publication) error {
	if publication.Validate() != nil || o.Repository != publication.Repository || o.TargetRef != publication.TargetRef ||
		!commitPattern.MatchString(o.Revision) || o.ObservedAt.IsZero() || o.ObservedAt.Before(publication.CreatedAt) ||
		(publication.ProviderObservedAt != nil && o.ObservedAt.Before(*publication.ProviderObservedAt)) {
		return ErrProviderMismatch
	}
	return nil
}

// Provider is the closed future GitHub App publication seam. It can create or
// inspect one exact pull request and prove ancestry on one exact repository;
// it cannot write arbitrary files, refs, comments, checks, or repository data.
type Provider interface {
	CreatePullRequest(context.Context, CreatePullRequestRequest) (PullRequestObservation, error)
	FindPullRequest(context.Context, FindPullRequestRequest) (PullRequestObservation, bool, error)
	GetPullRequest(context.Context, GetPullRequestRequest) (PullRequestObservation, error)
	ResolveTargetHead(context.Context, Repository, string) (TargetHeadObservation, error)
	IsAncestor(context.Context, Repository, string, string) (bool, error)
}
