package httpapi

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/automation"
	"github.com/kuberploy/kuberploy/internal/domain"
)

const serviceAccountTokenPrefix = "kp_sa_"

type serviceAccountRequest struct {
	Name string            `json:"name"`
	Role domain.AccessRole `json:"role"`
}

func (s *Server) projectServiceAccounts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	actor := currentUser(r.Context())
	projectID := strings.TrimSpace(r.PathValue("id"))
	if projectID == "" {
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "projectId is required.")
		return
	}
	if r.Method == http.MethodGet {
		accounts, err := s.store.ListServiceAccounts(r.Context(), actor.ID, projectID)
		if err != nil {
			mappedError(w, r, err)
			return
		}
		collection(w, accounts)
		return
	}

	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	var in serviceAccountRequest
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if !automation.ValidName(in.Name) || !automation.ValidRole(in.Role) {
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "name must contain 1-100 bytes and role must be viewer, developer, or project-admin.")
		return
	}
	result, err := s.store.CreateServiceAccount(r.Context(), actor.ID, key, fingerprint(in), requestID(r.Context()), domain.CreateServiceAccount{ProjectID: projectID, Name: in.Name, Role: in.Role})
	if err != nil {
		mappedError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Location", "/v1/service-accounts/"+result.Value.ID)
	writeJSON(w, http.StatusCreated, result.Value)
}

type serviceAccountTokenRequest struct {
	Name      string                   `json:"name"`
	Scopes    []domain.AutomationScope `json:"scopes"`
	ExpiresAt time.Time                `json:"expiresAt"`
}

type serviceAccountTokenIssue struct {
	TokenRecord domain.ServiceAccountToken `json:"tokenRecord"`
	Token       string                     `json:"token,omitempty"`
}

func (s *Server) serviceAccountTokens(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	actor := currentUser(r.Context())
	serviceAccountID := strings.TrimSpace(r.PathValue("id"))
	if serviceAccountID == "" {
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "serviceAccountId is required.")
		return
	}
	if r.Method == http.MethodGet {
		tokens, err := s.store.ListServiceAccountTokens(r.Context(), actor.ID, serviceAccountID)
		if err != nil {
			mappedError(w, r, err)
			return
		}
		collection(w, tokens)
		return
	}

	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	var in serviceAccountTokenRequest
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	scopes, validScopes := automation.NormalizeScopes(in.Scopes)
	now := time.Now().UTC()
	if !automation.ValidName(in.Name) || !validScopes || !automation.ValidExpiry(now, in.ExpiresAt) {
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "name, closed token scopes, and an expiry between 5 minutes and 90 days are required.")
		return
	}
	in.Scopes = scopes
	in.ExpiresAt = in.ExpiresAt.UTC()
	requestFingerprint := fingerprint(in)
	if replay, found, err := s.store.ServiceAccountTokenReplay(r.Context(), actor.ID, serviceAccountID, key, requestFingerprint); err != nil {
		mappedError(w, r, err)
		return
	} else if found {
		w.Header().Set("Idempotent-Replay", "true")
		w.Header().Set("Location", "/v1/service-accounts/"+serviceAccountID+"/tokens/"+replay.Value.ID)
		writeJSON(w, http.StatusCreated, serviceAccountTokenIssue{TokenRecord: replay.Value})
		return
	}

	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "EntropyUnavailable", "Internal error", "Secure service-account token creation failed.")
		return
	}
	suffix := make([]byte, base64.RawURLEncoding.EncodedLen(len(random)))
	base64.RawURLEncoding.Encode(suffix, random)
	credential := make([]byte, 0, len(serviceAccountTokenPrefix)+len(suffix))
	credential = append(credential, serviceAccountTokenPrefix...)
	credential = append(credential, suffix...)
	tokenHash := sha256.Sum256(credential)
	prefix := string(credential[:len(serviceAccountTokenPrefix)+8])
	for index := range random {
		random[index] = 0
	}
	for index := range suffix {
		suffix[index] = 0
	}

	result, err := s.store.CreateServiceAccountToken(r.Context(), actor.ID, key, requestFingerprint, requestID(r.Context()), domain.CreateServiceAccountToken{
		ServiceAccountID: serviceAccountID,
		Name:             in.Name,
		Prefix:           prefix,
		TokenHash:        tokenHash[:],
		Scopes:           scopes,
		ExpiresAt:        in.ExpiresAt,
	})
	if err != nil {
		for index := range credential {
			credential[index] = 0
		}
		mappedError(w, r, err)
		return
	}
	response := serviceAccountTokenIssue{TokenRecord: result.Value}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	} else {
		response.Token = string(credential)
	}
	for index := range credential {
		credential[index] = 0
	}
	w.Header().Set("Location", "/v1/service-accounts/"+serviceAccountID+"/tokens/"+result.Value.ID)
	writeJSON(w, http.StatusCreated, response)
	response.Token = ""
}

func (s *Server) revokeServiceAccountToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	serviceAccountID := strings.TrimSpace(r.PathValue("serviceAccountId"))
	tokenID := strings.TrimSpace(r.PathValue("tokenId"))
	if serviceAccountID == "" || tokenID == "" {
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "serviceAccountId and tokenId are required.")
		return
	}
	replay, err := s.store.RevokeServiceAccountToken(r.Context(), currentUser(r.Context()).ID, serviceAccountID, tokenID, key, fingerprint(struct {
		ServiceAccountID string `json:"serviceAccountId"`
		TokenID          string `json:"tokenId"`
	}{serviceAccountID, tokenID}), requestID(r.Context()))
	if err != nil {
		mappedError(w, r, err)
		return
	}
	if replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) disableServiceAccount(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	serviceAccountID := strings.TrimSpace(r.PathValue("id"))
	if serviceAccountID == "" {
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "serviceAccountId is required.")
		return
	}
	replay, err := s.store.DisableServiceAccount(r.Context(), currentUser(r.Context()).ID, serviceAccountID, key, fingerprint(struct {
		ServiceAccountID string `json:"serviceAccountId"`
	}{serviceAccountID}), requestID(r.Context()))
	if err != nil {
		mappedError(w, r, err)
		return
	}
	if replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.WriteHeader(http.StatusNoContent)
}
