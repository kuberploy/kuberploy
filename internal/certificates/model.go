// Package certificates owns custom ingress-certificate validation and safe
// attestation. Private keys and PEM bytes are write-only and delegated to the
// runtime-secret lifecycle; only public certificate metadata and a keyed
// secret-content identity may be persisted.
package certificates

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kuberploy/kuberploy/internal/secrets"
)

const (
	MaxCertificatePEMBytes = 64 << 10
	MaxPrivateKeyPEMBytes  = 32 << 10
	MaxCertificateChain    = 10
	MaxSubjectAltNames     = 128
	redactedMaterial       = "[REDACTED TLS certificate material]"
)

var (
	ErrInvalid      = errors.New("invalid TLS certificate input")
	ErrNotFound     = errors.New("TLS certificate not found")
	ErrNotReady     = errors.New("TLS certificate is not ready")
	ErrHostMismatch = errors.New("TLS certificate does not cover the route host")
	ErrConflict     = errors.New("TLS certificate conflict")
	ErrUnavailable  = errors.New("TLS certificate service unavailable")
	ErrMaterialGone = errors.New("TLS certificate material was already destroyed")
	ErrNoSerialize  = errors.New("TLS certificate material cannot be serialized")

	uuidRE   = regexp.MustCompile("^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$")
	digestRE = regexp.MustCompile("^sha256:[0-9a-f]{64}$")
	labelRE  = regexp.MustCompile("^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$")
)

// Reference is the complete certificate identity allowed in AppConfig. It
// deliberately contains no Kubernetes Secret name, provider object name,
// ciphertext identity, or key material. The target Secret is always derived
// from the immutable binding and version after an exact scope/readiness check.
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

// ResolvedReference is the metadata-only result consumed by the projection
// and runtime render boundaries. Provider/ciphertext digests stay internal to
// the resolver and no caller may choose TargetSecretName.
type ResolvedReference struct {
	BindingID            string    `json:"bindingId"`
	SecretVersionID      string    `json:"secretVersionId"`
	Name                 string    `json:"name"`
	Version              int64     `json:"version"`
	Namespace            string    `json:"namespace"`
	TargetSecretName     string    `json:"targetSecretName"`
	LeafFingerprint      string    `json:"leafFingerprint"`
	PublicKeyFingerprint string    `json:"publicKeyFingerprint"`
	NotBefore            time.Time `json:"notBefore"`
	NotAfter             time.Time `json:"notAfter"`
}

func (r ResolvedReference) Validate() error {
	if !uuidRE.MatchString(r.BindingID) || !uuidRE.MatchString(r.SecretVersionID) ||
		!labelRE.MatchString(r.Name) || r.Version <= 0 || !labelRE.MatchString(r.Namespace) ||
		!labelRE.MatchString(r.TargetSecretName) || !digestRE.MatchString(r.LeafFingerprint) ||
		!digestRE.MatchString(r.PublicKeyFingerprint) || r.NotBefore.IsZero() || !r.NotAfter.After(r.NotBefore) {
		return ErrInvalid
	}
	return nil
}

// Material owns bounded request-local PEM bytes. The service destroys them on
// every return path. It cannot be serialized or formatted into diagnostics.
type Material struct {
	certificatePEM []byte
	privateKeyPEM  []byte
	destroyed      bool
}

func NewMaterial(certificatePEM, privateKeyPEM []byte) (*Material, error) {
	if len(certificatePEM) == 0 || len(certificatePEM) > MaxCertificatePEMBytes ||
		len(privateKeyPEM) == 0 || len(privateKeyPEM) > MaxPrivateKeyPEMBytes {
		return nil, ErrInvalid
	}
	return &Material{certificatePEM: bytes.Clone(certificatePEM), privateKeyPEM: bytes.Clone(privateKeyPEM)}, nil
}

func (m *Material) Destroy() {
	if m == nil || m.destroyed {
		return
	}
	clear(m.certificatePEM)
	clear(m.privateKeyPEM)
	m.certificatePEM, m.privateKeyPEM, m.destroyed = nil, nil, true
}

