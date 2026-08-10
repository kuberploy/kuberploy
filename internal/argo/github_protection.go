package argo

import (
	"context"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/githubapp"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

// GitHubRepositoryProtectionClient is the narrow provider surface needed to
// prove that the mutable platform bootstrap ref cannot be pushed by a user or
// a different App. It cannot write repository settings or contents.
type GitHubRepositoryProtectionClient interface {
	VerifyInstallation(context.Context, int64, githubapp.AccountIdentity, githubapp.Permissions) (githubapp.Installation, error)
	MintInstallationToken(context.Context, githubapp.TokenRequest) (githubapp.InstallationToken, error)
	ObserveRepositoryProtection(context.Context, githubapp.InstallationToken, githubapp.RepositoryIdentity, string, string, int64) (githubapp.RepositoryProtectionObservation, error)
}

// GitHubPlatformRepositoryProtectionVerifier re-authorizes the immutable
// installation/repository catalog row, requests one repository-scoped
// read-only token, and delegates the closed branch/ruleset proof to the GitHub
// provider boundary. No URL, repository name, ref, or writer App identity is
// accepted from a caller outside the stored platform binding.
type GitHubPlatformRepositoryProtectionVerifier struct {
	AppID          int64
	Authorizations gitprojection.GitHubAuthorizationStore
	Client         GitHubRepositoryProtectionClient
	Now            func() time.Time
}

func (v GitHubPlatformRepositoryProtectionVerifier) VerifyPlatformRepositoryProtection(
	ctx context.Context,
	binding gitprojection.Binding,
	head gitprojection.VerifiedHead,
	now time.Time,
) (PlatformRepositoryProtectionObservation, error) {
	if v.AppID <= 0 || v.Authorizations == nil || v.Client == nil || now.IsZero() ||
		binding.Validate() != nil || binding.Kind != gitprojection.BindingPlatform ||
		binding.CredentialMode != gitprojection.CredentialGitHubApp || head.ValidateFor(binding) != nil {
		return PlatformRepositoryProtectionObservation{}, ErrInvalid
	}
	authorization, err := v.Authorizations.GitHubAuthorization(ctx, binding, v.AppID)
	if err != nil {
		return PlatformRepositoryProtectionObservation{}, err
	}
	if authorization.ValidateFor(binding) != nil {
		return PlatformRepositoryProtectionObservation{}, ErrArgoRuntimePrerequisiteNotReady
	}
	required := githubapp.Permissions{
		"metadata":       githubapp.PermissionRead,
		"contents":       githubapp.PermissionRead,
		"administration": githubapp.PermissionRead,
	}
	installation, err := v.Client.VerifyInstallation(
		ctx,
		binding.Repository.InstallationID,
		authorization.Account,
		required,
	)
	if err != nil {
		return PlatformRepositoryProtectionObservation{}, err
	}
	if !validProtectionInstallation(installation, binding, authorization, v.AppID) {
		return PlatformRepositoryProtectionObservation{}, ErrArgoRuntimePrerequisiteNotReady
	}
	token, err := v.Client.MintInstallationToken(ctx, githubapp.TokenRequest{
		InstallationID: binding.Repository.InstallationID,
		Account:        authorization.Account,
		Repositories:   []githubapp.RepositoryIdentity{authorization.Repository},
		Permissions:    required,
	})
	if err != nil {
		return PlatformRepositoryProtectionObservation{}, err
	}
	observed, err := v.Client.ObserveRepositoryProtection(
		ctx,
		token,
		authorization.Repository,
		binding.TargetRef,
		head.Commit,
		v.AppID,
	)
	if err != nil {
		return PlatformRepositoryProtectionObservation{}, err
	}
	completedAt := time.Now().UTC()
	if v.Now != nil {
		completedAt = v.Now().UTC()
	}
	if observed.InstallationID != binding.Repository.InstallationID ||
		observed.RepositoryID != binding.Repository.RepositoryID || observed.Ref != binding.TargetRef ||
		observed.Head != head.Commit || observed.WriterAppID != v.AppID ||
		!digestRE.MatchString(observed.PolicyDigest) || observed.ObservedAt.Before(now.UTC()) || observed.ObservedAt.After(completedAt) {
		return PlatformRepositoryProtectionObservation{}, ErrArgoRuntimePrerequisiteNotReady
	}

	// The prerequisite loop supplies the fenced observation time used by the
	// complete proof. The provider call may finish a few milliseconds after
	// that instant, so retaining the earlier instant is conservative and avoids
	// claiming freshness that predates the rest of the composite observation.
	result := PlatformRepositoryProtectionObservation{
		BindingID:    binding.ID,
		TargetRef:    binding.TargetRef,
		Head:         head.Commit,
		PolicyDigest: observed.PolicyDigest,
		ObservedAt:   now.UTC(),
	}
	if result.validateFor(binding, head, now.UTC()) != nil {
		return PlatformRepositoryProtectionObservation{}, ErrArgoRuntimePrerequisiteNotReady
	}
	return result, nil
}

func validProtectionInstallation(
	installation githubapp.Installation,
	binding gitprojection.Binding,
	authorization gitprojection.GitHubAuthorization,
	appID int64,
) bool {
	return installation.ID == binding.Repository.InstallationID && installation.AppID == appID &&
		installation.SuspendedAt == nil &&
		(installation.RepositorySelection == "all" || installation.RepositorySelection == "selected") &&
		installation.Account.ID == authorization.Account.ID &&
		strings.EqualFold(installation.Account.Login, authorization.Account.Login) &&
		installation.Account.Type == authorization.Account.Type &&
		allowsProviderRead(installation.Permissions["metadata"]) &&
		allowsProviderRead(installation.Permissions["contents"]) &&
		installation.Permissions["administration"] == githubapp.PermissionRead
}

func allowsProviderRead(level githubapp.PermissionLevel) bool {
	return level == githubapp.PermissionRead || level == githubapp.PermissionWrite
}

var _ PlatformRepositoryProtectionVerifier = GitHubPlatformRepositoryProtectionVerifier{}
