package builds

import (
	"context"

	"github.com/kuberploy/kuberploy/internal/githubapp"
)

type GitHubSetupProvider struct {
	Exchanger githubapp.CodeExchanger
	Client    *githubapp.Client
}

func (p *GitHubSetupProvider) CompleteSetup(ctx context.Context, request SetupProviderRequest) (SetupProviderResult, error) {
	if p == nil || p.Exchanger == nil || p.Client == nil || !validOAuthCode(request.Code) || request.InstallationID <= 0 ||
		request.ExpectedGitHubUserID < 0 || request.ExpectedAccountID < 0 {
		return SetupProviderResult{}, ErrInvalid
	}
	token, err := p.Exchanger.ExchangeCode(ctx, request.Code)
	if err != nil {
		return SetupProviderResult{}, err
	}
	user, err := p.Client.VerifyAuthenticatedUser(ctx, token)
	if err != nil {
		return SetupProviderResult{}, err
	}
	if request.ExpectedGitHubUserID > 0 && user.ID != request.ExpectedGitHubUserID {
		return SetupProviderResult{}, githubapp.ErrOwnershipMismatch
	}
	verification, err := p.Client.VerifySetupInstallation(ctx, token, request.InstallationID, user.ID, request.ExpectedAccountID)
	if err != nil {
		return SetupProviderResult{}, err
	}
	repositories, err := p.Client.ListUserInstallationRepositories(ctx, token, request.InstallationID, verification.Installation.Account)
	if err != nil {
		return SetupProviderResult{}, err
	}
	return SetupProviderResult{Verification: verification, Repositories: repositories}, nil
}

var _ SetupProvider = (*GitHubSetupProvider)(nil)
