package secrets

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/kuberploy/kuberploy/internal/id"
)

type StrictSealedSecretsObserver interface {
	ObserveStrictSealedSecret(context.Context, Artifact) (ReadinessObservation, error)
}

type RuntimeController struct {
	Store           RuntimeStore
	Observer        StrictSealedSecretsObserver
	Config          RuntimeConfig
	Identity        RuntimeIdentity
	WorkerID        string
	Now             func() time.Time
	ReportError     func(string, error)
	ResolveIdentity func(context.Context, RuntimeConfig, time.Time) (RuntimeIdentity, error)
}

// ValidateRuntime proves the complete local observer/config/store boundary.
// Callers must additionally obtain Identity through WorkerRuntimeIdentity so
// the public certificate and exact metadata contract are proven without
// exposing the API-only HMAC key before Run publishes readiness.
func (c *RuntimeController) ValidateRuntime() error {
	if c == nil || c.Store == nil || c.Observer == nil || c.Config.Validate() != nil ||
		c.Identity.Validate() != nil || !runtimeSecretWorkerIDRE.MatchString(c.WorkerID) || c.ResolveIdentity == nil {
		return ErrRuntimeUnavailable
	}
	expected, err := RuntimeIdentityForConfig(c.Config, c.Identity.SealingKeyFingerprint)
	if err != nil || !runtimeIdentityEqual(expected, c.Identity) {
		return ErrRuntimeUnavailable
	}
	return nil
}

func (c *RuntimeController) validatePrerequisites(ctx context.Context) error {
	if err := c.ValidateRuntime(); err != nil {
		return err
	}
	identity, err := c.ResolveIdentity(ctx, c.Config, c.now())
	if err != nil || !runtimeIdentityEqual(identity, c.Identity) {
		return ErrRuntimeUnavailable
	}
	return nil
}

func (c *RuntimeController) Run(ctx context.Context) error {
	if err := c.validatePrerequisites(ctx); err != nil {
		return err
	}
	startedAt := c.now()
	readiness, err := c.Store.AcquireRuntimeSecretReadiness(ctx, RuntimeWorkerObservation{
		WorkerID: c.WorkerID, Identity: c.Identity, StartedAt: startedAt, ObservedAt: startedAt,
	}, RuntimeSecretReadinessLease)
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	readinessError := make(chan error, 1)
	go func() {
		readinessError <- c.runReadiness(runCtx, readiness)
		cancel()
	}()
	reconcileError := c.runReconciliation(runCtx)
	cancel()
	readyError := <-readinessError
	if readyError != nil && !errors.Is(readyError, context.Canceled) {
		return readyError
	}
	if reconcileError != nil && !errors.Is(reconcileError, context.Canceled) {
		return reconcileError
	}
	return ctx.Err()
}

func (c *RuntimeController) runReadiness(ctx context.Context, lease RuntimeReadinessLease) error {
	ticker := time.NewTicker(c.Config.HeartbeatInterval)
	defer ticker.Stop()
	current := lease
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			updated, err := c.Store.HeartbeatRuntimeSecretReadiness(ctx, current, c.now(), RuntimeSecretReadinessLease)
			if err != nil {
				return err
			}
			current = updated
		}
	}
}

