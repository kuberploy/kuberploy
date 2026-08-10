package gitprojection

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/githubapp"
)

type GitHubAuthorization struct {
	Account    githubapp.AccountIdentity
	Repository githubapp.RepositoryIdentity
}

func (a GitHubAuthorization) ValidateFor(binding Binding) error {
	if binding.Validate() != nil || a.Account.ID <= 0 || !loginRE.MatchString(a.Account.Login) ||
		(a.Account.Type != "User" && a.Account.Type != "Organization") || a.Repository.ID != binding.Repository.RepositoryID ||
		a.Repository.OwnerID != a.Account.ID || a.Repository.OwnerID <= 0 || a.Repository.Name != binding.Repository.Name ||
		!strings.EqualFold(a.Repository.OwnerLogin, binding.Repository.Owner) || !strings.EqualFold(a.Account.Login, binding.Repository.Owner) ||
		!nameRE.MatchString(a.Repository.Name) || strings.EqualFold(a.Repository.Name, ".git") || strings.HasSuffix(strings.ToLower(a.Repository.Name), ".git") {
		return ErrProviderMismatch
	}
	return nil
}

type GitHubAuthorizationStore interface {
	GitHubAuthorization(context.Context, Binding, int64) (GitHubAuthorization, error)
}

type GitHubHeadClient interface {
	VerifyInstallation(context.Context, int64, githubapp.AccountIdentity, githubapp.Permissions) (githubapp.Installation, error)
	MintInstallationToken(context.Context, githubapp.TokenRequest) (githubapp.InstallationToken, error)
	ResolveRemoteRef(context.Context, githubapp.InstallationToken, githubapp.RepositoryIdentity, string) (githubapp.ResolvedRef, error)
}

// GitHubGitClient is deliberately narrower than the full provider client. Its
// mint operation must return a credential derived from a token that was scoped
// by repository ID and exact read permissions.
type GitHubGitClient interface {
	VerifyInstallation(context.Context, int64, githubapp.AccountIdentity, githubapp.Permissions) (githubapp.Installation, error)
	MintGitCredential(context.Context, githubapp.TokenRequest) (GitCredential, error)
}

type GitHubGitClientAdapter struct {
	Client interface {
		VerifyInstallation(context.Context, int64, githubapp.AccountIdentity, githubapp.Permissions) (githubapp.Installation, error)
		MintInstallationToken(context.Context, githubapp.TokenRequest) (githubapp.InstallationToken, error)
	}
}

func (a GitHubGitClientAdapter) VerifyInstallation(ctx context.Context, installationID int64, account githubapp.AccountIdentity, required githubapp.Permissions) (githubapp.Installation, error) {
	if a.Client == nil {
		return githubapp.Installation{}, ErrInvalid
	}
	return a.Client.VerifyInstallation(ctx, installationID, account, required)
}

func (a GitHubGitClientAdapter) MintGitCredential(ctx context.Context, request githubapp.TokenRequest) (GitCredential, error) {
	if a.Client == nil {
		return GitCredential{}, ErrInvalid
	}
	token, err := a.Client.MintInstallationToken(ctx, request)
	if err != nil {
		return GitCredential{}, err
	}
	raw := token.Authorization().Reveal()
	credential := GitCredential{Username: []byte(gitHubAppUsername), Password: []byte(raw), ExpiresAt: token.ExpiresAt.UTC()}
	if credential.validate(time.Now().UTC()) != nil {
		credential.clear()
		return GitCredential{}, ErrProviderMismatch
	}
	return credential, nil
}

// GitHubGitCredentialProvider re-authorizes the stored installation and
// repository immediately before Git transport, then asks GitHub for one
// repository-scoped token. Read is the default; the deployment writer sets
// Write=true and receives exact contents:write without any broader scope. The
// token is returned only as clearable bytes for the in-memory askpass broker.
type GitHubGitCredentialProvider struct {
	AppID          int64
	Authorizations GitHubAuthorizationStore
	Client         GitHubGitClient
	Write          bool
	Now            func() time.Time
}

func (p GitHubGitCredentialProvider) AcquireGitCredential(ctx context.Context, binding Binding) (GitCredential, error) {
	if p.AppID <= 0 || p.Authorizations == nil || p.Client == nil || binding.Validate() != nil || binding.CredentialMode != CredentialGitHubApp {
		return GitCredential{}, ErrInvalid
	}
	authorization, err := p.Authorizations.GitHubAuthorization(ctx, binding, p.AppID)
	if err != nil {
		return GitCredential{}, err
	}
	if authorization.ValidateFor(binding) != nil {
		return GitCredential{}, ErrProviderMismatch
	}
	required := githubapp.Permissions{"metadata": githubapp.PermissionRead, "contents": githubapp.PermissionRead}
	if p.Write {
		required["contents"] = githubapp.PermissionWrite
	}
	installation, err := p.Client.VerifyInstallation(ctx, binding.Repository.InstallationID, authorization.Account, required)
	if err != nil {
		return GitCredential{}, err
	}
	if !validProjectionInstallation(installation, binding, authorization, p.AppID) {
		return GitCredential{}, ErrProviderMismatch
	}
	credential, err := p.Client.MintGitCredential(ctx, githubapp.TokenRequest{
		InstallationID: binding.Repository.InstallationID,
		Account:        authorization.Account,
		Repositories:   []githubapp.RepositoryIdentity{authorization.Repository},
		Permissions:    required,
	})
	if err != nil {
		return GitCredential{}, err
	}
	now := time.Now().UTC()
	if p.Now != nil {
		now = p.Now().UTC()
	}
	if credential.validate(now) != nil {
		credential.clear()
		return GitCredential{}, ErrProviderMismatch
	}
	return credential, nil
}

