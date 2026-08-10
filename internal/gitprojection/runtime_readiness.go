package gitprojection

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

const (
	RuntimeContract          = "git-projection-runtime-v2"
	RuntimeHeartbeatInterval = 10 * time.Second
	RuntimeHeartbeatMaxAge   = 35 * time.Second
	RuntimeReadinessLease    = 45 * time.Second
)

var ErrRuntimeNotReady = errors.New("matching Git projection worker is not ready")

type RuntimeIdentity struct {
	ContractVersion string
	ConfigDigest    string
	GitHubAppID     int64
}

func RuntimeIdentityForConfig(config RuntimeConfig, appConfigPolicyDigest string) (RuntimeIdentity, error) {
	projectionDigest, err := config.RuntimeDigest()
	if err != nil {
		return RuntimeIdentity{}, err
	}
	if !digestRE.MatchString(appConfigPolicyDigest) {
		return RuntimeIdentity{}, ErrInvalid
	}
	canonical := struct {
		ContractVersion       string `json:"contractVersion"`
		ProjectionConfig      string `json:"projectionConfigDigest"`
		AppConfigPolicyDigest string `json:"appConfigPolicyDigest"`
	}{RuntimeContract, projectionDigest, appConfigPolicyDigest}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return RuntimeIdentity{}, ErrInvalid
	}
	digest := sha256.Sum256(encoded)
	return RuntimeIdentity{ContractVersion: RuntimeContract, ConfigDigest: "sha256:" + hex.EncodeToString(digest[:]), GitHubAppID: config.GitHub.AppID}, nil
}

func (i RuntimeIdentity) Validate() error {
	if i.ContractVersion != RuntimeContract || !digestRE.MatchString(i.ConfigDigest) || i.GitHubAppID <= 0 {
		return ErrInvalid
	}
	return nil
}

type RuntimeWorkerObservation struct {
	WorkerID string
	RuntimeIdentity
	StartedAt  time.Time
	ObservedAt time.Time
}

func (o RuntimeWorkerObservation) Validate() error {
	if o.RuntimeIdentity.Validate() != nil || !ownerRE.MatchString(o.WorkerID) || o.StartedAt.IsZero() || o.ObservedAt.IsZero() || o.ObservedAt.Before(o.StartedAt) {
		return ErrInvalid
	}
	return nil
}

type RuntimeLease struct {
	RuntimeWorkerObservation
	Epoch int64
	Until time.Time
}

func (l RuntimeLease) Validate() error {
	if l.RuntimeWorkerObservation.Validate() != nil || l.Epoch <= 0 || l.Until.IsZero() || !l.Until.After(l.ObservedAt) {
		return ErrInvalid
	}
	return nil
}

type RuntimeReadinessStore interface {
	AcquireRuntimeReadiness(context.Context, RuntimeWorkerObservation, time.Duration) (RuntimeLease, error)
	HeartbeatRuntimeReadiness(context.Context, RuntimeLease, time.Time, time.Duration) (RuntimeLease, error)
	RuntimeReady(context.Context, RuntimeIdentity, time.Time, time.Duration) error
}

type RuntimeReadinessProbe struct {
	Store    RuntimeReadinessStore
	Identity RuntimeIdentity
	MaxAge   time.Duration
	Now      func() time.Time
}

func (p *RuntimeReadinessProbe) Probe(ctx context.Context) error {
	if p == nil || p.Store == nil || p.Identity.Validate() != nil {
		return ErrRuntimeNotReady
	}
	maximumAge := p.MaxAge
	if maximumAge == 0 {
		maximumAge = RuntimeHeartbeatMaxAge
	}
	if maximumAge < 2*RuntimeHeartbeatInterval || maximumAge > 5*time.Minute {
		return ErrRuntimeNotReady
	}
	now := time.Now().UTC()
	if p.Now != nil {
		now = p.Now().UTC()
	}
	if err := p.Store.RuntimeReady(ctx, p.Identity, now, maximumAge); err != nil {
		return ErrRuntimeNotReady
	}
	return nil
}
