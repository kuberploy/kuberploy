package githubapp

import (
	"context"
	"os"
	"testing"
)

type unavailableLiveAppToken struct{}

func (unavailableLiveAppToken) AppToken(context.Context) (Credential, error) {
	return Credential{}, ErrTransport
}

func TestLiveOAuthCodeExchange(t *testing.T) {
	code, secret := os.Getenv("KUBERPLOY_TEST_GITHUB_OAUTH_CODE"), os.Getenv("KUBERPLOY_TEST_GITHUB_OAUTH_CLIENT_SECRET")
	if code == "" || secret == "" {
		t.Skip("live OAuth inputs are not set")
	}
	cfg := DefaultConfig()
	cfg.AppID = 4543722
	cfg.ClientID = "Iv23liuwUJXB6lYoGl3q"
	cfg.PrivateKeySecret = SecretRef{Name: "unused", Key: "private-key.pem"}
	cfg.WebhookSecret = SecretRef{Name: "unused", Key: "webhook-secret"}
	cfg.StateSigningSecret = SecretRef{Name: "unused", Key: "state-signing-secret"}
	cfg.MaximumTokenPermissions = Permissions{"metadata": PermissionRead, "contents": PermissionWrite}
	secretRef := SecretRef{Name: projectedRuntimeSecret, Key: projectedOAuthClientSecretKey}
	exchanger, err := newOAuthCodeExchanger(cfg, "https://kuberploy-test.adminchatmate.com/v1/github/installations/callback",
		&mapSecrets{values: map[SecretRef][]byte{secretRef: []byte(secret)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := exchanger.ExchangeCode(context.Background(), code)
	if err != nil {
		t.Fatalf("exchange OAuth code: %v", err)
	}
	client, err := NewClient(cfg, unavailableLiveAppToken{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	user, err := client.VerifyAuthenticatedUser(context.Background(), credential)
	if err != nil || user.ID != 14298568 {
		t.Fatalf("verify exchanged user: id=%d err=%v", user.ID, err)
	}
}

func TestLiveProjectedOAuthCodeExchange(t *testing.T) {
	code := os.Getenv("KUBERPLOY_TEST_GITHUB_OAUTH_CODE")
	if code == "" {
		t.Skip("KUBERPLOY_TEST_GITHUB_OAUTH_CODE is not set")
	}
	cfg := DefaultConfig()
	cfg.AppID = 4543722
	cfg.ClientID = "Iv23liuwUJXB6lYoGl3q"
	cfg.PrivateKeySecret = SecretRef{Name: projectedRuntimeSecret, Key: projectedPrivateKey}
	cfg.WebhookSecret = SecretRef{Name: projectedRuntimeSecret, Key: projectedWebhookKey}
	cfg.StateSigningSecret = SecretRef{Name: projectedRuntimeSecret, Key: projectedStateKey}
	cfg.MaximumTokenPermissions = Permissions{"metadata": PermissionRead, "contents": PermissionWrite}
	exchanger, err := NewProjectedOAuthCodeExchanger(cfg, "https://kuberploy-test.adminchatmate.com/v1/github/installations/callback")
	if err != nil {
		t.Fatal(err)
	}
	credential, err := exchanger.ExchangeCode(context.Background(), code)
	if err != nil {
		t.Fatalf("exchange projected OAuth code: %v", err)
	}
	client, err := NewClient(cfg, unavailableLiveAppToken{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	user, err := client.VerifyAuthenticatedUser(context.Background(), credential)
	if err != nil || user.ID != 14298568 {
		t.Fatalf("verify projected OAuth user: id=%d err=%v", user.ID, err)
	}
	verification, err := client.verifySetupInstallation(context.Background(), credential, 152576900, user.ID, 313690005)
	if err != nil {
		t.Fatalf("verify projected setup installation: %v", err)
	}
	repositories, err := client.ListUserInstallationRepositories(context.Background(), credential, verification.Installation.ID, verification.Installation.Account)
	if err != nil || len(repositories) != 3 {
		t.Fatalf("list projected setup repositories: count=%d err=%v", len(repositories), err)
	}
}

// TestLiveUserInstallationVerification is an opt-in provider compatibility
// check. It intentionally accepts the short-lived user token only through the
// process environment and never logs or persists it.
func TestLiveUserInstallationVerification(t *testing.T) {
	token := os.Getenv("KUBERPLOY_TEST_GITHUB_USER_TOKEN")
	if token == "" {
		t.Skip("KUBERPLOY_TEST_GITHUB_USER_TOKEN is not set")
	}
	cfg := DefaultConfig()
	cfg.AppID = 4543722
	cfg.ClientID = "Iv23liuwUJXB6lYoGl3q"
	cfg.PrivateKeySecret = SecretRef{Name: "unused", Key: "private-key.pem"}
	cfg.WebhookSecret = SecretRef{Name: "unused", Key: "webhook-secret"}
	cfg.StateSigningSecret = SecretRef{Name: "unused", Key: "state-signing-secret"}
	cfg.MaximumTokenPermissions = Permissions{
		"metadata":       PermissionRead,
		"contents":       PermissionWrite,
		"pull_requests":  PermissionWrite,
		"administration": PermissionRead,
	}
	client, err := NewClient(cfg, unavailableLiveAppToken{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	user, err := client.VerifyAuthenticatedUserRaw(context.Background(), token)
	if err != nil {
		t.Fatalf("verify authenticated user: %v", err)
	}
	if user.ID != 14298568 || user.Login != "thanet-s" || user.Type != "User" {
		t.Fatalf("unexpected authenticated user: id=%d login=%q type=%q", user.ID, user.Login, user.Type)
	}
	verification, err := client.VerifySetupInstallationRaw(context.Background(), token, 152576900, user.ID, 313690005)
	if err != nil {
		t.Fatalf("verify setup installation: %v", err)
	}
	if verification.Installation.ID != 152576900 || verification.Installation.Account.ID != 313690005 ||
		verification.Installation.Account.Login != "kuberploy" || verification.Installation.RepositorySelection != "selected" {
		t.Fatalf("unexpected installation identity: id=%d account=%d/%q selection=%q", verification.Installation.ID,
			verification.Installation.Account.ID, verification.Installation.Account.Login, verification.Installation.RepositorySelection)
	}
	credential, err := credentialFromRaw(token)
	if err != nil {
		t.Fatal(err)
	}
	repositories, err := client.ListUserInstallationRepositories(context.Background(), credential, verification.Installation.ID, verification.Installation.Account)
	if err != nil {
		t.Fatalf("list installation repositories: %v", err)
	}
	if len(repositories) != 3 {
		t.Fatalf("unexpected repository count: %d", len(repositories))
	}
}
