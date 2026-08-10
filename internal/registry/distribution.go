package registry

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/store"
)

const distributionManifestAccept = "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json"

var (
	ErrDistributionInvalidConfig         = errors.New("invalid managed registry configuration")
	ErrDistributionCredentialUnavailable = errors.New("managed registry credential is unavailable")
	ErrDistributionScopeMismatch         = errors.New("managed registry request is outside the bound target scope")
	ErrDistributionManifestUnconfirmed   = errors.New("managed registry manifest deletion could not be confirmed")
	ErrDistributionRequest               = errors.New("managed registry request failed")
)

// DistributionErrorClass is a stable, credential-free classification of a
// failed Distribution request. Provider response bodies and redirect targets
// are deliberately never retained.
type DistributionErrorClass string

const (
	DistributionErrorRedirect         DistributionErrorClass = "redirect"
	DistributionErrorAuthentication   DistributionErrorClass = "authentication"
	DistributionErrorForbidden        DistributionErrorClass = "forbidden"
	DistributionErrorUnsupported      DistributionErrorClass = "unsupported"
	DistributionErrorConflict         DistributionErrorClass = "conflict"
	DistributionErrorRateLimit        DistributionErrorClass = "rate_limit"
	DistributionErrorTransient        DistributionErrorClass = "transient"
	DistributionErrorInvalidResponse  DistributionErrorClass = "invalid_response"
	DistributionErrorResponseTooLarge DistributionErrorClass = "response_too_large"
	DistributionErrorTransport        DistributionErrorClass = "transport"
	DistributionErrorUnknown          DistributionErrorClass = "unknown"
)

// DistributionError contains only safe scheduling metadata. It never includes
// a URL, credential, request/response body, or Location header.
type DistributionError struct {
	StatusCode int
	Class      DistributionErrorClass
	RetryAt    time.Time
	RequestID  string
}

func (e *DistributionError) Error() string {
	if e == nil || e.StatusCode == 0 {
		return "managed registry request failed"
	}
	return fmt.Sprintf("managed registry request failed with HTTP %d (%s)", e.StatusCode, e.Class)
}

func (e *DistributionError) Unwrap() error { return ErrDistributionRequest }

func (e *DistributionError) Retryable() bool {
	return e != nil && (e.Class == DistributionErrorRateLimit || e.Class == DistributionErrorTransient || e.Class == DistributionErrorTransport)
}

// DistributionAuthorization is a short-lived, caller-owned authorization
// value. Credential sources must return a fresh value for every call. The
// client erases its backing bytes immediately after building the request.
type DistributionAuthorization struct {
	scheme string
	value  []byte
}

// NewDistributionBearerAuthorization copies a bounded bearer token. It does
// not retain the caller's input slice.
func NewDistributionBearerAuthorization(token []byte) (DistributionAuthorization, error) {
	if !validDistributionBearer(token) {
		return DistributionAuthorization{}, ErrDistributionCredentialUnavailable
	}
	return DistributionAuthorization{scheme: "Bearer", value: append([]byte(nil), token...)}, nil
}

// NewDistributionBasicAuthorization copies and encodes a username/password
// pair. Username is deliberately restricted because it becomes header data.
func NewDistributionBasicAuthorization(username string, password []byte) (DistributionAuthorization, error) {
	if username == "" || len(username) > 256 || strings.ContainsAny(username, ":\x00\r\n") || !validDistributionSecret(password) {
		return DistributionAuthorization{}, ErrDistributionCredentialUnavailable
	}
	pair := make([]byte, 0, len(username)+1+len(password))
	pair = append(pair, username...)
	pair = append(pair, ':')
	pair = append(pair, password...)
	encoded := make([]byte, base64.StdEncoding.EncodedLen(len(pair)))
	if len(encoded) > 16<<10 {
		zeroBytes(pair)
		return DistributionAuthorization{}, ErrDistributionCredentialUnavailable
	}
	base64.StdEncoding.Encode(encoded, pair)
	zeroBytes(pair)
	return DistributionAuthorization{scheme: "Basic", value: encoded}, nil
}

func (a *DistributionAuthorization) header() (string, bool) {
	if !a.valid() {
		return "", false
	}
	return a.scheme + " " + string(a.value), true
}

func (a *DistributionAuthorization) valid() bool {
	if a == nil {
		return false
	}
	switch a.scheme {
	case "Basic":
		if !validDistributionSecret(a.value) {
			return false
		}
	case "Bearer":
		if !validDistributionBearer(a.value) {
			return false
		}
	default:
		return false
	}
	return true
}

