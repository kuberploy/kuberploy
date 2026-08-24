// Package edge observes the exact, operator-approved Kubernetes edge runtime.
// It is intentionally read-only: no type in this package carries Kubernetes
// Secret data, provider credentials, arbitrary URLs, or mutation requests.
package edge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	RuntimeContract            = "edge-observer.v1"
	MaximumExternalDNSProfiles = 64
	MaximumTargets             = MaximumExternalDNSProfiles + 2
)

var (
	ErrInvalid         = errors.New("edge runtime metadata is invalid")
	ErrNotFound        = errors.New("edge runtime resource was not found")
	ErrUnavailable     = errors.New("edge runtime observation is unavailable")
	ErrConflict        = errors.New("edge runtime metadata conflicts with durable state")
	ErrLeaseLost       = errors.New("edge runtime lease was lost")
	ErrIdentityChanged = errors.New("edge runtime Kubernetes identity changed")
	ErrObservation     = errors.New("edge runtime observation does not match its approved profile")

	uuidPattern     = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	dnsLabelPattern = regexp.MustCompile(
		`^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$`,
	)
	versionPattern         = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)
	workerIDPattern        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$`)
	resourceVersionPattern = regexp.MustCompile(`^[A-Za-z0-9._:/+-]{1,128}$`)
	failureCodePattern     = regexp.MustCompile(`^[a-z][a-z0-9.-]{0,62}$`)
	txtOwnerPattern        = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9._]{0,126}[a-z0-9])?$`)
)

type Kind string

const (
	KindTraefik     Kind = "traefik"
	KindCertManager Kind = "cert-manager"
	KindExternalDNS Kind = "external-dns"
)

type ManagementMode string

const (
	ModeManaged ManagementMode = "managed"
	ModeAdopted ManagementMode = "adopted"
)

type RuntimeState string

const (
	StateAwaiting RuntimeState = "awaiting"
	StateReady    RuntimeState = "ready"
	StateFailed   RuntimeState = "failed"
)

var RequiredTraefikCRDs = []string{
	"ingressroutes.traefik.io",
	"ingressroutetcps.traefik.io",
	"ingressrouteudps.traefik.io",
	"middlewares.traefik.io",
	"middlewaretcps.traefik.io",
	"serverstransports.traefik.io",
	"serverstransporttcps.traefik.io",
	"tlsoptions.traefik.io",
	"tlsstores.traefik.io",
	"traefikservices.traefik.io",
}

var RequiredCertManagerCRDs = []string{
	"certificaterequests.cert-manager.io",
	"certificates.cert-manager.io",
	"challenges.acme.cert-manager.io",
	"clusterissuers.cert-manager.io",
	"issuers.cert-manager.io",
	"orders.acme.cert-manager.io",
}

type ObjectExpectation struct {
	Name       string `json:"name"`
	SpecDigest string `json:"specDigest"`
}

func (e ObjectExpectation) Validate() error {
	if !validObjectName(e.Name) || !validDigest(e.SpecDigest) {
		return ErrInvalid
	}
	return nil
}

type DeploymentExpectation struct {
	Name          string `json:"name"`
	ContainerName string `json:"containerName"`
	Image         string `json:"image"`
	SpecDigest    string `json:"specDigest"`
}

func (e DeploymentExpectation) Validate() error {
	if !validObjectName(e.Name) || !dnsLabelPattern.MatchString(e.ContainerName) || !validExactImage(e.Image) || !validDigest(e.SpecDigest) {
		return ErrInvalid
	}
	return nil
}

type TraefikProfile struct {
	Revision                 int64                 `json:"revision"`
	Mode                     ManagementMode        `json:"mode"`
	Namespace                string                `json:"namespace"`
	Version                  string                `json:"version"`
	Deployment               DeploymentExpectation `json:"deployment"`
	Service                  ObjectExpectation     `json:"service"`
	IngressClass             ObjectExpectation     `json:"ingressClass"`
	CRDs                     []ObjectExpectation   `json:"crds"`
	ProfileConfigMap         string                `json:"profileConfigMap"`
	RequireLoadBalancerReady bool                  `json:"requireLoadBalancerReady"`
	SSLIP                    *SSLIPProfile         `json:"sslip,omitempty"`
}

