package postgres

import (
	"context"
	"strconv"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/operationcache"
)

// OperationCacheIdentityForActor re-evaluates authorization in PostgreSQL and
// returns the current principal and operation revisions used for one exact
// disposable cache lookup. It deliberately does not return the operation body.
func (s *Store) OperationCacheIdentityForActor(ctx context.Context, actor, operationID string) (operationcache.Identity, error) {
	if err := authorizeWith(ctx, s.pool, actor, domain.PermissionOperationsRead, domain.AccessTarget{Type: "operation", ID: operationID}); err != nil {
		return operationcache.Identity{}, err
	}
	var grantRevision, generation, publicationVersion int64
	var updatedAt, publicationUpdatedAt time.Time
	if err := s.pool.QueryRow(ctx, `SELECT u.grant_revision,o.generation,o.updated_at,
		COALESCE(p.version,0),COALESCE(p.updated_at,to_timestamp(0))
		FROM users u CROSS JOIN operations o
		LEFT JOIN git_pull_request_publications p ON p.operation_id=o.id
		WHERE u.id=$1 AND o.id=$2`, actor, operationID).
		Scan(&grantRevision, &generation, &updatedAt, &publicationVersion, &publicationUpdatedAt); err != nil {
		return operationcache.Identity{}, classify(err)
	}
	identity, err := operationcache.NewIdentity(actor, grantRevision, operationID, generation, updatedAt)
	if err != nil {
		return operationcache.Identity{}, err
	}
	return identity.WithSourceRevision(strconv.FormatInt(publicationVersion, 10), publicationUpdatedAt.UTC().Format(time.RFC3339Nano))
}
