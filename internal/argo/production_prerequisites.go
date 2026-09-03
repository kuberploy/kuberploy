package argo

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/githubapp"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

const (
	PlatformRootApplicationName      = "kuberploy-platform-root"
	PlatformBootstrapProjectName     = "kuberploy-platform-bootstrap"
	RepositoryCredentialFieldManager = "kuberploy-argo-repository-credentials"
	MaximumArgoRepositoryBindings    = 1_000
	DefaultArgoCatalogMaximumAge     = 15 * time.Minute
)

var (
	ErrArgoRuntimePrerequisiteNotReady = errors.New("Argo runtime prerequisite is not ready")
	ErrRepositoryCredentialNotReady    = errors.New("Argo repository credential is not ready")
	ErrPlatformRootNotReady            = errors.New("Argo platform root Application is not ready")
)

// RepositoryCredentialName is the sole name family the runtime may mutate.
// Binding IDs are already immutable provider-catalog authority, so no caller,
// repository owner, URL, namespace, or Kubernetes Secret name enters it.
func RepositoryCredentialName(bindingID string) (string, error) {
	if !uuidRE.MatchString(bindingID) {
		return "", ErrInvalid
	}
	return "kuberploy-repo-" + strings.ReplaceAll(bindingID, "-", ""), nil
}

// RepositoryBindingAuthority is a metadata-only catalog observation. An
// unauthorized row is retained so the controller can revoke its deterministic
// credential; it must never be silently omitted from the reconciliation set.
type RepositoryBindingAuthority struct {
	Binding            gitprojection.Binding
	Authorized         bool
	RevocationRequired bool
	CatalogObservedAt  time.Time
}

func (a RepositoryBindingAuthority) validate(appID int64, now time.Time, maximumAge time.Duration) error {
	if a.Binding.Validate() != nil || a.Binding.CredentialMode != gitprojection.CredentialGitHubApp || appID <= 0 ||
		now.IsZero() || maximumAge <= 0 || maximumAge > time.Hour {
		return ErrInvalid
	}
	if a.Authorized {
		if a.RevocationRequired || a.CatalogObservedAt.IsZero() || a.CatalogObservedAt.After(now) ||
			a.CatalogObservedAt.Before(now.Add(-maximumAge)) {
			return ErrArgoRuntimePrerequisiteNotReady
		}
		return nil
	}
	if a.RevocationRequired {
		if !a.CatalogObservedAt.IsZero() {
			return ErrInvalid
		}
		return nil
	}
	// A matching catalog row that has never been verified or has aged out is
	// not authority to delete a still-valid credential. It blocks readiness
	// until the provider catalog refreshes, but only explicit identity,
	// lifecycle, App-ID, or permission revocation enters the delete path.
	if !a.CatalogObservedAt.IsZero() && a.CatalogObservedAt.After(now) {
		return ErrInvalid
	}
	if a.CatalogObservedAt.IsZero() || !a.CatalogObservedAt.Before(now.Add(-maximumAge)) {
		return ErrArgoRuntimePrerequisiteNotReady
	}
	return ErrArgoRuntimePrerequisiteNotReady
}

// RuntimeBindingCatalog returns every GitHub App binding in this installation,
// including catalog-deactivated rows. Implementations must compare provider
// installation/repository IDs, owner/name, App ID, lifecycle, and required
// permissions against the verified linked catalog.
type RuntimeBindingCatalog interface {
	ArgoRepositoryBindings(context.Context, int64, string, time.Time, time.Duration) ([]RepositoryBindingAuthority, error)
}

// RuntimeBindingCatalogRefresher lets a successful exact provider proof renew
// the durable catalog observation. Without this, every readiness heartbeat
// re-runs the full provider verification set once the catalog ages out.
type RuntimeBindingCatalogRefresher interface {
	MarkArgoRepositoryBindingsVerified(context.Context, int64, []gitprojection.Binding, time.Time) error
}

