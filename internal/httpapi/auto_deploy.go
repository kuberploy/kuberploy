package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/autodeploy"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/store"
)

type createAutoDeployPolicyRequest struct {
	EnvironmentID        string `json:"environmentId,omitempty"`
	TemplateDeploymentID string `json:"templateDeploymentId"`
	ServiceActorID       string `json:"serviceActorId"`
	Enabled              bool   `json:"enabled"`
}

type reviseAutoDeployPolicyRequest struct {
	TemplateDeploymentID string `json:"templateDeploymentId"`
	ServiceActorID       string `json:"serviceActorId"`
	Enabled              bool   `json:"enabled"`
	ExpectedRevision     int64  `json:"expectedRevision"`
}

type autoDeployRevisionView struct {
	Revision                   int64     `json:"revision"`
	Enabled                    bool      `json:"enabled"`
	SourceDeploymentID         string    `json:"sourceDeploymentId"`
	SourceDeploymentGeneration int64     `json:"sourceDeploymentGeneration"`
	SourceConfigETag           string    `json:"sourceConfigETag"`
	TemplateDigest             string    `json:"templateDigest"`
	ServiceActorID             string    `json:"serviceActorId"`
	CreatedBy                  string    `json:"createdBy"`
	CreatedAt                  time.Time `json:"createdAt"`
}

type autoDeployPolicyView struct {
	ID              string                 `json:"id"`
	ProjectID       string                 `json:"projectId"`
	ApplicationID   string                 `json:"applicationId"`
	EnvironmentID   string                 `json:"environmentId"`
	CurrentRevision int64                  `json:"currentRevision"`
	Current         autoDeployRevisionView `json:"current"`
	CreatedBy       string                 `json:"createdBy"`
	CreatedAt       time.Time              `json:"createdAt"`
	UpdateSemantics string                 `json:"updateSemantics"`
}

type autoDeployRunView struct {
	AttemptID      string              `json:"attemptId"`
	PolicyRevision int64               `json:"policyRevision"`
	ReleaseID      string              `json:"releaseId"`
	State          autodeploy.RunState `json:"state"`
	Attempts       int                 `json:"attempts"`
	OperationID    string              `json:"operationId,omitempty"`
	DeploymentID   string              `json:"deploymentId,omitempty"`
	FailureCode    string              `json:"failureCode,omitempty"`
	AvailableAt    time.Time           `json:"availableAt"`
	CreatedAt      time.Time           `json:"createdAt"`
	UpdatedAt      time.Time           `json:"updatedAt"`
	CompletedAt    *time.Time          `json:"completedAt,omitempty"`
}

const autoDeployUpdateSemantics = "The revision pins this exact deployment generation and AppConfig ETag. Configuration drift pauses image automation until you save a new revision."

func (s *Server) applicationAutoDeployPolicies(w http.ResponseWriter, r *http.Request) {
	if s.autoDeployPolicies == nil || s.autoDeployService == nil {
		autoDeployUnavailable(w, r, "Auto-deploy policy management is not configured.")
		return
	}
	application, err := s.store.GetApplication(r.Context(), strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		mappedError(w, r, err)
		return
	}
	actor := currentUser(r.Context())
	if err = s.store.Authorize(r.Context(), actor.ID, domain.PermissionBuildsRead, domain.AccessTarget{Type: "application", ID: application.ID}); err != nil {
		mappedError(w, r, err)
		return
	}
	if r.Method == http.MethodGet {
		items, listErr := s.autoDeployPolicies.PoliciesForApplication(r.Context(), actor.ID, application.ID)
		if listErr != nil {
			mappedAutoDeployError(w, r, listErr)
			return
		}
		views := make([]autoDeployPolicyView, 0, len(items))
		for _, item := range items {
			views = append(views, safeAutoDeployPolicy(item))
		}
		collection(w, views)
		return
	}
	key, ok := githubBuildIdempotencyKey(w, r)
	if !ok {
		return
	}
	var input createAutoDeployPolicyRequest
	if !decodeGitHubBuildJSON(w, r, &input) {
		return
	}
	input.EnvironmentID = strings.TrimSpace(input.EnvironmentID)
	input.TemplateDeploymentID = strings.TrimSpace(input.TemplateDeploymentID)
	input.ServiceActorID = strings.TrimSpace(input.ServiceActorID)
	requestDigest := "sha256:" + fingerprint(struct {
		ApplicationID string                        `json:"applicationId"`
		Input         createAutoDeployPolicyRequest `json:"input"`
	}{application.ID, input})
	if policy, revision, replay, replayErr := s.autoDeployService.CommandReplay(r.Context(), actor.ID, key, "create", requestDigest, application.ID, ""); replayErr != nil {
		mappedAutoDeployError(w, r, replayErr)
		return
	} else if replay {
		w.Header().Set("Idempotent-Replay", "true")
		w.Header().Set("Location", "/v1/auto-deploy-policies/"+policy.ID)
		writeJSON(w, http.StatusCreated, safeAutoDeployPolicy(autodeploy.PolicyStatus{Policy: policy, CurrentRevision: revision}))
		return
	}
	if !s.autoDeployRuntimeReady(r) {
		autoDeployUnavailable(w, r, "No fresh controller with the exact auto-deploy runtime configuration is ready.")
		return
	}
	policy, revision, replay, err := s.autoDeployService.Create(r.Context(), actor.ID, autodeploy.CreatePolicyInput{
		ExpectedApplicationID: application.ID, EnvironmentID: input.EnvironmentID, TemplateDeploymentID: input.TemplateDeploymentID,
		ServiceActorID: input.ServiceActorID, Enabled: input.Enabled, IdempotencyKey: key, RequestDigest: requestDigest, RequestID: requestID(r.Context()),
	})
	if err != nil {
		mappedAutoDeployError(w, r, err)
		return
	}
	if policy.ApplicationID != application.ID || policy.ProjectID != application.ProjectID {
		mappedError(w, r, store.ErrNotFound)
		return
	}
	if replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Location", "/v1/auto-deploy-policies/"+policy.ID)
	writeJSON(w, http.StatusCreated, safeAutoDeployPolicy(autodeploy.PolicyStatus{Policy: policy, CurrentRevision: revision}))
}

