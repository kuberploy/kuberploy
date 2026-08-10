package httpapi

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

// CertificateIssuerCatalog exposes only safe identities from the exact
// operator-owned cert-manager profile. Account email, ACME account Secrets,
// DNS provider Secret references, and raw solver configuration never cross
// this boundary.
type CertificateIssuerCatalog interface {
	ApprovedCertificateIssuers(context.Context, string, time.Time) ([]CertificateIssuerCatalogItem, error)
}

type CertificateIssuerCatalogItem struct {
	Name        string   `json:"name"`
	Environment string   `json:"environment"`
	SolverTypes []string `json:"solverTypes"`
	Source      string   `json:"source"`
	Revision    int64    `json:"revision,omitempty"`
}

func (s *Server) applicationCertificateIssuerCatalog(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.certificateIssuers == nil || s.edgeReadiness == nil || !s.edgeFeatures.CertManager {
		certificateIssuerCatalogUnavailable(w, r)
		return
	}
	applicationID := strings.TrimSpace(r.PathValue("id"))
	environmentID, hostname, ok := certificateIssuerCatalogQuery(w, r)
	if !ok {
		return
	}
	_, target, err := s.resolveSecretScope(r.Context(), applicationID, environmentID, applicationID)
	if err != nil {
		mappedError(w, r, err)
		return
	}
	target.Type = "application"
	target.ID = applicationID
	if err = s.store.Authorize(r.Context(), currentUser(r.Context()).ID, domain.PermissionResourcesRead, target); err != nil {
		mappedError(w, r, err)
		return
	}
	if err = s.edgeReadiness.Probe(r.Context()); err != nil {
		certificateIssuerCatalogUnavailable(w, r)
		return
	}
	items, err := s.certificateIssuers.ApprovedCertificateIssuers(r.Context(), hostname, time.Now().UTC())
	if err != nil || len(items) == 0 {
		certificateIssuerCatalogUnavailable(w, r)
		return
	}
	for _, item := range items {
		if item.Name == "" || item.Environment != "production" && item.Environment != "staging" || len(item.SolverTypes) == 0 ||
			item.Source != "bootstrap" && item.Source != "managed" || item.Source == "managed" && item.Revision < 1 {
			certificateIssuerCatalogUnavailable(w, r)
			return
		}
	}
	writeJSON(w, http.StatusOK, struct {
		Items []CertificateIssuerCatalogItem `json:"items"`
	}{Items: items})
}

func certificateIssuerCatalogQuery(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	environments, hostnames := values["environmentId"], values["hostname"]
	if err != nil || len(values) != 2 || len(environments) != 1 || len(hostnames) != 1 || !validUUID(environments[0]) ||
		strings.TrimSpace(environments[0]) != environments[0] || !validCertificateIssuerHostname(hostnames[0]) {
		writeProblem(w, r, http.StatusBadRequest, "InvalidCertificateIssuerScope", "Invalid certificate issuer scope",
			"Provide exactly one canonical UUID environmentId and one lowercase public hostname.")
		return "", "", false
	}
	return environments[0], hostnames[0], true
}

func validCertificateIssuerHostname(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || value != strings.ToLower(value) || len(value) > 253 ||
		!strings.Contains(value, ".") || strings.ContainsAny(value, "*/:@") || net.ParseIP(value) != nil {
		return false
	}
	parsed, err := url.Parse("https://" + value)
	return err == nil && parsed.Hostname() == value && parsed.Host == value && parsed.Path == "" && parsed.RawQuery == "" && parsed.Fragment == ""
}

func certificateIssuerCatalogUnavailable(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, http.StatusServiceUnavailable, "CertificateIssuerCatalogUnavailable", "Certificate issuers unavailable",
		"The exact operator-approved cert-manager issuer catalog is unavailable or not freshly observed.")
}
