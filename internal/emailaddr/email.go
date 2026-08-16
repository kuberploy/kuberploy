// Package emailaddr contains the narrow email identity rules shared by the
// HTTP and persistence layers. Email is a local credential identifier; an
// external SSO provider remains identified by issuer and subject.
package emailaddr

import (
	"net/mail"
	"strings"
)

// Normalize validates a plain mailbox address and returns its canonical form.
// Display-name syntax is deliberately rejected: API callers must submit the
// address itself, not a value that could be rendered inconsistently.
func Normalize(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 254 || strings.ContainsAny(value, "\r\n<>") {
		return "", false
	}
	parsed, err := mail.ParseAddress(value)
	if err != nil || parsed.Address != value {
		return "", false
	}
	if !strings.Contains(value, "@") || strings.HasPrefix(value, "@") || strings.HasSuffix(value, "@") {
		return "", false
	}
	return strings.ToLower(value), true
}
