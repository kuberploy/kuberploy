package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/observability"
	"github.com/kuberploy/kuberploy/internal/store"
)

var metricQueryParameters = map[string]struct{}{
	"scopeType": {},
	"scopeId":   {},
	"metric":    {},
	"from":      {},
	"to":        {},
	"step":      {},
}

func (s *Server) metricsQueryRange(w http.ResponseWriter, r *http.Request) {
	if s.metrics == nil || s.monitoringMode == "disabled" {
		writeProblem(w, r, http.StatusServiceUnavailable, "MonitoringUnavailable", "Metrics unavailable", "No healthy Prometheus-compatible query boundary is configured.")
		return
	}
	query := r.URL.Query()
	for key, values := range query {
		if _, allowed := metricQueryParameters[key]; !allowed || len(values) != 1 || strings.TrimSpace(values[0]) != values[0] || values[0] == "" {
			writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "The metrics query contains an unknown, repeated, empty, or non-canonical parameter.", FieldError{Pointer: "/query/" + key, Code: "InvalidQueryParameter", Detail: "Use exactly one supported value."})
			return
		}
	}
	for required := range metricQueryParameters {
		if !query.Has(required) {
			writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "All metrics query parameters are required.", FieldError{Pointer: "/query/" + required, Code: "Required", Detail: "Supply exactly one value."})
			return
		}
	}
	metric := observability.MetricKey(query.Get("metric"))
	if !observability.ValidMetric(metric) {
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "The metric is not in the platform query catalog.", FieldError{Pointer: "/query/metric", Code: "UnsupportedMetric", Detail: "Select a documented metric key."})
		return
	}
	from, fromErr := time.Parse(time.RFC3339Nano, query.Get("from"))
	to, toErr := time.Parse(time.RFC3339Nano, query.Get("to"))
	step, stepErr := parseMetricStep(query.Get("step"))
	if fromErr != nil || toErr != nil || stepErr != nil {
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "The metrics time range is invalid.", FieldError{Pointer: "/query", Code: "InvalidRange", Detail: "Use RFC 3339 timestamps and an integer step from 10s through 3600s."})
		return
	}
	scope, err := s.resolveMetricScope(r.Context(), currentUser(r.Context()).ID, query.Get("scopeType"), query.Get("scopeId"))
	if err != nil {
		mappedError(w, r, err)
		return
	}
	result, err := s.metrics.QueryRange(r.Context(), scope, metric, observability.Range{From: from, To: to, Step: step})
	if err != nil {
		switch {
		case errors.Is(err, observability.ErrInvalidMetric), errors.Is(err, observability.ErrInvalidRange), errors.Is(err, observability.ErrInvalidScope):
			writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", observability.ErrorMessage(err))
		case errors.Is(err, observability.ErrRateLimited):
			w.Header().Set("Retry-After", "15")
			writeProblem(w, r, http.StatusTooManyRequests, "MonitoringRateLimited", "Metrics temporarily limited", observability.ErrorMessage(err))
		default:
			writeProblem(w, r, http.StatusServiceUnavailable, "MonitoringUnavailable", "Metrics unavailable", observability.ErrorMessage(err))
		}
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=15")
	writeJSON(w, http.StatusOK, result)
}

func parseMetricStep(value string) (time.Duration, error) {
	if len(value) < 2 || len(value) > 5 || !strings.HasSuffix(value, "s") {
		return 0, observability.ErrInvalidRange
	}
	seconds, err := strconv.Atoi(strings.TrimSuffix(value, "s"))
	if err != nil || seconds < 10 || seconds > 3600 || strconv.Itoa(seconds)+"s" != value {
		return 0, observability.ErrInvalidRange
	}
	return time.Duration(seconds) * time.Second, nil
}

func (s *Server) resolveMetricScope(ctx context.Context, actorID, scopeType, scopeID string) (observability.Scope, error) {
	switch observability.ScopeType(scopeType) {
	case observability.ScopeService:
		deployment, err := s.store.GetDeploymentForActor(ctx, actorID, scopeID)
		if err != nil {
			return observability.Scope{}, err
		}
		environment, err := s.store.GetEnvironment(ctx, deployment.EnvironmentID)
		if err != nil {
			return observability.Scope{}, err
		}
		application, err := s.store.GetApplication(ctx, deployment.ApplicationID)
		if err != nil {
			return observability.Scope{}, err
		}
		project, err := s.store.GetProject(ctx, environment.ProjectID)
		if err != nil {
			return observability.Scope{}, err
		}
		if application.ProjectID != environment.ProjectID {
			return observability.Scope{}, errors.New("deployment ownership projection is inconsistent")
		}
		target := domain.AccessTarget{Type: "deployment", ID: deployment.ID, TeamID: project.TeamID, ProjectID: project.ID, EnvironmentID: environment.ID, Namespace: environment.Namespace, ApplicationID: application.ID}
		if err = s.store.Authorize(ctx, actorID, domain.PermissionMetricsRead, target); err != nil {
			return observability.Scope{}, err
		}
		// P0 has one stable HTTP service per application. The opaque API scope is
		// the Deployment ID, while the protected runtime/recording-rule label is
		// the stable application ID shared across retained releases.
		return observability.Scope{Type: observability.ScopeService, Namespace: environment.Namespace, Project: project.ID, Environment: environment.ID, Application: application.ID, Service: application.ID}, nil
	case observability.ScopeNamespace:
		environment, err := s.store.GetEnvironmentForActor(ctx, actorID, scopeID)
		if err != nil {
			return observability.Scope{}, err
		}
		project, err := s.store.GetProject(ctx, environment.ProjectID)
		if err != nil {
			return observability.Scope{}, err
		}
		target := domain.AccessTarget{Type: "environment", ID: environment.ID, TeamID: project.TeamID, ProjectID: project.ID, EnvironmentID: environment.ID, Namespace: environment.Namespace}
		if err = s.store.Authorize(ctx, actorID, domain.PermissionMetricsRead, target); err != nil {
			return observability.Scope{}, err
		}
		return observability.Scope{Type: observability.ScopeNamespace, Namespace: environment.Namespace}, nil
	case observability.ScopeGlobal:
		if scopeID != "platform" {
			return observability.Scope{}, domainScopeNotFound()
		}
		if err := s.store.Authorize(ctx, actorID, domain.PermissionPlatformAdmin, domain.AccessTarget{Type: "platform", ID: "platform"}); err != nil {
			return observability.Scope{}, err
		}
		return observability.Scope{Type: observability.ScopeGlobal}, nil
	default:
		return observability.Scope{}, domainScopeNotFound()
	}
}

func domainScopeNotFound() error {
	// Unknown scope types and non-canonical global IDs are indistinguishable
	// from an inaccessible opaque resource.
	return store.ErrNotFound
}
