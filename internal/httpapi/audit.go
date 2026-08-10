package httpapi

import (
	"net/http"
	"regexp"
	"strconv"

	"github.com/kuberploy/kuberploy/internal/domain"
)

var auditFilterRE = regexp.MustCompile(`^[a-z][a-z0-9.-]{0,126}[a-z0-9]$|^[a-z]$`)

type auditEventList struct {
	Items []domain.AuditEvent `json:"items"`
}

func (s *Server) listAuditEvents(w http.ResponseWriter, r *http.Request) {
	query := domain.AuditEventQuery{
		TargetType: r.URL.Query().Get("targetType"),
		TargetID:   r.URL.Query().Get("targetId"),
		Action:     r.URL.Query().Get("action"),
		Limit:      50,
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil {
			writeProblem(w, r, http.StatusUnprocessableEntity, "AuditQueryInvalid", "Invalid audit query", "limit must be an integer from 1 through 100.")
			return
		}
		query.Limit = limit
	}
	if query.TargetType == "" != (query.TargetID == "") ||
		query.TargetType != "" && (!auditFilterRE.MatchString(query.TargetType) || !validUUID(query.TargetID)) ||
		query.Action != "" && !auditFilterRE.MatchString(query.Action) || query.Limit < 1 || query.Limit > 100 {
		writeProblem(w, r, http.StatusUnprocessableEntity, "AuditQueryInvalid", "Invalid audit query", "Use an exact targetType/targetId pair, an optional exact action, and limit 1 through 100.")
		return
	}
	items, err := s.store.ListAuditEventsForActor(r.Context(), currentUser(r.Context()).ID, query)
	if err != nil {
		mappedError(w, r, err)
		return
	}
	if items == nil {
		items = []domain.AuditEvent{}
	}
	writeJSON(w, http.StatusOK, auditEventList{Items: items})
}
