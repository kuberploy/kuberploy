// Package gitprojection owns the rebuildable, revisioned read model derived
// from authoritative Git refs and the short compare-and-swap coordination used
// by Kuberploy's Git writer. Git remains desired-state authority; none of the
// records in this package are an alternative deployment specification.
package gitprojection

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kuberploy/kuberploy/internal/variables"
)

const (
	MaxDocumentBytes                   = 256 << 10
	MaxProtectedHelmPayloadBytes       = 2 << 20
	MaxProtectedHelmApplicationBytes   = 32 << 10
	MaxProtectedFoundationBytes        = 256 << 10
	MaxProtectedCertificateIssuerBytes = 32 << 10
	MaxProtectedExternalDNSBytes       = 128 << 10
	MaxDocumentsBundle                 = 16
	DefaultParser                      = "appconfig-v1alpha1"
	minimumReconciliationLease         = 15 * time.Second
	maximumReconciliationLease         = 5 * time.Minute
)

var (
	ErrNotFound = errors.New("Git projection record not found")
	ErrConflict = errors.New("Git projection compare-and-swap conflict")
	// ErrProtectedPathAbsent is a closed conflict subtype. Callers must still
	// prove absence against the exact provider-pinned head before treating it as
	// durable evidence; ordinary CAS, provider, and Git failures never match it.
	ErrProtectedPathAbsent   = errors.New("protected Git path is absent")
	ErrInvalid               = errors.New("invalid Git projection input")
	ErrStale                 = errors.New("Git projection is stale")
	ErrLeaseHeld             = errors.New("Git projection path is reserved")
	ErrLeaseLost             = errors.New("Git projection reservation lease was lost")
	ErrDiverged              = errors.New("Git binding diverged")
	ErrMissingRef            = errors.New("Git binding target ref is missing")
	ErrProviderMismatch      = errors.New("Git provider verification did not match the binding")
	ErrPolicyUnavailable     = errors.New("AppConfig policy validation is unavailable")
	ErrProtectionUnavailable = errors.New("protected Git branch policy is unavailable")

	uuidRE          = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	commitRE        = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	blobRE          = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	digestRE        = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	chartIdentityRE = regexp.MustCompile(`^(?:sha256:[0-9a-f]{64}|[0-9]+\.[0-9]+\.[0-9]+(?:-rc\.[0-9]+)?)$`)
	refRE           = regexp.MustCompile(`^refs/heads/[A-Za-z0-9](?:[A-Za-z0-9._/-]{0,198}[A-Za-z0-9])?$`)
	nameRE          = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
	loginRE         = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38}[A-Za-z0-9])?$`)
	kubeRE          = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`)
	ownerRE         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$`)
	errorRE         = regexp.MustCompile(`^[a-z][a-z0-9.-]{0,62}$`)
)

type BindingKind string

const (
	BindingPlatform    BindingKind = "platform"
	BindingEnvironment BindingKind = "environment"
)

type BindingState string

const (
	BindingReady      BindingState = "ready"
	BindingIndexing   BindingState = "indexing"
	BindingWaiting    BindingState = "waiting-for-git"
	BindingDiverged   BindingState = "diverged"
	BindingMissingRef BindingState = "missing-ref"
)

type CredentialMode string

const (
	CredentialLegacySecret CredentialMode = "legacy-secret"
	CredentialGitHubApp    CredentialMode = "github-app"
)

// RepositoryIdentity is copied only after provider verification. RepositoryID
// and InstallationID are the authorization boundary; owner/name are display
// and canonical HTTPS transport components, never tenant-provided URLs.
type RepositoryIdentity struct {
	Provider       string `json:"provider"`
	InstallationID int64  `json:"installationId"`
	RepositoryID   int64  `json:"repositoryId"`
	Owner          string `json:"owner"`
	Name           string `json:"name"`
}

func (r RepositoryIdentity) Validate() error {
	if r.Provider != "github" || r.InstallationID <= 0 || r.RepositoryID <= 0 || !loginRE.MatchString(r.Owner) || !nameRE.MatchString(r.Name) || r.Name == "." || r.Name == ".." {
		return ErrInvalid
	}
	return nil
}

func (r RepositoryIdentity) CanonicalRemote() (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	u := &url.URL{Scheme: "https", Host: "github.com", Path: "/" + r.Owner + "/" + r.Name + ".git"}
	return u.String(), nil
}

// Binding is operator-created. Scope identity determines Prefix; callers
// cannot supply an arbitrary Git path, URL, destination namespace, or Argo
// project through this model.
type Binding struct {
	ID                   string             `json:"id"`
	Kind                 BindingKind        `json:"kind"`
	ScopeID              string             `json:"scopeId"`
	ProjectID            string             `json:"projectId,omitempty"`
	EnvironmentID        string             `json:"environmentId,omitempty"`
	Repository           RepositoryIdentity `json:"repository"`
	TargetRef            string             `json:"targetRef"`
	Prefix               string             `json:"prefix"`
	CredentialMode       CredentialMode     `json:"credentialMode"`
	CredentialSecretName string             `json:"credentialSecretName,omitempty"`
	State                BindingState       `json:"state"`
	TargetHeadRevision   string             `json:"targetHeadRevision,omitempty"`
	IndexedRevision      string             `json:"indexedRevision,omitempty"`
	ProjectionGeneration int64              `json:"projectionGeneration"`
	ParserVersion        string             `json:"parserVersion"`
	TargetHeadObservedAt time.Time          `json:"targetHeadObservedAt,omitempty"`
	IndexedAt            time.Time          `json:"indexedAt,omitempty"`
	CreatedAt            time.Time          `json:"createdAt"`
	UpdatedAt            time.Time          `json:"updatedAt"`
}

// CreateEnvironmentBindingInput carries only opaque linked-catalog IDs plus a
// repository identity already resolved by the GitHub App catalog. Production
// persistence re-reads and compares that identity in the authorization
// transaction; callers cannot supply a remote URL, Git path, or credential.
type CreateEnvironmentBindingInput struct {
	EnvironmentID        string
	LinkedInstallationID string
	LinkedRepositoryID   string
	GitHubAppID          int64
	Repository           RepositoryIdentity
	TargetRef            string
}

func (in CreateEnvironmentBindingInput) Validate() error {
	if !uuidRE.MatchString(in.EnvironmentID) || !uuidRE.MatchString(in.LinkedInstallationID) || !uuidRE.MatchString(in.LinkedRepositoryID) ||
		in.GitHubAppID <= 0 || in.Repository.Validate() != nil || !validTargetRef(in.TargetRef) {
		return ErrInvalid
	}
	return nil
}

// CreatePlatformBindingInput is assembled only by the platform HTTP/service
// boundary after resolving opaque linked-catalog IDs against the operator's
// GitHub App configuration. The central store re-reads and compares the same
// active catalog rows transactionally. Repository is therefore server
// authority, never a request-body field.
type CreatePlatformBindingInput struct {
	BindingID            string
	LinkedInstallationID string
	LinkedRepositoryID   string
	GitHubAppID          int64
	Repository           RepositoryIdentity
	TargetRef            string
}

func (in CreatePlatformBindingInput) Validate() error {
	if !uuidRE.MatchString(in.BindingID) || !uuidRE.MatchString(in.LinkedInstallationID) || !uuidRE.MatchString(in.LinkedRepositoryID) ||
		in.GitHubAppID <= 0 || in.Repository.Validate() != nil || !validTargetRef(in.TargetRef) {
		return ErrInvalid
	}
	return nil
}

func NewEnvironmentBinding(id, projectID, environmentID string, repository RepositoryIdentity, targetRef, credentialSecret string, now time.Time) (Binding, error) {
	b := Binding{ID: id, Kind: BindingEnvironment, ScopeID: environmentID, ProjectID: projectID, EnvironmentID: environmentID,
		Repository: repository, TargetRef: targetRef, Prefix: EnvironmentPrefix(projectID, environmentID), CredentialSecretName: credentialSecret,
		CredentialMode: CredentialLegacySecret, State: BindingWaiting, ParserVersion: DefaultParser, CreatedAt: now.UTC(), UpdatedAt: now.UTC()}
	return b, b.Validate()
}

func NewGitHubEnvironmentBinding(id, projectID, environmentID string, repository RepositoryIdentity, targetRef string, now time.Time) (Binding, error) {
	b := Binding{ID: id, Kind: BindingEnvironment, ScopeID: environmentID, ProjectID: projectID, EnvironmentID: environmentID,
		Repository: repository, TargetRef: targetRef, Prefix: EnvironmentPrefix(projectID, environmentID), CredentialMode: CredentialGitHubApp,
		State: BindingWaiting, ParserVersion: DefaultParser, CreatedAt: now.UTC(), UpdatedAt: now.UTC()}
	return b, b.Validate()
}

func NewPlatformBinding(id string, repository RepositoryIdentity, targetRef, credentialSecret string, now time.Time) (Binding, error) {
	b := Binding{ID: id, Kind: BindingPlatform, ScopeID: id, Repository: repository,
		TargetRef: targetRef, Prefix: PlatformPrefix(), CredentialSecretName: credentialSecret,
		CredentialMode: CredentialLegacySecret, State: BindingWaiting, ParserVersion: DefaultParser, CreatedAt: now.UTC(), UpdatedAt: now.UTC()}
	return b, b.Validate()
}

func NewGitHubPlatformBinding(id string, repository RepositoryIdentity, targetRef string, now time.Time) (Binding, error) {
	b := Binding{ID: id, Kind: BindingPlatform, ScopeID: id, Repository: repository,
		TargetRef: targetRef, Prefix: PlatformPrefix(), CredentialMode: CredentialGitHubApp,
		State: BindingWaiting, ParserVersion: DefaultParser, CreatedAt: now.UTC(), UpdatedAt: now.UTC()}
	return b, b.Validate()
}

func EnvironmentPrefix(projectID, environmentID string) string {
	return path.Join("tenants", projectID, "environments", environmentID)
}

func PlatformPrefix() string { return "platform" }

func ApplicationPath(binding Binding, applicationID string) (string, error) {
	if err := binding.Validate(); err != nil || binding.Kind != BindingEnvironment || !uuidRE.MatchString(applicationID) {
		return "", ErrInvalid
	}
	return path.Join(binding.Prefix, "apps", applicationID, "app.yaml"), nil
}

func DependencyPaths(binding Binding) ([]string, error) {
	if err := binding.Validate(); err != nil || binding.Kind != BindingEnvironment {
		return nil, ErrInvalid
	}
	return []string{path.Join("tenants", binding.ProjectID, "variables.yaml"), path.Join(binding.Prefix, "variables.yaml")}, nil
}

func (b Binding) Validate() error {
	if !uuidRE.MatchString(b.ID) || !uuidRE.MatchString(b.ScopeID) || b.Repository.Validate() != nil || !validTargetRef(b.TargetRef) ||
		!validCredentialBinding(b.CredentialMode, b.CredentialSecretName) || b.ParserVersion == "" || len(b.ParserVersion) > 64 || b.CreatedAt.IsZero() || b.UpdatedAt.IsZero() {
		return ErrInvalid
	}
	switch b.Kind {
	case BindingEnvironment:
		if !uuidRE.MatchString(b.ProjectID) || b.EnvironmentID != b.ScopeID || b.Prefix != EnvironmentPrefix(b.ProjectID, b.EnvironmentID) {
			return ErrInvalid
		}
	case BindingPlatform:
		if b.ProjectID != "" || b.EnvironmentID != "" || b.ScopeID != b.ID || b.Prefix != PlatformPrefix() {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	if !validRelativePath(b.Prefix) {
		return ErrInvalid
	}
	if !validBindingState(b.State) || b.ProjectionGeneration < 0 {
		return ErrInvalid
	}
	if b.TargetHeadRevision != "" && !commitRE.MatchString(b.TargetHeadRevision) || b.IndexedRevision != "" && !commitRE.MatchString(b.IndexedRevision) {
		return ErrInvalid
	}
	if (b.TargetHeadRevision == "") != b.TargetHeadObservedAt.IsZero() || b.UpdatedAt.Before(b.CreatedAt) ||
		!b.TargetHeadObservedAt.IsZero() && (b.TargetHeadObservedAt.Before(b.CreatedAt) || b.TargetHeadObservedAt.After(b.UpdatedAt)) {
		return ErrInvalid
	}
	if (b.IndexedRevision == "") != b.IndexedAt.IsZero() || b.IndexedRevision != "" && (b.ProjectionGeneration == 0 || b.IndexedAt.Before(b.CreatedAt) || b.IndexedAt.After(b.UpdatedAt)) {
		return ErrInvalid
	}
	if b.State == BindingReady && (b.TargetHeadRevision == "" || b.TargetHeadRevision != b.IndexedRevision) ||
		(b.State == BindingIndexing || b.State == BindingDiverged) && b.TargetHeadRevision == "" {
		return ErrInvalid
	}
	return nil
}

func validCredentialBinding(mode CredentialMode, secretName string) bool {
	switch mode {
	case CredentialLegacySecret:
		return validSecretName(secretName)
	case CredentialGitHubApp:
		return secretName == ""
	default:
		return false
	}
}

func validBindingState(state BindingState) bool {
	return state == BindingReady || state == BindingIndexing || state == BindingWaiting || state == BindingDiverged || state == BindingMissingRef
}

func validTargetRef(ref string) bool {
	if !refRE.MatchString(ref) || strings.Contains(ref, "..") || strings.Contains(ref, "//") || strings.HasSuffix(ref, ".lock") || strings.Contains(ref, "@{") {
		return false
	}
	for _, component := range strings.Split(strings.TrimPrefix(ref, "refs/heads/"), "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".") {
			return false
		}
	}
	return true
}

func validSecretName(value string) bool {
	return len(value) <= 253 && kubeRE.MatchString(value) && !strings.Contains(value, "..")
}

type ObservationSource string

const (
	ObservationWebhook ObservationSource = "verified-webhook"
	ObservationPoll    ObservationSource = "safety-poll"
	ObservationWrite   ObservationSource = "write-finalization"
)

// VerifiedHead is returned by a provider client after it has authenticated the
// installation and read the configured repository/ref. Webhook after-SHAs are
// only hints; they must not be passed directly as VerifiedHead.
type VerifiedHead struct {
	BindingID       string             `json:"bindingId"`
	Repository      RepositoryIdentity `json:"repository"`
	TargetRef       string             `json:"targetRef"`
	Commit          string             `json:"commit"`
	Source          ObservationSource  `json:"source"`
	ProviderRequest string             `json:"providerRequest"`
	ObservedAt      time.Time          `json:"observedAt"`
}

func (h VerifiedHead) ValidateFor(binding Binding) error {
	if binding.Validate() != nil || h.BindingID != binding.ID || h.Repository != binding.Repository || h.TargetRef != binding.TargetRef ||
		!commitRE.MatchString(h.Commit) || h.ObservedAt.IsZero() || len(h.ProviderRequest) == 0 || len(h.ProviderRequest) > 256 || !utf8.ValidString(h.ProviderRequest) {
		return ErrProviderMismatch
	}
	if h.Source != ObservationWebhook && h.Source != ObservationPoll && h.Source != ObservationWrite {
		return ErrInvalid
	}
	return nil
}

type WebhookTombstone struct {
	Provider     string    `json:"provider"`
	RepositoryID int64     `json:"repositoryId"`
	TargetRef    string    `json:"targetRef"`
	AfterCommit  string    `json:"afterCommit"`
	DeliveryHash string    `json:"deliveryHash"`
	ReceivedAt   time.Time `json:"receivedAt"`
}

func (w WebhookTombstone) Validate() error {
	if w.Provider != "github" || w.RepositoryID <= 0 || !validTargetRef(w.TargetRef) || !commitRE.MatchString(w.AfterCommit) ||
		!digestRE.MatchString(w.DeliveryHash) || w.ReceivedAt.IsZero() {
		return ErrInvalid
	}
	return nil
}

type PollCursor struct {
	BindingID       string    `json:"bindingId"`
	LastCommit      string    `json:"lastCommit,omitempty"`
	ProviderCursor  string    `json:"providerCursor,omitempty"`
	ConsecutiveFail int       `json:"consecutiveFailures"`
	NextPollAt      time.Time `json:"nextPollAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

