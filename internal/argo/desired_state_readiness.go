package argo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

const (
	DesiredStateHeartbeatInterval = 10 * time.Second
	DesiredStateHeartbeatMaxAge   = 35 * time.Second
	DesiredStateReadinessLease    = 45 * time.Second
)

// DesiredStateRuntimeConfig contains only operator-owned non-secret identity.
// RepositorySecretName is the name of an existing Argo repository Secret; its
// data is never read by this worker, persisted in these tables, or committed.
type DesiredStateRuntimeConfig struct {
	Enabled              bool
	GitHubAppID          int64
	PlatformBindingID    string
	ArgoNamespace        string
	RootApplicationName  string
	RepositorySecretName string
	Runtime              RuntimeLock
	DigestEnforcement    ChartDigestEnforcement
}

func (c DesiredStateRuntimeConfig) Validate() error {
	if !c.Enabled {
		if c.GitHubAppID != 0 || c.PlatformBindingID != "" || c.ArgoNamespace != "" ||
			c.RootApplicationName != "" || c.RepositorySecretName != "" || c.Runtime != (RuntimeLock{}) || c.DigestEnforcement != "" {
			return ErrInvalid
		}
		return nil
	}
	if c.GitHubAppID <= 0 || !uuidRE.MatchString(c.PlatformBindingID) ||
		!kubeRE.MatchString(c.ArgoNamespace) || !kubeRE.MatchString(c.RootApplicationName) || !kubeRE.MatchString(c.RepositorySecretName) ||
		c.Runtime.Validate() != nil || c.DigestEnforcement != ChartDigestNativeOCI {
		return ErrInvalid
	}
	return nil
}

