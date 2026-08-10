package githubapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	providerPageSize = 100
	maximumPages     = 50
)

// Installation is verified provider metadata. It contains no credential.
type Installation struct {
	ID                  int64
	AppID               int64
	ClientID            string
	Account             AccountIdentity
	RepositorySelection string
	Permissions         Permissions
	SuspendedAt         *time.Time
}

// TokenRequest always identifies concrete repositories and explicit narrow
// permissions. Empty repository or permission sets are rejected.
type TokenRequest struct {
	InstallationID int64
	Account        AccountIdentity
	Repositories   []RepositoryIdentity
	Permissions    Permissions
}

// InstallationToken retains its scope alongside the opaque credential so later
// helpers can reject use against a different repository or installation.
type InstallationToken struct {
	ExpiresAt      time.Time
	credential     Credential
	installationID int64
	repositoryIDs  []int64
	permissions    Permissions
}

// Authorization exposes the token only for immediate use by an isolated source
// fetcher. Formatting and JSON remain redacted through Credential.
func (t InstallationToken) Authorization() Credential { return t.credential }

func (t InstallationToken) InstallationID() int64 { return t.installationID }

func (t InstallationToken) RepositoryIDs() []int64 {
	return append([]int64(nil), t.repositoryIDs...)
}

func (t InstallationToken) Permissions() Permissions { return clonePermissions(t.permissions) }

func (t InstallationToken) authorizes(repositoryID int64) bool {
	_, found := slices.BinarySearch(t.repositoryIDs, repositoryID)
	return found
}

type apiAccount struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}

func (a apiAccount) identity() AccountIdentity {
	return AccountIdentity{ID: a.ID, Login: a.Login, Type: a.Type}
}

type apiInstallation struct {
	ID                  int64             `json:"id"`
	AppID               int64             `json:"app_id"`
	ClientID            string            `json:"client_id"`
	Account             apiAccount        `json:"account"`
	RepositorySelection string            `json:"repository_selection"`
	Permissions         map[string]string `json:"permissions"`
	SuspendedAt         *time.Time        `json:"suspended_at"`
}

// VerifyInstallation verifies app identity, installation ownership,
// suspension, and the permissions needed by a future narrow token.
func (c *Client) VerifyInstallation(ctx context.Context, installationID int64, account AccountIdentity, required Permissions) (Installation, error) {
	appToken, err := c.appTokens.AppToken(ctx)
	if err != nil {
		return Installation{}, sanitizedTokenSourceError(err)
	}
	return c.verifyInstallation(ctx, appToken, installationID, account, required)
}

func (c *Client) verifyInstallation(ctx context.Context, appToken Credential, installationID int64, account AccountIdentity, required Permissions) (Installation, error) {
	if installationID <= 0 || account.validate() != nil {
		return Installation{}, ErrInvalidTokenRequest
	}
	permissions, err := c.normalizePermissions(required)
	if err != nil {
		return Installation{}, err
	}
	var remote apiInstallation
	if err = c.doJSON(ctx, http.MethodGet, appToken, []string{"app", "installations", strconv.FormatInt(installationID, 10)}, nil, nil, http.StatusOK, &remote); err != nil {
		return Installation{}, err
	}
	return c.validateInstallation(remote, installationID, account, permissions)
}

func (c *Client) validateInstallation(remote apiInstallation, installationID int64, account AccountIdentity, required Permissions) (Installation, error) {
	remoteAccount := remote.Account.identity()
	if remote.ID != installationID || remote.AppID != c.config.AppID || (remote.ClientID != "" && remote.ClientID != c.config.ClientID) ||
		remoteAccount.validate() != nil || !remoteAccount.equal(account) {
		return Installation{}, ErrOwnershipMismatch
	}
	if remote.RepositorySelection != "all" && remote.RepositorySelection != "selected" {
		return Installation{}, ErrProviderResponse
	}
	remotePermissions, err := permissionsFromStrings(remote.Permissions, true)
	if err != nil || len(remotePermissions) == 0 {
		return Installation{}, ErrProviderResponse
	}
	for name, level := range required {
		if !permissionAllows(remotePermissions[name], level) {
			return Installation{}, ErrScopeMismatch
		}
	}
	if remote.SuspendedAt != nil {
		return Installation{}, ErrScopeMismatch
	}
	return Installation{
		ID: remote.ID, AppID: remote.AppID, ClientID: remote.ClientID, Account: remoteAccount,
		RepositorySelection: remote.RepositorySelection, Permissions: clonePermissions(remotePermissions), SuspendedAt: remote.SuspendedAt,
	}, nil
}