func (s *Server) autoDeployPolicy(w http.ResponseWriter, r *http.Request) {
	if s.autoDeployPolicies == nil || s.autoDeployService == nil {
		autoDeployUnavailable(w, r, "Auto-deploy policy management is not configured.")
		return
	}
	actor := currentUser(r.Context())
	policyID := strings.TrimSpace(r.PathValue("id"))
	if r.Method == http.MethodGet {
		status, err := s.autoDeployPolicies.PolicyForActor(r.Context(), actor.ID, policyID)
		if err != nil {
			mappedAutoDeployError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, safeAutoDeployPolicy(status))
		return
	}
	key, ok := githubBuildIdempotencyKey(w, r)
	if !ok {
		return
	}
	var input reviseAutoDeployPolicyRequest
	if !decodeGitHubBuildJSON(w, r, &input) {
		return
	}
	input.TemplateDeploymentID = strings.TrimSpace(input.TemplateDeploymentID)
	input.ServiceActorID = strings.TrimSpace(input.ServiceActorID)
	requestDigest := "sha256:" + fingerprint(struct {
		PolicyID string                        `json:"policyId"`
		Input    reviseAutoDeployPolicyRequest `json:"input"`
	}{policyID, input})
	if policy, revision, replay, replayErr := s.autoDeployService.CommandReplay(r.Context(), actor.ID, key, "revise", requestDigest, "", policyID); replayErr != nil {
		mappedAutoDeployError(w, r, replayErr)
		return
	} else if replay {
		w.Header().Set("Idempotent-Replay", "true")
		writeJSON(w, http.StatusOK, safeAutoDeployPolicy(autodeploy.PolicyStatus{Policy: policy, CurrentRevision: revision}))
		return
	}
	status, err := s.autoDeployPolicies.PolicyForActor(r.Context(), actor.ID, policyID)
	if err != nil {
		mappedAutoDeployError(w, r, err)
		return
	}
	if input.ExpectedRevision != status.Policy.CurrentRevision {
		writeProblem(w, r, http.StatusConflict, "AutoDeployRevisionConflict", "Auto-deploy policy changed", "Refresh the policy before saving a new immutable revision.")
		return
	}
	if !s.autoDeployRuntimeReady(r) {
		autoDeployUnavailable(w, r, "No fresh controller with the exact auto-deploy runtime configuration is ready.")
		return
	}
	policy, revision, replay, err := s.autoDeployService.Revise(r.Context(), actor.ID, autodeploy.RevisePolicyInput{
		Policy: status.Policy, CurrentRevision: status.CurrentRevision, TemplateDeploymentID: input.TemplateDeploymentID, ServiceActorID: input.ServiceActorID,
		Enabled: input.Enabled, IdempotencyKey: key, RequestDigest: requestDigest, RequestID: requestID(r.Context()),
	})
	if err != nil {
		mappedAutoDeployError(w, r, err)
		return
	}
	if replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeJSON(w, http.StatusOK, safeAutoDeployPolicy(autodeploy.PolicyStatus{Policy: policy, CurrentRevision: revision}))
}

