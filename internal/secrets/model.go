// Package secrets owns the write-only runtime-secret lifecycle. Plaintext is
// consumed in process and is never part of any returned or persisted model.
package secrets

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	MaxMaterialKeys  = 64
	MaxValueBytes    = 64 << 10
	MaxMaterialBytes = 256 << 10
	MaxDeliveries    = 128
	fileRoot         = "/var/run/secrets/kuberploy/"
)

var (
	ErrInvalid                   = errors.New("invalid runtime secret input")
	ErrNotFound                  = errors.New("runtime secret record not found")
	ErrConflict                  = errors.New("runtime secret conflict")
	ErrReferenced                = errors.New("runtime secret is still referenced")
	ErrNotReady                  = errors.New("runtime secret provider is not ready")
	ErrProviderMismatch          = errors.New("runtime secret provider observation mismatch")
	ErrProviderOperation         = errors.New("runtime secret provider operation failed")
	ErrRuntimeLeaseLost          = errors.New("runtime secret reconciliation lease was lost")
	ErrRuntimeUnavailable        = errors.New("runtime secret reconciliation runtime is unavailable")
	ErrFingerprintKeyUnavailable = errors.New("runtime secret fingerprint key is unavailable")
	ErrPlaintextDestroyed        = errors.New("runtime secret plaintext was already destroyed")
	ErrPlaintextSerialization    = errors.New("runtime secret plaintext cannot be serialized")

	uuidRE        = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	dnsLabelRE    = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$`)
	kubeNameRE    = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`)
	secretKeyRE   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,253}$`)
	envNameRE     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,252}$`)
	digestRE      = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	revisionRE    = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64}|sha256:[0-9a-f]{64})$`)
	safeCodeRE    = regexp.MustCompile(`^[a-z][a-z0-9.-]{0,62}$`)
	keyIDRE       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	requestIDRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	idempotencyRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$`)
)

type ProviderKind string

const (
	ProviderExternalSecrets ProviderKind = "external-secrets"
	ProviderSealedSecrets   ProviderKind = "sealed-secrets"
)

func (k ReferenceKind) valid() bool {
	return k == ReferenceGitCurrent || k == ReferenceCurrentRelease || k == ReferenceRetainedRelease
}

func (p ProviderKind) valid() bool {
	return p == ProviderExternalSecrets || p == ProviderSealedSecrets
}

type BindingState string

const (
	BindingProvisioning BindingState = "provisioning"
	BindingReady        BindingState = "ready"
	BindingDeleting     BindingState = "deleting"
	BindingDeleted      BindingState = "deleted"
	BindingFailed       BindingState = "failed"
)

// BindingPurpose separates general workload values from certificate material.
// A purpose is immutable for the lifetime of a binding so the generic runtime
// secret API cannot list, rotate, bind, or delete an ingress private key.
type BindingPurpose string

const (
	PurposeRuntimeSecret  BindingPurpose = "runtime-secret"
	PurposeTLSCertificate BindingPurpose = "tls-certificate"
)

func (p BindingPurpose) valid() bool {
	return p == PurposeRuntimeSecret || p == PurposeTLSCertificate
}

type VersionState string

const (
	VersionStaging           VersionState = "staging"
	VersionAwaitingReadiness VersionState = "awaiting-readiness"
	VersionActive            VersionState = "active"
	VersionRetained          VersionState = "retained"
	VersionFailed            VersionState = "failed"
	VersionDeleted           VersionState = "deleted"
)

// TargetSecretType is the immutable Kubernetes Secret type produced for a
// runtime-secret version. Ordinary application values are Opaque. The TLS
// type is reserved for the certificate service, which validates the
// certificate/private-key pair before material reaches the provider.
type TargetSecretType string

const (
	TargetSecretOpaque TargetSecretType = "Opaque"
	TargetSecretTLS    TargetSecretType = "kubernetes.io/tls"
)

func (t TargetSecretType) valid() bool {
	return t == TargetSecretOpaque || t == TargetSecretTLS
}

// Scope is the complete tenant and workload boundary. OrganizationID maps to
// a Kuberploy team when the project is team-owned and is empty for a personal
// or platform-owned project. PostgreSQL verifies that exact ownership choice,
// every descendant relationship, and the environment's Kubernetes namespace.
type Scope struct {
	OrganizationID string `json:"organizationId"`
	ProjectID      string `json:"projectId"`
	EnvironmentID  string `json:"environmentId"`
	ApplicationID  string `json:"applicationId"`
	Namespace      string `json:"namespace"`
}

