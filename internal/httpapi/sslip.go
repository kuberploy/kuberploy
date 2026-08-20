package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/store"
)

var sslipHostnamePattern = regexp.MustCompile(`^kp-[0-9a-f]{20}\.[0-9]{1,3}-[0-9]{1,3}-[0-9]{1,3}-[0-9]{1,3}\.sslip\.io$`)

type SSLIPHostnameSource string

const (
	SSLIPSourceServiceIP      SSLIPHostnameSource = "service-ip"
	SSLIPSourceVerifiedStatic SSLIPHostnameSource = "verified-static-ip"
)

// SSLIPHostnameRequest contains only central-store-derived destination
// identity. A caller can never provide an IP, hostname, namespace, or source.
type SSLIPHostnameRequest struct {
	ApplicationID string
	EnvironmentID string
	ProjectID     string
	Namespace     string
}

// SSLIPHostnamePreview is the complete public response. The concrete resolver
// must reject absent, stale, or mismatched observations and never returns the
// underlying raw IP as a separate field.
type SSLIPHostnamePreview struct {
	Mode       string              `json:"mode"`
	Hostname   string              `json:"hostname"`
	Source     SSLIPHostnameSource `json:"source"`
	ObservedAt time.Time           `json:"observedAt"`
}

type SSLIPHostnameResolver interface {
	Probe(context.Context) error
	ResolveSSLIPHostname(context.Context, SSLIPHostnameRequest) (SSLIPHostnamePreview, error)
}

func (p SSLIPHostnamePreview) valid() bool {
	return p.Mode == "sslip" && validSSLIPHostname(p.Hostname) &&
		(p.Source == SSLIPSourceServiceIP || p.Source == SSLIPSourceVerifiedStatic) && !p.ObservedAt.IsZero()
}

func validSSLIPHostname(value string) bool {
	if len(value) < len("a.sslip.io") || len(value) > 253 || value != strings.ToLower(value) ||
		strings.TrimSpace(value) != value || !strings.HasSuffix(value, ".sslip.io") || !sslipHostnamePattern.MatchString(value) {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if character > unicode.MaxASCII || character != '-' && !unicode.IsLower(character) && !unicode.IsDigit(character) {
				return false
			}
		}
	}
	return true
}

func (s *Server) sslipNoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Pragma", "no-cache")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) sslipReady(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.sslip == nil || !s.edgeFeatures.Traefik {
			sslipUnavailable(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		// The resolver probes the exact current Traefik target. A global edge
		// digest from API startup becomes stale when managed ExternalDNS targets
		// are admitted or removed and must not disable an otherwise healthy
		// Traefik/SSLIP path.
		if err := s.sslip.Probe(ctx); err != nil {
			sslipUnavailable(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) sslipHostname(w http.ResponseWriter, r *http.Request) {
	if !certificateBodyEmpty(w, r) {
		return
	}
	environmentID, ok := sslipEnvironmentQuery(w, r)
	if !ok {
		return
	}
	applicationID := strings.TrimSpace(r.PathValue("id"))
	application, err := s.store.GetApplication(r.Context(), applicationID)
	if err != nil {
		mappedError(w, r, err)
		return
	}
	environment, err := s.store.GetEnvironment(r.Context(), environmentID)
	if err != nil {
		mappedError(w, r, err)
		return
	}
	if application.ProjectID == "" || application.ProjectID != environment.ProjectID {
		mappedError(w, r, store.ErrNotFound)
		return
	}
	actor := currentUser(r.Context()).ID
	applicationAuthorization := s.store.Authorize(r.Context(), actor, domain.PermissionResourcesRead, domain.AccessTarget{Type: "application", ID: application.ID})
	environmentAuthorization := s.store.Authorize(r.Context(), actor, domain.PermissionResourcesRead, domain.AccessTarget{Type: "environment", ID: environment.ID})
	if applicationAuthorization != nil || environmentAuthorization != nil {
		if errors.Is(applicationAuthorization, store.ErrForbidden) || errors.Is(environmentAuthorization, store.ErrForbidden) {
			mappedError(w, r, store.ErrForbidden)
		} else if applicationAuthorization != nil {
			mappedError(w, r, applicationAuthorization)
		} else {
			mappedError(w, r, environmentAuthorization)
		}
		return
	}
	preview, err := s.sslip.ResolveSSLIPHostname(r.Context(), SSLIPHostnameRequest{
		ApplicationID: application.ID, EnvironmentID: environment.ID, ProjectID: application.ProjectID, Namespace: environment.Namespace,
	})
	if err != nil || !preview.valid() {
		sslipUnavailable(w, r)
		return
	}
	preview.ObservedAt = preview.ObservedAt.UTC()
	writeJSON(w, http.StatusOK, preview)
}

func sslipEnvironmentQuery(w http.ResponseWriter, r *http.Request) (string, bool) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	entries := values["environmentId"]
	if err != nil || len(values) != 1 || len(entries) != 1 || strings.TrimSpace(entries[0]) != entries[0] || !validUUID(entries[0]) {
		writeProblem(w, r, http.StatusBadRequest, "InvalidEnvironment", "Invalid environment", "Provide exactly one canonical UUID environmentId query parameter.")
		return "", false
	}
	return entries[0], true
}

func sslipUnavailable(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, http.StatusServiceUnavailable, "SSLIPHostnameUnavailable", "sslip.io hostname unavailable", "A fresh exact public ingress IP observation is unavailable or does not match this environment.")
}