// GitHubHeadVerifier re-authorizes the stored installation and repository,
// mints a token scoped to exactly that repository, and resolves only the exact
// fully-qualified ref. No webhook SHA or tenant-provided URL is consulted.
type GitHubHeadVerifier struct {
	AppID          int64
	Authorizations GitHubAuthorizationStore
	Client         GitHubHeadClient
	RequestID      func() (string, error)
}

func (v GitHubHeadVerifier) VerifyTargetHead(ctx context.Context, binding Binding, source ObservationSource) (VerifiedHead, error) {
	if v.AppID <= 0 || v.Authorizations == nil || v.Client == nil || binding.Validate() != nil ||
		binding.CredentialMode != CredentialGitHubApp ||
		(source != ObservationWebhook && source != ObservationPoll && source != ObservationWrite) {
		return VerifiedHead{}, ErrInvalid
	}
	authorization, err := v.Authorizations.GitHubAuthorization(ctx, binding, v.AppID)
	if err != nil {
		return VerifiedHead{}, err
	}
	if authorization.ValidateFor(binding) != nil {
		return VerifiedHead{}, ErrProviderMismatch
	}
	required := githubapp.Permissions{"metadata": githubapp.PermissionRead, "contents": githubapp.PermissionRead}
	installation, err := v.Client.VerifyInstallation(ctx, binding.Repository.InstallationID, authorization.Account, required)
	if err != nil {
		return VerifiedHead{}, err
	}
	if !validProjectionInstallation(installation, binding, authorization, v.AppID) {
		return VerifiedHead{}, ErrProviderMismatch
	}
	token, err := v.Client.MintInstallationToken(ctx, githubapp.TokenRequest{
		InstallationID: binding.Repository.InstallationID,
		Account:        authorization.Account,
		Repositories:   []githubapp.RepositoryIdentity{authorization.Repository},
		Permissions:    required,
	})
	if err != nil {
		return VerifiedHead{}, err
	}
	resolved, err := v.Client.ResolveRemoteRef(ctx, token, authorization.Repository, binding.TargetRef)
	if err != nil {
		var providerError *githubapp.APIError
		if errors.Is(err, githubapp.ErrRefDeleted) || errors.As(err, &providerError) && providerError.Class == githubapp.APIErrorNotFound {
			return VerifiedHead{}, ErrMissingRef
		}
		return VerifiedHead{}, err
	}
	if resolved.Ref != binding.TargetRef || !commitRE.MatchString(resolved.CommitSHA) || resolved.ResolvedAt.IsZero() {
		return VerifiedHead{}, ErrProviderMismatch
	}
	requestID, err := newProjectionRequestID(v.RequestID)
	if err != nil {
		return VerifiedHead{}, err
	}
	head := VerifiedHead{
		BindingID: binding.ID, Repository: binding.Repository, TargetRef: binding.TargetRef,
		Commit: resolved.CommitSHA, Source: source, ProviderRequest: requestID, ObservedAt: resolved.ResolvedAt.UTC(),
	}
	if head.ValidateFor(binding) != nil {
		return VerifiedHead{}, ErrProviderMismatch
	}
	return head, nil
}

func validProjectionInstallation(installation githubapp.Installation, binding Binding, authorization GitHubAuthorization, appID int64) bool {
	return installation.ID == binding.Repository.InstallationID && installation.AppID == appID && installation.SuspendedAt == nil &&
		(installation.RepositorySelection == "all" || installation.RepositorySelection == "selected") &&
		installation.Account.ID == authorization.Account.ID && strings.EqualFold(installation.Account.Login, authorization.Account.Login) && installation.Account.Type == authorization.Account.Type &&
		allowsProjectionRead(installation.Permissions["metadata"]) && allowsProjectionRead(installation.Permissions["contents"])
}

func allowsProjectionRead(level githubapp.PermissionLevel) bool {
	return level == githubapp.PermissionRead || level == githubapp.PermissionWrite
}

func newProjectionRequestID(source func() (string, error)) (string, error) {
	if source != nil {
		value, err := source()
		if err != nil || len(value) < 8 || len(value) > 128 || strings.TrimSpace(value) != value || strings.IndexFunc(value, func(r rune) bool { return r < 0x21 || r > 0x7e }) >= 0 {
			return "", ErrProviderMismatch
		}
		return value, nil
	}
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", ErrProviderMismatch
	}
	return "poll-" + hex.EncodeToString(value[:]), nil
}

var _ HeadVerifier = GitHubHeadVerifier{}
var _ GitCredentialProvider = GitHubGitCredentialProvider{}
var _ GitHubGitClient = GitHubGitClientAdapter{}
