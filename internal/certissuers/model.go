package certissuers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/mail"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	Contract              = "cert-manager-cluster-issuer-profile.v1"
	LetsEncryptProduction = "https://acme-v02.api.letsencrypt.org/directory"
	LetsEncryptStaging    = "https://acme-staging-v02.api.letsencrypt.org/directory"
	HTTP01                = SolverType("http01")
	DNS01Cloudflare       = SolverType("dns01-cloudflare")
	Active                = Lifecycle("active")
	Deactivated           = Lifecycle("deactivated")
	Pending               = ObservationState("pending")
	Ready                 = ObservationState("ready")
	Degraded              = ObservationState("degraded")
)

var (
	ErrInvalid    = errors.New("invalid cert-manager issuer input")
	ErrConflict   = errors.New("cert-manager issuer conflict")
	ErrNotFound   = errors.New("cert-manager issuer not found")
	ErrInactive   = errors.New("cert-manager issuer inactive")
	ErrReferenced = errors.New("cert-manager issuer is referenced")
	uuidRE        = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	dnsLabelRE    = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$`)
	hostLabelRE   = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$`)
	idemRE        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$`)
	digestRE      = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	secretKeyRE   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,253}$`)
)

type SolverType string
type Lifecycle string
type ObservationState string
type Environment string

const (
	Production Environment = "production"
	Staging    Environment = "staging"
)

// Spec is deliberately closed. Arbitrary solver YAML and credential bytes are
// never accepted. Profiles contain exactly one solver for deterministic tenant
// selection.
type Spec struct {
	ACME       ACME                 `json:"acme"`
	HTTP01     *HTTP01Spec          `json:"http01,omitempty"`
	Cloudflare *CloudflareDNS01Spec `json:"cloudflare,omitempty"`
}
type ACME struct {
	Email                       string `json:"email"`
	Server                      string `json:"server"`
	AccountPrivateKeySecretName string `json:"accountPrivateKeySecretName"`
}
type HTTP01Spec struct{}
type CloudflareDNS01Spec struct {
	APITokenSecretName string   `json:"apiTokenSecretName"`
	APITokenSecretKey  string   `json:"apiTokenSecretKey"`
	DNSZones           []string `json:"dnsZones"`
}
type Profile struct {
	ID, Name, CreatedBy, DeactivatedBy string
	Lifecycle                          Lifecycle
	CurrentRevision                    int64
	CreatedAt                          time.Time
	DeactivatedAt                      *time.Time
}
type Revision struct {
	ProfileID, SpecDigest, CreatedBy string
	Revision                         int64
	Solver                           SolverType
	Spec                             Spec
	CreatedAt                        time.Time
}
type Entry struct {
	Profile  Profile
	Revision Revision
}
type Ref struct {
	ProfileID string
	Revision  int64
}
type Command struct {
	ActorID, IdempotencyKey, RequestID string
	Now                                time.Time
}
type MutationResult struct {
	Profile  Profile
	Revision Revision
	Replay   bool
}
type Observation struct {
	ProfileID, ObservedSpecDigest, Reason string
	Revision, ObservedGeneration          int64
	State                                 ObservationState
	ObservedAt                            *time.Time
	UpdatedAt                             time.Time
}
type Desired struct {
	ProfileID, Name, SpecDigest string
	Revision                    int64
	Solver                      SolverType
	Spec                        Spec
}

// TenantIdentity is intentionally non-sensitive. It excludes ACME email,
// Secret references, provider credentials and DNS zone configuration.
type TenantIdentity struct {
	ProfileID   string      `json:"profileId"`
	Name        string      `json:"name"`
	Revision    int64       `json:"revision"`
	Solver      SolverType  `json:"solver"`
	Environment Environment `json:"environment"`
}
type Selection struct{ Hostname, IssuerName string }

const minCatalogMaxAge = time.Minute
const maxCatalogMaxAge = 24 * time.Hour

func validFreshness(now time.Time, maxAge time.Duration) bool {
	return !now.IsZero() && now.Location() == time.UTC && maxAge >= minCatalogMaxAge && maxAge <= maxCatalogMaxAge
}
func cloneSpec(in Spec) Spec {
	out := in
	if in.HTTP01 != nil {
		out.HTTP01 = &HTTP01Spec{}
	}
	if in.Cloudflare != nil {
		c := *in.Cloudflare
		c.DNSZones = append([]string(nil), in.Cloudflare.DNSZones...)
		out.Cloudflare = &c
	}
	return out
}

