package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/store"
)

var errSSLIPDeploymentUnavailable = errors.New("sslip deployment hostname is unavailable")

func (s *Server) resolveSSLIPDeploymentHostname(ctx context.Context, actor, applicationID, environmentID string) (string, error) {
	if s == nil || s.sslip == nil || s.edgeReadiness == nil || !s.edgeFeatures.Traefik {
		return "", errSSLIPDeploymentUnavailable
	}
	application, err := s.store.GetApplicationForActor(ctx, actor, applicationID)
	if err != nil {
		return "", err
	}
	environment, err := s.store.GetEnvironmentForActor(ctx, actor, environmentID)
	if err != nil {
		return "", err
	}
	if application.ProjectID == "" || application.ProjectID != environment.ProjectID {
		return "", store.ErrNotFound
	}
	probeContext, cancel := context.WithTimeout(ctx, 2*time.Second)
	probeErr := s.edgeReadiness.Probe(probeContext)
	cancel()
	if probeErr != nil {
		return "", errSSLIPDeploymentUnavailable
	}
	preview, err := s.sslip.ResolveSSLIPHostname(ctx, SSLIPHostnameRequest{ApplicationID: application.ID,
		EnvironmentID: environment.ID, ProjectID: application.ProjectID, Namespace: environment.Namespace})
	if err != nil || !preview.valid() {
		return "", errSSLIPDeploymentUnavailable
	}
	return preview.Hostname, nil
}

func (s *Server) sslipRouteDiagnostics(ctx context.Context, actor string, deployment domain.Deployment, candidate appconfig.Candidate) ([]appconfig.Diagnostic, bool) {
	if len(candidate.Diagnostics) != 0 || candidate.Parsed == nil {
		return nil, false
	}
	spec, ok := candidate.Parsed["spec"].(map[string]any)
	if !ok {
		return nil, false
	}
	rawRoutes, ok := spec["routes"].([]any)
	if !ok {
		return nil, false
	}
	type route struct {
		index int
		host  string
	}
	routes := make([]route, 0, len(rawRoutes))
	for index, raw := range rawRoutes {
		value, valueOK := raw.(map[string]any)
		dns, dnsOK := value["dns"].(map[string]any)
		mode, modeOK := dns["mode"].(string)
		if !valueOK || !dnsOK || !modeOK || mode != "sslip" {
			continue
		}
		host, hostOK := value["host"].(string)
		if !hostOK {
			continue
		}
		routes = append(routes, route{index: index, host: host})
	}
	if len(routes) == 0 {
		return nil, false
	}
	expected, err := s.resolveSSLIPDeploymentHostname(ctx, actor, deployment.ApplicationID, deployment.EnvironmentID)
	if err != nil {
		diagnostics := make([]appconfig.Diagnostic, 0, len(routes))
		for _, route := range routes {
			diagnostics = append(diagnostics, appconfig.Diagnostic{Code: "SSLIPHostnameUnavailable", Detail: "No fresh exact public Traefik ingress address is available for sslip.io.", Pointer: "/spec/routes/" + strconv.Itoa(route.index) + "/dns"})
		}
		return diagnostics, true
	}
	diagnostics := []appconfig.Diagnostic{}
	for _, route := range routes {
		if route.host != expected {
			diagnostics = append(diagnostics, appconfig.Diagnostic{Code: "SSLIPHostnameMismatch", Detail: "Use the exact server-derived sslip.io hostname returned for this application and environment.", Pointer: "/spec/routes/" + strconv.Itoa(route.index) + "/host"})
		}
	}
	return diagnostics, true
}

func mappedSSLIPDeploymentError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrForbidden) || errors.Is(err, store.ErrNotFound) {
		mappedError(w, r, err)
		return
	}
	sslipUnavailable(w, r)
}
