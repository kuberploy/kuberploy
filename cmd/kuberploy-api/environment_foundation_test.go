package main

import (
	"context"
	"errors"
	"testing"
)

type orderedReadinessProbe struct {
	order *[]int
	value int
	err   error
}

func (p orderedReadinessProbe) Probe(context.Context) error {
	*p.order = append(*p.order, p.value)
	return p.err
}

func TestCombinedReadinessIsOrderedAndFailClosed(t *testing.T) {
	wantErr := errors.New("foundation not ready")
	order := []int{}
	probe := combinedReadiness{
		orderedReadinessProbe{order: &order, value: 1, err: wantErr},
		orderedReadinessProbe{order: &order, value: 2},
	}
	if err := probe.Probe(t.Context()); !errors.Is(err, wantErr) || len(order) != 1 || order[0] != 1 {
		t.Fatalf("combined readiness did not stop at foundation: order=%v err=%v", order, err)
	}
	if err := (combinedReadiness{nil}).Probe(t.Context()); err == nil {
		t.Fatal("combined readiness accepted a missing probe")
	}
}