func (m *Material) MarshalJSON() ([]byte, error) { return nil, ErrNoSerialize }
func (m *Material) String() string               { return redactedMaterial }
func (m *Material) GoString() string             { return redactedMaterial }
func (m *Material) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, redactedMaterial)
}

// Version is an immutable public attestation for one exact runtime-secret
// version. SecretContentFingerprint is keyed by the platform and is never
// returned to API callers.
type Version struct {
	BindingID                string    `json:"bindingId"`
	SecretVersionID          string    `json:"secretVersionId"`
	Number                   int64     `json:"number"`
	LeafFingerprint          string    `json:"leafFingerprint"`
	PublicKeyFingerprint     string    `json:"publicKeyFingerprint"`
	DNSNames                 []string  `json:"dnsNames"`
	IPAddresses              []string  `json:"ipAddresses"`
	NotBefore                time.Time `json:"notBefore"`
	NotAfter                 time.Time `json:"notAfter"`
	CreatedBy                string    `json:"createdBy"`
	CreatedAt                time.Time `json:"createdAt"`
	SecretContentFingerprint [32]byte  `json:"-"`
}

func (v Version) Validate() error {
	if !uuidRE.MatchString(v.BindingID) || !uuidRE.MatchString(v.SecretVersionID) || v.Number <= 0 ||
		!digestRE.MatchString(v.LeafFingerprint) || !digestRE.MatchString(v.PublicKeyFingerprint) ||
		!uuidRE.MatchString(v.CreatedBy) || v.CreatedAt.IsZero() || v.NotBefore.IsZero() || !v.NotAfter.After(v.NotBefore) ||
		v.SecretContentFingerprint == [32]byte{} || !canonicalDNSNames(v.DNSNames) || !canonicalIPAddresses(v.IPAddresses) {
		return ErrInvalid
	}
	return nil
}

func (v Version) ValidateFor(binding secrets.Binding, version secrets.Version) error {
	if v.Validate() != nil || binding.Validate() != nil || version.Validate() != nil ||
		binding.ID != v.BindingID || binding.Purpose != secrets.PurposeTLSCertificate || binding.Provider != secrets.ProviderSealedSecrets ||
		version.ID != v.SecretVersionID || version.BindingID != binding.ID || version.Number != v.Number ||
		version.Provider != secrets.ProviderSealedSecrets || version.TargetSecretType != secrets.TargetSecretTLS || version.Artifact == nil ||
		version.Artifact.TargetSecretType != secrets.TargetSecretTLS || version.Artifact.ValidateFor(binding, version.Number) != nil ||
		subtle.ConstantTimeCompare(v.SecretContentFingerprint[:], version.ContentFingerprint[:]) != 1 {
		return ErrConflict
	}
	return nil
}

func (v Version) CoversHost(host string) bool {
	host, ok := canonicalRouteHost(host)
	if !ok {
		return false
	}
	for _, name := range v.DNSNames {
		if name == host {
			return true
		}
		if strings.HasPrefix(name, "*.") {
			suffix := strings.TrimPrefix(name, "*.")
			if strings.HasSuffix(host, "."+suffix) && strings.Count(host, ".") == strings.Count(suffix, ".")+1 {
				return true
			}
		}
	}
	return false
}

type parsedCertificate struct {
	LeafFingerprint      string
	PublicKeyFingerprint string
	DNSNames             []string
	IPAddresses          []string
	NotBefore            time.Time
	NotAfter             time.Time
}

