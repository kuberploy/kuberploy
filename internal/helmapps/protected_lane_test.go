package helmapps

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

func TestProtectedLaneRoundRobinActivatesOnceAndIsFair(t *testing.T) {
	var lane protectedLaneRoundRobin
	var activations atomic.Int64
	var phases [protectedLanePhaseCount]atomic.Int64
	callbacks := [protectedLanePhaseCount]func(context.Context) error{}
	for index := range callbacks {
		index := index
		callbacks[index] = func(context.Context) error {
			phases[index].Add(1)
			return ErrNotFound
		}
	}
	for range 40 {
		err := lane.processOne(t.Context(), func(context.Context) error {
			activations.Add(1)
			return nil
		}, callbacks)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("round-robin result=%v", err)
		}
	}
	if activations.Load() != 40 {
		t.Fatalf("activations=%d", activations.Load())
	}
	for index := range phases {
		if phases[index].Load() != 10 {
			t.Fatalf("phase %d calls=%d", index, phases[index].Load())
		}
	}
}

func TestProtectedLaneRoundRobinDoesNotAdvancePastFailedActivation(t *testing.T) {
	var lane protectedLaneRoundRobin
	var phaseCalls atomic.Int64
	callbacks := [protectedLanePhaseCount]func(context.Context) error{}
	for index := range callbacks {
		callbacks[index] = func(context.Context) error {
			phaseCalls.Add(1)
			return nil
		}
	}
	want := errors.New("activation unavailable")
	if err := lane.processOne(t.Context(), func(context.Context) error { return want }, callbacks); !errors.Is(err, want) || phaseCalls.Load() != 0 {
		t.Fatalf("result=%v phaseCalls=%d", err, phaseCalls.Load())
	}
}
