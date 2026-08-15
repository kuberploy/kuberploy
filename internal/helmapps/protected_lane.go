package helmapps

import (
	"context"
	"fmt"
	"sync/atomic"
)

const protectedLanePhaseCount = 4

// ProtectedLaneScheduler gives the four protected Helm phases one fair,
// process-local lane. Each cycle activates authority once and performs at most
// one durable operation. Database advisory locks remain the cross-process
// authority boundary; this scheduler prevents phase-aligned goroutines in one
// worker from manufacturing avoidable serializable conflicts.
type ProtectedLaneScheduler struct {
	Publisher  *ProtectedGitPublisher
	Cascade    *ProtectedCascadeObserver
	roundRobin protectedLaneRoundRobin
}

type protectedLaneRoundRobin struct{ cursor atomic.Uint32 }

func (s *ProtectedLaneScheduler) Validate() error {
	if s == nil || s.Publisher == nil || s.Cascade == nil ||
		s.Publisher.Validate() != nil || s.Cascade.Validate() != nil ||
		s.Publisher.WorkerID != s.Cascade.WorkerID ||
		s.Publisher.WorkerEpoch != s.Cascade.WorkerEpoch ||
		s.Publisher.Publisher != s.Cascade.Publisher {
		return ErrInvalid
	}
	return nil
}

func (s *ProtectedLaneScheduler) ProcessOne(ctx context.Context) error {
	if s.Validate() != nil || ctx == nil {
		return ErrInvalid
	}
	phases := [protectedLanePhaseCount]func(context.Context) error{
		func(current context.Context) error {
			_, err := s.Publisher.processPayloadOneActivated(current)
			return err
		},
		func(current context.Context) error {
			_, err := s.Publisher.processApplicationOneActivated(current)
			return err
		},
		func(current context.Context) error {
			_, err := s.Publisher.processCascadePreflightOneActivated(current)
			return err
		},
		func(current context.Context) error {
			_, err := s.Cascade.processOneActivated(current)
			return err
		},
	}
	return s.roundRobin.processOne(ctx, func(current context.Context) error {
		return s.Publisher.activate(current, s.Publisher.now())
	}, phases)
}

func (r *protectedLaneRoundRobin) processOne(ctx context.Context, activate func(context.Context) error,
	phases [protectedLanePhaseCount]func(context.Context) error) error {
	if r == nil || ctx == nil || activate == nil {
		return ErrInvalid
	}
	for _, phase := range phases {
		if phase == nil {
			return ErrInvalid
		}
	}
	if err := activate(ctx); err != nil {
		return fmt.Errorf("activate protected lane: %w", err)
	}
	phase := (r.cursor.Add(1) - 1) % protectedLanePhaseCount
	if err := phases[phase](ctx); err != nil {
		return fmt.Errorf("protected lane phase %d: %w", phase, err)
	}
	return nil
}
