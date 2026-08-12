package gitpublication_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/githubapp"
	"github.com/kuberploy/kuberploy/internal/gitpublication"
)

type githubAuthorizationStub struct {
	authorization gitpublication.GitHubAuthorization
	err           error
	calls         int
}

func (s *githubAuthorizationStub) GitHubPublicationAuthorization(_ context.Context, _ gitpublication.Repository, _ int64) (gitpublication.GitHubAuthorization, error) {
	s.calls++
	return s.authorization, s.err
}

type githubClientStub struct {
	installation  githubapp.Installation
	verifyErr     error
	tokenErr      error
	pullRequest   githubapp.PullRequest
	pullErr       error
	found         bool
	resolved      githubapp.ResolvedRef
	ancestor      bool
	ancestorErr   error
	verifyCalls   int
	mintCalls     int
	providerCalls int
	tokenRequest  githubapp.TokenRequest
}

func (s *githubClientStub) VerifyInstallation(_ context.Context, _ int64, _ githubapp.AccountIdentity, _ githubapp.Permissions) (githubapp.Installation, error) {
	s.verifyCalls++
	return s.installation, s.verifyErr
}

func (s *githubClientStub) MintInstallationToken(_ context.Context, request githubapp.TokenRequest) (githubapp.InstallationToken, error) {
	s.mintCalls++
	s.tokenRequest = request
	return githubapp.InstallationToken{}, s.tokenErr
}

func (s *githubClientStub) CreatePullRequest(_ context.Context, _ githubapp.InstallationToken, _ githubapp.RepositoryIdentity, _, _, _, _ string) (githubapp.PullRequest, error) {
	s.providerCalls++
	return s.pullRequest, s.pullErr
}

func (s *githubClientStub) FindPullRequest(_ context.Context, _ githubapp.InstallationToken, _ githubapp.RepositoryIdentity, _, _ string) (githubapp.PullRequest, bool, error) {
	s.providerCalls++
	return s.pullRequest, s.found, s.pullErr
}

func (s *githubClientStub) GetPullRequest(_ context.Context, _ githubapp.InstallationToken, _ githubapp.RepositoryIdentity, _ int64) (githubapp.PullRequest, error) {
	s.providerCalls++
	return s.pullRequest, s.pullErr
}

func (s *githubClientStub) ResolveRemoteRef(_ context.Context, _ githubapp.InstallationToken, _ githubapp.RepositoryIdentity, _ string) (githubapp.ResolvedRef, error) {
	s.providerCalls++
	return s.resolved, s.pullErr
}

func (s *githubClientStub) IsCommitAncestor(_ context.Context, _ githubapp.InstallationToken, _ githubapp.RepositoryIdentity, _, _ string) (bool, error) {
	s.providerCalls++
	return s.ancestor, s.ancestorErr
}

func githubProviderFixture(t *testing.T) (gitpublication.GitHubProvider, *githubAuthorizationStub, *githubClientStub, gitpublication.Publication, time.Time) {
	t.Helper()
	publication, started := publicationFixture(t)
	publication, err := publication.WithWriteBase(baseSHA, started.Add(time.Minute))
	if err == nil {
		publication, err = publication.WithCandidate(candidateSHA, started.Add(2*time.Minute))
	}
	if err != nil {
		t.Fatal(err)
	}
	account := githubapp.AccountIdentity{ID: 61, Login: repository.Owner, Type: "Organization"}
	repositoryIdentity := githubapp.RepositoryIdentity{ID: repository.ID, Name: repository.Name, OwnerID: account.ID, OwnerLogin: account.Login}
	authorizations := &githubAuthorizationStub{authorization: gitpublication.GitHubAuthorization{Account: account, Repository: repositoryIdentity}}
	client := &githubClientStub{
		installation: githubapp.Installation{ID: repository.InstallationID, AppID: 71, Account: account,
			RepositorySelection: "selected", Permissions: githubapp.Permissions{
				"metadata": githubapp.PermissionRead, "contents": githubapp.PermissionWrite, "pull_requests": githubapp.PermissionWrite,
			}},
		pullRequest: githubapp.PullRequest{Repository: repositoryIdentity, Number: 7,
			URL: "https://github.com/kuberploy/platform/pull/7", TargetRef: publication.TargetRef,
			HeadRef: publication.CandidateRef, HeadRevision: candidateSHA, State: "open", ObservedAt: started.Add(90 * time.Second)},
	}
	provider := gitpublication.GitHubProvider{AppID: 71, Authorizations: authorizations, Client: client, Now: func() time.Time { return started.Add(2 * time.Minute) }}
	return provider, authorizations, client, publication, started
}

