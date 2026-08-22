package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/migrations"
)

var (
	ErrMigrationsNotDeployed = errors.New("database migrations are not deployed")
	ErrMigrationMismatch     = errors.New("database migration history does not match this Kuberploy release")
)

type appliedMigration struct {
	name         string
	checksum     string
	appliedSteps int
	startedAt    time.Time
	finishedAt   time.Time
}

// VerifySchema proves that the dedicated Prisma migration Job completed the
// exact immutable history compiled into this release. It is intentionally
// read-only: API and worker startup must never alter a production schema.
func VerifySchema(ctx context.Context, pool *pgxpool.Pool) error {
	var prismaHistoryExists, legacyHistoryExists bool
	if err := pool.QueryRow(ctx, `SELECT
		to_regclass(current_schema() || '._prisma_migrations') IS NOT NULL,
		to_regclass(current_schema() || '.schema_migrations') IS NOT NULL`).Scan(&prismaHistoryExists, &legacyHistoryExists); err != nil {
		return fmt.Errorf("inspect migration history: %w", err)
	}
	if legacyHistoryExists {
		return fmt.Errorf("%w: pre-Prisma release-candidate databases require a fresh PostgreSQL database", ErrMigrationMismatch)
	}
	if !prismaHistoryExists {
		return fmt.Errorf("%w: run the kuberploy-migration Job before API or worker startup", ErrMigrationsNotDeployed)
	}

	var unfinished int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM _prisma_migrations WHERE finished_at IS NULL AND rolled_back_at IS NULL`).Scan(&unfinished); err != nil {
		return fmt.Errorf("inspect unfinished Prisma migrations: %w", err)
	}
	if unfinished != 0 {
		return fmt.Errorf("%w: %d migration(s) are unfinished", ErrMigrationMismatch, unfinished)
	}

	var rolledBack int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM _prisma_migrations WHERE rolled_back_at IS NOT NULL`).Scan(&rolledBack); err != nil {
		return fmt.Errorf("inspect rolled-back Prisma migrations: %w", err)
	}
	if rolledBack != 0 {
		return fmt.Errorf("%w: found %d rolled-back migration(s)", ErrMigrationMismatch, rolledBack)
	}

	rows, err := pool.Query(ctx, `SELECT migration_name, checksum, applied_steps_count,started_at,finished_at
		FROM _prisma_migrations
		WHERE finished_at IS NOT NULL AND rolled_back_at IS NULL
		ORDER BY migration_name`)
	if err != nil {
		return fmt.Errorf("read applied Prisma migrations: %w", err)
	}
	defer rows.Close()
	var applied []appliedMigration
	for rows.Next() {
		var migration appliedMigration
		if err = rows.Scan(&migration.name, &migration.checksum, &migration.appliedSteps, &migration.startedAt, &migration.finishedAt); err != nil {
			return fmt.Errorf("scan applied Prisma migration: %w", err)
		}
		applied = append(applied, migration)
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("iterate applied Prisma migrations: %w", err)
	}

	expected, err := migrations.History()
	if err != nil {
		return err
	}
	if len(applied) != len(expected) {
		return fmt.Errorf("%w: found %d successful migration(s), expected %d", ErrMigrationMismatch, len(applied), len(expected))
	}
	for i := range expected {
		if applied[i].name != expected[i].Name || applied[i].checksum != expected[i].Checksum || applied[i].appliedSteps != 1 ||
			applied[i].finishedAt.Before(applied[i].startedAt) {
			return fmt.Errorf("%w at position %d: found %q, expected %q", ErrMigrationMismatch, i+1, applied[i].name, expected[i].Name)
		}
	}
	return nil
}
