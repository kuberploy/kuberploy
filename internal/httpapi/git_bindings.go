package httpapi

import (
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/builds"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

var platformBindingIDRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type environmentGitBindingRequest struct {
	InstallationID string `json:"installationId"`
	RepositoryID   string `json:"repositoryId"`
	TargetRef      string `json:"targetRef"`
}

// PlatformGitBindingConfig is server/operator authority. None of these fields
// is accepted from the platform-binding request body. Enabled=false is the
// fail-closed zero value until an operator wires the exact cluster and GitHub
// App identities.
type PlatformGitBindingConfig struct {
	Enabled     bool
	BindingID   string
	GitHubAppID int64
}

func (c PlatformGitBindingConfig) Validate() error {
	if !c.Enabled {
		if c.BindingID != "" || c.GitHubAppID != 0 {
			return gitprojection.ErrInvalid
		}
		return nil
	}
	if !platformBindingIDRE.MatchString(c.BindingID) || c.GitHubAppID <= 0 {
		return gitprojection.ErrInvalid
	}
	return nil
}

type platformGitBindingRequest struct {
	InstallationID string `json:"installationId"`
	RepositoryID   string `json:"repositoryId"`
	TargetRef      string `json:"targetRef"`
}

type platformGitRepositoryView struct {
	Provider       string `json:"provider"`
	InstallationID int64  `json:"installationId"`
	RepositoryID   int64  `json:"repositoryId"`
	Owner          string `json:"owner"`
	Name           string `json:"name"`
}

type platformGitBindingView struct {
	ID                   string                     `json:"id"`
	Repository           platformGitRepositoryView  `json:"repository"`
	TargetRef            string                     `json:"targetRef"`
	PathPrefix           string                     `json:"pathPrefix"`
	State                gitprojection.BindingState `json:"state"`
	TargetHeadRevision   string                     `json:"targetHeadRevision,omitempty"`
	TargetHeadObservedAt *time.Time                 `json:"targetHeadObservedAt,omitempty"`
	CreatedAt            time.Time                  `json:"createdAt"`
	UpdatedAt            time.Time                  `json:"updatedAt"`
}

func safePlatformGitBinding(binding gitprojection.Binding) platformGitBindingView {
	view := platformGitBindingView{ID: binding.ID,
		Repository: platformGitRepositoryView{Provider: binding.Repository.Provider,
			InstallationID: binding.Repository.InstallationID, RepositoryID: binding.Repository.RepositoryID,
			Owner: binding.Repository.Owner, Name: binding.Repository.Name},
		TargetRef: binding.TargetRef, PathPrefix: binding.Prefix, State: binding.State,
		TargetHeadRevision: binding.TargetHeadRevision, CreatedAt: binding.CreatedAt, UpdatedAt: binding.UpdatedAt}
	if !binding.TargetHeadObservedAt.IsZero() {
		observed := binding.TargetHeadObservedAt
		view.TargetHeadObservedAt = &observed
	}
	return view
}

type environmentGitBindingView struct {
	ID                   string                           `json:"id"`
	ProjectID            string                           `json:"projectId"`
	EnvironmentID        string                           `json:"environmentId"`
	Repository           gitprojection.RepositoryIdentity `json:"repository"`
	TargetRef            string                           `json:"targetRef"`
	PathPrefix           string                           `json:"pathPrefix"`
	CredentialMode       gitprojection.CredentialMode     `json:"credentialMode"`
	State                gitprojection.BindingState       `json:"state"`
	TargetHeadRevision   string                           `json:"targetHeadRevision,omitempty"`
	IndexedRevision      string                           `json:"indexedRevision,omitempty"`
	ProjectionGeneration int64                            `json:"projectionGeneration"`
	ParserVersion        string                           `json:"parserVersion"`
	TargetHeadObservedAt *time.Time                       `json:"targetHeadObservedAt,omitempty"`
	IndexedAt            *time.Time                       `json:"indexedAt,omitempty"`
	CreatedAt            time.Time                        `json:"createdAt"`
	UpdatedAt            time.Time                        `json:"updatedAt"`
}

func safeEnvironmentGitBinding(binding gitprojection.Binding) environmentGitBindingView {
	view := environmentGitBindingView{ID: binding.ID, ProjectID: binding.ProjectID, EnvironmentID: binding.EnvironmentID,
		Repository: binding.Repository, TargetRef: binding.TargetRef, PathPrefix: binding.Prefix, CredentialMode: binding.CredentialMode,
		State: binding.State, TargetHeadRevision: binding.TargetHeadRevision, IndexedRevision: binding.IndexedRevision,
		ProjectionGeneration: binding.ProjectionGeneration, ParserVersion: binding.ParserVersion,
		CreatedAt: binding.CreatedAt, UpdatedAt: binding.UpdatedAt}
	if !binding.TargetHeadObservedAt.IsZero() {
		observed := binding.TargetHeadObservedAt
		view.TargetHeadObservedAt = &observed
	}
	if !binding.IndexedAt.IsZero() {
		indexed := binding.IndexedAt
		view.IndexedAt = &indexed
	}
	return view
}

func (s *Server) environmentGitBinding(w http.ResponseWriter, r *http.Request) {
	environmentID := strings.TrimSpace(r.PathValue("id"))
	actorID := currentUser(r.Context()).ID
	if r.Method == http.MethodGet {
		binding, err := s.store.GetEnvironmentGitBindingForActor(r.Context(), actorID, environmentID)
		if err != nil {
			mappedError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, safeEnvironmentGitBinding(binding))
		return
	}
	if s.builds == nil {
		githubBuildUnavailable(w, r, "The verified GitHub repository catalog is not configured.")
		return
	}
	resolver, ok := s.builds.(GitBindingRepositoryResolver)
	if !ok {
		githubBuildUnavailable(w, r, "The verified GitHub repository catalog is unavailable.")
		return
	}
	var err error
	for _, permission := range []domain.Permission{domain.PermissionConfigWrite, domain.PermissionBuildsManage} {
		if err = s.store.Authorize(r.Context(), actorID, permission, domain.AccessTarget{Type: "environment", ID: environmentID}); err != nil {
			mappedError(w, r, err)
			return
		}
	}
	environment, err := s.store.GetEnvironment(r.Context(), environmentID)
	if err != nil {
		mappedError(w, r, err)
		return
	}
	key, ok := githubBuildIdempotencyKey(w, r)
	if !ok {
		return
	}
	var input environmentGitBindingRequest
	if !decodeGitHubBuildJSON(w, r, &input) {
		return
	}
	input.InstallationID = strings.TrimSpace(input.InstallationID)
	input.RepositoryID = strings.TrimSpace(input.RepositoryID)
	input.TargetRef = strings.TrimSpace(input.TargetRef)
	if err = s.store.AuthorizeGitHubInstallationForProject(r.Context(), actorID, input.InstallationID, environment.ProjectID); err != nil {
		mappedError(w, r, err)
		return
	}
	resolved, err := resolver.ResolveGitBindingRepository(r.Context(), input.InstallationID, input.RepositoryID)
	if err != nil {
		mappedGitHubBuildError(w, r, err)
		return
	}
	create := gitprojection.CreateEnvironmentBindingInput{EnvironmentID: environmentID, LinkedInstallationID: input.InstallationID,
		LinkedRepositoryID: input.RepositoryID, GitHubAppID: resolved.GitHubAppID, Repository: resolved.Repository, TargetRef: input.TargetRef}
	result, err := s.store.CreateEnvironmentGitBinding(r.Context(), actorID, key, "sha256:"+fingerprint(input), requestID(r.Context()), create)
	if err != nil {
		if err == gitprojection.ErrInvalid {
			mappedGitHubBuildError(w, r, builds.ErrInvalid)
			return
		}
		mappedError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Location", "/v1/environments/"+environmentID+"/git-binding")
	writeJSON(w, http.StatusCreated, safeEnvironmentGitBinding(result.Value))
}

func (s *Server) platformArgoGitBinding(w http.ResponseWriter, r *http.Request) {
	if s.platformGitBinding.Validate() != nil || !s.platformGitBinding.Enabled {
		githubBuildUnavailable(w, r, "The operator-owned Argo platform Git binding is not configured.")
		return
	}
	actorID := currentUser(r.Context()).ID
	if r.Method == http.MethodGet {
		binding, err := s.store.GetPlatformGitBindingForActor(r.Context(), actorID)
		if err != nil {
			mappedError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, safePlatformGitBinding(binding))
		return
	}
	if s.gitBindingRepositories == nil {
		githubBuildUnavailable(w, r, "The verified GitHub repository catalog is unavailable.")
		return
	}
	key, ok := githubBuildIdempotencyKey(w, r)
	if !ok {
		return
	}
	var input platformGitBindingRequest
	if !decodeGitHubBuildJSON(w, r, &input) {
		return
	}
	input.InstallationID = strings.TrimSpace(input.InstallationID)
	input.RepositoryID = strings.TrimSpace(input.RepositoryID)
	input.TargetRef = strings.TrimSpace(input.TargetRef)
	resolved, err := s.gitBindingRepositories.ResolveGitBindingRepository(r.Context(), input.InstallationID, input.RepositoryID)
	if err != nil {
		mappedGitHubBuildError(w, r, err)
		return
	}
	if resolved.GitHubAppID != s.platformGitBinding.GitHubAppID {
		githubBuildUnavailable(w, r, "The verified repository does not belong to the operator-configured GitHub App.")
		return
	}
	create := gitprojection.CreatePlatformBindingInput{BindingID: s.platformGitBinding.BindingID,
		LinkedInstallationID: input.InstallationID, LinkedRepositoryID: input.RepositoryID,
		GitHubAppID: s.platformGitBinding.GitHubAppID, Repository: resolved.Repository, TargetRef: input.TargetRef}
	result, err := s.store.CreatePlatformGitBinding(r.Context(), actorID, key, "sha256:"+fingerprint(input), requestID(r.Context()), create)
	if err != nil {
		if err == gitprojection.ErrInvalid {
			mappedGitHubBuildError(w, r, builds.ErrInvalid)
			return
		}
		mappedError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Location", "/v1/platform/argo/git-binding")
	writeJSON(w, http.StatusCreated, safePlatformGitBinding(result.Value))
}
