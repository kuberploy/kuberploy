// Package valkeystartup contains the bounded retry policy used while opening
// the control-plane's Valkey clients. Kubernetes can start the API and worker
// before the managed Valkey Service has accepted connections; that transient
// ordering must not turn into a needless container restart.
package valkeystartup

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"
)

const (
	RetryTimeout = 30 * time.Second
	RetryBackoff = 500 * time.Millisecond
)

// Open retries only transport-level startup failures. Invalid addresses,
// authentication failures, and other configuration/protocol errors return
// immediately so they remain visible instead of being hidden by a retry loop.
func Open[T any](ctx context.Context, open func() (T, error)) (T, error) {
	return openWithPolicy(ctx, RetryTimeout, RetryBackoff, open)
}

func openWithPolicy[T any](ctx context.Context, timeout, backoff time.Duration, open func() (T, error)) (T, error) {
	var zero T
	retryCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var lastErr error
	for {
		value, err := open()
		if err == nil {
			return value, nil
		}
		lastErr = err
		if ctxErr := ctx.Err(); ctxErr != nil {
			return zero, ctxErr
		}
		if !IsRetryable(err) {
			return zero, err
		}

		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-retryCtx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return zero, ctxErr
			}
			return zero, fmt.Errorf("Valkey startup retry timed out after %s (last error: %v): %w", timeout, lastErr, retryCtx.Err())
		}
	}
}

// IsRetryable reports whether an error is caused by temporary network
// unavailability during process startup. DNS "not found" is intentionally
// excluded: a misspelled service name is a configuration error, not a startup
// ordering problem.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ETIMEDOUT) ||
		errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH) {
		return true
	}
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && (dnsErr.IsTimeout || dnsErr.IsTemporary)
}
