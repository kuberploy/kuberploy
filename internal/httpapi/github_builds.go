package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kuberploy/kuberploy/internal/builder"
	"github.com/kuberploy/kuberploy/internal/builds"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/githubapp"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/gitssh"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/store"
)

type GitHubSetupBackend interface {
	Begin(context.Context, builds.BeginSetupRequest) (builds.BeginSetupResult, error)
	Continue(context.Context, builds.ContinueSetupRequest) (builds.ContinueSetupResult, error)
	Complete(context.Context, builds.CompleteSetupRequest) (builds.CompleteSetupResult, error)
	Link(context.Context, builds.LinkSetupRequest) (builds.LinkSetupResult, error)
}

type GitHubWebhookBackend interface {
	Accept(context.Context, http.Header, io.Reader) (builds.WebhookOutcome, error)
}

type BuildDefinitionResolution struct {
	Registry  builds.RegistryBinding
	Execution builds.ExecutionSettings
}

// BuildDefinitionResolver supplies operator-owned execution and registry
// settings. Public requests can select only an already-authorized target ID;
// Kubernetes namespaces, Secret names, images and egress never come from JSON.
type BuildDefinitionResolver interface {
	ResolveBuildDefinition(context.Context, string, string, string, string) (BuildDefinitionResolution, error)
}

type BuildSecretProfileResolver interface {
	SecretProfileCatalog(string) (builds.BuildSecretProfileCatalog, error)
	ResolveSecretProfiles(string, []string, []string) (builds.BuildSecretSelection, error)
	ResolveSecretFiles(string, []builder.FileReference, []builder.FileReference) (builds.BuildSecretSelection, error)
}

type BuildSecretProfileCatalog interface {
	SecretProfileCatalog(context.Context, string) (builds.BuildSecretProfileCatalog, error)
}

type BuildDefinitionMutation struct {
	ApplicationID     string
	ProjectID         string
	InstallationID    string
	RepositoryID      string
	SourceKind        builds.SourceKind
	RepositoryURL     string
	GitSSHKeyScope    gitssh.Scope
	GitSSHKeyRevision uint64
	HostKeyPins       []gitssh.HostKeyPin
	RegistryTargetID  string
	TriggerRef        string
	ContextPath       string
	DockerfilePath    string
	Platforms         []string
	BuildArgs         []builder.BuildArg
	SecretFiles       []builder.FileReference
	SSHFiles          []builder.FileReference
	SecretProfileIDs  []string
	SSHProfileIDs     []string
	CacheTrustLane    string
	CacheImports      int
	Profile           builder.BuildProfile
	MaxAttempts       int
	ActorID           string
	IdempotencyKey    string
	Fingerprint       string
}

type BuildBackend interface {
	CreateDefinition(context.Context, BuildDefinitionMutation) (builds.BuildDefinition, bool, error)
	DeleteDefinition(context.Context, string, string, string, string, string, string) (bool, error)
	Definition(context.Context, string) (builds.BuildDefinition, error)
	Definitions(context.Context, string) ([]builds.BuildDefinition, error)
	Repositories(context.Context, string) ([]builds.Repository, error)
	Attempt(context.Context, string) (builds.BuildAttempt, error)
	Attempts(context.Context, string, int) ([]builds.BuildAttempt, error)
	Cancel(context.Context, string, string, string, string) (builds.BuildAttempt, bool, error)
	Retry(context.Context, string, string, string, string) (builds.BuildAttempt, bool, error)
	Build(context.Context, string, string, string, string, string) (builds.BuildAttempt, bool, error)
}

func (b *buildBackend) DeleteDefinition(ctx context.Context, actorID, applicationID, definitionID, key, fingerprint, requestID string) (bool, error) {
	return b.store.DeleteDefinition(ctx, actorID, applicationID, definitionID, key, fingerprint, requestID, b.clock())
}

type GitBindingRepositoryResolution struct {
	GitHubAppID int64
	Repository  gitprojection.RepositoryIdentity
}

// GitBindingRepositoryResolver is deliberately narrower than BuildBackend so
// test backends and future external builders cannot accidentally become Git
// authority catalogs. The production build backend resolves only exact linked
// installation/repository IDs from its verified GitHub App catalog.
type GitBindingRepositoryResolver interface {
	ResolveGitBindingRepository(context.Context, string, string) (GitBindingRepositoryResolution, error)
}

type buildBackend struct {
	store    builds.APIStore
	resolver BuildDefinitionResolver
	now      func() time.Time
}

func NewBuildBackend(store builds.APIStore, resolver BuildDefinitionResolver) (BuildBackend, error) {
	if store == nil || resolver == nil {
		return nil, builds.ErrInvalid
	}
	return &buildBackend{store: store, resolver: resolver}, nil
}

func NewBuildBackendWithClock(store builds.APIStore, resolver BuildDefinitionResolver, now func() time.Time) (BuildBackend, error) {
	if store == nil || resolver == nil || now == nil {
		return nil, builds.ErrInvalid
	}
	return &buildBackend{store: store, resolver: resolver, now: now}, nil
}

func (b *buildBackend) ResolveGitBindingRepository(ctx context.Context, installationID, repositoryID string) (GitBindingRepositoryResolution, error) {
	installation, err := b.store.Installation(ctx, installationID)
	if err != nil {
		return GitBindingRepositoryResolution{}, err
	}
	repository, err := b.store.Repository(ctx, repositoryID)
	if err != nil {
		return GitBindingRepositoryResolution{}, err
	}
	if installation.Lifecycle != builds.InstallationActive || repository.Lifecycle != builds.RepositoryActive ||
		repository.InstallationID != installation.ID || installation.AppID <= 0 || installation.GitHubInstallationID <= 0 ||
		repository.Identity.OwnerID != installation.Account.ID || !strings.EqualFold(repository.Identity.OwnerLogin, installation.Account.Login) {
		return GitBindingRepositoryResolution{}, builds.ErrUnauthorized
	}
	resolved := GitBindingRepositoryResolution{GitHubAppID: installation.AppID, Repository: gitprojection.RepositoryIdentity{
		Provider: "github", InstallationID: installation.GitHubInstallationID, RepositoryID: repository.Identity.ID,
		Owner: repository.Identity.OwnerLogin, Name: repository.Identity.Name,
	}}
	if resolved.Repository.Validate() != nil {
		return GitBindingRepositoryResolution{}, builds.ErrInfrastructure
	}
	return resolved, nil
}

var _ GitBindingRepositoryResolver = (*buildBackend)(nil)

