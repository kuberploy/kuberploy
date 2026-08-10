package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client is a bounded GitHub REST client for the small set of GitHub App
// operations used by Kuberploy. It never follows redirects, because doing so
// could forward a bearer credential to an unintended origin.
type Client struct {
	config    Config
	baseURL   *url.URL
	http      *http.Client
	appTokens AppTokenSource
	clock     Clock
}

func NewClient(cfg Config, appTokens AppTokenSource, transport http.RoundTripper, clock Clock) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if appTokens == nil {
		return nil, fmt.Errorf("%w: app token source is required", ErrInvalidConfig)
	}
	cfg.MaximumTokenPermissions = clonePermissions(cfg.MaximumTokenPermissions)
	baseURL, err := url.Parse(cfg.APIBaseURL)
	if err != nil {
		return nil, ErrInvalidConfig
	}
	if transport == nil {
		transport = defaultHTTPTransport(cfg.RequestTimeout)
	}
	httpClient := &http.Client{
		Transport: transport,
		Timeout:   cfg.RequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &Client{config: cfg, baseURL: baseURL, http: httpClient, appTokens: appTokens, clock: clockOrSystem(clock)}, nil
}

func defaultHTTPTransport(timeout time.Duration) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: min(timeout, 5*time.Second), KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = min(timeout, 5*time.Second)
	transport.ResponseHeaderTimeout = timeout
	transport.ExpectContinueTimeout = time.Second
	transport.MaxIdleConnsPerHost = 10
	return transport
}

func (c *Client) doJSON(
	ctx context.Context,
	method string,
	auth Credential,
	segments []string,
	query url.Values,
	requestBody any,
	expectedStatus int,
	responseBody any,
) error {
	if auth.empty() || !validRawCredential(auth.Reveal()) {
		return ErrTransport
	}
	endpoint, err := c.endpoint(segments, query)
	if err != nil {
		return ErrTransport
	}
	var body io.Reader
	if requestBody != nil {
		encoded, marshalErr := json.Marshal(requestBody)
		if marshalErr != nil || len(encoded) > 64<<10 {
			return ErrTransport
		}
		body = bytes.NewReader(encoded)
	}
	requestContext, cancel := context.WithTimeout(ctx, c.config.RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestContext, method, endpoint.String(), body)
	if err != nil {
		return ErrTransport
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", c.config.APIVersion)
	req.Header.Set("User-Agent", c.config.UserAgent)
	req.Header.Set("Authorization", "Bearer "+auth.Reveal())
	if requestBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	req.Header.Del("Authorization")
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if requestContext.Err() != nil {
			return context.DeadlineExceeded
		}
		return ErrTransport
	}
	if resp == nil || resp.Body == nil {
		return ErrTransport
	}
	defer resp.Body.Close()
	if resp.StatusCode != expectedStatus {
		_, _ = io.CopyN(io.Discard, resp.Body, 64<<10)
		return classifyAPIError(resp.StatusCode, resp.Header, c.clock.Now().UTC())
	}
	if responseBody == nil {
		_, _ = io.CopyN(io.Discard, resp.Body, 64<<10)
		return nil
	}
	encoded, err := readResponseBounded(resp.Body, c.config.MaxResponseBytes)
	if err != nil || len(encoded) == 0 {
		return ErrProviderResponse
	}
	if err = decodeSingleJSON(encoded, responseBody); err != nil {
		return ErrProviderResponse
	}
	return nil
}

func (c *Client) endpoint(segments []string, query url.Values) (*url.URL, error) {
	u := *c.baseURL
	decodedPath := strings.TrimRight(u.Path, "/")
	escapedPath := strings.TrimRight(u.EscapedPath(), "/")
	for _, segment := range segments {
		if segment == "" || strings.Contains(segment, "/") || strings.ContainsAny(segment, "\x00\r\n") {
			return nil, ErrTransport
		}
		decodedPath += "/" + segment
		escapedPath += "/" + url.PathEscape(segment)
	}
	u.Path = decodedPath
	u.RawPath = escapedPath
	u.RawQuery = query.Encode()
	u.Fragment = ""
	return &u, nil
}

func readResponseBounded(body io.Reader, maximum int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maximum+1))
	if err != nil || int64(len(data)) > maximum {
		return nil, ErrProviderResponse
	}
	return data, nil
}

func classifyAPIError(status int, headers http.Header, now time.Time) error {
	class := APIErrorUnknown
	retryAt := retryTime(headers, now)
	switch {
	case status >= 300 && status < 400:
		class = APIErrorRedirect
	case status == http.StatusUnauthorized:
		class = APIErrorAuth
	case status == http.StatusForbidden && (!retryAt.IsZero() || headers.Get("X-RateLimit-Remaining") == "0"):
		class = APIErrorRateLimit
		if retryAt.IsZero() {
			retryAt = now.Add(time.Minute)
		}
	case status == http.StatusForbidden:
		class = APIErrorForbidden
	case status == http.StatusNotFound:
		class = APIErrorNotFound
	case status == http.StatusConflict:
		class = APIErrorConflict
	case status == http.StatusUnprocessableEntity || status == http.StatusBadRequest:
		class = APIErrorInvalid
	case status == http.StatusTooManyRequests:
		class = APIErrorRateLimit
		if retryAt.IsZero() {
			retryAt = now.Add(time.Minute)
		}
	case status >= 500:
		class = APIErrorTransient
	}
	return &APIError{StatusCode: status, Class: class, RetryAt: retryAt, RequestID: safeRequestID(headers.Get("X-GitHub-Request-Id"))}
}

func retryTime(headers http.Header, now time.Time) time.Time {
	if raw := headers.Get("Retry-After"); raw != "" {
		if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds >= 0 && seconds <= int64(7*24*time.Hour/time.Second) {
			return now.Add(time.Duration(seconds) * time.Second)
		}
		if parsed, err := http.ParseTime(raw); err == nil && parsed.After(now) && parsed.Before(now.Add(7*24*time.Hour)) {
			return parsed.UTC()
		}
	}
	if headers.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(headers.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			parsed := time.Unix(reset, 0).UTC()
			if parsed.After(now) && parsed.Before(now.Add(7*24*time.Hour)) {
				return parsed
			}
		}
	}
	return time.Time{}
}

func safeRequestID(value string) string {
	if len(value) > 128 {
		return ""
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == ':' || r == '-') {
			return ""
		}
	}
	return value
}

func validRawCredential(value string) bool {
	if len(value) < 16 || len(value) > 16<<10 {
		return false
	}
	for _, r := range value {
		if r <= 0x20 || r >= 0x7f {
			return false
		}
	}
	return true
}

func credentialFromRaw(value string) (Credential, error) {
	if !validRawCredential(value) {
		return Credential{}, ErrTransport
	}
	return newCredential(value), nil
}

func isAPIClass(err error, class APIErrorClass) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Class == class
}
