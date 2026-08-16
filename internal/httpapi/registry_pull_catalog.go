package httpapi

import (
	"net/http"
	"strings"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/id"
)

type projectRegistryPullCredentialRequest struct {
	Name             string `json:"name"`
	RegistryTargetID string `json:"registryTargetId"`
}

type safeRegistryPullTargetView struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Server           string `json:"server"`
	RepositoryPrefix string `json:"repositoryPrefix"`
}

func (s *Server) projectRegistryPullCredentials(w http.ResponseWriter, r *http.Request) {
	registryResponseHeaders(w)
	actor, projectID := currentUser(r.Context()).ID, strings.TrimSpace(r.PathValue("id"))
	if r.Method == http.MethodGet {
		items, err := s.store.ListProjectRegistryPullCredentialsForActor(r.Context(), actor, projectID)
		if err != nil {
			mappedRegistryError(w, r, err)
			return
		}
		targets, err := s.store.ListRegistryPullTargetsForActor(r.Context(), actor, projectID)
		if err != nil {
			mappedRegistryError(w, r, err)
			return
		}
		available := make([]safeRegistryPullTargetView, 0, len(targets))
		for _, target := range targets {
			available = append(available, safeRegistryPullTargetView{ID: target.ID, Name: target.Name, Server: target.Endpoint, RepositoryPrefix: target.RepositoryPrefix})
		}
		writeJSON(w, http.StatusOK, struct {
			Items            []domain.ProjectRegistryPullCredential `json:"items"`
			AvailableTargets []safeRegistryPullTargetView           `json:"availableTargets"`
		}{Items: items, AvailableTargets: available})
		return
	}
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	var input projectRegistryPullCredentialRequest
	if !decode(w, r, &input) {
		return
	}
	value := domain.ProjectRegistryPullCredential{ID: id.New(), ProjectID: projectID, RegistryTargetID: strings.TrimSpace(input.RegistryTargetID), Name: strings.TrimSpace(input.Name)}
	result, err := s.store.CreateProjectRegistryPullCredentialForActor(r.Context(), actor, key, fingerprint(input), requestID(r.Context()), value)
	if err != nil {
		mappedRegistryError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeJSON(w, http.StatusCreated, result.Value)
}

func (s *Server) projectRegistryPullCredential(w http.ResponseWriter, r *http.Request) {
	registryResponseHeaders(w)
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	projectID, credentialID := strings.TrimSpace(r.PathValue("id")), strings.TrimSpace(r.PathValue("credentialId"))
	replay, err := s.store.DeleteProjectRegistryPullCredentialForActor(r.Context(), currentUser(r.Context()).ID, projectID, credentialID, key, fingerprint(struct {
		ProjectID    string `json:"projectId"`
		CredentialID string `json:"credentialId"`
	}{projectID, credentialID}), requestID(r.Context()))
	if err != nil {
		mappedRegistryError(w, r, err)
		return
	}
	if replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.WriteHeader(http.StatusNoContent)
}

type applicationRegistryPullSelectionRequest struct {
	Type                domain.ApplicationRegistryPullMode `json:"type"`
	ProjectCredentialID string                             `json:"projectCredentialId,omitempty"`
}

func (s *Server) applicationRegistryPullSelection(w http.ResponseWriter, r *http.Request) {
	registryResponseHeaders(w)
	actor, applicationID := currentUser(r.Context()).ID, strings.TrimSpace(r.PathValue("id"))
	if r.Method == http.MethodGet {
		value, err := s.store.ApplicationRegistryPullSelectionForActor(r.Context(), actor, applicationID)
		if err != nil {
			mappedRegistryError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, struct {
			ApplicationID       string                             `json:"applicationId"`
			Type                domain.ApplicationRegistryPullMode `json:"type"`
			ProjectCredentialID string                             `json:"projectCredentialId,omitempty"`
		}{ApplicationID: value.ApplicationID, Type: value.Mode, ProjectCredentialID: value.ProjectCredentialID})
		return
	}
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	var input applicationRegistryPullSelectionRequest
	if !decode(w, r, &input) {
		return
	}
	value := domain.ApplicationRegistryPullSelection{ApplicationID: applicationID, Mode: input.Type, ProjectCredentialID: strings.TrimSpace(input.ProjectCredentialID)}
	result, err := s.store.PutApplicationRegistryPullSelectionForActor(r.Context(), actor, key, fingerprint(input), requestID(r.Context()), value)
	if err != nil {
		mappedRegistryError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeJSON(w, http.StatusOK, struct {
		ApplicationID       string                             `json:"applicationId"`
		Type                domain.ApplicationRegistryPullMode `json:"type"`
		ProjectCredentialID string                             `json:"projectCredentialId,omitempty"`
	}{ApplicationID: result.Value.ApplicationID, Type: result.Value.Mode, ProjectCredentialID: result.Value.ProjectCredentialID})
}