// GitHubAppPrivateKeySource returns a fresh caller-owned key buffer. The
// controller clears it after the bounded apply set and never stores it in Git,
// PostgreSQL, observations, errors, or readiness identities.
type GitHubAppPrivateKeySource interface {
	ReadGitHubAppPrivateKey(context.Context) ([]byte, error)
}

type ProjectedGitHubAppPrivateKeySource struct {
	Reader    githubapp.SecretReader
	Reference githubapp.SecretRef
}

func NewProjectedGitHubAppPrivateKeySource(config githubapp.Config, reader githubapp.SecretReader) (*ProjectedGitHubAppPrivateKeySource, error) {
	if config.Validate() != nil || reader == nil {
		return nil, ErrInvalid
	}
	return &ProjectedGitHubAppPrivateKeySource{Reader: reader, Reference: config.PrivateKeySecret}, nil
}

func (s *ProjectedGitHubAppPrivateKeySource) ReadGitHubAppPrivateKey(ctx context.Context) ([]byte, error) {
	if s == nil || s.Reader == nil {
		return nil, ErrInvalid
	}
	value, err := s.Reader.ReadSecret(ctx, s.Reference)
	if err != nil || len(value) < 1 || len(value) > 128<<10 {
		clearBytes(value)
		if err != nil {
			return nil, err
		}
		return nil, ErrRepositoryCredentialNotReady
	}
	return value, nil
}

// RepositoryCredentialApply is intentionally not JSON-marshaled or returned
// from public APIs. PrivateKey is a short-lived caller-owned buffer.
type RepositoryCredentialApply struct {
	Namespace      string
	Name           string
	BindingID      string
	GitHubAppID    int64
	InstallationID int64
	RepositoryURL  string
	PrivateKey     []byte
	SpecDigest     string
}

func newRepositoryCredentialApply(namespace string, appID int64, binding gitprojection.Binding, privateKey []byte) (RepositoryCredentialApply, error) {
	name, err := RepositoryCredentialName(binding.ID)
	if err != nil || !kubeRE.MatchString(namespace) || appID <= 0 || binding.Validate() != nil ||
		binding.CredentialMode != gitprojection.CredentialGitHubApp || len(privateKey) < 1 || len(privateKey) > 128<<10 {
		return RepositoryCredentialApply{}, ErrInvalid
	}
	remote, err := binding.Repository.CanonicalRemote()
	if err != nil {
		return RepositoryCredentialApply{}, ErrInvalid
	}
	keyDigest := sha256.Sum256(privateKey)
	canonical := struct {
		Contract       string `json:"contract"`
		Namespace      string `json:"namespace"`
		Name           string `json:"name"`
		BindingID      string `json:"bindingId"`
		GitHubAppID    int64  `json:"githubAppId"`
		InstallationID int64  `json:"installationId"`
		RepositoryURL  string `json:"repositoryUrl"`
		PrivateKeyHash string `json:"privateKeyHash"`
	}{"argo-repository-credential-v1", namespace, name, binding.ID, appID, binding.Repository.InstallationID, remote,
		"sha256:" + hex.EncodeToString(keyDigest[:])}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return RepositoryCredentialApply{}, ErrInvalid
	}
	digest := sha256.Sum256(encoded)
	return RepositoryCredentialApply{Namespace: namespace, Name: name, BindingID: binding.ID, GitHubAppID: appID,
		InstallationID: binding.Repository.InstallationID, RepositoryURL: remote, PrivateKey: privateKey,
		SpecDigest: "sha256:" + hex.EncodeToString(digest[:])}, nil
}

func (a RepositoryCredentialApply) validate() error {
	if !kubeRE.MatchString(a.Namespace) || !kubeRE.MatchString(a.Name) || !uuidRE.MatchString(a.BindingID) ||
		a.GitHubAppID <= 0 || a.InstallationID <= 0 || len(a.PrivateKey) < 1 || len(a.PrivateKey) > 128<<10 ||
		!digestRE.MatchString(a.SpecDigest) {
		return ErrInvalid
	}
	name, err := RepositoryCredentialName(a.BindingID)
	if err != nil || name != a.Name || !strings.HasPrefix(a.RepositoryURL, "https://github.com/") ||
		strings.ContainsAny(a.RepositoryURL, "?#@\x00\r\n") || !strings.HasSuffix(a.RepositoryURL, ".git") {
		return ErrInvalid
	}
	return nil
}

