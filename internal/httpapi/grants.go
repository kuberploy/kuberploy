package httpapi

import (
	"net/http"
	"sort"
	"strings"

	accesspolicy "github.com/kuberploy/kuberploy/internal/access"
	"github.com/kuberploy/kuberploy/internal/domain"
)

type accessGrantRequest struct {
	SubjectUserID string                 `json:"subjectUserId"`
	SubjectTeamID string                 `json:"subjectTeamId"`
	Role          domain.AccessRole      `json:"role"`
	ScopeType     domain.AccessScopeType `json:"scopeType"`
	ScopeID       string                 `json:"scopeId"`
	Permissions   []domain.Permission    `json:"permissions,omitempty"`
}

func (s *Server) projectAccessGrants(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if !validUUID(projectID) {
		writeProblem(w, r, http.StatusNotFound, "NotFound", "Not found", "The requested resource was not found.")
		return
	}
	actor := currentUser(r.Context())
	if r.Method == http.MethodGet {
		items, err := s.store.ListProjectAccessGrants(r.Context(), actor.ID, projectID)
		if err != nil {
			mappedError(w, r, err)
			return
		}
		collection(w, items)
		return
	}
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	var in accessGrantRequest
	if !decode(w, r, &in) {
		return
	}
	in.SubjectUserID = strings.TrimSpace(in.SubjectUserID)
	in.SubjectTeamID = strings.TrimSpace(in.SubjectTeamID)
	in.ScopeID = strings.TrimSpace(in.ScopeID)
	permissions := append([]domain.Permission(nil), in.Permissions...)
	sort.Slice(permissions, func(i, j int) bool { return permissions[i] < permissions[j] })
	in.Permissions = permissions
	errors := validateAccessGrantRequest(in)
	if len(errors) > 0 {
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "The access grant role, scope, or additive permissions are invalid.", errors...)
		return
	}
	create := domain.CreateAccessGrant{ProjectID: projectID, SubjectUserID: in.SubjectUserID, SubjectTeamID: in.SubjectTeamID, Role: in.Role, ScopeType: in.ScopeType, ScopeID: in.ScopeID, Permissions: in.Permissions}
	result, err := s.store.CreateProjectAccessGrant(r.Context(), actor.ID, key, fingerprint(in), requestID(r.Context()), create)
	if err != nil {
		mappedError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Location", "/v1/projects/"+projectID+"/grants/"+result.Value.ID)
	writeJSON(w, http.StatusCreated, result.Value)
}

func validateAccessGrantRequest(in accessGrantRequest) []FieldError {
	var out []FieldError
	if (in.SubjectUserID == "") == (in.SubjectTeamID == "") {
		out = append(out, FieldError{Pointer: "/subjectUserId", Code: "ExactlyOneRequired", Detail: "Choose exactly one user or team subject."})
	} else if in.SubjectUserID != "" && !validUUID(in.SubjectUserID) {
		out = append(out, FieldError{Pointer: "/subjectUserId", Code: "InvalidID", Detail: "Choose an exact user identifier."})
	} else if in.SubjectTeamID != "" && !validUUID(in.SubjectTeamID) {
		out = append(out, FieldError{Pointer: "/subjectTeamId", Code: "InvalidID", Detail: "Choose an exact team identifier."})
	}
	if !accesspolicy.ValidRole(in.Role) || in.Role == domain.RolePlatformAdmin {
		out = append(out, FieldError{Pointer: "/role", Code: "InvalidRole", Detail: "Use viewer, developer, project-admin, or organization-admin. Platform administrator grants are bootstrap-managed."})
	}
	if !accesspolicy.ValidScope(in.ScopeType) || in.ScopeType == domain.ScopePlatform || in.ScopeID == "" {
		out = append(out, FieldError{Pointer: "/scopeType", Code: "InvalidScope", Detail: "Use the exact team, project, environment, namespace, or application scope."})
	} else if in.ScopeType == domain.ScopeNamespace {
		if !validSlug(in.ScopeID) {
			out = append(out, FieldError{Pointer: "/scopeId", Code: "InvalidID", Detail: "Use the exact Kubernetes namespace name."})
		}
	} else if !validUUID(in.ScopeID) {
		out = append(out, FieldError{Pointer: "/scopeId", Code: "InvalidID", Detail: "Use the exact resource identifier for this scope."})
	}
	if in.Role == domain.RoleOrganizationAdmin && in.ScopeType != domain.ScopeTeam {
		out = append(out, FieldError{Pointer: "/scopeType", Code: "RoleScopeMismatch", Detail: "organization-admin may only be assigned to the owning team."})
	}
	if in.Role == domain.RoleProjectAdmin && in.ScopeType != domain.ScopeProject {
		out = append(out, FieldError{Pointer: "/scopeType", Code: "RoleScopeMismatch", Detail: "project-admin may only be assigned to this project."})
	}
	if !accesspolicy.ValidExtraPermissions(in.Permissions) {
		out = append(out, FieldError{Pointer: "/permissions", Code: "InvalidPermission", Detail: "The only P0 additive permission is logs.read."})
	}
	return out
}

func (s *Server) deleteProjectAccessGrant(w http.ResponseWriter, r *http.Request) {
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	projectID, grantID := r.PathValue("projectId"), r.PathValue("grantId")
	if !validUUID(projectID) || !validUUID(grantID) {
		writeProblem(w, r, http.StatusNotFound, "NotFound", "Not found", "The requested resource was not found.")
		return
	}
	fp := fingerprint(struct {
		ProjectID string `json:"projectId"`
		GrantID   string `json:"grantId"`
	}{projectID, grantID})
	replay, err := s.store.DeleteProjectAccessGrant(r.Context(), currentUser(r.Context()).ID, projectID, grantID, key, fp, requestID(r.Context()))
	if err != nil {
		mappedError(w, r, err)
		return
	}
	if replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.WriteHeader(http.StatusNoContent)
}
