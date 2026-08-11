package postgres

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/migrations"
)

var migrationFilenamePattern = regexp.MustCompile(`^[0-9]{3}_[a-z0-9]+(?:_[a-z0-9]+)*\.sql$`)

const stableBaseline = "0.1.0"

func loadMigrationFilename(name string) (bool, error) {
	// macOS archive tools may materialize AppleDouble sidecars next to source
	// files. They are transport metadata, never executable migrations.
	if strings.HasPrefix(name, "._") {
		return false, nil
	}
	if !strings.HasSuffix(name, ".sql") {
		return false, nil
	}
	if !migrationFilenamePattern.MatchString(name) {
		return false, fmt.Errorf("embedded migration has noncanonical filename %q", name)
	}
	return true, nil
}

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()
	if _, err = conn.Exec(ctx, `SELECT pg_advisory_lock(hashtext('kuberploy-schema-migrations'))`); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext('kuberploy-schema-migrations'))`) //nolint:errcheck
	if _, err = conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("initialize migrations: %w", err)
	}
	var appliedCount int
	if err = conn.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&appliedCount); err != nil {
		return fmt.Errorf("inspect migration history: %w", err)
	}
	if appliedCount > 0 {
		var hasStableIdentity bool
		if err = conn.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema=current_schema() AND table_name='schema_migrations' AND column_name='baseline'
		)`).Scan(&hasStableIdentity); err != nil {
			return fmt.Errorf("inspect stable baseline identity: %w", err)
		}
		if !hasStableIdentity {
			return errors.New("pre-stable database history is not upgradeable; install 0.1.0 with a fresh PostgreSQL database")
		}
	}
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		load, nameErr := loadMigrationFilename(entry.Name())
		if nameErr != nil {
			return nameErr
		}
		if load {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		var applied bool
		err = conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version=$1)`, name).Scan(&applied)
		if err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if applied {
			continue
		}
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", name, err)
		}
		if _, err = tx.Exec(ctx, string(body)); err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES ($1) ON CONFLICT DO NOTHING`, name)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if err = tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	var baseline string
	if err = conn.QueryRow(ctx, `SELECT baseline FROM schema_migrations WHERE version=$1`, migrations.CurrentSchema+".sql").Scan(&baseline); err != nil {
		return fmt.Errorf("read stable baseline identity: %w", err)
	}
	if baseline != stableBaseline {
		return fmt.Errorf("unsupported database baseline %q", baseline)
	}
	return nil
}
