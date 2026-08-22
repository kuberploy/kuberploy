package helmapps

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresApprovedPackageSource serves the exact package captured during
// admission. Render workers therefore never receive source credentials or
// perform provider network access.
type PostgresApprovedPackageSource struct {
	pool *pgxpool.Pool
}

func NewPostgresApprovedPackageSource(pool *pgxpool.Pool) (*PostgresApprovedPackageSource, error) {
	if pool == nil {
		return nil, ErrInvalid
	}
	return &PostgresApprovedPackageSource{pool: pool}, nil
}

func (s *PostgresApprovedPackageSource) Fetch(ctx context.Context, approval Approval) (ChartArtifact, error) {
	if ctx == nil || s == nil || s.pool == nil || approval.Validate() != nil {
		return ChartArtifact{}, ErrInvalid
	}
	var packageBytes []byte
	err := s.pool.QueryRow(ctx, `SELECT package_bytes FROM helm_chart_approval_documents
		WHERE approval_id=$1 AND approval_revision=$2`, approval.ID, approval.Revision).Scan(&packageBytes)
	if err != nil {
		return ChartArtifact{}, classifyPostgres(err)
	}
	if len(packageBytes) == 0 || len(packageBytes) > MaximumChartSize || digestBytes(packageBytes) != approval.PackageDigest {
		clear(packageBytes)
		return ChartArtifact{}, ErrConflict
	}
	return ChartArtifact{ManifestDigest: approval.ManifestDigest, PackageDigest: approval.PackageDigest,
		PackageBytes: packageBytes}, nil
}

var _ ChartPackageSource = (*PostgresApprovedPackageSource)(nil)
