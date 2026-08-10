package builds

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/githubapp"
)

var setupIdempotencyRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$`)
var setupFingerprintRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var setupOAuthClientIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{3,128}$`)

const setupInstallationReturnKeyPrefix = "github-installation-"

// SetupProvider owns the short-lived OAuth code/token boundary. Implementations
// exchange Code, resolve GET /user, verify the callback installation through
// the authenticated user's installation list, and enumerate that installation's
// repositories. No user token may be returned from this interface.
type SetupProvider interface {
	CompleteSetup(context.Context, SetupProviderRequest) (SetupProviderResult, error)
}

type SetupProviderRequest struct {
	Code                 string
	InstallationID       int64
	ExpectedGitHubUserID int64
	ExpectedAccountID    int64
}

type SetupProviderResult struct {
	Verification githubapp.SetupVerification
	Repositories []githubapp.RepositoryIdentity
}

type SetupAuthorization struct {
	ActorID            string
	IdempotencyKey     string
	RequestFingerprint string
	State              string
	ExpiresAt          time.Time
	CreatedAt          time.Time
}

type SetupHandoff struct {
	Digest       [sha256.Size]byte
	ActorID      string
	GitHubUser   githubapp.AccountIdentity
	Installation githubapp.Installation
	Repositories []githubapp.RepositoryIdentity
	ExpiresAt    time.Time
	CreatedAt    time.Time
}

type ConsumedSetupHandoff struct {
	SetupHandoff
	LinkedInstallationID string
	Replay               bool
}

// SetupStore extends the durable build store with only safe setup metadata.
// Authorization state is signed but not a provider credential; handoffs are
// persisted only by their domain-separated digest.
type SetupStore interface {
	Store
	PutSetupAuthorization(context.Context, SetupAuthorization) (SetupAuthorization, bool, error)
	GitHubUserBinding(context.Context, string) (githubapp.AccountIdentity, error)
	BindGitHubUser(context.Context, string, githubapp.AccountIdentity, time.Time) error
	PutSetupHandoff(context.Context, SetupHandoff) error
	ConsumeSetupHandoff(context.Context, [sha256.Size]byte, string, string, string, time.Time) (ConsumedSetupHandoff, bool, error)
	CompleteSetupHandoff(context.Context, [sha256.Size]byte, string, time.Time) error
}

// VerifiedInstallationCatalog is the central authorization/catalog seam. It
// deliberately differs from the administrative metadata-registration method:
// only SetupService calls it after provider verification and handoff consume.
type VerifiedInstallationCatalog interface {
	LinkVerifiedGitHubInstallation(context.Context, string, string, string, string, domain.CreateGitHubInstallation) (domain.GitHubInstallation, bool, error)
}

type SetupService struct {
	StateManager     *githubapp.StateManager
	Provider         SetupProvider
	Store            SetupStore
	Catalog          VerifiedInstallationCatalog
	InstallURL       string
	OAuthClientID    string
	OAuthCallbackURL string
	AppID            int64
	HandoffTTL       time.Duration
	Clock            githubapp.Clock
	Random           io.Reader
}

type BeginSetupRequest struct {
	ActorID            string
	ExpectedAccountID  int64
	ReturnKey          string
	IdempotencyKey     string
	RequestFingerprint string
}

type BeginSetupResult struct {
	AuthorizationURL string
	State            string
	ExpiresAt        time.Time
	Replay           bool
}

type ContinueSetupRequest struct {
	ActorID        string
	State          string
	InstallationID int64
}

type ContinueSetupResult struct {
	AuthorizationURL string
	State            string
	ExpiresAt        time.Time
}

type CompleteSetupRequest struct {
	ActorID string
	State   string
	Code    string
}

type CompleteSetupResult struct {
	Handoff      string
	ExpiresAt    time.Time
	GitHubUser   githubapp.AccountIdentity
	Installation githubapp.Installation
	Repositories []githubapp.RepositoryIdentity
}

