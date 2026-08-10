package githubapp

import (
	"errors"
	"fmt"
	"time"
)

var (
	ErrInvalidConfig           = errors.New("invalid GitHub App configuration")
	ErrSecretUnavailable       = errors.New("GitHub App secret is unavailable")
	ErrInvalidPrivateKey       = errors.New("invalid GitHub App private key")
	ErrInvalidState            = errors.New("invalid GitHub App state")
	ErrExpiredState            = errors.New("expired GitHub App state")
	ErrStateReplay             = errors.New("GitHub App state was already consumed")
	ErrInvalidHandoff          = errors.New("invalid GitHub App handoff token")
	ErrExpiredHandoff          = errors.New("expired GitHub App handoff token")
	ErrInvalidWebhook          = errors.New("invalid GitHub webhook")
	ErrWebhookTooLarge         = errors.New("GitHub webhook body is too large")
	ErrWebhookReplay           = errors.New("GitHub webhook delivery was already consumed")
	ErrInvalidTokenRequest     = errors.New("invalid GitHub installation token request")
	ErrScopeMismatch           = errors.New("GitHub credential scope does not match the request")
	ErrOwnershipMismatch       = errors.New("GitHub installation or repository ownership does not match")
	ErrRefDeleted              = errors.New("GitHub ref was deleted")
	ErrUnsupportedObjectFormat = errors.New("GitHub object id format is unsupported by the builder")
	ErrRepositoryProtection    = errors.New("GitHub repository protection is not ready")
	ErrProviderResponse        = errors.New("invalid response from GitHub")
	ErrTransport               = errors.New("GitHub request failed")
)

// APIErrorClass is a stable, credential-free classification of an unsuccessful
// GitHub API response. The response body and redirect target are never retained.
type APIErrorClass string

const (
	APIErrorRedirect  APIErrorClass = "redirect"
	APIErrorAuth      APIErrorClass = "authentication"
	APIErrorForbidden APIErrorClass = "forbidden"
	APIErrorNotFound  APIErrorClass = "not_found"
	APIErrorConflict  APIErrorClass = "conflict"
	APIErrorInvalid   APIErrorClass = "invalid_request"
	APIErrorRateLimit APIErrorClass = "rate_limit"
	APIErrorTransient APIErrorClass = "transient"
	APIErrorUnknown   APIErrorClass = "unknown"
)

// APIError intentionally exposes only scheduling-safe metadata. In particular,
// it never contains an authorization value, request body, response body, URL,
// or Location header.
type APIError struct {
	StatusCode int
	Class      APIErrorClass
	RetryAt    time.Time
	RequestID  string
}

func (e *APIError) Error() string {
	if e == nil {
		return "GitHub API request failed"
	}
	if e.StatusCode == 0 {
		return fmt.Sprintf("GitHub API request failed (%s)", e.Class)
	}
	return fmt.Sprintf("GitHub API request failed with HTTP %d (%s)", e.StatusCode, e.Class)
}

// Retryable reports whether retrying after RetryAt may succeed.
func (e *APIError) Retryable() bool {
	return e != nil && (e.Class == APIErrorRateLimit || e.Class == APIErrorTransient)
}
