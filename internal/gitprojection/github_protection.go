package gitprojection

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/githubapp"
)

type RepositoryProtectionObservation struct {
	BindingID, TargetRef, Head, PolicyDigest string
	ObservedAt                               time.Time
}

type RepositoryProtectionVerifier interface {
	VerifyRepositoryProtection(context.Context, Binding, VerifiedHead, time.Time) (RepositoryProtectionObservation, error)
}

type GitHubRepositoryProtectionClient interface {
	VerifyInstallation(context.Context, int64, githubapp.AccountIdentity, githubapp.Permissions) (githubapp.Installation, error)
	MintInstallationToken(context.Context, githubapp.TokenRequest) (githubapp.InstallationToken, error)
	ObserveRepositoryProtection(context.Context, githubapp.InstallationToken, githubapp.RepositoryIdentity, string, string, int64) (githubapp.RepositoryProtectionObservation, error)
}

type GitHubRepositoryProtectionVerifier struct {
	AppID          int64
	Authorizations GitHubAuthorizationStore
	Client         GitHubRepositoryProtectionClient
	Now            func() time.Time
}

func (v GitHubRepositoryProtectionVerifier) VerifyRepositoryProtection(ctx context.Context, binding Binding, head VerifiedHead, startedAt time.Time) (RepositoryProtectionObservation, error) {
	if v.AppID <= 0 || v.Authorizations == nil || v.Client == nil || startedAt.IsZero() || binding.Validate() != nil ||
		binding.Kind != BindingEnvironment || binding.CredentialMode != CredentialGitHubApp || head.ValidateFor(binding) != nil {
		return RepositoryProtectionObservation{}, ErrInvalid
	}
	authorization, err := v.Authorizations.GitHubAuthorization(ctx, binding, v.AppID)
	if err != nil || authorization.ValidateFor(binding) != nil {
		return RepositoryProtectionObservation{}, errors.Join(ErrProtectionUnavailable, err)
	}
	required := githubapp.Permissions{"metadata": githubapp.PermissionRead, "contents": githubapp.PermissionRead, "administration": githubapp.PermissionRead}
	installation, err := v.Client.VerifyInstallation(ctx, binding.Repository.InstallationID, authorization.Account, required)
	if err != nil || !validEnvironmentProtectionInstallation(installation, binding, authorization, v.AppID) {
		return RepositoryProtectionObservation{}, errors.Join(ErrProtectionUnavailable, err)
	}
	token, err := v.Client.MintInstallationToken(ctx, githubapp.TokenRequest{InstallationID: binding.Repository.InstallationID,
		Account: authorization.Account, Repositories: []githubapp.RepositoryIdentity{authorization.Repository}, Permissions: required})
	if err != nil {
		return RepositoryProtectionObservation{}, errors.Join(ErrProtectionUnavailable, err)
	}
	observed, err := v.Client.ObserveRepositoryProtection(ctx, token, authorization.Repository, binding.TargetRef, head.Commit, v.AppID)
	if err != nil {
		return RepositoryProtectionObservation{}, errors.Join(ErrProtectionUnavailable, err)
	}
	completedAt := time.Now().UTC()
	if v.Now != nil {
		completedAt = v.Now().UTC()
	}
	if observed.InstallationID != binding.Repository.InstallationID || observed.RepositoryID != binding.Repository.RepositoryID ||
		observed.Ref != binding.TargetRef || observed.Head != head.Commit || observed.WriterAppID != v.AppID ||
		!digestRE.MatchString(observed.PolicyDigest) || observed.ObservedAt.Before(startedAt.UTC()) || observed.ObservedAt.After(completedAt) {
		return RepositoryProtectionObservation{}, ErrProtectionUnavailable
	}
	return RepositoryProtectionObservation{BindingID: binding.ID, TargetRef: binding.TargetRef, Head: head.Commit,
		PolicyDigest: observed.PolicyDigest, ObservedAt: observed.ObservedAt.UTC()}, nil
}

func validEnvironmentProtectionInstallation(installation githubapp.Installation, binding Binding, authorization GitHubAuthorization, appID int64) bool {
	return installation.ID == binding.Repository.InstallationID && installation.AppID == appID && installation.SuspendedAt == nil &&
		(installation.RepositorySelection == "all" || installation.RepositorySelection == "selected") &&
		installation.Account.ID == authorization.Account.ID && strings.EqualFold(installation.Account.Login, authorization.Account.Login) &&
		installation.Account.Type == authorization.Account.Type &&
		(installation.Permissions["metadata"] == githubapp.PermissionRead || installation.Permissions["metadata"] == githubapp.PermissionWrite) &&
		(installation.Permissions["contents"] == githubapp.PermissionRead || installation.Permissions["contents"] == githubapp.PermissionWrite) &&
		installation.Permissions["administration"] == githubapp.PermissionRead
}

var _ RepositoryProtectionVerifier = GitHubRepositoryProtectionVerifier{}
