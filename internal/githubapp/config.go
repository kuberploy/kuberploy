package githubapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// CurrentAPIVersion is the current GitHub REST API version used by this
	// package. Changing it is an explicit compatibility change, not tenant input.
	CurrentAPIVersion = "2026-03-10"

	defaultAPIBaseURL      = "https://api.github.com"
	defaultUserAgent       = "kuberploy-github-app"
	defaultRequestTimeout  = 8 * time.Second
	defaultResponseLimit   = int64(2 << 20)
	defaultWebhookLimit    = int64(2 << 20)
	defaultReplayWindow    = 24 * time.Hour
	defaultStateTTL        = 10 * time.Minute
	defaultHandoffTTL      = 5 * time.Minute
	defaultJWTLifetime     = 9 * time.Minute
	defaultJWTBackdate     = 60 * time.Second
	maximumGitHubBodyBytes = int64(25 << 20)
)

var (
	secretNamePattern       = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`)
	secretKeyPattern        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`)
	clientIDPattern         = regexp.MustCompile(`^[A-Za-z0-9_-]{3,128}$`)
	permissionPattern       = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	hardTokenPermissionCaps = Permissions{
		"metadata":       PermissionRead,
		"contents":       PermissionWrite,
		"administration": PermissionRead,
		"actions":        PermissionRead,
		"pull_requests":  PermissionWrite,
		"checks":         PermissionWrite,
		"deployments":    PermissionWrite,
		"statuses":       PermissionWrite,
	}
)

// SecretRef is an opaque, operator-configured reference understood by the
// injected SecretReader. It is intentionally not a file path and contains no
// namespace: the reader itself owns and enforces its namespace boundary.
type SecretRef struct {
	Name string
	Key  string
}

func (r SecretRef) validate(label string) error {
	if !secretNamePattern.MatchString(r.Name) || !secretKeyPattern.MatchString(r.Key) ||
		strings.Contains(r.Name, "..") || strings.Contains(r.Key, "..") ||
		strings.ContainsAny(r.Name+r.Key, `/\\`) {
		return fmt.Errorf("%w: %s secret reference is invalid", ErrInvalidConfig, label)
	}
	return nil
}

// SecretReader returns a fresh, caller-owned byte slice for the referenced
// value. Implementations must not interpret tenant-controlled data as a path.
type SecretReader interface {
	ReadSecret(context.Context, SecretRef) ([]byte, error)
}

// Clock makes expiry and retry behavior deterministic in tests.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// PermissionLevel is a GitHub App permission level. Token requests are limited
// to read or write; "none" is accepted only in remote installation metadata.
type PermissionLevel string

const (
	PermissionNone  PermissionLevel = "none"
	PermissionRead  PermissionLevel = "read"
	PermissionWrite PermissionLevel = "write"
)

// Permissions is a set of GitHub App repository permission levels.
type Permissions map[string]PermissionLevel

// Config contains only GitHub-provider settings. The caller decides how these
// operator settings are represented by the wider application configuration.
type Config struct {
	AppID                   int64
	ClientID                string
	PrivateKeySecret        SecretRef
	WebhookSecret           SecretRef
	StateSigningSecret      SecretRef
	APIBaseURL              string
	APIVersion              string
	UserAgent               string
	RequestTimeout          time.Duration
	MaxResponseBytes        int64
	MaxWebhookBytes         int64
	WebhookReplayWindow     time.Duration
	StateTTL                time.Duration
	HandoffTTL              time.Duration
	JWTLifetime             time.Duration
	JWTBackdate             time.Duration
	MaximumTokenPermissions Permissions
}

// DefaultConfig supplies safe operational defaults. Identity, secret
// references, and allowed token permissions remain explicit.
func DefaultConfig() Config {
	return Config{
		APIBaseURL:          defaultAPIBaseURL,
		APIVersion:          CurrentAPIVersion,
		UserAgent:           defaultUserAgent,
		RequestTimeout:      defaultRequestTimeout,
		MaxResponseBytes:    defaultResponseLimit,
		MaxWebhookBytes:     defaultWebhookLimit,
		WebhookReplayWindow: defaultReplayWindow,
		StateTTL:            defaultStateTTL,
		HandoffTTL:          defaultHandoffTTL,
		JWTLifetime:         defaultJWTLifetime,
		JWTBackdate:         defaultJWTBackdate,
	}
}

