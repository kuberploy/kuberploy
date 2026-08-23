package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/id"
	base "github.com/kuberploy/kuberploy/internal/store"
	"github.com/kuberploy/kuberploy/internal/testdb"
)

func TestConcurrentBootstrapCreatesExactlyOneAdmin(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := context.Background()
	baseConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	adminConfig := baseConfig.Copy()
	adminConfig.ConnConfig.Database = "postgres"
	admin, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	databaseName := "bootstrap_" + strings.ReplaceAll(id.New(), "-", "")
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)") //nolint:errcheck

	testConfig := baseConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = testdb.ApplyMigrations(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := &Store{pool: pool}

	var successes atomic.Int32
	errorsSeen := make(chan error, 2)
	var wait sync.WaitGroup
	for index := range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			stamp := time.Now().UTC()
			user := domain.User{ID: id.New(), Email: "admin" + string(rune('a'+index)) + "@integration.test", DisplayName: "Admin", Role: "platform-admin", Issuer: "integration", Subject: id.New(), GrantRevision: 1, CreatedAt: stamp}
			session := sha256.Sum256([]byte(user.ID))
			bootstrapErr := store.BootstrapAdmin(ctx, user, strings.Repeat("h", 64), session[:], stamp.Add(time.Hour))
			if bootstrapErr == nil {
				successes.Add(1)
				return
			}
			errorsSeen <- bootstrapErr
		}()
	}
	wait.Wait()
	close(errorsSeen)
	if successes.Load() != 1 {
		t.Fatalf("successful bootstraps=%d", successes.Load())
	}
	for bootstrapErr := range errorsSeen {
		if !errors.Is(bootstrapErr, base.ErrBootstrapConsumed) {
			t.Fatalf("losing bootstrap error=%v", bootstrapErr)
		}
	}
	var users, grants, sessions int
	if err = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM users),(SELECT count(*) FROM access_grants WHERE source='bootstrap'),(SELECT count(*) FROM sessions)`).Scan(&users, &grants, &sessions); err != nil || users != 1 || grants != 1 || sessions != 1 {
		t.Fatalf("users=%d grants=%d sessions=%d err=%v", users, grants, sessions, err)
	}
	if required, requiredErr := store.BootstrapRequired(ctx); requiredErr != nil || required {
		t.Fatalf("bootstrap required=%v err=%v", required, requiredErr)
	}
}