func (a *DistributionAuthorization) destroy() {
	if a == nil {
		return
	}
	zeroBytes(a.value)
	a.value = nil
	a.scheme = ""
}

func validDistributionSecret(value []byte) bool {
	if len(value) < 1 || len(value) > 16<<10 {
		return false
	}
	for _, b := range value {
		if b == 0 || b == '\r' || b == '\n' {
			return false
		}
	}
	return true
}

func validDistributionBearer(value []byte) bool {
	if !validDistributionSecret(value) {
		return false
	}
	for _, b := range value {
		if b < '!' || b > '~' {
			return false
		}
	}
	return true
}

func zeroBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}

// DistributionCredentialSource brokers the internal delete credential by
// managed target ID. It is intentionally not represented by a tenant-provided
// path or by an external-target delete credential in the domain model.
type DistributionCredentialSource interface {
	Authorization(context.Context, string) (DistributionAuthorization, error)
}

type DistributionClientConfig struct {
	AllowPlainHTTP bool
	// ExpectedOrigin comes from trusted platform configuration, independently
	// of the persisted target record, and must match it exactly.
	ExpectedOrigin   string
	RequestTimeout   time.Duration
	MaxResponseBytes int64
	UserAgent        string
}

func DefaultDistributionClientConfig() DistributionClientConfig {
	return DistributionClientConfig{
		RequestTimeout:   10 * time.Second,
		MaxResponseBytes: 64 << 10,
		UserAgent:        "kuberploy-registry-controller/1",
	}
}

func (c DistributionClientConfig) validate() error {
	if c.RequestTimeout < time.Second || c.RequestTimeout > 30*time.Second ||
		c.MaxResponseBytes < 1024 || c.MaxResponseBytes > 1<<20 ||
		c.ExpectedOrigin == "" || strings.TrimSpace(c.ExpectedOrigin) != c.ExpectedOrigin ||
		len(c.UserAgent) < 1 || len(c.UserAgent) > 256 || strings.TrimSpace(c.UserAgent) != c.UserAgent || strings.ContainsAny(c.UserAgent, "\x00\r\n") {
		return ErrDistributionInvalidConfig
	}
	return nil
}

// ManifestDeleteOutcome distinguishes an API deletion from the idempotent
// case where the exact digest was already absent.
type ManifestDeleteOutcome string

const (
	ManifestDeleted        ManifestDeleteOutcome = "deleted"
	ManifestAlreadyMissing ManifestDeleteOutcome = "already_missing"
)

type ManifestDeleteResult struct {
	TargetID   string
	Repository string
	Digest     string
	Outcome    ManifestDeleteOutcome
}

type ManifestDeleter interface {
	ManagedTarget() domain.RegistryTarget
	DeleteManifest(context.Context, string, string, string) (ManifestDeleteResult, error)
}

// DistributionClient is bound to one managed registry origin and repository
// prefix. It never accepts an alternate URL and never follows redirects, which
// prevents an item or provider response from turning a delete into an SSRF.
type DistributionClient struct {
	target      domain.RegistryTarget
	baseURL     *url.URL
	credentials DistributionCredentialSource
	http        *http.Client
	config      DistributionClientConfig
	now         func() time.Time
}

var _ ManifestDeleter = (*DistributionClient)(nil)

