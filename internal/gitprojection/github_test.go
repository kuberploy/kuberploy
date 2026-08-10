package gitprojection

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/githubapp"
)

type githubAuthorizationStoreStub struct {
	authorization GitHubAuthorization
	err           error
	calls         int
}

func (s *githubAuthorizationStoreStub) GitHubAuthorization(_ context.Context, _ Binding, _ int64) (GitHubAuthorization, error) {
	s.calls++
	return s.authorization, s.err
}

type githubHeadClientStub struct {
	installation   githubapp.Installation
	resolved       githubapp.ResolvedRef
	verifyErr      error
	mintErr        error
	resolveErr     error
	verifyCalls    int
	mintCalls      int
	resolveCalls   int
	installationID int64
	account        githubapp.AccountIdentity
	required       githubapp.Permissions
	request        githubapp.TokenRequest
	repository     githubapp.RepositoryIdentity
	ref            string
}

type githubGitClientStub struct {
	installation githubapp.Installation
	credential   GitCredential
	verifyErr    error
	mintErr      error
	verifyCalls  int
	mintCalls    int
	request      githubapp.TokenRequest
}

func (c *githubGitClientStub) VerifyInstallation(_ context.Context, _ int64, _ githubapp.AccountIdentity, _ githubapp.Permissions) (githubapp.Installation, error) {
	c.verifyCalls++
	return c.installation, c.verifyErr
}

func (c *githubGitClientStub) MintGitCredential(_ context.Context, request githubapp.TokenRequest) (GitCredential, error) {
	c.mintCalls++
	c.request = request
	return c.credential, c.mintErr
}

func (c *githubHeadClientStub) VerifyInstallation(_ context.Context, installationID int64, account githubapp.AccountIdentity, required githubapp.Permissions) (githubapp.Installation, error) {
	c.verifyCalls++
	c.installationID, c.account, c.required = installationID, account, required
	return c.installation, c.verifyErr
}

func (c *githubHeadClientStub) MintInstallationToken(_ context.Context, request githubapp.TokenRequest) (githubapp.InstallationToken, error) {
	c.mintCalls++
	c.request = request
	return githubapp.InstallationToken{}, c.mintErr
}

func (c *githubHeadClientStub) ResolveRemoteRef(_ context.Context, _ githubapp.InstallationToken, repository githubapp.RepositoryIdentity, ref string) (githubapp.ResolvedRef, error) {
	c.resolveCalls++
	c.repository, c.ref = repository, ref
	return c.resolved, c.resolveErr
}

func TestGitHubHeadVerifierUsesExactStoredIdentityRepositoryAndRef(t *testing.T) {
	now := time.Now().UTC()
	binding := githubProjectionBinding(t, now)
	authorization := githubProjectionAuthorization(binding)
	store := &githubAuthorizationStoreStub{authorization: authorization}
	client := &githubHeadClientStub{
		installation: githubapp.Installation{
			ID: binding.Repository.InstallationID, AppID: 12345, Account: authorization.Account,
			RepositorySelection: "selected", Permissions: githubapp.Permissions{"metadata": githubapp.PermissionRead, "contents": githubapp.PermissionWrite},
		},
		resolved: githubapp.ResolvedRef{Ref: binding.TargetRef, CommitSHA: strings.Repeat("a", 40), ResolvedAt: now.Add(time.Second)},
	}
	verifier := GitHubHeadVerifier{AppID: 12345, Authorizations: store, Client: client, RequestID: func() (string, error) { return "poll-request-123", nil }}
	head, err := verifier.VerifyTargetHead(t.Context(), binding, ObservationPoll)
	if err != nil {
		t.Fatal(err)
	}
	if head.BindingID != binding.ID || head.Repository != binding.Repository || head.TargetRef != binding.TargetRef || head.Commit != strings.Repeat("a", 40) || head.ProviderRequest != "poll-request-123" || !head.ObservedAt.Equal(now.Add(time.Second)) {
		t.Fatalf("head=%#v", head)
	}
	wantPermissions := githubapp.Permissions{"metadata": githubapp.PermissionRead, "contents": githubapp.PermissionRead}
	if store.calls != 1 || client.verifyCalls != 1 || client.mintCalls != 1 || client.resolveCalls != 1 || client.installationID != binding.Repository.InstallationID || client.account != authorization.Account ||
		!reflect.DeepEqual(client.required, wantPermissions) || client.request.InstallationID != binding.Repository.InstallationID || client.request.Account != authorization.Account ||
		!reflect.DeepEqual(client.request.Repositories, []githubapp.RepositoryIdentity{authorization.Repository}) || !reflect.DeepEqual(client.request.Permissions, wantPermissions) ||
		client.repository != authorization.Repository || client.ref != binding.TargetRef {
		t.Fatalf("calls store=%d client=%#v", store.calls, client)
	}
}