type RepositoryCredentialObservation struct {
	BindingID       string
	Namespace       string
	Name            string
	UID             string
	ResourceVersion string
	SpecDigest      string
	ObservedAt      time.Time
}

func (o RepositoryCredentialObservation) validateFor(apply RepositoryCredentialApply, now time.Time) error {
	if apply.validate() != nil || o.BindingID != apply.BindingID || o.Namespace != apply.Namespace || o.Name != apply.Name ||
		!uuidRE.MatchString(o.UID) || o.ResourceVersion == "" || len(o.ResourceVersion) > 128 ||
		stringsContainsControl(o.ResourceVersion) || o.SpecDigest != apply.SpecDigest || o.ObservedAt.IsZero() || o.ObservedAt.After(now) {
		return ErrRepositoryCredentialNotReady
	}
	return nil
}

// RepositoryCredentialKubernetes has no generic Secret read/list surface.
// Apply must use the exact deterministic name and return metadata only. Delete
// exists solely for catalog revocation of that same closed name family.
type RepositoryCredentialKubernetes interface {
	ApplyRepositoryCredential(context.Context, RepositoryCredentialApply, time.Time) (RepositoryCredentialObservation, error)
	DeleteRepositoryCredential(context.Context, string, string, string, time.Time) (RepositoryCredentialRevocationObservation, error)
}

// RepositoryCredentialRevocationObservation is the bounded acknowledgement
// for an exact deterministic delete. A successful DELETE response only starts
// deletion; readiness resumes after a later DELETE observes NotFound.
type RepositoryCredentialRevocationObservation struct {
	BindingID  string
	Namespace  string
	Name       string
	Absent     bool
	ObservedAt time.Time
}

func (o RepositoryCredentialRevocationObservation) validate(bindingID, namespace, name string, now time.Time) error {
	if o.BindingID != bindingID || o.Namespace != namespace || o.Name != name || !uuidRE.MatchString(bindingID) ||
		o.ObservedAt.IsZero() || o.ObservedAt.After(now) {
		return ErrRepositoryCredentialNotReady
	}
	return nil
}

type RepositoryCredentialSetObservation struct {
	PlatformBindingID string
	Observed          []RepositoryCredentialObservation
	ObservedAt        time.Time
}

func (o RepositoryCredentialSetObservation) validate(platformBindingID string, expected int, now time.Time) error {
	if o.PlatformBindingID != platformBindingID || !uuidRE.MatchString(platformBindingID) || len(o.Observed) != expected ||
		expected < 1 || expected > MaximumArgoRepositoryBindings || o.ObservedAt.IsZero() || o.ObservedAt.After(now) {
		return ErrRepositoryCredentialNotReady
	}
	seen := make(map[string]struct{}, len(o.Observed))
	for _, observation := range o.Observed {
		if !uuidRE.MatchString(observation.BindingID) || observation.ObservedAt.After(o.ObservedAt) {
			return ErrRepositoryCredentialNotReady
		}
		if _, duplicate := seen[observation.BindingID]; duplicate {
			return ErrRepositoryCredentialNotReady
		}
		seen[observation.BindingID] = struct{}{}
	}
	if _, found := seen[platformBindingID]; !found {
		return ErrRepositoryCredentialNotReady
	}
	return nil
}

type RepositoryCredentialController struct {
	Namespace   string
	GitHubAppID int64
	Keys        GitHubAppPrivateKeySource
	Kubernetes  RepositoryCredentialKubernetes
}

