package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitssh"
	"github.com/kuberploy/kuberploy/internal/store"
)

type GitSSHKeyBackend interface {
	Mutate(context.Context, gitssh.MutationRequest) (gitssh.MutationResult, error)
	List(context.Context, gitssh.Scope, string) ([]gitssh.KeyMetadata, error)
}

func (s *Server) projectGitSSHKeys(w http.ResponseWriter, r *http.Request) {
	s.gitSSHKeysForScope(w, r, gitssh.ScopeProject, r.PathValue("id"), r.Method == http.MethodPost)
}

func (s *Server) applicationGitSSHKeys(w http.ResponseWriter, r *http.Request) {
	s.gitSSHKeysForScope(w, r, gitssh.ScopeApp, r.PathValue("id"), r.Method == http.MethodPost)
}

func (s *Server) rotateProjectGitSSHKey(w http.ResponseWriter, r *http.Request) {
	s.mutateGitSSHKey(w, r, gitssh.ScopeProject, r.PathValue("id"), "rotate")
}

func (s *Server) rotateApplicationGitSSHKey(w http.ResponseWriter, r *http.Request) {
	s.mutateGitSSHKey(w, r, gitssh.ScopeApp, r.PathValue("id"), "rotate")
}

func (s *Server) revokeProjectGitSSHKey(w http.ResponseWriter, r *http.Request) {
	s.mutateGitSSHKey(w, r, gitssh.ScopeProject, r.PathValue("id"), "revoke")
}

func (s *Server) revokeApplicationGitSSHKey(w http.ResponseWriter, r *http.Request) {
	s.mutateGitSSHKey(w, r, gitssh.ScopeApp, r.PathValue("id"), "revoke")
}

func (s *Server) gitSSHKeysForScope(w http.ResponseWriter, r *http.Request, scope gitssh.Scope, ownerID string, create bool) {
	if !s.gitSSHReady(w, r) || !s.authorizeGitSSHOwner(w, r, scope, ownerID, create) {
		return
	}
	if !create {
		items, err := s.gitSSHKeys.List(r.Context(), scope, strings.TrimSpace(ownerID))
		if err != nil {
			s.writeGitSSHError(w, r, err)
			return
		}
		collection(w, items)
		return
	}
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	result, err := s.gitSSHKeys.Mutate(r.Context(), gitssh.MutationRequest{
		Operation: gitssh.OperationCreate, ActorID: currentUser(r.Context()).ID, IdempotencyKey: key,
		RequestFingerprint: fingerprint(struct{ Scope, OwnerID, Operation string }{string(scope), strings.TrimSpace(ownerID), string(gitssh.OperationCreate)}),
		Scope:              scope, OwnerID: strings.TrimSpace(ownerID),
	})
	if err != nil {
		s.writeGitSSHError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeJSON(w, http.StatusCreated, result.Value)
}

func (s *Server) mutateGitSSHKey(w http.ResponseWriter, r *http.Request, scope gitssh.Scope, ownerID, operation string) {
	if !s.gitSSHReady(w, r) || !s.authorizeGitSSHOwner(w, r, scope, ownerID, true) {
		return
	}
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	op := gitssh.OperationRevoke
	if operation == "rotate" {
		op = gitssh.OperationRotate
	}
	result, err := s.gitSSHKeys.Mutate(r.Context(), gitssh.MutationRequest{
		Operation: op, ActorID: currentUser(r.Context()).ID, IdempotencyKey: key,
		RequestFingerprint: fingerprint(struct{ Scope, OwnerID, Operation string }{string(scope), strings.TrimSpace(ownerID), string(op)}),
		Scope:              scope, OwnerID: strings.TrimSpace(ownerID),
	})
	if err != nil {
		s.writeGitSSHError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeJSON(w, http.StatusOK, result.Value)
}

func (s *Server) gitSSHReady(w http.ResponseWriter, r *http.Request) bool {
	if s.gitSSHKeys != nil {
		return true
	}
	writeProblem(w, r, http.StatusServiceUnavailable, "GitSSHUnavailable", "Git SSH unavailable", "Git SSH key storage is not configured.")
	return false
}

func (s *Server) authorizeGitSSHOwner(w http.ResponseWriter, r *http.Request, scope gitssh.Scope, ownerID string, write bool) bool {
	ownerID = strings.TrimSpace(ownerID)
	if !validUUID(ownerID) {
		mappedError(w, r, store.ErrNotFound)
		return false
	}
	actorID := currentUser(r.Context()).ID
	permission := domain.PermissionResourcesRead
	if write {
		permission = domain.PermissionResourcesWrite
	}
	var target domain.AccessTarget
	if scope == gitssh.ScopeProject {
		project, err := s.store.GetProjectForActor(r.Context(), actorID, ownerID)
		if err != nil {
			mappedError(w, r, err)
			return false
		}
		target = domain.AccessTarget{Type: "project", ID: project.ID, ProjectID: project.ID, TeamID: project.TeamID}
	} else {
		application, err := s.store.GetApplicationForActor(r.Context(), actorID, ownerID)
		if err != nil {
			mappedError(w, r, err)
			return false
		}
		project, err := s.store.GetProjectForActor(r.Context(), actorID, application.ProjectID)
		if err != nil {
			mappedError(w, r, err)
			return false
		}
		target = domain.AccessTarget{Type: "application", ID: application.ID, ApplicationID: application.ID, ProjectID: project.ID, TeamID: project.TeamID}
	}
	if err := s.store.Authorize(r.Context(), actorID, permission, target); err != nil {
		mappedError(w, r, err)
		return false
	}
	return true
}

func (s *Server) writeGitSSHError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, gitssh.ErrActiveKeyExists):
		writeProblem(w, r, http.StatusConflict, "GitSSHKeyExists", "Git SSH key already exists", "Rotate or revoke the active key before creating another key.")
	case errors.Is(err, gitssh.ErrActiveKeyNotFound):
		writeProblem(w, r, http.StatusNotFound, "GitSSHKeyNotFound", "Git SSH key not found", "No active Git SSH key exists for this scope.")
	case errors.Is(err, gitssh.ErrIdempotencyConflict):
		writeProblem(w, r, http.StatusConflict, "IdempotencyConflict", "Idempotency conflict", "This idempotency key was already used with different input.")
	case errors.Is(err, gitssh.ErrInvalidScope), errors.Is(err, gitssh.ErrInvalidOwner), errors.Is(err, gitssh.ErrInvalidEnvelope):
		writeProblem(w, r, http.StatusUnprocessableEntity, "GitSSHValidationFailed", "Git SSH key validation failed", "The Git SSH key request is invalid.")
	default:
		writeProblem(w, r, http.StatusInternalServerError, "GitSSHPersistenceFailed", "Git SSH key operation failed", "The Git SSH key operation could not be persisted.")
	}
}