func (b *buildBackend) CreateDefinition(ctx context.Context, input BuildDefinitionMutation) (builds.BuildDefinition, bool, error) {
	now := b.clock()
	resourceID, replay, err := b.store.ClaimAPICommand(ctx, input.ActorID, builds.APICommandDefinitionCreate, input.ApplicationID,
		input.IdempotencyKey, input.Fingerprint, id.New(), now)
	if err != nil {
		return builds.BuildDefinition{}, false, err
	}
	if replay {
		definition, getErr := b.store.Definition(ctx, resourceID)
		if getErr == nil {
			if definition.ServiceID != input.ApplicationID || definition.ProjectID != input.ProjectID {
				return builds.BuildDefinition{}, false, builds.ErrConflict
			}
			return definition, true, nil
		}
		if !errors.Is(getErr, builds.ErrNotFound) {
			return builds.BuildDefinition{}, false, getErr
		}
	}
	resolution, err := b.resolver.ResolveBuildDefinition(ctx, input.ActorID, input.ProjectID, input.ApplicationID, input.RegistryTargetID)
	if err != nil {
		return builds.BuildDefinition{}, false, err
	}
	if resolution.Registry.TargetID != input.RegistryTargetID {
		return builds.BuildDefinition{}, false, builds.ErrUnauthorized
	}
	if len(input.SecretProfileIDs) > 0 || len(input.SSHProfileIDs) > 0 {
		profileResolver, ok := b.resolver.(BuildSecretProfileResolver)
		if !ok {
			return builds.BuildDefinition{}, false, builds.ErrInfrastructure
		}
		selection, selectionErr := profileResolver.ResolveSecretProfiles(input.ApplicationID, input.SecretProfileIDs, input.SSHProfileIDs)
		if selectionErr != nil {
			return builds.BuildDefinition{}, false, builds.ErrInvalid
		}
		input.SecretFiles, input.SSHFiles = selection.SecretFiles, selection.SSHFiles
		resolution.Execution.BuildSecret, resolution.Execution.SSHSecret = selection.BuildSecret, selection.SSHSecret
	}
	if input.SourceKind == "" {
		input.SourceKind = builds.SourceGitHub
	}
	var gitSSHSource *builds.GitSSHSource
	if input.SourceKind == builds.SourceGitHub {
		repository, repositoryErr := b.store.Repository(ctx, input.RepositoryID)
		if repositoryErr != nil {
			return builds.BuildDefinition{}, false, repositoryErr
		}
		installation, installationErr := b.store.Installation(ctx, input.InstallationID)
		if installationErr != nil {
			return builds.BuildDefinition{}, false, installationErr
		}
		if repository.InstallationID != installation.ID || installation.Lifecycle != builds.InstallationActive || repository.Lifecycle != builds.RepositoryActive {
			return builds.BuildDefinition{}, false, builds.ErrUnauthorized
		}
	} else if input.SourceKind == builds.SourceGitSSH {
		u, parseErr := url.Parse(input.RepositoryURL)
		if parseErr != nil || u.Scheme != "ssh" || u.Hostname() == "" {
			return builds.BuildDefinition{}, false, builds.ErrInvalid
		}
		port := u.Port()
		if port == "" {
			port = "22"
		}
		portNumber, portErr := strconv.Atoi(port)
		if portErr != nil || portNumber < 1 || portNumber > 65535 {
			return builds.BuildDefinition{}, false, builds.ErrInvalid
		}
		expectedEndpoint := net.JoinHostPort(u.Hostname(), port)
		for _, pin := range input.HostKeyPins {
			if pin.Endpoint != expectedEndpoint {
				return builds.BuildDefinition{}, false, builds.ErrInvalid
			}
		}
		knownHosts, knownHostsErr := gitssh.KnownHosts(input.HostKeyPins)
		if knownHostsErr != nil {
			return builds.BuildDefinition{}, false, builds.ErrInvalid
		}
		keyOwnerID := input.ApplicationID
		if input.GitSSHKeyScope == gitssh.ScopeProject {
			keyOwnerID = input.ProjectID
		} else if input.GitSSHKeyScope != gitssh.ScopeApp {
			return builds.BuildDefinition{}, false, builds.ErrInvalid
		}
		gitSSHSource = &builds.GitSSHSource{RepositoryURL: input.RepositoryURL, ApprovedHost: u.Host,
			KeyScope: string(input.GitSSHKeyScope), KeyOwnerID: keyOwnerID, KeyRevision: input.GitSSHKeyRevision, KnownHosts: string(knownHosts)}
		resolution.Execution = addSourcePort(resolution.Execution, portNumber)
		input.InstallationID, input.RepositoryID = "", ""
	} else {
		return builds.BuildDefinition{}, false, builds.ErrInvalid
	}
	definition, err := builds.PrepareDefinition(builds.BuildDefinition{ID: resourceID, ProjectID: input.ProjectID, ServiceID: input.ApplicationID,
		SourceKind: input.SourceKind, InstallationID: input.InstallationID, RepositoryID: input.RepositoryID, GitSSH: gitSSHSource, TriggerRef: input.TriggerRef, Enabled: true,
		DefinitionGeneration: 1, Spec: builds.DefinitionSpec{ContextPath: input.ContextPath, DockerfilePath: input.DockerfilePath,
			Platforms: append([]string(nil), input.Platforms...), Registry: resolution.Registry, BuildArgs: append([]builder.BuildArg(nil), input.BuildArgs...),
			SecretFiles: append([]builder.FileReference(nil), input.SecretFiles...), SSHFiles: append([]builder.FileReference(nil), input.SSHFiles...),
			CacheTrustLane: input.CacheTrustLane, CacheImports: input.CacheImports, Profile: input.Profile,
			Execution: resolution.Execution, MaxAttempts: input.MaxAttempts}}, now)
	if err != nil {
		return builds.BuildDefinition{}, false, err
	}
	if err = b.store.PutDefinition(ctx, definition); err != nil {
		return builds.BuildDefinition{}, false, err
	}
	return definition, replay, nil
}

func (b *buildBackend) Definitions(ctx context.Context, applicationID string) ([]builds.BuildDefinition, error) {
	return b.store.DefinitionsForService(ctx, applicationID)
}
func (b *buildBackend) Definition(ctx context.Context, definitionID string) (builds.BuildDefinition, error) {
	return b.store.Definition(ctx, definitionID)
}
func (b *buildBackend) Repositories(ctx context.Context, installationID string) ([]builds.Repository, error) {
	return b.store.ListRepositories(ctx, installationID)
}
func (b *buildBackend) Attempt(ctx context.Context, attemptID string) (builds.BuildAttempt, error) {
	return b.store.HistoricalAttempt(ctx, attemptID)
}
func (b *buildBackend) Attempts(ctx context.Context, applicationID string, limit int) ([]builds.BuildAttempt, error) {
	return b.store.AttemptsForService(ctx, applicationID, limit)
}
func (b *buildBackend) Cancel(ctx context.Context, actorID, attemptID, key, fingerprint string) (builds.BuildAttempt, bool, error) {
	now := b.clock()
	attempt, err := b.store.Attempt(ctx, attemptID)
	if err != nil {
		return builds.BuildAttempt{}, false, err
	}
	_, replay, err := b.store.ClaimAPICommand(ctx, actorID, builds.APICommandAttemptCancel, attemptID, key, fingerprint, attemptID, now)
	if err != nil {
		return builds.BuildAttempt{}, false, err
	}
	if replay {
		return attempt, true, nil
	}
	if attempt.State == builds.AttemptSucceeded || attempt.State == builds.AttemptFailed || attempt.State == builds.AttemptCancelled {
		return builds.BuildAttempt{}, false, builds.ErrTerminal
	}
	attempt, err = b.store.RequestCancel(ctx, attemptID, now)
	return attempt, replay, err
}
func (b *buildBackend) Retry(ctx context.Context, actorID, sourceAttemptID, key, fingerprint string) (builds.BuildAttempt, bool, error) {
	source, err := b.store.Attempt(ctx, sourceAttemptID)
	if err != nil {
		return builds.BuildAttempt{}, false, err
	}
	claimKey := builds.APICommandClaimKey(actorID, builds.APICommandAttemptRetry, sourceAttemptID, key)
	retryID := builds.RetryAttemptID(claimKey, source.DefinitionID)
	now := b.clock()
	resourceID, commandReplay, err := b.store.ClaimAPICommand(ctx, actorID, builds.APICommandAttemptRetry, sourceAttemptID, key, fingerprint, retryID, now)
	if err != nil {
		return builds.BuildAttempt{}, false, err
	}
	if resourceID != retryID {
		return builds.BuildAttempt{}, false, builds.ErrConflict
	}
	if commandReplay {
		if existing, getErr := b.store.Attempt(ctx, retryID); getErr == nil {
			if existing.TriggerKey != claimKey || existing.DefinitionID != source.DefinitionID {
				return builds.BuildAttempt{}, false, builds.ErrConflict
			}
			return existing, true, nil
		} else if !errors.Is(getErr, builds.ErrNotFound) {
			return builds.BuildAttempt{}, false, getErr
		}
	}
	definition, err := b.store.Definition(ctx, source.DefinitionID)
	if err != nil {
		return builds.BuildAttempt{}, false, err
	}
	resolution, err := b.resolver.ResolveBuildDefinition(ctx, actorID, definition.ProjectID, definition.ServiceID, definition.Spec.Registry.TargetID)
	if err != nil {
		return builds.BuildAttempt{}, false, err
	}
	if resolution.Registry.TargetID != definition.Spec.Registry.TargetID {
		return builds.BuildAttempt{}, false, builds.ErrUnauthorized
	}
	if len(definition.Spec.SecretFiles) > 0 || len(definition.Spec.SSHFiles) > 0 {
		profileResolver, ok := b.resolver.(BuildSecretProfileResolver)
		if !ok {
			return builds.BuildAttempt{}, false, builds.ErrInfrastructure
		}
		selection, selectionErr := profileResolver.ResolveSecretFiles(definition.ServiceID, definition.Spec.SecretFiles, definition.Spec.SSHFiles)
		if selectionErr != nil {
			return builds.BuildAttempt{}, false, builds.ErrInfrastructure
		}
		resolution.Execution.BuildSecret, resolution.Execution.SSHSecret = selection.BuildSecret, selection.SSHSecret
	}
	if definition.SourceKind == builds.SourceGitSSH {
		resolution.Execution, err = executionForGitSSHSource(resolution.Execution, definition.GitSSH)
		if err != nil {
			return builds.BuildAttempt{}, false, err
		}
	}
	attempt, retryReplay, err := b.store.RetryAttempt(ctx, sourceAttemptID, retryID, claimKey, resolution.Execution, now)
	return attempt, commandReplay || retryReplay, err
}