func (c *RuntimeController) runReconciliation(ctx context.Context) error {
	for {
		didWork, err := c.Reconcile(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		delay := c.Config.IdleDelay
		if err != nil {
			if c.ReportError != nil {
				c.ReportError("runtime-secret-reconcile", err)
			}
			delay = c.Config.MinimumBackoff
		} else if didWork {
			delay = 0
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

func (c *RuntimeController) Reconcile(ctx context.Context) (bool, error) {
	if err := c.validatePrerequisites(ctx); err != nil {
		return false, err
	}
	work, err := c.Store.ClaimRuntimeSecret(ctx, c.Identity, c.WorkerID, c.Config.Namespaces, c.Config.NamespacePrefixes, c.now(), c.Config.WorkLease)
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if work.Validate() != nil || !c.Config.AllowsNamespace(work.Binding.Scope.Namespace) {
		return true, ErrRuntimeUnavailable
	}
	workContext, heartbeat := c.startWorkHeartbeat(ctx, work.Lease)
	observation, observeErr := c.Observer.ObserveStrictSealedSecret(workContext, *work.Version.Artifact)
	lease, heartbeatErr := heartbeat.stop()
	if heartbeatErr != nil {
		return true, heartbeatErr
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}
	if observeErr != nil {
		return c.deferFailure(ctx, work, lease, "provider-observe-failed", ErrProviderOperation)
	}
	if observation.validate(*work.Version.Artifact) != nil {
		return c.deferFailure(ctx, work, lease, "provider-observation-mismatch", ErrProviderMismatch)
	}
	now := c.now()
	requestID := runtimeRequestID(work.Version.ID, lease.Epoch)
	switch observation.Status {
	case ReadinessPending:
		err = c.Store.ApplyRuntimeSecretPending(ctx, lease, RuntimePendingOutcome{NextAt: now.Add(c.Config.PollInterval)}, now)
		return true, err
	case ReadinessReady:
		event := Event{ID: id.New(), BindingID: work.Binding.ID, VersionID: work.Version.ID,
			Kind: EventVersionActive, RequestID: requestID, OccurredAt: now}
		_, _, err = c.Store.ApplyRuntimeSecretReady(ctx, lease, event, now)
		return true, err
	case ReadinessFailed:
		event := Event{ID: id.New(), BindingID: work.Binding.ID, VersionID: work.Version.ID,
			Kind: EventVersionFailed, RequestID: requestID, OccurredAt: now}
		_, err = c.Store.ApplyRuntimeSecretFailed(ctx, lease, observation.FailureCode, event, now)
		return true, err
	default:
		return c.deferFailure(ctx, work, lease, "provider-observation-mismatch", ErrProviderMismatch)
	}
}

func (c *RuntimeController) deferFailure(ctx context.Context, work RuntimeWork, lease RuntimeLease, code string, cause error) (bool, error) {
	now := c.now()
	failures := min(30, work.ConsecutiveFailures+1)
	next := now.Add(runtimeSecretBackoff(c.Config.MinimumBackoff, c.Config.MaximumBackoff, failures))
	err := c.Store.ApplyRuntimeSecretPending(context.WithoutCancel(ctx), lease, RuntimePendingOutcome{FailureCode: code, NextAt: next}, now)
	if err != nil {
		return true, err
	}
	return true, cause
}

func runtimeSecretBackoff(minimum, maximum time.Duration, failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	delay := minimum
	for index := 1; index < failures && delay < maximum/2; index++ {
		delay *= 2
	}
	if delay > maximum {
		return maximum
	}
	return delay
}

func (c *RuntimeController) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func runtimeRequestID(versionID string, epoch int64) string {
	return "runtime-secret:" + versionID + ":" + strconv.FormatInt(epoch, 10)
}

type runtimeWorkHeartbeat struct {
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	lease  RuntimeLease
	err    error
}

func (c *RuntimeController) startWorkHeartbeat(parent context.Context, lease RuntimeLease) (context.Context, *runtimeWorkHeartbeat) {
	ctx, cancel := context.WithCancel(parent)
	heartbeat := &runtimeWorkHeartbeat{cancel: cancel, done: make(chan struct{}), lease: lease}
	go func() {
		defer close(heartbeat.done)
		ticker := time.NewTicker(c.Config.HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				heartbeat.mu.Lock()
				current := heartbeat.lease
				heartbeat.mu.Unlock()
				updated, err := c.Store.HeartbeatRuntimeSecret(ctx, current, c.now(), c.Config.WorkLease)
				if err != nil {
					heartbeat.mu.Lock()
					heartbeat.err = err
					heartbeat.mu.Unlock()
					cancel()
					return
				}
				heartbeat.mu.Lock()
				heartbeat.lease = updated
				heartbeat.mu.Unlock()
			}
		}
	}()
	return ctx, heartbeat
}

func (h *runtimeWorkHeartbeat) stop() (RuntimeLease, error) {
	h.cancel()
	<-h.done
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lease, h.err
}