func (p PollCursor) Validate() error {
	if !uuidRE.MatchString(p.BindingID) || p.LastCommit != "" && !commitRE.MatchString(p.LastCommit) || p.ConsecutiveFail < 0 || p.ConsecutiveFail > 32 ||
		len(p.ProviderCursor) > 512 || !utf8.ValidString(p.ProviderCursor) || p.NextPollAt.IsZero() || p.UpdatedAt.IsZero() {
		return ErrInvalid
	}
	return nil
}

// ReconciliationLease is an expiring, monotonically fenced claim for one
// binding. Epoch, not owner alone, prevents a restarted worker from completing
// work acquired by an older incarnation with the same owner identity.
type ReconciliationLease struct {
	BindingID      string    `json:"bindingId"`
	Owner          string    `json:"owner"`
	Epoch          int64     `json:"epoch"`
	WakeGeneration int64     `json:"wakeGeneration"`
	Until          time.Time `json:"until"`
}

func (l ReconciliationLease) Validate() error {
	if !uuidRE.MatchString(l.BindingID) || !ownerRE.MatchString(l.Owner) || l.Epoch <= 0 || l.WakeGeneration < 0 || l.Until.IsZero() {
		return ErrInvalid
	}
	return nil
}

func validReconciliationLeaseDuration(value time.Duration) bool {
	return value >= minimumReconciliationLease && value <= maximumReconciliationLease
}