func (b *buildBackend) Build(ctx context.Context, actorID, definitionID, commitSHA, key, fingerprint string) (builds.BuildAttempt, bool, error) {
	definition, err := b.store.Definition(ctx, definitionID)
	if err != nil {
		return builds.BuildAttempt{}, false, err
	}
	if definition.SourceKind != builds.SourceGitSSH || definition.GitSSH == nil {
		return builds.BuildAttempt{}, false, builds.ErrInvalid
	}
	claimKey := builds.APICommandClaimKey(actorID, builds.APICommandDefinitionBuild, definitionID, key)
	attemptID := builds.ManualAttemptID(claimKey, definitionID)
	now := b.clock()
	resourceID, commandReplay, err := b.store.ClaimAPICommand(ctx, actorID, builds.APICommandDefinitionBuild, definitionID, key, fingerprint, attemptID, now)
	if err != nil || resourceID != attemptID {
		if err == nil {
			err = builds.ErrConflict
		}
		return builds.BuildAttempt{}, false, err
	}
	if commandReplay {
		if existing, getErr := b.store.Attempt(ctx, attemptID); getErr == nil {
			if existing.TriggerKind != "manual" || existing.TriggerKey != claimKey || existing.DefinitionID != definitionID || existing.CommitSHA != commitSHA {
				return builds.BuildAttempt{}, false, builds.ErrConflict
			}
			return existing, true, nil
		} else if !errors.Is(getErr, builds.ErrNotFound) {
			return builds.BuildAttempt{}, false, getErr
		}
	}
	resolution, err := b.resolver.ResolveBuildDefinition(ctx, actorID, definition.ProjectID, definition.ServiceID, definition.Spec.Registry.TargetID)
	if err != nil || resolution.Registry.TargetID != definition.Spec.Registry.TargetID {
		if err == nil {
			err = builds.ErrUnauthorized
		}
		return builds.BuildAttempt{}, false, err
	}
	if len(definition.Spec.SecretFiles) > 0 || len(definition.Spec.SSHFiles) > 0 {
		profileResolver, ok := b.resolver.(BuildSecretProfileResolver)
		if !ok {
			return builds.BuildAttempt{}, false, builds.ErrInfrastructure
		}
		selection, selectionErr := profileResolver.ResolveSecretFiles(definition.ServiceID, definition.Spec.SecretFiles, definition.Spec.SSHFiles)
		if selectionErr != nil {
			return builds.BuildAttempt{}, false, builds.ErrInfrastructure
		}
		resolution.Execution.BuildSecret, resolution.Execution.SSHSecret = selection.BuildSecret, selection.SSHSecret
	}
	resolution.Execution, err = executionForGitSSHSource(resolution.Execution, definition.GitSSH)
	if err != nil {
		return builds.BuildAttempt{}, false, err
	}
	attempt, replay, err := b.store.EnqueueManualAttempt(ctx, definitionID, commitSHA, claimKey, resolution.Execution, now)
	return attempt, commandReplay || replay, err
}

func executionForGitSSHSource(execution builds.ExecutionSettings, source *builds.GitSSHSource) (builds.ExecutionSettings, error) {
	if source == nil {
		return builds.ExecutionSettings{}, builds.ErrInvalid
	}
	u, err := url.Parse(source.RepositoryURL)
	if err != nil {
		return builds.ExecutionSettings{}, builds.ErrInvalid
	}
	port := 22
	if u.Port() != "" {
		port, err = strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return builds.ExecutionSettings{}, builds.ErrInvalid
		}
	}
	return addSourcePort(execution, port), nil
}

func addSourcePort(execution builds.ExecutionSettings, port int) builds.ExecutionSettings {
	execution.Egress = append([]builder.EgressEndpoint(nil), execution.Egress...)
	for index := range execution.Egress {
		execution.Egress[index].Ports = append([]int(nil), execution.Egress[index].Ports...)
		if !slices.Contains(execution.Egress[index].Ports, port) {
			execution.Egress[index].Ports = append(execution.Egress[index].Ports, port)
			slices.Sort(execution.Egress[index].Ports)
		}
	}
	return execution
}
func (b *buildBackend) clock() time.Time {
	if b.now != nil {
		return b.now().UTC()
	}
	return time.Now().UTC()
}

type beginGitHubSetupRequest struct {
	ExpectedAccountID      *int64 `json:"expectedAccountId,omitempty"`
	ExistingInstallationID *int64 `json:"existingInstallationId,omitempty"`
	ReturnKey              string `json:"returnKey"`
}

type githubSetupAuthorizationView struct {
	AuthorizationURL string    `json:"authorizationUrl"`
	State            string    `json:"state"`
	ExpiresAt        time.Time `json:"expiresAt"`
}

type githubIdentityView struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}

type githubRepositoryView struct {
	ID                 string `json:"id,omitempty"`
	GitHubRepositoryID int64  `json:"githubRepositoryId"`
	InstallationID     string `json:"installationId,omitempty"`
	OwnerID            int64  `json:"ownerId"`
	OwnerLogin         string `json:"ownerLogin"`
	Name               string `json:"name"`
	Lifecycle          string `json:"lifecycle,omitempty"`
}

type linkedGitHubSetupView struct {
	Installation domain.GitHubInstallation `json:"installation"`
	Repositories []githubRepositoryView    `json:"repositories"`
}

const githubOAuthIssuer = "https://github.com/login/oauth"

func (s *Server) setGitHubSetupSessionCookie(w http.ResponseWriter, r *http.Request, sourceName, targetPath string) bool {
	cookies := r.CookiesNamed(sourceName)
	if len(cookies) != 1 {
		writeProblem(w, r, http.StatusUnauthorized, "Unauthenticated", "Authentication required", "The GitHub setup browser session is invalid or expired.")
		return false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(cookies[0].Value)
	valid := err == nil && len(raw) == 32
	for index := range raw {
		raw[index] = 0
	}
	if !valid {
		writeProblem(w, r, http.StatusUnauthorized, "Unauthenticated", "Authentication required", "The GitHub setup browser session is invalid or expired.")
		return false
	}
	if targetPath != githubInstallReturnCookiePath && targetPath != githubOAuthCallbackCookiePath {
		writeProblem(w, r, http.StatusInternalServerError, "InternalError", "Internal error", "The GitHub setup browser session could not be scoped safely.")
		return false
	}
	http.SetCookie(w, &http.Cookie{Name: githubSetupSessionCookie, Value: cookies[0].Value, Path: targetPath,
		HttpOnly: true, Secure: s.secureCookie, SameSite: http.SameSiteLaxMode, MaxAge: int(githubSetupCookieTTL.Seconds()),
		Expires: time.Now().UTC().Add(githubSetupCookieTTL)})
	return true
}

func (s *Server) clearGitHubSetupSessionCookie(w http.ResponseWriter, path string) {
	http.SetCookie(w, &http.Cookie{Name: githubSetupSessionCookie, Path: path, HttpOnly: true,
		Secure: s.secureCookie, SameSite: http.SameSiteLaxMode, MaxAge: -1, Expires: time.Unix(1, 0).UTC()})
}

func (s *Server) setGitHubSetupHandoffCookie(w http.ResponseWriter, handoff string, expiresAt time.Time) bool {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(handoff)
	now := time.Now().UTC()
	valid := err == nil && len(raw) == 32 && expiresAt.After(now)
	clear(raw)
	if !valid {
		return false
	}
	maximumExpiry := now.Add(githubSetupCookieTTL)
	if expiresAt.After(maximumExpiry) {
		expiresAt = maximumExpiry
	}
	maxAge := int(expiresAt.Sub(now).Seconds())
	if maxAge < 1 {
		return false
	}
	http.SetCookie(w, &http.Cookie{Name: githubSetupHandoffCookie, Value: handoff, Path: githubSetupLinkCookiePath,
		HttpOnly: true, Secure: s.secureCookie, SameSite: http.SameSiteStrictMode, MaxAge: maxAge, Expires: expiresAt.UTC()})
	return true
}

func (s *Server) clearGitHubSetupHandoffCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: githubSetupHandoffCookie, Path: githubSetupLinkCookiePath, HttpOnly: true,
		Secure: s.secureCookie, SameSite: http.SameSiteStrictMode, MaxAge: -1, Expires: time.Unix(1, 0).UTC()})
}

