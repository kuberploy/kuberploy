package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/middlewareprofiles"
)

func (s *Server) middlewareRuntimeReady(ctx context.Context) bool {
	if s.middleware == nil || !s.edgeFeatures.Traefik || s.edgeReadiness == nil || s.gitProjection == nil || s.gitReadiness == nil || s.argoReadiness == nil {
		return false
	}
	probeContext, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return s.edgeReadiness.Probe(probeContext) == nil && s.gitReadiness.Probe(probeContext) == nil && s.argoReadiness.Probe(probeContext) == nil
}

func (s *Server) materializeMiddlewareCandidate(ctx context.Context, actor string, deployment domain.Deployment, current []byte, candidate appconfig.Candidate) appconfig.Candidate {
	if len(candidate.Diagnostics) != 0 || candidate.Parsed == nil {
		return candidate
	}
	spec, ok := candidate.Parsed["spec"].(map[string]any)
	if !ok {
		return middlewareCandidateDiagnostic(candidate, middlewareprofiles.ErrInvalid)
	}
	rawDefinitions, present := spec["middlewares"]
	if !present {
		return candidate
	}
	values, ok := rawDefinitions.([]any)
	if !ok {
		return middlewareCandidateDiagnostic(candidate, middlewareprofiles.ErrInvalid)
	}
	definitions := make([]middlewareprofiles.MaterializedDefinition, 0, len(values))
	hasReusable := false
	for _, raw := range values {
		value, ok := raw.(map[string]any)
		if !ok {
			return middlewareCandidateDiagnostic(candidate, middlewareprofiles.ErrInvalid)
		}
		name, nameOK := value["name"].(string)
		middlewareSpec, specOK := value["spec"].(map[string]any)
		if !nameOK || !specOK {
			return middlewareCandidateDiagnostic(candidate, middlewareprofiles.ErrInvalid)
		}
		definition := middlewareprofiles.MaterializedDefinition{Name: name, Spec: middlewareprofiles.Spec(middlewareSpec)}
		if rawRef, hasRef := value["profileRef"]; hasRef {
			hasReusable = true
			encoded, _ := json.Marshal(rawRef)
			var ref middlewareprofiles.Ref
			if json.Unmarshal(encoded, &ref) != nil {
				return middlewareCandidateDiagnostic(candidate, middlewareprofiles.ErrInvalid)
			}
			definition.ProfileRef = &ref
		}
		definitions = append(definitions, definition)
	}
	if !hasReusable {
		return candidate
	}
	if !s.middlewareRuntimeReady(ctx) {
		return middlewareCandidateDiagnostic(candidate, errMiddlewareRuntimeUnavailable)
	}
	environment, err := s.store.GetEnvironmentForActor(ctx, actor, deployment.EnvironmentID)
	if err != nil {
		return middlewareCandidateDiagnostic(candidate, err)
	}
	project, err := s.store.GetProjectForActor(ctx, actor, environment.ProjectID)
	if err != nil {
		return middlewareCandidateDiagnostic(candidate, err)
	}
	resolver, err := middlewareprofiles.NewResolver(s.middleware)
	if err != nil {
		return middlewareCandidateDiagnostic(candidate, errMiddlewareRuntimeUnavailable)
	}
	target := middlewareprofiles.Target{ProjectID: project.ID, EnvironmentID: environment.ID, ApplicationID: deployment.ApplicationID}
	for index, definition := range definitions {
		if definition.ProfileRef == nil {
			continue
		}
		resolved, resolveErr := resolver.Resolve(ctx, *definition.ProfileRef, target)
		if resolveErr != nil {
			return middlewareCandidateDiagnostic(candidate, resolveErr)
		}
		left, _ := json.Marshal(definition.Spec)
		right, _ := json.Marshal(resolved.Spec)
		if !bytes.Equal(left, right) {
			return middlewareCandidateDiagnostic(candidate, middlewareprofiles.ErrConflict)
		}
		definitions[index] = middlewareprofiles.MaterializedDefinition{Name: definition.Name, ProfileRef: &resolved.Ref, Spec: resolved.Spec}
	}
	return appconfig.WithResolvedMiddlewares(current, candidate, definitions)
}

var errMiddlewareRuntimeUnavailable = errors.New("middleware runtime unavailable")

func middlewareCandidateDiagnostic(candidate appconfig.Candidate, err error) appconfig.Candidate {
	diagnostic := appconfig.Diagnostic{Code: "MiddlewareProfileUnavailable", Detail: "The reusable middleware profile runtime could not be resolved safely.", Pointer: "/spec/middlewares"}
	switch {
	case errors.Is(err, middlewareprofiles.ErrInvalid):
		diagnostic.Code, diagnostic.Detail = "MiddlewareProfileInvalid", "A middleware profile reference or materialized specification is invalid."
	case errors.Is(err, middlewareprofiles.ErrNotFound):
		diagnostic.Code, diagnostic.Detail = "MiddlewareProfileNotFound", "The exact middleware profile revision does not exist."
	case errors.Is(err, middlewareprofiles.ErrInactive):
		diagnostic.Code, diagnostic.Detail = "MiddlewareProfileInactive", "The middleware profile has been deactivated."
	case errors.Is(err, middlewareprofiles.ErrUnassigned):
		diagnostic.Code, diagnostic.Detail = "MiddlewareProfileUnassigned", "The middleware profile is not assigned to this exact project, environment, or application."
	case errors.Is(err, middlewareprofiles.ErrConflict):
		diagnostic.Code, diagnostic.Detail = "MiddlewareProfileConflict", "The profile revision, immutable digests, or materialized specification is stale."
	}
	candidate.Diagnostics = append(candidate.Diagnostics, diagnostic)
	return candidate
}

func mappedMiddlewareRuntimeError(w http.ResponseWriter, r *http.Request, err error) {
	candidate := middlewareCandidateDiagnostic(appconfig.Candidate{}, err)
	diagnostic := candidate.Diagnostics[0]
	status := http.StatusUnprocessableEntity
	if diagnostic.Code == "MiddlewareProfileUnavailable" {
		status = http.StatusServiceUnavailable
	}
	if diagnostic.Code == "MiddlewareProfileConflict" || diagnostic.Code == "MiddlewareProfileInactive" {
		status = http.StatusConflict
	}
	writeProblem(w, r, status, diagnostic.Code, "Middleware profile rejected", diagnostic.Detail, FieldError{Pointer: diagnostic.Pointer, Code: diagnostic.Code, Detail: diagnostic.Detail})
}
