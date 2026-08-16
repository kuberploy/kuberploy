package postgres

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/testdb"
)

func TestMain(m *testing.M) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		os.Exit(m.Run())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err == nil {
		err = testdb.ApplyMigrations(ctx, pool)
	}
	if pool != nil {
		pool.Close()
	}
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "postgres integration migration setup failed: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