type ReconciliationWork struct {
	Binding            Binding             `json:"binding"`
	Lease              ReconciliationLease `json:"lease"`
	ConsecutiveFailure int                 `json:"consecutiveFailures"`
	Reclaimed          bool                `json:"reclaimed"`
	BindingChanged     bool                `json:"bindingChanged"`
}

func (w ReconciliationWork) Validate() error {
	if w.Binding.Validate() != nil || w.Lease.Validate() != nil || w.Lease.BindingID != w.Binding.ID || w.ConsecutiveFailure < 0 || w.ConsecutiveFailure > 32 {
		return ErrInvalid
	}
	return nil
}

type ReconciliationOutcome struct {
	LastCommit         string    `json:"lastCommit,omitempty"`
	ConsecutiveFailure int       `json:"consecutiveFailures"`
	NextPollAt         time.Time `json:"nextPollAt"`
	FailureCode        string    `json:"failureCode,omitempty"`
}

func (o ReconciliationOutcome) Validate() error {
	if o.LastCommit != "" && !commitRE.MatchString(o.LastCommit) || o.ConsecutiveFailure < 0 || o.ConsecutiveFailure > 32 || o.NextPollAt.IsZero() ||
		(o.FailureCode != "" && !errorRE.MatchString(o.FailureCode)) || (o.ConsecutiveFailure == 0) != (o.FailureCode == "") ||
		o.ConsecutiveFailure == 0 && o.LastCommit == "" {
		return ErrInvalid
	}
	return nil
}

type ProjectionState string

const (
	ProjectionStaging ProjectionState = "staging"
	ProjectionActive  ProjectionState = "active"
	ProjectionFailed  ProjectionState = "failed"
)

type Generation struct {
	BindingID     string          `json:"bindingId"`
	Number        int64           `json:"number"`
	HeadRevision  string          `json:"headRevision"`
	ParserVersion string          `json:"parserVersion"`
	State         ProjectionState `json:"state"`
	StartedAt     time.Time       `json:"startedAt"`
	ActivatedAt   *time.Time      `json:"activatedAt,omitempty"`
}

