// Package testdb contains database helpers imported only by integration tests.
package testdb

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/migrations"
)

// ApplyMigrations mirrors the immutable Prisma SQL history for disposable Go
// integration databases. Production packages do not import this package.
func ApplyMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS _prisma_migrations (
		id varchar(36) PRIMARY KEY,
		checksum varchar(64) NOT NULL,
		finished_at timestamptz,
		migration_name varchar(255) NOT NULL,
		logs text,
		rolled_back_at timestamptz,
		started_at timestamptz NOT NULL DEFAULT now(),
		applied_steps_count integer NOT NULL DEFAULT 0
	)`); err != nil {
		return fmt.Errorf("initialize test migration history: %w", err)
	}
	history, err := migrations.History()
	if err != nil {
		return err
	}
	for index, migration := range history {
		var appliedChecksum string
		err = pool.QueryRow(ctx, `SELECT checksum FROM _prisma_migrations
			WHERE migration_name=$1 AND finished_at IS NOT NULL AND rolled_back_at IS NULL`, migration.Name).Scan(&appliedChecksum)
		if err == nil {
			if appliedChecksum != migration.Checksum {
				return fmt.Errorf("test migration %s checksum differs", migration.Name)
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("inspect test migration %s: %w", migration.Name, err)
		}
		tx, beginErr := pool.Begin(ctx)
		if beginErr != nil {
			return fmt.Errorf("begin test migration %s: %w", migration.Name, beginErr)
		}
		body, applyErr := migrations.FS.ReadFile("prisma/migrations/" + migration.Name + "/migration.sql")
		if applyErr == nil {
			_, applyErr = tx.Exec(ctx, string(body))
		}
		if applyErr == nil {
			id := fmt.Sprintf("00000000-0000-0000-0000-%012d", index+1)
			_, applyErr = tx.Exec(ctx, `INSERT INTO _prisma_migrations
				(id,checksum,finished_at,migration_name,started_at,applied_steps_count)
				VALUES($1,$2,now(),$3,now(),1)`, id, migration.Checksum, migration.Name)
		}
		if applyErr != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply test migration %s: %w", migration.Name, applyErr)
		}
		if err = tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit test migration %s: %w", migration.Name, err)
		}
	}
	return nil
}
