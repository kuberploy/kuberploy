package secrets

import (
	"context"
	"slices"
	"time"
)

const (
	StrictSealingScope         = "strict"
	ExternalRefreshCreatedOnce = "CreatedOnce"
)

// StageRequest is derived entirely from durable scope and version metadata.
// TargetSecretName is deterministic, preventing a provider from redirecting
// material into a different workload or Kubernetes Secret.
type StageRequest struct {
	Binding               Binding
	Version               Version
	TargetSecretName      string
	ExplicitKeys          []string
	ImmutableTarget       bool
	ExternalRefreshPolicy string
	SealingScope          string
}

func (r StageRequest) validate() error {
	if r.Binding.Validate() != nil || r.Version.Validate() != nil || r.Version.BindingID != r.Binding.ID ||
		r.Version.Provider != r.Binding.Provider || r.Version.State != VersionStaging ||
		r.TargetSecretName != TargetSecretName(r.Binding, r.Version.Number) || !r.ImmutableTarget ||
		!validPurposeTarget(r.Binding.Purpose, r.Binding.Provider, r.Version.TargetSecretType) {
		return ErrInvalid
	}
	if r.Version.TargetSecretType == TargetSecretTLS && r.Binding.Provider != ProviderSealedSecrets {
		return ErrInvalid
	}
	expectedKeys := deliveryKeys(r.Version.Deliveries)
	if !slices.Equal(r.ExplicitKeys, expectedKeys) {
		return ErrInvalid
	}
	if r.Binding.Provider == ProviderSealedSecrets {
		if r.SealingScope != StrictSealingScope || r.ExternalRefreshPolicy != "" {
			return ErrInvalid
		}
	} else if r.SealingScope != "" || r.ExternalRefreshPolicy != ExternalRefreshCreatedOnce {
		return ErrInvalid
	}
	return nil
}

func deliveryKeys(deliveries []Delivery) []string {
	set := make(map[string]struct{}, len(deliveries))
	for _, delivery := range deliveries {
		set[delivery.SourceKey] = struct{}{}
	}
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

type ReadinessStatus string

const (
	ReadinessPending ReadinessStatus = "pending"
	ReadinessReady   ReadinessStatus = "ready"
	ReadinessFailed  ReadinessStatus = "failed"
)

// ReadinessObservation must echo the complete immutable artifact identity.
// Conditions and Secret data are intentionally not accepted.
type ReadinessObservation struct {
	Artifact    Artifact
	Status      ReadinessStatus
	FailureCode string
	ObservedAt  time.Time
}

func (o ReadinessObservation) validate(expected Artifact) error {
	if o.Artifact != expected || o.ObservedAt.IsZero() {
		return ErrProviderMismatch
	}
	switch o.Status {
	case ReadinessPending, ReadinessReady:
		if o.FailureCode != "" {
			return ErrProviderMismatch
		}
	case ReadinessFailed:
		if !safeCodeRE.MatchString(o.FailureCode) {
			return ErrProviderMismatch
		}
	default:
		return ErrProviderMismatch
	}
	return nil
}

type DeleteObservation struct {
	Artifact   Artifact
	Absent     bool
	ObservedAt time.Time
}

func (o DeleteObservation) validate(expected Artifact) error {
	if o.Artifact != expected || !o.Absent || o.ObservedAt.IsZero() {
		return ErrProviderMismatch
	}
	return nil
}

// ExternalSecretsProvider writes material to an operator-controlled external
// backend and returns only the resulting ExternalSecret identity and digests.
// Implementations must consume Material synchronously and must not retain it.
// Stage must be idempotent for the immutable Version.ID because a process can
// lose its response after the provider accepted the write.
type ExternalSecretsProvider interface {
	StageExternalSecret(context.Context, StageRequest, *Material) (Artifact, error)
	ObserveExternalSecret(context.Context, Artifact) (ReadinessObservation, error)
	DeleteExternalSecret(context.Context, Artifact) (DeleteObservation, error)
}

// SealedSecretsProvider performs strict namespace/name-bound sealing. The
// ciphertext itself belongs in Git/provider storage and is never returned to
// or persisted by this package; only its digest is retained. Stage must be
// idempotent for the immutable Version.ID.
type SealedSecretsProvider interface {
	StageStrictSealedSecret(context.Context, StageRequest, *Material) (Artifact, error)
	ObserveStrictSealedSecret(context.Context, Artifact) (ReadinessObservation, error)
	DeleteStrictSealedSecret(context.Context, Artifact) (DeleteObservation, error)
}
