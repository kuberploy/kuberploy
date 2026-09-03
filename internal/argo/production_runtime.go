package argo

import (
	"context"
	"errors"
	"time"

	"github.com/kuberploy/kuberploy/internal/githubapp"
)

// ProductionDesiredStateMaterializer discovers exact active indexed
// environment generations, calls DesiredStatePlanner, and durably creates at
// most one new protected command. It is deliberately separate from the Git
// writer: materialization cannot mutate Kubernetes or Argo directly.
type ProductionDesiredStateMaterializer interface {
	MaterializeDesiredStateOnce(context.Context, time.Time) (bool, error)
}

// ProductionDesiredStateRuntime is the only runtime allowed to advertise the
// 024 readiness identity. A heartbeat is written only after a fresh composite
// prerequisite proof; it then plans and executes the protected, lease-fenced
// writer. An infrastructure, planner, claim-loop, credential, or root-object
// failure terminates the runtime and lets the durable observation age out.
type ProductionDesiredStateRuntime struct {
	Worker        *DesiredStateRuntimeWorker
	Prerequisites ProductionPrerequisiteObserver
	Materializer  ProductionDesiredStateMaterializer
	PollInterval  time.Duration
	Now           func() time.Time
	// readinessHeartbeatInterval is overridden only by focused package tests.
	// Production uses DesiredStateHeartbeatInterval.
	readinessHeartbeatInterval time.Duration
	// ReportPrerequisiteError receives the exact reason a composite readiness
	// proof was withheld. Production callers use it for operator diagnostics;
	// readiness remains fail-closed when the callback is nil.
	ReportPrerequisiteError func(error)
}

// ProductionDesiredStateReadinessProbe is the single API-facing readiness
// seam for Argo and all route-dependent capabilities. The matching durable
// lease can only be emitted by ProductionDesiredStateRuntime after its exact
// credential, provider-protection, root-Application, projection-policy, and
// claim-gate checks. Wiring the legacy worker probe directly is not a
// production capability boundary.
type ProductionDesiredStateReadinessProbe struct {
	Store    DesiredStateReadinessStore
	Identity DesiredStateRuntimeIdentity
	MaxAge   time.Duration
	Now      func() time.Time
}

func (p *ProductionDesiredStateReadinessProbe) Probe(ctx context.Context) error {
	if p == nil {
		return ErrDesiredStateNotReady
	}
	return (&DesiredStateReadinessProbe{Store: p.Store, Identity: p.Identity, MaxAge: p.MaxAge, Now: p.Now}).Probe(ctx)
}

func (r *ProductionDesiredStateRuntime) validate() error {
	if r == nil || r.Worker == nil || r.Worker.validate() != nil || r.Prerequisites == nil || r.Materializer == nil {
		return ErrInvalid
	}
	if _, ok := r.Worker.Writer.ClaimGate.(ProductionDesiredStateClaimGate); !ok {
		return ErrInvalid
	}
	poll := r.PollInterval
	if poll == 0 {
		poll = time.Second
	}
	if poll < 250*time.Millisecond || poll > time.Minute {
		return ErrInvalid
	}
	heartbeat := r.readinessHeartbeatInterval
	if heartbeat == 0 {
		heartbeat = DesiredStateHeartbeatInterval
	}
	if heartbeat < 5*time.Millisecond || heartbeat >= DesiredStateReadinessLease/2 {
		return ErrInvalid
	}
	return nil
}

func (r *ProductionDesiredStateRuntime) heartbeatInterval() time.Duration {
	if r.readinessHeartbeatInterval != 0 {
		return r.readinessHeartbeatInterval
	}
	return DesiredStateHeartbeatInterval
}

func (r *ProductionDesiredStateRuntime) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *ProductionDesiredStateRuntime) reportPrerequisiteError(err error) {
	if r != nil && err != nil && r.ReportPrerequisiteError != nil {
		r.ReportPrerequisiteError(err)
	}
}

func (r *ProductionDesiredStateRuntime) prerequisiteWait(err error, fallback time.Duration) time.Duration {
	if r == nil {
		return fallback
	}
	var apiErr *githubapp.APIError
	if !errors.As(err, &apiErr) || !apiErr.Retryable() || apiErr.RetryAt.IsZero() {
		return fallback
	}
	wait := apiErr.RetryAt.Sub(r.now())
	if wait > fallback {
		return wait
	}
	return fallback
}

