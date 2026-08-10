// Package ratelimit provides the bounded distributed and process-local rate
// limit primitives used by API middleware. Limit state is acceleration and
// abuse-control data only; it is never an authorization source.
package ratelimit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	valkey "github.com/valkey-io/valkey-go"
)

const (
	minimumWindow  = time.Second
	maximumWindow  = 24 * time.Hour
	maximumLimit   = 1_000_000
	maximumCost    = 10_000
	defaultBuckets = 100_000
)

var bucketPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)

type Request struct {
	Bucket  string
	Subject string
	Limit   int64
	Window  time.Duration
	Cost    int64
}

type Decision struct {
	Allowed    bool
	Remaining  int64
	RetryAfter time.Duration
	ResetAt    time.Time
}

type Limiter interface {
	Allow(context.Context, Request) (Decision, error)
}

type ValkeyOptions struct {
	Addresses []string
	Username  string
	Password  string
}

type ValkeyLimiter struct {
	client valkey.Client
	now    func() time.Time
	script *valkey.Lua
}

const fixedWindowScript = `
local current = redis.call('INCRBY', KEYS[1], ARGV[1])
local ttl = redis.call('PTTL', KEYS[1])
if current == tonumber(ARGV[1]) or ttl < 0 then
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
  ttl = tonumber(ARGV[2])
end
return {current, ttl}
`

func NewValkeyLimiter(options ValkeyOptions) (*ValkeyLimiter, error) {
	if len(options.Addresses) == 0 {
		return nil, errors.New("Valkey limiter requires at least one address")
	}
	for _, address := range options.Addresses {
		if strings.TrimSpace(address) == "" || len(address) > 512 || strings.ContainsAny(address, "\x00\r\n") {
			return nil, errors.New("Valkey limiter address is invalid")
		}
	}
	client, err := valkey.NewClient(valkey.ClientOption{InitAddress: options.Addresses, Username: options.Username, Password: options.Password, ClientName: "kuberploy-rate-limiter", DisableCache: true})
	if err != nil {
		return nil, err
	}
	return &ValkeyLimiter{client: client, now: time.Now, script: valkey.NewLuaScript(fixedWindowScript, valkey.WithLoadSHA1(true))}, nil
}

func (l *ValkeyLimiter) Close() {
	if l != nil && l.client != nil {
		l.client.Close()
	}
}

func (l *ValkeyLimiter) Ping(ctx context.Context) error {
	if l == nil || l.client == nil {
		return errors.New("Valkey limiter is not configured")
	}
	return l.client.Do(ctx, l.client.B().Ping().Build()).Error()
}

func (l *ValkeyLimiter) Allow(ctx context.Context, request Request) (Decision, error) {
	if l == nil || l.client == nil || l.script == nil {
		return Decision{}, errors.New("Valkey limiter is not configured")
	}
	request, err := validateRequest(request)
	if err != nil {
		return Decision{}, err
	}
	result, err := l.script.Exec(ctx, l.client, []string{key(request)}, []string{strconv.FormatInt(request.Cost, 10), strconv.FormatInt(request.Window.Milliseconds(), 10)}).ToArray()
	if err != nil {
		return Decision{}, fmt.Errorf("Valkey limiter: %w", err)
	}
	if len(result) != 2 {
		return Decision{}, errors.New("Valkey limiter returned an invalid response")
	}
	used, err := result[0].ToInt64()
	if err != nil || used < request.Cost {
		return Decision{}, errors.New("Valkey limiter returned an invalid counter")
	}
	ttlMillis, err := result[1].ToInt64()
	if err != nil || ttlMillis < 1 || ttlMillis > request.Window.Milliseconds() {
		return Decision{}, errors.New("Valkey limiter returned an invalid TTL")
	}
	remaining := request.Limit - used
	if remaining < 0 {
		remaining = 0
	}
	retry := time.Duration(ttlMillis) * time.Millisecond
	return Decision{Allowed: used <= request.Limit, Remaining: remaining, RetryAfter: retry, ResetAt: l.now().UTC().Add(retry)}, nil
}

type localBucket struct {
	used      int64
	resetAt   time.Time
	lastTouch time.Time
}

// MemoryLimiter is the conservative per-process fallback for ordinary reads.
// High-risk endpoints must use ValkeyLimiter directly and fail closed on its
// error; they must not substitute this fallback.
type MemoryLimiter struct {
	mu         sync.Mutex
	buckets    map[string]localBucket
	maxBuckets int
	now        func() time.Time
}

func NewMemoryLimiter(maxBuckets int) *MemoryLimiter {
	if maxBuckets <= 0 || maxBuckets > 1_000_000 {
		maxBuckets = defaultBuckets
	}
	return &MemoryLimiter{buckets: make(map[string]localBucket), maxBuckets: maxBuckets, now: time.Now}
}

func (l *MemoryLimiter) Allow(_ context.Context, request Request) (Decision, error) {
	if l == nil {
		return Decision{}, errors.New("local limiter is not configured")
	}
	request, err := validateRequest(request)
	if err != nil {
		return Decision{}, err
	}
	now := l.now().UTC()
	bucketKey := key(request)
	l.mu.Lock()
	defer l.mu.Unlock()
	current, exists := l.buckets[bucketKey]
	if !exists && len(l.buckets) >= l.maxBuckets {
		l.evictExpiredOrOldest(now)
	}
	if !exists || !current.resetAt.After(now) {
		current = localBucket{resetAt: now.Add(request.Window)}
	}
	current.used += request.Cost
	current.lastTouch = now
	l.buckets[bucketKey] = current
	remaining := request.Limit - current.used
	if remaining < 0 {
		remaining = 0
	}
	return Decision{Allowed: current.used <= request.Limit, Remaining: remaining, RetryAfter: current.resetAt.Sub(now), ResetAt: current.resetAt}, nil
}

func (l *MemoryLimiter) evictExpiredOrOldest(now time.Time) {
	oldestKey := ""
	var oldest time.Time
	for key, bucket := range l.buckets {
		if !bucket.resetAt.After(now) {
			delete(l.buckets, key)
			return
		}
		if oldestKey == "" || bucket.lastTouch.Before(oldest) {
			oldestKey, oldest = key, bucket.lastTouch
		}
	}
	if oldestKey != "" {
		delete(l.buckets, oldestKey)
	}
}

func validateRequest(request Request) (Request, error) {
	request.Subject = strings.TrimSpace(request.Subject)
	if !bucketPattern.MatchString(request.Bucket) || request.Subject == "" || len(request.Subject) > 512 ||
		strings.ContainsAny(request.Subject, "\x00\r\n") || request.Limit < 1 || request.Limit > maximumLimit ||
		request.Window < minimumWindow || request.Window > maximumWindow || request.Window%time.Millisecond != 0 ||
		request.Cost < 1 || request.Cost > maximumCost {
		return Request{}, errors.New("rate-limit request is invalid")
	}
	return request, nil
}

func key(request Request) string {
	digest := sha256.Sum256([]byte(request.Bucket + "\x00" + request.Subject))
	return "kp:v1:limit:" + request.Bucket + ":" + hex.EncodeToString(digest[:16])
}