// SetupVerification binds the callback result to both the immutable GitHub user
// behind the OAuth token and the installation account.
type SetupVerification struct {
	User         AccountIdentity
	Installation Installation
}

// VerifyAuthenticatedUser resolves the exact GitHub user behind a user access
// token. The local identity flow persists User.ID as the immutable association.
func (c *Client) VerifyAuthenticatedUser(ctx context.Context, userToken Credential) (AccountIdentity, error) {
	if userToken.empty() || !validRawCredential(userToken.Reveal()) {
		return AccountIdentity{}, ErrTransport
	}
	return c.verifyAuthenticatedUser(ctx, userToken)
}

func (c *Client) VerifyAuthenticatedUserRaw(ctx context.Context, rawUserToken string) (AccountIdentity, error) {
	userToken, err := credentialFromRaw(rawUserToken)
	if err != nil {
		return AccountIdentity{}, err
	}
	return c.verifyAuthenticatedUser(ctx, userToken)
}

func (c *Client) verifyAuthenticatedUser(ctx context.Context, userToken Credential) (AccountIdentity, error) {
	var remote apiAccount
	if err := c.doJSON(ctx, http.MethodGet, userToken, []string{"user"}, nil, nil, http.StatusOK, &remote); err != nil {
		return AccountIdentity{}, err
	}
	user := remote.identity()
	if user.validate() != nil || user.Type != "User" {
		return AccountIdentity{}, ErrOwnershipMismatch
	}
	return user, nil

}

// VerifySetupInstallation proves that a setup callback's installation id is in
// the expected authenticated user's own installation list. expectedUserID is
// the immutable association previously bound to the local actor; the callback
// parameter alone is never trusted. expectedAccountID may be zero when the user
// chose the installation account on GitHub; otherwise it binds the state choice.
func (c *Client) VerifySetupInstallation(ctx context.Context, userToken Credential, callbackInstallationID, expectedUserID, expectedAccountID int64) (SetupVerification, error) {
	if userToken.empty() || !validRawCredential(userToken.Reveal()) {
		return SetupVerification{}, ErrTransport
	}
	return c.verifySetupInstallation(ctx, userToken, callbackInstallationID, expectedUserID, expectedAccountID)
}

// VerifySetupInstallationRaw is the OAuth-callback-friendly entry point. It
// strictly bounds the user token before wrapping it in the redacting type.
func (c *Client) VerifySetupInstallationRaw(ctx context.Context, rawUserToken string, callbackInstallationID, expectedUserID, expectedAccountID int64) (SetupVerification, error) {
	userToken, err := credentialFromRaw(rawUserToken)
	if err != nil {
		return SetupVerification{}, err
	}
	return c.verifySetupInstallation(ctx, userToken, callbackInstallationID, expectedUserID, expectedAccountID)
}

func (c *Client) verifySetupInstallation(ctx context.Context, userToken Credential, callbackInstallationID, expectedUserID, expectedAccountID int64) (SetupVerification, error) {
	if callbackInstallationID <= 0 || expectedUserID <= 0 || expectedAccountID < 0 {
		return SetupVerification{}, ErrOwnershipMismatch
	}
	user, err := c.verifyAuthenticatedUser(ctx, userToken)
	if err != nil {
		return SetupVerification{}, err
	}
	if user.ID != expectedUserID {
		return SetupVerification{}, ErrOwnershipMismatch
	}
	seenIDs := make(map[int64]struct{})
	var match *apiInstallation
	totalExpected := -1
	totalSeen := 0
	for page := 1; page <= maximumPages; page++ {
		query := url.Values{"per_page": {strconv.Itoa(providerPageSize)}, "page": {strconv.Itoa(page)}}
		var response struct {
			TotalCount    int               `json:"total_count"`
			Installations []apiInstallation `json:"installations"`
		}
		if err := c.doJSON(ctx, http.MethodGet, userToken, []string{"user", "installations"}, query, nil, http.StatusOK, &response); err != nil {
			return SetupVerification{}, err
		}
		if response.TotalCount < 0 || response.TotalCount > maximumPages*providerPageSize || (totalExpected >= 0 && response.TotalCount != totalExpected) {
			return SetupVerification{}, ErrProviderResponse
		}
		if len(response.Installations) > providerPageSize {
			return SetupVerification{}, ErrProviderResponse
		}
		if totalExpected < 0 {
			totalExpected = response.TotalCount
		}
		for i := range response.Installations {
			installation := response.Installations[i]
			if installation.ID <= 0 {
				return SetupVerification{}, ErrProviderResponse
			}
			if _, duplicate := seenIDs[installation.ID]; duplicate {
				return SetupVerification{}, ErrProviderResponse
			}
			seenIDs[installation.ID] = struct{}{}
			totalSeen++
			if installation.ID == callbackInstallationID {
				candidate := installation
				match = &candidate
			}
		}
		if len(response.Installations) < providerPageSize {
			break
		}
	}
	if totalSeen != totalExpected || match == nil {
		return SetupVerification{}, ErrOwnershipMismatch
	}
	account := match.Account.identity()
	if account.validate() != nil || (expectedAccountID > 0 && account.ID != expectedAccountID) {
		return SetupVerification{}, ErrOwnershipMismatch
	}
	// Setup verification does not request a token yet, so only the mandatory
	// metadata scope is checked here. Minting repeats all checks immediately.
	installation, err := c.validateInstallation(*match, callbackInstallationID, account, Permissions{"metadata": PermissionRead})
	if err != nil {
		return SetupVerification{}, err
	}
	return SetupVerification{User: user, Installation: installation}, nil
}