func (g Generation) Validate() error {
	if !uuidRE.MatchString(g.BindingID) || g.Number <= 0 || !commitRE.MatchString(g.HeadRevision) || g.ParserVersion == "" || len(g.ParserVersion) > 64 || g.StartedAt.IsZero() {
		return ErrInvalid
	}
	if g.State != ProjectionStaging && g.State != ProjectionActive && g.State != ProjectionFailed {
		return ErrInvalid
	}
	if g.State == ProjectionActive && g.ActivatedAt == nil || g.State != ProjectionActive && g.ActivatedAt != nil || g.ActivatedAt != nil && g.ActivatedAt.Before(g.StartedAt) {
		return ErrInvalid
	}
	return nil
}

type Diagnostic struct {
	Code    string `json:"code"`
	Detail  string `json:"detail"`
	Pointer string `json:"pointer,omitempty"`
}

type Document struct {
	BindingID      string         `json:"bindingId"`
	Generation     int64          `json:"generation"`
	Path           string         `json:"path"`
	ApplicationID  string         `json:"applicationId,omitempty"`
	SourceRevision string         `json:"sourceRevision"`
	ConfigRevision string         `json:"configRevision"`
	BlobID         string         `json:"blobId"`
	ContentSHA256  string         `json:"contentSha256"`
	Raw            []byte         `json:"-"`
	Parsed         map[string]any `json:"parsed,omitempty"`
	Valid          bool           `json:"valid"`
	Diagnostics    []Diagnostic   `json:"diagnostics,omitempty"`
	SchemaVersion  string         `json:"schemaVersion"`
	ParserVersion  string         `json:"parserVersion"`
	IndexedAt      time.Time      `json:"indexedAt"`
}

func NewDocument(binding Binding, generation int64, applicationID, sourceRevision, configRevision, blobID string, raw []byte, parsed map[string]any, diagnostics []Diagnostic, now time.Time) (Document, error) {
	d := Document{BindingID: binding.ID, Generation: generation, ApplicationID: applicationID, SourceRevision: sourceRevision,
		ConfigRevision: configRevision, BlobID: blobID, Raw: append([]byte(nil), raw...), Parsed: cloneMap(parsed), Valid: len(diagnostics) == 0,
		Diagnostics: slices.Clone(diagnostics), SchemaVersion: "config.kuberploy.io/v1alpha1", ParserVersion: binding.ParserVersion, IndexedAt: now.UTC()}
	var err error
	d.Path, err = ApplicationPath(binding, applicationID)
	if err != nil {
		return Document{}, err
	}
	sum := sha256.Sum256(raw)
	d.ContentSHA256 = "sha256:" + hex.EncodeToString(sum[:])
	return d, d.Validate(binding)
}

func NewDependencyDocument(binding Binding, generation int64, dependencyPath, sourceRevision, configRevision, blobID string, raw []byte, parsed map[string]any, diagnostics []Diagnostic, now time.Time) (Document, error) {
	allowed, err := DependencyPaths(binding)
	if err != nil || !slices.Contains(allowed, dependencyPath) {
		return Document{}, ErrInvalid
	}
	if len(diagnostics) == 0 && parsed == nil {
		return Document{}, ErrInvalid
	}
	d := Document{BindingID: binding.ID, Generation: generation, Path: dependencyPath, SourceRevision: sourceRevision, ConfigRevision: configRevision,
		BlobID: blobID, Raw: append([]byte(nil), raw...), Parsed: cloneMap(parsed), Valid: len(diagnostics) == 0, Diagnostics: slices.Clone(diagnostics),
		SchemaVersion: "variables.kuberploy.io/v1alpha1", ParserVersion: binding.ParserVersion, IndexedAt: now.UTC()}
	sum := sha256.Sum256(raw)
	d.ContentSHA256 = "sha256:" + hex.EncodeToString(sum[:])
	return d, d.Validate(binding)
}

func (d Document) Validate(binding Binding) error {
	pathValid := false
	if d.ApplicationID != "" {
		expectedPath, err := ApplicationPath(binding, d.ApplicationID)
		pathValid = err == nil && d.Path == expectedPath && d.SchemaVersion == "config.kuberploy.io/v1alpha1"
	} else if dependencies, err := DependencyPaths(binding); err == nil {
		pathValid = slices.Contains(dependencies, d.Path) && d.SchemaVersion == "variables.kuberploy.io/v1alpha1"
	}
	if !pathValid || d.BindingID != binding.ID || d.Generation <= 0 || !commitRE.MatchString(d.SourceRevision) ||
		!commitRE.MatchString(d.ConfigRevision) || !blobRE.MatchString(d.BlobID) || !digestRE.MatchString(d.ContentSHA256) ||
		len(d.Raw) == 0 || len(d.Raw) > MaxDocumentBytes || d.SchemaVersion == "" || d.ParserVersion != binding.ParserVersion || d.IndexedAt.IsZero() {
		return ErrInvalid
	}
	sum := sha256.Sum256(d.Raw)
	if d.ContentSHA256 != "sha256:"+hex.EncodeToString(sum[:]) || d.Valid != (len(d.Diagnostics) == 0) || len(d.Diagnostics) > 64 {
		return ErrInvalid
	}
	for _, diagnostic := range d.Diagnostics {
		if diagnostic.Code == "" || len(diagnostic.Code) > 64 || len(diagnostic.Detail) == 0 || len(diagnostic.Detail) > 1024 || !utf8.ValidString(diagnostic.Detail) ||
			len(diagnostic.Pointer) > 1024 || !utf8.ValidString(diagnostic.Pointer) || strings.ContainsAny(diagnostic.Pointer, "\x00\r\n") {
			return ErrInvalid
		}
	}
	return nil
}

type Bundle struct {
	BindingID          string            `json:"bindingId"`
	TargetRef          string            `json:"targetRef"`
	TargetHeadRevision string            `json:"targetHeadRevision"`
	IndexedRevision    string            `json:"indexedRevision"`
	ConfigRevision     string            `json:"configRevision"`
	ETag               string            `json:"etag"`
	Stale              bool              `json:"stale"`
	Documents          []Document        `json:"documents"`
	Dependencies       []DependencyState `json:"dependencies"`
	IndexedAt          time.Time         `json:"indexedAt"`
}

// DependencyState records both presence and absence for every server-derived
// VariableSet path. Absence is meaningful Git state and is therefore part of
// the application's strong ETag instead of being collapsed into ErrNotFound.
type DependencyState struct {
	Path    string `json:"path"`
	Present bool   `json:"present"`
	BlobID  string `json:"blobId,omitempty"`
}

