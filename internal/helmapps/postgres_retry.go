package helmapps

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"
)

const (
	protectedTransactionMaxAttempts = 5
	protectedTransactionMaxElapsed  = 2 * time.Second
	protectedTransactionBaseBackoff = 10 * time.Millisecond
	protectedTransactionMaxBackoff  = 200 * time.Millisecond
)

type sqlStateError interface {
	SQLState() string
}

func retryableProtectedTransactionError(err error) bool {
	var state sqlStateError
	if !errors.As(err, &state) {
		return false
	}
	return state.SQLState() == "40001" || state.SQLState() == "40P01"
}

func retryProtectedTransaction[T any](ctx context.Context, operation func() (T, error)) (T, error) {
	var zero T
	if ctx == nil || operation == nil {
		return zero, ErrInvalid
	}
	started := time.Now()
	for attempt := 0; attempt < protectedTransactionMaxAttempts; attempt++ {
		value, err := operation()
		if err == nil || !retryableProtectedTransactionError(err) {
			return value, err
		}
		if attempt+1 == protectedTransactionMaxAttempts {
			return zero, err
		}
		backoff := protectedTransactionBaseBackoff << attempt
		if backoff > protectedTransactionMaxBackoff {
			backoff = protectedTransactionMaxBackoff
		}
		jitter := time.Duration(rand.Int64N(int64(backoff/2) + 1))
		delay := backoff/2 + jitter
		if time.Since(started)+delay > protectedTransactionMaxElapsed {
			return zero, err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return zero, ctx.Err()
		case <-timer.C:
		}
	}
	return zero, ErrConflict
}
