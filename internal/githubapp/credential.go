package githubapp

import (
	"encoding/json"
	"fmt"
	"io"
)

// Credential is an intentionally opaque authorization value. Reveal is named
// explicitly so provider calls stand out in review; formatting and JSON always
// redact it.
// The value is captured by a closure rather than stored as a reflectable string
// field. Go's recursive %#v formatting bypasses nested Stringer methods; a
// closure keeps that formatting path from reconstructing the credential.
type Credential struct{ reveal func() string }

func newCredential(value string) Credential {
	return Credential{reveal: func() string { return value }}
}

// Reveal returns the authorization value for immediate use in an outbound
// request. Callers must never persist, log, audit, or return it.
func (c Credential) Reveal() string {
	if c.reveal == nil {
		return ""
	}
	return c.reveal()
}

func (Credential) String() string   { return "[REDACTED]" }
func (Credential) GoString() string { return "githubapp.Credential([REDACTED])" }

// Format prevents reflective formatting of Credential fields nested in other
// structs from exposing the unexported backing string.
func (Credential) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "[REDACTED]")
}

func (Credential) MarshalJSON() ([]byte, error) { return json.Marshal("[REDACTED]") }

func (c Credential) empty() bool { return c.reveal == nil || c.reveal() == "" }