func TestGitHubHeadVerifierFailsClosedBeforeTokenMintOnIdentityMismatch(t *testing.T) {
	now := time.Now().UTC()
	binding := githubProjectionBinding(t, now)
	authorization := githubProjectionAuthorization(binding)
	authorization.Repository.ID++
	store := &githubAuthorizationStoreStub{authorization: authorization}
	client := &githubHeadClientStub{}
	verifier := GitHubHeadVerifier{AppID: 12345, Authorizations: store, Client: client}
	if _, err := verifier.VerifyTargetHead(t.Context(), binding, ObservationPoll); !errors.Is(err, ErrProviderMismatch) {
		t.Fatalf("error=%v", err)
	}
	if client.verifyCalls != 0 || client.mintCalls != 0 || client.resolveCalls != 0 {
		t.Fatalf("provider called after stored identity mismatch: %#v", client)
	}

	databaseErr := errors.New("database unavailable")
	store.authorization, store.err = githubProjectionAuthorization(binding), databaseErr
	if _, err := verifier.VerifyTargetHead(t.Context(), binding, ObservationPoll); !errors.Is(err, databaseErr) {
		t.Fatalf("database failure hidden as provider mismatch: %v", err)
	}
}

func TestGitHubHeadVerifierRejectsLegacyBindingBeforeStoreOrProviderCalls(t *testing.T) {
	now := time.Now().UTC()
	binding, err := NewEnvironmentBinding(
		"11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333",
		RepositoryIdentity{Provider: "github", InstallationID: 77, RepositoryID: 88, Owner: "kuberploy", Name: "environment"},
		"refs/heads/main", "legacy-secret", now,
	)
	if err != nil {
		t.Fatal(err)
	}
	store := &githubAuthorizationStoreStub{authorization: githubProjectionAuthorization(binding)}
	client := &githubHeadClientStub{}
	verifier := GitHubHeadVerifier{AppID: 12345, Authorizations: store, Client: client}
	if _, err = verifier.VerifyTargetHead(t.Context(), binding, ObservationPoll); !errors.Is(err, ErrInvalid) {
		t.Fatalf("legacy binding error=%v", err)
	}
	if store.calls != 0 || client.verifyCalls != 0 || client.mintCalls != 0 || client.resolveCalls != 0 {
		t.Fatalf("legacy binding reached store/provider: store=%d client=%#v", store.calls, client)
	}
}