type LinkSetupRequest struct {
	ActorID            string
	Handoff            string
	IdempotencyKey     string
	RequestFingerprint string
	RequestID          string
}

type LinkSetupResult struct {
	Installation domain.GitHubInstallation
	Repositories []Repository
	Replay       bool
}

func (s *SetupService) Begin(ctx context.Context, request BeginSetupRequest) (BeginSetupResult, error) {
	if s == nil || s.StateManager == nil || s.Store == nil || s.Provider == nil || s.Catalog == nil ||
		!uuidRE.MatchString(request.ActorID) || request.ExpectedAccountID < 0 || !setupIdempotencyRE.MatchString(request.IdempotencyKey) ||
		!setupFingerprintRE.MatchString(request.RequestFingerprint) {
		return BeginSetupResult{}, ErrInvalid
	}
	base, err := validatedInstallURL(s.InstallURL)
	if err != nil || s.AppID <= 0 || validateOAuthSetupConfig(s.OAuthClientID, s.OAuthCallbackURL) != nil {
		return BeginSetupResult{}, ErrInvalid
	}
	issued, err := s.StateManager.Issue(ctx, githubapp.StateRequest{Purpose: githubapp.StatePurposeSetup, ActorID: request.ActorID,
		ExpectedAccountID: request.ExpectedAccountID, ReturnKey: request.ReturnKey})
	if err != nil {
		return BeginSetupResult{}, err
	}
	state := issued.Reveal()
	verified, err := s.StateManager.VerifyRaw(ctx, state, githubapp.StateExpectation{Purpose: githubapp.StatePurposeSetup, ActorID: request.ActorID})
	if err != nil {
		return BeginSetupResult{}, err
	}
	now := s.now()
	stored, replay, err := s.Store.PutSetupAuthorization(ctx, SetupAuthorization{ActorID: request.ActorID, IdempotencyKey: request.IdempotencyKey,
		RequestFingerprint: request.RequestFingerprint, State: state, ExpiresAt: verified.ExpiresAt, CreatedAt: now})
	if err != nil {
		return BeginSetupResult{}, err
	}
	if replay {
		verified, err = s.StateManager.VerifyRaw(ctx, stored.State, githubapp.StateExpectation{Purpose: githubapp.StatePurposeSetup, ActorID: request.ActorID})
		if err != nil {
			return BeginSetupResult{}, err
		}
	}
	query := base.Query()
	query.Set("state", stored.State)
	base.RawQuery = query.Encode()
	return BeginSetupResult{AuthorizationURL: base.String(), State: stored.State, ExpiresAt: verified.ExpiresAt, Replay: replay}, nil
}

// Continue consumes the GitHub App Setup URL return and starts a distinct,
// actor-bound OAuth authorization. GitHub documents installation_id only on
// the Setup URL return and code only on the OAuth callback, so the installation
// identity is carried between those stages solely inside a new signed state.
func (s *SetupService) Continue(ctx context.Context, request ContinueSetupRequest) (ContinueSetupResult, error) {
	if s == nil || s.StateManager == nil || s.Store == nil || !uuidRE.MatchString(request.ActorID) || request.InstallationID <= 0 ||
		validateOAuthSetupConfig(s.OAuthClientID, s.OAuthCallbackURL) != nil {
		return ContinueSetupResult{}, ErrInvalid
	}
	returnKey, err := installationReturnKey(request.InstallationID)
	if err != nil {
		return ContinueSetupResult{}, err
	}
	installState, err := s.StateManager.VerifyRaw(ctx, request.State, githubapp.StateExpectation{Purpose: githubapp.StatePurposeSetup, ActorID: request.ActorID})
	if err != nil {
		return ContinueSetupResult{}, err
	}
	if err = githubapp.ClaimState(ctx, s.Clock, s.Store, installState); err != nil {
		return ContinueSetupResult{}, err
	}
	issued, err := s.StateManager.Issue(ctx, githubapp.StateRequest{Purpose: githubapp.StatePurposeOAuth, ActorID: request.ActorID,
		ExpectedAccountID: installState.ExpectedAccountID, ReturnKey: returnKey})
	if err != nil {
		return ContinueSetupResult{}, err
	}
	state := issued.Reveal()
	verified, err := s.StateManager.VerifyRaw(ctx, state, githubapp.StateExpectation{Purpose: githubapp.StatePurposeOAuth, ActorID: request.ActorID})
	if err != nil {
		return ContinueSetupResult{}, err
	}
	authorizationURL, err := oauthAuthorizationURL(s.OAuthClientID, s.OAuthCallbackURL, state)
	if err != nil {
		return ContinueSetupResult{}, err
	}
	return ContinueSetupResult{AuthorizationURL: authorizationURL, State: state, ExpiresAt: verified.ExpiresAt}, nil
}

