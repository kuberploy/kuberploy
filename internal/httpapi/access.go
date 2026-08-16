package httpapi

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/emailaddr"
	"github.com/kuberploy/kuberploy/internal/passwordauth"
	"github.com/kuberploy/kuberploy/internal/store"
)

type invitationRequest struct {
	Email string `json:"email"`
}

func (s *Server) createInvitation(w http.ResponseWriter, r *http.Request) {
	var in invitationRequest
	if !decode(w, r, &in) {
		return
	}
	in.Email, _ = emailaddr.Normalize(in.Email)
	if in.Email == "" {
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "email is required and must be a valid email address.")
		return
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		writeProblem(w, r, 500, "EntropyUnavailable", "Internal error", "Secure invitation creation failed.")
		return
	}
	hash := sha256.Sum256(raw)
	invitation, err := s.store.CreateUserInvitation(r.Context(), currentUser(r.Context()).ID, in.Email, hash[:], time.Now().UTC().Add(24*time.Hour), requestID(r.Context()))
	if err != nil {
		mappedError(w, r, err)
		return
	}
	invitation.Token = base64.RawURLEncoding.EncodeToString(raw)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, invitation)
}

type acceptInvitationRequest struct {
	Token       string `json:"token"`
	DisplayName string `json:"displayName"`
	Password    string `json:"password"`
}

func (s *Server) acceptInvitation(w http.ResponseWriter, r *http.Request) {
	var in acceptInvitationRequest
	if !decode(w, r, &in) {
		return
	}
	in.DisplayName = strings.TrimSpace(in.DisplayName)
	rawToken, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(in.Token))
	if err != nil || len(rawToken) != 32 || in.DisplayName == "" || len(in.DisplayName) > 100 {
		writeProblem(w, r, http.StatusUnauthorized, "InvalidInvitation", "Invitation rejected", "The invitation is invalid, expired, or already used.")
		return
	}
	tokenHash := sha256.Sum256(rawToken)
	passwordHash, err := passwordauth.Hash(in.Password)
	if err != nil {
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "password must be 12 to 256 bytes.", FieldError{Pointer: "/password", Code: "Invalid", Detail: "Use a password between 12 and 256 bytes."})
		return
	}
	sessionRaw := make([]byte, 32)
	if _, err = rand.Read(sessionRaw); err != nil {
		writeProblem(w, r, 500, "EntropyUnavailable", "Internal error", "Secure session creation failed.")
		return
	}
	sessionHash := sha256.Sum256(sessionRaw)
	u, err := s.store.AcceptUserInvitation(r.Context(), tokenHash[:], in.DisplayName, passwordHash, sessionHash[:], time.Now().UTC().Add(s.sessionTTL))
	if err != nil {
		if errors.Is(err, store.ErrInvitationInvalid) {
			writeProblem(w, r, http.StatusUnauthorized, "InvalidInvitation", "Invitation rejected", "The invitation is invalid, expired, or already used.")
			return
		}
		mappedError(w, r, err)
		return
	}
	csrf, err := s.setSessionCookies(w, sessionRaw)
	if err != nil {
		writeProblem(w, r, 500, "EntropyUnavailable", "Internal error", "Secure session creation failed.")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-CSRF-Token", csrf)
	writeJSON(w, http.StatusCreated, u)
}

type safeUserView struct {
	ID            string    `json:"id"`
	Email         string    `json:"email"`
	DisplayName   string    `json:"displayName"`
	Role          string    `json:"role"`
	GrantRevision int64     `json:"grantRevision"`
	CreatedAt     time.Time `json:"createdAt"`
}

func safeUser(u domain.User) safeUserView {
	name := u.DisplayName
	return safeUserView{ID: u.ID, Email: u.Email, DisplayName: name, Role: u.Role, GrantRevision: u.GrantRevision, CreatedAt: u.CreatedAt}
}

func (s *Server) users(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListUsersForActor(r.Context(), currentUser(r.Context()).ID)
	if err != nil {
		mappedError(w, r, err)
		return
	}
	views := make([]safeUserView, 0, len(items))
	for _, u := range items {
		views = append(views, safeUser(u))
	}
	collection(w, views)
}

type teamRequest struct {
	Name string `json:"name"`
	Slug string `json:"slug,omitempty"`
}

func (s *Server) teams(w http.ResponseWriter, r *http.Request) {
	actor := currentUser(r.Context())
	if r.Method == http.MethodGet {
		items, err := s.store.ListTeamsForActor(r.Context(), actor.ID)
		if err != nil {
			mappedError(w, r, err)
			return
		}
		collection(w, items)
		return
	}
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	var in teamRequest
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Slug == "" {
		in.Slug = slugify(in.Name)
	} else {
		in.Slug = strings.ToLower(strings.TrimSpace(in.Slug))
	}
	if in.Name == "" || len(in.Name) > 100 || !validSlug(in.Slug) {
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "The team name or slug is invalid.")
		return
	}
	result, err := s.store.CreateTeam(r.Context(), actor.ID, key, fingerprint(in), requestID(r.Context()), domain.CreateTeam{Name: in.Name, Slug: in.Slug})
	if err != nil {
		mappedError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Location", "/v1/teams/"+result.Value.ID)
	writeJSON(w, http.StatusCreated, result.Value)
}

