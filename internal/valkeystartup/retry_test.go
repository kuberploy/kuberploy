package valkeystartup

import (
	"context"
	"errors"
	"net"
	"syscall"
	"testing"
	"time"
)

func TestOpenRetriesConnectionRefused(t *testing.T) {
	attempts := 0
	value, err := openWithPolicy(t.Context(), time.Second, time.Millisecond, func() (string, error) {
		attempts++
		if attempts < 3 {
			return "", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
		}
		return "ready", nil
	})
	if err != nil || value != "ready" || attempts != 3 {
		t.Fatalf("value=%q err=%v attempts=%d", value, err, attempts)
	}
}

func TestOpenReturnsPermanentErrorImmediately(t *testing.T) {
	want := errors.New("invalid Valkey password")
	attempts := 0
	_, err := openWithPolicy(t.Context(), time.Second, time.Millisecond, func() (string, error) {
		attempts++
		return "", want
	})
	if !errors.Is(err, want) || attempts != 1 {
		t.Fatalf("err=%v attempts=%d", err, attempts)
	}
}

func TestOpenHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	_, err := openWithPolicy(ctx, time.Second, time.Millisecond, func() (string, error) {
		attempts++
		cancel()
		return "", syscall.ECONNREFUSED
	})
	if !errors.Is(err, context.Canceled) || attempts != 1 {
		t.Fatalf("err=%v attempts=%d", err, attempts)
	}
}

func TestIsRetryableExcludesPermanentDNSMiss(t *testing.T) {
	if IsRetryable(&net.DNSError{Name: "missing.invalid", IsNotFound: true}) {
		t.Fatal("DNS not-found error was considered retryable")
	}
	if !IsRetryable(&net.DNSError{Name: "valkey", IsTemporary: true}) {
		t.Fatal("temporary DNS error was not considered retryable")
	}
}
