package httpapi

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/appconfig"
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
	items, ok := s.approvedCertificateIssuerCatalog(r.Context(), hostname, time.Now().UTC())
	if !ok {
		certificateIssuerCatalogUnavailable(w, r)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Items []CertificateIssuerCatalogItem `json:"items"`
	}{Items: items})
}

func (s *Server) approvedCertificateIssuerCatalog(ctx context.Context, hostname string, now time.Time) ([]CertificateIssuerCatalogItem, bool) {
	if s.certificateIssuers == nil || s.edgeReadiness == nil || !s.edgeFeatures.CertManager ||
		!validCertificateIssuerHostname(hostname) || now.IsZero() || s.edgeReadiness.Probe(ctx) != nil {
		return nil, false
	}
	items, err := s.certificateIssuers.ApprovedCertificateIssuers(ctx, hostname, now.UTC())
	if err != nil || len(items) == 0 {
		return nil, false
	}
	for _, item := range items {
		if item.Name == "" || item.Environment != "production" && item.Environment != "staging" || len(item.SolverTypes) == 0 ||
			item.Source != "bootstrap" && item.Source != "managed" || item.Source == "managed" && item.Revision < 1 {
			return nil, false
		}
	}
	return items, true
}

func (s *Server) certificateIssuerRouteDiagnostics(ctx context.Context, candidate appconfig.Candidate) []appconfig.Diagnostic {
	if len(candidate.Diagnostics) != 0 || candidate.Parsed == nil {
		return nil
	}
	spec, _ := candidate.Parsed["spec"].(map[string]any)
	routes, _ := spec["routes"].([]any)
	diagnostics := make([]appconfig.Diagnostic, 0)
	for index, rawRoute := range routes {
		route, _ := rawRoute.(map[string]any)
		tls, _ := route["tls"].(map[string]any)
		if mode, _ := tls["mode"].(string); mode != "letsencrypt" {
			continue
		}
		pointer := "/spec/routes/" + strconv.Itoa(index) + "/tls/issuerRef"
		hostname, _ := route["host"].(string)
		items, ok := s.approvedCertificateIssuerCatalog(ctx, hostname, time.Now().UTC())
		if !ok {
			diagnostics = append(diagnostics, appconfig.Diagnostic{Code: "CertificateIssuerCatalogUnavailable", Detail: "The exact operator-approved cert-manager issuer catalog is unavailable or not freshly observed.", Pointer: pointer})
			continue
		}
		issuerRef, _ := tls["issuerRef"].(string)
		approved := false
		for _, item := range items {
			if item.Name == issuerRef {
				approved = true
				break
			}
		}
		if !approved {
			diagnostics = append(diagnostics, appconfig.Diagnostic{Code: "CertificateIssuerNotApproved", Detail: "Select a freshly observed issuer from the exact operator-approved catalog for this hostname.", Pointer: pointer})
		}
	}
	return diagnostics
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
