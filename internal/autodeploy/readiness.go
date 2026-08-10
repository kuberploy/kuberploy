package autodeploy

import (
	"context"
	"errors"
	"time"
)

const (
	RuntimeContractVersion  = "auto-deploy.v1"
	RuntimeHeartbeatPeriod  = 10 * time.Second
	RuntimeReadinessMaxAge  = 35 * time.Second
	RuntimeReadinessLease   = 30 * time.Second
	maximumRuntimeClockSkew = 30 * time.Second
)

var ErrRuntimeNotReady = errors.New("matching auto-deploy controller is not ready")

type RuntimeIdentity struct {
	ContractVersion      string
	OperatorConfigDigest string
}

func (i RuntimeIdentity) Validate() error {
	if i.ContractVersion != RuntimeContractVersion || !digestRE.MatchString(i.OperatorConfigDigest) {
		return ErrInvalid
	}
	return nil
}

type RuntimeObservation struct {
	WorkerID string
	RuntimeIdentity
	StartedAt  time.Time
	ObservedAt time.Time
}

func (o RuntimeObservation) validate() error {
	if o.RuntimeIdentity.Validate() != nil || o.WorkerID == "" || len(o.WorkerID) > 128 || o.StartedAt.IsZero() ||
		o.ObservedAt.IsZero() || o.ObservedAt.Before(o.StartedAt) || o.ObservedAt.After(time.Now().UTC().Add(maximumRuntimeClockSkew)) {
		return ErrInvalid
	}
	return nil
}

type RuntimeLease struct {
	RuntimeObservation
	Epoch int64
	Until time.Time
}

type RuntimeReadinessStore interface {
	AcquireRuntimeReadiness(context.Context, RuntimeObservation, time.Duration) (RuntimeLease, error)
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
		maximumAge = RuntimeReadinessMaxAge
	}
	if maximumAge < 2*RuntimeHeartbeatPeriod || maximumAge > 5*time.Minute {
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
