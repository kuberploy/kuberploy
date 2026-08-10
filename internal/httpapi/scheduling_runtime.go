package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/scheduling"
)

func (s *Server) resolveSchedulingRuntime(ctx context.Context, actor string, deployment domain.Deployment, runtime domain.WorkloadRuntime, rejectEffectiveInput bool) (domain.WorkloadRuntime, error) {
	if runtime.SchedulingProfile == nil {
		if scheduling.HasEffectiveMaterial(runtime) {
			return domain.WorkloadRuntime{}, scheduling.ErrInvalid
		}
		return runtime, nil
	}
	if rejectEffectiveInput && scheduling.HasEffectiveMaterial(runtime) {
		return domain.WorkloadRuntime{}, scheduling.ErrInvalid
	}
	if s.scheduling == nil {
		return domain.WorkloadRuntime{}, errSchedulingUnavailable
	}
	environment, err := s.store.GetEnvironmentForActor(ctx, actor, deployment.EnvironmentID)
	if err != nil {
		return domain.WorkloadRuntime{}, err
	}
	project, err := s.store.GetProjectForActor(ctx, actor, environment.ProjectID)
	if err != nil {
		return domain.WorkloadRuntime{}, err
	}
	resolver, err := scheduling.NewResolver(s.scheduling)
	if err != nil {
		return domain.WorkloadRuntime{}, errSchedulingUnavailable
	}
	ref := runtime.SchedulingProfile
	resolved, err := resolver.ResolveExact(ctx, scheduling.Ref{ProfileID: ref.ProfileID, Revision: ref.Revision}, scheduling.Target{TeamID: project.TeamID, ProjectID: project.ID, EnvironmentID: environment.ID})
	if err != nil {
		return domain.WorkloadRuntime{}, err
	}
	if ref.SpecDigest != resolved.SpecDigest || ref.AssignmentsDigest != resolved.AssignmentsDigest {
		return domain.WorkloadRuntime{}, scheduling.ErrConflict
	}
	return scheduling.Materialize(runtime, resolved, deployment.ApplicationID)
}

func (s *Server) materializeSchedulingCandidate(ctx context.Context, actor string, deployment domain.Deployment, current []byte, candidate appconfig.Candidate) appconfig.Candidate {
	if len(candidate.Diagnostics) != 0 {
		return candidate
	}
	runtime, err := s.resolveSchedulingRuntime(ctx, actor, deployment, candidate.Runtime, false)
	if err != nil {
		candidate.Diagnostics = append(candidate.Diagnostics, schedulingDiagnostic(err))
		return candidate
	}
	if candidate.Runtime.SchedulingProfile == nil {
		return candidate
	}
	return appconfig.WithResolvedScheduling(current, candidate, runtime)
}

var errSchedulingUnavailable = errors.New("scheduling profile runtime unavailable")

func schedulingDiagnostic(err error) appconfig.Diagnostic {
	switch {
	case errors.Is(err, scheduling.ErrInvalid):
		return appconfig.Diagnostic{Code: "SchedulingProfileInvalid", Detail: "Submit only an exact assigned scheduling profile reference; effective Pod placement fields are server-owned.", Pointer: "/spec/runtime/schedulingProfile"}
	case errors.Is(err, scheduling.ErrNotFound):
		return appconfig.Diagnostic{Code: "SchedulingProfileNotFound", Detail: "The exact scheduling profile revision does not exist.", Pointer: "/spec/runtime/schedulingProfile"}
	case errors.Is(err, scheduling.ErrInactive):
		return appconfig.Diagnostic{Code: "SchedulingProfileInactive", Detail: "The scheduling profile has been deactivated.", Pointer: "/spec/runtime/schedulingProfile"}
	case errors.Is(err, scheduling.ErrUnassigned):
		return appconfig.Diagnostic{Code: "SchedulingProfileUnassigned", Detail: "The scheduling profile is not assigned to this exact team, project, or environment.", Pointer: "/spec/runtime/schedulingProfile"}
	case errors.Is(err, scheduling.ErrConflict):
		return appconfig.Diagnostic{Code: "SchedulingProfileConflict", Detail: "The profile revision or immutable digests are stale. Reload the assigned profile catalog.", Pointer: "/spec/runtime/schedulingProfile"}
	default:
		return appconfig.Diagnostic{Code: "SchedulingProfileUnavailable", Detail: "The scheduling profile could not be resolved safely.", Pointer: "/spec/runtime/schedulingProfile"}
	}
}

func mappedSchedulingRuntimeError(w http.ResponseWriter, r *http.Request, err error) {
	diagnostic := schedulingDiagnostic(err)
	if diagnostic.Code == "SchedulingProfileUnavailable" {
		writeProblem(w, r, http.StatusServiceUnavailable, diagnostic.Code, "Scheduling profile unavailable", diagnostic.Detail)
		return
	}
	status := http.StatusUnprocessableEntity
	if diagnostic.Code == "SchedulingProfileConflict" || diagnostic.Code == "SchedulingProfileInactive" {
		status = http.StatusConflict
	}
	writeProblem(w, r, status, diagnostic.Code, "Scheduling profile rejected", diagnostic.Detail, FieldError{Pointer: diagnostic.Pointer, Code: diagnostic.Code, Detail: diagnostic.Detail})
}
