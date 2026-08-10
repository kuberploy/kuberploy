package certificates

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"time"

	"github.com/kuberploy/kuberploy/internal/secrets"
)

const (
	CertificateObservationContract = "certificate-observation-v1"
	certificateTargetContract      = "certificate-observation-target-v1"

	CertificateObservationPollInterval      = 30 * time.Second
	CertificateObservationWorkLease         = 45 * time.Second
	CertificateObservationHeartbeat         = 10 * time.Second
	CertificateObservationIdleDelay         = time.Second
	CertificateObservationMinimumBackoff    = 2 * time.Second
	CertificateObservationMaximumBackoff    = 5 * time.Minute
	CertificateObservationMaximumAge        = 90 * time.Second
	CertificateObservationReadinessLease    = 45 * time.Second
	CertificateObservationHeartbeatMaxAge   = 35 * time.Second
	CertificateObservationReadinessSkew     = 5 * time.Second
	maximumCertificateObservationNamespaces = 128
)

var (
	ErrObservationLeaseLost   = errors.New("certificate observation lease was lost")
	ErrObservationUnavailable = errors.New("certificate observation runtime is unavailable")

	observationWorkerIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$`)
	observationContractRE = regexp.MustCompile(`^[a-z][a-z0-9.-]{7,63}$`)
	observationCodeRE     = regexp.MustCompile(`^[a-z][a-z0-9.-]{0,62}$`)
	observationKubeNameRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`)
)

type ObservationFailureCode string

const (
	ObservationCertificateExpired     ObservationFailureCode = "certificate-expired"
	ObservationCertificateNotValid    ObservationFailureCode = "certificate-not-valid"
	ObservationSealedSecretNotReady   ObservationFailureCode = "sealed-secret-not-ready"
	ObservationSealedSecretSyncFailed ObservationFailureCode = "sealed-secret-sync-failed"
	ObservationProviderMismatch       ObservationFailureCode = "provider-observation-mismatch"
	ObservationProviderUnavailable    ObservationFailureCode = "provider-observe-failed"
)

func (c ObservationFailureCode) valid() bool {
	switch c {
	case ObservationCertificateExpired, ObservationCertificateNotValid, ObservationSealedSecretNotReady,
		ObservationSealedSecretSyncFailed, ObservationProviderMismatch, ObservationProviderUnavailable:
		return true
	default:
		return false
	}
}

// ObservationConfig is the complete metadata-only continuous-observation
// contract. Namespaces are an exact sorted allowlist and all timings enter the
// worker identity, so an API cannot mistake a differently configured worker
// for the runtime that produced a certificate-readiness result.
type ObservationConfig struct {
	Enabled               bool
	Namespaces            []string
	PollInterval          time.Duration
	WorkLease             time.Duration
	HeartbeatInterval     time.Duration
	IdleDelay             time.Duration
	MinimumBackoff        time.Duration
	MaximumBackoff        time.Duration
	MaximumObservationAge time.Duration
}

func DefaultObservationConfig() ObservationConfig {
	return ObservationConfig{
		PollInterval: CertificateObservationPollInterval, WorkLease: CertificateObservationWorkLease,
		HeartbeatInterval: CertificateObservationHeartbeat, IdleDelay: CertificateObservationIdleDelay,
		MinimumBackoff: CertificateObservationMinimumBackoff, MaximumBackoff: CertificateObservationMaximumBackoff,
		MaximumObservationAge: CertificateObservationMaximumAge,
	}
}

func NormalizeObservationNamespaces(input []string) ([]string, error) {
	if len(input) == 0 || len(input) > maximumCertificateObservationNamespaces {
		return nil, ErrObservationUnavailable
	}
	result := append([]string(nil), input...)
	for _, namespace := range result {
		if !observationKubeNameRE.MatchString(namespace) {
			return nil, ErrObservationUnavailable
		}
	}
	slices.Sort(result)
	result = slices.Compact(result)
	if len(result) != len(input) {
		return nil, ErrObservationUnavailable
	}
	return result, nil
}