func (s Scope) Validate() error {
	if (s.OrganizationID != "" && !uuidRE.MatchString(s.OrganizationID)) || !uuidRE.MatchString(s.ProjectID) ||
		!uuidRE.MatchString(s.EnvironmentID) || !uuidRE.MatchString(s.ApplicationID) ||
		!dnsLabelRE.MatchString(s.Namespace) {
		return ErrInvalid
	}
	return nil
}

type DeliveryKind string

const (
	DeliveryEnvironment DeliveryKind = "environment"
	DeliveryFile        DeliveryKind = "file"
)

// Delivery is an exact, immutable projection of one material key. File paths
// are confined to Kuberploy's read-only runtime-secret mount.
type Delivery struct {
	SourceKey       string       `json:"sourceKey"`
	Kind            DeliveryKind `json:"kind"`
	EnvironmentName string       `json:"environmentName,omitempty"`
	FilePath        string       `json:"filePath,omitempty"`
	FileMode        uint32       `json:"fileMode,omitempty"`
}

func (d Delivery) Validate() error {
	if !secretKeyRE.MatchString(d.SourceKey) {
		return ErrInvalid
	}
	switch d.Kind {
	case DeliveryEnvironment:
		if !envNameRE.MatchString(d.EnvironmentName) || d.FilePath != "" || d.FileMode != 0 {
			return ErrInvalid
		}
	case DeliveryFile:
		if d.EnvironmentName != "" || (d.FileMode != 0o400 && d.FileMode != 0o440) || !validFilePath(d.FilePath) {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

func validFilePath(value string) bool {
	if len(value) <= len(fileRoot) || len(value) > 1024 || !strings.HasPrefix(value, fileRoot) ||
		strings.Contains(value, "\\") || strings.Contains(value, "//") || path.Clean(value) != value {
		return false
	}
	for _, part := range strings.Split(strings.TrimPrefix(value, fileRoot), "/") {
		if part == "" || part == "." || part == ".." || len(part) > 253 || !secretKeyRE.MatchString(part) {
			return false
		}
	}
	return true
}

func normalizeDeliveries(input []Delivery) ([]Delivery, error) {
	if len(input) == 0 || len(input) > MaxDeliveries {
		return nil, ErrInvalid
	}
	out := append([]Delivery(nil), input...)
	for _, delivery := range out {
		if delivery.Validate() != nil {
			return nil, ErrInvalid
		}
	}
	slices.SortFunc(out, func(a, b Delivery) int {
		left := string(a.Kind) + "\x00" + a.EnvironmentName + "\x00" + a.FilePath + "\x00" + a.SourceKey
		right := string(b.Kind) + "\x00" + b.EnvironmentName + "\x00" + b.FilePath + "\x00" + b.SourceKey
		return strings.Compare(left, right)
	})
	environments, files := map[string]struct{}{}, map[string]struct{}{}
	for _, delivery := range out {
		var set map[string]struct{}
		var destination string
		if delivery.Kind == DeliveryEnvironment {
			set, destination = environments, delivery.EnvironmentName
		} else {
			set, destination = files, delivery.FilePath
		}
		if _, duplicate := set[destination]; duplicate {
			return nil, ErrInvalid
		}
		set[destination] = struct{}{}
	}
	return out, nil
}

type materialEntry struct {
	key   string
	value []byte
}

// Material owns bounded plaintext bytes. NewMaterial copies its input;
// Destroy zeros those copies. MarshalJSON always fails and formatting is
// redacted, so the type cannot accidentally enter a response or structured log.
type Material struct {
	entries   []materialEntry
	total     int
	destroyed bool
}

func NewMaterial(values map[string][]byte) (*Material, error) {
	if len(values) == 0 || len(values) > MaxMaterialKeys {
		return nil, ErrInvalid
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	material := &Material{entries: make([]materialEntry, 0, len(keys))}
	for _, key := range keys {
		value := values[key]
		if !secretKeyRE.MatchString(key) || len(value) == 0 || len(value) > MaxValueBytes || material.total+len(value) > MaxMaterialBytes {
			material.Destroy()
			return nil, ErrInvalid
		}
		material.entries = append(material.entries, materialEntry{key: key, value: append([]byte(nil), value...)})
		material.total += len(value)
	}
	return material, nil
}

// WithEntries exposes request-local copies to a trusted provider callback and
// erases each copy immediately. Providers must not retain or log the bytes.
func (m *Material) WithEntries(fn func(string, []byte) error) error {
	if m == nil || m.destroyed || fn == nil {
		return ErrPlaintextDestroyed
	}
	for _, entry := range m.entries {
		value := append([]byte(nil), entry.value...)
		err := func() error {
			defer clear(value)
			return fn(entry.key, value)
		}()
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *Material) Destroy() {
	if m == nil || m.destroyed {
		return
	}
	for i := range m.entries {
		clear(m.entries[i].value)
		m.entries[i].value = nil
	}
	m.entries = nil
	m.total = 0
	m.destroyed = true
}

func (m *Material) MarshalJSON() ([]byte, error) { return nil, ErrPlaintextSerialization }
func (m *Material) String() string               { return "[REDACTED runtime-secret material]" }
func (m *Material) GoString() string             { return m.String() }

func (m *Material) keys() ([]string, error) {
	if m == nil || m.destroyed {
		return nil, ErrPlaintextDestroyed
	}
	keys := make([]string, len(m.entries))
	for i := range m.entries {
		keys[i] = m.entries[i].key
	}
	return keys, nil
}

type FingerprintKey struct {
	ID    string
	Bytes []byte
}

func (k *FingerprintKey) destroy() {
	if k == nil {
		return
	}
	clear(k.Bytes)
	k.Bytes = nil
}

type FingerprintKeyProvider interface {
	// ActiveKey returns caller-owned bytes; Service erases them before return.
	ActiveKey(context.Context) (FingerprintKey, error)
}

func fingerprint(key FingerprintKey, label string, scope Scope, bindingName string, provider ProviderKind, targetType TargetSecretType, bindingID string, expected int64, deliveries []Delivery, material *Material) ([32]byte, error) {
	if !keyIDRE.MatchString(key.ID) || len(key.Bytes) < 32 || len(key.Bytes) > 128 || material == nil || material.destroyed {
		return [32]byte{}, ErrInvalid
	}
	if !targetType.valid() || targetType == TargetSecretTLS && provider != ProviderSealedSecrets {
		return [32]byte{}, ErrInvalid
	}
	mac := hmac.New(sha256.New, key.Bytes)
	writeMACString(mac, "kuberploy-runtime-secret-v1")
	writeMACString(mac, label)
	for _, value := range []string{scope.OrganizationID, scope.ProjectID, scope.EnvironmentID, scope.ApplicationID, scope.Namespace, bindingName, string(provider), bindingID} {
		writeMACString(mac, value)
	}
	// Keep existing Opaque idempotency fingerprints byte-for-byte compatible
	// across this schema extension. TLS versions add an unambiguous framed
	// domain before deliveries and material.
	if targetType == TargetSecretTLS {
		writeMACString(mac, "target-secret-type")
		writeMACString(mac, string(targetType))
	}
	var number [8]byte
	binary.BigEndian.PutUint64(number[:], uint64(expected))
	_, _ = mac.Write(number[:])
	for _, delivery := range deliveries {
		writeMACString(mac, delivery.SourceKey)
		writeMACString(mac, string(delivery.Kind))
		writeMACString(mac, delivery.EnvironmentName)
		writeMACString(mac, delivery.FilePath)
		binary.BigEndian.PutUint32(number[:4], delivery.FileMode)
		_, _ = mac.Write(number[:4])
	}
	for _, entry := range material.entries {
		writeMACString(mac, entry.key)
		writeMACBytes(mac, entry.value)
	}
	var out [32]byte
	copy(out[:], mac.Sum(nil))
	return out, nil
}

type hashWriter interface{ Write([]byte) (int, error) }

func writeMACString(w hashWriter, value string) { writeMACBytes(w, []byte(value)) }
func writeMACBytes(w hashWriter, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = w.Write(size[:])
	_, _ = w.Write(value)
}

type Binding struct {
	ID            string         `json:"id"`
	Scope         Scope          `json:"scope"`
	Name          string         `json:"name"`
	Provider      ProviderKind   `json:"provider"`
	Purpose       BindingPurpose `json:"-"`
	State         BindingState   `json:"state"`
	ActiveVersion int64          `json:"activeVersion,omitempty"`
	CreatedBy     string         `json:"createdBy"`
	CreatedAt     time.Time      `json:"createdAt"`
	UpdatedAt     time.Time      `json:"updatedAt"`
	DeleteStarted time.Time      `json:"deleteStartedAt,omitempty"`
	DeletedAt     time.Time      `json:"deletedAt,omitempty"`
}

func (b Binding) Validate() error {
	if !uuidRE.MatchString(b.ID) || b.Scope.Validate() != nil || !dnsLabelRE.MatchString(b.Name) ||
		!b.Provider.valid() || !b.Purpose.valid() || !uuidRE.MatchString(b.CreatedBy) || b.CreatedAt.IsZero() ||
		b.UpdatedAt.Before(b.CreatedAt) || b.ActiveVersion < 0 {
		return ErrInvalid
	}
	switch b.State {
	case BindingProvisioning, BindingFailed:
		if b.ActiveVersion != 0 || !b.DeleteStarted.IsZero() || !b.DeletedAt.IsZero() {
			return ErrInvalid
		}
	case BindingReady:
		if b.ActiveVersion <= 0 || !b.DeleteStarted.IsZero() || !b.DeletedAt.IsZero() {
			return ErrInvalid
		}
	case BindingDeleting:
		if b.DeleteStarted.IsZero() || !b.DeletedAt.IsZero() {
			return ErrInvalid
		}
	case BindingDeleted:
		if b.ActiveVersion != 0 || b.DeleteStarted.IsZero() || b.DeletedAt.Before(b.DeleteStarted) {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

// Artifact contains provider identities and cryptographic digests only. It
// never contains provider payloads, ciphertext, Secret data, or base64.
type Artifact struct {
	Provider             ProviderKind     `json:"provider"`
	Namespace            string           `json:"namespace"`
	ObjectName           string           `json:"objectName"`
	TargetSecretName     string           `json:"targetSecretName"`
	ProviderRevision     string           `json:"providerRevision"`
	ManifestDigest       string           `json:"manifestDigest"`
	SealedKeyFingerprint string           `json:"sealedKeyFingerprint,omitempty"`
	CiphertextDigest     string           `json:"ciphertextDigest,omitempty"`
	TargetSecretType     TargetSecretType `json:"-"`
}

func (a Artifact) ValidateFor(binding Binding, version int64) error {
	if binding.Validate() != nil || version <= 0 || a.Provider != binding.Provider || a.Namespace != binding.Scope.Namespace ||
		!kubeNameRE.MatchString(a.ObjectName) || !kubeNameRE.MatchString(a.TargetSecretName) ||
		a.TargetSecretName != TargetSecretName(binding, version) || !a.TargetSecretType.valid() ||
		!safeOpaque(a.ProviderRevision, 256) || !digestRE.MatchString(a.ManifestDigest) {
		return ErrProviderMismatch
	}
	if a.Provider == ProviderExternalSecrets {
		if a.SealedKeyFingerprint != "" || a.CiphertextDigest != "" {
			return ErrProviderMismatch
		}
		return nil
	}
	if !digestRE.MatchString(a.SealedKeyFingerprint) || !digestRE.MatchString(a.CiphertextDigest) {
		return ErrProviderMismatch
	}
	return nil
}

func TargetSecretName(binding Binding, version int64) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", binding.ID, version)))
	suffix := hex.EncodeToString(digest[:5])
	versionText := fmt.Sprintf("%d", version)
	maxBindingName := 63 - len("kp-") - len("-v") - len(versionText) - 1 - len(suffix)
	name := binding.Name
	if len(name) > maxBindingName {
		name = strings.Trim(name[:maxBindingName], "-")
	}
	return fmt.Sprintf("kp-%s-v%s-%s", name, versionText, suffix)
}

type Version struct {
	ID                  string           `json:"id"`
	BindingID           string           `json:"bindingId"`
	Number              int64            `json:"number"`
	Provider            ProviderKind     `json:"provider"`
	State               VersionState     `json:"state"`
	Deliveries          []Delivery       `json:"deliveries"`
	Artifact            *Artifact        `json:"artifact,omitempty"`
	FailureCode         string           `json:"failureCode,omitempty"`
	StagedAt            time.Time        `json:"stagedAt,omitempty"`
	ReadinessObservedAt time.Time        `json:"readinessObservedAt,omitempty"`
	ActivatedAt         time.Time        `json:"activatedAt,omitempty"`
	RetainedAt          time.Time        `json:"retainedAt,omitempty"`
	CreatedAt           time.Time        `json:"createdAt"`
	UpdatedAt           time.Time        `json:"updatedAt"`
	TargetSecretType    TargetSecretType `json:"-"`
	FingerprintKeyID    string           `json:"-"`
	ContentFingerprint  [32]byte         `json:"-"`
	RequestFingerprint  [32]byte         `json:"-"`
}

func (v Version) Validate() error {
	deliveries, err := normalizeDeliveries(v.Deliveries)
	if err != nil || !slices.Equal(deliveries, v.Deliveries) || !uuidRE.MatchString(v.ID) || !uuidRE.MatchString(v.BindingID) ||
		v.Number <= 0 || !v.Provider.valid() || !v.TargetSecretType.valid() || !keyIDRE.MatchString(v.FingerprintKeyID) ||
		v.CreatedAt.IsZero() || v.UpdatedAt.Before(v.CreatedAt) {
		return ErrInvalid
	}
	if v.State != VersionStaging && v.State != VersionAwaitingReadiness && v.State != VersionActive && v.State != VersionRetained && v.State != VersionFailed && v.State != VersionDeleted {
		return ErrInvalid
	}
	if v.State == VersionFailed && v.FailureCode == "" || v.State != VersionFailed && v.State != VersionDeleted && v.FailureCode != "" ||
		v.FailureCode != "" && !safeCodeRE.MatchString(v.FailureCode) {
		return ErrInvalid
	}
	if v.State == VersionStaging && v.Artifact != nil ||
		(v.State == VersionAwaitingReadiness || v.State == VersionActive || v.State == VersionRetained) && v.Artifact == nil {
		return ErrInvalid
	}
	return nil
}

type ReferenceKind string

const (
	ReferenceGitCurrent      ReferenceKind = "git-current"
	ReferenceCurrentRelease  ReferenceKind = "current-release"
	ReferenceRetainedRelease ReferenceKind = "retained-release"
)

type Reference struct {
	BindingID string        `json:"bindingId"`
	VersionID string        `json:"versionId"`
	Kind      ReferenceKind `json:"kind"`
	Reference string        `json:"reference"`
	Revision  string        `json:"revision"`
	CreatedAt time.Time     `json:"createdAt"`
}

func (r Reference) Validate() error {
	if !uuidRE.MatchString(r.BindingID) || !uuidRE.MatchString(r.VersionID) ||
		!r.Kind.valid() ||
		!safeOpaque(r.Reference, 256) || !revisionRE.MatchString(r.Revision) || r.CreatedAt.IsZero() {
		return ErrInvalid
	}
	return nil
}

type EventKind string

const (
	EventVersionStaging           EventKind = "version-staging"
	EventVersionAwaitingReadiness EventKind = "version-awaiting-readiness"
	EventVersionActive            EventKind = "version-active"
	EventVersionFailed            EventKind = "version-failed"
	EventReferenceAdded           EventKind = "reference-added"
	EventReferenceRemoved         EventKind = "reference-removed"
	EventBindingDeleting          EventKind = "binding-deleting"
	EventBindingDeleted           EventKind = "binding-deleted"
)

type Event struct {
	ID          string    `json:"id"`
	BindingID   string    `json:"bindingId"`
	VersionID   string    `json:"versionId,omitempty"`
	ActorID     string    `json:"actorId,omitempty"`
	Kind        EventKind `json:"kind"`
	RequestID   string    `json:"requestId"`
	OccurredAt  time.Time `json:"occurredAt"`
	PublishedAt time.Time `json:"publishedAt,omitempty"`
}

func (e Event) Validate() error {
	if !uuidRE.MatchString(e.ID) || !uuidRE.MatchString(e.BindingID) || e.VersionID != "" && !uuidRE.MatchString(e.VersionID) ||
		e.ActorID != "" && !uuidRE.MatchString(e.ActorID) || !validEventKind(e.Kind) || !requestIDRE.MatchString(e.RequestID) || e.OccurredAt.IsZero() ||
		!e.PublishedAt.IsZero() && e.PublishedAt.Before(e.OccurredAt) {
		return ErrInvalid
	}
	return nil
}

func validEventKind(kind EventKind) bool {
	switch kind {
	case EventVersionStaging, EventVersionAwaitingReadiness, EventVersionActive, EventVersionFailed,
		EventReferenceAdded, EventReferenceRemoved, EventBindingDeleting, EventBindingDeleted:
		return true
	default:
		return false
	}
}

func safeOpaque(value string, max int) bool {
	if len(value) < 1 || len(value) > max || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func cloneVersion(version Version) Version {
	version.Deliveries = append([]Delivery(nil), version.Deliveries...)
	if version.Artifact != nil {
		artifact := *version.Artifact
		version.Artifact = &artifact
	}
	return version
}
