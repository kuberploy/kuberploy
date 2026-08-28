package builds

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/githubapp"
	centralmemory "github.com/kuberploy/kuberploy/internal/store/memory"
)

const setupActorID = "11111111-1111-4111-8111-111111111111"

type setupTestClock struct{ now time.Time }

func (c *setupTestClock) Now() time.Time { return c.now }

type setupTestRandom struct{ next byte }

func (r *setupTestRandom) Read(p []byte) (int, error) {
	r.next++
	for i := range p {
		p[i] = r.next
	}
	return len(p), nil
}

type setupTestSecrets struct {
	state githubapp.SecretRef
	key   []byte
}

func (s setupTestSecrets) ReadSecret(_ context.Context, ref githubapp.SecretRef) ([]byte, error) {
	if ref != s.state {
		return nil, errors.New("unexpected secret reference")
	}
	return append([]byte(nil), s.key...), nil
}

type setupTestProvider struct {
	mu      sync.Mutex
	result  SetupProviderResult
	request SetupProviderRequest
	calls   int
}

func (p *setupTestProvider) CompleteSetup(_ context.Context, request SetupProviderRequest) (SetupProviderResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.request = request
	result := p.result
	result.Verification.Installation.Permissions = clonePermissions(result.Verification.Installation.Permissions)
	result.Repositories = cloneRepositoryIdentities(result.Repositories)
	return result, nil
}