func TestGitHubProviderRejectsInvalidRequestBeforeAuthorization(t *testing.T) {
	provider, authorizations, client, publication, _ := githubProviderFixture(t)
	title, _ := gitpublication.PullRequestTitle(operationID)
	body, _ := gitpublication.PullRequestBody(publication)
	request := gitpublication.CreatePullRequestRequest{Repository: repository, TargetRef: publication.TargetRef,
		HeadRef: "refs/heads/tenant-selected", HeadSHA: candidateSHA, Title: title, Body: body}

	if _, err := provider.CreatePullRequest(t.Context(), request); !errors.Is(err, gitpublication.ErrInvalid) {
		t.Fatalf("invalid request error=%v", err)
	}
	if authorizations.calls != 0 || client.verifyCalls != 0 || client.mintCalls != 0 || client.providerCalls != 0 {
		t.Fatalf("invalid request reached authorization/provider: auth=%d verify=%d mint=%d provider=%d",
			authorizations.calls, client.verifyCalls, client.mintCalls, client.providerCalls)
	}
}

func TestGitHubProviderUsesExactRepositoryScopedWritePermissions(t *testing.T) {
	provider, _, client, publication, _ := githubProviderFixture(t)
	title, _ := gitpublication.PullRequestTitle(operationID)
	body, _ := gitpublication.PullRequestBody(publication)
	request := gitpublication.CreatePullRequestRequest{Repository: repository, TargetRef: publication.TargetRef,
		HeadRef: publication.CandidateRef, HeadSHA: candidateSHA, Title: title, Body: body}

	result, err := provider.CreatePullRequest(t.Context(), request)
	if err != nil || result.Number != 7 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if strings.TrimSpace(body) != body || strings.HasSuffix(body, "\n") {
		t.Fatalf("pull request body is incompatible with the bounded GitHub client: %q", body)
	}
	wantPermissions := githubapp.Permissions{
		"metadata": githubapp.PermissionRead, "contents": githubapp.PermissionWrite, "pull_requests": githubapp.PermissionWrite,
	}
	if client.tokenRequest.InstallationID != repository.InstallationID ||
		!reflect.DeepEqual(client.tokenRequest.Repositories, []githubapp.RepositoryIdentity{client.pullRequest.Repository}) ||
		!reflect.DeepEqual(client.tokenRequest.Permissions, wantPermissions) {
		t.Fatalf("token request=%#v", client.tokenRequest)
	}
}

func TestGitHubProviderRejectsSubstitutedProviderResponse(t *testing.T) {
	provider, _, client, publication, _ := githubProviderFixture(t)
	client.pullRequest.HeadRevision = targetSHA
	title, _ := gitpublication.PullRequestTitle(operationID)
	body, _ := gitpublication.PullRequestBody(publication)
	request := gitpublication.CreatePullRequestRequest{Repository: repository, TargetRef: publication.TargetRef,
		HeadRef: publication.CandidateRef, HeadSHA: candidateSHA, Title: title, Body: body}

	if _, err := provider.CreatePullRequest(t.Context(), request); !errors.Is(err, gitpublication.ErrProviderMismatch) {
		t.Fatalf("substituted SHA accepted: %v", err)
	}
}

func TestGitHubProviderRejectsAuthorizationSubstitutionBeforeTokenMint(t *testing.T) {
	provider, authorizations, client, publication, _ := githubProviderFixture(t)
	authorizations.authorization.Repository.ID++
	request := gitpublication.FindPullRequestRequest{Repository: repository, TargetRef: publication.TargetRef,
		HeadRef: publication.CandidateRef, HeadSHA: candidateSHA}

	if _, _, err := provider.FindPullRequest(t.Context(), request); !errors.Is(err, gitpublication.ErrProviderMismatch) {
		t.Fatalf("substituted authorization accepted: %v", err)
	}
	if client.verifyCalls != 0 || client.mintCalls != 0 || client.providerCalls != 0 {
		t.Fatalf("substituted authorization reached provider: verify=%d mint=%d provider=%d", client.verifyCalls, client.mintCalls, client.providerCalls)
	}
}
