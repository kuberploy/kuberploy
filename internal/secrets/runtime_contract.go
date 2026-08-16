package secrets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"time"
)

const (
	// RuntimeSecretWorkerContract is bumped whenever the durable claim/apply
	// state machine or the strict SealedSecret observer boundary changes.
	RuntimeSecretWorkerContract = "runtime-secrets-v1"
	// RuntimeSecretReferencePolicyContract binds Git projection readiness to
	// the exact metadata-only runtime-secret policy configuration. It excludes
	// key and certificate bytes, which are independently proven by the strict
	// runtime worker readiness identity.
	RuntimeSecretReferencePolicyContract = "runtime-secret-reference-policy-v1"

	RuntimeSecretHeartbeatInterval = 10 * time.Second
	RuntimeSecretHeartbeatMaxAge   = 35 * time.Second
	RuntimeSecretReadinessLease    = 45 * time.Second
	RuntimeSecretReadinessSkew     = 5 * time.Second
	RuntimeSecretWorkLease         = 45 * time.Second
	RuntimeSecretPollInterval      = 5 * time.Second
	RuntimeSecretMinimumBackoff    = 2 * time.Second
	RuntimeSecretMaximumBackoff    = 5 * time.Minute
	RuntimeSecretIdleDelay         = time.Second
	maximumRuntimeSecretNamespaces = 128
)

