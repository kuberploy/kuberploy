// Package registrycredentials owns self-service registry pull credentials:
// a project member types a registry host, username, and password once, and
// the raw material is sealed via the runtime-secret lifecycle and never
// stored in plaintext. Only public, non-secret metadata (the host, and the
// username — never the password) may be persisted.
package registrycredentials

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kuberploy/kuberploy/internal/secrets"
)

const (
	MaxHostLength       = 253
	MaxUsernameLength   = 256
	MaxPasswordBytes    = 8 << 10
	MaxEmailLength      = 254
	redactedMaterial    = "[REDACTED registry credential material]"
	dockerConfigJSONKey = ".dockerconfigjson"
)

var (
	ErrInvalid      = errors.New("invalid registry credential input")
	ErrNotFound     = errors.New("registry credential not found")
	ErrNotReady     = errors.New("registry credential is not ready")
	ErrConflict     = errors.New("registry credential conflict")
	ErrUnavailable  = errors.New("registry credential service unavailable")
	ErrMaterialGone = errors.New("registry credential material was already destroyed")
	ErrNoSerialize  = errors.New("registry credential material cannot be serialized")

	uuidRE  = regexp.MustCompile("^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$")
	labelRE = regexp.MustCompile("^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$")
	hostRE  = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,62})(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,62}))*(?::[0-9]{1,5})?$`)
)

// Reference is the complete registry-credential identity allowed in an
// Application's pull-credential selection. It deliberately contains no
// Kubernetes Secret name, provider object name, or credential material. The
// target Secret is always derived from the immutable binding and version
// after an exact scope/readiness check.
type Reference struct {
	BindingID string `json:"bindingId" yaml:"bindingId"`
	Name      string `json:"name" yaml:"name"`
	Version   int64  `json:"version" yaml:"version"`
}

func (r Reference) Validate() error {
	if !uuidRE.MatchString(r.BindingID) || !labelRE.MatchString(r.Name) || r.Version <= 0 {
		return ErrInvalid
	}
	return nil
}

// ResolvedReference is the metadata-only result consumed by the deployment
// materialization boundary. No caller may choose TargetSecretName.
type ResolvedReference struct {
	BindingID        string `json:"bindingId"`
	SecretVersionID  string `json:"secretVersionId"`
	Name             string `json:"name"`
	Version          int64  `json:"version"`
	Namespace        string `json:"namespace"`
	TargetSecretName string `json:"targetSecretName"`
	Host             string `json:"host"`
}

func (r ResolvedReference) Validate() error {
	if !uuidRE.MatchString(r.BindingID) || !uuidRE.MatchString(r.SecretVersionID) ||
		!labelRE.MatchString(r.Name) || r.Version <= 0 || !labelRE.MatchString(r.Namespace) ||
		!labelRE.MatchString(r.TargetSecretName) || !validHost(r.Host) {
		return ErrInvalid
	}
	return nil
}

// Material owns bounded request-local credential bytes. The service destroys
// them on every return path. It cannot be serialized or formatted into
// diagnostics, and the password is never exposed through any public field.
type Material struct {
	host      []byte
	username  []byte
	password  []byte
	email     []byte
	destroyed bool
}

func NewMaterial(host, username, password, email []byte) (*Material, error) {
	if !validHost(string(host)) || len(username) == 0 || len(username) > MaxUsernameLength ||
		len(password) == 0 || len(password) > MaxPasswordBytes ||
		(len(email) > 0 && (len(email) > MaxEmailLength || !strings.Contains(string(email), "@"))) ||
		!utf8.Valid(username) || !utf8.Valid(email) {
		return nil, ErrInvalid
	}
	return &Material{
		host: bytes.Clone(bytes.ToLower(host)), username: bytes.Clone(username),
		password: bytes.Clone(password), email: bytes.Clone(email),
	}, nil
}

func (m *Material) Destroy() {
	if m == nil || m.destroyed {
		return
	}
	clear(m.host)
	clear(m.username)
	clear(m.password)
	clear(m.email)
	m.host, m.username, m.password, m.email, m.destroyed = nil, nil, nil, nil, true
}

func (m *Material) MarshalJSON() ([]byte, error) { return nil, ErrNoSerialize }
func (m *Material) String() string               { return redactedMaterial }
func (m *Material) GoString() string             { return redactedMaterial }
func (m *Material) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, redactedMaterial)
}

// dockerConfigJSON builds the Kubernetes-standard .dockerconfigjson payload.
// The returned bytes are the only place the password ever appears in
// serialized form, and only transiently on the way into the sealed-secrets
// pipeline — never returned to a caller, never persisted by this package.
func (m *Material) dockerConfigJSON() ([]byte, error) {
	if m == nil || m.destroyed {
		return nil, ErrMaterialGone
	}
	authBytes := append(append(make([]byte, 0, len(m.username)+1+len(m.password)), m.username...), ':')
	authBytes = append(authBytes, m.password...)
	defer clear(authBytes)
	entry := map[string]string{
		"username": string(m.username),
		"password": string(m.password),
		"auth":     base64.StdEncoding.EncodeToString(authBytes),
	}
	if len(m.email) > 0 {
		entry["email"] = string(m.email)
	}
	config := map[string]any{"auths": map[string]any{string(m.host): entry}}
	encoded, err := json.Marshal(config)
	if err != nil {
		return nil, ErrInvalid
	}
	return encoded, nil
}

// Version is an immutable public attestation for one exact runtime-secret
// version. Only the host and username are public; the password never
// appears here. SecretContentFingerprint is keyed by the platform and is
// never returned to API callers.
type Version struct {
	BindingID                string    `json:"bindingId"`
	SecretVersionID          string    `json:"secretVersionId"`
	Number                   int64     `json:"number"`
	Host                     string    `json:"host"`
	Username                 string    `json:"username"`
	CreatedBy                string    `json:"createdBy"`
	CreatedAt                time.Time `json:"createdAt"`
	SecretContentFingerprint [32]byte  `json:"-"`
}

func (v Version) Validate() error {
	if !uuidRE.MatchString(v.BindingID) || !uuidRE.MatchString(v.SecretVersionID) || v.Number <= 0 ||
		!validHost(v.Host) || v.Username == "" || len(v.Username) > MaxUsernameLength || !utf8.ValidString(v.Username) ||
		!uuidRE.MatchString(v.CreatedBy) || v.CreatedAt.IsZero() || v.SecretContentFingerprint == [32]byte{} {
		return ErrInvalid
	}
	return nil
}

func (v Version) ValidateFor(binding secrets.Binding, version secrets.Version) error {
	if v.Validate() != nil || binding.Validate() != nil || version.Validate() != nil ||
		binding.ID != v.BindingID || binding.Purpose != secrets.PurposeRegistryPullCredential || binding.Provider != secrets.ProviderSealedSecrets ||
		version.ID != v.SecretVersionID || version.BindingID != binding.ID || version.Number != v.Number ||
		version.Provider != secrets.ProviderSealedSecrets || version.TargetSecretType != secrets.TargetSecretDockerConfigJSON || version.Artifact == nil ||
		version.Artifact.TargetSecretType != secrets.TargetSecretDockerConfigJSON || version.Artifact.ValidateFor(binding, version.Number) != nil ||
		subtle.ConstantTimeCompare(v.SecretContentFingerprint[:], version.ContentFingerprint[:]) != 1 {
		return ErrConflict
	}
	return nil
}

func validHost(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return value != "" && len(value) <= MaxHostLength && hostRE.MatchString(value)
}

func cloneVersion(value Version) Version { return value }

func safeText(value string, maximum int) bool {
	if len(value) == 0 || len(value) > maximum || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
