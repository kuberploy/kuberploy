package postgres

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestPostgresStartupRetryPolicyIsBounded(t *testing.T) {
	if startupPingTimeout != 30*time.Second {
		t.Fatalf("startup ping timeout = %s", startupPingTimeout)
	}
	if startupPingAttemptTimeout != 3*time.Second {
		t.Fatalf("startup ping attempt timeout = %s", startupPingAttemptTimeout)
	}
	if startupPingBackoff != 500*time.Millisecond {
		t.Fatalf("startup ping backoff = %s", startupPingBackoff)
	}
}

func TestOpenRejectsInvalidDatabaseURLBeforeRetry(t *testing.T) {
	store, err := Open(t.Context(), "postgres://%")
	if store != nil {
		store.Close()
		t.Fatal("Open returned a store for an invalid database URL")
	}
	if err == nil || !strings.Contains(err.Error(), "parse database URL") {
		t.Fatalf("error = %v, want database URL parse failure", err)
	}
}

func TestPingPostgresAtStartupRetriesConnectionRefused(t *testing.T) {
	attempts := 0
	err := pingPostgresAtStartup(t.Context(), time.Second, time.Second, time.Millisecond, func(context.Context) error {
		attempts++
		if attempts < 3 {
			return &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Fatalf("ping attempts = %d, want 3", attempts)
	}
}

func TestPingPostgresAtStartupRetriesPostgresStartingUp(t *testing.T) {
	attempts := 0
	err := pingPostgresAtStartup(t.Context(), time.Second, time.Second, time.Millisecond, func(context.Context) error {
		attempts++
		if attempts == 1 {
			return fmt.Errorf("connect: %w", &pgconn.PgError{Code: "57P03", Message: "the database system is starting up"})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("ping attempts = %d, want 2", attempts)
	}
}

func TestPingPostgresAtStartupFailsPermanentErrorsImmediately(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "authentication", err: &pgconn.PgError{Code: "28P01", Message: "password authentication failed"}},
		{name: "dns not found", err: &net.DNSError{Name: "missing.invalid", IsNotFound: true}},
		{name: "other", err: errors.New("permanent configuration failure")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			attempts := 0
			err := pingPostgresAtStartup(t.Context(), time.Second, time.Second, time.Millisecond, func(context.Context) error {
				attempts++
				return fmt.Errorf("connect: %w", test.err)
			})
			if !errors.Is(err, test.err) {
				t.Fatalf("error = %v, want wrapped %v", err, test.err)
			}
			if attempts != 1 {
				t.Fatalf("ping attempts = %d, want 1", attempts)
			}
		})
	}
}

func TestPingPostgresAtStartupHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	err := pingPostgresAtStartup(ctx, time.Second, time.Second, time.Second, func(context.Context) error {
		attempts++
		cancel()
		return syscall.ECONNREFUSED
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	if attempts != 1 {
		t.Fatalf("ping attempts = %d, want 1", attempts)
	}
}

func TestPingPostgresAtStartupStopsAtRetryTimeout(t *testing.T) {
	attempts := 0
	err := pingPostgresAtStartup(t.Context(), 10*time.Millisecond, time.Second, time.Second, func(context.Context) error {
		attempts++
		return syscall.ECONNREFUSED
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
	if attempts != 1 {
		t.Fatalf("ping attempts = %d, want retry budget to expire during backoff", attempts)
	}
}

func TestPingPostgresAtStartupBoundsEachAttempt(t *testing.T) {
	attempts := 0
	err := pingPostgresAtStartup(t.Context(), time.Second, 5*time.Millisecond, time.Millisecond, func(attemptCtx context.Context) error {
		attempts++
		if attempts == 1 {
			<-attemptCtx.Done()
			return attemptCtx.Err()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("ping attempts = %d, want 2", attempts)
	}
}
