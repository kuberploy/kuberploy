package httpapi

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/automation"
	"github.com/kuberploy/kuberploy/internal/domain"
)

const (
	authenticationSession        = "session"
	authenticationServiceAccount = "service-account"
)

type requestAuthentication struct {
	Kind             string                   `json:"kind"`
	ServiceAccountID string                   `json:"serviceAccountId,omitempty"`
	TokenID          string                   `json:"tokenId,omitempty"`
	Scopes           []domain.AutomationScope `json:"scopes,omitempty"`
	ExpiresAt        *time.Time               `json:"expiresAt,omitempty"`
}

func currentAuthentication(r *http.Request) requestAuthentication {
	authentication, _ := r.Context().Value(authenticationKey).(requestAuthentication)
	return authentication
}

func (s *Server) authenticateRequest(w http.ResponseWriter, r *http.Request) (domain.User, requestAuthentication, []byte, bool) {
	authorization := r.Header.Values("Authorization")
	if len(authorization) > 0 {
		if _, err := r.Cookie(sessionCookie); err == nil {
			rejectBearer(w, r)
			return domain.User{}, requestAuthentication{}, nil, false
		}
		if len(authorization) != 1 || !strings.HasPrefix(authorization[0], "Bearer ") || strings.Count(authorization[0], " ") != 1 {
			rejectBearer(w, r)
			return domain.User{}, requestAuthentication{}, nil, false
		}
		token := strings.TrimPrefix(authorization[0], "Bearer ")
		if !validServiceAccountBearer(token) {
			rejectBearer(w, r)
			return domain.User{}, requestAuthentication{}, nil, false
		}
		tokenBytes := []byte(token)
		tokenHash := sha256.Sum256(tokenBytes)
		for index := range tokenBytes {
			tokenBytes[index] = 0
		}
		principal, err := s.store.ServiceAccountByToken(r.Context(), tokenHash[:], time.Now().UTC())
		if err != nil {
			rejectBearer(w, r)
			return domain.User{}, requestAuthentication{}, nil, false
		}
		expires := principal.ExpiresAt
		return principal.User, requestAuthentication{Kind: authenticationServiceAccount, ServiceAccountID: principal.ServiceAccountID, TokenID: principal.TokenID, Scopes: append([]domain.AutomationScope(nil), principal.Scopes...), ExpiresAt: &expires}, nil, true
	}

	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		writeProblem(w, r, http.StatusUnauthorized, "Unauthenticated", "Authentication required", "A valid Kuberploy session or service-account bearer token is required.")
		return domain.User{}, requestAuthentication{}, nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil || len(raw) != 32 {
		writeProblem(w, r, http.StatusUnauthorized, "Unauthenticated", "Authentication required", "The session is invalid or expired.")
		return domain.User{}, requestAuthentication{}, nil, false
	}
	hash := sha256.Sum256(raw)
	for index := range raw {
		raw[index] = 0
	}
	u, err := s.store.UserBySession(r.Context(), hash[:], time.Now())
	if err != nil || u.Role != "platform-admin" && u.Role != "developer" {
		writeProblem(w, r, http.StatusUnauthorized, "Unauthenticated", "Authentication required", "The session is invalid or expired.")
		return domain.User{}, requestAuthentication{}, nil, false
	}
	return u, requestAuthentication{Kind: authenticationSession}, append([]byte(nil), hash[:]...), true
}

func (s *Server) authenticateGitHubSetupReturn(w http.ResponseWriter, r *http.Request) (domain.User, []byte, bool) {
	if len(r.Header.Values("Authorization")) != 0 {
		writeProblem(w, r, http.StatusUnauthorized, "Unauthenticated", "Authentication required", "The GitHub setup browser session is invalid or expired.")
		return domain.User{}, nil, false
	}
	cookies := r.CookiesNamed(githubSetupSessionCookie)
	if len(cookies) != 1 || cookies[0].Value == "" {
		writeProblem(w, r, http.StatusUnauthorized, "Unauthenticated", "Authentication required", "The GitHub setup browser session is invalid or expired.")
		return domain.User{}, nil, false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(cookies[0].Value)
	if err != nil || len(raw) != 32 {
		for index := range raw {
			raw[index] = 0
		}
		writeProblem(w, r, http.StatusUnauthorized, "Unauthenticated", "Authentication required", "The GitHub setup browser session is invalid or expired.")
		return domain.User{}, nil, false
	}
	hash := sha256.Sum256(raw)
	for index := range raw {
		raw[index] = 0
	}
	u, err := s.store.UserBySession(r.Context(), hash[:], time.Now().UTC())
	if err != nil || u.Role != "platform-admin" && u.Role != "developer" {
		writeProblem(w, r, http.StatusUnauthorized, "Unauthenticated", "Authentication required", "The GitHub setup browser session is invalid or expired.")
		return domain.User{}, nil, false
	}
	return u, append([]byte(nil), hash[:]...), true
}

func validServiceAccountBearer(token string) bool {
	if !strings.HasPrefix(token, "kp_sa_") || len(token) != len("kp_sa_")+43 {
		return false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(token, "kp_sa_"))
	valid := err == nil && len(raw) == 32
	for index := range raw {
		raw[index] = 0
	}
	return valid
}

func rejectBearer(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="kuberploy", error="invalid_token"`)
	writeProblem(w, r, http.StatusUnauthorized, "Unauthenticated", "Authentication required", "The service-account bearer token is invalid, expired, or revoked.")
}

func (s *Server) requireAutomationScope(scope domain.AutomationScope, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authentication := currentAuthentication(r)
		if authentication.Kind == authenticationServiceAccount && !automation.Allows(authentication.Scopes, scope) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="kuberploy", error="insufficient_scope", scope="`+string(scope)+`"`)
			writeProblem(w, r, http.StatusForbidden, "InsufficientTokenScope", "Permission denied", "The service-account token does not carry the required coarse API scope.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) humanOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if currentAuthentication(r).Kind == authenticationServiceAccount {
			writeProblem(w, r, http.StatusForbidden, "HumanSessionRequired", "Permission denied", "This credential-management or administrative operation requires an interactive human session.")
			return
		}
		next.ServeHTTP(w, r)
	})
}