func parseAndValidate(material *Material, now time.Time) (parsedCertificate, error) {
	if material == nil || material.destroyed || now.IsZero() {
		return parsedCertificate{}, ErrInvalid
	}
	chain, err := parseCertificateChain(material.certificatePEM)
	if err != nil {
		return parsedCertificate{}, err
	}
	leaf := chain[0]
	if leaf.IsCA || now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) || len(leaf.DNSNames) == 0 {
		return parsedCertificate{}, ErrInvalid
	}
	if len(leaf.ExtKeyUsage) > 0 && !containsServerAuth(leaf.ExtKeyUsage) {
		return parsedCertificate{}, ErrInvalid
	}
	if leaf.KeyUsage != 0 && leaf.KeyUsage&(x509.KeyUsageDigitalSignature|x509.KeyUsageKeyEncipherment|x509.KeyUsageKeyAgreement) == 0 {
		return parsedCertificate{}, ErrInvalid
	}
	for index := 0; index+1 < len(chain); index++ {
		if err = chain[index].CheckSignatureFrom(chain[index+1]); err != nil {
			return parsedCertificate{}, ErrInvalid
		}
	}
	keyPublic, err := parsePrivateKeyPublic(material.privateKeyPEM)
	if err != nil {
		return parsedCertificate{}, err
	}
	leafPublic, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if err != nil {
		clear(keyPublic)
		return parsedCertificate{}, ErrInvalid
	}
	leafPublicHash, keyPublicHash := sha256.Sum256(leafPublic), sha256.Sum256(keyPublic)
	clear(leafPublic)
	clear(keyPublic)
	if subtle.ConstantTimeCompare(leafPublicHash[:], keyPublicHash[:]) != 1 {
		return parsedCertificate{}, ErrInvalid
	}
	dnsNames, err := normalizeDNSNames(leaf.DNSNames)
	if err != nil {
		return parsedCertificate{}, err
	}
	ipAddresses := make([]string, 0, len(leaf.IPAddresses))
	for _, address := range leaf.IPAddresses {
		parsed, parseErr := netip.ParseAddr(address.String())
		if parseErr != nil {
			return parsedCertificate{}, ErrInvalid
		}
		ipAddresses = append(ipAddresses, parsed.Unmap().String())
	}
	slices.Sort(ipAddresses)
	ipAddresses = slices.Compact(ipAddresses)
	if len(dnsNames)+len(ipAddresses) > MaxSubjectAltNames {
		return parsedCertificate{}, ErrInvalid
	}
	leafHash := sha256.Sum256(leaf.Raw)
	return parsedCertificate{
		LeafFingerprint:      "sha256:" + hex.EncodeToString(leafHash[:]),
		PublicKeyFingerprint: "sha256:" + hex.EncodeToString(leafPublicHash[:]),
		DNSNames:             dnsNames,
		IPAddresses:          ipAddresses,
		NotBefore:            leaf.NotBefore.UTC(),
		NotAfter:             leaf.NotAfter.UTC(),
	}, nil
}

func parseCertificateChain(input []byte) ([]*x509.Certificate, error) {
	rest := input
	chain := make([]*x509.Certificate, 0, 3)
	seen := map[[32]byte]struct{}{}
	for len(bytes.TrimSpace(rest)) > 0 {
		block, next := pem.Decode(rest)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 || len(block.Bytes) == 0 || len(chain) >= MaxCertificateChain {
			return nil, ErrInvalid
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, ErrInvalid
		}
		digest := sha256.Sum256(certificate.Raw)
		if _, duplicate := seen[digest]; duplicate {
			return nil, ErrInvalid
		}
		seen[digest] = struct{}{}
		chain = append(chain, certificate)
		rest = next
	}
	if len(chain) == 0 {
		return nil, ErrInvalid
	}
	return chain, nil
}