func StrongETagWithDependencies(binding Binding, documents []Document, dependencyPaths []string, dependencies []Document, chartDigest, policyVersion string) (string, error) {
	allowed, err := DependencyPaths(binding)
	if err != nil || !slices.Equal(dependencyPaths, allowed) || len(documents) == 0 || len(dependencies) > len(dependencyPaths) {
		return "", ErrInvalid
	}
	// Preserve existing application-only ETags for repositories that have not
	// opted into either VariableSet yet. Adding the first parent document still
	// necessarily changes the ETag, so the all-absent state remains an exact and
	// unambiguous dependency snapshot without breaking existing deployments.
	if len(dependencies) == 0 {
		return StrongETag(binding, documents, nil, chartDigest, policyVersion)
	}
	byPath := make(map[string]Document, len(dependencies))
	for _, dependency := range dependencies {
		if dependency.Validate(binding) != nil || dependency.ApplicationID != "" || !slices.Contains(dependencyPaths, dependency.Path) {
			return "", ErrInvalid
		}
		if _, duplicate := byPath[dependency.Path]; duplicate {
			return "", ErrInvalid
		}
		byPath[dependency.Path] = dependency
	}
	if binding.Validate() != nil || !chartIdentityRE.MatchString(chartDigest) || policyVersion == "" || len(policyVersion) > 128 || len(documents)+len(dependencies) > MaxDocumentsBundle {
		return "", ErrInvalid
	}
	entries := make([]string, 0, len(documents)+len(dependencyPaths))
	seen := map[string]struct{}{}
	for _, document := range documents {
		if document.Validate(binding) != nil || document.ApplicationID == "" {
			return "", ErrInvalid
		}
		if _, duplicate := seen[document.Path]; duplicate {
			return "", ErrInvalid
		}
		seen[document.Path] = struct{}{}
		entries = append(entries, document.Path+"\x00present\x00"+document.BlobID)
	}
	for _, dependencyPath := range dependencyPaths {
		if dependency, present := byPath[dependencyPath]; present {
			entries = append(entries, dependencyPath+"\x00present\x00"+dependency.BlobID)
		} else {
			entries = append(entries, dependencyPath+"\x00absent")
		}
	}
	slices.Sort(entries)
	hash := sha256.New()
	for _, value := range append([]string{"kuberploy-bundle-etag-v2", binding.ID, binding.TargetRef, binding.ParserVersion, chartDigest, policyVersion}, entries...) {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	return `"sha256:` + hex.EncodeToString(hash.Sum(nil)) + `"`, nil
}

func StrongETag(binding Binding, documents, dependencies []Document, chartDigest, policyVersion string) (string, error) {
	if binding.Validate() != nil || !chartIdentityRE.MatchString(chartDigest) || policyVersion == "" || len(policyVersion) > 128 || len(documents) == 0 || len(documents)+len(dependencies) > MaxDocumentsBundle {
		return "", ErrInvalid
	}
	entries := make([]string, 0, len(documents)+len(dependencies))
	seen := map[string]struct{}{}
	for _, document := range append(slices.Clone(documents), dependencies...) {
		if document.Validate(binding) != nil {
			return "", ErrInvalid
		}
		entry := document.Path + "\x00" + document.BlobID
		if _, exists := seen[document.Path]; exists {
			return "", ErrInvalid
		}
		seen[document.Path] = struct{}{}
		entries = append(entries, entry)
	}
	slices.Sort(entries)
	hash := sha256.New()
	for _, value := range append([]string{binding.ID, binding.TargetRef, binding.ParserVersion, chartDigest, policyVersion}, entries...) {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	return `"sha256:` + hex.EncodeToString(hash.Sum(nil)) + `"`, nil
}

type ReservationState string

const (
	ReservationCandidate             ReservationState = "candidate"
	ReservationCommittedPendingIndex ReservationState = "committed-pending-index"
)

type PathReservation struct {
	BindingID         string           `json:"bindingId"`
	TargetRef         string           `json:"targetRef"`
	Path              string           `json:"path"`
	OperationID       string           `json:"operationId"`
	Owner             string           `json:"owner"`
	BaseRevision      string           `json:"baseRevision"`
	CommittedRevision string           `json:"committedRevision,omitempty"`
	State             ReservationState `json:"state"`
	LeaseUntil        *time.Time       `json:"leaseUntil,omitempty"`
	CreatedAt         time.Time        `json:"createdAt"`
	UpdatedAt         time.Time        `json:"updatedAt"`
}

func (r PathReservation) Validate(binding Binding) error {
	pathAuthorized := validRelativePath(r.Path) && strings.HasPrefix(r.Path+"/", binding.Prefix+"/")
	if dependencies, err := DependencyPaths(binding); err == nil && slices.Contains(dependencies, r.Path) {
		pathAuthorized = true
	}
	if r.BindingID != binding.ID || r.TargetRef != binding.TargetRef || !pathAuthorized ||
		!uuidRE.MatchString(r.OperationID) || r.Owner == "" || len(r.Owner) > 128 || !utf8.ValidString(r.Owner) || strings.ContainsAny(r.Owner, "\x00\r\n") ||
		!commitRE.MatchString(r.BaseRevision) || r.CreatedAt.IsZero() || r.UpdatedAt.IsZero() || r.UpdatedAt.Before(r.CreatedAt) {
		return ErrInvalid
	}
	switch r.State {
	case ReservationCandidate:
		if r.LeaseUntil == nil || r.CommittedRevision != "" {
			return ErrInvalid
		}
	case ReservationCommittedPendingIndex:
		if r.LeaseUntil != nil || !commitRE.MatchString(r.CommittedRevision) {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

type Mutation struct {
	BindingID        string               `json:"bindingId"`
	OperationID      string               `json:"operationId"`
	Path             string               `json:"path"`
	BaseRevision     string               `json:"baseRevision"`
	Precondition     MutationPrecondition `json:"precondition"`
	ExpectedETag     string               `json:"expectedETag,omitempty"`
	Content          []byte               `json:"-"`
	ContentSHA256    string               `json:"contentSHA256,omitempty"`
	Message          string               `json:"message"`
	Action           MutationAction       `json:"action,omitempty"`
	Authority        MutationAuthority    `json:"authority,omitempty"`
	CommitTrailer    string               `json:"commitTrailer,omitempty"`
	RequiredAncestor string               `json:"requiredAncestor,omitempty"`
}

type WriteCommandState string

type PublicationMode string

const (
	WriteCommandPending      WriteCommandState = "pending"
	WriteCommandGitCommitted WriteCommandState = "git-committed"
	WriteCommandIndexed      WriteCommandState = "indexed"
)

const (
	PublicationDirect      PublicationMode = "direct"
	PublicationPullRequest PublicationMode = "pull-request"
)

// WritePlan is the immutable, authorization-time snapshot persisted in the
// same transaction as a deployment operation. Every Git identity and path is
// server-derived; a worker reconstructs its Mutation from the stored command.
type WritePlan struct {
	BindingID     string               `json:"bindingId"`
	ProjectID     string               `json:"projectId"`
	EnvironmentID string               `json:"environmentId"`
	ApplicationID string               `json:"applicationId"`
	BaseRevision  string               `json:"baseRevision"`
	Precondition  MutationPrecondition `json:"precondition"`
	ExpectedETag  string               `json:"expectedETag,omitempty"`
	ChartDigest   string               `json:"chartDigest"`
	PolicyVersion string               `json:"policyVersion"`
	VariableScope string               `json:"variableScope,omitempty"`
	VariablePath  string               `json:"variablePath,omitempty"`
}

func (p WritePlan) Validate(binding Binding) error {
	if p.validateIdentity(binding) != nil || binding.State != BindingReady || binding.TargetHeadRevision != p.BaseRevision || binding.IndexedRevision != p.BaseRevision {
		return ErrInvalid
	}
	return nil
}

func (p WritePlan) validateIdentity(binding Binding) error {
	validPrecondition := p.Precondition == MutationMatchETag && validStrongETag(p.ExpectedETag) ||
		p.Precondition == MutationCreateIfAbsent && p.ExpectedETag == ""
	if binding.Kind != BindingEnvironment || p.BindingID != binding.ID || p.ProjectID != binding.ProjectID ||
		p.EnvironmentID != binding.EnvironmentID || !commitRE.MatchString(p.BaseRevision) || !validPrecondition ||
		p.PolicyVersion == "" || len(p.PolicyVersion) > 128 || !utf8.ValidString(p.PolicyVersion) ||
		strings.ContainsAny(p.PolicyVersion, "\x00\r\n") {
		return ErrInvalid
	}
	if p.VariableScope == "" {
		_, err := ApplicationPath(binding, p.ApplicationID)
		if err != nil || p.VariablePath != "" || !chartIdentityRE.MatchString(p.ChartDigest) {
			return ErrInvalid
		}
		return nil
	}
	dependencies, err := DependencyPaths(binding)
	if err != nil || p.ApplicationID != "" || p.ChartDigest != "" || p.PolicyVersion != binding.ParserVersion {
		return ErrInvalid
	}
	if p.VariableScope == "project" && p.VariablePath == dependencies[0] || p.VariableScope == "environment" && p.VariablePath == dependencies[1] {
		return nil
	}
	return ErrInvalid
}

type WriteCommand struct {
	OperationID       string            `json:"operationId"`
	DeploymentID      string            `json:"deploymentId"`
	ActorID           string            `json:"actorId"`
	Plan              WritePlan         `json:"plan"`
	TargetRef         string            `json:"targetRef"`
	Path              string            `json:"path"`
	Content           []byte            `json:"-"`
	ContentSHA256     string            `json:"contentSha256"`
	Message           string            `json:"message"`
	RequestDigest     string            `json:"requestDigest,omitempty"`
	PublicationMode   PublicationMode   `json:"publicationMode"`
	State             WriteCommandState `json:"state"`
	CommittedRevision string            `json:"committedRevision,omitempty"`
	CommittedAt       *time.Time        `json:"committedAt,omitempty"`
	IndexedGeneration int64             `json:"indexedGeneration,omitempty"`
	IndexedAt         *time.Time        `json:"indexedAt,omitempty"`
	CreatedAt         time.Time         `json:"createdAt"`
	UpdatedAt         time.Time         `json:"updatedAt"`
}

func NewWriteCommand(operationID, deploymentID, actorID string, plan WritePlan, binding Binding, content []byte, message string, now time.Time) (WriteCommand, error) {
	applicationPath, err := ApplicationPath(binding, plan.ApplicationID)
	if err != nil {
		return WriteCommand{}, err
	}
	digest := sha256.Sum256(content)
	command := WriteCommand{OperationID: operationID, DeploymentID: deploymentID, ActorID: actorID, Plan: plan, TargetRef: binding.TargetRef,
		Path: applicationPath, Content: append([]byte(nil), content...), ContentSHA256: "sha256:" + hex.EncodeToString(digest[:]), Message: message,
		PublicationMode: PublicationDirect, State: WriteCommandPending, CreatedAt: now.UTC(), UpdatedAt: now.UTC()}
	return command, command.Validate(binding)
}

func NewVariableWriteCommand(operationID, actorID string, plan WritePlan, binding Binding, content []byte, message, requestDigest string, now time.Time) (WriteCommand, error) {
	if plan.validateIdentity(binding) != nil || plan.VariableScope == "" {
		return WriteCommand{}, ErrInvalid
	}
	digest := sha256.Sum256(content)
	command := WriteCommand{OperationID: operationID, ActorID: actorID, Plan: plan, TargetRef: binding.TargetRef,
		Path: plan.VariablePath, Content: append([]byte(nil), content...), ContentSHA256: "sha256:" + hex.EncodeToString(digest[:]), Message: message,
		RequestDigest: requestDigest, PublicationMode: PublicationDirect, State: WriteCommandPending, CreatedAt: now.UTC(), UpdatedAt: now.UTC()}
	return command, command.Validate(binding)
}

func (c WriteCommand) Mutation() Mutation {
	authority := MutationAuthority("")
	if c.Plan.VariableScope != "" {
		authority = MutationAuthorityVariables
	}
	return Mutation{BindingID: c.Plan.BindingID, OperationID: c.OperationID, Path: c.Path, BaseRevision: c.Plan.BaseRevision, Authority: authority,
		Precondition: c.Plan.Precondition, ExpectedETag: c.Plan.ExpectedETag, Content: append([]byte(nil), c.Content...), Message: c.Message}
}

func (c WriteCommand) Validate(binding Binding) error {
	validTarget := c.Plan.VariableScope != "" && c.DeploymentID == "" || c.Plan.VariableScope == "" && uuidRE.MatchString(c.DeploymentID)
	validRequestDigest := c.Plan.VariableScope != "" && digestRE.MatchString(c.RequestDigest) || c.Plan.VariableScope == "" && c.RequestDigest == ""
	if c.Plan.validateIdentity(binding) != nil || !uuidRE.MatchString(c.OperationID) || !validTarget || !uuidRE.MatchString(c.ActorID) ||
		c.TargetRef != binding.TargetRef || len(c.Content) == 0 || len(c.Content) > MaxDocumentBytes || !digestRE.MatchString(c.ContentSHA256) ||
		len(c.Message) == 0 || len(c.Message) > 512 || !utf8.ValidString(c.Message) || strings.ContainsAny(c.Message, "\x00\r") ||
		(c.PublicationMode != PublicationDirect && c.PublicationMode != PublicationPullRequest) || !validRequestDigest ||
		c.CreatedAt.IsZero() || c.UpdatedAt.IsZero() || c.UpdatedAt.Before(c.CreatedAt) || c.Mutation().Validate(binding) != nil {
		return ErrInvalid
	}
	digest := sha256.Sum256(c.Content)
	if c.ContentSHA256 != "sha256:"+hex.EncodeToString(digest[:]) {
		return ErrInvalid
	}
	switch c.State {
	case WriteCommandPending:
		if c.CommittedRevision != "" || c.CommittedAt != nil || c.IndexedGeneration != 0 || c.IndexedAt != nil {
			return ErrInvalid
		}
	case WriteCommandGitCommitted:
		if !commitRE.MatchString(c.CommittedRevision) || c.CommittedAt == nil || c.CommittedAt.Before(c.CreatedAt) ||
			c.IndexedGeneration != 0 || c.IndexedAt != nil {
			return ErrInvalid
		}
	case WriteCommandIndexed:
		if !commitRE.MatchString(c.CommittedRevision) || c.CommittedAt == nil || c.IndexedGeneration <= 0 || c.IndexedAt == nil ||
			c.CommittedAt.Before(c.CreatedAt) || c.IndexedAt.Before(*c.CommittedAt) {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

// MutationPrecondition makes first-path creation distinct from an optimistic
// update. An absent ExpectedETag is never interpreted as a synthetic version.
// Empty remains a compatibility spelling for match-etag only when an exact
// strong ExpectedETag is present; newly persisted commands always store the
// explicit value.
type MutationPrecondition string

type MutationAction string

const (
	MutationUpsert MutationAction = "upsert"
	MutationDelete MutationAction = "delete"
)

type MutationAuthority string

const (
	MutationAuthorityHelmPayload       MutationAuthority = "helm-protected-payload.v1"
	MutationAuthorityHelmApplication   MutationAuthority = "helm-protected-application.v1"
	MutationAuthorityHelmCascade       MutationAuthority = "helm-application-cascade-preflight.v1"
	MutationAuthorityFoundation        MutationAuthority = "environment-foundation-protected-git.v1"
	MutationAuthorityCertificateIssuer MutationAuthority = "certificate-issuer-protected-git.v1"
	MutationAuthorityExternalDNS       MutationAuthority = "external-dns-protected-git.v1"
	MutationAuthorityVariables         MutationAuthority = "variables-protected-git.v1"
)

const (
	MutationMatchETag      MutationPrecondition = "match-etag"
	MutationCreateIfAbsent MutationPrecondition = "create-if-absent"
)

func (m Mutation) EffectivePrecondition() MutationPrecondition {
	if m.Precondition == "" && validStrongETag(m.ExpectedETag) {
		return MutationMatchETag
	}
	return m.Precondition
}

func (m Mutation) EffectiveAction() MutationAction {
	if m.Action == "" {
		return MutationUpsert
	}
	return m.Action
}

func (m Mutation) Validate(binding Binding) error {
	precondition := m.EffectivePrecondition()
	validPrecondition := precondition == MutationMatchETag && validStrongETag(m.ExpectedETag) ||
		precondition == MutationCreateIfAbsent && m.ExpectedETag == ""
	pathInBinding := validRelativePath(m.Path) && strings.HasPrefix(m.Path+"/", binding.Prefix+"/")
	if m.Authority == MutationAuthorityVariables {
		dependencies, err := DependencyPaths(binding)
		pathInBinding = err == nil && slices.Contains(dependencies, m.Path)
	}
	if m.BindingID != binding.ID || !uuidRE.MatchString(m.OperationID) || !commitRE.MatchString(m.BaseRevision) || !validPrecondition ||
		!pathInBinding ||
		len(m.Message) == 0 || len(m.Message) > 512 || !utf8.ValidString(m.Message) || strings.IndexFunc(m.Message, func(r rune) bool { return r == 0 || r == '\r' }) >= 0 {
		return ErrInvalid
	}
	action := m.EffectiveAction()
	switch m.Authority {
	case "":
		if action != MutationUpsert || m.CommitTrailer != "" || m.RequiredAncestor != "" || m.ContentSHA256 != "" ||
			len(m.Content) == 0 || len(m.Content) > MaxDocumentBytes || !validProtectedDocumentPath(binding, m.Path) {
			return ErrInvalid
		}
	case MutationAuthorityHelmPayload:
		if binding.Kind != BindingPlatform || action != MutationUpsert || precondition != MutationCreateIfAbsent ||
			m.RequiredAncestor != "" || m.CommitTrailer != "Kuberploy-Helm-Payload-Intent: "+m.OperationID ||
			!validHelmPayloadPath(binding, m.Path) || len(m.Content) == 0 || len(m.Content) > MaxProtectedHelmPayloadBytes ||
			!contentDigestMatches(m.Content, m.ContentSHA256) {
			return ErrInvalid
		}
	case MutationAuthorityHelmApplication:
		if binding.Kind != BindingPlatform || !commitRE.MatchString(m.RequiredAncestor) ||
			m.CommitTrailer != "Kuberploy-Helm-Application-Intent: "+m.OperationID ||
			!validHelmApplicationPath(binding, m.Path) {
			return ErrInvalid
		}
		switch action {
		case MutationUpsert:
			if len(m.Content) == 0 || len(m.Content) > MaxProtectedHelmApplicationBytes || !contentDigestMatches(m.Content, m.ContentSHA256) {
				return ErrInvalid
			}
		case MutationDelete:
			if precondition != MutationMatchETag || len(m.Content) != 0 || m.ContentSHA256 != "" {
				return ErrInvalid
			}
		default:
			return ErrInvalid
		}
	case MutationAuthorityHelmCascade:
		if binding.Kind != BindingPlatform || action != MutationUpsert || precondition != MutationMatchETag ||
			!commitRE.MatchString(m.RequiredAncestor) ||
			m.CommitTrailer != "Kuberploy-Helm-Cascade-Preflight: "+m.OperationID ||
			!validHelmApplicationPath(binding, m.Path) || len(m.Content) == 0 ||
			len(m.Content) > MaxProtectedHelmApplicationBytes || !contentDigestMatches(m.Content, m.ContentSHA256) {
			return ErrInvalid
		}
	case MutationAuthorityFoundation:
		if binding.Kind != BindingPlatform || (precondition != MutationCreateIfAbsent && precondition != MutationMatchETag) ||
			!commitRE.MatchString(m.RequiredAncestor) || m.CommitTrailer != "Kuberploy-Environment-Foundation-Intent: "+m.OperationID ||
			!validFoundationPath(binding, m.Path) {
			return ErrInvalid
		}
		switch action {
		case MutationUpsert:
			if len(m.Content) == 0 || len(m.Content) > MaxProtectedFoundationBytes || !contentDigestMatches(m.Content, m.ContentSHA256) {
				return ErrInvalid
			}
		case MutationDelete:
			if precondition != MutationMatchETag || len(m.Content) != 0 || m.ContentSHA256 != "" {
				return ErrInvalid
			}
		default:
			return ErrInvalid
		}
	case MutationAuthorityCertificateIssuer:
		if binding.Kind != BindingPlatform || !commitRE.MatchString(m.RequiredAncestor) ||
			m.CommitTrailer != "Kuberploy-Certificate-Issuer-Intent: "+m.OperationID ||
			!validCertificateIssuerPath(binding, m.Path) {
			return ErrInvalid
		}
		switch action {
		case MutationUpsert:
			if len(m.Content) == 0 || len(m.Content) > MaxProtectedCertificateIssuerBytes || !contentDigestMatches(m.Content, m.ContentSHA256) {
				return ErrInvalid
			}
		case MutationDelete:
			if precondition != MutationMatchETag || len(m.Content) != 0 || m.ContentSHA256 != "" {
				return ErrInvalid
			}
		default:
			return ErrInvalid
		}
	case MutationAuthorityExternalDNS:
		if binding.Kind != BindingPlatform || !commitRE.MatchString(m.RequiredAncestor) ||
			m.CommitTrailer != "Kuberploy-External-DNS-Intent: "+m.OperationID || !validExternalDNSPath(binding, m.Path) {
			return ErrInvalid
		}
		switch action {
		case MutationUpsert:
			if len(m.Content) == 0 || len(m.Content) > MaxProtectedExternalDNSBytes || !contentDigestMatches(m.Content, m.ContentSHA256) {
				return ErrInvalid
			}
		case MutationDelete:
			if precondition != MutationMatchETag || len(m.Content) != 0 || m.ContentSHA256 != "" {
				return ErrInvalid
			}
		default:
			return ErrInvalid
		}
	case MutationAuthorityVariables:
		dependencies, err := DependencyPaths(binding)
		if err != nil || action != MutationUpsert || !slices.Contains(dependencies, m.Path) || len(m.Content) == 0 || len(m.Content) > variables.MaxDocumentBytes || m.ContentSHA256 != "" || m.CommitTrailer != "" || m.RequiredAncestor != "" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

func validFoundationPath(binding Binding, value string) bool {
	if binding.Validate() != nil || binding.Kind != BindingPlatform || !strings.HasPrefix(value, binding.Prefix+"/") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(value, binding.Prefix+"/"), "/")
	return len(parts) == 3 && parts[0] == "argocd" && parts[1] == "foundations" && strings.HasSuffix(parts[2], ".yaml") &&
		uuidRE.MatchString(strings.TrimSuffix(parts[2], ".yaml"))
}

func validCertificateIssuerPath(binding Binding, value string) bool {
	if binding.Validate() != nil || binding.Kind != BindingPlatform || !strings.HasPrefix(value, binding.Prefix+"/") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(value, binding.Prefix+"/"), "/")
	return len(parts) == 4 && parts[0] == "argocd" && parts[1] == "platform" &&
		parts[2] == "certificate-issuers" && strings.HasSuffix(parts[3], ".yaml") &&
		kubeRE.MatchString(strings.TrimSuffix(parts[3], ".yaml"))
}

func validExternalDNSPath(binding Binding, value string) bool {
	if binding.Validate() != nil || binding.Kind != BindingPlatform || !strings.HasPrefix(value, binding.Prefix+"/") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(value, binding.Prefix+"/"), "/")
	return len(parts) == 4 && parts[0] == "argocd" && parts[1] == "platform" && parts[2] == "external-dns" && strings.HasSuffix(parts[3], ".yaml") && uuidRE.MatchString(strings.TrimSuffix(parts[3], ".yaml"))
}

func contentDigestMatches(content []byte, expected string) bool {
	if !digestRE.MatchString(expected) {
		return false
	}
	digest := sha256.Sum256(content)
	return expected == "sha256:"+hex.EncodeToString(digest[:])
}

func validHelmPayloadPath(binding Binding, value string) bool {
	if binding.Validate() != nil || binding.Kind != BindingPlatform || !strings.HasPrefix(value, binding.Prefix+"/") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(value, binding.Prefix+"/"), "/")
	if len(parts) != 8 || parts[0] != "helm-manifests" || parts[1] != "environments" || !uuidRE.MatchString(parts[2]) ||
		parts[3] != "applications" || !uuidRE.MatchString(parts[4]) || parts[5] != "revisions" || !uuidRE.MatchString(parts[6]) {
		return false
	}
	return parts[7] == "release.yaml" || parts[7] == "disabled.json"
}

func validHelmApplicationPath(binding Binding, value string) bool {
	if binding.Validate() != nil || binding.Kind != BindingPlatform || !strings.HasPrefix(value, binding.Prefix+"/") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(value, binding.Prefix+"/"), "/")
	if len(parts) != 4 || parts[0] != "argocd" || parts[1] != "helm-applications" || !uuidRE.MatchString(parts[2]) ||
		!strings.HasSuffix(parts[3], ".yaml") {
		return false
	}
	return uuidRE.MatchString(strings.TrimSuffix(parts[3], ".yaml"))
}

func validStrongETag(value string) bool {
	return len(value) == len(`"sha256:`)+64+1 && strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) && digestRE.MatchString(strings.Trim(value, `"`))
}

func validRelativePath(value string) bool {
	return value != "" && len(value) <= 1024 && utf8.ValidString(value) && !strings.HasPrefix(value, "/") && path.Clean(value) == value &&
		value != "." && value != ".." && !strings.HasPrefix(value, "../") && !strings.Contains(value, "//") && !strings.ContainsAny(value, "\x00\r\n\\")
}

func cloneMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	result := make(map[string]any, len(value))
	for key, item := range value {
		result[key] = cloneValue(item)
	}
	return result
}

func cloneValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneMap(typed)
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			result[index] = cloneValue(item)
		}
		return result
	default:
		return typed
	}
}

func ValidateCandidateHead(binding Binding, expected, actual string) error {
	if binding.Validate() != nil || !commitRE.MatchString(expected) || !commitRE.MatchString(actual) {
		return ErrInvalid
	}
	if expected != actual {
		return fmt.Errorf("%w: target ref changed", ErrConflict)
	}
	return nil
}