var (
	runtimeSecretWorkerIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$`)
	runtimeSecretContractRE = regexp.MustCompile(`^[a-z][a-z0-9.-]{7,63}$`)
)

// RuntimeConfig is the complete operator-owned worker contract. Namespaces
// must be sorted and unique, making the allowlist byte-for-byte stable in the
// digest used by both the API and worker readiness probe.
type RuntimeConfig struct {
	Enabled                     bool
	Namespaces                  []string
	FingerprintSecretRef        string
	FingerprintSecretKey        string
	FingerprintKeyID            string
	SealingCertificateSecretRef string
	SealingCertificateSecretKey string
	PollInterval                time.Duration
	WorkLease                   time.Duration
	HeartbeatInterval           time.Duration
	IdleDelay                   time.Duration
	MinimumBackoff              time.Duration
	MaximumBackoff              time.Duration
}

func DefaultRuntimeConfig() RuntimeConfig {
	return RuntimeConfig{
		PollInterval: RuntimeSecretPollInterval, WorkLease: RuntimeSecretWorkLease,
		HeartbeatInterval: RuntimeSecretHeartbeatInterval, IdleDelay: RuntimeSecretIdleDelay,
		MinimumBackoff: RuntimeSecretMinimumBackoff, MaximumBackoff: RuntimeSecretMaximumBackoff,
		FingerprintSecretKey: DefaultFingerprintSecretKey, FingerprintKeyID: DefaultFingerprintKeyID,
		SealingCertificateSecretKey: DefaultSealedSecretsCertificateKey,
	}
}

func NormalizeRuntimeNamespaces(input []string) ([]string, error) {
	if len(input) == 0 || len(input) > maximumRuntimeSecretNamespaces {
		return nil, ErrRuntimeUnavailable
	}
	result := append([]string(nil), input...)
	for _, namespace := range result {
		if !dnsLabelRE.MatchString(namespace) {
			return nil, ErrRuntimeUnavailable
		}
	}
	slices.Sort(result)
	result = slices.Compact(result)
	return result, nil
}

func (c RuntimeConfig) Validate() error {
	namespaces, err := NormalizeRuntimeNamespaces(c.Namespaces)
	if err != nil || !c.Enabled || !slices.Equal(namespaces, c.Namespaces) ||
		!kubeNameRE.MatchString(c.FingerprintSecretRef) || !keyIDRE.MatchString(c.FingerprintKeyID) ||
		!secretKeyRE.MatchString(c.FingerprintSecretKey) || !kubeNameRE.MatchString(c.SealingCertificateSecretRef) ||
		!secretKeyRE.MatchString(c.SealingCertificateSecretKey) ||
		c.PollInterval < time.Second || c.PollInterval > 10*time.Minute ||
		c.WorkLease < 20*time.Second || c.WorkLease > time.Hour ||
		c.HeartbeatInterval < time.Second || c.HeartbeatInterval >= c.WorkLease/2 || c.HeartbeatInterval >= RuntimeSecretReadinessLease/2 ||
		c.IdleDelay < 100*time.Millisecond || c.IdleDelay > time.Minute ||
		c.MinimumBackoff < time.Second || c.MaximumBackoff < c.MinimumBackoff || c.MaximumBackoff > time.Hour {
		return ErrRuntimeUnavailable
	}
	return nil
}

func (c RuntimeConfig) AllowsNamespace(namespace string) bool {
	if c.Validate() != nil || !dnsLabelRE.MatchString(namespace) {
		return false
	}
	_, found := slices.BinarySearch(c.Namespaces, namespace)
	return found
}

// RuntimePolicyDigest returns a canonical metadata-only digest shared by the
// API and Git projection worker. Enabled configurations must satisfy the full
// strict-Sealed runtime contract. The only valid disabled configuration is the
// exact zero value produced by RuntimeConfigFromEnvironment.
func RuntimePolicyDigest(config RuntimeConfig) (string, error) {
	if !config.Enabled {
		if len(config.Namespaces) != 0 || config.FingerprintSecretRef != "" || config.FingerprintSecretKey != "" ||
			config.FingerprintKeyID != "" || config.SealingCertificateSecretRef != "" || config.SealingCertificateSecretKey != "" ||
			config.PollInterval != 0 || config.WorkLease != 0 || config.HeartbeatInterval != 0 || config.IdleDelay != 0 ||
			config.MinimumBackoff != 0 || config.MaximumBackoff != 0 {
			return "", ErrRuntimeUnavailable
		}
		encoded, _ := json.Marshal(struct {
			Contract string `json:"contract"`
			Enabled  bool   `json:"enabled"`
		}{RuntimeSecretReferencePolicyContract, false})
		digest := sha256.Sum256(encoded)
		return "sha256:" + hex.EncodeToString(digest[:]), nil
	}
	if config.Validate() != nil {
		return "", ErrRuntimeUnavailable
	}
	canonical := struct {
		Contract                  string       `json:"contract"`
		Enabled                   bool         `json:"enabled"`
		Namespaces                []string     `json:"namespaces"`
		Provider                  ProviderKind `json:"provider"`
		FingerprintSecretRef      string       `json:"fingerprintSecretRef"`
		FingerprintSecretKey      string       `json:"fingerprintSecretKey"`
		FingerprintKeyID          string       `json:"fingerprintKeyId"`
		FingerprintProjectionPath string       `json:"fingerprintProjectionPath"`
		CertificateSecretRef      string       `json:"certificateSecretRef"`
		CertificateSecretKey      string       `json:"certificateSecretKey"`
		CertificateProjectionPath string       `json:"certificateProjectionPath"`
		PollIntervalNanos         int64        `json:"pollIntervalNanos"`
		WorkLeaseNanos            int64        `json:"workLeaseNanos"`
		HeartbeatIntervalNanos    int64        `json:"heartbeatIntervalNanos"`
		IdleDelayNanos            int64        `json:"idleDelayNanos"`
		MinimumBackoffNanos       int64        `json:"minimumBackoffNanos"`
		MaximumBackoffNanos       int64        `json:"maximumBackoffNanos"`
	}{
		Contract: RuntimeSecretReferencePolicyContract, Enabled: true,
		Namespaces: append([]string(nil), config.Namespaces...), Provider: ProviderSealedSecrets,
		FingerprintSecretRef: config.FingerprintSecretRef, FingerprintSecretKey: config.FingerprintSecretKey,
		FingerprintKeyID: config.FingerprintKeyID, FingerprintProjectionPath: DefaultFingerprintKeyPath,
		CertificateSecretRef: config.SealingCertificateSecretRef, CertificateSecretKey: config.SealingCertificateSecretKey,
		CertificateProjectionPath: DefaultSealedSecretsCertificatePath, PollIntervalNanos: int64(config.PollInterval),
		WorkLeaseNanos: int64(config.WorkLease), HeartbeatIntervalNanos: int64(config.HeartbeatInterval),
		IdleDelayNanos: int64(config.IdleDelay), MinimumBackoffNanos: int64(config.MinimumBackoff),
		MaximumBackoffNanos: int64(config.MaximumBackoff),
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", ErrRuntimeUnavailable
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// RuntimeIdentity contains public identities and digests only. The HMAC key
// bytes and certificate bytes are consumed transiently and never retained.
type RuntimeIdentity struct {
	ContractVersion       string
	ConfigDigest          string
	FingerprintKeyID      string
	SealingKeyFingerprint string
}

func RuntimeIdentityForConfig(config RuntimeConfig, sealingKeyFingerprint string) (RuntimeIdentity, error) {
	if config.Validate() != nil || !digestRE.MatchString(sealingKeyFingerprint) {
		return RuntimeIdentity{}, ErrRuntimeUnavailable
	}
	canonical := struct {
		ContractVersion           string   `json:"contractVersion"`
		Namespaces                []string `json:"namespaces"`
		FingerprintSecretRef      string   `json:"fingerprintSecretRef"`
		FingerprintSecretKey      string   `json:"fingerprintSecretKey"`
		FingerprintKeyID          string   `json:"fingerprintKeyId"`
		FingerprintProjectionPath string   `json:"fingerprintProjectionPath"`
		CertificateSecretRef      string   `json:"certificateSecretRef"`
		CertificateSecretKey      string   `json:"certificateSecretKey"`
		CertificateProjectionPath string   `json:"certificateProjectionPath"`
		SealingKeyFingerprint     string   `json:"sealingKeyFingerprint"`
		PollIntervalNanos         int64    `json:"pollIntervalNanos"`
		WorkLeaseNanos            int64    `json:"workLeaseNanos"`
		HeartbeatIntervalNanos    int64    `json:"heartbeatIntervalNanos"`
		IdleDelayNanos            int64    `json:"idleDelayNanos"`
		MinimumBackoffNanos       int64    `json:"minimumBackoffNanos"`
		MaximumBackoffNanos       int64    `json:"maximumBackoffNanos"`
	}{
		ContractVersion: RuntimeSecretWorkerContract, Namespaces: append([]string(nil), config.Namespaces...),
		FingerprintSecretRef: config.FingerprintSecretRef, FingerprintSecretKey: config.FingerprintSecretKey,
		FingerprintKeyID:          config.FingerprintKeyID,
		FingerprintProjectionPath: DefaultFingerprintKeyPath,
		CertificateSecretRef:      config.SealingCertificateSecretRef,
		CertificateSecretKey:      config.SealingCertificateSecretKey,
		CertificateProjectionPath: DefaultSealedSecretsCertificatePath,
		SealingKeyFingerprint:     sealingKeyFingerprint, PollIntervalNanos: int64(config.PollInterval),
		WorkLeaseNanos: int64(config.WorkLease), HeartbeatIntervalNanos: int64(config.HeartbeatInterval),
		IdleDelayNanos: int64(config.IdleDelay), MinimumBackoffNanos: int64(config.MinimumBackoff),
		MaximumBackoffNanos: int64(config.MaximumBackoff),
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return RuntimeIdentity{}, ErrRuntimeUnavailable
	}
	digest := sha256.Sum256(encoded)
	return RuntimeIdentity{ContractVersion: RuntimeSecretWorkerContract,
		ConfigDigest: "sha256:" + hex.EncodeToString(digest[:]), FingerprintKeyID: config.FingerprintKeyID,
		SealingKeyFingerprint: sealingKeyFingerprint}, nil
}

func (i RuntimeIdentity) Validate() error {
	if !runtimeSecretContractRE.MatchString(i.ContractVersion) || !digestRE.MatchString(i.ConfigDigest) ||
		!keyIDRE.MatchString(i.FingerprintKeyID) || !digestRE.MatchString(i.SealingKeyFingerprint) {
		return ErrRuntimeUnavailable
	}
	return nil
}

// DefaultRuntimeIdentity is the API identity resolver. It validates both fixed
// projections because the write-only API uses the HMAC key to fingerprint
// transient secret material. Only public identities survive.
func DefaultRuntimeIdentity(ctx context.Context, config RuntimeConfig, now time.Time) (RuntimeIdentity, error) {
	if config.Validate() != nil || now.IsZero() {
		return RuntimeIdentity{}, ErrRuntimeUnavailable
	}
	keys := NewDefaultProjectedFingerprintKeyProvider()
	key, err := keys.ActiveKey(ctx)
	if err != nil {
		return RuntimeIdentity{}, ErrRuntimeUnavailable
	}
	defer key.destroy()
	if key.ID != config.FingerprintKeyID {
		return RuntimeIdentity{}, ErrRuntimeUnavailable
	}
	return runtimeIdentityFromSealingCertificate(ctx, config, now, projectedSealingCertificate{path: DefaultSealedSecretsCertificatePath})
}

// WorkerRuntimeIdentity proves the public sealing certificate and the exact
// metadata-only runtime contract without reading the API's private HMAC key.
// The worker observes SealedSecrets but never ingests plaintext or computes
// content fingerprints, so mounting that key would violate least privilege.
func WorkerRuntimeIdentity(ctx context.Context, config RuntimeConfig, now time.Time) (RuntimeIdentity, error) {
	return runtimeIdentityFromSealingCertificate(ctx, config, now, projectedSealingCertificate{path: DefaultSealedSecretsCertificatePath})
}

func runtimeIdentityFromSealingCertificate(ctx context.Context, config RuntimeConfig, now time.Time, source sealingPublicKeySource) (RuntimeIdentity, error) {
	if config.Validate() != nil || now.IsZero() || source == nil {
		return RuntimeIdentity{}, ErrRuntimeUnavailable
	}
	certificate, err := source.ActivePublicKey(ctx, now.UTC())
	if err != nil {
		return RuntimeIdentity{}, ErrRuntimeUnavailable
	}
	return RuntimeIdentityForConfig(config, certificate.fingerprint)
}

type RuntimeLease struct {
	VersionID string
	BindingID string
	Owner     string
	Epoch     int64
	Until     time.Time
	Identity  RuntimeIdentity
}

func (l RuntimeLease) Validate() error {
	if !uuidRE.MatchString(l.VersionID) || !uuidRE.MatchString(l.BindingID) ||
		!runtimeSecretWorkerIDRE.MatchString(l.Owner) || l.Epoch < 1 || l.Until.IsZero() || l.Identity.Validate() != nil {
		return ErrRuntimeUnavailable
	}
	return nil
}

type RuntimeWork struct {
	Binding             Binding
	Version             Version
	Lease               RuntimeLease
	ConsecutiveFailures int
}

func (w RuntimeWork) Validate() error {
	if w.Binding.Validate() != nil || w.Version.Validate() != nil || w.Lease.Validate() != nil ||
		w.Binding.ID != w.Version.BindingID || w.Binding.ID != w.Lease.BindingID || w.Version.ID != w.Lease.VersionID ||
		w.Binding.Provider != ProviderSealedSecrets || w.Version.Provider != ProviderSealedSecrets ||
		w.Version.State != VersionAwaitingReadiness || w.Version.Artifact == nil ||
		w.ConsecutiveFailures < 0 || w.ConsecutiveFailures > 30 {
		return ErrRuntimeUnavailable
	}
	return nil
}

type RuntimePendingOutcome struct {
	FailureCode string
	NextAt      time.Time
}

func (o RuntimePendingOutcome) validate(now time.Time) error {
	if now.IsZero() || o.NextAt.IsZero() || !o.NextAt.After(now) || o.NextAt.After(now.Add(24*time.Hour)) ||
		o.FailureCode != "" && !safeCodeRE.MatchString(o.FailureCode) {
		return ErrInvalid
	}
	return nil
}

type RuntimeStore interface {
	ClaimRuntimeSecret(context.Context, RuntimeIdentity, string, []string, time.Time, time.Duration) (RuntimeWork, error)
	HeartbeatRuntimeSecret(context.Context, RuntimeLease, time.Time, time.Duration) (RuntimeLease, error)
	ApplyRuntimeSecretPending(context.Context, RuntimeLease, RuntimePendingOutcome, time.Time) error
	ApplyRuntimeSecretReady(context.Context, RuntimeLease, Event, time.Time) (Binding, Version, error)
	ApplyRuntimeSecretFailed(context.Context, RuntimeLease, string, Event, time.Time) (Version, error)

	AcquireRuntimeSecretReadiness(context.Context, RuntimeWorkerObservation, time.Duration) (RuntimeReadinessLease, error)
	HeartbeatRuntimeSecretReadiness(context.Context, RuntimeReadinessLease, time.Time, time.Duration) (RuntimeReadinessLease, error)
	RuntimeSecretReady(context.Context, RuntimeIdentity, time.Time, time.Duration) error
}

type RuntimeWorkerObservation struct {
	WorkerID   string
	Identity   RuntimeIdentity
	StartedAt  time.Time
	ObservedAt time.Time
}

func (o RuntimeWorkerObservation) Validate() error {
	if !runtimeSecretWorkerIDRE.MatchString(o.WorkerID) || o.Identity.Validate() != nil ||
		o.StartedAt.IsZero() || o.ObservedAt.IsZero() || o.ObservedAt.Before(o.StartedAt) {
		return ErrRuntimeUnavailable
	}
	return nil
}

type RuntimeReadinessLease struct {
	RuntimeWorkerObservation
	Epoch int64
	Until time.Time
}

func (l RuntimeReadinessLease) Validate() error {
	if l.RuntimeWorkerObservation.Validate() != nil || l.Epoch < 1 || l.Until.IsZero() || !l.Until.After(l.ObservedAt) {
		return ErrRuntimeUnavailable
	}
	return nil
}

type RuntimeReadinessProbe struct {
	Store           RuntimeStore
	Config          RuntimeConfig
	MaxAge          time.Duration
	Now             func() time.Time
	ResolveIdentity func(context.Context, RuntimeConfig, time.Time) (RuntimeIdentity, error)
}

func (p *RuntimeReadinessProbe) Probe(ctx context.Context) error {
	if p == nil || p.Store == nil || p.Config.Validate() != nil {
		return ErrRuntimeUnavailable
	}
	now := time.Now().UTC()
	if p.Now != nil {
		now = p.Now().UTC()
	}
	resolve := p.ResolveIdentity
	if resolve == nil {
		resolve = DefaultRuntimeIdentity
	}
	identity, err := resolve(ctx, p.Config, now)
	if err != nil {
		return ErrRuntimeUnavailable
	}
	maximumAge := p.MaxAge
	if maximumAge == 0 {
		maximumAge = RuntimeSecretHeartbeatMaxAge
	}
	if maximumAge < 2*p.Config.HeartbeatInterval || maximumAge > 5*time.Minute {
		return ErrRuntimeUnavailable
	}
	if err = p.Store.RuntimeSecretReady(ctx, identity, now, maximumAge); err != nil {
		return ErrRuntimeUnavailable
	}
	return nil
}

func exactRuntimeNamespaces(input []string) bool {
	normalized, err := NormalizeRuntimeNamespaces(input)
	return err == nil && slices.Equal(normalized, input)
}

func runtimeIdentityEqual(left, right RuntimeIdentity) bool {
	return left == right
}

func runtimeLeaseError(err error) error {
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) || errors.Is(err, ErrRuntimeLeaseLost) {
		return ErrRuntimeLeaseLost
	}
	return err
}