func (c *RepositoryCredentialController) Reconcile(ctx context.Context, authorities []RepositoryBindingAuthority, platformBindingID string, now time.Time, maximumCatalogAge time.Duration) (RepositoryCredentialSetObservation, error) {
	if c == nil || !kubeRE.MatchString(c.Namespace) || c.GitHubAppID <= 0 || c.Keys == nil || c.Kubernetes == nil ||
		!uuidRE.MatchString(platformBindingID) || now.IsZero() || maximumCatalogAge <= 0 || maximumCatalogAge > time.Hour ||
		len(authorities) < 1 || len(authorities) > MaximumArgoRepositoryBindings {
		return RepositoryCredentialSetObservation{}, ErrInvalid
	}
	values := slices.Clone(authorities)
	slices.SortFunc(values, func(left, right RepositoryBindingAuthority) int {
		return strings.Compare(left.Binding.ID, right.Binding.ID)
	})
	seen := make(map[string]struct{}, len(values))
	active := make([]RepositoryBindingAuthority, 0, len(values))
	revocationPending := false
	for _, authority := range values {
		if _, duplicate := seen[authority.Binding.ID]; duplicate {
			return RepositoryCredentialSetObservation{}, ErrInvalid
		}
		seen[authority.Binding.ID] = struct{}{}
		validationErr := authority.validate(c.GitHubAppID, now, maximumCatalogAge)
		if authority.Authorized {
			if validationErr != nil {
				return RepositoryCredentialSetObservation{}, validationErr
			}
			active = append(active, authority)
			continue
		}
		if authority.RevocationRequired {
			if validationErr != nil {
				return RepositoryCredentialSetObservation{}, validationErr
			}
			name, nameErr := RepositoryCredentialName(authority.Binding.ID)
			if nameErr != nil {
				return RepositoryCredentialSetObservation{}, nameErr
			}
			revocation, deleteErr := c.Kubernetes.DeleteRepositoryCredential(ctx, c.Namespace, name, authority.Binding.ID, now.UTC())
			if deleteErr != nil {
				return RepositoryCredentialSetObservation{}, deleteErr
			}
			if revocation.validate(authority.Binding.ID, c.Namespace, name, now.UTC()) != nil {
				return RepositoryCredentialSetObservation{}, ErrRepositoryCredentialNotReady
			}
			if !revocation.Absent {
				revocationPending = true
			}
			continue
		}
		if validationErr != nil {
			return RepositoryCredentialSetObservation{}, validationErr
		}
		return RepositoryCredentialSetObservation{}, ErrRepositoryCredentialNotReady
	}
	if _, found := seen[platformBindingID]; !found {
		return RepositoryCredentialSetObservation{}, ErrRepositoryCredentialNotReady
	}
	privateKey, err := c.Keys.ReadGitHubAppPrivateKey(ctx)
	if err != nil {
		return RepositoryCredentialSetObservation{}, err
	}
	defer clearBytes(privateKey)
	observed := make([]RepositoryCredentialObservation, 0, len(active))
	for _, authority := range active {
		apply, applyErr := newRepositoryCredentialApply(c.Namespace, c.GitHubAppID, authority.Binding, privateKey)
		if applyErr != nil {
			return RepositoryCredentialSetObservation{}, applyErr
		}
		observation, applyErr := c.Kubernetes.ApplyRepositoryCredential(ctx, apply, now)
		if applyErr != nil {
			return RepositoryCredentialSetObservation{}, applyErr
		}
		if applyErr = observation.validateFor(apply, now); applyErr != nil {
			return RepositoryCredentialSetObservation{}, applyErr
		}
		observed = append(observed, observation)
	}
	if revocationPending {
		return RepositoryCredentialSetObservation{}, ErrRepositoryCredentialNotReady
	}
	result := RepositoryCredentialSetObservation{PlatformBindingID: platformBindingID, Observed: observed, ObservedAt: now.UTC()}
	if result.validate(platformBindingID, len(active), now) != nil {
		return RepositoryCredentialSetObservation{}, ErrRepositoryCredentialNotReady
	}
	return result, nil
}

type PlatformRootApplicationExpectation struct {
	Namespace                string
	Name                     string
	Project                  string
	RepositoryURL            string
	TargetRevision           string
	Path                     string
	RepositoryCredentialName string
	ExpectedGitRevision      string
	SpecDigest               string
}

