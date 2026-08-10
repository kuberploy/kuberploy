package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/imagepull"
)

// ResolveRegistryPull derives only the safe locked AppConfig reference from a
// serializable snapshot. Credential material remains outside PostgreSQL and
// the API process; projection activation repeats the complete policy check.
func (s *Store) ResolveRegistryPull(
	ctx context.Context,
	config imagepull.RuntimeConfig,
	applicationID, environmentID, repository string,
) (domain.RegistryPullReference, bool, error) {
	if s == nil || s.pool == nil || config.Validate() != nil {
		return domain.RegistryPullReference{}, false, imagepull.ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return domain.RegistryPullReference{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	resolved, present, err := imagepull.ResolveReferenceTx(ctx, tx, config, applicationID, environmentID, repository)
	if err != nil {
		return domain.RegistryPullReference{}, false, err
	}
	result := domain.RegistryPullReference{TargetID: resolved.TargetID, ProfileName: resolved.ProfileName, ProfileRevision: resolved.ProfileRevision}
	if present && !result.Valid() {
		return domain.RegistryPullReference{}, false, imagepull.ErrConflict
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.RegistryPullReference{}, false, err
	}
	return result, present, nil
}