func newSetupService(t *testing.T) (*SetupService, *MemoryStore, *centralmemory.Store, *setupTestProvider, *setupTestClock) {
	t.Helper()
	clock := &setupTestClock{now: time.Date(2026, 8, 9, 3, 0, 0, 0, time.UTC)}
	config := githubapp.DefaultConfig()
	config.AppID = 12345
	config.ClientID = "Iv1_kuberploy_test"
	config.PrivateKeySecret = githubapp.SecretRef{Name: "github-app", Key: "private-key.pem"}
	config.WebhookSecret = githubapp.SecretRef{Name: "github-app", Key: "webhook-secret"}
	config.StateSigningSecret = githubapp.SecretRef{Name: "github-app", Key: "state-secret"}
	config.MaximumTokenPermissions = githubapp.Permissions{"metadata": githubapp.PermissionRead, "contents": githubapp.PermissionWrite}
	manager, err := githubapp.NewStateManager(config, setupTestSecrets{state: config.StateSigningSecret, key: []byte("state-signing-secret-with-at-least-32-bytes")}, clock,
		&setupTestRandom{})
	if err != nil {
		t.Fatal(err)
	}
	provider := &setupTestProvider{result: SetupProviderResult{Verification: githubapp.SetupVerification{
		User: githubapp.AccountIdentity{ID: 101, Login: "alice", Type: "User"},
		Installation: githubapp.Installation{ID: 4242, AppID: config.AppID, ClientID: config.ClientID,
			Account: githubapp.AccountIdentity{ID: 202, Login: "example-org", Type: "Organization"}, RepositorySelection: "selected",
			Permissions: githubapp.Permissions{"metadata": githubapp.PermissionRead, "contents": githubapp.PermissionWrite}},
	}, Repositories: []githubapp.RepositoryIdentity{{ID: 303, OwnerID: 202, OwnerLogin: "example-org", Name: "api"}}}}
	buildStore := NewMemoryStore()
	catalog := centralmemory.New()
	user := domain.User{ID: setupActorID, Email: "alice@example.test", DisplayName: "Alice", Role: "platform-admin", Issuer: "test", Subject: "alice", GrantRevision: 1, CreatedAt: clock.now}
	hash := sha256.Sum256([]byte("session"))
	if err = catalog.BootstrapAdmin(context.Background(), user, strings.Repeat("h", 64), hash[:], clock.now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	service := &SetupService{StateManager: manager, Provider: provider, Store: buildStore, Catalog: catalog,
		InstallURL: "https://github.com/apps/kuberploy/installations/new", OAuthClientID: config.ClientID,
		OAuthCallbackURL: "https://kuberploy.example.test/v1/github/installations/callback", AppID: config.AppID, HandoffTTL: 5 * time.Minute,
		Clock: clock, Random: &setupTestRandom{next: 32}}
	return service, buildStore, catalog, provider, clock
}

func continueSetup(t *testing.T, service *SetupService, state string, installationID int64) ContinueSetupResult {
	t.Helper()
	result, err := service.Continue(context.Background(), ContinueSetupRequest{ActorID: setupActorID, State: state, InstallationID: installationID})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func relinkExistingSetup(t *testing.T, service *SetupService, suffix, fingerprintCharacter string) LinkSetupResult {
	t.Helper()
	ctx := context.Background()
	begin, err := service.Begin(ctx, BeginSetupRequest{ActorID: setupActorID, ExpectedAccountID: 202, ExistingInstallationID: 4242,
		ReturnKey: "application-source", IdempotencyKey: "setup-authorize-" + suffix,
		RequestFingerprint: "sha256:" + strings.Repeat(fingerprintCharacter, 64)})
	if err != nil {
		t.Fatal(err)
	}
	oauth := continueSetup(t, service, begin.State, 4242)
	completed, err := service.Complete(ctx, CompleteSetupRequest{ActorID: setupActorID, State: oauth.State, Code: "oauth-code-" + suffix + "-1234567890"})
	if err != nil {
		t.Fatal(err)
	}
	linked, err := service.Link(ctx, LinkSetupRequest{ActorID: setupActorID, Handoff: completed.Handoff,
		IdempotencyKey: "setup-link-" + suffix, RequestFingerprint: "sha256:" + strings.Repeat(fingerprintCharacter, 64), RequestID: "request-" + suffix})
	if err != nil {
		t.Fatal(err)
	}
	return linked
}

func TestSetupBindsExactGitHubUserAndLinksOnlyVerifiedMetadata(t *testing.T) {
	service, buildStore, catalog, provider, clock := newSetupService(t)
	ctx := context.Background()
	begin, err := service.Begin(ctx, BeginSetupRequest{ActorID: setupActorID, ExpectedAccountID: 202, ReturnKey: "application-source",
		IdempotencyKey: "setup-authorize-0001", RequestFingerprint: "sha256:" + strings.Repeat("1", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(begin.AuthorizationURL, "https://github.com/apps/kuberploy/installations/new?state=") || begin.State == "" {
		t.Fatalf("unsafe authorization result: %#v", begin)
	}
	existing, err := service.Begin(ctx, BeginSetupRequest{ActorID: setupActorID, ExpectedAccountID: 202, ExistingInstallationID: 4242,
		ReturnKey: "application-source", IdempotencyKey: "setup-authorize-existing-0001",
		RequestFingerprint: "sha256:" + strings.Repeat("2", 64)})
	if err != nil {
		t.Fatal(err)
	}
	existingURL, err := url.Parse(existing.AuthorizationURL)
	if err != nil || existingURL.Scheme != "https" || existingURL.Host != "kuberploy.example.test" ||
		existingURL.Path != "/v1/github/installations/setup" || existingURL.Query().Get("state") != existing.State ||
		existingURL.Query().Get("installation_id") != "4242" || existingURL.Query().Get("setup_action") != "update" || len(existingURL.Query()) != 3 {
		t.Fatalf("unsafe existing-installation continuation: %q err=%v", existing.AuthorizationURL, err)
	}
	replay, err := service.Begin(ctx, BeginSetupRequest{ActorID: setupActorID, ExpectedAccountID: 202, ReturnKey: "application-source",
		IdempotencyKey: "setup-authorize-0001", RequestFingerprint: "sha256:" + strings.Repeat("1", 64)})
	if err != nil || !replay.Replay || replay.State != begin.State {
		t.Fatalf("authorization replay=%#v err=%v", replay, err)
	}
	if _, err = service.Complete(ctx, CompleteSetupRequest{ActorID: setupActorID, State: begin.State, Code: "oauth-code-1234567890"}); !errors.Is(err, githubapp.ErrInvalidState) {
		t.Fatalf("installation state accepted at OAuth callback: %v", err)
	}
	oauth := continueSetup(t, service, begin.State, 4242)
	authorizationURL, err := url.Parse(oauth.AuthorizationURL)
	if err != nil || authorizationURL.Scheme != "https" || authorizationURL.Host != "github.com" || authorizationURL.Path != "/login/oauth/authorize" ||
		authorizationURL.Query().Get("client_id") != "Iv1_kuberploy_test" ||
		authorizationURL.Query().Get("redirect_uri") != "https://kuberploy.example.test/v1/github/installations/callback" ||
		authorizationURL.Query().Get("state") != oauth.State || len(authorizationURL.Query()) != 3 {
		t.Fatalf("unsafe OAuth authorization URL: %q err=%v", oauth.AuthorizationURL, err)
	}
	if _, err = service.Continue(ctx, ContinueSetupRequest{ActorID: setupActorID, State: oauth.State, InstallationID: 4242}); !errors.Is(err, githubapp.ErrInvalidState) {
		t.Fatalf("OAuth state accepted at setup return: %v", err)
	}
	if _, err = service.Continue(ctx, ContinueSetupRequest{ActorID: setupActorID, State: begin.State, InstallationID: 4242}); !errors.Is(err, githubapp.ErrStateReplay) {
		t.Fatalf("installation state replay accepted: %v", err)
	}
	completed, err := service.Complete(ctx, CompleteSetupRequest{ActorID: setupActorID, State: oauth.State, Code: "oauth-code-1234567890"})
	if err != nil {
		t.Fatal(err)
	}
	if completed.Handoff == "" || len(completed.Handoff) != 43 || completed.GitHubUser.ID != 101 {
		t.Fatalf("unexpected completion: %#v", completed)
	}
	provider.mu.Lock()
	providerRequest, providerCalls := provider.request, provider.calls
	provider.mu.Unlock()
	if providerCalls != 1 || providerRequest.ExpectedGitHubUserID != 0 || providerRequest.ExpectedAccountID != 202 || providerRequest.InstallationID != 4242 {
		t.Fatalf("provider request was not exactly bound: %#v calls=%d", providerRequest, providerCalls)
	}
	if _, err = service.Complete(ctx, CompleteSetupRequest{ActorID: setupActorID, State: oauth.State, Code: "oauth-code-1234567890"}); !errors.Is(err, githubapp.ErrStateReplay) {
		t.Fatalf("callback state replay accepted: %v", err)
	}
	binding, err := buildStore.GitHubUserBinding(ctx, setupActorID)
	if err != nil || binding.ID != 101 {
		t.Fatalf("immutable user binding=%#v err=%v", binding, err)
	}
	for digest, stored := range buildStore.setupHandoffs {
		if digest == ([sha256.Size]byte{}) || stored.record.ActorID != setupActorID || strings.Contains(string(digest[:]), completed.Handoff) {
			t.Fatalf("unsafe durable handoff: %#v", stored)
		}
	}
	linked, err := service.Link(ctx, LinkSetupRequest{ActorID: setupActorID, Handoff: completed.Handoff, IdempotencyKey: "setup-link-00000001",
		RequestFingerprint: "sha256:" + strings.Repeat("2", 64), RequestID: "request-1"})
	if err != nil {
		t.Fatal(err)
	}
	if linked.Installation.OwnerUserID != setupActorID || linked.Installation.Visibility != "private" || len(linked.Repositories) != 1 || linked.Repositories[0].Identity.ID != 303 {
		t.Fatalf("unsafe linked result: %#v", linked)
	}
	linkedReplay, err := service.Link(ctx, LinkSetupRequest{ActorID: setupActorID, Handoff: completed.Handoff, IdempotencyKey: "setup-link-00000001",
		RequestFingerprint: "sha256:" + strings.Repeat("2", 64), RequestID: "request-2"})
	if err != nil || !linkedReplay.Replay || linkedReplay.Installation.ID != linked.Installation.ID {
		t.Fatalf("link replay=%#v err=%v", linkedReplay, err)
	}
	visible, err := catalog.ListGitHubInstallationsForActor(ctx, setupActorID)
	if err != nil || len(visible) != 1 || visible[0].ID != linked.Installation.ID {
		t.Fatalf("central catalog mismatch: %#v err=%v", visible, err)
	}

	clock.now = clock.now.Add(time.Second)
	provider.mu.Lock()
	provider.result.Verification.Installation.RepositorySelection = "all"
	provider.result.Repositories = []githubapp.RepositoryIdentity{
		{ID: 303, OwnerID: 202, OwnerLogin: "example-org", Name: "api"},
		{ID: 304, OwnerID: 202, OwnerLogin: "example-org", Name: "worker"},
	}
	provider.mu.Unlock()
	relinked := relinkExistingSetup(t, service, "selection-all-01", "a")
	if relinked.Installation.ID != linked.Installation.ID || relinked.Installation.RepositorySelection != "all" || len(relinked.Repositories) != 2 {
		t.Fatalf("selection expansion mismatch: %#v", relinked)
	}

	clock.now = clock.now.Add(time.Second)
	provider.mu.Lock()
	provider.result.Verification.Installation.RepositorySelection = "selected"
	provider.result.Repositories = []githubapp.RepositoryIdentity{{ID: 304, OwnerID: 202, OwnerLogin: "example-org", Name: "worker"}}
	provider.mu.Unlock()
	relinked = relinkExistingSetup(t, service, "selection-selected-02", "b")
	if relinked.Installation.ID != linked.Installation.ID || relinked.Installation.RepositorySelection != "selected" || len(relinked.Repositories) != 1 {
		t.Fatalf("selection reduction mismatch: %#v", relinked)
	}
	visible, err = catalog.ListGitHubInstallationsForActor(ctx, setupActorID)
	if err != nil || len(visible) != 1 || visible[0].RepositorySelection != "selected" || visible[0].RepositoryCount != 1 {
		t.Fatalf("reverified central catalog mismatch: %#v err=%v", visible, err)
	}
	removedID := deterministicUUID("github-repository-v1", linked.Installation.ID, "303")
	keptID := deterministicUUID("github-repository-v1", linked.Installation.ID, "304")
	buildStore.mu.Lock()
	removed, removedExists := buildStore.repositories[removedID]
	kept, keptExists := buildStore.repositories[keptID]
	buildStore.mu.Unlock()
	if !removedExists || removed.Lifecycle != RepositoryRemoved || removed.RemovedAt == nil || !keptExists || kept.Lifecycle != RepositoryActive || kept.RemovedAt != nil {
		t.Fatalf("repository reconciliation mismatch: removed=%#v kept=%#v", removed, kept)
	}
}

func TestSetupLinksPersonalGitHubInstallation(t *testing.T) {
	service, buildStore, _, provider, _ := newSetupService(t)
	provider.mu.Lock()
	provider.result.Verification.Installation.Account = githubapp.AccountIdentity{ID: 202, Login: "alice", Type: "User"}
	provider.result.Repositories = []githubapp.RepositoryIdentity{{ID: 303, OwnerID: 202, OwnerLogin: "alice", Name: "personal-installation-fixture"}}
	provider.mu.Unlock()

	ctx := context.Background()
	begin, err := service.Begin(ctx, BeginSetupRequest{ActorID: setupActorID, ExpectedAccountID: 202, ReturnKey: "source-builds",
		IdempotencyKey: "personal-setup-authorize-01", RequestFingerprint: "sha256:" + strings.Repeat("8", 64)})
	if err != nil {
		t.Fatal(err)
	}
	oauth := continueSetup(t, service, begin.State, 4242)
	completed, err := service.Complete(ctx, CompleteSetupRequest{ActorID: setupActorID, State: oauth.State, Code: "oauth-code-personal-123"})
	if err != nil {
		t.Fatal(err)
	}
	linked, err := service.Link(ctx, LinkSetupRequest{ActorID: setupActorID, Handoff: completed.Handoff,
		IdempotencyKey: "personal-setup-link-01", RequestFingerprint: "sha256:" + strings.Repeat("9", 64), RequestID: "personal-request-1"})
	if err != nil {
		t.Fatal(err)
	}
	if linked.Installation.AccountType != "User" || linked.Installation.AccountLogin != "alice" || len(linked.Repositories) != 1 ||
		linked.Repositories[0].Identity.OwnerID != 202 || linked.Repositories[0].Identity.OwnerLogin != "alice" {
		t.Fatalf("personal installation was not preserved: %#v", linked)
	}
	if _, err = buildStore.GitHubUserBinding(ctx, setupActorID); err != nil {
		t.Fatalf("personal installation did not preserve GitHub user binding: %v", err)
	}
}

func TestSetupReturnIsAtomicUnderConcurrency(t *testing.T) {
	service, _, _, _, _ := newSetupService(t)
	ctx := context.Background()
	begin, err := service.Begin(ctx, BeginSetupRequest{ActorID: setupActorID, ExpectedAccountID: 202, ReturnKey: "application-source",
		IdempotencyKey: "setup-authorize-race", RequestFingerprint: "sha256:" + strings.Repeat("6", 64)})
	if err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int64
	var replays atomic.Int64
	var wait sync.WaitGroup
	for range 16 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, continueErr := service.Continue(ctx, ContinueSetupRequest{ActorID: setupActorID, State: begin.State, InstallationID: 4242})
			switch {
			case continueErr == nil && strings.HasPrefix(result.AuthorizationURL, "https://github.com/login/oauth/authorize?"):
				successes.Add(1)
			case errors.Is(continueErr, githubapp.ErrStateReplay):
				replays.Add(1)
			default:
				t.Errorf("unexpected concurrent setup result=%#v err=%v", result, continueErr)
			}
		}()
	}
	wait.Wait()
	if successes.Load() != 1 || replays.Load() != 15 {
		t.Fatalf("setup successes=%d replays=%d", successes.Load(), replays.Load())
	}
}

func TestSetupRejectsUnsafeOAuthConfigurationAndNonCanonicalBoundInstallation(t *testing.T) {
	for name, mutate := range map[string]func(*SetupService){
		"client": func(service *SetupService) { service.OAuthClientID = "client id" },
		"http": func(service *SetupService) {
			service.OAuthCallbackURL = "http://kuberploy.example.test/v1/github/installations/callback"
		},
		"credentials": func(service *SetupService) {
			service.OAuthCallbackURL = "https://user@kuberploy.example.test/v1/github/installations/callback"
		},
		"query":    func(service *SetupService) { service.OAuthCallbackURL += "?next=evil" },
		"fragment": func(service *SetupService) { service.OAuthCallbackURL += "#fragment" },
		"wrong-path": func(service *SetupService) {
			service.OAuthCallbackURL = "https://kuberploy.example.test/oauth/callback"
		},
		"invalid-port": func(service *SetupService) {
			service.OAuthCallbackURL = "https://kuberploy.example.test:0/v1/github/installations/callback"
		},
	} {
		t.Run(name, func(t *testing.T) {
			service, _, _, _, _ := newSetupService(t)
			mutate(service)
			_, err := service.Begin(context.Background(), BeginSetupRequest{ActorID: setupActorID, ExpectedAccountID: 202, ReturnKey: "application-source",
				IdempotencyKey: "setup-invalid-config", RequestFingerprint: "sha256:" + strings.Repeat("7", 64)})
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("unsafe OAuth configuration accepted: %v", err)
			}
		})
	}

	service, _, _, provider, _ := newSetupService(t)
	issued, err := service.StateManager.Issue(context.Background(), githubapp.StateRequest{Purpose: githubapp.StatePurposeOAuth, ActorID: setupActorID,
		ExpectedAccountID: 202, ReturnKey: "github-installation-04242"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Complete(context.Background(), CompleteSetupRequest{ActorID: setupActorID, State: issued.Reveal(), Code: "oauth-code-1234567890"}); !errors.Is(err, githubapp.ErrInvalidState) {
		t.Fatalf("non-canonical installation binding accepted: %v", err)
	}
	provider.mu.Lock()
	providerCalls := provider.calls
	provider.mu.Unlock()
	if providerCalls != 0 {
		t.Fatalf("provider called before signed installation binding validation: %d", providerCalls)
	}
}

func TestSetupRejectsIdentityConfusionAndConcurrentHandoffReuse(t *testing.T) {
	service, _, _, provider, _ := newSetupService(t)
	ctx := context.Background()
	begin, err := service.Begin(ctx, BeginSetupRequest{ActorID: setupActorID, ExpectedAccountID: 202, ReturnKey: "application-source",
		IdempotencyKey: "setup-authorize-0002", RequestFingerprint: "sha256:" + strings.Repeat("3", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Continue(ctx, ContinueSetupRequest{ActorID: "22222222-2222-4222-8222-222222222222", State: begin.State,
		InstallationID: 4242}); !errors.Is(err, githubapp.ErrInvalidState) {
		t.Fatalf("cross-actor setup return accepted: %v", err)
	}
	oauth := continueSetup(t, service, begin.State, 4242)
	provider.mu.Lock()
	provider.result.Verification.Installation.Account.ID = 999
	provider.mu.Unlock()
	if _, err = service.Complete(ctx, CompleteSetupRequest{ActorID: setupActorID, State: oauth.State, Code: "oauth-code-1234567890"}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("spoofed installation account accepted: %v", err)
	}

	service, _, _, _, _ = newSetupService(t)
	begin, _ = service.Begin(ctx, BeginSetupRequest{ActorID: setupActorID, ExpectedAccountID: 202, ReturnKey: "application-source",
		IdempotencyKey: "setup-authorize-0003", RequestFingerprint: "sha256:" + strings.Repeat("4", 64)})
	oauth = continueSetup(t, service, begin.State, 4242)
	completed, err := service.Complete(ctx, CompleteSetupRequest{ActorID: setupActorID, State: oauth.State, Code: "oauth-code-1234567890"})
	if err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int64
	var replays atomic.Int64
	var wait sync.WaitGroup
	for range 16 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, linkErr := service.Link(ctx, LinkSetupRequest{ActorID: setupActorID, Handoff: completed.Handoff,
				IdempotencyKey: "setup-link-race-01", RequestFingerprint: "sha256:" + strings.Repeat("5", 64), RequestID: "race"})
			if linkErr != nil {
				t.Errorf("concurrent link: %v", linkErr)
				return
			}
			successes.Add(1)
			if result.Replay {
				replays.Add(1)
			}
		}()
	}
	wait.Wait()
	if successes.Load() != 16 || (replays.Load() != 15 && replays.Load() != 16) {
		t.Fatalf("successes=%d replays=%d", successes.Load(), replays.Load())
	}
	if _, err = service.Link(ctx, LinkSetupRequest{ActorID: setupActorID, Handoff: completed.Handoff, IdempotencyKey: "setup-link-other-01",
		RequestFingerprint: "sha256:" + strings.Repeat("5", 64), RequestID: "other"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("handoff rebound under a different idempotency key: %v", err)
	}
}

func TestMemoryGitHubUserBindingUsesImmutableProviderIDButAllowsRename(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	now := time.Now().UTC()
	identity := githubapp.AccountIdentity{ID: 101, Login: "alice", Type: "User"}
	if err := store.BindGitHubUser(ctx, setupActorID, identity, now); err != nil {
		t.Fatal(err)
	}
	identity.Login = "alice-renamed"
	if err := store.BindGitHubUser(ctx, setupActorID, identity, now.Add(time.Minute)); err != nil {
		t.Fatalf("same immutable GitHub user could not refresh login: %v", err)
	}
	stored, err := store.GitHubUserBinding(ctx, setupActorID)
	if err != nil || stored.ID != identity.ID || stored.Login != identity.Login {
		t.Fatalf("binding=%#v err=%v", stored, err)
	}
	if err = store.BindGitHubUser(ctx, setupActorID, githubapp.AccountIdentity{ID: 202, Login: "mallory", Type: "User"}, now); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("actor rebound to a different GitHub user: %v", err)
	}
	if err = store.BindGitHubUser(ctx, "22222222-2222-4222-8222-222222222222", identity, now); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("GitHub user bound to a second actor: %v", err)
	}
}
