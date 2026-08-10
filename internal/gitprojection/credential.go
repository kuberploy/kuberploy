package gitprojection

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"
)

const (
	gitHubAppUsername        = "x-access-token"
	maximumGitPasswordBytes  = 16 << 10
	minimumGitCredentialLife = 30 * time.Second
)

// GitCredential is short-lived process memory handed from a provider-scoped
// token issuer to one prepared repository. It must never be logged, encoded,
// persisted, placed in a URL, or copied into a process environment.
type GitCredential struct {
	Username  []byte    `json:"-"`
	Password  []byte    `json:"-"`
	ExpiresAt time.Time `json:"-"`
}

func (GitCredential) String() string   { return "GitCredential{redacted}" }
func (GitCredential) GoString() string { return "GitCredential{redacted}" }
func (GitCredential) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "GitCredential{redacted}")
}
func (GitCredential) LogValue() slog.Value {
	return slog.StringValue("GitCredential{redacted}")
}

func (c GitCredential) validate(now time.Time) error {
	return c.validateFor(now, minimumGitCredentialLife)
}

func (c GitCredential) validateFor(now time.Time, minimumLife time.Duration) error {
	if string(c.Username) != gitHubAppUsername || len(c.Password) < 16 || len(c.Password) > maximumGitPasswordBytes ||
		minimumLife < minimumGitCredentialLife || c.ExpiresAt.IsZero() || !c.ExpiresAt.After(now.UTC().Add(minimumLife)) || !printableCredential(c.Password) {
		return ErrInvalid
	}
	return nil
}

func (c *GitCredential) clear() {
	if c == nil {
		return
	}
	clear(c.Username)
	clear(c.Password)
	c.Username = nil
	c.Password = nil
	c.ExpiresAt = time.Time{}
}

func printableCredential(value []byte) bool {
	for _, character := range value {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

// GitCredentialProvider returns one repository-scoped credential. Ownership of
// the returned byte slices transfers to the caller, which clears them after
// the prepared repository closes or immediately on any setup failure.
type GitCredentialProvider interface {
	AcquireGitCredential(context.Context, Binding) (GitCredential, error)
}