func NewPlatformRootApplicationExpectation(identity DesiredStateRuntimeIdentity, binding gitprojection.Binding, head gitprojection.VerifiedHead) (PlatformRootApplicationExpectation, error) {
	credentialName, err := RepositoryCredentialName(binding.ID)
	if identity.Validate() != nil || err != nil || identity.RootApplicationName != PlatformRootApplicationName ||
		identity.RepositorySecretName != credentialName || binding.Validate() != nil || binding.Kind != gitprojection.BindingPlatform ||
		binding.ID != identity.PlatformBindingID || binding.CredentialMode != gitprojection.CredentialGitHubApp ||
		head.ValidateFor(binding) != nil {
		return PlatformRootApplicationExpectation{}, ErrInvalid
	}
	remote, err := binding.Repository.CanonicalRemote()
	if err != nil {
		return PlatformRootApplicationExpectation{}, ErrInvalid
	}
	expectation := PlatformRootApplicationExpectation{Namespace: identity.ArgoNamespace, Name: identity.RootApplicationName,
		Project: PlatformBootstrapProjectName, RepositoryURL: remote, TargetRevision: binding.TargetRef,
		Path: path.Join(binding.Prefix, "argocd"), RepositoryCredentialName: credentialName, ExpectedGitRevision: head.Commit}
	expectation.SpecDigest, err = expectation.expectedSpecDigest()
	if err != nil {
		return PlatformRootApplicationExpectation{}, err
	}
	return expectation, nil
}