func githubSetupHandoff(r *http.Request) (string, bool) {
	cookies := r.CookiesNamed(githubSetupHandoffCookie)
	if len(cookies) != 1 {
		return "", false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(cookies[0].Value)
	valid := err == nil && len(raw) == 32
	clear(raw)
	return cookies[0].Value, valid
}

func (s *Server) beginGitHubSetup(w http.ResponseWriter, r *http.Request) {
	key, ok := githubBuildIdempotencyKey(w, r)
	if !ok {
		return
	}
	if s.githubSetup == nil {
		githubBuildUnavailable(w, r, "GitHub App setup is not configured.")
		return
	}
	var input beginGitHubSetupRequest
	if !decodeGitHubBuildJSON(w, r, &input) {
		return
	}
	input.ReturnKey = strings.TrimSpace(input.ReturnKey)
	expectedAccountID := int64(0)
	if input.ExpectedAccountID != nil {
		if *input.ExpectedAccountID <= 0 {
			writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "expectedAccountId must be a positive integer when provided.")
			return
		}
		expectedAccountID = *input.ExpectedAccountID
	}
	existingInstallationID := int64(0)
	if input.ExistingInstallationID != nil {
		if *input.ExistingInstallationID <= 0 {
			writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "existingInstallationId must be a positive integer when provided.")
			return
		}
		existingInstallationID = *input.ExistingInstallationID
	}
	result, err := s.githubSetup.Begin(r.Context(), builds.BeginSetupRequest{ActorID: currentUser(r.Context()).ID,
		ExpectedAccountID: expectedAccountID, ExistingInstallationID: existingInstallationID, ReturnKey: input.ReturnKey, IdempotencyKey: key,
		RequestFingerprint: "sha256:" + fingerprint(input)})
	if err != nil {
		mappedGitHubBuildError(w, r, err)
		return
	}
	s.clearGitHubSetupSessionCookie(w, githubOAuthCallbackCookiePath)
	if !s.setGitHubSetupSessionCookie(w, r, sessionCookie, githubInstallReturnCookiePath) {
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeJSON(w, http.StatusOK, githubSetupAuthorizationView{AuthorizationURL: result.AuthorizationURL, State: result.State, ExpiresAt: result.ExpiresAt})
}

func (s *Server) continueGitHubSetup(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	if s.githubSetup == nil {
		githubBuildUnavailable(w, r, "GitHub App setup is not configured.")
		return
	}
	query := r.URL.Query()
	for key := range query {
		if key != "state" && key != "installation_id" && key != "setup_action" {
			writeProblem(w, r, http.StatusBadRequest, "InvalidGitHubSetupReturn", "GitHub setup return rejected", "The GitHub setup return parameters are invalid.")
			return
		}
	}
	state, stateOK := exactlyOneQuery(query, "state")
	installationRaw, installationOK := exactlyOneQuery(query, "installation_id")
	if !stateOK || !installationOK {
		writeProblem(w, r, http.StatusBadRequest, "InvalidGitHubSetupReturn", "GitHub setup return rejected", "The GitHub setup return parameters are invalid.")
		return
	}
	if actions, exists := query["setup_action"]; exists && (len(actions) != 1 || actions[0] != "install" && actions[0] != "update") {
		writeProblem(w, r, http.StatusBadRequest, "InvalidGitHubSetupReturn", "GitHub setup return rejected", "The GitHub setup return parameters are invalid.")
		return
	}
	installationID, err := strconv.ParseInt(installationRaw, 10, 64)
	if err != nil || installationID <= 0 || strconv.FormatInt(installationID, 10) != installationRaw {
		writeProblem(w, r, http.StatusBadRequest, "InvalidGitHubSetupReturn", "GitHub setup return rejected", "The GitHub setup return parameters are invalid.")
		return
	}
	result, err := s.githubSetup.Continue(r.Context(), builds.ContinueSetupRequest{ActorID: currentUser(r.Context()).ID,
		State: state, InstallationID: installationID})
	if err != nil {
		mappedGitHubBuildError(w, r, err)
		return
	}
	if !s.setGitHubSetupSessionCookie(w, r, githubSetupSessionCookie, githubOAuthCallbackCookiePath) {
		return
	}
	s.clearGitHubSetupSessionCookie(w, githubInstallReturnCookiePath)
	w.Header().Set("Location", result.AuthorizationURL)
	w.WriteHeader(http.StatusSeeOther)
}

func (s *Server) completeGitHubSetup(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	if s.githubSetup == nil {
		githubBuildUnavailable(w, r, "GitHub App setup is not configured.")
		return
	}
	query := r.URL.Query()
	for key := range query {
		if key != "state" && key != "code" && key != "iss" {
			writeProblem(w, r, http.StatusBadRequest, "InvalidGitHubCallback", "GitHub callback rejected", "The GitHub callback parameters are invalid.")
			return
		}
	}
	if issuers, exists := query["iss"]; exists && (len(issuers) != 1 || issuers[0] != githubOAuthIssuer) {
		writeProblem(w, r, http.StatusBadRequest, "InvalidGitHubCallback", "GitHub callback rejected", "The GitHub callback parameters are invalid.")
		return
	}
	state, stateOK := exactlyOneQuery(query, "state")
	code, codeOK := exactlyOneQuery(query, "code")
	if !stateOK || !codeOK {
		writeProblem(w, r, http.StatusBadRequest, "InvalidGitHubCallback", "GitHub callback rejected", "The GitHub callback parameters are invalid.")
		return
	}
	result, err := s.githubSetup.Complete(r.Context(), builds.CompleteSetupRequest{ActorID: currentUser(r.Context()).ID, State: state, Code: code})
	code = ""
	if err != nil {
		mappedGitHubBuildError(w, r, err)
		return
	}
	s.clearGitHubSetupSessionCookie(w, githubOAuthCallbackCookiePath)
	if !s.setGitHubSetupHandoffCookie(w, result.Handoff, result.ExpiresAt) {
		writeProblem(w, r, http.StatusBadGateway, "GitHubSetupInvalid", "GitHub setup failed", "GitHub returned an invalid or expired setup result.")
		return
	}
	result.Handoff = ""
	w.Header().Set("Location", githubSetupCompletePath)
	w.WriteHeader(http.StatusSeeOther)
}

