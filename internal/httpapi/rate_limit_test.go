package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
)

type fixedHighRiskLimiter struct {
	requests []ratelimit.Request
	decision ratelimit.Decision
	err      error
}

func TestLoginRateLimitPolicy(t *testing.T) {
	if loginLimit.bucket != "auth-login" || loginLimit.limit != 30 || loginLimit.window != 15*time.Minute {
		t.Fatalf("login policy=%#v", loginLimit)
	}
}

func (f *fixedHighRiskLimiter) Allow(_ context.Context, request ratelimit.Request) (ratelimit.Decision, error) {
	f.requests = append(f.requests, request)
	return f.decision, f.err
}

func TestRemoteRateLimitSubjectUsesSanitizedForwardedClient(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "https://kuberploy.example/v1/auth/bootstrap", nil)
	request.RemoteAddr = "10.42.0.230:43100"
	request.Header.Set("Forwarded", "for=203.0.113.9")
	request.Header.Set("X-Forwarded-For", "203.0.113.9")
	request.Header.Set("X-Real-IP", "203.0.113.9")
	subject, err := remoteRateLimitSubject(request)
	if err != nil || subject != "ip:203.0.113.9" {
		t.Fatalf("subject=%q err=%v", subject, err)
	}

	request.Header.Set("X-Forwarded-For", "198.51.100.12, 10.42.0.28")
	subject, err = remoteRateLimitSubject(request)
	if err != nil || subject != "ip:198.51.100.12" {
		t.Fatalf("multi-hop subject=%q err=%v", subject, err)
	}

	request.Header.Set("X-Forwarded-For", "not-an-ip")
	if subject, err = remoteRateLimitSubject(request); err == nil || subject != "" {
		t.Fatalf("malformed subject=%q err=%v", subject, err)
	}

	request.Header.Del("X-Forwarded-For")
	subject, err = remoteRateLimitSubject(request)
	if err != nil || subject != "ip:10.42.0.230" {
		t.Fatalf("untrusted-header subject=%q err=%v", subject, err)
	}
}

func TestHighRiskLimiterSeparatesClientsBehindManagedProxy(t *testing.T) {
	limiter := &fixedHighRiskLimiter{decision: ratelimit.Decision{Allowed: true, Remaining: 4}}
	server := &Server{highRiskLimiter: limiter}
	handler := server.highRiskRemote(bootstrapLimit, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	for _, client := range []string{"198.51.100.12", "203.0.113.9"} {
		request := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
		request.RemoteAddr = "10.42.0.230:43100"
		request.Header.Set("X-Forwarded-For", client+", 10.42.0.28")
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}
	if len(limiter.requests) != 2 || limiter.requests[0].Subject != "ip:198.51.100.12" || limiter.requests[1].Subject != "ip:203.0.113.9" {
		t.Fatalf("requests=%#v", limiter.requests)
	}
}

func TestHighRiskLimiterDeniesAndFailsClosed(t *testing.T) {
	denied := &fixedHighRiskLimiter{decision: ratelimit.Decision{Allowed: false, RetryAfter: 1501 * time.Millisecond}}
	server := &Server{highRiskLimiter: denied}
	called := false
	handler := server.highRiskRemote(bootstrapLimit, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	request := httptest.NewRequest(http.MethodPost, "/v1/auth/bootstrap", nil)
	request.RemoteAddr = "192.0.2.25:43100"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "2" || called {
		t.Fatalf("status=%d retry=%q called=%v body=%s", response.Code, response.Header().Get("Retry-After"), called, response.Body.String())
	}
	if len(denied.requests) != 1 || denied.requests[0].Bucket != "auth-bootstrap" || denied.requests[0].Subject != "ip:192.0.2.25" {
		t.Fatalf("requests=%#v", denied.requests)
	}

	for name, limiter := range map[string]ratelimit.Limiter{
		"missing": nil,
		"outage":  &fixedHighRiskLimiter{err: errors.New("Valkey unavailable")},
	} {
		t.Run(name, func(t *testing.T) {
			server := &Server{highRiskLimiter: limiter}
			response := httptest.NewRecorder()
			server.highRiskRemote(bootstrapLimit, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("protected handler ran while limiter was unavailable")
			})).ServeHTTP(response, request.Clone(context.Background()))
			if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "1" || response.Header().Get("Cache-Control") != "no-store" || !strings.Contains(response.Body.String(), `"code":"RateLimitUnavailable"`) {
				t.Fatalf("status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
			}
		})
	}
}

func TestVariableSetMutationLimitUsesDedicatedActorBucket(t *testing.T) {
	limiter := &fixedHighRiskLimiter{decision: ratelimit.Decision{Allowed: true, Remaining: 119, RetryAfter: time.Minute}}
	server := &Server{highRiskLimiter: limiter}
	request := httptest.NewRequest(http.MethodPost, "/v1/environments/11111111-1111-4111-8111-111111111111/variable-sets/project/preview", nil)
	request = request.WithContext(context.WithValue(request.Context(), userKey, domain.User{ID: "22222222-2222-4222-8222-222222222222"}))
	called := false
	server.highRiskActor(variableSetMutationLimit, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })).ServeHTTP(httptest.NewRecorder(), request)
	if !called || len(limiter.requests) != 1 || limiter.requests[0].Bucket != "variable-set-mutation" || limiter.requests[0].Limit != 120 {
		t.Fatalf("called=%t requests=%#v", called, limiter.requests)
	}
}