func (c ObservationConfig) Validate() error {
	namespaces, err := NormalizeObservationNamespaces(c.Namespaces)
	if err != nil || !c.Enabled || !slices.Equal(namespaces, c.Namespaces) ||
		c.PollInterval < 5*time.Second || c.PollInterval > 10*time.Minute ||
		c.WorkLease < 20*time.Second || c.WorkLease > time.Hour ||
		c.HeartbeatInterval < time.Second || c.HeartbeatInterval >= c.WorkLease/2 || c.HeartbeatInterval >= CertificateObservationReadinessLease/2 ||
		c.IdleDelay < 100*time.Millisecond || c.IdleDelay > time.Minute ||
		c.MinimumBackoff < time.Second || c.MaximumBackoff < c.MinimumBackoff || c.MaximumBackoff > time.Hour ||
		c.MaximumObservationAge < 2*c.PollInterval || c.MaximumObservationAge > 30*time.Minute {
		return ErrObservationUnavailable
	}
	return nil
}

func (c ObservationConfig) AllowsNamespace(namespace string) bool {
	if c.Validate() != nil || !observationKubeNameRE.MatchString(namespace) {
		return false
	}
	_, found := slices.BinarySearch(c.Namespaces, namespace)
	return found
}

type ObservationIdentity struct {
	ContractVersion string
	ConfigDigest    string
}

func ObservationIdentityForConfig(config ObservationConfig) (ObservationIdentity, error) {
	if config.Validate() != nil {
		return ObservationIdentity{}, ErrObservationUnavailable
	}
	canonical := struct {
		ContractVersion      string                   `json:"contractVersion"`
		ProviderContract     string                   `json:"providerContract"`
		Provider             secrets.ProviderKind     `json:"provider"`
		Purpose              secrets.BindingPurpose   `json:"purpose"`
		TargetSecretType     secrets.TargetSecretType `json:"targetSecretType"`
		Namespaces           []string                 `json:"namespaces"`
		PollIntervalNanos    int64                    `json:"pollIntervalNanos"`
		WorkLeaseNanos       int64                    `json:"workLeaseNanos"`
		HeartbeatNanos       int64                    `json:"heartbeatNanos"`
		IdleDelayNanos       int64                    `json:"idleDelayNanos"`
		MinimumBackoffNanos  int64                    `json:"minimumBackoffNanos"`
		MaximumBackoffNanos  int64                    `json:"maximumBackoffNanos"`
		MaximumAgeNanos      int64                    `json:"maximumAgeNanos"`
		ReadinessLeaseNanos  int64                    `json:"readinessLeaseNanos"`
		ReadinessMaxAgeNanos int64                    `json:"readinessMaxAgeNanos"`
		ReadinessSkewNanos   int64                    `json:"readinessSkewNanos"`
	}{
		ContractVersion: CertificateObservationContract, ProviderContract: secrets.RuntimeSecretWorkerContract,
		Provider: secrets.ProviderSealedSecrets, Purpose: secrets.PurposeTLSCertificate, TargetSecretType: secrets.TargetSecretTLS,
		Namespaces: append([]string(nil), config.Namespaces...), PollIntervalNanos: int64(config.PollInterval),
		WorkLeaseNanos: int64(config.WorkLease), HeartbeatNanos: int64(config.HeartbeatInterval), IdleDelayNanos: int64(config.IdleDelay),
		MinimumBackoffNanos: int64(config.MinimumBackoff), MaximumBackoffNanos: int64(config.MaximumBackoff),
		MaximumAgeNanos: int64(config.MaximumObservationAge), ReadinessLeaseNanos: int64(CertificateObservationReadinessLease),
		ReadinessMaxAgeNanos: int64(CertificateObservationHeartbeatMaxAge), ReadinessSkewNanos: int64(CertificateObservationReadinessSkew),
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return ObservationIdentity{}, ErrObservationUnavailable
	}
	digest := sha256.Sum256(encoded)
	return ObservationIdentity{ContractVersion: CertificateObservationContract, ConfigDigest: "sha256:" + hex.EncodeToString(digest[:])}, nil
}

