package githubapp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientSendsCurrentHeadersAndVerifiesExactUser(t *testing.T) {
	cfg := validTestConfig(t)
	var calls atomic.Int64
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		if request.Method != http.MethodGet || request.URL.String() != "https://api.github.com/user" {
			t.Fatalf("unexpected request %s %s", request.Method, request.URL)
		}
		if request.Header.Get("Authorization") != "Bearer user-access-token-opaque" ||
			request.Header.Get("Accept") != "application/vnd.github+json" ||
			request.Header.Get("X-GitHub-Api-Version") != CurrentAPIVersion ||
			request.Header.Get("User-Agent") != cfg.UserAgent {
			t.Fatalf("headers=%#v", request.Header)
		}
		return httpResponse(http.StatusOK, `{"id":900,"login":"octocat","type":"User","future_additive_field":true}`, nil), nil
	})
	client, err := NewClient(cfg, staticAppTokens{token: testAppToken()}, transport, &fixedClock{now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	user, err := client.VerifyAuthenticatedUserRaw(context.Background(), "user-access-token-opaque")
	if err != nil {
		t.Fatal(err)
	}
	if user.ID != 900 || user.Login != "octocat" || user.Type != "User" || calls.Load() != 1 {
		t.Fatalf("user=%#v calls=%d", user, calls.Load())
	}
}

func TestClientDefensivelyCopiesPermissionCaps(t *testing.T) {
	cfg := validTestConfig(t)
	client, err := NewClient(cfg, staticAppTokens{token: testAppToken()}, roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("scope widening should fail before HTTP")
		return nil, nil
	}), &fixedClock{now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaximumTokenPermissions["contents"] = PermissionWrite
	if _, err = client.normalizePermissions(Permissions{"metadata": PermissionRead, "contents": PermissionWrite}); !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("post-construction config mutation widened permissions: %v", err)
	}
}

func TestClientRefusesRedirectWithoutForwardingCredential(t *testing.T) {
	cfg := validTestConfig(t)
	token := "user-access-token-never-forward"
	var calls atomic.Int64
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		call := calls.Add(1)
		if call > 1 {
			t.Fatalf("redirect was followed to %s with authorization=%q", request.URL, request.Header.Get("Authorization"))
		}
		return httpResponse(http.StatusFound, `redirect-body-with-user-access-token-never-forward`, map[string]string{"Location": "https://attacker.invalid/collect"}), nil
	})
	client, _ := NewClient(cfg, staticAppTokens{token: testAppToken()}, transport, &fixedClock{now: time.Now()})
	_, err := client.VerifyAuthenticatedUserRaw(context.Background(), token)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Class != APIErrorRedirect || calls.Load() != 1 {
		t.Fatalf("redirect result calls=%d err=%v", calls.Load(), err)
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "attacker") || strings.Contains(err.Error(), "redirect-body") {
		t.Fatalf("redirect details leaked: %v", err)
	}
}

func TestClientClassifiesRateLimitsWithoutLeakingBodies(t *testing.T) {
	cfg := validTestConfig(t)
	now := time.Date(2026, 8, 9, 5, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		status    int
		headers   map[string]string
		wantClass APIErrorClass
		wantRetry time.Time
	}{
		{name: "retry after", status: http.StatusTooManyRequests, headers: map[string]string{"Retry-After": "120", "X-GitHub-Request-Id": "ABCD:1234"}, wantClass: APIErrorRateLimit, wantRetry: now.Add(2 * time.Minute)},
		{name: "primary reset", status: http.StatusForbidden, headers: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1786251900"}, wantClass: APIErrorRateLimit, wantRetry: time.Unix(1786251900, 0).UTC()},
		{name: "forbidden", status: http.StatusForbidden, wantClass: APIErrorForbidden},
		{name: "transient", status: http.StatusBadGateway, wantClass: APIErrorTransient},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			credentialMarker := "user-access-token-secret-marker"
			bodyMarker := "provider-error-body-secret-marker"
			transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
				return httpResponse(test.status, bodyMarker+credentialMarker, test.headers), nil
			})
			client, _ := NewClient(cfg, staticAppTokens{token: testAppToken()}, transport, &fixedClock{now: now})
			_, err := client.VerifyAuthenticatedUserRaw(context.Background(), credentialMarker)
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Class != test.wantClass || !apiErr.RetryAt.Equal(test.wantRetry) {
				t.Fatalf("api error=%#v err=%v", apiErr, err)
			}
			if strings.Contains(err.Error(), credentialMarker) || strings.Contains(err.Error(), bodyMarker) {
				t.Fatalf("provider error leaked credential/body: %v", err)
			}
		})
	}
}