func (s *SetupService) Complete(ctx context.Context, request CompleteSetupRequest) (CompleteSetupResult, error) {
	if s == nil || s.StateManager == nil || s.Store == nil || s.Provider == nil || !uuidRE.MatchString(request.ActorID) ||
		!validOAuthCode(request.Code) {
		return CompleteSetupResult{}, ErrInvalid
	}
	verifiedState, err := s.StateManager.VerifyRaw(ctx, request.State, githubapp.StateExpectation{Purpose: githubapp.StatePurposeOAuth, ActorID: request.ActorID})
	if err != nil {
		return CompleteSetupResult{}, err
	}
	installationID, err := parseInstallationReturnKey(verifiedState.ReturnKey)
	if err != nil {
		return CompleteSetupResult{}, err
	}
	if err = githubapp.ClaimState(ctx, s.Clock, s.Store, verifiedState); err != nil {
		return CompleteSetupResult{}, err
	}
	expectedUserID := int64(0)
	if binding, bindingErr := s.Store.GitHubUserBinding(ctx, request.ActorID); bindingErr == nil {
		expectedUserID = binding.ID
	} else if !errors.Is(bindingErr, ErrNotFound) {
		return CompleteSetupResult{}, bindingErr
	}
	providerResult, err := s.Provider.CompleteSetup(ctx, SetupProviderRequest{Code: request.Code, InstallationID: installationID,
		ExpectedGitHubUserID: expectedUserID, ExpectedAccountID: verifiedState.ExpectedAccountID})
	if err != nil {
		return CompleteSetupResult{}, err
	}
	if err = validateSetupProviderResult(s.AppID, installationID, expectedUserID, verifiedState.ExpectedAccountID, providerResult); err != nil {
		return CompleteSetupResult{}, err
	}
	if err = s.Store.BindGitHubUser(ctx, request.ActorID, providerResult.Verification.User, s.now()); err != nil {
		return CompleteSetupResult{}, err
	}
	ttl := s.HandoffTTL
	if ttl == 0 {
		ttl = 5 * time.Minute
	}
	issued, err := githubapp.IssueHandoff(s.Clock, s.Random, ttl)
	if err != nil {
		return CompleteSetupResult{}, err
	}
	handoff := SetupHandoff{Digest: issued.Record.Digest, ActorID: request.ActorID, GitHubUser: providerResult.Verification.User,
		Installation: providerResult.Verification.Installation, Repositories: cloneRepositoryIdentities(providerResult.Repositories),
		ExpiresAt: issued.Record.ExpiresAt, CreatedAt: s.now()}
	if err = s.Store.PutSetupHandoff(ctx, handoff); err != nil {
		return CompleteSetupResult{}, err
	}
	return CompleteSetupResult{Handoff: issued.Token.Reveal(), ExpiresAt: handoff.ExpiresAt, GitHubUser: handoff.GitHubUser,
		Installation: handoff.Installation, Repositories: cloneRepositoryIdentities(handoff.Repositories)}, nil
}