func (s *Server) linkGitHubSetup(w http.ResponseWriter, r *http.Request) {
	key, ok := githubBuildIdempotencyKey(w, r)
	if !ok {
		return
	}
	if s.githubSetup == nil {
		githubBuildUnavailable(w, r, "GitHub App setup is not configured.")
		return
	}
	if r.Body != nil {
		body, readErr := io.ReadAll(io.LimitReader(r.Body, 2))
		if readErr != nil || len(body) != 0 {
			clear(body)
			writeProblem(w, r, http.StatusBadRequest, "InvalidJSON", "Invalid request", "The GitHub link request body must be empty.")
			return
		}
	}
	handoff, ok := githubSetupHandoff(r)
	if !ok {
		writeProblem(w, r, http.StatusUnauthorized, "GitHubSetupHandoffRequired", "GitHub setup expired", "Restart GitHub App setup to obtain a fresh verified handoff.")
		return
	}
	fingerprintHash := sha256.New()
	_, _ = fingerprintHash.Write([]byte("kuberploy-github-link-v1\x00"))
	handoffBytes := []byte(handoff)
	_, _ = fingerprintHash.Write(handoffBytes)
	clear(handoffBytes)
	fingerprintDigest := fingerprintHash.Sum(nil)
	fingerprintValue := "sha256:" + hex.EncodeToString(fingerprintDigest)
	clear(fingerprintDigest)
	result, err := s.githubSetup.Link(r.Context(), builds.LinkSetupRequest{ActorID: currentUser(r.Context()).ID, Handoff: handoff,
		IdempotencyKey: key, RequestFingerprint: fingerprintValue, RequestID: requestID(r.Context())})
	handoff = ""
	if err != nil {
		mappedGitHubBuildError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	s.clearGitHubSetupHandoffCookie(w)
	repositories := make([]githubRepositoryView, 0, len(result.Repositories))
	for _, repository := range result.Repositories {
		repositories = append(repositories, safeRepositoryView(repository))
	}
	w.Header().Set("Location", "/v1/github/installations/"+result.Installation.ID)
	writeJSON(w, http.StatusCreated, linkedGitHubSetupView{Installation: result.Installation, Repositories: repositories})
}

func (s *Server) receiveGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	if s.githubWebhookBackend == nil {
		githubBuildUnavailable(w, r, "GitHub webhook reception is not configured.")
		return
	}
	contentType, _, contentTypeErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if contentTypeErr != nil || contentType != "application/json" {
		writeProblem(w, r, http.StatusUnsupportedMediaType, "UnsupportedMediaType", "Unsupported media type", "GitHub webhooks require application/json.")
		return
	}
	outcome, err := s.githubWebhookBackend.Accept(r.Context(), r.Header, r.Body)
	if err != nil {
		mappedGitHubBuildError(w, r, err)
		return
	}
	if outcome.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
}

type buildDefinitionRequest struct {
	SourceKind        builds.SourceKind    `json:"sourceKind,omitempty"`
	InstallationID    string               `json:"installationId"`
	RepositoryID      string               `json:"repositoryId"`
	RepositoryURL     string               `json:"repositoryUrl,omitempty"`
	GitSSHKeyScope    gitssh.Scope         `json:"gitSSHKeyScope,omitempty"`
	GitSSHKeyRevision uint64               `json:"gitSSHKeyRevision,omitempty"`
	HostKeyPins       []gitssh.HostKeyPin  `json:"hostKeyPins,omitempty"`
	RegistryTargetID  string               `json:"registryTargetId"`
	TriggerRef        string               `json:"triggerRef"`
	ContextPath       string               `json:"contextPath"`
	DockerfilePath    string               `json:"dockerfilePath"`
	Platforms         []string             `json:"platforms"`
	BuildArgs         []builder.BuildArg   `json:"buildArgs,omitempty"`
	SecretProfileIDs  []string             `json:"secretProfileIds,omitempty"`
	SSHProfileIDs     []string             `json:"sshProfileIds,omitempty"`
	CacheTrustLane    string               `json:"cacheTrustLane"`
	CacheImports      int                  `json:"cacheImports"`
	Profile           builder.BuildProfile `json:"profile"`
	MaxAttempts       int                  `json:"maxAttempts"`
}

type manualBuildRequest struct {
	CommitSHA string `json:"commitSha"`
}

type safeRegistryBindingView struct {
	TargetID         string              `json:"targetId"`
	Mode             builds.RegistryMode `json:"mode"`
	Server           string              `json:"server"`
	RepositoryPrefix string              `json:"repositoryPrefix"`
}

type buildDefinitionView struct {
	ID                   string                  `json:"id"`
	ProjectID            string                  `json:"projectId"`
	ApplicationID        string                  `json:"applicationId"`
	SourceKind           builds.SourceKind       `json:"sourceKind"`
	InstallationID       string                  `json:"installationId,omitempty"`
	RepositoryID         string                  `json:"repositoryId,omitempty"`
	RepositoryURL        string                  `json:"repositoryUrl,omitempty"`
	GitSSHKeyScope       string                  `json:"gitSSHKeyScope,omitempty"`
	GitSSHKeyRevision    uint64                  `json:"gitSSHKeyRevision,omitempty"`
	TriggerRef           string                  `json:"triggerRef"`
	ContextPath          string                  `json:"contextPath"`
	DockerfilePath       string                  `json:"dockerfilePath"`
	Platforms            []string                `json:"platforms"`
	Registry             safeRegistryBindingView `json:"registry"`
	BuildArgs            []builder.BuildArg      `json:"buildArgs"`
	SecretFiles          []builder.FileReference `json:"secretFiles"`
	SSHFiles             []builder.FileReference `json:"sshFiles"`
	CacheTrustLane       string                  `json:"cacheTrustLane"`
	CacheImports         int                     `json:"cacheImports"`
	Profile              builder.BuildProfile    `json:"profile"`
	MaxAttempts          int                     `json:"maxAttempts"`
	DefinitionDigest     string                  `json:"definitionDigest"`
	DefinitionGeneration int64                   `json:"definitionGeneration"`
	Enabled              bool                    `json:"enabled"`
	CreatedAt            time.Time               `json:"createdAt"`
	UpdatedAt            time.Time               `json:"updatedAt"`
}

type buildSecretProfileView struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type buildSecretProfileCatalogView struct {
	Build []buildSecretProfileView `json:"build"`
	SSH   []buildSecretProfileView `json:"ssh"`
}

type buildAttemptView struct {
	ID                string              `json:"id"`
	DefinitionID      string              `json:"definitionId"`
	ProjectID         string              `json:"projectId"`
	ApplicationID     string              `json:"applicationId"`
	CommitSHA         string              `json:"commitSha"`
	GitRef            string              `json:"gitRef"`
	Generation        int64               `json:"generation"`
	State             builds.AttemptState `json:"state"`
	ExecutionAttempts int                 `json:"executionAttempts"`
	MaxAttempts       int                 `json:"maxAttempts"`
	Image             *builder.Image      `json:"image,omitempty"`
	CacheReuse        builder.CacheReuse  `json:"cacheReuse,omitempty"`
	Warnings          []builder.Warning   `json:"warnings,omitempty"`
	CacheReference    string              `json:"cacheReference,omitempty"`
	FailureCode       string              `json:"failureCode,omitempty"`
	CancelRequestedAt *time.Time          `json:"cancelRequestedAt,omitempty"`
	StartedAt         *time.Time          `json:"startedAt,omitempty"`
	CompletedAt       *time.Time          `json:"completedAt,omitempty"`
	CreatedAt         time.Time           `json:"createdAt"`
	UpdatedAt         time.Time           `json:"updatedAt"`
}

