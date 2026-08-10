package gitpublication

import (
	"context"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/githubapp"
)

type GitHubAuthorization struct {
	Account    githubapp.AccountIdentity
	Repository githubapp.RepositoryIdentity
}

type GitHubAuthorizationStore interface {
	GitHubPublicationAuthorization(context.Context, Repository, int64) (GitHubAuthorization, error)
}

type GitHubClient interface {
	VerifyInstallation(context.Context, int64, githubapp.AccountIdentity, githubapp.Permissions) (githubapp.Installation, error)
	MintInstallationToken(context.Context, githubapp.TokenRequest) (githubapp.InstallationToken, error)
	CreatePullRequest(context.Context, githubapp.InstallationToken, githubapp.RepositoryIdentity, string, string, string, string) (githubapp.PullRequest, error)
	FindPullRequest(context.Context, githubapp.InstallationToken, githubapp.RepositoryIdentity, string, string) (githubapp.PullRequest, bool, error)
	GetPullRequest(context.Context, githubapp.InstallationToken, githubapp.RepositoryIdentity, int64) (githubapp.PullRequest, error)
	ResolveRemoteRef(context.Context, githubapp.InstallationToken, githubapp.RepositoryIdentity, string) (githubapp.ResolvedRef, error)
	IsCommitAncestor(context.Context, githubapp.InstallationToken, githubapp.RepositoryIdentity, string, string) (bool, error)
}

type GitHubProvider struct {
	AppID          int64
	Authorizations GitHubAuthorizationStore
	Client         GitHubClient
	Now            func() time.Time
}

