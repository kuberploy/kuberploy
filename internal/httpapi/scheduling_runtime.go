package httpapi

import (
	"context"
	"net/http"

	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/domain"
)

// runtimeSchedulingError carries the closed, application-scoped placement
// diagnostics produced by the domain validator. Scheduling is configured on
// each workload; there is no server-side profile materialization step.
type runtimeSchedulingError struct {
	problems []domain.WorkloadValidationError
}

func (e *runtimeSchedulingError) Error() string { return "invalid workload scheduling" }

func (s *Server) resolveSchedulingRuntime(_ context.Context, _ string, deployment domain.Deployment, runtime domain.WorkloadRuntime, _ bool) (domain.WorkloadRuntime, error) {
	runtime = domain.NormalizeWorkloadRuntime(runtime)
	problems := domain.ValidateWorkloadRuntime(runtime)
	problems = append(problems, domain.ValidateApplicationScheduling(runtime, deployment.ApplicationID)...)
	if len(problems) != 0 {
		return domain.WorkloadRuntime{}, &runtimeSchedulingError{problems: problems}
	}
	return runtime, nil
}

// AppConfig parsing already validates the direct scheduling document against
// its embedded application identity. Keep this hook as a no-op so preview,
// save, and auto-deploy continue sharing one pipeline without rewriting the
// caller's placement intent.
func (s *Server) materializeSchedulingCandidate(_ context.Context, _ string, _ domain.Deployment, _ []byte, candidate appconfig.Candidate) appconfig.Candidate {
	return candidate
}

func mappedSchedulingRuntimeError(w http.ResponseWriter, r *http.Request, err error) {
	typed, ok := err.(*runtimeSchedulingError)
	if !ok || len(typed.problems) == 0 {
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "The workload scheduling configuration is invalid.")
		return
	}
	limit := len(typed.problems)
	if limit > 50 {
		limit = 50
	}
	fields := make([]FieldError, 0, limit)
	for _, problem := range typed.problems[:limit] {
		fields = append(fields, FieldError{Pointer: problem.Pointer, Code: problem.Code, Detail: problem.Detail})
	}
	writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "The workload scheduling configuration is invalid.", fields...)
}