func TestClientRejectsDuplicateTrailingAndOversizedJSON(t *testing.T) {
	cfg := validTestConfig(t)
	tests := map[string]string{
		"duplicate": `{"id":900,"id":901,"login":"octocat","type":"User"}`,
		"trailing":  `{"id":900,"login":"octocat","type":"User"} {}`,
		"malformed": `{"id":"not-a-number","login":"octocat","type":"User"}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			client, _ := NewClient(cfg, staticAppTokens{token: testAppToken()}, roundTripFunc(func(*http.Request) (*http.Response, error) {
				return httpResponse(http.StatusOK, body, nil), nil
			}), &fixedClock{now: time.Now()})
			if _, err := client.VerifyAuthenticatedUserRaw(context.Background(), "valid-user-access-token"); !errors.Is(err, ErrProviderResponse) {
				t.Fatalf("expected closed JSON rejection, got %v", err)
			}
		})
	}
	cfg.MaxResponseBytes = 1024
	client, _ := NewClient(cfg, staticAppTokens{token: testAppToken()}, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return httpResponse(http.StatusOK, strings.Repeat("x", 1025), nil), nil
	}), &fixedClock{now: time.Now()})
	if _, err := client.VerifyAuthenticatedUserRaw(context.Background(), "valid-user-access-token"); !errors.Is(err, ErrProviderResponse) {
		t.Fatalf("oversized response accepted: %v", err)
	}
}

func TestClientBoundsTimeoutAndRedactsTransportErrors(t *testing.T) {
	cfg := validTestConfig(t)
	secretMarker := "transport-user-access-secret"
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, errors.New("malicious transport included " + request.Header.Get("Authorization"))
	})
	client, _ := NewClient(cfg, staticAppTokens{token: testAppToken()}, transport, &fixedClock{now: time.Now()})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	_, err := client.VerifyAuthenticatedUserRaw(ctx, secretMarker)
	if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), secretMarker) {
		t.Fatalf("timeout/leak result: %v", err)
	}
	transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, errors.New("credential was " + request.Header.Get("Authorization"))
	})
	client, _ = NewClient(cfg, staticAppTokens{token: testAppToken()}, transport, &fixedClock{now: time.Now()})
	_, err = client.VerifyAuthenticatedUserRaw(context.Background(), secretMarker)
	if !errors.Is(err, ErrTransport) || strings.Contains(err.Error(), secretMarker) {
		t.Fatalf("transport error leaked: %v", err)
	}
}

func TestClientDiscardsInvalidRequestIDs(t *testing.T) {
	cfg := validTestConfig(t)
	client, _ := NewClient(cfg, staticAppTokens{token: testAppToken()}, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusInternalServerError, Header: http.Header{"X-Github-Request-Id": {"ok\r\nsecret"}}, Body: io.NopCloser(strings.NewReader("ignored"))}, nil
	}), &fixedClock{now: time.Now()})
	_, err := client.VerifyAuthenticatedUserRaw(context.Background(), "valid-user-access-token")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.RequestID != "" {
		t.Fatalf("unsafe request id retained: %#v", apiErr)
	}
}
