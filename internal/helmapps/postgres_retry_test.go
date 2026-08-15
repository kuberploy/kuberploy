package helmapps

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetryProtectedTransactionRetriesOnlyDatabaseConcurrency(t *testing.T) {
	for _, code := range []string{"40001", "40P01"} {
		t.Run(code, func(t *testing.T) {
			attempts := 0
			value, err := retryProtectedTransaction(t.Context(), func() (int, error) {
				attempts++
				if attempts < 3 {
					return 0, postgresConflictError{code: code, detail: "bounded test conflict"}
				}
				return 7, nil
			})
			if err != nil || value != 7 || attempts != 3 {
				t.Fatalf("value=%d attempts=%d err=%v", value, attempts, err)
			}
		})
	}
	semantic := postgresConflictError{code: "23514", detail: "semantic rejection"}
	attempts := 0
	if _, err := retryProtectedTransaction(t.Context(), func() (int, error) {
		attempts++
		return 0, semantic
	}); !errors.Is(err, ErrConflict) || attempts != 1 {
		t.Fatalf("semantic attempts=%d err=%v", attempts, err)
	}
}

func TestRetryProtectedTransactionStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	attempts := 0
	_, err := retryProtectedTransaction(ctx, func() (int, error) {
		attempts++
		cancel()
		return 0, postgresConflictError{code: "40001", detail: "bounded test conflict"}
	})
	if !errors.Is(err, context.Canceled) || attempts != 1 {
		t.Fatalf("attempts=%d err=%v", attempts, err)
	}
}

func TestRetryProtectedTransactionExhaustionIsBounded(t *testing.T) {
	attempts := 0
	started := time.Now()
	_, err := retryProtectedTransaction(t.Context(), func() (int, error) {
		attempts++
		return 0, postgresConflictError{code: "40001", detail: "bounded test conflict"}
	})
	var state sqlStateError
	if !errors.Is(err, ErrConflict) || !errors.As(err, &state) || state.SQLState() != "40001" ||
		attempts != protectedTransactionMaxAttempts || time.Since(started) >= protectedTransactionMaxElapsed {
		t.Fatalf("attempts=%d elapsed=%s state=%v err=%v",
			attempts, time.Since(started), state, err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	attempts = 0
	_, err = retryProtectedTransaction(ctx, func() (int, error) {
		attempts++
		return 0, postgresConflictError{code: "40P01", detail: "bounded test deadlock"}
	})
	if !errors.Is(err, context.DeadlineExceeded) || attempts != 1 {
		t.Fatalf("deadline attempts=%d err=%v", attempts, err)
	}
}