func (s *Server) autoDeployPolicyRevisions(w http.ResponseWriter, r *http.Request) {
	if s.autoDeployPolicies == nil {
		autoDeployUnavailable(w, r, "Auto-deploy policy history is not configured.")
		return
	}
	limit, ok := autoDeployLimit(w, r)
	if !ok {
		return
	}
	items, err := s.autoDeployPolicies.PolicyRevisionsForActor(r.Context(), currentUser(r.Context()).ID, strings.TrimSpace(r.PathValue("id")), limit)
	if err != nil {
		mappedAutoDeployError(w, r, err)
		return
	}
	views := make([]autoDeployRevisionView, 0, len(items))
	for _, revision := range items {
		views = append(views, safeAutoDeployRevision(revision))
	}
	collection(w, views)
}

func (s *Server) autoDeployPolicyRuns(w http.ResponseWriter, r *http.Request) {
	if s.autoDeployPolicies == nil {
		autoDeployUnavailable(w, r, "Auto-deploy run history is not configured.")
		return
	}
	limit, ok := autoDeployLimit(w, r)
	if !ok {
		return
	}
	items, err := s.autoDeployPolicies.PolicyRunsForActor(r.Context(), currentUser(r.Context()).ID, strings.TrimSpace(r.PathValue("id")), limit)
	if err != nil {
		mappedAutoDeployError(w, r, err)
		return
	}
	views := make([]autoDeployRunView, 0, len(items))
	for _, run := range items {
		views = append(views, autoDeployRunView{AttemptID: run.AttemptID, PolicyRevision: run.PolicyRevision, ReleaseID: run.ReleaseID,
			State: run.State, Attempts: run.Attempts, OperationID: run.OperationID, DeploymentID: run.DeploymentID, FailureCode: run.FailureCode,
			AvailableAt: run.AvailableAt, CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt, CompletedAt: run.CompletedAt})
	}
	collection(w, views)
}

func (s *Server) autoDeployRuntimeReady(r *http.Request) bool {
	if s.autoDeployReadiness == nil {
		return false
	}
	ctx, cancel := contextWithProbeTimeout(r)
	defer cancel()
	return s.autoDeployReadiness.Probe(ctx) == nil
}

func contextWithProbeTimeout(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 2*time.Second)
}

func autoDeployLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	limit := 25
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "limit must be between 1 and 100.")
			return 0, false
		}
		limit = parsed
	}
	return limit, true
}

func safeAutoDeployPolicy(status autodeploy.PolicyStatus) autoDeployPolicyView {
	policy := status.Policy
	return autoDeployPolicyView{ID: policy.ID, ProjectID: policy.ProjectID,
		ApplicationID: policy.ApplicationID, EnvironmentID: policy.EnvironmentID, CurrentRevision: policy.CurrentRevision,
		Current: safeAutoDeployRevision(status.CurrentRevision), CreatedBy: policy.CreatedBy, CreatedAt: policy.CreatedAt,
		UpdateSemantics: autoDeployUpdateSemantics}
}

func safeAutoDeployRevision(revision autodeploy.Revision) autoDeployRevisionView {
	return autoDeployRevisionView{Revision: revision.Revision, Enabled: revision.Enabled,
		SourceDeploymentID: revision.Template.SourceDeploymentID, SourceDeploymentGeneration: revision.Template.SourceDeploymentGeneration,
		SourceConfigETag: revision.Template.SourceConfigETag, TemplateDigest: revision.TemplateDigest,
		ServiceActorID: revision.ServiceActorID, CreatedBy: revision.CreatedBy, CreatedAt: revision.CreatedAt}
}

func mappedAutoDeployError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, autodeploy.ErrInvalid):
		writeProblem(w, r, http.StatusUnprocessableEntity, "AutoDeployInvalid", "Auto-deploy policy is invalid", "Connect the App source, then select an environment, deployment snapshot, and enabled same-project service account.")
	case errors.Is(err, autodeploy.ErrConflict):
		writeProblem(w, r, http.StatusConflict, "AutoDeployConflict", "Auto-deploy policy conflict", "The pinned resource relationship or immutable revision changed; refresh and create a new revision.")
	case errors.Is(err, autodeploy.ErrUnauthorized):
		mappedError(w, r, store.ErrForbidden)
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrForbidden), errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrIdempotencyConflict):
		mappedError(w, r, err)
	default:
		autoDeployUnavailable(w, r, "The auto-deploy policy service is temporarily unavailable.")
	}
}

func autoDeployUnavailable(w http.ResponseWriter, r *http.Request, detail string) {
	writeProblem(w, r, http.StatusServiceUnavailable, "AutoDeployUnavailable", "Auto-deploy unavailable", detail)
}
