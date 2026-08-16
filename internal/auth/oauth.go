package auth

import (
	"context"
	"errors"
)

// Identity is keyed by Issuer and Subject. Email is profile metadata for a
// future SSO flow, never the durable external identity key.
type Identity struct{ Issuer, Subject, Email, DisplayName string }

// OAuthProvider keeps interactive identity provider details outside HTTP and
// domain code. A real GitHub App OAuth implementation can replace Disabled.
type OAuthProvider interface {
	AuthorizationURL(state, redirectURI string) (string, error)
	Exchange(ctx context.Context, code, redirectURI string) (Identity, error)
}

type Disabled struct{}

func (Disabled) AuthorizationURL(string, string) (string, error) {
	return "", errors.New("GitHub OAuth is not configured")
}
func (Disabled) Exchange(context.Context, string, string) (Identity, error) {
	return Identity{}, errors.New("GitHub OAuth is not configured")
}