func parsePrivateKeyPublic(input []byte) ([]byte, error) {
	block, rest := pem.Decode(input)
	if block == nil || len(block.Bytes) == 0 || len(block.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0 || strings.Contains(block.Type, "ENCRYPTED") {
		return nil, ErrInvalid
	}
	defer clear(block.Bytes)
	var value any
	var err error
	switch block.Type {
	case "PRIVATE KEY":
		value, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		value, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		value, err = x509.ParseECPrivateKey(block.Bytes)
	default:
		return nil, ErrInvalid
	}
	if err != nil {
		return nil, ErrInvalid
	}
	var public any
	switch key := value.(type) {
	case *rsa.PrivateKey:
		if key.Validate() != nil || key.N.BitLen() < 2048 {
			return nil, ErrInvalid
		}
		public = &key.PublicKey
		defer destroyRSAPrivateKey(key)
	case *ecdsa.PrivateKey:
		if key.Curve == nil || key.D == nil {
			return nil, ErrInvalid
		}
		public = &key.PublicKey
		defer clear(key.D.Bits())
	case ed25519.PrivateKey:
		if len(key) != ed25519.PrivateKeySize {
			return nil, ErrInvalid
		}
		public = key.Public()
		defer clear(key)
	default:
		return nil, ErrInvalid
	}
	encoded, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		return nil, ErrInvalid
	}
	return encoded, nil
}

func destroyRSAPrivateKey(key *rsa.PrivateKey) {
	if key == nil {
		return
	}
	clearBigInt(key.D)
	for _, prime := range key.Primes {
		clearBigInt(prime)
	}
	clearBigInt(key.Precomputed.Dp)
	clearBigInt(key.Precomputed.Dq)
	clearBigInt(key.Precomputed.Qinv)
	for index := range key.Precomputed.CRTValues {
		clearBigInt(key.Precomputed.CRTValues[index].Exp)
		clearBigInt(key.Precomputed.CRTValues[index].Coeff)
		clearBigInt(key.Precomputed.CRTValues[index].R)
	}
}

func clearBigInt(value *big.Int) {
	if value != nil {
		clear(value.Bits())
	}
}

func containsServerAuth(usages []x509.ExtKeyUsage) bool {
	return slices.Contains(usages, x509.ExtKeyUsageServerAuth) || slices.Contains(usages, x509.ExtKeyUsageAny)
}

func normalizeDNSNames(input []string) ([]string, error) {
	if len(input) == 0 || len(input) > MaxSubjectAltNames {
		return nil, ErrInvalid
	}
	result := make([]string, 0, len(input))
	for _, value := range input {
		value = strings.ToLower(value)
		if !validCertificateDNSName(value) {
			return nil, ErrInvalid
		}
		result = append(result, value)
	}
	slices.Sort(result)
	result = slices.Compact(result)
	if len(result) != len(input) {
		return nil, ErrInvalid
	}
	return result, nil
}

func validCertificateDNSName(value string) bool {
	if len(value) == 0 || len(value) > 253 || !utf8.ValidString(value) || strings.TrimSpace(value) != value || strings.HasSuffix(value, ".") {
		return false
	}
	if strings.HasPrefix(value, "*.") {
		value = strings.TrimPrefix(value, "*.")
		if !strings.Contains(value, ".") {
			return false
		}
	} else if strings.Contains(value, "*") {
		return false
	}
	for _, r := range value {
		if r > unicode.MaxASCII || unicode.IsControl(r) {
			return false
		}
	}
	for _, part := range strings.Split(value, ".") {
		if !labelRE.MatchString(part) {
			return false
		}
	}
	return true
}

func canonicalDNSNames(values []string) bool {
	normalized, err := normalizeDNSNames(values)
	return err == nil && slices.Equal(normalized, values)
}

func canonicalIPAddresses(values []string) bool {
	if len(values) > MaxSubjectAltNames {
		return false
	}
	copyValues := slices.Clone(values)
	for _, value := range copyValues {
		address, err := netip.ParseAddr(value)
		if err != nil || address.Unmap().String() != value {
			return false
		}
	}
	slices.Sort(copyValues)
	copyValues = slices.Compact(copyValues)
	return slices.Equal(copyValues, values)
}

func canonicalRouteHost(value string) (string, bool) {
	value = strings.ToLower(value)
	if strings.Contains(value, "*") || !validCertificateDNSName(value) {
		return "", false
	}
	return value, true
}

func cloneVersion(value Version) Version {
	value.DNSNames = slices.Clone(value.DNSNames)
	value.IPAddresses = slices.Clone(value.IPAddresses)
	return value
}

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