func (s *Server) applicationBuildDefinitions(w http.ResponseWriter, r *http.Request) {
	application, ok := s.authorizedBuildApplication(w, r, domain.PermissionBuildsRead)
	if !ok {
		return
	}
	if s.builds == nil {
		githubBuildUnavailable(w, r, "Source builds are not configured.")
		return
	}
	if r.Method == http.MethodGet {
		definitions, err := s.builds.Definitions(r.Context(), application.ID)
		if err != nil {
			mappedGitHubBuildError(w, r, err)
			return
		}
		views := make([]buildDefinitionView, 0, len(definitions))
		for _, definition := range definitions {
			if definition.ServiceID != application.ID || definition.ProjectID != application.ProjectID {
				mappedError(w, r, store.ErrNotFound)
				return
			}
			views = append(views, safeBuildDefinition(definition))
		}
		collection(w, views)
		return
	}
	if err := s.store.Authorize(r.Context(), currentUser(r.Context()).ID, domain.PermissionBuildsManage, domain.AccessTarget{Type: "application", ID: application.ID}); err != nil {
		mappedError(w, r, err)
		return
	}
	key, ok := githubBuildIdempotencyKey(w, r)
	if !ok {
		return
	}
	var input buildDefinitionRequest
	if !decodeGitHubBuildJSON(w, r, &input) {
		return
	}
	if input.SourceKind == "" {
		input.SourceKind = builds.SourceGitHub
	}
	if len(input.Platforms) == 0 {
		input.Platforms = []string{s.defaultBuildPlatform}
	}
	if input.SourceKind == builds.SourceGitHub {
		if err := s.store.AuthorizeGitHubInstallationForProject(r.Context(), currentUser(r.Context()).ID, strings.TrimSpace(input.InstallationID), application.ProjectID); err != nil {
			mappedError(w, r, err)
			return
		}
	}
	mutation := BuildDefinitionMutation{ApplicationID: application.ID, ProjectID: application.ProjectID, InstallationID: strings.TrimSpace(input.InstallationID),
		RepositoryID: strings.TrimSpace(input.RepositoryID), SourceKind: input.SourceKind, RepositoryURL: strings.TrimSpace(input.RepositoryURL),
		GitSSHKeyScope: input.GitSSHKeyScope, GitSSHKeyRevision: input.GitSSHKeyRevision, HostKeyPins: append([]gitssh.HostKeyPin(nil), input.HostKeyPins...),
		RegistryTargetID: strings.TrimSpace(input.RegistryTargetID), TriggerRef: strings.TrimSpace(input.TriggerRef),
		ContextPath: input.ContextPath, DockerfilePath: input.DockerfilePath, Platforms: input.Platforms, BuildArgs: input.BuildArgs,
		CacheTrustLane: strings.TrimSpace(input.CacheTrustLane), CacheImports: input.CacheImports,
		Profile: input.Profile, MaxAttempts: input.MaxAttempts, ActorID: currentUser(r.Context()).ID, IdempotencyKey: key,
		SecretProfileIDs: input.SecretProfileIDs, SSHProfileIDs: input.SSHProfileIDs,
		Fingerprint: "sha256:" + fingerprint(input)}
	definition, replay, err := s.builds.CreateDefinition(r.Context(), mutation)
	if err != nil {
		mappedGitHubBuildError(w, r, err)
		return
	}
	if replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Location", "/v1/applications/"+application.ID+"/build-definitions/"+definition.ID)
	writeJSON(w, http.StatusCreated, safeBuildDefinition(definition))
}

func (s *Server) deleteApplicationBuildDefinition(w http.ResponseWriter, r *http.Request) {
	application, ok := s.authorizedBuildApplication(w, r, domain.PermissionBuildsManage)
	if !ok {
		return
	}
	if s.builds == nil {
		githubBuildUnavailable(w, r, "Source builds are not configured.")
		return
	}
	definitionID := strings.TrimSpace(r.PathValue("definitionId"))
	if !validUUID(definitionID) {
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "The build definition identifier is invalid.")
		return
	}
	key, ok := githubBuildIdempotencyKey(w, r)
	if !ok {
		return
	}
	fp := "sha256:" + fingerprint(struct {
		ApplicationID string `json:"applicationId"`
		DefinitionID  string `json:"definitionId"`
	}{application.ID, definitionID})
	replay, err := s.builds.DeleteDefinition(r.Context(), currentUser(r.Context()).ID, application.ID, definitionID, key, fp, requestID(r.Context()))
	if err != nil {
		mappedGitHubBuildError(w, r, err)
		return
	}
	if replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) applicationBuildSecretProfiles(w http.ResponseWriter, r *http.Request) {
	application, ok := s.authorizedBuildApplication(w, r, domain.PermissionBuildsRead)
	if !ok {
		return
	}
	cataloger, ok := s.builds.(BuildSecretProfileCatalog)
	if !ok {
		githubBuildUnavailable(w, r, "Build-secret profiles are not configured.")
		return
	}
	catalog, err := cataloger.SecretProfileCatalog(r.Context(), application.ID)
	if err != nil {
		mappedGitHubBuildError(w, r, err)
		return
	}
	view := buildSecretProfileCatalogView{Build: make([]buildSecretProfileView, 0, len(catalog.Build)), SSH: make([]buildSecretProfileView, 0, len(catalog.SSH))}
	for _, profile := range catalog.Build {
		view.Build = append(view.Build, buildSecretProfileView{ID: profile.ID, Label: profile.Label})
	}
	for _, profile := range catalog.SSH {
		view.SSH = append(view.SSH, buildSecretProfileView{ID: profile.ID, Label: profile.Label})
	}
	writeJSON(w, http.StatusOK, view)
}

func (b *buildBackend) SecretProfileCatalog(_ context.Context, applicationID string) (builds.BuildSecretProfileCatalog, error) {
	if applicationID == "" {
		return builds.BuildSecretProfileCatalog{}, builds.ErrInvalid
	}
	resolver, ok := b.resolver.(BuildSecretProfileResolver)
	if !ok {
		return builds.BuildSecretProfileCatalog{}, builds.ErrInfrastructure
	}
	return resolver.SecretProfileCatalog(applicationID)
}

func (s *Server) githubInstallationRepositories(w http.ResponseWriter, r *http.Request) {
	if s.builds == nil {
		githubBuildUnavailable(w, r, "GitHub repository discovery is not configured.")
		return
	}
	installationID := strings.TrimSpace(r.PathValue("id"))
	if !s.actorCanAccessGitHubInstallation(r.Context(), currentUser(r.Context()).ID, installationID) {
		mappedError(w, r, store.ErrNotFound)
		return
	}
	repositories, err := s.builds.Repositories(r.Context(), installationID)
	if err != nil {
		mappedGitHubBuildError(w, r, err)
		return
	}
	views := make([]githubRepositoryView, 0, len(repositories))
	for _, repository := range repositories {
		if repository.InstallationID != installationID {
			mappedError(w, r, store.ErrNotFound)
			return
		}
		views = append(views, safeRepositoryView(repository))
	}
	collection(w, views)
}

func (s *Server) buildDefinition(w http.ResponseWriter, r *http.Request) {
	if s.builds == nil {
		githubBuildUnavailable(w, r, "Source builds are not configured.")
		return
	}
	definition, err := s.builds.Definition(r.Context(), strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		mappedGitHubBuildError(w, r, err)
		return
	}
	application, err := s.store.GetApplication(r.Context(), definition.ServiceID)
	if err != nil || application.ProjectID != definition.ProjectID {
		mappedError(w, r, store.ErrNotFound)
		return
	}
	if err = s.store.Authorize(r.Context(), currentUser(r.Context()).ID, domain.PermissionBuildsRead, domain.AccessTarget{Type: "application", ID: application.ID}); err != nil {
		mappedError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, safeBuildDefinition(definition))
}

func (s *Server) buildDefinitionBuild(w http.ResponseWriter, r *http.Request) {
	if s.builds == nil {
		githubBuildUnavailable(w, r, "Source builds are not configured.")
		return
	}
	definition, err := s.builds.Definition(r.Context(), strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		mappedGitHubBuildError(w, r, err)
		return
	}
	application, err := s.store.GetApplication(r.Context(), definition.ServiceID)
	if err != nil || application.ProjectID != definition.ProjectID {
		mappedError(w, r, store.ErrNotFound)
		return
	}
	if err = s.store.Authorize(r.Context(), currentUser(r.Context()).ID, domain.PermissionBuildsManage, domain.AccessTarget{Type: "application", ID: application.ID}); err != nil {
		mappedError(w, r, err)
		return
	}
	key, ok := githubBuildIdempotencyKey(w, r)
	if !ok {
		return
	}
	var input manualBuildRequest
	if !decodeGitHubBuildJSON(w, r, &input) {
		return
	}
	input.CommitSHA = strings.TrimSpace(input.CommitSHA)
	if !manualBuildCommitRE.MatchString(input.CommitSHA) {
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "commitSha must be an exact lowercase 40-hex Git commit ID.")
		return
	}
	attempt, replay, err := s.builds.Build(r.Context(), currentUser(r.Context()).ID, definition.ID, input.CommitSHA, key, "sha256:"+fingerprint(input))
	if err != nil {
		mappedGitHubBuildError(w, r, err)
		return
	}
	if replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Location", "/v1/builds/"+attempt.ID)
	writeJSON(w, http.StatusAccepted, safeBuildAttempt(attempt))
}

