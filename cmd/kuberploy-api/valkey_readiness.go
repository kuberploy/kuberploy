package main

import (
	"context"
	"errors"
)

type valkeyPinger interface {
	Ping(context.Context) error
}

// valkeyReadinessProbe keeps the HTTP layer independent from a concrete
// Valkey client while making the production API's required cache/limiter
// dependency observable through /readyz.
type valkeyReadinessProbe struct {
	pinger valkeyPinger
}

func (p valkeyReadinessProbe) Probe(ctx context.Context) error {
	if p.pinger == nil {
		return errors.New("Valkey readiness client is not configured")
	}
	return p.pinger.Ping(ctx)
}
