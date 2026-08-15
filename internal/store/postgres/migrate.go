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

	rolledBackRows, err := pool.Query(ctx, `SELECT migration_name,checksum,COALESCE(logs,''),applied_steps_count,
		finished_at IS NULL,started_at,rolled_back_at
		FROM _prisma_migrations
		WHERE rolled_back_at IS NOT NULL
		ORDER BY started_at,id`)
	if err != nil {
		return fmt.Errorf("read rolled-back Prisma migrations: %w", err)
	}
	defer rolledBackRows.Close()
	var rc171FailureSeen, rc171InterruptionSeen, cleanupInterruptionSeen bool
	var rc171FailureStarted, rc171FailureRolledBack time.Time
	var rc171InterruptionStarted, rc171InterruptionRolledBack time.Time
	var cleanupInterruptionStarted, cleanupInterruptionRolledBack time.Time
	for rolledBackRows.Next() {
		var name, checksum, logs string
		var appliedSteps int
		var unfinishedFailure bool
		var startedAt, rolledBackAt time.Time
		if err = rolledBackRows.Scan(&name, &checksum, &logs, &appliedSteps, &unfinishedFailure, &startedAt, &rolledBackAt); err != nil {
			return fmt.Errorf("scan rolled-back Prisma migration: %w", err)
		}
		if !unfinishedFailure || rolledBackAt.Before(startedAt) ||
			!migrations.IsRecoverableRolledBackMigration(name, checksum, logs, appliedSteps) {
			return fmt.Errorf("%w: unexpected rolled-back migration %q", ErrMigrationMismatch, name)
		}
		switch {
		case name == migrations.RecoverableRC171Migration && logs == "":
			if rc171InterruptionSeen {
				return fmt.Errorf("%w: duplicate interrupted migration %q", ErrMigrationMismatch, name)
			}
			rc171InterruptionSeen = true
			rc171InterruptionStarted, rc171InterruptionRolledBack = startedAt, rolledBackAt
		case name == migrations.RecoverableRC171Migration:
			if rc171FailureSeen {
				return fmt.Errorf("%w: duplicate failed migration %q", ErrMigrationMismatch, name)
			}
			rc171FailureSeen = true
			rc171FailureStarted, rc171FailureRolledBack = startedAt, rolledBackAt
		case name == migrations.RecoverableRC171CleanupMigration:
			if cleanupInterruptionSeen {
				return fmt.Errorf("%w: duplicate interrupted migration %q", ErrMigrationMismatch, name)
			}
			cleanupInterruptionSeen = true
			cleanupInterruptionStarted, cleanupInterruptionRolledBack = startedAt, rolledBackAt
		}
	}
	if err = rolledBackRows.Err(); err != nil {
		return fmt.Errorf("iterate rolled-back Prisma migrations: %w", err)
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
		validSteps := applied[i].appliedSteps == 1 ||
			(applied[i].name == migrations.RecoverableRC171Migration &&
				applied[i].appliedSteps == 0 && rc171InterruptionSeen)
		if applied[i].name != expected[i].Name || applied[i].checksum != expected[i].Checksum || !validSteps {
			return fmt.Errorf("%w at position %d: found %q, expected %q", ErrMigrationMismatch, i+1, applied[i].name, expected[i].Name)
		}
	}
	var applied011, applied012 *appliedMigration
	for i := range applied {
		switch applied[i].name {
		case migrations.RecoverableRC171Migration:
			applied011 = &applied[i]
		case migrations.RecoverableRC171CleanupMigration:
			applied012 = &applied[i]
		}
	}
	if applied011 == nil || applied012 == nil ||
		(rc171FailureSeen && rc171FailureRolledBack.After(applied011.startedAt)) ||
		(rc171InterruptionSeen && rc171InterruptionRolledBack.After(applied011.startedAt)) ||
		(rc171FailureSeen && rc171InterruptionSeen &&
			(rc171FailureStarted.After(rc171InterruptionStarted) ||
				rc171FailureRolledBack.After(rc171InterruptionStarted))) ||
		(cleanupInterruptionSeen && (cleanupInterruptionStarted.Before(applied011.finishedAt) ||
			cleanupInterruptionRolledBack.After(applied012.startedAt))) {
		return fmt.Errorf("%w: rolled-back migration chronology is not canonical", ErrMigrationMismatch)
	}
	return nil
}