func (i ObservationIdentity) Validate() error {
	if i.ContractVersion != CertificateObservationContract || !observationContractRE.MatchString(i.ContractVersion) || !digestRE.MatchString(i.ConfigDigest) {
		return ErrObservationUnavailable
	}
	return nil
}

// CertificateObservationTargetDigest binds a readiness result to the exact
// immutable runtime-secret artifact and append-only certificate attestation.
// It contains no plaintext, ciphertext, Secret data, or provider credential.
func CertificateObservationTargetDigest(binding secrets.Binding, secretVersion secrets.Version, attestation Version) (string, error) {
	if validateActiveCertificateTarget(binding, secretVersion, attestation) != nil {
		return "", ErrInvalid
	}
	artifact := *secretVersion.Artifact
	canonical := struct {
		Contract string `json:"contract"`
		Binding  struct {
			ID, OrganizationID, ProjectID, EnvironmentID, ApplicationID, Namespace, Name, Provider, Purpose string
			ActiveVersion                                                                                   int64
			CreatedAt                                                                                       time.Time
		} `json:"binding"`
		SecretVersion struct {
			ID, BindingID, Provider, TargetSecretType, FingerprintKeyID, ContentFingerprint string
			Number                                                                          int64
			Deliveries                                                                      []secrets.Delivery
			Artifact                                                                        secrets.Artifact
			CreatedAt, ActivatedAt                                                          time.Time
		} `json:"secretVersion"`
		Attestation struct {
			BindingID, SecretVersionID, LeafFingerprint, PublicKeyFingerprint, SecretContentFingerprint string
			Number                                                                                      int64
			DNSNames, IPAddresses                                                                       []string
			NotBefore, NotAfter, CreatedAt                                                              time.Time
			CreatedBy                                                                                   string
		} `json:"attestation"`
	}{Contract: certificateTargetContract}
	canonical.Binding.ID, canonical.Binding.OrganizationID = binding.ID, binding.Scope.OrganizationID
	canonical.Binding.ProjectID, canonical.Binding.EnvironmentID = binding.Scope.ProjectID, binding.Scope.EnvironmentID
	canonical.Binding.ApplicationID, canonical.Binding.Namespace = binding.Scope.ApplicationID, binding.Scope.Namespace
	canonical.Binding.Name, canonical.Binding.Provider, canonical.Binding.Purpose = binding.Name, string(binding.Provider), string(binding.Purpose)
	canonical.Binding.ActiveVersion, canonical.Binding.CreatedAt = binding.ActiveVersion, binding.CreatedAt.UTC()
	canonical.SecretVersion.ID, canonical.SecretVersion.BindingID = secretVersion.ID, secretVersion.BindingID
	canonical.SecretVersion.Provider, canonical.SecretVersion.TargetSecretType = string(secretVersion.Provider), string(secretVersion.TargetSecretType)
	canonical.SecretVersion.FingerprintKeyID = secretVersion.FingerprintKeyID
	canonical.SecretVersion.ContentFingerprint = hex.EncodeToString(secretVersion.ContentFingerprint[:])
	canonical.SecretVersion.Number, canonical.SecretVersion.Deliveries = secretVersion.Number, append([]secrets.Delivery(nil), secretVersion.Deliveries...)
	canonical.SecretVersion.Artifact = artifact
	canonical.SecretVersion.CreatedAt, canonical.SecretVersion.ActivatedAt = secretVersion.CreatedAt.UTC(), secretVersion.ActivatedAt.UTC()
	canonical.Attestation.BindingID, canonical.Attestation.SecretVersionID = attestation.BindingID, attestation.SecretVersionID
	canonical.Attestation.LeafFingerprint, canonical.Attestation.PublicKeyFingerprint = attestation.LeafFingerprint, attestation.PublicKeyFingerprint
	canonical.Attestation.SecretContentFingerprint = hex.EncodeToString(attestation.SecretContentFingerprint[:])
	canonical.Attestation.Number = attestation.Number
	canonical.Attestation.DNSNames, canonical.Attestation.IPAddresses = append([]string(nil), attestation.DNSNames...), append([]string(nil), attestation.IPAddresses...)
	canonical.Attestation.NotBefore, canonical.Attestation.NotAfter = attestation.NotBefore.UTC(), attestation.NotAfter.UTC()
	canonical.Attestation.CreatedBy, canonical.Attestation.CreatedAt = attestation.CreatedBy, attestation.CreatedAt.UTC()
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", ErrUnavailable
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func validateActiveCertificateTarget(binding secrets.Binding, secretVersion secrets.Version, attestation Version) error {
	if binding.Validate() != nil || secretVersion.Validate() != nil || attestation.ValidateFor(binding, secretVersion) != nil ||
		binding.Purpose != secrets.PurposeTLSCertificate || binding.Provider != secrets.ProviderSealedSecrets || binding.State != secrets.BindingReady ||
		secretVersion.BindingID != binding.ID || secretVersion.Number != binding.ActiveVersion || secretVersion.State != secrets.VersionActive ||
		secretVersion.Provider != secrets.ProviderSealedSecrets || secretVersion.TargetSecretType != secrets.TargetSecretTLS || secretVersion.Artifact == nil ||
		!slices.Equal(secretVersion.Deliveries, certificateDeliveries()) ||
		secretVersion.Artifact.Provider != secrets.ProviderSealedSecrets || secretVersion.Artifact.TargetSecretType != secrets.TargetSecretTLS ||
		secretVersion.Artifact.ObjectName != secretVersion.Artifact.TargetSecretName ||
		secretVersion.Artifact.ValidateFor(binding, secretVersion.Number) != nil {
		return ErrInvalid
	}
	return nil
}

type ObservationLease struct {
	BindingID    string
	VersionID    string
	Owner        string
	Epoch        int64
	ClaimedAt    time.Time
	Until        time.Time
	Identity     ObservationIdentity
	TargetDigest string
}

func (l ObservationLease) Validate() error {
	if !uuidRE.MatchString(l.BindingID) || !uuidRE.MatchString(l.VersionID) || !observationWorkerIDRE.MatchString(l.Owner) ||
		l.Epoch < 1 || l.ClaimedAt.IsZero() || l.Until.IsZero() || !l.Until.After(l.ClaimedAt) ||
		l.Identity.Validate() != nil || !digestRE.MatchString(l.TargetDigest) {
		return ErrObservationUnavailable
	}
	return nil
}

type ObservationWork struct {
	Binding             secrets.Binding
	SecretVersion       secrets.Version
	Attestation         Version
	Lease               ObservationLease
	ConsecutiveFailures int
}

func (w ObservationWork) Validate() error {
	digest, err := CertificateObservationTargetDigest(w.Binding, w.SecretVersion, w.Attestation)
	if err != nil || w.Lease.Validate() != nil || w.Binding.ID != w.Lease.BindingID || w.SecretVersion.ID != w.Lease.VersionID ||
		digest != w.Lease.TargetDigest || w.ConsecutiveFailures < 0 || w.ConsecutiveFailures > 30 {
		return ErrObservationUnavailable
	}
	return nil
}

type ObservationReadyOutcome struct {
	ObservedAt time.Time
	NextAt     time.Time
}

type ObservationDegradedOutcome struct {
	FailureCode ObservationFailureCode
	ObservedAt  time.Time
	NextAt      time.Time
}

type ObservationRequeueOutcome struct {
	FailureCode ObservationFailureCode
	NextAt      time.Time
}

func validateObservationTime(observedAt, nextAt, now, claimedAt time.Time) error {
	if observedAt.IsZero() || nextAt.IsZero() || now.IsZero() || claimedAt.IsZero() ||
		observedAt.Before(claimedAt.Add(-CertificateObservationReadinessSkew)) || observedAt.After(now.Add(CertificateObservationReadinessSkew)) ||
		!nextAt.After(now) || nextAt.After(now.Add(24*time.Hour)) {
		return ErrInvalid
	}
	return nil
}

type ObservationStore interface {
	ClaimCertificateObservation(context.Context, ObservationIdentity, string, []string, time.Time, time.Duration) (ObservationWork, error)
	HeartbeatCertificateObservation(context.Context, ObservationLease, time.Time, time.Duration) (ObservationLease, error)
	ApplyCertificateObservationReady(context.Context, ObservationLease, ObservationReadyOutcome, time.Time) error
	ApplyCertificateObservationDegraded(context.Context, ObservationLease, ObservationDegradedOutcome, time.Time) error
	RequeueCertificateObservation(context.Context, ObservationLease, ObservationRequeueOutcome, time.Time) error
	ActiveCertificateReady(context.Context, string, string, ObservationIdentity, time.Time, time.Duration) error

	AcquireCertificateObservationReadiness(context.Context, ObservationWorkerObservation, time.Duration) (ObservationReadinessLease, error)
	HeartbeatCertificateObservationReadiness(context.Context, ObservationReadinessLease, time.Time, time.Duration) (ObservationReadinessLease, error)
	CertificateObservationRuntimeReady(context.Context, ObservationIdentity, time.Time, time.Duration) error
}

type ObservationWorkerObservation struct {
	WorkerID   string
	Identity   ObservationIdentity
	StartedAt  time.Time
	ObservedAt time.Time
}

func (o ObservationWorkerObservation) Validate() error {
	if !observationWorkerIDRE.MatchString(o.WorkerID) || o.Identity.Validate() != nil || o.StartedAt.IsZero() ||
		o.ObservedAt.IsZero() || o.ObservedAt.Before(o.StartedAt) {
		return ErrObservationUnavailable
	}
	return nil
}

type ObservationReadinessLease struct {
	ObservationWorkerObservation
	Epoch int64
	Until time.Time
}

func (l ObservationReadinessLease) Validate() error {
	if l.ObservationWorkerObservation.Validate() != nil || l.Epoch < 1 || l.Until.IsZero() || !l.Until.After(l.ObservedAt) {
		return ErrObservationUnavailable
	}
	return nil
}

type ObservationReadinessProbe struct {
	Store  ObservationStore
	Config ObservationConfig
	MaxAge time.Duration
	Now    func() time.Time
}

func (p *ObservationReadinessProbe) Probe(ctx context.Context) error {
	if p == nil || p.Store == nil || p.Config.Validate() != nil {
		return ErrObservationUnavailable
	}
	identity, err := ObservationIdentityForConfig(p.Config)
	if err != nil {
		return ErrObservationUnavailable
	}
	now := time.Now().UTC()
	if p.Now != nil {
		now = p.Now().UTC()
	}
	maximumAge := p.MaxAge
	if maximumAge == 0 {
		maximumAge = CertificateObservationHeartbeatMaxAge
	}
	if maximumAge < 2*p.Config.HeartbeatInterval || maximumAge > 5*time.Minute {
		return ErrObservationUnavailable
	}
	if err = p.Store.CertificateObservationRuntimeReady(ctx, identity, now, maximumAge); err != nil {
		return ErrObservationUnavailable
	}
	return nil
}

func observationIdentityEqual(left, right ObservationIdentity) bool { return left == right }
