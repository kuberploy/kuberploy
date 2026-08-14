package helmapps

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const maximumExpiredHelmReadinessPruneBatch = 64

// pruneExpiredHelmReadinessTx bounds process-identity retention without
// weakening concurrent readiness. A fresh peer is never selected. SKIP LOCKED
// also leaves any row being heartbeated by another process untouched.
func pruneExpiredHelmReadinessTx(ctx context.Context, tx pgx.Tx, runtimeKind, currentWorkerID string) error {
	if tx == nil || (runtimeKind != "helm-renderer" && runtimeKind != "helm-protected-publisher") ||
		!workerIDRE.MatchString(currentWorkerID) {
		return ErrInvalid
	}
	_, err := tx.Exec(ctx, `WITH expired AS (
		SELECT worker_id FROM public.runtime_readiness
		WHERE runtime_kind=$1 AND scope_key='global' AND worker_id<>$2 AND lease_until<=clock_timestamp()
		ORDER BY lease_until,worker_id
		LIMIT $3 FOR UPDATE SKIP LOCKED
	)
	DELETE FROM public.runtime_readiness readiness USING expired
	WHERE readiness.runtime_kind=$1 AND readiness.scope_key='global'
	  AND readiness.worker_id=expired.worker_id`, runtimeKind, currentWorkerID, maximumExpiredHelmReadinessPruneBatch)
	return classifyPostgres(err)
}
