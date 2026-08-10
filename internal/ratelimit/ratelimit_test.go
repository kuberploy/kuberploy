package ratelimit

import (
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMemoryLimiterIsAtomicAndBounded(t *testing.T) {
	now := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	limiter := NewMemoryLimiter(10)
	limiter.now = func() time.Time { return now }
	request := Request{Bucket: "ordinary-read", Subject: "user-1", Limit: 25, Window: time.Minute, Cost: 1}
	var allowed atomic.Int64
	var wait sync.WaitGroup
	for range 100 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			decision, err := limiter.Allow(context.Background(), request)
			if err != nil {
				t.Errorf("allow: %v", err)
			} else if decision.Allowed {
				allowed.Add(1)
			}
		}()
	}
	wait.Wait()
	if allowed.Load() != 25 {
		t.Fatalf("allowed=%d", allowed.Load())
	}
	now = now.Add(time.Minute)
	decision, err := limiter.Allow(context.Background(), request)
	if err != nil || !decision.Allowed || decision.Remaining != 24 {
		t.Fatalf("reset decision=%#v err=%v", decision, err)
	}
	for index := 0; index < 20; index++ {
		_, err = limiter.Allow(context.Background(), Request{Bucket: "ordinary-read", Subject: string(rune('a' + index)), Limit: 1, Window: time.Minute, Cost: 1})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(limiter.buckets) > 10 {
		t.Fatalf("local bucket memory is unbounded: %d", len(limiter.buckets))
	}
}

func TestRateLimitKeysAreOpaqueAndRequestsFailClosed(t *testing.T) {
	request := Request{Bucket: "secret-write", Subject: "private-user@example.test", Limit: 3, Window: time.Minute, Cost: 1}
	cacheKey := key(request)
	if strings.Contains(cacheKey, request.Subject) || len(cacheKey) > 128 || cacheKey != key(request) {
		t.Fatalf("unsafe key=%q", cacheKey)
	}
	invalid := []Request{
		{},
		{Bucket: "UPPER", Subject: "user", Limit: 1, Window: time.Second, Cost: 1},
		{Bucket: "valid", Subject: "user\nspoof", Limit: 1, Window: time.Second, Cost: 1},
		{Bucket: "valid", Subject: "user", Limit: 0, Window: time.Second, Cost: 1},
		{Bucket: "valid", Subject: "user", Limit: 1, Window: time.Microsecond, Cost: 1},
	}
	limiter := NewMemoryLimiter(1)
	for _, value := range invalid {
		if _, err := limiter.Allow(context.Background(), value); err == nil {
			t.Fatalf("invalid request accepted: %#v", value)
		}
	}
	if _, err := NewValkeyLimiter(ValkeyOptions{}); err == nil {
		t.Fatal("empty Valkey configuration was accepted")
	}
}

func TestValkeyLimiterIntegration(t *testing.T) {
	address := strings.TrimSpace(os.Getenv("KUBERPLOY_TEST_VALKEY_ADDRESS"))
	if address == "" {
		t.Skip("KUBERPLOY_TEST_VALKEY_ADDRESS is not set")
	}
	limiter, err := NewValkeyLimiter(ValkeyOptions{Addresses: []string{address}})
	if err != nil {
		t.Fatal(err)
	}
	defer limiter.Close()
	request := Request{Bucket: "test-limit", Subject: t.Name() + time.Now().String(), Limit: 2, Window: 2 * time.Second, Cost: 1}
	for index := 0; index < 3; index++ {
		decision, allowErr := limiter.Allow(context.Background(), request)
		if allowErr != nil {
			t.Fatal(allowErr)
		}
		if decision.Allowed != (index < 2) || decision.RetryAfter <= 0 || decision.RetryAfter > request.Window {
			t.Fatalf("index=%d decision=%#v", index, decision)
		}
	}
	_ = limiter.client.Do(context.Background(), limiter.client.B().Del().Key(key(request)).Build()).Error()
}