type setupLinkConsumer struct {
	store       SetupStore
	actorID     string
	idemKey     string
	fingerprint string
	result      ConsumedSetupHandoff
	replay      bool
}

func (c *setupLinkConsumer) ConsumeHandoff(ctx context.Context, digest [sha256.Size]byte, now time.Time) (bool, error) {
	result, replay, err := c.store.ConsumeSetupHandoff(ctx, digest, c.actorID, c.idemKey, c.fingerprint, now)
	if err != nil {
		return false, err
	}
	c.result, c.replay = result, replay
	return true, nil
}

func (s *SetupService) Link(ctx context.Context, request LinkSetupRequest) (LinkSetupResult, error) {
	if s == nil || s.Store == nil || s.Catalog == nil || !uuidRE.MatchString(request.ActorID) ||
		!setupIdempotencyRE.MatchString(request.IdempotencyKey) || !setupFingerprintRE.MatchString(request.RequestFingerprint) ||
		len(request.RequestID) == 0 || len(request.RequestID) > 128 {
		return LinkSetupResult{}, ErrInvalid
	}
	consumer := &setupLinkConsumer{store: s.Store, actorID: request.ActorID, idemKey: request.IdempotencyKey, fingerprint: request.RequestFingerprint}
	if err := githubapp.ConsumeHandoffRaw(ctx, s.Clock, consumer, request.Handoff); err != nil {
		return LinkSetupResult{}, err
	}
	handoff := consumer.result
	create := domain.CreateGitHubInstallation{GitHubInstallationID: handoff.Installation.ID, AccountLogin: handoff.Installation.Account.Login,
		AccountType: handoff.Installation.Account.Type, RepositorySelection: handoff.Installation.RepositorySelection, RepositoryCount: len(handoff.Repositories)}
	linked, catalogReplay, err := s.Catalog.LinkVerifiedGitHubInstallation(ctx, request.ActorID, request.IdempotencyKey,
		request.RequestFingerprint, request.RequestID, create)
	if err != nil {
		return LinkSetupResult{}, err
	}
	now := s.now()
	installation := Installation{ID: linked.ID, AppID: handoff.Installation.AppID, GitHubInstallationID: handoff.Installation.ID,
		Account: handoff.Installation.Account, RepositorySelection: handoff.Installation.RepositorySelection,
		Permissions: clonePermissions(handoff.Installation.Permissions), Lifecycle: InstallationActive, LastVerifiedAt: now, UpdatedAt: now}
	if err = s.Store.PutInstallation(ctx, installation); err != nil {
		return LinkSetupResult{}, err
	}
	repositories := make([]Repository, 0, len(handoff.Repositories))
	for _, identity := range handoff.Repositories {
		repository := Repository{ID: deterministicUUID("github-repository-v1", linked.ID, strconv.FormatInt(identity.ID, 10)),
			InstallationID: linked.ID, Identity: identity, Lifecycle: RepositoryActive, LastVerifiedAt: now, CreatedAt: now, UpdatedAt: now}
		if err = s.Store.PutRepository(ctx, repository); err != nil {
			return LinkSetupResult{}, err
		}
		repositories = append(repositories, repository)
	}
	if err = s.Store.CompleteSetupHandoff(ctx, handoff.Digest, linked.ID, now); err != nil {
		return LinkSetupResult{}, err
	}
	sort.Slice(repositories, func(i, j int) bool { return repositories[i].Identity.ID < repositories[j].Identity.ID })
	return LinkSetupResult{Installation: linked, Repositories: repositories, Replay: consumer.replay || catalogReplay}, nil
}

func validatedInstallURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "github.com") || u.User != nil || u.Fragment != "" ||
		u.RawQuery != "" || !regexp.MustCompile(`^/apps/[a-z0-9](?:[a-z0-9-]{0,98}[a-z0-9])?/installations/new$`).MatchString(u.EscapedPath()) {
		return nil, ErrInvalid
	}
	return u, nil
}