func normalizeSpec(in Spec) (Spec, SolverType, string, error) {
	out := cloneSpec(in)
	out.ACME.Email = strings.TrimSpace(strings.ToLower(out.ACME.Email))
	if _, err := mail.ParseAddress(out.ACME.Email); err != nil || len(out.ACME.Email) > 254 || strings.ContainsAny(out.ACME.Email, "<>") {
		return Spec{}, "", "", ErrInvalid
	}
	if out.ACME.Server != LetsEncryptProduction && out.ACME.Server != LetsEncryptStaging || !dnsLabelRE.MatchString(out.ACME.AccountPrivateKeySecretName) {
		return Spec{}, "", "", ErrInvalid
	}
	if (out.HTTP01 == nil) == (out.Cloudflare == nil) {
		return Spec{}, "", "", ErrInvalid
	}
	solver := HTTP01
	if out.Cloudflare != nil {
		solver = DNS01Cloudflare
		if !dnsLabelRE.MatchString(out.Cloudflare.APITokenSecretName) || !secretKeyRE.MatchString(out.Cloudflare.APITokenSecretKey) || len(out.Cloudflare.DNSZones) < 1 || len(out.Cloudflare.DNSZones) > 64 {
			return Spec{}, "", "", ErrInvalid
		}
		zones := make([]string, len(out.Cloudflare.DNSZones))
		for i, zone := range out.Cloudflare.DNSZones {
			zone = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(zone)), ".")
			if !validHostname(zone, false) {
				return Spec{}, "", "", ErrInvalid
			}
			zones[i] = zone
		}
		sort.Strings(zones)
		for i := 1; i < len(zones); i++ {
			if zones[i] == zones[i-1] {
				return Spec{}, "", "", ErrInvalid
			}
		}
		out.Cloudflare.DNSZones = zones
	}
	raw, _ := json.Marshal(struct {
		Contract string `json:"contract"`
		Spec     Spec   `json:"spec"`
	}{Contract, out})
	sum := sha256.Sum256(raw)
	return out, solver, "sha256:" + hex.EncodeToString(sum[:]), nil
}

func validHostname(value string, allowWildcard bool) bool {
	value = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(value)), ".")
	if allowWildcard && strings.HasPrefix(value, "*.") {
		value = value[2:]
	} else if strings.Contains(value, "*") {
		return false
	}
	if len(value) < 3 || len(value) > 253 || !strings.Contains(value, ".") || net.ParseIP(value) != nil {
		return false
	}
	for _, suffix := range []string{".local", ".localhost", ".internal", ".invalid", ".test", ".example"} {
		if strings.HasSuffix(value, suffix) {
			return false
		}
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) < 1 || len(label) > 63 || !hostLabelRE.MatchString(label) {
			return false
		}
	}
	return true
}
func coversHostname(spec Spec, solver SolverType, hostname string) bool {
	host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(hostname)), ".")
	wild := strings.HasPrefix(host, "*.")
	if !validHostname(host, true) || solver == HTTP01 && wild {
		return false
	}
	if solver == HTTP01 {
		return true
	}
	host = strings.TrimPrefix(host, "*.")
	for _, zone := range spec.Cloudflare.DNSZones {
		if host == zone || strings.HasSuffix(host, "."+zone) {
			return true
		}
	}
	return false
}
func environmentForServer(server string) Environment {
	if server == LetsEncryptProduction {
		return Production
	}
	return Staging
}
func validateCommand(c Command) bool {
	return uuidRE.MatchString(c.ActorID) && idemRE.MatchString(c.IdempotencyKey) && idemRE.MatchString(c.RequestID) && !c.Now.IsZero()
}
func commandDigest(action, profileID, name string, revision int64, spec Spec) (string, error) {
	clean, _, sd, err := normalizeSpec(spec)
	if err != nil {
		return "", err
	}
	raw, _ := json.Marshal([]any{Contract, action, profileID, name, revision, sd, clean})
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