func (p GitHubProvider) now() time.Time {
	if p.Now != nil {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}

func (p GitHubProvider) CreatePullRequest(ctx context.Context, request CreatePullRequestRequest) (PullRequestObservation, error) {
	if request.Repository.Validate() != nil || !validRef(request.TargetRef) || !validRef(request.HeadRef) || request.TargetRef == request.HeadRef ||
		!commitPattern.MatchString(request.HeadSHA) || !validCreateRequestText(request) {
		return PullRequestObservation{}, ErrInvalid
	}
	authorization, token, err := p.authorize(ctx, request.Repository)
	if err != nil {
		return PullRequestObservation{}, err
	}
	observed, err := p.Client.CreatePullRequest(ctx, token, authorization.Repository, request.TargetRef, request.HeadRef, request.Title, request.Body)
	if err != nil {
		return PullRequestObservation{}, err
	}
	result, err := mapGitHubPullRequest(observed, request.Repository)
	if err != nil || result.TargetRef != request.TargetRef || result.HeadRef != request.HeadRef || result.HeadRevision != request.HeadSHA {
		return PullRequestObservation{}, ErrProviderMismatch
	}
	return result, nil
}

func (p GitHubProvider) FindPullRequest(ctx context.Context, request FindPullRequestRequest) (PullRequestObservation, bool, error) {
	if request.Repository.Validate() != nil || !validRef(request.TargetRef) || !validRef(request.HeadRef) || request.TargetRef == request.HeadRef ||
		!commitPattern.MatchString(request.HeadSHA) {
		return PullRequestObservation{}, false, ErrInvalid
	}
	authorization, token, err := p.authorize(ctx, request.Repository)
	if err != nil {
		return PullRequestObservation{}, false, err
	}
	observed, found, err := p.Client.FindPullRequest(ctx, token, authorization.Repository, request.TargetRef, request.HeadRef)
	if err != nil || !found {
		return PullRequestObservation{}, found, err
	}
	result, err := mapGitHubPullRequest(observed, request.Repository)
	if err != nil || result.TargetRef != request.TargetRef || result.HeadRef != request.HeadRef || result.HeadRevision != request.HeadSHA {
		return PullRequestObservation{}, false, ErrProviderMismatch
	}
	return result, true, nil
}

func (p GitHubProvider) GetPullRequest(ctx context.Context, request GetPullRequestRequest) (PullRequestObservation, error) {
	if request.Repository.Validate() != nil || request.Number <= 0 {
		return PullRequestObservation{}, ErrInvalid
	}
	authorization, token, err := p.authorize(ctx, request.Repository)
	if err != nil {
		return PullRequestObservation{}, err
	}
	observed, err := p.Client.GetPullRequest(ctx, token, authorization.Repository, request.Number)
	if err != nil {
		return PullRequestObservation{}, err
	}
	result, err := mapGitHubPullRequest(observed, request.Repository)
	if err != nil || result.Number != request.Number {
		return PullRequestObservation{}, ErrProviderMismatch
	}
	return result, nil
}

func (p GitHubProvider) ResolveTargetHead(ctx context.Context, repository Repository, targetRef string) (TargetHeadObservation, error) {
	if repository.Validate() != nil || !validRef(targetRef) {
		return TargetHeadObservation{}, ErrInvalid
	}
	authorization, token, err := p.authorize(ctx, repository)
	if err != nil {
		return TargetHeadObservation{}, err
	}
	resolved, err := p.Client.ResolveRemoteRef(ctx, token, authorization.Repository, targetRef)
	if err != nil {
		return TargetHeadObservation{}, err
	}
	if resolved.Ref != targetRef || !commitPattern.MatchString(resolved.CommitSHA) || resolved.ResolvedAt.IsZero() || resolved.ResolvedAt.After(p.now()) {
		return TargetHeadObservation{}, ErrProviderMismatch
	}
	return TargetHeadObservation{Repository: repository, TargetRef: targetRef, Revision: resolved.CommitSHA, ObservedAt: resolved.ResolvedAt.UTC()}, nil
}

func (p GitHubProvider) IsAncestor(ctx context.Context, repository Repository, ancestor, descendant string) (bool, error) {
	if repository.Validate() != nil || !commitPattern.MatchString(ancestor) || !commitPattern.MatchString(descendant) {
		return false, ErrInvalid
	}
	authorization, token, err := p.authorize(ctx, repository)
	if err != nil {
		return false, err
	}
	return p.Client.IsCommitAncestor(ctx, token, authorization.Repository, ancestor, descendant)
}

func (p GitHubProvider) authorize(ctx context.Context, repository Repository) (GitHubAuthorization, githubapp.InstallationToken, error) {
	if p.AppID <= 0 || p.Authorizations == nil || p.Client == nil || repository.Validate() != nil {
		return GitHubAuthorization{}, githubapp.InstallationToken{}, ErrInvalid
	}
	authorization, err := p.Authorizations.GitHubPublicationAuthorization(ctx, repository, p.AppID)
	if err != nil {
		return GitHubAuthorization{}, githubapp.InstallationToken{}, err
	}
	if authorization.Account.ID <= 0 || authorization.Repository.ID != repository.ID ||
		authorization.Repository.Name != repository.Name || !strings.EqualFold(authorization.Repository.OwnerLogin, repository.Owner) ||
		authorization.Repository.OwnerID != authorization.Account.ID || !strings.EqualFold(authorization.Account.Login, repository.Owner) ||
		(authorization.Account.Type != "User" && authorization.Account.Type != "Organization") {
		return GitHubAuthorization{}, githubapp.InstallationToken{}, ErrProviderMismatch
	}
	required := githubapp.Permissions{
		"metadata": githubapp.PermissionRead, "contents": githubapp.PermissionWrite, "pull_requests": githubapp.PermissionWrite,
	}
	installation, err := p.Client.VerifyInstallation(ctx, repository.InstallationID, authorization.Account, required)
	if err != nil {
		return GitHubAuthorization{}, githubapp.InstallationToken{}, err
	}
	if installation.ID != repository.InstallationID || installation.AppID != p.AppID || installation.SuspendedAt != nil ||
		installation.Account.ID != authorization.Account.ID || !strings.EqualFold(installation.Account.Login, authorization.Account.Login) ||
		installation.Account.Type != authorization.Account.Type || installation.Permissions["metadata"] != githubapp.PermissionRead ||
		installation.Permissions["contents"] != githubapp.PermissionWrite || installation.Permissions["pull_requests"] != githubapp.PermissionWrite {
		return GitHubAuthorization{}, githubapp.InstallationToken{}, ErrProviderMismatch
	}
	token, err := p.Client.MintInstallationToken(ctx, githubapp.TokenRequest{
		InstallationID: repository.InstallationID, Account: authorization.Account,
		Repositories: []githubapp.RepositoryIdentity{authorization.Repository}, Permissions: required,
	})
	if err != nil {
		return GitHubAuthorization{}, githubapp.InstallationToken{}, err
	}
	return authorization, token, nil
}

func validCreateRequestText(request CreatePullRequestRequest) bool {
	operationID := strings.TrimPrefix(request.HeadRef, "refs/heads/kuberploy/operations/")
	title, err := PullRequestTitle(operationID)
	if err != nil || request.Title != title {
		return false
	}
	lines := strings.Split(request.Body, "\n")
	return len(lines) == 4 && lines[0] == "Kuberploy-Operation: "+operationID &&
		strings.HasPrefix(lines[1], "Kuberploy-Binding: ") && uuidPattern.MatchString(strings.TrimPrefix(lines[1], "Kuberploy-Binding: ")) &&
		lines[2] == "Kuberploy-Candidate: "+request.HeadSHA && lines[3] == ""
}

func mapGitHubPullRequest(observed githubapp.PullRequest, repository Repository) (PullRequestObservation, error) {
	if observed.Repository.ID != repository.ID || observed.Repository.Name != repository.Name ||
		!strings.EqualFold(observed.Repository.OwnerLogin, repository.Owner) {
		return PullRequestObservation{}, ErrProviderMismatch
	}
	state := PullRequestState(observed.State)
	result := PullRequestObservation{Repository: repository, Number: observed.Number, URL: observed.URL,
		TargetRef: observed.TargetRef, HeadRef: observed.HeadRef, HeadRevision: observed.HeadRevision,
		State: state, Merged: observed.Merged, MergeRevision: observed.MergeRevision, ObservedAt: observed.ObservedAt.UTC()}
	if state != PullRequestOpen && state != PullRequestClosed {
		return PullRequestObservation{}, ErrProviderMismatch
	}
	return result, nil
}

var _ Provider = GitHubProvider{}