// ListUserInstallationRepositories enumerates only repositories visible to
// the authenticated user for the already verified installation. It is bounded
// to the same 500-repository setup contract persisted by builds.SetupService.
func (c *Client) ListUserInstallationRepositories(ctx context.Context, userToken Credential, installationID int64, account AccountIdentity) ([]RepositoryIdentity, error) {
	if userToken.empty() || !validRawCredential(userToken.Reveal()) || installationID <= 0 || account.validate() != nil {
		return nil, ErrInvalidTokenRequest
	}
	repositories := make([]RepositoryIdentity, 0)
	seen := make(map[int64]struct{})
	totalExpected := -1
	for page := 1; page <= 6; page++ {
		query := url.Values{"per_page": {strconv.Itoa(providerPageSize)}, "page": {strconv.Itoa(page)}}
		var response struct {
			TotalCount   int             `json:"total_count"`
			Repositories []apiRepository `json:"repositories"`
		}
		if err := c.doJSON(ctx, http.MethodGet, userToken, []string{"user", "installations", strconv.FormatInt(installationID, 10), "repositories"}, query, nil, http.StatusOK, &response); err != nil {
			return nil, err
		}
		if response.TotalCount < 0 || response.TotalCount > 500 || totalExpected >= 0 && response.TotalCount != totalExpected || len(response.Repositories) > providerPageSize {
			return nil, ErrProviderResponse
		}
		if totalExpected < 0 {
			totalExpected = response.TotalCount
		}
		for _, raw := range response.Repositories {
			repository, err := raw.identity()
			if err != nil || raw.Owner.Type != account.Type || repository.OwnerID != account.ID || !strings.EqualFold(repository.OwnerLogin, account.Login) {
				return nil, ErrOwnershipMismatch
			}
			if _, duplicate := seen[repository.ID]; duplicate {
				return nil, ErrProviderResponse
			}
			seen[repository.ID] = struct{}{}
			repositories = append(repositories, repository)
		}
		if len(response.Repositories) < providerPageSize {
			break
		}
	}
	if len(repositories) != totalExpected {
		return nil, ErrProviderResponse
	}
	slices.SortFunc(repositories, func(a, b RepositoryIdentity) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return repositories, nil
}

// MintInstallationToken mints a repository-id-scoped token and then enumerates
// the repositories visible through that token. Any broadening fails closed and
// the credential is not returned.
func (c *Client) MintInstallationToken(ctx context.Context, request TokenRequest) (InstallationToken, error) {
	normalized, err := c.validateTokenRequest(request)
	if err != nil {
		return InstallationToken{}, err
	}
	appToken, err := c.appTokens.AppToken(ctx)
	if err != nil {
		return InstallationToken{}, sanitizedTokenSourceError(err)
	}
	if _, err = c.verifyInstallation(ctx, appToken, normalized.InstallationID, normalized.Account, normalized.Permissions); err != nil {
		return InstallationToken{}, err
	}
	repositoryIDs := make([]int64, len(normalized.Repositories))
	for i := range normalized.Repositories {
		repositoryIDs[i] = normalized.Repositories[i].ID
	}
	body := struct {
		RepositoryIDs []int64     `json:"repository_ids"`
		Permissions   Permissions `json:"permissions"`
	}{RepositoryIDs: repositoryIDs, Permissions: normalized.Permissions}
	var response struct {
		Token               string            `json:"token"`
		ExpiresAt           time.Time         `json:"expires_at"`
		Permissions         map[string]string `json:"permissions"`
		RepositorySelection string            `json:"repository_selection"`
		Repositories        []apiRepository   `json:"repositories"`
	}
	path := []string{"app", "installations", strconv.FormatInt(normalized.InstallationID, 10), "access_tokens"}
	if err = c.doJSON(ctx, http.MethodPost, appToken, path, nil, body, http.StatusCreated, &response); err != nil {
		return InstallationToken{}, err
	}
	now := c.clock.Now().UTC()
	credential, err := credentialFromRaw(response.Token)
	if err != nil || !response.ExpiresAt.After(now.Add(30*time.Second)) || response.ExpiresAt.After(now.Add(65*time.Minute)) {
		return InstallationToken{}, ErrProviderResponse
	}
	responsePermissions, err := permissionsFromStrings(response.Permissions, false)
	if err != nil || !equalPermissions(responsePermissions, normalized.Permissions) {
		return InstallationToken{}, ErrScopeMismatch
	}
	if response.RepositorySelection != "selected" {
		return InstallationToken{}, ErrScopeMismatch
	}
	if response.Repositories != nil {
		if err = verifyRepositorySet(response.Repositories, normalized.Repositories, normalized.Account); err != nil {
			return InstallationToken{}, err
		}
	}
	if err = c.verifyTokenRepositoryScope(ctx, credential, normalized.Repositories, normalized.Account); err != nil {
		return InstallationToken{}, err
	}
	return InstallationToken{
		ExpiresAt: response.ExpiresAt.UTC(), credential: credential, installationID: normalized.InstallationID,
		repositoryIDs: repositoryIDs, permissions: clonePermissions(normalized.Permissions),
	}, nil
}

func (c *Client) validateTokenRequest(request TokenRequest) (TokenRequest, error) {
	if request.InstallationID <= 0 || request.Account.validate() != nil || len(request.Repositories) == 0 || len(request.Repositories) > 500 {
		return TokenRequest{}, ErrInvalidTokenRequest
	}
	permissions, err := c.normalizePermissions(request.Permissions)
	if err != nil {
		return TokenRequest{}, err
	}
	repositories := append([]RepositoryIdentity(nil), request.Repositories...)
	slices.SortFunc(repositories, func(a, b RepositoryIdentity) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	seenNames := make(map[string]struct{}, len(repositories))
	for i, repository := range repositories {
		if repository.validate() != nil || repository.OwnerID != request.Account.ID || !strings.EqualFold(repository.OwnerLogin, request.Account.Login) {
			return TokenRequest{}, ErrOwnershipMismatch
		}
		if i > 0 && repositories[i-1].ID == repository.ID {
			return TokenRequest{}, ErrInvalidTokenRequest
		}
		name := strings.ToLower(repository.fullName())
		if _, duplicate := seenNames[name]; duplicate {
			return TokenRequest{}, ErrInvalidTokenRequest
		}
		seenNames[name] = struct{}{}
	}
	return TokenRequest{
		InstallationID: request.InstallationID, Account: request.Account,
		Repositories: repositories, Permissions: permissions,
	}, nil
}

func (c *Client) normalizePermissions(request Permissions) (Permissions, error) {
	if len(request) == 0 || len(request) > 64 || validatePermissions(request, false) != nil || request["metadata"] != PermissionRead {
		return nil, ErrInvalidTokenRequest
	}
	normalized := clonePermissions(request)
	for name, requested := range normalized {
		maximum, allowed := c.config.MaximumTokenPermissions[name]
		if !allowed || !permissionAllows(maximum, requested) {
			return nil, ErrScopeMismatch
		}
	}
	return normalized, nil
}

func permissionAllows(have, requested PermissionLevel) bool {
	rank := func(level PermissionLevel) int {
		switch level {
		case PermissionRead:
			return 1
		case PermissionWrite:
			return 2
		default:
			return 0
		}
	}
	return rank(have) >= rank(requested) && rank(requested) > 0
}

func clonePermissions(source Permissions) Permissions {
	result := make(Permissions, len(source))
	for name, level := range source {
		result[name] = level
	}
	return result
}

func equalPermissions(left, right Permissions) bool {
	if len(left) != len(right) {
		return false
	}
	for name, level := range left {
		if right[name] != level {
			return false
		}
	}
	return true
}

type apiRepository struct {
	ID       int64      `json:"id"`
	Name     string     `json:"name"`
	FullName string     `json:"full_name"`
	Owner    apiAccount `json:"owner"`
}

func (r apiRepository) identity() (RepositoryIdentity, error) {
	owner := r.Owner.identity()
	identity := RepositoryIdentity{ID: r.ID, Name: r.Name, OwnerID: r.Owner.ID, OwnerLogin: r.Owner.Login}
	if owner.validate() != nil || identity.validate() != nil || !strings.EqualFold(r.FullName, identity.fullName()) {
		return RepositoryIdentity{}, ErrProviderResponse
	}
	return identity, nil
}

func (c *Client) verifyTokenRepositoryScope(ctx context.Context, token Credential, expected []RepositoryIdentity, account AccountIdentity) error {
	seen := make([]apiRepository, 0, len(expected))
	totalExpected := -1
	for page := 1; page <= 6; page++ {
		query := url.Values{"per_page": {strconv.Itoa(providerPageSize)}, "page": {strconv.Itoa(page)}}
		var response struct {
			TotalCount   int             `json:"total_count"`
			Repositories []apiRepository `json:"repositories"`
		}
		if err := c.doJSON(ctx, http.MethodGet, token, []string{"installation", "repositories"}, query, nil, http.StatusOK, &response); err != nil {
			return err
		}
		if response.TotalCount < 0 || response.TotalCount > 500 || (totalExpected >= 0 && response.TotalCount != totalExpected) {
			return ErrProviderResponse
		}
		if len(response.Repositories) > providerPageSize {
			return ErrProviderResponse
		}
		if totalExpected < 0 {
			totalExpected = response.TotalCount
		}
		seen = append(seen, response.Repositories...)
		if len(response.Repositories) < providerPageSize {
			break
		}
	}
	if len(seen) != totalExpected {
		return ErrProviderResponse
	}
	return verifyRepositorySet(seen, expected, account)
}

func verifyRepositorySet(remote []apiRepository, expected []RepositoryIdentity, account AccountIdentity) error {
	if len(remote) != len(expected) {
		return ErrScopeMismatch
	}
	expectedByID := make(map[int64]RepositoryIdentity, len(expected))
	for _, repository := range expected {
		expectedByID[repository.ID] = repository
	}
	seen := make(map[int64]struct{}, len(remote))
	for _, raw := range remote {
		repository, err := raw.identity()
		if err != nil {
			return err
		}
		wanted, ok := expectedByID[repository.ID]
		if !ok || raw.Owner.Type != account.Type || repository.OwnerID != account.ID || !strings.EqualFold(repository.OwnerLogin, account.Login) ||
			repository.Name != wanted.Name || repository.OwnerID != wanted.OwnerID || !strings.EqualFold(repository.OwnerLogin, wanted.OwnerLogin) {
			return ErrOwnershipMismatch
		}
		if _, duplicate := seen[repository.ID]; duplicate {
			return ErrProviderResponse
		}
		seen[repository.ID] = struct{}{}
	}
	return nil
}

func sanitizedTokenSourceError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrSecretUnavailable) || errors.Is(err, ErrInvalidPrivateKey) {
		return err
	}
	return ErrTransport
}

