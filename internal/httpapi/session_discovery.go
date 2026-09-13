package httpapi

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/store"
)

// sessionDiscovery lets the browser distinguish a signed-out page from a
// database outage without making an expected unauthorized protected request.
// It does not establish a session or authorize any other operation.
func (s *Server) sessionDiscovery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if len(r.Header.Values("Authorization")) != 0 {
		writeProblem(w, r, http.StatusUnauthorized, "Unauthenticated", "Browser session required", "Session discovery accepts only a browser session cookie.")
		return
	}
	var principal *struct {
		domain.User
		Authentication requestAuthentication `json:"authentication"`
	}
	respond := func() {
		writeJSON(w, http.StatusOK, map[string]any{"principal": principal})
	}
	cookies := r.CookiesNamed(sessionCookie)
	if len(cookies) != 1 {
		respond()
		return
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(cookies[0].Value)
	valid := err == nil && len(raw) == 32
	hash := sha256.Sum256(raw)
	for index := range raw {
		raw[index] = 0
	}
	if !valid {
		respond()
		return
	}
	u, err := s.store.UserBySession(r.Context(), hash[:], time.Now().UTC())
	if errors.Is(err, store.ErrNotFound) {
		respond()
		return
	}
	if err != nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "DatabaseUnavailable", "Database unavailable", "The browser session could not be checked. Retry when the database is available.")
		return
	}
	if u.Issuer != "kuberploy:service-account" && (u.Role == "platform-admin" || u.Role == "developer") {
		principal = &struct {
			domain.User
			Authentication requestAuthentication `json:"authentication"`
		}{User: u, Authentication: requestAuthentication{Kind: authenticationSession}}
	}
	respond()
}