func (s *Server) applicationBuildHistory(w http.ResponseWriter, r *http.Request) {
	application, ok := s.authorizedBuildApplication(w, r, domain.PermissionBuildsRead)
	if !ok {
		return
	}
	if s.builds == nil {
		githubBuildUnavailable(w, r, "Source builds are not configured.")
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "limit must be between 1 and 100.")
			return
		}
		limit = parsed
	}
	attempts, err := s.builds.Attempts(r.Context(), application.ID, limit)
	if err != nil {
		mappedGitHubBuildError(w, r, err)
		return
	}
	views := make([]buildAttemptView, 0, len(attempts))
	for _, attempt := range attempts {
		if attempt.ServiceID != application.ID || attempt.ProjectID != application.ProjectID {
			mappedError(w, r, store.ErrNotFound)
			return
		}
		views = append(views, safeBuildAttempt(attempt))
	}
	collection(w, views)
}

func (s *Server) buildAttempt(w http.ResponseWriter, r *http.Request) {
	attempt, ok := s.authorizedBuildAttempt(w, r, domain.PermissionBuildsRead)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, safeBuildAttempt(attempt))
}

func (s *Server) cancelBuildAttempt(w http.ResponseWriter, r *http.Request) {
	attempt, ok := s.authorizedBuildAttempt(w, r, domain.PermissionBuildsCancel)
	if !ok {
		return
	}
	key, ok := githubBuildIdempotencyKey(w, r)
	if !ok {
		return
	}
	updated, replay, err := s.builds.Cancel(r.Context(), currentUser(r.Context()).ID, attempt.ID, key, "sha256:"+fingerprint(map[string]string{"attemptId": attempt.ID}))
	if err != nil {
		mappedGitHubBuildError(w, r, err)
		return
	}
	if replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Location", "/v1/builds/"+updated.ID)
	writeJSON(w, http.StatusAccepted, safeBuildAttempt(updated))
}

func (s *Server) retryBuildAttempt(w http.ResponseWriter, r *http.Request) {
	attempt, ok := s.authorizedBuildAttempt(w, r, domain.PermissionBuildsRetry)
	if !ok {
		return
	}
	key, ok := githubBuildIdempotencyKey(w, r)
	if !ok {
		return
	}
	retried, replay, err := s.builds.Retry(r.Context(), currentUser(r.Context()).ID, attempt.ID, key, "sha256:"+fingerprint(map[string]string{"attemptId": attempt.ID}))
	if err != nil {
		mappedGitHubBuildError(w, r, err)
		return
	}
	if replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Location", "/v1/builds/"+retried.ID)
	writeJSON(w, http.StatusAccepted, safeBuildAttempt(retried))
}

func (s *Server) authorizedBuildApplication(w http.ResponseWriter, r *http.Request, permission domain.Permission) (domain.Application, bool) {
	application, err := s.store.GetApplication(r.Context(), strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		mappedError(w, r, err)
		return domain.Application{}, false
	}
	if err = s.store.Authorize(r.Context(), currentUser(r.Context()).ID, permission, domain.AccessTarget{Type: "application", ID: application.ID}); err != nil {
		mappedError(w, r, err)
		return domain.Application{}, false
	}
	return application, true
}

func (s *Server) authorizedBuildAttempt(w http.ResponseWriter, r *http.Request, permission domain.Permission) (builds.BuildAttempt, bool) {
	if s.builds == nil {
		githubBuildUnavailable(w, r, "Source builds are not configured.")
		return builds.BuildAttempt{}, false
	}
	attempt, err := s.builds.Attempt(r.Context(), strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		mappedGitHubBuildError(w, r, err)
		return builds.BuildAttempt{}, false
	}
	application, err := s.store.GetApplication(r.Context(), attempt.ServiceID)
	if err != nil || application.ProjectID != attempt.ProjectID {
		mappedError(w, r, store.ErrNotFound)
		return builds.BuildAttempt{}, false
	}
	if err = s.store.Authorize(r.Context(), currentUser(r.Context()).ID, permission, domain.AccessTarget{Type: "application", ID: application.ID}); err != nil {
		mappedError(w, r, err)
		return builds.BuildAttempt{}, false
	}
	return attempt, true
}

func (s *Server) actorCanAccessGitHubInstallation(ctx context.Context, actorID, installationID string) bool {
	if installationID == "" {
		return false
	}
	items, err := s.store.ListGitHubInstallationsForActor(ctx, actorID)
	if err != nil {
		return false
	}
	for _, item := range items {
		if item.ID == installationID {
			return true
		}
	}
	return false
}

func safeBuildDefinition(definition builds.BuildDefinition) buildDefinitionView {
	return buildDefinitionView{ID: definition.ID, ProjectID: definition.ProjectID, ApplicationID: definition.ServiceID,
		SourceKind: definition.SourceKind, InstallationID: definition.InstallationID, RepositoryID: definition.RepositoryID,
		RepositoryURL: gitSSHRepositoryURL(definition), GitSSHKeyScope: gitSSHKeyScope(definition), GitSSHKeyRevision: gitSSHKeyRevision(definition), TriggerRef: definition.TriggerRef,
		ContextPath: definition.Spec.ContextPath, DockerfilePath: definition.Spec.DockerfilePath,
		Platforms: append([]string{}, definition.Spec.Platforms...), Registry: safeRegistryBindingView{TargetID: definition.Spec.Registry.TargetID,
			Mode: definition.Spec.Registry.Mode, Server: definition.Spec.Registry.Server, RepositoryPrefix: definition.Spec.Registry.RepositoryPrefix},
		BuildArgs: safeBuildArguments(definition.Spec.BuildArgs), SecretFiles: append([]builder.FileReference{}, definition.Spec.SecretFiles...),
		SSHFiles: append([]builder.FileReference{}, definition.Spec.SSHFiles...), CacheTrustLane: definition.Spec.CacheTrustLane,
		CacheImports: definition.Spec.CacheImports, Profile: definition.Spec.Profile, MaxAttempts: definition.Spec.MaxAttempts,
		DefinitionDigest: definition.DefinitionDigest, DefinitionGeneration: definition.DefinitionGeneration, Enabled: definition.Enabled,
		CreatedAt: definition.CreatedAt, UpdatedAt: definition.UpdatedAt}
}

func gitSSHRepositoryURL(definition builds.BuildDefinition) string {
	if definition.GitSSH != nil {
		return definition.GitSSH.RepositoryURL
	}
	return ""
}

func gitSSHKeyScope(definition builds.BuildDefinition) string {
	if definition.GitSSH != nil {
		return definition.GitSSH.KeyScope
	}
	return ""
}

func gitSSHKeyRevision(definition builds.BuildDefinition) uint64 {
	if definition.GitSSH != nil {
		return definition.GitSSH.KeyRevision
	}
	return 0
}

// Build argument values are caller-provided configuration, not safe metadata.
// Keep the names so operators can identify the arguments, but never echo their
// plaintext values to a reader of the definition or its create response.
func safeBuildArguments(arguments []builder.BuildArg) []builder.BuildArg {
	if arguments == nil {
		return []builder.BuildArg{}
	}
	redacted := make([]builder.BuildArg, len(arguments))
	for i, argument := range arguments {
		// Keep the response valid against the public BuildArgument schema while
		// making it explicit that the write-only value was redacted.
		redacted[i] = builder.BuildArg{Name: argument.Name, Value: ""}
	}
	return redacted
}

