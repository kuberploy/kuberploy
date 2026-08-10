package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/domain"
	base "github.com/kuberploy/kuberploy/internal/store"
)

// AuditBuildLogAccess freshly resolves the immutable build-attempt ownership
// chain, reauthorizes both build visibility and logs.read, and writes the audit
// event in one transaction before Kubernetes is contacted.
func (s *Store) AuditBuildLogAccess(ctx context.Context, actor, attemptID, action, requestID string) error {
	if action != "build.logs.snapshot" && action != "build.logs.follow" {
		return base.ErrConflict
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// Access-grant mutations invalidate this exact user row in the same
	// transaction. Holding a shared lock gives authorization plus audit one
	// linearization point with concurrent grant creation/revocation.
	var lockedActor string
	if err = tx.QueryRow(ctx, `SELECT id::text FROM users WHERE id=$1 FOR SHARE`, actor).Scan(&lockedActor); errors.Is(err, pgx.ErrNoRows) {
		return base.ErrNotFound
	} else if err != nil {
		return err
	}
	var projectID, applicationID, teamID string
	err = tx.QueryRow(ctx, `SELECT ba.project_id::text,ba.service_id::text,COALESCE(p.team_id::text,'')
		FROM build_attempts ba
		JOIN applications a ON a.id=ba.service_id AND a.project_id=ba.project_id
		JOIN projects p ON p.id=a.project_id
		WHERE ba.id=$1
		FOR SHARE OF ba,a,p`, attemptID).Scan(&projectID, &applicationID, &teamID)
	if errors.Is(err, pgx.ErrNoRows) {
		return base.ErrNotFound
	}
	if err != nil {
		return err
	}
	target := domain.AccessTarget{Type: "application", ID: applicationID, TeamID: teamID, ProjectID: projectID, ApplicationID: applicationID}
	for _, permission := range []domain.Permission{domain.PermissionBuildsRead, domain.PermissionLogsRead} {
		if err = authorizeWith(ctx, tx, actor, permission, target); err != nil {
			return err
		}
	}
	if err = audit(ctx, tx, actor, action, "build-attempt", attemptID, requestID, map[string]any{"source": "kubernetes-live"}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