func TestGitHubHeadVerifierRejectsProviderMutationAndMapsDeletedRef(t *testing.T) {
	now := time.Now().UTC()
	binding := githubProjectionBinding(t, now)
	authorization := githubProjectionAuthorization(binding)
	validInstallation := githubapp.Installation{
		ID: binding.Repository.InstallationID, AppID: 12345, Account: authorization.Account,
		RepositorySelection: "selected", Permissions: githubapp.Permissions{"metadata": githubapp.PermissionRead, "contents": githubapp.PermissionRead},
	}
	for name, mutate := range map[string]func(*githubHeadClientStub){
		"installation id":        func(c *githubHeadClientStub) { c.installation.ID++ },
		"installation app":       func(c *githubHeadClientStub) { c.installation.AppID++ },
		"installation owner":     func(c *githubHeadClientStub) { c.installation.Account.ID++ },
		"installation scope":     func(c *githubHeadClientStub) { delete(c.installation.Permissions, "contents") },
		"installation selection": func(c *githubHeadClientStub) { c.installation.RepositorySelection = "unknown" },
		"resolved ref":           func(c *githubHeadClientStub) { c.resolved.Ref = "refs/heads/other" },
		"resolved sha":           func(c *githubHeadClientStub) { c.resolved.CommitSHA = strings.Repeat("z", 40) },
		"resolved time":          func(c *githubHeadClientStub) { c.resolved.ResolvedAt = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			client := &githubHeadClientStub{installation: validInstallation, resolved: githubapp.ResolvedRef{Ref: binding.TargetRef, CommitSHA: strings.Repeat("a", 40), ResolvedAt: now}}
			client.installation.Permissions = githubapp.Permissions{"metadata": githubapp.PermissionRead, "contents": githubapp.PermissionRead}
			mutate(client)
			verifier := GitHubHeadVerifier{AppID: 12345, Authorizations: &githubAuthorizationStoreStub{authorization: authorization}, Client: client, RequestID: func() (string, error) { return "poll-request-123", nil }}
			if _, err := verifier.VerifyTargetHead(t.Context(), binding, ObservationPoll); !errors.Is(err, ErrProviderMismatch) {
				t.Fatalf("error=%v", err)
			}
		})
	}

	for name, providerErr := range map[string]error{
		"deleted":   githubapp.ErrRefDeleted,
		"not found": &githubapp.APIError{StatusCode: 404, Class: githubapp.APIErrorNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			client := &githubHeadClientStub{installation: validInstallation, resolveErr: providerErr}
			verifier := GitHubHeadVerifier{AppID: 12345, Authorizations: &githubAuthorizationStoreStub{authorization: authorization}, Client: client}
			if _, err := verifier.VerifyTargetHead(t.Context(), binding, ObservationPoll); !errors.Is(err, ErrMissingRef) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestGitHubHeadVerifierRejectsUnsafeRequestIdentity(t *testing.T) {
	now := time.Now().UTC()
	binding := githubProjectionBinding(t, now)
	authorization := githubProjectionAuthorization(binding)
	client := &githubHeadClientStub{
		installation: githubapp.Installation{ID: binding.Repository.InstallationID, AppID: 12345, Account: authorization.Account, RepositorySelection: "selected", Permissions: githubapp.Permissions{"metadata": githubapp.PermissionRead, "contents": githubapp.PermissionRead}},
		resolved:     githubapp.ResolvedRef{Ref: binding.TargetRef, CommitSHA: strings.Repeat("a", 40), ResolvedAt: now},
	}
	for _, requestID := range []string{"short", " contains-space", strings.Repeat("a", 129), "line\nbreak"} {
		verifier := GitHubHeadVerifier{AppID: 12345, Authorizations: &githubAuthorizationStoreStub{authorization: authorization}, Client: client, RequestID: func() (string, error) { return requestID, nil }}
		if _, err := verifier.VerifyTargetHead(t.Context(), binding, ObservationPoll); !errors.Is(err, ErrProviderMismatch) {
			t.Fatalf("request id %q accepted: %v", requestID, err)
		}
	}
}

func TestGitHubGitCredentialProviderMintsExactRepositoryScopedReadToken(t *testing.T) {
	now := time.Now().UTC()
	binding := githubProjectionBinding(t, now)
	authorization := githubProjectionAuthorization(binding)
	client := &githubGitClientStub{
		installation: githubapp.Installation{ID: binding.Repository.InstallationID, AppID: 12345, Account: authorization.Account,
			RepositorySelection: "selected", Permissions: githubapp.Permissions{"metadata": githubapp.PermissionRead, "contents": githubapp.PermissionRead}},
		credential: GitCredential{Username: []byte(gitHubAppUsername), Password: []byte(strings.Repeat("t", 40)), ExpiresAt: now.Add(time.Hour)},
	}
	provider := GitHubGitCredentialProvider{AppID: 12345, Authorizations: &githubAuthorizationStoreStub{authorization: authorization}, Client: client, Now: func() time.Time { return now }}
	credential, err := provider.AcquireGitCredential(t.Context(), binding)
	if err != nil {
		t.Fatal(err)
	}
	if string(credential.Username) != gitHubAppUsername || string(credential.Password) != strings.Repeat("t", 40) || client.verifyCalls != 1 || client.mintCalls != 1 ||
		client.request.InstallationID != binding.Repository.InstallationID || client.request.Account != authorization.Account ||
		!reflect.DeepEqual(client.request.Repositories, []githubapp.RepositoryIdentity{authorization.Repository}) ||
		!reflect.DeepEqual(client.request.Permissions, githubapp.Permissions{"metadata": githubapp.PermissionRead, "contents": githubapp.PermissionRead}) {
		t.Fatalf("credential username=%q passwordBytes=%d calls=%d/%d request=%#v", credential.Username, len(credential.Password), client.verifyCalls, client.mintCalls, client.request)
	}
	credential.clear()
	if credential.Username != nil || credential.Password != nil {
		t.Fatal("credential was not cleared")
	}
}

func TestGitHubGitCredentialProviderFailsClosedBeforeMintAndRejectsUnsafeCredential(t *testing.T) {
	now := time.Now().UTC()
	binding := githubProjectionBinding(t, now)
	authorization := githubProjectionAuthorization(binding)
	validInstallation := githubapp.Installation{ID: binding.Repository.InstallationID, AppID: 12345, Account: authorization.Account,
		RepositorySelection: "selected", Permissions: githubapp.Permissions{"metadata": githubapp.PermissionRead, "contents": githubapp.PermissionRead}}
	client := &githubGitClientStub{installation: validInstallation, credential: GitCredential{Username: []byte("attacker"), Password: []byte(strings.Repeat("t", 40)), ExpiresAt: now.Add(time.Hour)}}
	provider := GitHubGitCredentialProvider{AppID: 12345, Authorizations: &githubAuthorizationStoreStub{authorization: authorization}, Client: client, Now: func() time.Time { return now }}
	unsafePassword := client.credential.Password
	if _, err := provider.AcquireGitCredential(t.Context(), binding); !errors.Is(err, ErrProviderMismatch) {
		t.Fatalf("unsafe credential error=%v", err)
	}
	if strings.Trim(string(unsafePassword), "\x00") != "" {
		t.Fatal("rejected credential bytes were not cleared")
	}

	client.credential = GitCredential{Username: []byte(gitHubAppUsername), Password: []byte(strings.Repeat("t", 40)), ExpiresAt: now.Add(time.Hour)}
	client.installation.AppID++
	client.mintCalls = 0
	if _, err := provider.AcquireGitCredential(t.Context(), binding); !errors.Is(err, ErrProviderMismatch) || client.mintCalls != 0 {
		t.Fatalf("mutated installation error=%v mintCalls=%d", err, client.mintCalls)
	}

	store := &githubAuthorizationStoreStub{authorization: authorization}
	store.authorization.Repository.ID++
	client.installation = validInstallation
	client.verifyCalls, client.mintCalls = 0, 0
	provider.Authorizations = store
	if _, err := provider.AcquireGitCredential(t.Context(), binding); !errors.Is(err, ErrProviderMismatch) || client.verifyCalls != 0 || client.mintCalls != 0 {
		t.Fatalf("substituted repository error=%v calls=%d/%d", err, client.verifyCalls, client.mintCalls)
	}
}

func githubProjectionBinding(t *testing.T, now time.Time) Binding {
	t.Helper()
	binding, err := NewGitHubEnvironmentBinding(
		"11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333",
		RepositoryIdentity{Provider: "github", InstallationID: 77, RepositoryID: 88, Owner: "Kuberploy", Name: "environment"},
		"refs/heads/main", now,
	)
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

func githubProjectionAuthorization(binding Binding) GitHubAuthorization {
	return GitHubAuthorization{
		Account:    githubapp.AccountIdentity{ID: 99, Login: "kuberploy", Type: "Organization"},
		Repository: githubapp.RepositoryIdentity{ID: binding.Repository.RepositoryID, OwnerID: 99, OwnerLogin: "Kuberploy", Name: binding.Repository.Name},
	}
}