func safeBuildAttempt(attempt builds.BuildAttempt) buildAttemptView {
	view := buildAttemptView{ID: attempt.ID, DefinitionID: attempt.DefinitionID, ProjectID: attempt.ProjectID, ApplicationID: attempt.ServiceID,
		CommitSHA: attempt.CommitSHA, GitRef: attempt.GitRef, Generation: attempt.Generation, State: attempt.State,
		ExecutionAttempts: attempt.ExecutionAttempts, MaxAttempts: attempt.MaxAttempts, CacheReference: attempt.CacheReference,
		FailureCode: attempt.FailureCode, CancelRequestedAt: attempt.CancelRequestedAt,
		StartedAt: attempt.StartedAt, CompletedAt: attempt.CompletedAt, CreatedAt: attempt.CreatedAt, UpdatedAt: attempt.UpdatedAt}
	if attempt.Result != nil {
		image := attempt.Result.Image
		image.Platforms = append([]string(nil), image.Platforms...)
		view.Image, view.CacheReuse, view.Warnings = &image, attempt.Result.CacheReuse, append([]builder.Warning(nil), attempt.Result.Warnings...)
	}
	return view
}

func providerRepositoryView(repository githubapp.RepositoryIdentity) githubRepositoryView {
	return githubRepositoryView{GitHubRepositoryID: repository.ID, OwnerID: repository.OwnerID, OwnerLogin: repository.OwnerLogin, Name: repository.Name}
}
func safeRepositoryView(repository builds.Repository) githubRepositoryView {
	view := providerRepositoryView(repository.Identity)
	view.ID, view.InstallationID, view.Lifecycle = repository.ID, repository.InstallationID, string(repository.Lifecycle)
	return view
}

func exactlyOneQuery(query map[string][]string, key string) (string, bool) {
	values := query[key]
	returnValue := ""
	if len(values) == 1 {
		returnValue = values[0]
	}
	return returnValue, len(values) == 1 && returnValue != ""
}

func githubBuildIdempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	values := r.Header.Values("Idempotency-Key")
	if len(values) != 1 || !setupHTTPIdempotencyRE.MatchString(values[0]) {
		writeProblem(w, r, http.StatusBadRequest, "IdempotencyKeyRequired", "Idempotency key required", "Provide one Idempotency-Key header containing 16 to 128 safe ASCII characters.")
		return "", false
	}
	return values[0], true
}

var setupHTTPIdempotencyRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$`)
var manualBuildCommitRE = regexp.MustCompile(`^[a-f0-9]{40}$`)

const maxGitHubBuildRequestBytes = 512 << 10

func decodeGitHubBuildJSON(w http.ResponseWriter, r *http.Request, output any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeProblem(w, r, http.StatusUnsupportedMediaType, "UnsupportedMediaType", "Unsupported media type", "GitHub and build mutations require application/json.")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxGitHubBuildRequestBytes)
	raw, err := io.ReadAll(r.Body)
	defer clear(raw)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeProblem(w, r, http.StatusRequestEntityTooLarge, "PayloadTooLarge", "Payload too large", "The GitHub or build request exceeds the encoded request limit.")
		} else {
			writeProblem(w, r, http.StatusBadRequest, "InvalidJSON", "Invalid JSON", "The request body is not valid JSON.")
		}
		return false
	}
	if len(raw) == 0 || !utf8.Valid(raw) || !uniqueJSONObject(raw) {
		writeProblem(w, r, http.StatusBadRequest, "InvalidJSON", "Invalid JSON", "The request body must be one UTF-8 JSON object without duplicate fields.")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(output); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "InvalidJSON", "Invalid JSON", "The request body is invalid or contains an unknown field.")
		return false
	}
	if err = decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeProblem(w, r, http.StatusBadRequest, "InvalidJSON", "Invalid JSON", "The request body must contain exactly one JSON object.")
		return false
	}
	return true
}

func uniqueJSONObject(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var walk func(json.Token) bool
	walk = func(token json.Token) bool {
		delimiter, isDelimiter := token.(json.Delim)
		if !isDelimiter {
			return true
		}
		switch delimiter {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				keyToken, tokenErr := decoder.Token()
				key, ok := keyToken.(string)
				if tokenErr != nil || !ok {
					return false
				}
				if _, duplicate := seen[key]; duplicate {
					return false
				}
				seen[key] = struct{}{}
				value, valueErr := decoder.Token()
				if valueErr != nil || !walk(value) {
					return false
				}
			}
			end, endErr := decoder.Token()
			return endErr == nil && end == json.Delim('}')
		case '[':
			for decoder.More() {
				value, valueErr := decoder.Token()
				if valueErr != nil || !walk(value) {
					return false
				}
			}
			end, endErr := decoder.Token()
			return endErr == nil && end == json.Delim(']')
		default:
			return false
		}
	}
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') || !walk(first) {
		return false
	}
	_, err = decoder.Token()
	return errors.Is(err, io.EOF)
}

func mappedGitHubBuildError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, githubapp.ErrInvalidWebhook):
		writeProblem(w, r, http.StatusUnauthorized, "GitHubWebhookRejected", "GitHub webhook rejected", "The webhook signature, headers, or payload is invalid.")
	case errors.Is(err, githubapp.ErrWebhookTooLarge):
		writeProblem(w, r, http.StatusRequestEntityTooLarge, "PayloadTooLarge", "Payload too large", "The GitHub webhook exceeds the configured request limit.")
	case errors.Is(err, githubapp.ErrInvalidState), errors.Is(err, githubapp.ErrExpiredState), errors.Is(err, githubapp.ErrStateReplay):
		writeProblem(w, r, http.StatusBadRequest, "InvalidGitHubState", "GitHub callback rejected", "The setup state is invalid, expired, already used, or bound to another session.")
	case errors.Is(err, githubapp.ErrInvalidHandoff), errors.Is(err, githubapp.ErrExpiredHandoff):
		writeProblem(w, r, http.StatusBadRequest, "InvalidGitHubHandoff", "GitHub link rejected", "The one-time setup handoff is invalid, expired, or already used.")
	case errors.Is(err, githubapp.ErrOwnershipMismatch), errors.Is(err, githubapp.ErrScopeMismatch), errors.Is(err, builds.ErrUnauthorized):
		writeProblem(w, r, http.StatusForbidden, "GitHubOwnershipMismatch", "GitHub authorization rejected", "The authenticated GitHub user, installation, repository, or requested scope does not match.")
	case errors.Is(err, builds.ErrGitSSHKeyInactive):
		writeProblem(w, r, http.StatusConflict, "GitSSHKeyInactive", "Git SSH key is inactive", "This Git SSH source references an inactive key. Reconnect it with the active key before building.")
	case errors.Is(err, builds.ErrNotFound):
		mappedError(w, r, store.ErrNotFound)
	case errors.Is(err, builds.ErrConflict), errors.Is(err, builds.ErrTerminal):
		writeProblem(w, r, http.StatusConflict, "BuildConflict", "Build conflict", "The build definition or attempt is not in a state that permits this command.")
	case errors.Is(err, builds.ErrDeletionBlocked):
		writeProblem(w, r, http.StatusConflict, "BuildDefinitionDeletionBlocked", "Source disconnect blocked", "Wait for active builds to finish or cancel them before disconnecting this source.")
	case errors.Is(err, builds.ErrInvalid), errors.Is(err, githubapp.ErrInvalidTokenRequest):
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "The GitHub or build request is invalid.")
	case errors.Is(err, store.ErrIdempotencyConflict):
		mappedError(w, r, err)
	case errors.Is(err, store.ErrConflict):
		mappedError(w, r, err)
	case errors.Is(err, githubapp.ErrTransport), errors.Is(err, githubapp.ErrSecretUnavailable), errors.Is(err, githubapp.ErrProviderResponse), errors.Is(err, builds.ErrInfrastructure):
		w.Header().Set("Retry-After", "1")
		writeProblem(w, r, http.StatusServiceUnavailable, "GitHubUnavailable", "GitHub integration unavailable", "The GitHub provider or build persistence boundary is temporarily unavailable.")
	default:
		w.Header().Set("Retry-After", "1")
		writeProblem(w, r, http.StatusServiceUnavailable, "GitHubBuildUnavailable", "Temporarily unavailable", "The GitHub or build operation could not be completed safely.")
	}
}

func githubBuildUnavailable(w http.ResponseWriter, r *http.Request, detail string) {
	w.Header().Set("Retry-After", "1")
	writeProblem(w, r, http.StatusServiceUnavailable, "GitHubBuildUnavailable", "Temporarily unavailable", detail)
}
