package registry

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"regexp"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/store"
)

var ErrRegistryRuntimeUnavailable = errors.New("managed registry runtime is unavailable")
var registryRuntimeControllerOwnerRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,109}$`)

type RuntimeController struct {
	Store   store.RegistryRuntimeStore
	Targets interface {
		RegistryTarget(context.Context, string) (domain.RegistryTarget, error)
	}
	Credentials       DistributionCredentialSource
	Transport         http.RoundTripper
	Cleanup           CleanupPlanExecutor
	Config            RuntimeConfig
	Owner             string
	LeaseDuration     time.Duration
	HeartbeatInterval time.Duration
	IdleDelay         time.Duration
	MinimumBackoff    time.Duration
	MaximumBackoff    time.Duration
	JitterFraction    float64
	Now               func() time.Time
	ReportError       func(string, error)
}

func (c *RuntimeController) validate() error {
	if c == nil || c.Store == nil || c.Targets == nil || c.Credentials == nil || c.Cleanup == nil || c.Config.Validate() != nil || !c.Config.Enabled ||
		!registryRuntimeControllerOwnerRE.MatchString(c.Owner) || c.LeaseDuration < 20*time.Second || c.LeaseDuration > time.Hour ||
		c.HeartbeatInterval < time.Second || c.HeartbeatInterval >= c.LeaseDuration/2 || c.IdleDelay < 100*time.Millisecond || c.IdleDelay > time.Minute ||
		c.MinimumBackoff < time.Second || c.MaximumBackoff < c.MinimumBackoff || c.MaximumBackoff > time.Hour || c.JitterFraction < 0 || c.JitterFraction > 0.5 {
		return ErrRegistryRuntimeUnavailable
	}
	return nil
}

// ValidateRuntime proves that both controller loops have their complete local
// observer/executor dependency boundary before a worker may publish readiness.
func (c *RuntimeController) ValidateRuntime() error { return c.validate() }

func (c *RuntimeController) Run(ctx context.Context) error {
	if err := c.validate(); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errorsChannel := make(chan error, 2)
	go func() { errorsChannel <- c.runLoop(runCtx, "observation", c.ReconcileObservation) }()
	go func() { errorsChannel <- c.runLoop(runCtx, "cleanup", c.ReconcileCleanup) }()
	first := <-errorsChannel
	cancel()
	second := <-errorsChannel
	if first != nil && !errors.Is(first, context.Canceled) {
		return first
	}
	if second != nil && !errors.Is(second, context.Canceled) {
		return second
	}
	return ctx.Err()
}

func (c *RuntimeController) runLoop(ctx context.Context, name string, reconcile func(context.Context) (bool, error)) error {
	failures := 0
	for {
		didWork, err := reconcile(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		delay := c.IdleDelay
		if err != nil {
			failures++
			delay = registryRuntimeBackoff(c.MinimumBackoff, c.MaximumBackoff, failures, c.JitterFraction)
			if c.ReportError != nil {
				c.ReportError(name, err)
			}
		} else {
			failures = 0
			if didWork {
				delay = 0
			}
		}
		if delay == 0 {
			continue
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *RuntimeController) ReconcileObservation(ctx context.Context) (bool, error) {
	if err := c.validate(); err != nil {
		return false, err
	}
	now := c.now()
	work, err := c.Store.ClaimRegistryObservation(ctx, c.Config.TargetID, c.Owner+"-observe", now, c.LeaseDuration)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrConflict) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	lease := work.Lease
	fail := func(code string, cause error) (bool, error) {
		failureNow := c.now()
		failures := work.ConsecutiveFailures + 1
		next := failureNow.Add(registryRuntimeBackoff(c.MinimumBackoff, c.MaximumBackoff, failures, c.JitterFraction))
		finishErr := c.Store.FailRegistryObservation(context.WithoutCancel(ctx), lease, store.RegistryObservationOutcome{FailureCode: code, NextAt: next}, failureNow)
		if finishErr != nil {
			return true, finishErr
		}
		return true, cause
	}
	if err = c.validateTarget(work.Target); err != nil {
		return fail("target-mismatch", err)
	}
	observerConfig := DefaultDistributionObserverConfig()
	observerConfig.ExpectedOrigin = c.Config.Endpoint
	observerConfig.AllowPlainHTTP = c.Config.AllowPlainHTTP
	// Keep provider-side work within the atomic publication bound enforced by
	// the durable runtime store. A larger observation can never be committed.
	observerConfig.MaximumRepositories = 128
	observer, err := NewDistributionObserver(work.Target, observerConfig, c.Credentials, c.Transport)
	if err != nil {
		return fail("observer-config", err)
	}
	workCtx, heartbeat := c.startObservationHeartbeat(ctx, lease)
	roots, err := c.Store.RegistryObservationRoots(workCtx, work.Target.ID)
	if err != nil {
		heartbeat.stop()
		return fail("roots", err)
	}
	observedAt := c.now()
	inventory, catalogs, err := observer.Observe(workCtx, roots, lease.Revision, observedAt)
	heartbeatErr := heartbeat.stop()
	if heartbeatErr != nil {
		return true, heartbeatErr
	}
	if err != nil {
		return fail(registryRuntimeObservationFailureCode(err), err)
	}
	publication := store.RegistryObservationPublication{Inventory: inventory, Catalogs: catalogs, ObservedAt: observedAt, NextAt: observedAt.Add(c.Config.ObservationInterval)}
	if err = c.Store.PublishRegistryObservation(ctx, lease, publication); err != nil {
		return true, err
	}
	return true, nil
}

func (c *RuntimeController) ReconcileCleanup(ctx context.Context) (bool, error) {
	if err := c.validate(); err != nil {
		return false, err
	}
	target, err := c.Targets.RegistryTarget(ctx, c.Config.TargetID)
	if err != nil {
		return false, err
	}
	if err = c.validateTarget(target); err != nil {
		return false, err
	}
	planID, err := c.Store.NextAcceptedRegistryCleanup(ctx, c.Config.TargetID, c.now())
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err = c.Cleanup.Execute(ctx, planID, c.Owner+"-cleanup"); err != nil {
		return true, err
	}
	return true, nil
}

func (c *RuntimeController) validateTarget(target domain.RegistryTarget) error {
	return c.Config.ValidateTarget(target)
}

func (c *RuntimeController) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

type registryObservationHeartbeat struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    chan error
}

func (c *RuntimeController) startObservationHeartbeat(parent context.Context, lease store.RegistryObservationLease) (context.Context, *registryObservationHeartbeat) {
	ctx, cancel := context.WithCancel(parent)
	heartbeat := &registryObservationHeartbeat{cancel: cancel, done: make(chan struct{}), err: make(chan error, 1)}
	go func() {
		defer close(heartbeat.done)
		ticker := time.NewTicker(c.HeartbeatInterval)
		defer ticker.Stop()
		current := lease
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				updated, err := c.Store.HeartbeatRegistryObservation(ctx, current, c.now(), c.LeaseDuration)
				if err != nil {
					heartbeat.err <- err
					cancel()
					return
				}
				current = updated
			}
		}
	}()
	return ctx, heartbeat
}

func (h *registryObservationHeartbeat) stop() error {
	h.cancel()
	<-h.done
	select {
	case err := <-h.err:
		return err
	default:
		return nil
	}
}

func registryRuntimeBackoff(minimum, maximum time.Duration, failures int, jitter float64) time.Duration {
	if failures < 1 {
		failures = 1
	}
	delay := minimum
	for index := 1; index < failures && delay < maximum/2; index++ {
		delay *= 2
	}
	if delay > maximum {
		delay = maximum
	}
	if jitter > 0 {
		factor := 1 - jitter + rand.Float64()*(2*jitter)
		delay = time.Duration(float64(delay) * factor)
	}
	return delay
}

func registryRuntimeObservationFailureCode(err error) string {
	var distribution *DistributionError
	if errors.As(err, &distribution) {
		return "distribution-" + string(distribution.Class)
	}
	switch {
	case errors.Is(err, ErrDistributionCredentialUnavailable):
		return "credential-unavailable"
	case errors.Is(err, ErrDistributionScopeMismatch):
		return "scope-mismatch"
	default:
		return "observation-incomplete"
	}
}
