package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/middlewareprofiles"
	"github.com/kuberploy/kuberploy/internal/secrets"
	"github.com/kuberploy/kuberploy/internal/store"
)

type middlewareProfileBackend interface{ middlewareprofiles.Store }
type middlewareProfileMutation struct {
	Name         string                          `json:"name"`
	BaseRevision int64                           `json:"baseRevision,omitempty"`
	Spec         middlewareprofiles.Spec         `json:"spec"`
	Assignments  []middlewareprofiles.Assignment `json:"assignments"`
}
type middlewareProfileDeactivate struct {
	Revision int64 `json:"revision"`
}
type middlewareProfileClone struct {
	Name           string                          `json:"name"`
	SourceRevision int64                           `json:"sourceRevision"`
	Assignments    []middlewareprofiles.Assignment `json:"assignments"`
}
type assignedMiddlewareProfileView struct {
	ProfileID         string                  `json:"profileId"`
	Name              string                  `json:"name"`
	Revision          int64                   `json:"revision"`
	Spec              middlewareprofiles.Spec `json:"spec"`
	SpecDigest        string                  `json:"specDigest"`
	AssignmentsDigest string                  `json:"assignmentsDigest"`
}

func (s *Server) middlewareProfiles(w http.ResponseWriter, r *http.Request) {
	if s.middleware == nil {
		mappedMiddlewareRuntimeError(w, r, errMiddlewareRuntimeUnavailable)
		return
	}
	if r.Method == http.MethodGet {
		s.assignedMiddlewareProfiles(w, r)
		return
	}
	if !s.middlewareRuntimeReady(r.Context()) {
		mappedMiddlewareRuntimeError(w, r, errMiddlewareRuntimeUnavailable)
		return
	}
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	var input middlewareProfileMutation
	if !decode(w, r, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.BaseRevision != 0 || s.authorizeMiddlewareAssignments(r, input.Assignments) != nil {
		writeProblem(w, r, http.StatusForbidden, "MiddlewareProfileForbidden", "Middleware profile forbidden", "The actor cannot create a reusable profile in every requested scope.")
		return
	}
	if s.validateBasicAuthProfile(r, input.Spec, input.Assignments, "") != nil {
		mappedError(w, r, store.ErrNotFound)
		return
	}
	result, err := s.middleware.Create(r.Context(), middlewareprofiles.Command{ActorID: currentUser(r.Context()).ID, IdempotencyKey: key, RequestID: requestID(r.Context()), Now: time.Now().UTC()}, input.Name, input.Spec, input.Assignments)
	if err != nil {
		mappedMiddlewareProfileError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Location", "/v1/middlewares/"+result.Profile.ID)
	writeJSON(w, http.StatusCreated, middlewareprofiles.Entry{Profile: result.Profile, Revision: result.Revision})
}

func (s *Server) middlewareProfile(w http.ResponseWriter, r *http.Request) {
	if s.middleware == nil || !s.middlewareRuntimeReady(r.Context()) {
		mappedMiddlewareRuntimeError(w, r, errMiddlewareRuntimeUnavailable)
		return
	}
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	var input middlewareProfileMutation
	if !decode(w, r, &input) {
		return
	}
	if strings.TrimSpace(input.Name) != "" || input.BaseRevision < 1 {
		mappedMiddlewareProfileError(w, r, middlewareprofiles.ErrInvalid)
		return
	}
	_, current, loadErr := s.middleware.Current(r.Context(), r.PathValue("id"))
	if loadErr != nil || current.Revision != input.BaseRevision || s.authorizeMiddlewareAssignments(r, current.Assignments) != nil {
		mappedError(w, r, store.ErrNotFound)
		return
	}
	if s.authorizeMiddlewareAssignments(r, input.Assignments) != nil {
		writeProblem(w, r, http.StatusForbidden, "MiddlewareProfileForbidden", "Middleware profile forbidden", "The actor cannot revise this exact reusable profile scope.")
		return
	}
	if s.validateBasicAuthProfile(r, input.Spec, input.Assignments, "") != nil {
		mappedError(w, r, store.ErrNotFound)
		return
	}
	result, err := s.middleware.Revise(r.Context(), middlewareprofiles.Command{ActorID: currentUser(r.Context()).ID, IdempotencyKey: key, RequestID: requestID(r.Context()), Now: time.Now().UTC()}, middlewareprofiles.Ref{ProfileID: r.PathValue("id"), Revision: input.BaseRevision}, input.Spec, input.Assignments)
	if err != nil {
		mappedMiddlewareProfileError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeJSON(w, http.StatusOK, middlewareprofiles.Entry{Profile: result.Profile, Revision: result.Revision})
}

func (s *Server) cloneMiddlewareProfile(w http.ResponseWriter, r *http.Request) {
	if s.middleware == nil || !s.middlewareRuntimeReady(r.Context()) {
		mappedMiddlewareRuntimeError(w, r, errMiddlewareRuntimeUnavailable)
		return
	}
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	var input middlewareProfileClone
	if !decode(w, r, &input) {
		return
	}
	if input.SourceRevision < 1 {
		mappedMiddlewareProfileError(w, r, middlewareprofiles.ErrInvalid)
		return
	}
	_, source, loadErr := s.middleware.Revision(r.Context(), middlewareprofiles.Ref{ProfileID: r.PathValue("id"), Revision: input.SourceRevision})
	if loadErr != nil || s.authorizeMiddlewareAssignments(r, source.Assignments) != nil {
		mappedError(w, r, store.ErrNotFound)
		return
	}
	if s.authorizeMiddlewareAssignments(r, input.Assignments) != nil {
		writeProblem(w, r, http.StatusForbidden, "MiddlewareProfileForbidden", "Middleware profile forbidden", "The actor cannot clone into every requested scope.")
		return
	}
	if s.validateBasicAuthProfile(r, source.Spec, input.Assignments, "") != nil {
		mappedError(w, r, store.ErrNotFound)
		return
	}
	result, err := s.middleware.Clone(r.Context(), middlewareprofiles.Command{ActorID: currentUser(r.Context()).ID, IdempotencyKey: key, RequestID: requestID(r.Context()), Now: time.Now().UTC()}, middlewareprofiles.Ref{ProfileID: r.PathValue("id"), Revision: input.SourceRevision}, strings.TrimSpace(input.Name), input.Assignments)
	if err != nil {
		mappedMiddlewareProfileError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeJSON(w, http.StatusCreated, middlewareprofiles.Entry{Profile: result.Profile, Revision: result.Revision})
}

func (s *Server) deactivateMiddlewareProfile(w http.ResponseWriter, r *http.Request) {
	if s.middleware == nil || !s.middlewareRuntimeReady(r.Context()) {
		mappedMiddlewareRuntimeError(w, r, errMiddlewareRuntimeUnavailable)
		return
	}
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	var input middlewareProfileDeactivate
	if !decode(w, r, &input) {
		return
	}
	_, revision, err := s.middleware.Current(r.Context(), r.PathValue("id"))
	if err != nil {
		mappedMiddlewareProfileError(w, r, err)
		return
	}
	if input.Revision != revision.Revision || s.authorizeMiddlewareAssignments(r, revision.Assignments) != nil {
		writeProblem(w, r, http.StatusForbidden, "MiddlewareProfileForbidden", "Middleware profile forbidden", "The actor cannot deactivate this reusable profile.")
		return
	}
	result, err := s.middleware.Deactivate(r.Context(), middlewareprofiles.Command{ActorID: currentUser(r.Context()).ID, IdempotencyKey: key, RequestID: requestID(r.Context()), Now: time.Now().UTC()}, middlewareprofiles.Ref{ProfileID: r.PathValue("id"), Revision: input.Revision})
	if err != nil {
		mappedMiddlewareProfileError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeJSON(w, http.StatusOK, middlewareprofiles.Entry{Profile: result.Profile, Revision: result.Revision})
}

func (s *Server) assignedMiddlewareProfiles(w http.ResponseWriter, r *http.Request) {
	environments, applications := r.URL.Query()["environmentId"], r.URL.Query()["applicationId"]
	if len(environments) != 1 || len(applications) != 1 || !validUUID(environments[0]) || !validUUID(applications[0]) {
		mappedMiddlewareProfileError(w, r, middlewareprofiles.ErrInvalid)
		return
	}
	environmentID, applicationID := environments[0], applications[0]
	actor := currentUser(r.Context()).ID
	environment, err := s.store.GetEnvironmentForActor(r.Context(), actor, environmentID)
	if err != nil {
		mappedError(w, r, err)
		return
	}
	application, err := s.store.GetApplicationForActor(r.Context(), actor, applicationID)
	if err != nil || application.ProjectID != environment.ProjectID {
		mappedError(w, r, store.ErrNotFound)
		return
	}
	project, err := s.store.GetProjectForActor(r.Context(), actor, environment.ProjectID)
	if err != nil {
		mappedError(w, r, err)
		return
	}
	items, err := s.middleware.Assigned(r.Context(), middlewareprofiles.Target{ProjectID: project.ID, EnvironmentID: environment.ID, ApplicationID: application.ID}, 200)
	if err != nil {
		mappedMiddlewareProfileError(w, r, err)
		return
	}
	views := make([]assignedMiddlewareProfileView, 0, len(items))
	for _, item := range items {
		if s.validateBasicAuthProfile(r, item.Revision.Spec, item.Revision.Assignments, environment.ID) != nil {
			continue
		}
		views = append(views, assignedMiddlewareProfileView{ProfileID: item.Profile.ID, Name: item.Profile.Name, Revision: item.Revision.Revision, Spec: item.Revision.Spec, SpecDigest: item.Revision.SpecDigest, AssignmentsDigest: item.Revision.AssignmentsDigest})
	}
	collection(w, views)
}

// middlewareProfileCatalog returns full current revisions only when the actor
// can manage every current assignment. The exact target is required so a
// BasicAuth binding identity can never cross its application/environment.
func (s *Server) middlewareProfileCatalog(w http.ResponseWriter, r *http.Request) {
	if s.middleware == nil || !s.middlewareRuntimeReady(r.Context()) {
		mappedMiddlewareRuntimeError(w, r, errMiddlewareRuntimeUnavailable)
		return
	}
	environments, applications := r.URL.Query()["environmentId"], r.URL.Query()["applicationId"]
	if len(environments) != 1 || len(applications) != 1 || !validUUID(environments[0]) || !validUUID(applications[0]) {
		mappedMiddlewareProfileError(w, r, middlewareprofiles.ErrInvalid)
		return
	}
	environmentID, applicationID := environments[0], applications[0]
	actor := currentUser(r.Context()).ID
	environment, err := s.store.GetEnvironmentForActor(r.Context(), actor, environmentID)
	if err != nil {
		mappedError(w, r, err)
		return
	}
	application, err := s.store.GetApplicationForActor(r.Context(), actor, applicationID)
	if err != nil || application.ProjectID != environment.ProjectID {
		mappedError(w, r, store.ErrNotFound)
		return
	}
	entries, err := s.middleware.Catalog(r.Context(), 200)
	if err != nil {
		mappedMiddlewareProfileError(w, r, err)
		return
	}
	target := middlewareprofiles.Target{ProjectID: application.ProjectID, EnvironmentID: environment.ID, ApplicationID: application.ID}
	views := make([]middlewareprofiles.Entry, 0, len(entries))
	for _, entry := range entries {
		assigned := false
		for _, assignment := range entry.Revision.Assignments {
			if assignment.Scope == middlewareprofiles.ProjectScope && assignment.ID == target.ProjectID ||
				assignment.Scope == middlewareprofiles.EnvironmentScope && assignment.ID == target.EnvironmentID ||
				assignment.Scope == middlewareprofiles.ApplicationScope && assignment.ID == target.ApplicationID {
				assigned = true
				break
			}
		}
		if !assigned || s.authorizeMiddlewareAssignments(r, entry.Revision.Assignments) != nil ||
			s.validateBasicAuthProfile(r, entry.Revision.Spec, entry.Revision.Assignments, environment.ID) != nil {
			continue
		}
		views = append(views, entry)
	}
	collection(w, views)
}

func (s *Server) validateMiddlewareProfile(w http.ResponseWriter, r *http.Request) {
	if !s.middlewareRuntimeReady(r.Context()) {
		mappedMiddlewareRuntimeError(w, r, errMiddlewareRuntimeUnavailable)
		return
	}
	var input struct {
		Name string                  `json:"name"`
		Spec middlewareprofiles.Spec `json:"spec"`
	}
	if !decode(w, r, &input) {
		return
	}
	if middlewareprofiles.ValidateDefinition(strings.TrimSpace(input.Name), input.Spec) != nil {
		mappedMiddlewareProfileError(w, r, middlewareprofiles.ErrInvalid)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"valid": true})
}

func (s *Server) authorizeMiddlewareAssignments(r *http.Request, assignments []middlewareprofiles.Assignment) error {
	if len(assignments) == 0 {
		return middlewareprofiles.ErrInvalid
	}
	actor := currentUser(r.Context()).ID
	for _, assignment := range assignments {
		var permission domain.Permission
		var target domain.AccessTarget
		switch assignment.Scope {
		case middlewareprofiles.ProjectScope:
			permission = domain.PermissionGrantsManage
			target = domain.AccessTarget{Type: "project", ID: assignment.ID}
		case middlewareprofiles.EnvironmentScope:
			permission = domain.PermissionGrantsManage
			target = domain.AccessTarget{Type: "environment", ID: assignment.ID}
		case middlewareprofiles.ApplicationScope:
			permission = domain.PermissionConfigWrite
			target = domain.AccessTarget{Type: "application", ID: assignment.ID}
		default:
			return middlewareprofiles.ErrInvalid
		}
		if err := s.store.Authorize(r.Context(), actor, permission, target); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) validateBasicAuthProfile(r *http.Request, spec middlewareprofiles.Spec, assignments []middlewareprofiles.Assignment, environmentID string) error {
	refs := middlewareprofiles.SecretReferences(spec)
	if len(refs) == 0 {
		return nil
	}
	if len(refs) != 1 || s.runtimeSecrets == nil {
		return middlewareprofiles.ErrInvalid
	}
	ref := refs[0]
	binding, err := s.runtimeSecrets.Binding(r.Context(), ref.BindingID)
	if err != nil || binding.Validate() != nil || binding.Name != ref.Name || binding.ActiveVersion != ref.Version || binding.State != secrets.BindingReady || binding.Purpose != secrets.PurposeRuntimeSecret {
		return middlewareprofiles.ErrNotFound
	}
	if environmentID != "" && binding.Scope.EnvironmentID != environmentID {
		return middlewareprofiles.ErrNotFound
	}
	for _, assignment := range assignments {
		if assignment.Scope != middlewareprofiles.ApplicationScope || assignment.ID != binding.Scope.ApplicationID {
			return middlewareprofiles.ErrUnassigned
		}
	}
	actor := currentUser(r.Context()).ID
	application, err := s.store.GetApplicationForActor(r.Context(), actor, binding.Scope.ApplicationID)
	if err != nil {
		return middlewareprofiles.ErrNotFound
	}
	environment, err := s.store.GetEnvironmentForActor(r.Context(), actor, binding.Scope.EnvironmentID)
	if err != nil || environment.ProjectID != application.ProjectID || environment.Namespace != binding.Scope.Namespace {
		return middlewareprofiles.ErrNotFound
	}
	project, err := s.store.GetProjectForActor(r.Context(), actor, application.ProjectID)
	if err != nil || project.ID != binding.Scope.ProjectID || project.TeamID != binding.Scope.OrganizationID {
		return middlewareprofiles.ErrNotFound
	}
	target := domain.AccessTarget{Type: "secret-binding", ID: binding.ID, TeamID: project.TeamID, ProjectID: project.ID, ApplicationID: application.ID, EnvironmentID: environment.ID, Namespace: environment.Namespace}
	return s.store.Authorize(r.Context(), actor, domain.PermissionSecretsBind, target)
}

func mappedMiddlewareProfileError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, middlewareprofiles.ErrInvalid):
		writeProblem(w, r, http.StatusUnprocessableEntity, "MiddlewareProfileInvalid", "Middleware profile invalid", "The profile, scope assignments, or closed Traefik HTTP middleware specification is invalid.")
	case errors.Is(err, middlewareprofiles.ErrNotFound), errors.Is(err, store.ErrNotFound):
		mappedError(w, r, store.ErrNotFound)
	case errors.Is(err, middlewareprofiles.ErrConflict):
		writeProblem(w, r, http.StatusConflict, "MiddlewareProfileConflict", "Middleware profile conflict", "The exact revision, digests, or idempotency input no longer matches.")
	case errors.Is(err, middlewareprofiles.ErrInactive):
		writeProblem(w, r, http.StatusConflict, "MiddlewareProfileInactive", "Middleware profile inactive", "The middleware profile is deactivated.")
	case errors.Is(err, middlewareprofiles.ErrReferenced):
		writeProblem(w, r, http.StatusConflict, "MiddlewareProfileReferenced", "Middleware profile referenced", "Detach this profile from every current AppConfig before deactivation.")
	case errors.Is(err, middlewareprofiles.ErrUnassigned):
		writeProblem(w, r, http.StatusUnprocessableEntity, "MiddlewareProfileUnassigned", "Middleware profile unassigned", "The profile is not assigned to this workload target.")
	default:
		mappedMiddlewareRuntimeError(w, r, errMiddlewareRuntimeUnavailable)
	}
}
