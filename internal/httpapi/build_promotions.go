package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/kuberploy/kuberploy/internal/buildpromotion"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/store"
)

// buildPromotionRequest deliberately reuses the closed deployment runtime and
// route inputs while excluding every build-derived authority coordinate.
type buildPromotionRequest struct {
	EnvironmentID string                 `json:"environmentId"`
	Runtime       domain.WorkloadRuntime `json:"runtime"`
	Route         *routeRequest          `json:"route,omitempty"`
}

type buildPromotionFingerprint struct {
	AttemptID string                `json:"attemptId"`
	Request   buildPromotionRequest `json:"request"`
}

func (s *Server) promoteBuildAttempt(w http.ResponseWriter, r *http.Request) {
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	var in buildPromotionRequest
	if !decode(w, r, &in) {
		return
	}
	in.EnvironmentID = strings.TrimSpace(in.EnvironmentID)
	in.Runtime = domain.NormalizeWorkloadRuntime(in.Runtime)
	if problems := domain.ValidateWorkloadRuntime(in.Runtime); len(problems) > 0 {
		limit := len(problems)
		if limit > 50 {
			limit = 50
		}
		fields := make([]FieldError, 0, limit)
		for _, problem := range problems[:limit] {
			fields = append(fields, FieldError{Pointer: problem.Pointer, Code: problem.Code, Detail: problem.Detail})
		}
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "The workload runtime configuration is invalid.", fields...)
		return
	}
	if !normalizePromotionRoute(w, r, in.Route) {
		return
	}
	attemptID := strings.TrimSpace(r.PathValue("id"))
	fp := fingerprint(buildPromotionFingerprint{AttemptID: attemptID, Request: in})
	if s.buildPromotions == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "BuildPromotionUnavailable", "Build promotion unavailable", "The verified build release projection is not configured.")
		return
	}
	actor := currentUser(r.Context()).ID
	request := buildpromotion.Request{ActorID: actor, AttemptID: attemptID, EnvironmentID: in.EnvironmentID}
	authorized, err := s.buildPromotions.ResolveAuthorized(r.Context(), request)
	if err != nil {
		mappedBuildPromotionError(w, r, err)
		return
	}
	if problems := domain.ValidateApplicationScheduling(in.Runtime, authorized.ApplicationID); len(problems) > 0 {
		limit := len(problems)
		if limit > 50 {
			limit = 50
		}
		fields := make([]FieldError, 0, limit)
		for _, problem := range problems[:limit] {
			fields = append(fields, FieldError{Pointer: problem.Pointer, Code: problem.Code, Detail: problem.Detail})
		}
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "The workload scheduling configuration is invalid.", fields...)
		return
	}
	replicas, port, ordinary := domain.LegacyWorkloadFields(in.Runtime)
	var route *domain.Route
	if in.Route != nil {
		route = &domain.Route{Hostname: in.Route.Hostname, DNSMode: in.Route.DNSMode, PathPrefix: in.Route.PathPrefix, TLSMode: in.Route.TLSMode}
	}
	create := domain.CreateDeployment{EnvironmentID: authorized.EnvironmentID, ApplicationID: authorized.ApplicationID, Image: authorized.ImageReference, Replicas: replicas, Port: port, Environment: ordinary, Runtime: in.Runtime, Route: route}

	// Recover a committed response before registry, Git, Argo, pull-profile, or
	// sslip readiness checks. A deliberately invalid write plan cannot create a
	// new deployment, while CreateDeployment resolves matching idempotency first.
	result, operation, replayErr := s.store.CreateDeployment(r.Context(), actor, key, fp, requestID(r.Context()), create, &gitprojection.WritePlan{})
	if replayErr == nil && result.Replay {
		writeDeploymentAccepted(w, result, operation)
		return
	}
	if errors.Is(replayErr, store.ErrIdempotencyConflict) || errors.Is(replayErr, store.ErrForbidden) {
		mappedError(w, r, replayErr)
		return
	}

	// Requiring the independent RegistryRelease only after replay recovery keeps
	// new promotions fail-closed while an already accepted command remains
	// exactly recoverable if artifact observation later becomes unavailable.
	source, err := s.buildPromotions.Resolve(r.Context(), request)
	if err != nil {
		mappedBuildPromotionError(w, r, err)
		return
	}
	create.Image = source.ImageReference
	if in.Route != nil && in.Route.DNSMode == "sslip" {
		hostname, resolveErr := s.resolveSSLIPDeploymentHostname(r.Context(), actor, source.ApplicationID, source.EnvironmentID)
		if resolveErr != nil {
			mappedSSLIPDeploymentError(w, r, resolveErr)
			return
		}
		create.Route.Hostname = hostname
	}
	s.submitDeployment(w, r, actor, key, fp, create, true)
}

func normalizePromotionRoute(w http.ResponseWriter, r *http.Request, route *routeRequest) bool {
	if route == nil {
		return true
	}
	route.Hostname = strings.ToLower(strings.TrimSpace(route.Hostname))
	route.DNSMode = strings.TrimSpace(route.DNSMode)
	if route.DNSMode == "" {
		route.DNSMode = "manual"
	}
	if route.PathPrefix == "" {
		route.PathPrefix = "/"
	}
	if route.TLSMode == "" {
		route.TLSMode = "httpOnly"
	}
	if route.PathPrefix != "/" || route.TLSMode != "httpOnly" ||
		(route.DNSMode == "manual" && !validHostname(route.Hostname)) ||
		(route.DNSMode == "sslip" && route.Hostname != "") ||
		(route.DNSMode != "manual" && route.DNSMode != "sslip") {
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "Use an HTTP-only manual hostname or request the server-derived sslip DNS mode.", FieldError{Pointer: "/route", Code: "InvalidRoute", Detail: "manual requires hostname; sslip forbids caller hostname and accepts only dnsMode."})
		return false
	}
	return true
}

func mappedBuildPromotionError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, buildpromotion.ErrInvalid):
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "The build attempt and environment identifiers must be valid UUIDs.")
	case errors.Is(err, buildpromotion.ErrNotFound), errors.Is(err, store.ErrNotFound):
		mappedError(w, r, store.ErrNotFound)
	case errors.Is(err, store.ErrForbidden):
		mappedError(w, r, store.ErrForbidden)
	case errors.Is(err, buildpromotion.ErrArtifactUnavailable):
		writeProblem(w, r, http.StatusServiceUnavailable, "BuildArtifactUnavailable", "Build artifact unavailable", "The exact verified release is no longer observed as present in the registry.")
	case errors.Is(err, buildpromotion.ErrNotReady):
		writeProblem(w, r, http.StatusServiceUnavailable, "BuildReleaseProjectionUnavailable", "Build release projection unavailable", "The exact successful build attempt has not completed its durable release projection.")
	case errors.Is(err, buildpromotion.ErrConflict):
		writeProblem(w, r, http.StatusServiceUnavailable, "BuildReleaseProjectionMismatch", "Build release projection unavailable", "The build attempt and registry release projection do not match exactly.")
	default:
		writeProblem(w, r, http.StatusServiceUnavailable, "BuildPromotionUnavailable", "Build promotion unavailable", "The verified build release projection could not be resolved safely.")
	}
}
