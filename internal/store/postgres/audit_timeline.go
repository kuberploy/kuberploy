package postgres

import (
	"context"
	"strings"

	"github.com/kuberploy/kuberploy/internal/domain"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func auditOutcome(action string) string {
	switch {
	case strings.HasSuffix(action, ".accepted"):
		return "accepted"
	case strings.HasSuffix(action, ".succeeded"):
		return "succeeded"
	case strings.HasSuffix(action, ".failed"):
		return "failed"
	default:
		return "recorded"
	}
}

func auditReadTarget(targetType, targetID string) (domain.AccessTarget, bool) {
	switch targetType {
	case "project", "environment", "application", "deployment", "operation":
		return domain.AccessTarget{Type: targetType, ID: targetID}, true
	default:
		return domain.AccessTarget{}, false
	}
}

func (s *Store) ListAuditEventsForActor(ctx context.Context, actor string, query domain.AuditEventQuery) ([]domain.AuditEvent, error) {
	if query.Limit < 1 || query.Limit > 100 || query.TargetType == "" != (query.TargetID == "") {
		return nil, base.ErrConflict
	}
	platform := authorizeWith(ctx, s.pool, actor, domain.PermissionPlatformAdmin,
		domain.AccessTarget{Type: "platform", ID: "platform"}) == nil
	if !platform {
		target, ok := auditReadTarget(query.TargetType, query.TargetID)
		if !ok {
			return nil, base.ErrNotFound
		}
		permission := domain.PermissionResourcesRead
		if target.Type == "operation" {
			permission = domain.PermissionOperationsRead
		}
		if err := authorizeWith(ctx, s.pool, actor, permission, target); err != nil {
			return nil, base.ErrNotFound
		}
	}
	rows, err := s.pool.Query(ctx, `SELECT id,actor_id,action,target_type,target_id,request_id,created_at
		FROM audit_events
		WHERE ($1='' OR target_type=$1) AND ($2='' OR target_id=NULLIF($2,'')::uuid)
		  AND ($3='' OR action=$3)
		ORDER BY created_at DESC,id DESC LIMIT $4`, query.TargetType, query.TargetID, query.Action, query.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.AuditEvent, 0, query.Limit)
	for rows.Next() {
		var event domain.AuditEvent
		if err = rows.Scan(&event.ID, &event.ActorID, &event.Action, &event.TargetType,
			&event.TargetID, &event.RequestID, &event.CreatedAt); err != nil {
			return nil, err
		}
		event.Outcome = auditOutcome(event.Action)
		out = append(out, event)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