func validateOAuthSetupConfig(clientID, callbackURL string) error {
	if !setupOAuthClientIDRE.MatchString(clientID) {
		return ErrInvalid
	}
	u, err := url.Parse(callbackURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		u.EscapedPath() != "/v1/github/installations/callback" {
		return ErrInvalid
	}
	if port := u.Port(); port != "" {
		value, portErr := strconv.Atoi(port)
		if portErr != nil || value < 1 || value > 65535 {
			return ErrInvalid
		}
	}
	return nil
}

func oauthAuthorizationURL(clientID, callbackURL, state string) (string, error) {
	if validateOAuthSetupConfig(clientID, callbackURL) != nil || len(state) < 64 || len(state) > 4096 || strings.Count(state, ".") != 1 {
		return "", ErrInvalid
	}
	u := &url.URL{Scheme: "https", Host: "github.com", Path: "/login/oauth/authorize"}
	query := u.Query()
	query.Set("client_id", clientID)
	query.Set("redirect_uri", callbackURL)
	query.Set("state", state)
	u.RawQuery = query.Encode()
	return u.String(), nil
}

func installationReturnKey(installationID int64) (string, error) {
	if installationID <= 0 {
		return "", ErrInvalid
	}
	key := setupInstallationReturnKeyPrefix + strconv.FormatInt(installationID, 10)
	if len(key) > 64 {
		return "", ErrInvalid
	}
	return key, nil
}

func parseInstallationReturnKey(key string) (int64, error) {
	if !strings.HasPrefix(key, setupInstallationReturnKeyPrefix) {
		return 0, githubapp.ErrInvalidState
	}
	raw := strings.TrimPrefix(key, setupInstallationReturnKeyPrefix)
	installationID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || installationID <= 0 || strconv.FormatInt(installationID, 10) != raw {
		return 0, githubapp.ErrInvalidState
	}
	return installationID, nil
}

func validOAuthCode(code string) bool {
	if len(code) < 16 || len(code) > 512 || strings.TrimSpace(code) != code {
		return false
	}
	for _, r := range code {
		if r < 0x21 || r > 0x7e || strings.ContainsRune("&=?#%\\", r) {
			return false
		}
	}
	return true
}

func validateSetupProviderResult(appID, callbackInstallationID, expectedUserID, expectedAccountID int64, result SetupProviderResult) error {
	user, installation := result.Verification.User, result.Verification.Installation
	if appID <= 0 || user.ID <= 0 || user.Type != "User" || !loginRE.MatchString(user.Login) ||
		(expectedUserID > 0 && user.ID != expectedUserID) || installation.ID != callbackInstallationID || installation.AppID != appID ||
		!validAccount(installation.Account) || (expectedAccountID > 0 && installation.Account.ID != expectedAccountID) ||
		(installation.RepositorySelection != "all" && installation.RepositorySelection != "selected") || installation.SuspendedAt != nil ||
		installation.Permissions["metadata"] != githubapp.PermissionRead ||
		(installation.Permissions["contents"] != githubapp.PermissionRead && installation.Permissions["contents"] != githubapp.PermissionWrite) ||
		len(result.Repositories) > 500 {
		return ErrUnauthorized
	}
	seen := make(map[int64]struct{}, len(result.Repositories))
	for _, repository := range result.Repositories {
		if !validRepository(repository) || repository.OwnerID != installation.Account.ID || !strings.EqualFold(repository.OwnerLogin, installation.Account.Login) {
			return ErrUnauthorized
		}
		if _, duplicate := seen[repository.ID]; duplicate {
			return ErrInvalid
		}
		seen[repository.ID] = struct{}{}
	}
	return nil
}

func cloneRepositoryIdentities(input []githubapp.RepositoryIdentity) []githubapp.RepositoryIdentity {
	return append([]githubapp.RepositoryIdentity(nil), input...)
}

func (s *SetupService) now() time.Time {
	if s.Clock != nil {
		return s.Clock.Now().UTC()
	}
	return time.Now().UTC()
}