type teamMemberRequest struct {
	UserID string `json:"userId"`
	Role   string `json:"role"`
}

type teamMemberView struct {
	TeamID    string        `json:"teamId"`
	UserID    string        `json:"userId"`
	Role      string        `json:"role"`
	User      *safeUserView `json:"user,omitempty"`
	CreatedAt time.Time     `json:"createdAt"`
}

func safeTeamMember(member domain.TeamMember) teamMemberView {
	view := teamMemberView{TeamID: member.TeamID, UserID: member.UserID, Role: member.Role, CreatedAt: member.CreatedAt}
	if member.User != nil {
		u := safeUser(*member.User)
		view.User = &u
	}
	return view
}

func (s *Server) teamMembers(w http.ResponseWriter, r *http.Request) {
	actor := currentUser(r.Context())
	teamID := r.PathValue("id")
	if r.Method == http.MethodGet {
		items, err := s.store.ListTeamMembersForActor(r.Context(), actor.ID, teamID)
		if err != nil {
			mappedError(w, r, err)
			return
		}
		views := make([]teamMemberView, 0, len(items))
		for _, member := range items {
			views = append(views, safeTeamMember(member))
		}
		collection(w, views)
		return
	}
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	var in teamMemberRequest
	if !decode(w, r, &in) {
		return
	}
	in.UserID = strings.TrimSpace(in.UserID)
	if in.UserID == "" || in.Role != "owner" && in.Role != "member" {
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "userId and role owner|member are required.")
		return
	}
	result, err := s.store.AddTeamMember(r.Context(), actor.ID, teamID, key, fingerprint(in), requestID(r.Context()), domain.AddTeamMember{UserID: in.UserID, Role: in.Role})
	if err != nil {
		mappedError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeJSON(w, http.StatusCreated, safeTeamMember(result.Value))
}

func (s *Server) removeTeamMember(w http.ResponseWriter, r *http.Request) {
	actor := currentUser(r.Context())
	teamID := strings.TrimSpace(r.PathValue("teamId"))
	userID := strings.TrimSpace(r.PathValue("userId"))
	if teamID == "" || userID == "" {
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "teamId and userId are required.")
		return
	}
	if err := s.store.RemoveTeamMember(r.Context(), actor.ID, teamID, userID, requestID(r.Context())); err != nil {
		mappedError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type githubInstallationRequest struct {
	GitHubInstallationID int64  `json:"githubInstallationId"`
	AccountLogin         string `json:"accountLogin"`
	AccountType          string `json:"accountType"`
	RepositorySelection  string `json:"repositorySelection"`
	RepositoryCount      int    `json:"repositoryCount"`
}

func (s *Server) githubInstallations(w http.ResponseWriter, r *http.Request) {
	actor := currentUser(r.Context())
	if r.Method == http.MethodGet {
		items, err := s.store.ListGitHubInstallationsForActor(r.Context(), actor.ID)
		if err != nil {
			mappedError(w, r, err)
			return
		}
		collection(w, items)
		return
	}
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	var in githubInstallationRequest
	if !decode(w, r, &in) {
		return
	}
	in.AccountLogin = strings.TrimSpace(in.AccountLogin)
	if in.GitHubInstallationID < 1 || in.AccountLogin == "" || len(in.AccountLogin) > 100 || in.AccountType != "User" && in.AccountType != "Organization" || in.RepositorySelection != "all" && in.RepositorySelection != "selected" || in.RepositoryCount < 0 {
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "GitHub installation metadata is invalid.")
		return
	}
	create := domain.CreateGitHubInstallation{GitHubInstallationID: in.GitHubInstallationID, AccountLogin: in.AccountLogin, AccountType: in.AccountType, RepositorySelection: in.RepositorySelection, RepositoryCount: in.RepositoryCount}
	result, err := s.store.CreateGitHubInstallation(r.Context(), actor.ID, key, fingerprint(in), requestID(r.Context()), create)
	if err != nil {
		mappedError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Location", "/v1/github/installations/"+result.Value.ID)
	writeJSON(w, http.StatusCreated, result.Value)
}

type githubSharingRequest struct {
	Visibility string `json:"visibility"`
	TeamID     string `json:"teamId,omitempty"`
}

func (s *Server) githubInstallationSharing(w http.ResponseWriter, r *http.Request) {
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	var in githubSharingRequest
	if !decode(w, r, &in) {
		return
	}
	in.TeamID = strings.TrimSpace(in.TeamID)
	if in.Visibility != "private" && in.Visibility != "team" || in.Visibility == "private" && in.TeamID != "" || in.Visibility == "team" && in.TeamID == "" {
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "private visibility must omit teamId; team visibility requires teamId.")
		return
	}
	result, err := s.store.UpdateGitHubInstallationSharing(r.Context(), currentUser(r.Context()).ID, r.PathValue("id"), key, fingerprint(in), requestID(r.Context()), domain.UpdateGitHubInstallationSharing{Visibility: in.Visibility, TeamID: in.TeamID})
	if err != nil {
		mappedError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeJSON(w, http.StatusOK, result.Value)
}
