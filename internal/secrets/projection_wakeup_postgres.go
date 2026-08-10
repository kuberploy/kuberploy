package secrets

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// invalidateRuntimeSecretProjectionTx makes metadata-only policy changes
// observable even when the Git provider reports the same branch head. The
// monotonic microsecond bump prevents a concurrent same-timestamp observation
// from erasing the wakeup. The caller owns the transaction, so the secret
// transition and projection invalidation either both commit or both roll back.
func invalidateRuntimeSecretProjectionTx(ctx context.Context, tx pgx.Tx, bindingID string, changedAt time.Time) error {
	if tx == nil || !uuidRE.MatchString(bindingID) || changedAt.IsZero() {
		return ErrInvalid
	}
	_, err := tx.Exec(ctx, `UPDATE git_repository_bindings g
		SET state=CASE WHEN g.target_head_revision IS NULL THEN g.state ELSE 'indexing' END,
			updated_at=GREATEST(g.updated_at+interval '1 microsecond',$2)
		FROM secret_bindings s
		WHERE s.id=$1 AND g.kind='environment' AND g.project_id=s.project_id AND g.environment_id=s.environment_id`,
		bindingID, changedAt.UTC())
	return classifyPostgres(err)
}
