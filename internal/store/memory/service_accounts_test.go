package memory

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func TestServiceAccountTokenIsScopedHashedExpiringAndRevocable(t *testing.T) {
	ctx := context.Background()
	store := New()
	admin := bootstrapAccessAdmin(t, store)
	projectResult, err := store.CreateProject(ctx, admin.ID, "project", "project", domain.CreateProject{Name: "Payments", Slug: "payments-service-account"})
	if err != nil {
		t.Fatal(err)
	}
	project := projectResult.Value

	accountResult, err := store.CreateServiceAccount(ctx, admin.ID, "account", "account-fingerprint", "request", domain.CreateServiceAccount{ProjectID: project.ID, Name: "Deploy agent", Role: domain.RoleDeveloper})
	if err != nil {
		t.Fatal(err)
	}
	account := accountResult.Value
	if account.ProjectID != project.ID || account.Role != domain.RoleDeveloper || account.DisabledAt != nil {
		t.Fatalf("account=%#v", account)
	}
	replay, err := store.CreateServiceAccount(ctx, admin.ID, "account", "account-fingerprint", "request", domain.CreateServiceAccount{ProjectID: project.ID, Name: "Deploy agent", Role: domain.RoleDeveloper})
	if err != nil || !replay.Replay || replay.Value.ID != account.ID {
		t.Fatalf("account replay=%#v err=%v", replay, err)
	}

	raw := []byte("kp_sa_test-token-never-persisted")
	hash := sha256.Sum256(raw)
	tokenResult, err := store.CreateServiceAccountToken(ctx, admin.ID, "token", "token-fingerprint", "request", domain.CreateServiceAccountToken{
		ServiceAccountID: account.ID,
		Name:             "CI",
		Prefix:           "kp_sa_test_tok",
		TokenHash:        hash[:],
		Scopes:           []domain.AutomationScope{domain.AutomationScopeLogsRead, domain.AutomationScopeAppRead},
		ExpiresAt:        time.Now().UTC().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	token := tokenResult.Value
	if len(token.Scopes) != 2 || token.Scopes[0] != domain.AutomationScopeAppRead || token.Scopes[1] != domain.AutomationScopeLogsRead {
		t.Fatalf("token scopes=%v", token.Scopes)
	}
	if replay, found, replayErr := store.ServiceAccountTokenReplay(ctx, admin.ID, account.ID, "token", "token-fingerprint"); replayErr != nil || !found || !replay.Replay || replay.Value.ID != token.ID {
		t.Fatalf("token replay lookup=%#v found=%t err=%v", replay, found, replayErr)
	}
	if _, _, replayErr := store.ServiceAccountTokenReplay(ctx, admin.ID, account.ID, "token", "different"); !errors.Is(replayErr, base.ErrIdempotencyConflict) {
		t.Fatalf("token replay fingerprint error=%v", replayErr)
	}
	principal, err := store.ServiceAccountByToken(ctx, hash[:], time.Now().UTC())
	if err != nil || principal.User.ID != account.ID || principal.TokenID != token.ID || principal.ServiceAccountID != account.ID {
		t.Fatalf("principal=%#v err=%v", principal, err)
	}
	if _, err = store.ServiceAccountByToken(ctx, hash[:], token.ExpiresAt.Add(time.Nanosecond)); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("expired bearer error=%v", err)
	}
	wrong := sha256.Sum256([]byte("wrong-token"))
	if _, err = store.ServiceAccountByToken(ctx, wrong[:], time.Now().UTC()); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("wrong bearer error=%v", err)
	}

	users, err := store.ListUsersForActor(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range users {
		if user.ID == account.ID {
			t.Fatal("service account leaked into the human user directory")
		}
	}
	team, err := store.CreateTeam(ctx, admin.ID, "team", "team", "request", domain.CreateTeam{Name: "Operators", Slug: "service-account-operators"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.AddTeamMember(ctx, admin.ID, team.Value.ID, "add-service-account", "add-service-account", "request", domain.AddTeamMember{UserID: account.ID, Role: "member"}); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("service account was accepted as a human team member: %v", err)
	}

	if replay, err := store.RevokeServiceAccountToken(ctx, admin.ID, account.ID, token.ID, "revoke", "revoke-fingerprint", "request"); err != nil || replay {
		t.Fatalf("revoke replay=%t err=%v", replay, err)
	}
	if _, err = store.ServiceAccountByToken(ctx, hash[:], time.Now().UTC()); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("revoked bearer error=%v", err)
	}
	if replay, err := store.RevokeServiceAccountToken(ctx, admin.ID, account.ID, token.ID, "revoke", "revoke-fingerprint", "request"); err != nil || !replay {
		t.Fatalf("revoke idempotent replay=%t err=%v", replay, err)
	}
}

func TestServiceAccountPolicyRejectsPrivilegeAndDisablesAllTokens(t *testing.T) {
	ctx := context.Background()
	store := New()
	admin := bootstrapAccessAdmin(t, store)
	project, err := store.CreateProject(ctx, admin.ID, "project", "project", domain.CreateProject{Name: "Identity", Slug: "identity-service-account"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.CreateServiceAccount(ctx, admin.ID, "platform", "platform", "request", domain.CreateServiceAccount{ProjectID: project.Value.ID, Name: "Root bot", Role: domain.RolePlatformAdmin}); !errors.Is(err, base.ErrForbidden) {
		t.Fatalf("platform service account error=%v", err)
	}
	account, err := store.CreateServiceAccount(ctx, admin.ID, "viewer", "viewer", "request", domain.CreateServiceAccount{ProjectID: project.Value.ID, Name: "Read bot", Role: domain.RoleViewer})
	if err != nil {
		t.Fatal(err)
	}
	for index, seed := range []string{"one", "two"} {
		hash := sha256.Sum256([]byte(seed))
		_, err = store.CreateServiceAccountToken(ctx, admin.ID, "token-"+seed, "fp-"+seed, "request", domain.CreateServiceAccountToken{ServiceAccountID: account.Value.ID, Name: seed, Prefix: "kp_sa_prefix0" + string(rune('0'+index)), TokenHash: hash[:], Scopes: []domain.AutomationScope{domain.AutomationScopeAppRead}, ExpiresAt: time.Now().UTC().Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
	}
	tooLong := sha256.Sum256([]byte("long"))
	if _, err = store.CreateServiceAccountToken(ctx, admin.ID, "long", "long", "request", domain.CreateServiceAccountToken{ServiceAccountID: account.Value.ID, Name: "Long", Prefix: "kp_sa_longtok0", TokenHash: tooLong[:], Scopes: []domain.AutomationScope{domain.AutomationScopeAppRead}, ExpiresAt: time.Now().UTC().Add(91 * 24 * time.Hour)}); !errors.Is(err, base.ErrConflict) {
		t.Fatalf("long-lived token error=%v", err)
	}
	if replay, err := store.DisableServiceAccount(ctx, admin.ID, account.Value.ID, "disable", "disable", "request"); err != nil || replay {
		t.Fatalf("disable replay=%t err=%v", replay, err)
	}
	for _, seed := range []string{"one", "two"} {
		hash := sha256.Sum256([]byte(seed))
		if _, err = store.ServiceAccountByToken(ctx, hash[:], time.Now().UTC()); !errors.Is(err, base.ErrNotFound) {
			t.Fatalf("disabled account token %q error=%v", seed, err)
		}
	}
}