func (c DesiredStateRuntimeConfig) RuntimeDigest() (string, error) {
	if c.Validate() != nil || !c.Enabled {
		return "", ErrInvalid
	}
	canonical := struct {
		Enabled              bool                   `json:"enabled"`
		GitHubAppID          int64                  `json:"githubAppId"`
		PlatformBindingID    string                 `json:"platformBindingId"`
		ArgoNamespace        string                 `json:"argoNamespace"`
		RootApplicationName  string                 `json:"rootApplicationName"`
		RepositorySecretName string                 `json:"repositorySecretName"`
		Runtime              RuntimeLock            `json:"runtime"`
		DigestEnforcement    ChartDigestEnforcement `json:"digestEnforcement"`
	}{true, c.GitHubAppID, c.PlatformBindingID, c.ArgoNamespace, c.RootApplicationName, c.RepositorySecretName, c.Runtime, c.DigestEnforcement}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", ErrInvalid
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

type DesiredStateRuntimeIdentity struct {
	DesiredStateWorkerIdentity
	GitHubAppID          int64                  `json:"githubAppId"`
	PlatformBindingID    string                 `json:"platformBindingId"`
	ArgoNamespace        string                 `json:"argoNamespace"`
	RootApplicationName  string                 `json:"rootApplicationName"`
	RepositorySecretName string                 `json:"repositorySecretName"`
	Runtime              RuntimeLock            `json:"runtime"`
	DigestEnforcement    ChartDigestEnforcement `json:"digestEnforcement"`
}

func DesiredStateRuntimeIdentityForConfig(config DesiredStateRuntimeConfig) (DesiredStateRuntimeIdentity, error) {
	digest, err := config.RuntimeDigest()
	if err != nil {
		return DesiredStateRuntimeIdentity{}, err
	}
	return DesiredStateRuntimeIdentity{
		DesiredStateWorkerIdentity: DesiredStateWorkerIdentity{ContractVersion: DesiredStateContract, ConfigDigest: digest},
		GitHubAppID:                config.GitHubAppID, PlatformBindingID: config.PlatformBindingID,
		ArgoNamespace: config.ArgoNamespace, RootApplicationName: config.RootApplicationName,
		RepositorySecretName: config.RepositorySecretName, Runtime: config.Runtime, DigestEnforcement: config.DigestEnforcement,
	}, nil
}

func (i DesiredStateRuntimeIdentity) Validate() error {
	if i.DesiredStateWorkerIdentity.Validate() != nil || i.GitHubAppID <= 0 || !uuidRE.MatchString(i.PlatformBindingID) ||
		!kubeRE.MatchString(i.ArgoNamespace) || !kubeRE.MatchString(i.RootApplicationName) ||
		!kubeRE.MatchString(i.RepositorySecretName) || i.Runtime.Validate() != nil || i.DigestEnforcement != ChartDigestNativeOCI {
		return ErrInvalid
	}
	digest, err := (DesiredStateRuntimeConfig{
		Enabled: true, GitHubAppID: i.GitHubAppID, PlatformBindingID: i.PlatformBindingID,
		ArgoNamespace: i.ArgoNamespace, RootApplicationName: i.RootApplicationName,
		RepositorySecretName: i.RepositorySecretName, Runtime: i.Runtime, DigestEnforcement: i.DigestEnforcement,
	}).RuntimeDigest()
	if err != nil || digest != i.ConfigDigest {
		return ErrInvalid
	}
	return nil
}

type DesiredStateRuntimeWorkerObservation struct {
	WorkerID string `json:"workerId"`
	DesiredStateRuntimeIdentity
	StartedAt  time.Time `json:"startedAt"`
	ObservedAt time.Time `json:"observedAt"`
}

func (o DesiredStateRuntimeWorkerObservation) Validate() error {
	if !desiredStateOwnerRE.MatchString(o.WorkerID) || o.DesiredStateRuntimeIdentity.Validate() != nil || o.StartedAt.IsZero() ||
		o.ObservedAt.IsZero() || o.ObservedAt.Before(o.StartedAt) {
		return ErrInvalid
	}
	return nil
}

type DesiredStateRuntimeLease struct {
	DesiredStateRuntimeWorkerObservation
	Epoch int64     `json:"epoch"`
	Until time.Time `json:"until"`
}

func (l DesiredStateRuntimeLease) Validate() error {
	if l.DesiredStateRuntimeWorkerObservation.Validate() != nil || l.Epoch <= 0 || l.Until.IsZero() || !l.Until.After(l.ObservedAt) {
		return ErrInvalid
	}
	return nil
}

type DesiredStateReadinessStore interface {
	AcquireDesiredStateReadiness(context.Context, DesiredStateRuntimeWorkerObservation, time.Duration) (DesiredStateRuntimeLease, error)
	HeartbeatDesiredStateReadiness(context.Context, DesiredStateRuntimeLease, time.Time, time.Duration) (DesiredStateRuntimeLease, error)
	DesiredStateRuntimeReady(context.Context, DesiredStateRuntimeIdentity, time.Time, time.Duration) error
}

type DesiredStateReadinessProbe struct {
	Store    DesiredStateReadinessStore
	Identity DesiredStateRuntimeIdentity
	MaxAge   time.Duration
	Now      func() time.Time
}

func (p *DesiredStateReadinessProbe) Probe(ctx context.Context) error {
	if p == nil || p.Store == nil || p.Identity.Validate() != nil {
		return ErrDesiredStateNotReady
	}
	maximumAge := p.MaxAge
	if maximumAge == 0 {
		maximumAge = DesiredStateHeartbeatMaxAge
	}
	if maximumAge < 2*DesiredStateHeartbeatInterval || maximumAge > 5*time.Minute {
		return ErrDesiredStateNotReady
	}
	now := time.Now().UTC()
	if p.Now != nil {
		now = p.Now().UTC()
	}
	if err := p.Store.DesiredStateRuntimeReady(ctx, p.Identity, now, maximumAge); err != nil {
		return ErrDesiredStateNotReady
	}
	return nil
}

func validDesiredStateReadinessLease(duration time.Duration) bool {
	return duration >= 2*DesiredStateHeartbeatInterval && duration <= 5*time.Minute
}
