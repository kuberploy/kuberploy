package builds

import (
	"context"
	"errors"
	"time"
)

const (
	SourceBuildHeartbeatInterval = 10 * time.Second
	SourceBuildHeartbeatMaxAge   = 35 * time.Second
)

var ErrRuntimeNotReady = errors.New("matching source-build worker is not ready")

type SourceBuildRuntimeIdentity struct {
	ConfigDigest      string
	GitHubAppID       int64
	BuilderNamespace  string
	BuilderAgentImage string
}

func RuntimeIdentity(config WorkerRuntimeConfig) (SourceBuildRuntimeIdentity, error) {
	digest, err := config.RuntimeDigest()
	if err != nil {
		return SourceBuildRuntimeIdentity{}, err
	}
	return SourceBuildRuntimeIdentity{ConfigDigest: digest, GitHubAppID: config.GitHub.AppID,
		BuilderNamespace: config.BuilderNamespace, BuilderAgentImage: config.BuilderAgentImage}, nil
}

func (i SourceBuildRuntimeIdentity) validate() error {
	if !digestRE.MatchString(i.ConfigDigest) || i.GitHubAppID < 1 || !kubeNameRE.MatchString(i.BuilderNamespace) ||
		!validRuntimeAgentImage(i.BuilderAgentImage) {
		return ErrInvalid
	}
	return nil
}

type SourceBuildWorkerObservation struct {
	WorkerID string
	SourceBuildRuntimeIdentity
	StartedAt  time.Time
	ObservedAt time.Time
}

func (o SourceBuildWorkerObservation) validate() error {
	if err := o.SourceBuildRuntimeIdentity.validate(); err != nil || !setupIdempotencyRE.MatchString(o.WorkerID) ||
		o.StartedAt.IsZero() || o.ObservedAt.IsZero() || o.ObservedAt.Before(o.StartedAt) {
		return ErrInvalid
	}
	return nil
}

type RuntimeReadinessStore interface {
	ObserveSourceBuildWorker(context.Context, SourceBuildWorkerObservation) error
	SourceBuildRuntimeReady(context.Context, SourceBuildRuntimeIdentity, time.Time, time.Duration) error
}

type RuntimeReadinessProbe struct {
	Store    RuntimeReadinessStore
	Identity SourceBuildRuntimeIdentity
	MaxAge   time.Duration
	Now      func() time.Time
}

func (p *RuntimeReadinessProbe) Probe(ctx context.Context) error {
	if p == nil || p.Store == nil || p.Identity.validate() != nil {
		return ErrRuntimeNotReady
	}
	maximumAge := p.MaxAge
	if maximumAge == 0 {
		maximumAge = SourceBuildHeartbeatMaxAge
	}
	if maximumAge < 2*SourceBuildHeartbeatInterval || maximumAge > 5*time.Minute {
		return ErrRuntimeNotReady
	}
	now := time.Now().UTC()
	if p.Now != nil {
		now = p.Now().UTC()
	}
	if err := p.Store.SourceBuildRuntimeReady(ctx, p.Identity, now, maximumAge); err != nil {
		return ErrRuntimeNotReady
	}
	return nil
}