func (r *ProductionDesiredStateRuntime) Run(ctx context.Context) error {
	if r.validate() != nil {
		return ErrInvalid
	}
	pollDuration := r.PollInterval
	if pollDuration == 0 {
		pollDuration = time.Second
	}
	for {
		err := r.runReadyCycle(ctx)
		if !errors.Is(err, ErrArgoRuntimePrerequisiteNotReady) && !errors.Is(err, ErrLeaseLost) {
			return err
		}
		// A protected Git write can make the root Application transiently
		// OutOfSync or supersede a fenced command/readiness lease. Let the old
		// lease age/fence immediately, then acquire a fresh composite proof
		// instead of killing every runtime in the worker process.
		timer := time.NewTimer(r.prerequisiteWait(err, pollDuration))
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (r *ProductionDesiredStateRuntime) runReadyCycle(ctx context.Context) error {
	if r.validate() != nil {
		return ErrInvalid
	}
	started := r.Worker.Observation.StartedAt.UTC()
	now := r.now()
	if started.IsZero() || started.After(now) {
		return ErrInvalid
	}
	pollDuration := r.PollInterval
	if pollDuration == 0 {
		pollDuration = time.Second
	}
	var proof ProductionPrerequisiteProof
	var err error
	for {
		now = r.now()
		proof, err = r.Prerequisites.ObserveProductionPrerequisites(ctx, now)
		if err == nil {
			err = proof.validate(r.Worker.Observation.DesiredStateRuntimeIdentity, now)
		}
		if err == nil {
			break
		}
		r.reportPrerequisiteError(err)
		timer := time.NewTimer(r.prerequisiteWait(err, pollDuration))
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
	adoptedRootUID, adoptedRootSpecDigest := proof.RootUID, proof.RootSpecDigest
	observation := r.Worker.Observation
	observation.ObservedAt = now
	readiness, err := r.Worker.Store.AcquireDesiredStateReadiness(ctx, observation, DesiredStateReadinessLease)
	if err != nil {
		return err
	}
	runContext, cancel := context.WithCancel(ctx)
	heartbeatErrors := make(chan error, 1)
	heartbeatDone := make(chan struct{})
	go func(current DesiredStateRuntimeLease) {
		defer close(heartbeatDone)
		ticker := time.NewTicker(r.heartbeatInterval())
		defer ticker.Stop()
		for {
			select {
			case <-runContext.Done():
				return
			case <-ticker.C:
				observedAt := r.now()
				proof, prerequisiteErr := r.Prerequisites.ObserveProductionPrerequisites(runContext, observedAt)
				if prerequisiteErr == nil {
					prerequisiteErr = proof.validate(r.Worker.Observation.DesiredStateRuntimeIdentity, observedAt)
				}
				if prerequisiteErr == nil && (proof.RootUID != adoptedRootUID || proof.RootSpecDigest != adoptedRootSpecDigest) {
					prerequisiteErr = ErrPlatformRootNotReady
				}
				if prerequisiteErr != nil {
					r.reportPrerequisiteError(prerequisiteErr)
					select {
					case heartbeatErrors <- errors.Join(ErrArgoRuntimePrerequisiteNotReady, prerequisiteErr):
					default:
					}
					// A protected write intentionally makes the root Application
					// transiently unready while it advances the exact desired-state
					// revision. Stop extending API readiness, but let the separately
					// lease-fenced command finish. Cancelling it here strands the
					// command lease until expiry and adds a full reclaim delay.
					return
				}
				updated, heartbeatErr := r.Worker.Store.HeartbeatDesiredStateReadiness(runContext, current, observedAt, DesiredStateReadinessLease)
				if heartbeatErr != nil {
					select {
					case heartbeatErrors <- heartbeatErr:
					default:
					}
					cancel()
					return
				}
				current = updated
			}
		}
	}(readiness)
	defer func() {
		cancel()
		<-heartbeatDone
	}()

	ticker := time.NewTicker(pollDuration)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err = <-heartbeatErrors:
			return err
		case <-ticker.C:
			if _, err = r.Materializer.MaterializeDesiredStateOnce(runContext, r.now()); err != nil {
				return err
			}
			if _, err = r.Worker.ProcessOne(runContext); err != nil {
				select {
				case heartbeatErr := <-heartbeatErrors:
					return heartbeatErr
				default:
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return err
			}
		}
	}
}
