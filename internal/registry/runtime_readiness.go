package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

const (
	// ManagedRegistryRuntimeContract is bumped whenever the observer or cleanup
	// executor boundary changes in a way that an older worker cannot satisfy.
	ManagedRegistryRuntimeContract = "registry-runtime-v2"

	ManagedRegistryHeartbeatInterval  = 10 * time.Second
	ManagedRegistryHeartbeatMaxAge    = 35 * time.Second
	ManagedRegistryReadinessLease     = 45 * time.Second
	ManagedRegistryReadinessClockSkew = 5 * time.Second
)

var (
	ErrRegistryRuntimeNotReady = errors.New("matching managed registry worker is not ready")
	registryWorkerIDRE         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$`)
	registryContractRE         = regexp.MustCompile(`^[a-z][a-z0-9.-]{7,63}$`)
	registryReadinessDigestRE  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// RuntimeIdentity binds a worker heartbeat to the complete operator-owned
// configuration and to the code contract implemented by its observer and
// cleanup executor. LifecycleCredentialRef is an operator configuration
// identity, never the target's build-push credential and never credential
// material. ConfigDigest contains no credential material.
type RuntimeIdentity struct {
	TargetID               string
	TargetEndpoint         string
	TargetRepositoryPrefix string
	LifecycleCredentialRef string
	ConfigDigest           string
	ContractVersion        string
}

func RuntimeIdentityForConfig(config RuntimeConfig) (RuntimeIdentity, error) {
	digest, err := config.RuntimeDigest()
	if err != nil {
		return RuntimeIdentity{}, err
	}
	return RuntimeIdentity{TargetID: config.TargetID, TargetEndpoint: config.Endpoint,
		TargetRepositoryPrefix: config.RepositoryPrefix, LifecycleCredentialRef: config.CredentialRef,
		ConfigDigest: digest, ContractVersion: ManagedRegistryRuntimeContract}, nil
}

func (i RuntimeIdentity) Validate() error {
	if !registryUUIDRE.MatchString(i.TargetID) || !registryReadinessDigestRE.MatchString(i.ConfigDigest) ||
		!registryContractRE.MatchString(i.ContractVersion) || strings.TrimSpace(i.TargetEndpoint) != i.TargetEndpoint ||
		i.TargetEndpoint == "" || !validRepository(i.TargetRepositoryPrefix) || !registryCredentialRefRE.MatchString(i.LifecycleCredentialRef) {
		return errRegistryRuntimeConfig
	}
	return nil
}

// RuntimeDigest covers every setting consumed by either registry runtime
// loop. The credential reference is included; secret values never are.
func (c RuntimeConfig) RuntimeDigest() (string, error) {
	if c.Validate() != nil || !c.Enabled {
		return "", errRegistryRuntimeConfig
	}
	canonical := struct {
		Enabled                bool   `json:"enabled"`
		TargetID               string `json:"targetId"`
		TargetName             string `json:"targetName"`
		Endpoint               string `json:"endpoint"`
		RepositoryPrefix       string `json:"repositoryPrefix"`
		PullCredentialRef      string `json:"pullCredentialRef"`
		PushCredentialRef      string `json:"pushCredentialRef"`
		CacheCredentialRef     string `json:"cacheCredentialRef"`
		LifecycleCredentialRef string `json:"lifecycleCredentialRef"`
		AllowPlainHTTP         bool   `json:"allowPlainHttp"`
		Namespace              string `json:"namespace"`
		Deployment             string `json:"deployment"`
		PersistentVolumeClaim  string `json:"persistentVolumeClaim"`
		RegistryConfigMap      string `json:"registryConfigMap"`
		HelperServiceAccount   string `json:"helperServiceAccount"`
		HelperImage            string `json:"helperImage"`
		ObservationNanos       int64  `json:"observationNanos"`
	}{
		Enabled: c.Enabled, TargetID: c.TargetID, TargetName: c.TargetName, Endpoint: c.Endpoint,
		RepositoryPrefix: c.RepositoryPrefix, LifecycleCredentialRef: c.CredentialRef,
		PullCredentialRef: c.PullCredentialRef, PushCredentialRef: c.PushCredentialRef,
		CacheCredentialRef: c.CacheCredentialRef,
		AllowPlainHTTP:     c.AllowPlainHTTP, Namespace: c.Namespace, Deployment: c.Deployment,
		PersistentVolumeClaim: c.PersistentVolumeClaim, RegistryConfigMap: c.RegistryConfigMap,
		HelperServiceAccount: c.HelperServiceAccount, HelperImage: c.HelperImage,
		ObservationNanos: int64(c.ObservationInterval),
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", errRegistryRuntimeConfig
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

type RuntimeWorkerObservation struct {
	WorkerID string
	RuntimeIdentity
	StartedAt  time.Time
	ObservedAt time.Time
}

func (o RuntimeWorkerObservation) Validate() error {
	if !registryWorkerIDRE.MatchString(o.WorkerID) || o.RuntimeIdentity.Validate() != nil ||
		o.StartedAt.IsZero() || o.ObservedAt.IsZero() || o.ObservedAt.Before(o.StartedAt) {
		return errRegistryRuntimeConfig
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
		return errRegistryRuntimeConfig
	}
	return nil
}

type RuntimeReadinessStore interface {
	AcquireManagedRegistryReadiness(context.Context, RuntimeWorkerObservation, time.Duration) (RuntimeReadinessLease, error)
	HeartbeatManagedRegistryReadiness(context.Context, RuntimeReadinessLease, time.Time, time.Duration) (RuntimeReadinessLease, error)
	ManagedRegistryRuntimeReady(context.Context, RuntimeIdentity, time.Time, time.Duration) error
}

type RuntimeReadinessTargetCatalog interface {
	RegistryTarget(context.Context, string) (domain.RegistryTarget, error)
}

// RuntimeReadinessProbe revalidates the mutable database target on every
// request before accepting a fresh worker observation. A target edit therefore
// disables the public capability immediately, even before heartbeat expiry.
type RuntimeReadinessProbe struct {
	Store   RuntimeReadinessStore
	Targets RuntimeReadinessTargetCatalog
	Config  RuntimeConfig
	MaxAge  time.Duration
	Now     func() time.Time
}

func (p *RuntimeReadinessProbe) Probe(ctx context.Context) error {
	if p == nil || p.Store == nil || p.Targets == nil || p.Config.Validate() != nil || !p.Config.Enabled {
		return ErrRegistryRuntimeNotReady
	}
	identity, err := RuntimeIdentityForConfig(p.Config)
	if err != nil {
		return ErrRegistryRuntimeNotReady
	}
	target, err := p.Targets.RegistryTarget(ctx, identity.TargetID)
	if err != nil || p.Config.ValidateTarget(target) != nil {
		return ErrRegistryRuntimeNotReady
	}
	maximumAge := p.MaxAge
	if maximumAge == 0 {
		maximumAge = ManagedRegistryHeartbeatMaxAge
	}
	if maximumAge < 2*ManagedRegistryHeartbeatInterval || maximumAge > 5*time.Minute {
		return ErrRegistryRuntimeNotReady
	}
	now := time.Now().UTC()
	if p.Now != nil {
		now = p.Now().UTC()
	}
	if err = p.Store.ManagedRegistryRuntimeReady(ctx, identity, now, maximumAge); err != nil {
		return ErrRegistryRuntimeNotReady
	}
	return nil
}
