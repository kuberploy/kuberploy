package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/scheduling"
	"github.com/kuberploy/kuberploy/internal/store"
)

type schedulingProfileBackend interface {
	scheduling.Store
}

type schedulingProfileMutation struct {
	Name         string                  `json:"name"`
	BaseRevision int64                   `json:"baseRevision,omitempty"`
	Spec         scheduling.Spec         `json:"spec"`
	Assignments  []scheduling.Assignment `json:"assignments"`
}

type schedulingProfileDeactivate struct {
	Revision int64 `json:"revision"`
}

type assignedSchedulingProfileView struct {
	ProfileID         string          `json:"profileId"`
	Name              string          `json:"name"`
	Revision          int64           `json:"revision"`
	Spec              scheduling.Spec `json:"spec"`
	SpecDigest        string          `json:"specDigest"`
	AssignmentsDigest string          `json:"assignmentsDigest"`
}

func (s *Server) platformSchedulingProfiles(w http.ResponseWriter, r *http.Request) {
	if s.scheduling == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "SchedulingProfilesUnavailable", "Scheduling profiles unavailable", "The scheduling profile store is not configured.")
		return
	}
	if r.Method == http.MethodGet {
		items, err := s.scheduling.Catalog(r.Context(), 200)
		if err != nil {
			mappedSchedulingError(w, r, err)
			return
		}
		collection(w, items)
		return
	}
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	var input schedulingProfileMutation
	if !decode(w, r, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.BaseRevision != 0 {
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "baseRevision is accepted only when revising an existing profile.", FieldError{Pointer: "/baseRevision", Code: "Forbidden", Detail: "omit baseRevision when creating a profile"})
		return
	}
	result, err := s.scheduling.Create(r.Context(), scheduling.Command{ActorID: currentUser(r.Context()).ID, IdempotencyKey: key, RequestID: requestID(r.Context()), Now: time.Now().UTC()}, input.Name, input.Spec, input.Assignments)
	if err != nil {
		mappedSchedulingError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Location", "/v1/platform/scheduling-profiles/"+result.Profile.ID)
	writeJSON(w, http.StatusCreated, scheduling.Entry{Profile: result.Profile, Revision: result.Revision})
}

func (s *Server) platformSchedulingProfile(w http.ResponseWriter, r *http.Request) {
	if s.scheduling == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "SchedulingProfilesUnavailable", "Scheduling profiles unavailable", "The scheduling profile store is not configured.")
		return
	}
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	var input schedulingProfileMutation
	if !decode(w, r, &input) {
		return
	}
	if strings.TrimSpace(input.Name) != "" || input.BaseRevision < 1 {
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "Revise the exact current profile revision without changing its immutable name.")
		return
	}
	result, err := s.scheduling.Revise(r.Context(), scheduling.Command{ActorID: currentUser(r.Context()).ID, IdempotencyKey: key, RequestID: requestID(r.Context()), Now: time.Now().UTC()}, scheduling.Ref{ProfileID: r.PathValue("id"), Revision: input.BaseRevision}, input.Spec, input.Assignments)
	if err != nil {
		mappedSchedulingError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeJSON(w, http.StatusOK, scheduling.Entry{Profile: result.Profile, Revision: result.Revision})
}

func (s *Server) deactivatePlatformSchedulingProfile(w http.ResponseWriter, r *http.Request) {
	if s.scheduling == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "SchedulingProfilesUnavailable", "Scheduling profiles unavailable", "The scheduling profile store is not configured.")
		return
	}
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	var input schedulingProfileDeactivate
	if !decode(w, r, &input) {
		return
	}
	result, err := s.scheduling.Deactivate(r.Context(), scheduling.Command{ActorID: currentUser(r.Context()).ID, IdempotencyKey: key, RequestID: requestID(r.Context()), Now: time.Now().UTC()}, scheduling.Ref{ProfileID: r.PathValue("id"), Revision: input.Revision})
	if err != nil {
		mappedSchedulingError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeJSON(w, http.StatusOK, scheduling.Entry{Profile: result.Profile, Revision: result.Revision})
}

func (s *Server) assignedSchedulingProfiles(w http.ResponseWriter, r *http.Request) {
	if s.scheduling == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "SchedulingProfilesUnavailable", "Scheduling profiles unavailable", "The scheduling profile store is not configured.")
		return
	}
	actor := currentUser(r.Context()).ID
	environment, err := s.store.GetEnvironmentForActor(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		mappedError(w, r, err)
		return
	}
	project, err := s.store.GetProjectForActor(r.Context(), actor, environment.ProjectID)
	if err != nil {
		mappedError(w, r, err)
		return
	}
	items, err := s.scheduling.Assigned(r.Context(), scheduling.Target{TeamID: project.TeamID, ProjectID: project.ID, EnvironmentID: environment.ID}, 100)
	if err != nil {
		mappedSchedulingError(w, r, err)
		return
	}
	views := make([]assignedSchedulingProfileView, 0, len(items))
	for _, item := range items {
		views = append(views, assignedSchedulingProfileView{ProfileID: item.Profile.ID, Name: item.Profile.Name, Revision: item.Revision.Revision, Spec: item.Revision.Spec, SpecDigest: item.Revision.SpecDigest, AssignmentsDigest: item.Revision.AssignmentsDigest})
	}
	collection(w, views)
}

func mappedSchedulingError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, scheduling.ErrInvalid):
		writeProblem(w, r, http.StatusUnprocessableEntity, "SchedulingProfileInvalid", "Scheduling profile invalid", "The profile, assignments, or Pod scheduling material is invalid.")
	case errors.Is(err, scheduling.ErrNotFound), errors.Is(err, store.ErrNotFound):
		mappedError(w, r, store.ErrNotFound)
	case errors.Is(err, scheduling.ErrConflict):
		writeProblem(w, r, http.StatusConflict, "SchedulingProfileConflict", "Scheduling profile conflict", "The exact revision or idempotency input no longer matches.")
	case errors.Is(err, scheduling.ErrInactive):
		writeProblem(w, r, http.StatusConflict, "SchedulingProfileInactive", "Scheduling profile inactive", "The scheduling profile is deactivated.")
	case errors.Is(err, scheduling.ErrUnassigned):
		writeProblem(w, r, http.StatusUnprocessableEntity, "SchedulingProfileUnassigned", "Scheduling profile unassigned", "The scheduling profile is not assigned to this workload target.")
	default:
		writeProblem(w, r, http.StatusServiceUnavailable, "SchedulingProfilesUnavailable", "Scheduling profiles unavailable", "The scheduling profile store could not be read safely.")
	}
}