func NewDistributionClient(target domain.RegistryTarget, cfg DistributionClientConfig, credentials DistributionCredentialSource, transport http.RoundTripper) (*DistributionClient, error) {
	if target.Mode == domain.RegistryTargetExternal {
		return nil, store.ErrRegistryExternalLifecycle
	}
	if target.Mode != domain.RegistryTargetManaged || ValidateTarget(target) != nil || !validSafeIdentity(target.ID) || credentials == nil || cfg.validate() != nil || !validRepository(target.RepositoryPrefix) {
		return nil, ErrDistributionInvalidConfig
	}
	baseURL, err := distributionBaseURL(target.Endpoint, cfg.AllowPlainHTTP)
	if err != nil {
		return nil, err
	}
	expectedOrigin, err := distributionBaseURL(cfg.ExpectedOrigin, cfg.AllowPlainHTTP)
	if err != nil || expectedOrigin.String() != baseURL.String() {
		return nil, ErrDistributionInvalidConfig
	}
	if transport == nil {
		transport = defaultDistributionTransport(cfg.RequestTimeout)
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   cfg.RequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &DistributionClient{
		target:      target,
		baseURL:     baseURL,
		credentials: credentials,
		http:        client,
		config:      cfg,
		now:         func() time.Time { return time.Now().UTC() },
	}, nil
}

func defaultDistributionTransport(timeout time.Duration) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = (&net.Dialer{Timeout: min(timeout, 5*time.Second), KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = min(timeout, 5*time.Second)
	transport.ResponseHeaderTimeout = timeout
	transport.ExpectContinueTimeout = time.Second
	transport.MaxIdleConnsPerHost = 4
	transport.MaxResponseHeaderBytes = 64 << 10
	return transport
}

func distributionBaseURL(raw string, allowHTTP bool) (*url.URL, error) {
	if raw == "" || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "\x00\r\n") {
		return nil, ErrDistributionInvalidConfig
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, ErrDistributionInvalidConfig
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && allowHTTP) {
		return nil, ErrDistributionInvalidConfig
	}
	if u.Hostname() == "" || strings.Contains(u.Hostname(), "%") {
		return nil, ErrDistributionInvalidConfig
	}
	if port := u.Port(); port != "" {
		value, parseErr := strconv.Atoi(port)
		if parseErr != nil || value < 1 || value > 65535 {
			return nil, ErrDistributionInvalidConfig
		}
	}
	u.Path = ""
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	return u, nil
}

func (c *DistributionClient) ManagedTarget() domain.RegistryTarget { return c.target }

func (c *DistributionClient) DeleteManifest(ctx context.Context, targetID, repository, digest string) (ManifestDeleteResult, error) {
	result := ManifestDeleteResult{TargetID: targetID, Repository: repository, Digest: digest}
	if c == nil || targetID == "" || targetID != c.target.ID || !c.repositoryAllowed(repository) || !validDigest(digest) {
		return result, ErrDistributionScopeMismatch
	}
	endpoint := c.manifestURL(repository, digest)
	status, header, err := c.request(ctx, http.MethodHead, endpoint)
	if err != nil {
		return result, err
	}
	if status == http.StatusNotFound {
		result.Outcome = ManifestAlreadyMissing
		return result, nil
	}
	if status != http.StatusOK {
		return result, classifyDistributionStatus(status, header, c.now())
	}
	if header.Get("Docker-Content-Digest") != digest {
		return result, ErrDistributionManifestUnconfirmed
	}

	status, header, err = c.request(ctx, http.MethodDelete, endpoint)
	if err != nil {
		return result, err
	}
	if status == http.StatusNotFound {
		result.Outcome = ManifestAlreadyMissing
		return result, nil
	}
	if status != http.StatusAccepted {
		return result, classifyDistributionStatus(status, header, c.now())
	}

	status, header, err = c.request(ctx, http.MethodHead, endpoint)
	if err != nil {
		return result, err
	}
	if status != http.StatusNotFound {
		if status == http.StatusOK {
			return result, ErrDistributionManifestUnconfirmed
		}
		return result, classifyDistributionStatus(status, header, c.now())
	}
	result.Outcome = ManifestDeleted
	return result, nil
}

func (c *DistributionClient) repositoryAllowed(repository string) bool {
	if !validRepository(repository) {
		return false
	}
	prefix := strings.TrimSuffix(c.target.RepositoryPrefix, "/")
	return repository == prefix || strings.HasPrefix(repository, prefix+"/")
}

func validRepository(repository string) bool {
	if len(repository) < 1 || len(repository) > 255 || strings.HasPrefix(repository, "/") || strings.HasSuffix(repository, "/") {
		return false
	}
	for _, component := range strings.Split(repository, "/") {
		if !validRepositoryComponent(component) {
			return false
		}
	}
	return true
}

func validRepositoryComponent(component string) bool {
	if component == "" {
		return false
	}
	isAlphaNumeric := func(b byte) bool { return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' }
	i := 0
	for i < len(component) {
		if !isAlphaNumeric(component[i]) {
			return false
		}
		for i < len(component) && isAlphaNumeric(component[i]) {
			i++
		}
		if i == len(component) {
			return true
		}
		switch component[i] {
		case '.':
			i++
		case '_':
			i++
			if i < len(component) && component[i] == '_' {
				i++
			}
		case '-':
			for i < len(component) && component[i] == '-' {
				i++
			}
		default:
			return false
		}
		if i == len(component) || !isAlphaNumeric(component[i]) {
			return false
		}
	}
	return true
}

func (c *DistributionClient) manifestURL(repository, digest string) string {
	u := *c.baseURL
	segments := append([]string{"v2"}, strings.Split(repository, "/")...)
	segments = append(segments, "manifests", digest)
	decoded := ""
	escaped := ""
	for _, segment := range segments {
		decoded += "/" + segment
		escaped += "/" + url.PathEscape(segment)
	}
	u.Path = decoded
	u.RawPath = escaped
	return u.String()
}

func (c *DistributionClient) request(ctx context.Context, method, endpoint string) (int, http.Header, error) {
	requestContext, cancel := context.WithTimeout(ctx, c.config.RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestContext, method, endpoint, nil)
	if err != nil {
		return 0, nil, &DistributionError{Class: DistributionErrorTransport}
	}
	authorization, err := c.credentials.Authorization(requestContext, c.target.ID)
	if err != nil {
		authorization.destroy()
		if ctx.Err() != nil {
			return 0, nil, ctx.Err()
		}
		if requestContext.Err() != nil {
			return 0, nil, context.DeadlineExceeded
		}
		return 0, nil, ErrDistributionCredentialUnavailable
	}
	header, ok := authorization.header()
	if !ok {
		authorization.destroy()
		return 0, nil, ErrDistributionCredentialUnavailable
	}
	req.Header.Set("Authorization", header)
	authorization.destroy()
	req.Header.Set("Accept", distributionManifestAccept)
	req.Header.Set("User-Agent", c.config.UserAgent)
	resp, err := c.http.Do(req)
	req.Header.Del("Authorization")
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if ctx.Err() != nil {
			return 0, nil, ctx.Err()
		}
		if requestContext.Err() != nil {
			return 0, nil, context.DeadlineExceeded
		}
		return 0, nil, &DistributionError{Class: DistributionErrorTransport}
	}
	if resp == nil || resp.Body == nil {
		return 0, nil, &DistributionError{Class: DistributionErrorInvalidResponse}
	}
	defer resp.Body.Close()
	if err = discardDistributionBody(resp.Body, c.config.MaxResponseBytes); err != nil {
		if ctx.Err() != nil {
			return 0, nil, ctx.Err()
		}
		if requestContext.Err() != nil {
			return 0, nil, context.DeadlineExceeded
		}
		return 0, nil, err
	}
	return resp.StatusCode, boundedDistributionHeaders(resp.Header), nil
}

func boundedDistributionHeaders(headers http.Header) http.Header {
	out := make(http.Header, 3)
	for _, name := range []string{"Docker-Content-Digest", "Retry-After", "X-Request-Id"} {
		values := headers.Values(name)
		if len(values) == 1 && len(values[0]) <= 256 {
			out.Set(name, values[0])
		}
	}
	return out
}

func discardDistributionBody(body io.Reader, maximum int64) error {
	read, err := io.Copy(io.Discard, io.LimitReader(body, maximum+1))
	if err != nil {
		return &DistributionError{Class: DistributionErrorInvalidResponse}
	}
	if read > maximum {
		return &DistributionError{Class: DistributionErrorResponseTooLarge}
	}
	return nil
}

func classifyDistributionStatus(status int, headers http.Header, now time.Time) error {
	class := DistributionErrorUnknown
	switch {
	case status >= 300 && status < 400:
		class = DistributionErrorRedirect
	case status == http.StatusUnauthorized:
		class = DistributionErrorAuthentication
	case status == http.StatusForbidden:
		class = DistributionErrorForbidden
	case status == http.StatusMethodNotAllowed || status == http.StatusBadRequest:
		class = DistributionErrorUnsupported
	case status == http.StatusConflict:
		class = DistributionErrorConflict
	case status == http.StatusTooManyRequests:
		class = DistributionErrorRateLimit
	case status == http.StatusRequestTimeout:
		class = DistributionErrorTransient
	case status >= 500:
		class = DistributionErrorTransient
	}
	return &DistributionError{
		StatusCode: status,
		Class:      class,
		RetryAt:    distributionRetryAt(headers.Get("Retry-After"), now),
		RequestID:  safeDistributionRequestID(headers.Get("X-Request-Id")),
	}
}

func distributionRetryAt(value string, now time.Time) time.Time {
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 && seconds <= int64(24*time.Hour/time.Second) {
		return now.Add(time.Duration(seconds) * time.Second).UTC()
	}
	if parsed, err := http.ParseTime(value); err == nil && parsed.After(now) && parsed.Before(now.Add(24*time.Hour)) {
		return parsed.UTC()
	}
	return time.Time{}
}

func safeDistributionRequestID(value string) string {
	if len(value) > 128 {
		return ""
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == ':' || r == '-' || r == '_') {
			return ""
		}
	}
	return value
}
