package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/domain"
	base "github.com/kuberploy/kuberploy/internal/store"
)

// AuditRuntimeAccess authorizes and persists the security-sensitive read in
// one transaction before the API contacts Kubernetes. It records identities
// and bounded request metadata only; log and event bodies never enter SQL.
func (s *Store) AuditRuntimeAccess(ctx context.Context, actor, deploymentID, action, requestID string) error {
	if action != "runtime.logs.snapshot" && action != "runtime.logs.follow" && action != "runtime.events.snapshot" {
		return base.ErrConflict
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = authorizeWith(ctx, tx, actor, domain.PermissionLogsRead, domain.AccessTarget{Type: "deployment", ID: deploymentID}); err != nil {
		return err
	}
	if err = audit(ctx, tx, actor, action, "deployment", deploymentID, requestID, map[string]any{"source": "kubernetes-live"}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