func (p TraefikProfile) Validate() error {
	if p.Revision <= 0 || !validMode(p.Mode) || !dnsLabelPattern.MatchString(p.Namespace) || !versionPattern.MatchString(p.Version) ||
		p.Deployment.Validate() != nil || p.Service.Validate() != nil || p.IngressClass.Validate() != nil ||
		!validObjectName(p.ProfileConfigMap) || validateObjectSet(p.CRDs, RequiredTraefikCRDs) != nil ||
		p.SSLIP != nil && p.SSLIP.Validate() != nil {
		return ErrInvalid
	}
	return nil
}

func (p TraefikProfile) ProfileData() map[string]string {
	if p.Validate() != nil {
		return nil
	}
	result := map[string]string{
		"management":                             string(p.Mode),
		"ingressClassName":                       p.IngressClass.Name,
		"httpRoutesSupported":                    "true",
		"letsEncryptRoutesRequireApprovedIssuer": "true",
		"customTLSSecretRoutesSupported":         "true",
		"runtimeNamespaceSelector":               "kuberploy.io/runtime-namespace=true",
	}
	if p.SSLIP != nil {
		result["sslipMode"] = string(p.SSLIP.Mode)
		result["sslipStaticPublicIPv4"] = p.SSLIP.StaticPublicIPv4
	}
	return result
}

func (p TraefikProfile) Digest() (string, error) { return profileDigest(KindTraefik, p) }

type CertManagerProfile struct {
	Revision                int64                   `json:"revision"`
	Mode                    ManagementMode          `json:"mode"`
	Namespace               string                  `json:"namespace"`
	Version                 string                  `json:"version"`
	Deployments             []DeploymentExpectation `json:"deployments"`
	CRDs                    []ObjectExpectation     `json:"crds"`
	ProfileConfigMap        string                  `json:"profileConfigMap"`
	IngressClassName        string                  `json:"ingressClassName"`
	ProductionIssuer        string                  `json:"productionIssuer"`
	ProductionServerClass   string                  `json:"productionServerClass"`
	ProductionSolverTypes   []string                `json:"productionSolverTypes"`
	ProductionDNS01Profiles []string                `json:"productionDNS01Profiles"`
	StagingIssuer           string                  `json:"stagingIssuer"`
	StagingServerClass      string                  `json:"stagingServerClass"`
	StagingSolverTypes      []string                `json:"stagingSolverTypes"`
	StagingDNS01Profiles    []string                `json:"stagingDNS01Profiles"`
}

// ApprovedACMEIssuer is the safe, operator-authored issuer identity exposed to
// tenant selectors. It deliberately omits account email, Kubernetes Secret
// names, DNS provider credentials, and raw solver configuration.
type ApprovedACMEIssuer struct {
	Name          string   `json:"name"`
	Environment   string   `json:"environment"`
	ServerClass   string   `json:"serverClass"`
	SolverTypes   []string `json:"solverTypes"`
	DNS01Profiles []string `json:"dns01Profiles"`
}

func (p CertManagerProfile) Validate() error {
	if p.Revision <= 0 || !validMode(p.Mode) || !dnsLabelPattern.MatchString(p.Namespace) || !versionPattern.MatchString(p.Version) ||
		len(p.Deployments) < 3 || len(p.Deployments) > 8 || validateDeploymentSet(p.Deployments) != nil ||
		validateObjectSet(p.CRDs, RequiredCertManagerCRDs) != nil || !validObjectName(p.ProfileConfigMap) ||
		!dnsLabelPattern.MatchString(p.IngressClassName) || !validOptionalObjectName(p.ProductionIssuer) ||
		!validOptionalObjectName(p.StagingIssuer) || p.ProductionIssuer == "" && p.StagingIssuer == "" ||
		p.ProductionIssuer != "" && p.ProductionIssuer == p.StagingIssuer {
		return ErrInvalid
	}
	if !validACMEIssuerRuntime(p.ProductionIssuer, p.ProductionServerClass, p.ProductionSolverTypes, p.ProductionDNS01Profiles, "letsencrypt-production") ||
		!validACMEIssuerRuntime(p.StagingIssuer, p.StagingServerClass, p.StagingSolverTypes, p.StagingDNS01Profiles, "letsencrypt-staging") {
		return ErrInvalid
	}
	return nil
}

