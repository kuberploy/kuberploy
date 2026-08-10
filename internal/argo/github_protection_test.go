package argo

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/githubapp"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

type protectionAuthorizationStore struct {
	authorization gitprojection.GitHubAuthorization
	err           error
	calls         int
}

func (s *protectionAuthorizationStore) GitHubAuthorization(_ context.Context, _ gitprojection.Binding, _ int64) (gitprojection.GitHubAuthorization, error) {
	s.calls++
	return s.authorization, s.err
}

type protectionProviderStub struct {
	installation githubapp.Installation
	observation  githubapp.RepositoryProtectionObservation
	verifyErr    error
	mintErr      error
	observeErr   error
	verifyCalls  int
	mintCalls    int
	observeCalls int
	permissions  githubapp.Permissions
	request      githubapp.TokenRequest
	repository   githubapp.RepositoryIdentity
	ref          string
	head         string
	appID        int64
}

func (p *protectionProviderStub) VerifyInstallation(_ context.Context, _ int64, _ githubapp.AccountIdentity, permissions githubapp.Permissions) (githubapp.Installation, error) {
	p.verifyCalls++
	p.permissions = permissions
	return p.installation, p.verifyErr
}

func (p *protectionProviderStub) MintInstallationToken(_ context.Context, request githubapp.TokenRequest) (githubapp.InstallationToken, error) {
	p.mintCalls++
	p.request = request
	return githubapp.InstallationToken{}, p.mintErr
}

func (p *protectionProviderStub) ObserveRepositoryProtection(_ context.Context, _ githubapp.InstallationToken, repository githubapp.RepositoryIdentity, ref, head string, appID int64) (githubapp.RepositoryProtectionObservation, error) {
	p.observeCalls++
	p.repository, p.ref, p.head, p.appID = repository, ref, head, appID
	return p.observation, p.observeErr
}

func protectionAdapterFixture(t *testing.T) (gitprojection.Binding, gitprojection.VerifiedHead, *protectionAuthorizationStore, *protectionProviderStub, GitHubPlatformRepositoryProtectionVerifier, time.Time) {
	t.Helper()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	repository := gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 77, RepositoryID: 88, Owner: "kuberploy", Name: "platform"}
	binding, err := gitprojection.NewGitHubPlatformBinding("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", repository, "refs/heads/platform", now)
	if err != nil {
		t.Fatal(err)
	}
	binding.TargetHeadRevision = "0123456789abcdef0123456789abcdef01234567"
	binding.TargetHeadObservedAt = now
	binding.State = gitprojection.BindingIndexing
	head := gitprojection.VerifiedHead{BindingID: binding.ID, Repository: binding.Repository, TargetRef: binding.TargetRef,
		Commit: binding.TargetHeadRevision, Source: gitprojection.ObservationPoll, ProviderRequest: "provider-request", ObservedAt: now}
	authorization := gitprojection.GitHubAuthorization{
		Account:    githubapp.AccountIdentity{ID: 99, Login: "kuberploy", Type: "Organization"},
		Repository: githubapp.RepositoryIdentity{ID: 88, OwnerID: 99, OwnerLogin: "kuberploy", Name: "platform"},
	}
	store := &protectionAuthorizationStore{authorization: authorization}
	provider := &protectionProviderStub{
		installation: githubapp.Installation{ID: 77, AppID: 12345, Account: authorization.Account, RepositorySelection: "selected",
			Permissions: githubapp.Permissions{"metadata": githubapp.PermissionRead, "contents": githubapp.PermissionWrite, "administration": githubapp.PermissionRead}},
		observation: githubapp.RepositoryProtectionObservation{InstallationID: 77, RepositoryID: 88, Ref: binding.TargetRef,
			Head: binding.TargetHeadRevision, WriterAppID: 12345, PolicyDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ObservedAt: now.Add(time.Second)},
	}
	verifier := GitHubPlatformRepositoryProtectionVerifier{AppID: 12345, Authorizations: store, Client: provider, Now: func() time.Time { return now.Add(2 * time.Second) }}
	return binding, head, store, provider, verifier, now
}