// ResolvedRef is an authoritative remote ref resolution. CommitSHA comes from
// GitHub's exact ref endpoint (and annotated-tag peeling), never a webhook body.
type ResolvedRef struct {
	Ref        string
	CommitSHA  string
	ResolvedAt time.Time
}

// ResolveEventRef resolves the exact ref named by a verified push event. The
// event's UntrustedAfter value is intentionally not read.
func (c *Client) ResolveEventRef(ctx context.Context, token InstallationToken, event PushEvent) (ResolvedRef, error) {
	if event.Deleted {
		return ResolvedRef{}, ErrRefDeleted
	}
	if event.InstallationID != token.installationID || !token.authorizes(event.Repository.ID) {
		return ResolvedRef{}, ErrScopeMismatch
	}
	return c.ResolveRemoteRef(ctx, token, event.Repository, event.Ref)
}

// ResolveRemoteRef first revalidates immutable repository identity, then calls
// GitHub's exact single-ref endpoint. Ambiguous prefix/list endpoints are never
// used.
func (c *Client) ResolveRemoteRef(ctx context.Context, token InstallationToken, repository RepositoryIdentity, fullRef string) (ResolvedRef, error) {
	if token.credential.empty() || !c.clock.Now().UTC().Before(token.ExpiresAt) || !token.authorizes(repository.ID) || repository.validate() != nil ||
		!permissionAllows(token.permissions["contents"], PermissionRead) {
		return ResolvedRef{}, ErrScopeMismatch
	}
	kind, name, ok := splitFullRef(fullRef)
	if !ok {
		return ResolvedRef{}, ErrInvalidTokenRequest
	}
	if err := c.verifyRepositoryIdentity(ctx, token.credential, repository); err != nil {
		return ResolvedRef{}, err
	}
	segments := []string{"repos", repository.OwnerLogin, repository.Name, "git", "ref", kind}
	segments = append(segments, strings.Split(name, "/")...)
	var response struct {
		Ref    string `json:"ref"`
		Object struct {
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"object"`
	}
	if err := c.doJSON(ctx, http.MethodGet, token.credential, segments, nil, nil, http.StatusOK, &response); err != nil {
		return ResolvedRef{}, err
	}
	if response.Ref != fullRef || !objectIDPattern.MatchString(response.Object.SHA) {
		return ResolvedRef{}, ErrProviderResponse
	}
	commitSHA := response.Object.SHA
	if kind == "heads" {
		if response.Object.Type != "commit" {
			return ResolvedRef{}, ErrProviderResponse
		}
		if !builderObjectIDPattern.MatchString(commitSHA) {
			return ResolvedRef{}, ErrUnsupportedObjectFormat
		}
	} else {
		switch response.Object.Type {
		case "commit":
			if !builderObjectIDPattern.MatchString(commitSHA) {
				return ResolvedRef{}, ErrUnsupportedObjectFormat
			}
		case "tag":
			var err error
			commitSHA, err = c.peelTag(ctx, token.credential, repository, response.Object.SHA)
			if err != nil {
				return ResolvedRef{}, err
			}
		default:
			return ResolvedRef{}, ErrProviderResponse
		}
	}
	return ResolvedRef{Ref: fullRef, CommitSHA: commitSHA, ResolvedAt: c.clock.Now().UTC()}, nil
}

func (c *Client) verifyRepositoryIdentity(ctx context.Context, token Credential, expected RepositoryIdentity) error {
	var remote apiRepository
	if err := c.doJSON(ctx, http.MethodGet, token, []string{"repositories", strconv.FormatInt(expected.ID, 10)}, nil, nil, http.StatusOK, &remote); err != nil {
		return err
	}
	identity, err := remote.identity()
	if err != nil {
		return err
	}
	if identity.ID != expected.ID || identity.OwnerID != expected.OwnerID || identity.Name != expected.Name || !strings.EqualFold(identity.OwnerLogin, expected.OwnerLogin) {
		return ErrOwnershipMismatch
	}
	return nil
}

func splitFullRef(fullRef string) (string, string, bool) {
	if strings.HasPrefix(fullRef, "refs/heads/") {
		name := strings.TrimPrefix(fullRef, "refs/heads/")
		return "heads", name, validRefName(name)
	}
	if strings.HasPrefix(fullRef, "refs/tags/") {
		name := strings.TrimPrefix(fullRef, "refs/tags/")
		return "tags", name, validRefName(name)
	}
	return "", "", false
}

func (c *Client) peelTag(ctx context.Context, token Credential, repository RepositoryIdentity, initialSHA string) (string, error) {
	sha := initialSHA
	for range 5 {
		var response struct {
			SHA    string `json:"sha"`
			Object struct {
				Type string `json:"type"`
				SHA  string `json:"sha"`
			} `json:"object"`
		}
		segments := []string{"repos", repository.OwnerLogin, repository.Name, "git", "tags", sha}
		if err := c.doJSON(ctx, http.MethodGet, token, segments, nil, nil, http.StatusOK, &response); err != nil {
			return "", err
		}
		if response.SHA != sha || !objectIDPattern.MatchString(response.Object.SHA) {
			return "", ErrProviderResponse
		}
		switch response.Object.Type {
		case "commit":
			if !builderObjectIDPattern.MatchString(response.Object.SHA) {
				return "", ErrUnsupportedObjectFormat
			}
			return response.Object.SHA, nil
		case "tag":
			sha = response.Object.SHA
		default:
			return "", ErrProviderResponse
		}
	}
	return "", fmt.Errorf("%w: annotated tag nesting exceeds limit", ErrProviderResponse)
}