func validACMEIssuerRuntime(name, serverClass string, solverTypes, dns01Profiles []string, expectedServerClass string) bool {
	if name == "" {
		return serverClass == "" && len(solverTypes) == 0 && len(dns01Profiles) == 0
	}
	if serverClass != expectedServerClass || len(solverTypes) == 0 {
		return false
	}
	switch {
	case slices.Equal(solverTypes, []string{"http01"}):
		return len(dns01Profiles) == 0
	case slices.Equal(solverTypes, []string{"dns01", "http01"}):
		if len(dns01Profiles) == 0 || len(dns01Profiles) > 32 || !slices.IsSorted(dns01Profiles) {
			return false
		}
		for index, profile := range dns01Profiles {
			if !dnsLabelPattern.MatchString(profile) || index > 0 && profile == dns01Profiles[index-1] {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func (p CertManagerProfile) ApprovedIssuers() []string {
	if p.Validate() != nil {
		return nil
	}
	values := []string{}
	if p.ProductionIssuer != "" {
		values = append(values, p.ProductionIssuer)
	}
	if p.StagingIssuer != "" {
		values = append(values, p.StagingIssuer)
	}
	slices.Sort(values)
	return values
}

func (p CertManagerProfile) ApprovedIssuerCatalog() []ApprovedACMEIssuer {
	if p.Validate() != nil {
		return nil
	}
	values := make([]ApprovedACMEIssuer, 0, 2)
	if p.ProductionIssuer != "" {
		values = append(values, ApprovedACMEIssuer{Name: p.ProductionIssuer, Environment: "production", ServerClass: p.ProductionServerClass,
			SolverTypes: slices.Clone(p.ProductionSolverTypes), DNS01Profiles: slices.Clone(p.ProductionDNS01Profiles)})
	}
	if p.StagingIssuer != "" {
		values = append(values, ApprovedACMEIssuer{Name: p.StagingIssuer, Environment: "staging", ServerClass: p.StagingServerClass,
			SolverTypes: slices.Clone(p.StagingSolverTypes), DNS01Profiles: slices.Clone(p.StagingDNS01Profiles)})
	}
	slices.SortFunc(values, func(left, right ApprovedACMEIssuer) int { return strings.Compare(left.Name, right.Name) })
	return values
}

func (p CertManagerProfile) ProfileData() map[string]string {
	if p.Validate() != nil {
		return nil
	}
	return map[string]string{
		"management":              string(p.Mode),
		"ingressClassName":        p.IngressClassName,
		"productionIssuer":        p.ProductionIssuer,
		"productionServerClass":   p.ProductionServerClass,
		"productionSolverTypes":   strings.Join(p.ProductionSolverTypes, ","),
		"productionDNS01Profiles": strings.Join(p.ProductionDNS01Profiles, ","),
		"stagingIssuer":           p.StagingIssuer,
		"stagingServerClass":      p.StagingServerClass,
		"stagingSolverTypes":      strings.Join(p.StagingSolverTypes, ","),
		"stagingDNS01Profiles":    strings.Join(p.StagingDNS01Profiles, ","),
	}
}

func (p CertManagerProfile) Digest() (string, error) { return profileDigest(KindCertManager, p) }

type ExternalDNSProfile struct {
	IntegrationID       string                `json:"integrationId"`
	Revision            int64                 `json:"revision"`
	Mode                ManagementMode        `json:"mode"`
	Namespace           string                `json:"namespace"`
	Version             string                `json:"version"`
	Deployment          DeploymentExpectation `json:"deployment"`
	ProfileConfigMap    string                `json:"profileConfigMap"`
	LabelFilter         string                `json:"labelFilter"`
	AnnotationFilter    string                `json:"annotationFilter"`
	ProviderKind        string                `json:"providerKind"`
	CredentialSecretRef string                `json:"credentialSecretRef"`
	ProviderConfigRef   string                `json:"providerConfigRef"`
	EgressConfigRef     string                `json:"egressConfigRef"`
	TXTOwnerID          string                `json:"txtOwnerId"`
	Policy              string                `json:"policy"`
	DomainFilters       []string              `json:"domainFilters"`
}

func (p ExternalDNSProfile) Validate() error {
	if !uuidPattern.MatchString(p.IntegrationID) || p.Revision <= 0 || !validMode(p.Mode) || !dnsLabelPattern.MatchString(p.Namespace) ||
		!versionPattern.MatchString(p.Version) || p.Deployment.Validate() != nil || !validObjectName(p.ProfileConfigMap) ||
		!validBoundedFilter(p.LabelFilter) || !validBoundedFilter(p.AnnotationFilter) || !validExternalDNSProvider(p.ProviderKind) ||
		!validExternalDNSReferences(p) || !validTXTOwner(p.TXTOwnerID) ||
		(p.Policy != "upsert-only" && p.Policy != "sync") || len(p.DomainFilters) < 1 || len(p.DomainFilters) > 64 {
		return ErrInvalid
	}
	for index, value := range p.DomainFilters {
		if !validDNSName(value) || index > 0 && p.DomainFilters[index-1] >= value {
			return ErrInvalid
		}
	}
	return nil
}

func (p ExternalDNSProfile) ProfileData() map[string]string {
	if p.Validate() != nil {
		return nil
	}
	return map[string]string{
		"management":          string(p.Mode),
		"labelFilter":         p.LabelFilter,
		"annotationFilter":    p.AnnotationFilter,
		"providerKind":        p.ProviderKind,
		"credentialSecretRef": p.CredentialSecretRef,
		"providerConfigRef":   p.ProviderConfigRef,
		"egressConfigRef":     p.EgressConfigRef,
		"txtOwnerId":          p.TXTOwnerID,
		"policy":              p.Policy,
		"domainFilters":       strings.Join(p.DomainFilters, ","),
	}
}

func (p ExternalDNSProfile) Digest() (string, error) { return profileDigest(KindExternalDNS, p) }

func (p ExternalDNSProfile) RequiredArguments() []string {
	if p.Validate() != nil {
		return nil
	}
	return []string{"--provider=" + p.ProviderKind, "--policy=" + p.Policy, "--registry=txt", "--txt-prefix=a-", "--txt-owner-id=" + p.TXTOwnerID}
}

func validExternalDNSProvider(value string) bool {
	return slices.Contains([]string{"aws", "azure", "cloudflare", "google", "rfc2136"}, value)
}

func validExternalDNSReferences(profile ExternalDNSProfile) bool {
	switch profile.Mode {
	case ModeManaged:
		return validObjectName(profile.CredentialSecretRef) && validObjectName(profile.ProviderConfigRef) && validObjectName(profile.EgressConfigRef)
	case ModeAdopted:
		return profile.CredentialSecretRef == "" && profile.ProviderConfigRef == "" && profile.EgressConfigRef == ""
	default:
		return false
	}
}

type DesiredTarget struct {
	Key                         string         `json:"key"`
	Kind                        Kind           `json:"kind"`
	Mode                        ManagementMode `json:"mode"`
	IntegrationID               string         `json:"integrationId,omitempty"`
	Namespace                   string         `json:"namespace"`
	ProfileConfigMap            string         `json:"profileConfigMap"`
	Revision                    int64          `json:"revision"`
	ExternalTXTOwnerID          string         `json:"externalTxtOwnerId,omitempty"`
	ExternalPolicy              string         `json:"externalPolicy,omitempty"`
	ExternalDomains             string         `json:"externalDomains,omitempty"`
	ExternalProviderKind        string         `json:"externalProviderKind,omitempty"`
	ExternalCredentialSecretRef string         `json:"externalCredentialSecretRef,omitempty"`
	ExternalProviderConfigRef   string         `json:"externalProviderConfigRef,omitempty"`
	ExternalEgressConfigRef     string         `json:"externalEgressConfigRef,omitempty"`
	DesiredDigest               string         `json:"desiredDigest"`
	RuntimeConfigDigest         string         `json:"runtimeConfigDigest"`
}

func (d DesiredTarget) Validate() error {
	validKey := d.Kind == KindTraefik && d.Key == "traefik" && d.IntegrationID == "" ||
		d.Kind == KindCertManager && d.Key == "cert-manager" && d.IntegrationID == "" ||
		d.Kind == KindExternalDNS && uuidPattern.MatchString(d.IntegrationID) && d.Key == "external-dns/"+d.IntegrationID
	externalMetadata := d.Kind == KindExternalDNS && validTXTOwner(d.ExternalTXTOwnerID) &&
		(d.ExternalPolicy == "upsert-only" || d.ExternalPolicy == "sync") && validDomainCSV(d.ExternalDomains) &&
		validExternalDNSProvider(d.ExternalProviderKind) && ((d.Mode == ModeManaged && validObjectName(d.ExternalCredentialSecretRef) &&
		validObjectName(d.ExternalProviderConfigRef) && validObjectName(d.ExternalEgressConfigRef)) ||
		(d.Mode == ModeAdopted && d.ExternalCredentialSecretRef == "" && d.ExternalProviderConfigRef == "" && d.ExternalEgressConfigRef == "")) ||
		d.Kind != KindExternalDNS && d.ExternalTXTOwnerID == "" && d.ExternalPolicy == "" && d.ExternalDomains == "" &&
			d.ExternalProviderKind == "" && d.ExternalCredentialSecretRef == "" && d.ExternalProviderConfigRef == "" && d.ExternalEgressConfigRef == ""
	if !validKey || !validMode(d.Mode) || !externalMetadata || !dnsLabelPattern.MatchString(d.Namespace) ||
		!validObjectName(d.ProfileConfigMap) || d.Revision <= 0 ||
		!validDigest(d.DesiredDigest) || !validDigest(d.RuntimeConfigDigest) {
		return ErrInvalid
	}
	return nil
}

type Target struct {
	DesiredTarget
	Active                   bool
	State                    RuntimeState
	NextObservationAt        time.Time
	LastObservedAt           *time.Time
	ObservedIdentityDigest   string
	ObservedResourceVersions string
	ConsecutiveFailures      int
	LastFailureCode          string
	LeaseOwner               string
	LeaseEpoch               int64
	LeaseUntil               *time.Time
	WorkerContract           string
	WorkerConfigDigest       string
	CreatedAt                time.Time
	UpdatedAt                time.Time
}

func (t Target) Validate() error {
	if t.DesiredTarget.Validate() != nil || t.State != StateAwaiting && t.State != StateReady && t.State != StateFailed ||
		t.NextObservationAt.Before(t.CreatedAt) || t.UpdatedAt.Before(t.CreatedAt) || t.ConsecutiveFailures < 0 || t.ConsecutiveFailures > 30 ||
		(t.LastFailureCode == "") != (t.ConsecutiveFailures == 0) || t.LastFailureCode != "" && !failureCodePattern.MatchString(t.LastFailureCode) {
		return ErrInvalid
	}
	observed := t.LastObservedAt != nil
	if observed != (t.ObservedIdentityDigest != "") || observed != (t.ObservedResourceVersions != "") ||
		t.ObservedIdentityDigest != "" && !validDigest(t.ObservedIdentityDigest) ||
		t.ObservedResourceVersions != "" && !validDigest(t.ObservedResourceVersions) ||
		t.State == StateReady && !observed {
		return ErrInvalid
	}
	leased := t.LeaseOwner != ""
	if leased != (t.LeaseUntil != nil) || leased != (t.WorkerContract != "") || leased != (t.WorkerConfigDigest != "") {
		return ErrInvalid
	}
	if leased && (!workerIDPattern.MatchString(t.LeaseOwner) || t.LeaseEpoch <= 0 || !t.LeaseUntil.After(t.UpdatedAt) ||
		t.WorkerContract != RuntimeContract || !validDigest(t.WorkerConfigDigest) || t.WorkerConfigDigest != t.RuntimeConfigDigest) {
		return ErrInvalid
	}
	return nil
}

type Lease struct {
	Target Target
	Owner  string
	Epoch  int64
	Until  time.Time
}

func (l Lease) Validate(now time.Time) error {
	if l.Target.Validate() != nil || !l.Target.Active || !workerIDPattern.MatchString(l.Owner) || l.Epoch <= 0 || !l.Until.After(now) ||
		l.Target.LeaseOwner != l.Owner || l.Target.LeaseEpoch != l.Epoch || l.Target.LeaseUntil == nil || !l.Target.LeaseUntil.Equal(l.Until) {
		return ErrInvalid
	}
	return nil
}

type ObservationReceipt struct {
	TargetKey             string
	DesiredDigest         string
	IdentityDigest        string
	ResourceVersionDigest string
	SSLIP                 *SSLIPIngressEndpoint
}

func (o ObservationReceipt) Validate(target DesiredTarget) error {
	if target.Validate() != nil || o.TargetKey != target.Key || o.DesiredDigest != target.DesiredDigest ||
		!validDigest(o.IdentityDigest) || !validDigest(o.ResourceVersionDigest) ||
		(o.SSLIP != nil && (target.Kind != KindTraefik || o.SSLIP.Validate() != nil)) {
		return ErrInvalid
	}
	if target.Kind == KindTraefik && o.SSLIP == nil {
		// A Traefik profile without sslip legitimately carries no endpoint; the
		// profile digest is checked by the caller before accepting the receipt.
	}
	return nil
}

type Readiness struct {
	WorkerID     string
	WorkerEpoch  int64
	Contract     string
	ConfigDigest string
	TargetCount  int
	StartedAt    time.Time
	ObservedAt   time.Time
	LeaseUntil   time.Time
}

func (r Readiness) Validate() error {
	if !workerIDPattern.MatchString(r.WorkerID) || r.WorkerEpoch <= 0 || r.Contract != RuntimeContract || !validDigest(r.ConfigDigest) ||
		r.TargetCount < 1 || r.TargetCount > MaximumTargets || r.StartedAt.IsZero() || r.ObservedAt.Before(r.StartedAt) ||
		!r.LeaseUntil.After(r.ObservedAt) {
		return ErrInvalid
	}
	return nil
}

type ObservationMismatch struct{ Code string }

func (e *ObservationMismatch) Error() string { return "edge observation mismatch: " + e.Code }
func (e *ObservationMismatch) Unwrap() error { return ErrObservation }

func mismatch(code string) error {
	if !failureCodePattern.MatchString(code) {
		return ErrObservation
	}
	return &ObservationMismatch{Code: code}
}

func observationFailureCode(err error) string {
	var mismatchError *ObservationMismatch
	if errors.As(err, &mismatchError) && failureCodePattern.MatchString(mismatchError.Code) {
		return mismatchError.Code
	}
	if errors.Is(err, ErrNotFound) {
		return "resource-not-found"
	}
	return "kubernetes-unavailable"
}

func profileDigest(kind Kind, profile any) (string, error) {
	encoded, err := json.Marshal(struct {
		Contract string `json:"contract"`
		Kind     Kind   `json:"kind"`
		Profile  any    `json:"profile"`
	}{RuntimeContract, kind, profile})
	if err != nil {
		return "", ErrInvalid
	}
	return digestBytes(encoded), nil
}

func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func digestStrings(values []string) string {
	values = slices.Clone(values)
	slices.Sort(values)
	hash := sha256.New()
	for _, value := range values {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func validateObjectSet(values []ObjectExpectation, required []string) error {
	if len(values) != len(required) {
		return ErrInvalid
	}
	for index, value := range values {
		if value.Validate() != nil || index > 0 && values[index-1].Name >= value.Name {
			return ErrInvalid
		}
	}
	for _, name := range required {
		if _, found := slices.BinarySearchFunc(values, name, func(value ObjectExpectation, target string) int {
			return strings.Compare(value.Name, target)
		}); !found {
			return ErrInvalid
		}
	}
	return nil
}

func validateDeploymentSet(values []DeploymentExpectation) error {
	for index, value := range values {
		if value.Validate() != nil || index > 0 && values[index-1].Name >= value.Name {
			return ErrInvalid
		}
	}
	return nil
}

func validMode(value ManagementMode) bool { return value == ModeManaged || value == ModeAdopted }
func validDigest(value string) bool {
	return len(value) == 71 && strings.HasPrefix(value, "sha256:") && strings.Trim(value[7:], "0123456789abcdef") == ""
}
func validObjectName(value string) bool {
	return validDNSName(value)
}
func validOptionalObjectName(value string) bool { return value == "" || validObjectName(value) }
func validExactImage(value string) bool {
	return len(value) >= 3 && len(value) <= 512 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n \t") &&
		strings.Contains(value, "/") && (strings.Contains(value, "@sha256:") || strings.Contains(value, ":v"))
}
func validBoundedFilter(value string) bool {
	return len(value) <= 512 && strings.TrimSpace(value) == value && !hasControl(value)
}
func validTXTOwner(value string) bool {
	return len(value) >= 1 && len(value) <= 128 && strings.TrimSpace(value) == value &&
		strings.ToLower(value) == value && txtOwnerPattern.MatchString(value)
}
func validDNSName(value string) bool {
	if len(value) < 1 || len(value) > 253 || value != strings.ToLower(value) || strings.HasPrefix(value, ".") || strings.HasSuffix(value, ".") {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if !dnsLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
}

func validDomainCSV(value string) bool {
	parts := strings.Split(value, ",")
	if len(parts) < 1 || len(parts) > 64 {
		return false
	}
	for index, part := range parts {
		if !validDNSName(part) || index > 0 && parts[index-1] >= part {
			return false
		}
	}
	return true
}

func hasControl(value string) bool {
	return strings.IndexFunc(value, func(character rune) bool {
		return character < 0x20 || character == 0x7f
	}) >= 0
}

func cloneTarget(target Target) Target {
	if target.LastObservedAt != nil {
		value := *target.LastObservedAt
		target.LastObservedAt = &value
	}
	if target.LeaseUntil != nil {
		value := *target.LeaseUntil
		target.LeaseUntil = &value
	}
	return target
}

func targetMapKey(key string, revision int64) string { return fmt.Sprintf("%s\x00%d", key, revision) }