func (e PlatformRootApplicationExpectation) expectedSpecDigest() (string, error) {
	if !kubeRE.MatchString(e.Namespace) || e.Name != PlatformRootApplicationName || e.Project != PlatformBootstrapProjectName ||
		!commitRE.MatchString(e.ExpectedGitRevision) || !strings.HasPrefix(e.RepositoryURL, "https://github.com/") ||
		!strings.HasPrefix(e.TargetRevision, "refs/heads/") || e.Path == "" || strings.Contains(e.Path, "..") ||
		!kubeRE.MatchString(e.RepositoryCredentialName) {
		return "", ErrInvalid
	}
	encoded, err := json.Marshal(platformRootApplicationSpec(e))
	if err != nil {
		return "", ErrInvalid
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

type PlatformRootApplicationObservation struct {
	Namespace        string
	Name             string
	UID              string
	ResourceVersion  string
	SpecDigest       string
	ObservedRevision string
	SyncStatus       string
	HealthStatus     string
	ObservedAt       time.Time
}

func (o PlatformRootApplicationObservation) validateFor(expectation PlatformRootApplicationExpectation, now time.Time) error {
	expectedDigest, err := expectation.expectedSpecDigest()
	if err != nil || expectedDigest != expectation.SpecDigest || o.Namespace != expectation.Namespace || o.Name != expectation.Name ||
		!uuidRE.MatchString(o.UID) || o.ResourceVersion == "" || len(o.ResourceVersion) > 128 || stringsContainsControl(o.ResourceVersion) ||
		o.SpecDigest != expectation.SpecDigest || o.ObservedRevision != expectation.ExpectedGitRevision || o.SyncStatus != "Synced" ||
		o.HealthStatus != "Healthy" || o.ObservedAt.IsZero() || o.ObservedAt.After(now) {
		return ErrPlatformRootNotReady
	}
	return nil
}

// validateForCascade proves that the platform root has applied the exact
// provider revision without requiring child workload health. Disable must
// remain possible while a protected child is Progressing or Degraded.
func (o PlatformRootApplicationObservation) validateForCascade(expectation PlatformRootApplicationExpectation, now time.Time) error {
	expectedDigest, err := expectation.expectedSpecDigest()
	if err != nil || expectedDigest != expectation.SpecDigest || o.Namespace != expectation.Namespace || o.Name != expectation.Name ||
		!uuidRE.MatchString(o.UID) || o.ResourceVersion == "" || len(o.ResourceVersion) > 128 || stringsContainsControl(o.ResourceVersion) ||
		o.SpecDigest != expectation.SpecDigest || o.ObservedRevision != expectation.ExpectedGitRevision || o.SyncStatus != "Synced" ||
		o.ObservedAt.IsZero() || o.ObservedAt.After(now) {
		return ErrPlatformRootNotReady
	}
	return nil
}

type PlatformRootApplicationSource interface {
	ObservePlatformRootApplication(context.Context, PlatformRootApplicationExpectation, time.Time) (PlatformRootApplicationObservation, error)
}

type PlatformRootCascadeSource interface {
	ObservePlatformRootApplicationForCascade(context.Context, PlatformRootApplicationExpectation, time.Time) (PlatformRootApplicationObservation, error)
}

type ProductionPrerequisiteProof struct {
	PlatformBindingID string
	PlatformHead      string
	ProtectionDigest  string
	CredentialCount   int
	RootUID           string
	RootSpecDigest    string
	ObservedAt        time.Time
}

func (p ProductionPrerequisiteProof) validate(identity DesiredStateRuntimeIdentity, now time.Time) error {
	if identity.Validate() != nil || p.PlatformBindingID != identity.PlatformBindingID || !commitRE.MatchString(p.PlatformHead) ||
		!digestRE.MatchString(p.ProtectionDigest) || p.CredentialCount < 1 || p.CredentialCount > MaximumArgoRepositoryBindings || !uuidRE.MatchString(p.RootUID) ||
		!digestRE.MatchString(p.RootSpecDigest) || p.ObservedAt.IsZero() || p.ObservedAt.After(now) {
		return ErrArgoRuntimePrerequisiteNotReady
	}
	return nil
}

type ProductionPrerequisiteObserver interface {
	ObserveProductionPrerequisites(context.Context, time.Time) (ProductionPrerequisiteProof, error)
}

type PlatformRepositoryProtectionObservation struct {
	BindingID    string
	TargetRef    string
	Head         string
	PolicyDigest string
	ObservedAt   time.Time
}

func (o PlatformRepositoryProtectionObservation) validateFor(binding gitprojection.Binding, head gitprojection.VerifiedHead, now time.Time) error {
	if binding.Validate() != nil || head.ValidateFor(binding) != nil || o.BindingID != binding.ID || o.TargetRef != binding.TargetRef ||
		o.Head != head.Commit || !digestRE.MatchString(o.PolicyDigest) || o.ObservedAt.IsZero() || o.ObservedAt.After(now) {
		return ErrArgoRuntimePrerequisiteNotReady
	}
	return nil
}

// PlatformRepositoryProtectionVerifier proves that direct users/bots cannot
// bypass the protected writer on the branch followed by the bootstrap root.
// A mutable-branch root is unavoidable for app-of-apps bootstrap; therefore a
// fresh exact provider ruleset/branch-protection proof is mandatory.
type PlatformRepositoryProtectionVerifier interface {
	VerifyPlatformRepositoryProtection(context.Context, gitprojection.Binding, gitprojection.VerifiedHead, time.Time) (PlatformRepositoryProtectionObservation, error)
}

type FoundationReadinessProbe interface {
	Probe(context.Context) error
}

// ProductionPrerequisites is the fail-closed composition used before every
// durable readiness heartbeat. It proves the exact central platform binding,
// reconciles/revokes the closed repository credential set, provider-verifies
// the platform ref, and observes the exact synced root Application spec/UID.
// Child App health is independent and cannot block unrelated desired-state
// publication through the shared app-of-apps root.
type ProductionPrerequisites struct {
	Identity          DesiredStateRuntimeIdentity
	Catalog           RuntimeBindingCatalog
	Credentials       *RepositoryCredentialController
	Provider          gitprojection.HeadVerifier
	Protection        PlatformRepositoryProtectionVerifier
	RootApplications  PlatformRootCascadeSource
	RootRefresher     PlatformRootRefresher
	Foundation        FoundationReadinessProbe
	MaximumCatalogAge time.Duration
	Now               func() time.Time
}

func (p *ProductionPrerequisites) ObserveProductionPrerequisites(ctx context.Context, now time.Time) (ProductionPrerequisiteProof, error) {
	maximumAge := p.MaximumCatalogAge
	if maximumAge == 0 {
		maximumAge = DefaultArgoCatalogMaximumAge
	}
	if p == nil || p.Identity.Validate() != nil || p.Catalog == nil || p.Credentials == nil || p.Provider == nil || p.Protection == nil ||
		p.RootApplications == nil || p.RootRefresher == nil || p.Foundation == nil || now.IsZero() || maximumAge <= 0 || maximumAge > time.Hour {
		return ProductionPrerequisiteProof{}, ErrInvalid
	}
	if err := p.Foundation.Probe(ctx); err != nil {
		return ProductionPrerequisiteProof{}, errors.Join(ErrArgoRuntimePrerequisiteNotReady, err)
	}
	authorities, err := p.Catalog.ArgoRepositoryBindings(ctx, p.Identity.GitHubAppID, p.Identity.PlatformBindingID,
		now.UTC(), maximumAge)
	if err != nil {
		return ProductionPrerequisiteProof{}, err
	}
	verifiedHeads := make(map[string]gitprojection.VerifiedHead)
	verifiedBindings := make([]gitprojection.Binding, 0, len(authorities))
	authorities = slices.Clone(authorities)
	for index := range authorities {
		authority := &authorities[index]
		if authority.Authorized || authority.RevocationRequired {
			continue
		}
		// A matching but aged catalog row is not accepted by itself. Re-prove
		// the exact installation, repository identity, permissions, and ref via
		// the provider before allowing the credential controller to use it.
		head, verifyErr := p.Provider.VerifyTargetHead(ctx, authority.Binding, gitprojection.ObservationPoll)
		if verifyErr != nil {
			return ProductionPrerequisiteProof{}, verifyErr
		}
		if head.ValidateFor(authority.Binding) != nil || head.Commit != authority.Binding.TargetHeadRevision {
			return ProductionPrerequisiteProof{}, ErrArgoRuntimePrerequisiteNotReady
		}
		authority.Authorized = true
		authority.CatalogObservedAt = now.UTC()
		verifiedHeads[authority.Binding.ID] = head
		verifiedBindings = append(verifiedBindings, authority.Binding)
	}
	credentialObservation, err := p.Credentials.Reconcile(ctx, authorities, p.Identity.PlatformBindingID, now.UTC(), maximumAge)
	if err != nil {
		return ProductionPrerequisiteProof{}, err
	}
	var platform gitprojection.Binding
	platformCount := 0
	for _, authority := range authorities {
		if authority.Binding.ID == p.Identity.PlatformBindingID {
			platform, platformCount = authority.Binding, platformCount+1
		}
	}
	if platformCount != 1 || platform.Kind != gitprojection.BindingPlatform ||
		platform.CredentialMode != gitprojection.CredentialGitHubApp || platform.TargetHeadRevision == "" ||
		(platform.State != gitprojection.BindingReady && platform.State != gitprojection.BindingIndexing) {
		return ProductionPrerequisiteProof{}, ErrArgoRuntimePrerequisiteNotReady
	}
	head, found := verifiedHeads[platform.ID]
	if !found {
		head, err = p.Provider.VerifyTargetHead(ctx, platform, gitprojection.ObservationPoll)
	}
	if err != nil || head.ValidateFor(platform) != nil || head.Commit != platform.TargetHeadRevision {
		if err != nil {
			return ProductionPrerequisiteProof{}, err
		}
		return ProductionPrerequisiteProof{}, ErrArgoRuntimePrerequisiteNotReady
	}
	verifiedHeads[platform.ID] = head
	if refresher, ok := p.Catalog.(RuntimeBindingCatalogRefresher); ok && len(verifiedBindings) > 0 {
		if err = refresher.MarkArgoRepositoryBindingsVerified(ctx, p.Identity.GitHubAppID, verifiedBindings, now.UTC()); err != nil {
			return ProductionPrerequisiteProof{}, err
		}
	}
	protection, err := p.Protection.VerifyPlatformRepositoryProtection(ctx, platform, head, now.UTC())
	if err != nil {
		return ProductionPrerequisiteProof{}, err
	}
	if err = protection.validateFor(platform, head, now.UTC()); err != nil {
		return ProductionPrerequisiteProof{}, err
	}
	expectation, err := NewPlatformRootApplicationExpectation(p.Identity, platform, head)
	if err != nil {
		return ProductionPrerequisiteProof{}, err
	}
	root, err := p.observePlatformRoot(ctx, expectation, now.UTC())
	if errors.Is(err, ErrPlatformRootNotReady) {
		// A verified Git write advances the shared branch before Argo has
		// necessarily refreshed its cached target revision. Recover that exact
		// handoff with the same closed metadata-only hard refresh used by the
		// durable writer. Readiness remains fenced until the subsequent exact
		// observation proves the new provider head Synced. Child App health is
		// not a root publication prerequisite.
		if refreshErr := p.RootRefresher.RefreshPlatformRootApplication(ctx, expectation, now.UTC()); refreshErr != nil {
			return ProductionPrerequisiteProof{}, errors.Join(ErrArgoRuntimePrerequisiteNotReady, refreshErr)
		}
		root, err = p.observePlatformRoot(ctx, expectation, now.UTC())
	}
	if err != nil {
		return ProductionPrerequisiteProof{}, err
	}
	proof := ProductionPrerequisiteProof{PlatformBindingID: platform.ID, PlatformHead: head.Commit,
		ProtectionDigest: protection.PolicyDigest, CredentialCount: len(credentialObservation.Observed), RootUID: root.UID,
		RootSpecDigest: root.SpecDigest, ObservedAt: now.UTC()}
	if proof.validate(p.Identity, now.UTC()) != nil {
		return ProductionPrerequisiteProof{}, ErrArgoRuntimePrerequisiteNotReady
	}
	return proof, nil
}

func (p *ProductionPrerequisites) observePlatformRoot(ctx context.Context, expectation PlatformRootApplicationExpectation, now time.Time) (PlatformRootApplicationObservation, error) {
	root, err := p.RootApplications.ObservePlatformRootApplicationForCascade(ctx, expectation, now)
	if err != nil {
		return PlatformRootApplicationObservation{}, err
	}
	if err = root.validateForCascade(expectation, now); err != nil {
		return PlatformRootApplicationObservation{}, err
	}
	return root, nil
}

// platformRootApplicationSpec is shared with the Kubernetes decoder so exact
// spec comparison cannot drift from the expectation digest.
func platformRootApplicationSpec(expectation PlatformRootApplicationExpectation) rootApplicationSpecWire {
	return rootApplicationSpecWire{
		Project: expectation.Project,
		Source: rootApplicationSourceWire{RepoURL: expectation.RepositoryURL, TargetRevision: expectation.TargetRevision,
			Path: expectation.Path, Directory: rootApplicationDirectoryWire{Recurse: true}},
		Destination: rootApplicationDestinationWire{Server: InClusterServer, Namespace: expectation.Namespace},
		SyncPolicy: rootApplicationSyncPolicyWire{
			Automated:   rootApplicationAutomatedWire{AllowEmpty: false, Prune: true, SelfHeal: true},
			SyncOptions: []string{"CreateNamespace=false", "PrunePropagationPolicy=foreground", "RespectIgnoreDifferences=true", "ServerSideApply=true"},
		},
	}
}

func clearBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

// constantTimeBytesEqual exists for Kubernetes adapters that must compare a
// response buffer without turning credential material into a string.
func constantTimeBytesEqual(left, right []byte) bool {
	return len(left) == len(right) && subtle.ConstantTimeCompare(left, right) == 1
}

func canonicalPositiveInteger(value int64) string { return strconv.FormatInt(value, 10) }
