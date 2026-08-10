package imagepull

import (
	"context"
	"time"
)

// ReadinessProbe proves that a worker with the exact operator-owned profile
// configuration currently holds a fresh durable readiness lease. Individual
// artifact health is deliberately checked by the deployment/Argo eligibility
// path: one broken private registry must not hide public-image functionality.
type ReadinessProbe struct {
	Store  Store
	Config RuntimeConfig
	Now    func() time.Time
}

func (p *ReadinessProbe) Probe(ctx context.Context) error {
	if p == nil || p.Store == nil || p.Config.Validate() != nil || !p.Config.Enabled {
		return ErrUnavailable
	}
	digest, err := p.Config.Digest()
	if err != nil {
		return ErrUnavailable
	}
	now := time.Now().UTC()
	if p.Now != nil {
		now = p.Now().UTC()
	}
	if now.IsZero() || p.Store.RuntimeReady(ctx, RuntimeContract, digest, len(p.Config.Profiles), now) != nil {
		return ErrUnavailable
	}
	return nil
}