func TestGitHubPlatformRepositoryProtectionVerifierUsesExactReadOnlyRepositoryScope(t *testing.T) {
	binding, head, _, provider, verifier, now := protectionAdapterFixture(t)
	result, err := verifier.VerifyPlatformRepositoryProtection(t.Context(), binding, head, now)
	if err != nil {
		t.Fatal(err)
	}
	wantPermissions := githubapp.Permissions{"metadata": githubapp.PermissionRead, "contents": githubapp.PermissionRead, "administration": githubapp.PermissionRead}
	if !reflect.DeepEqual(provider.permissions, wantPermissions) || !reflect.DeepEqual(provider.request.Permissions, wantPermissions) ||
		provider.request.InstallationID != 77 || len(provider.request.Repositories) != 1 || provider.request.Repositories[0].ID != 88 ||
		provider.repository.ID != 88 || provider.ref != binding.TargetRef || provider.head != head.Commit || provider.appID != 12345 {
		t.Fatalf("provider scope mismatch: verify=%v request=%+v repository=%+v ref=%q head=%q app=%d", provider.permissions, provider.request, provider.repository, provider.ref, provider.head, provider.appID)
	}
	if result.BindingID != binding.ID || result.TargetRef != binding.TargetRef || result.Head != head.Commit ||
		result.PolicyDigest != provider.observation.PolicyDigest || !result.ObservedAt.Equal(now) {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestGitHubPlatformRepositoryProtectionVerifierRejectsInvalidBoundaryBeforeProvider(t *testing.T) {
	binding, head, store, provider, verifier, now := protectionAdapterFixture(t)
	binding.CredentialMode = gitprojection.CredentialLegacySecret
	if _, err := verifier.VerifyPlatformRepositoryProtection(t.Context(), binding, head, now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("legacy binding error=%v", err)
	}
	if store.calls != 0 || provider.verifyCalls != 0 || provider.mintCalls != 0 || provider.observeCalls != 0 {
		t.Fatalf("invalid boundary reached provider: store=%d verify=%d mint=%d observe=%d", store.calls, provider.verifyCalls, provider.mintCalls, provider.observeCalls)
	}
}

func TestGitHubPlatformRepositoryProtectionVerifierRequiresExactAdministrationRead(t *testing.T) {
	binding, head, _, provider, verifier, now := protectionAdapterFixture(t)
	provider.installation.Permissions["administration"] = githubapp.PermissionWrite
	if _, err := verifier.VerifyPlatformRepositoryProtection(t.Context(), binding, head, now); !errors.Is(err, ErrArgoRuntimePrerequisiteNotReady) {
		t.Fatalf("administration write error=%v", err)
	}
	if provider.mintCalls != 0 || provider.observeCalls != 0 {
		t.Fatalf("overprivileged installation reached token/protection calls: mint=%d observe=%d", provider.mintCalls, provider.observeCalls)
	}
}

func TestGitHubPlatformRepositoryProtectionVerifierRejectsProviderIdentitySubstitution(t *testing.T) {
	binding, head, _, provider, verifier, now := protectionAdapterFixture(t)
	provider.observation.RepositoryID++
	if _, err := verifier.VerifyPlatformRepositoryProtection(t.Context(), binding, head, now); !errors.Is(err, ErrArgoRuntimePrerequisiteNotReady) {
		t.Fatalf("substitution error=%v", err)
	}
}

func TestGitHubPlatformRepositoryProtectionVerifierRejectsStaleOrFutureObservation(t *testing.T) {
	for name, offset := range map[string]time.Duration{"stale": -time.Second, "future": 3 * time.Second} {
		t.Run(name, func(t *testing.T) {
			binding, head, _, provider, verifier, now := protectionAdapterFixture(t)
			provider.observation.ObservedAt = now.Add(offset)
			if _, err := verifier.VerifyPlatformRepositoryProtection(t.Context(), binding, head, now); !errors.Is(err, ErrArgoRuntimePrerequisiteNotReady) {
				t.Fatalf("observation error=%v", err)
			}
		})
	}
}
