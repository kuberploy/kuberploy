package main

import (
	"context"
	"errors"
	"testing"
)

type readinessPinger struct {
	err    error
	called int
}

func (p *readinessPinger) Ping(context.Context) error {
	p.called++
	return p.err
}

func TestValkeyReadinessProbeFailsClosedAndPropagatesPing(t *testing.T) {
	if err := (valkeyReadinessProbe{}).Probe(t.Context()); err == nil {
		t.Fatal("nil Valkey client was reported ready")
	}
	pinger := &readinessPinger{err: errors.New("unavailable")}
	if err := (valkeyReadinessProbe{pinger: pinger}).Probe(t.Context()); !errors.Is(err, pinger.err) || pinger.called != 1 {
		t.Fatalf("probe error=%v calls=%d", err, pinger.called)
	}
	pinger.err = nil
	if err := (valkeyReadinessProbe{pinger: pinger}).Probe(t.Context()); err != nil || pinger.called != 2 {
		t.Fatalf("healthy probe error=%v calls=%d", err, pinger.called)
	}
}