// Validate rejects ambiguous or overly broad provider settings.
func (c Config) Validate() error {
	if c.AppID <= 0 || !clientIDPattern.MatchString(c.ClientID) {
		return fmt.Errorf("%w: app id and client id are required", ErrInvalidConfig)
	}
	for label, ref := range map[string]SecretRef{
		"private key": c.PrivateKeySecret,
		"webhook":     c.WebhookSecret,
		"state":       c.StateSigningSecret,
	} {
		if err := ref.validate(label); err != nil {
			return err
		}
	}
	u, err := url.Parse(c.APIBaseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%w: API base URL must be an HTTPS origin without credentials, query, or fragment", ErrInvalidConfig)
	}
	if u.Path != "" && (u.Path != strings.TrimRight(u.Path, "/") || strings.Contains(u.Path, "..") || !strings.HasPrefix(u.Path, "/")) {
		return fmt.Errorf("%w: API base URL path is not canonical", ErrInvalidConfig)
	}
	if c.APIVersion != CurrentAPIVersion {
		return fmt.Errorf("%w: API version must be %s", ErrInvalidConfig, CurrentAPIVersion)
	}
	if !validHeaderValue(c.UserAgent, 3, 128) || strings.TrimSpace(c.UserAgent) != c.UserAgent {
		return fmt.Errorf("%w: user agent is invalid", ErrInvalidConfig)
	}
	if c.RequestTimeout < time.Second || c.RequestTimeout > 30*time.Second {
		return fmt.Errorf("%w: request timeout must be between 1s and 30s", ErrInvalidConfig)
	}
	if c.MaxResponseBytes < 1024 || c.MaxResponseBytes > 16<<20 {
		return fmt.Errorf("%w: response limit must be between 1 KiB and 16 MiB", ErrInvalidConfig)
	}
	if c.MaxWebhookBytes < 1024 || c.MaxWebhookBytes > maximumGitHubBodyBytes {
		return fmt.Errorf("%w: webhook limit must be between 1 KiB and 25 MiB", ErrInvalidConfig)
	}
	if c.WebhookReplayWindow < time.Minute || c.WebhookReplayWindow > 7*24*time.Hour {
		return fmt.Errorf("%w: webhook replay window must be between 1 minute and 7 days", ErrInvalidConfig)
	}
	if c.StateTTL < time.Minute || c.StateTTL > 15*time.Minute {
		return fmt.Errorf("%w: state TTL must be between 1 and 15 minutes", ErrInvalidConfig)
	}
	if c.HandoffTTL < time.Minute || c.HandoffTTL > 10*time.Minute {
		return fmt.Errorf("%w: handoff TTL must be between 1 and 10 minutes", ErrInvalidConfig)
	}
	if c.JWTLifetime < time.Minute || c.JWTLifetime > 10*time.Minute {
		return fmt.Errorf("%w: JWT lifetime must be between 1 and 10 minutes", ErrInvalidConfig)
	}
	if c.JWTBackdate < 0 || c.JWTBackdate > 60*time.Second {
		return fmt.Errorf("%w: JWT backdate must be between 0 and 60 seconds", ErrInvalidConfig)
	}
	if len(c.MaximumTokenPermissions) == 0 || len(c.MaximumTokenPermissions) > 64 {
		return fmt.Errorf("%w: explicit maximum token permissions are required", ErrInvalidConfig)
	}
	if err := validatePermissions(c.MaximumTokenPermissions, false); err != nil {
		return fmt.Errorf("%w: maximum permissions: %v", ErrInvalidConfig, err)
	}
	if c.MaximumTokenPermissions["metadata"] != PermissionRead {
		return fmt.Errorf("%w: metadata permission must be explicitly limited to read", ErrInvalidConfig)
	}
	for name, level := range c.MaximumTokenPermissions {
		cap, allowed := hardTokenPermissionCaps[name]
		if !allowed || !permissionAllows(cap, level) {
			return fmt.Errorf("%w: permission %s exceeds the broker hard cap", ErrInvalidConfig, name)
		}
	}
	return nil
}

func validHeaderValue(s string, min, max int) bool {
	if len(s) < min || len(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

func validatePermissions(p Permissions, allowNone bool) error {
	for name, level := range p {
		if !permissionPattern.MatchString(name) {
			return errors.New("permission name is invalid")
		}
		switch level {
		case PermissionRead, PermissionWrite:
		case PermissionNone:
			if !allowNone {
				return errors.New("token request cannot contain none permission")
			}
		default:
			return errors.New("permission level is invalid")
		}
	}
	return nil
}

func clockOrSystem(c Clock) Clock {
	if c == nil {
		return systemClock{}
	}
	return c
}

func randomOrCrypto(r io.Reader) io.Reader {
	if r == nil {
		return cryptoRandomReader{}
	}
	return r
}
