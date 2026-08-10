package certificates

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/kuberploy/kuberploy/internal/secrets"
)

// StrictCertificateObserver is the read-only subset of the strict
// SealedSecrets provider. It cannot stage/delete resources or read the target
// Kubernetes Secret and its plaintext data.
type StrictCertificateObserver interface {
	ObserveStrictSealedSecret(context.Context, secrets.Artifact) (secrets.ReadinessObservation, error)
}

type ObservationController struct {
	Store       ObservationStore
	Observer    StrictCertificateObserver
	Config      ObservationConfig
	Identity    ObservationIdentity
	WorkerID    string
	Now         func() time.Time
	ReportError func(string, error)
}

func (c *ObservationController) ValidateRuntime() error {
	if c == nil || c.Store == nil || c.Observer == nil || c.Config.Validate() != nil || c.Identity.Validate() != nil ||
		!observationWorkerIDRE.MatchString(c.WorkerID) {
		return ErrObservationUnavailable
	}
	expected, err := ObservationIdentityForConfig(c.Config)
	if err != nil || !observationIdentityEqual(expected, c.Identity) {
		return ErrObservationUnavailable
	}
	return nil
}

func (c *ObservationController) Run(ctx context.Context) error {
	if err := c.ValidateRuntime(); err != nil {
		return err
	}
	startedAt := c.now()
	readiness, err := c.Store.AcquireCertificateObservationReadiness(ctx, ObservationWorkerObservation{
		WorkerID: c.WorkerID, Identity: c.Identity, StartedAt: startedAt, ObservedAt: startedAt,
	}, CertificateObservationReadinessLease)
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
	reconciliationError := c.runReconciliation(runCtx)
	cancel()
	readyError := <-readinessError
	if readyError != nil && !errors.Is(readyError, context.Canceled) {
		return readyError
	}
	if reconciliationError != nil && !errors.Is(reconciliationError, context.Canceled) {
		return reconciliationError
	}
	return ctx.Err()
}

func (c *ObservationController) runReadiness(ctx context.Context, lease ObservationReadinessLease) error {
	ticker := time.NewTicker(c.Config.HeartbeatInterval)
	defer ticker.Stop()
	current := lease
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			updated, err := c.Store.HeartbeatCertificateObservationReadiness(ctx, current, c.now(), CertificateObservationReadinessLease)
			if err != nil {
				return err
			}
			current = updated
		}
	}
}

func (c *ObservationController) runReconciliation(ctx context.Context) error {
	for {
		didWork, err := c.Reconcile(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		delay := c.Config.IdleDelay
		if err != nil {
			if c.ReportError != nil {
				c.ReportError("certificate-observation-reconcile", err)
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

func (c *ObservationController) Reconcile(ctx context.Context) (bool, error) {
	if err := c.ValidateRuntime(); err != nil {
		return false, err
	}
	now := c.now()
	work, err := c.Store.ClaimCertificateObservation(ctx, c.Identity, c.WorkerID, c.Config.Namespaces, now, c.Config.WorkLease)
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if work.Validate() != nil || !c.Config.AllowsNamespace(work.Binding.Scope.Namespace) {
		return true, ErrObservationUnavailable
	}
	if now.Before(work.Attestation.NotBefore) {
		return true, c.applyDegraded(ctx, work, work.Lease, ObservationCertificateNotValid, now)
	}
	if !now.Before(work.Attestation.NotAfter) {
		return true, c.applyDegraded(ctx, work, work.Lease, ObservationCertificateExpired, now)
	}

	workContext, heartbeat := c.startWorkHeartbeat(ctx, work.Lease)
	observation, observeErr := c.Observer.ObserveStrictSealedSecret(workContext, *work.SecretVersion.Artifact)
	lease, heartbeatErr := heartbeat.stop()
	if heartbeatErr != nil {
		return true, heartbeatErr
	}
	if err = ctx.Err(); err != nil {
		return true, err
	}
	now = c.now()
	if observeErr != nil {
		if errors.Is(observeErr, secrets.ErrProviderMismatch) {
			return true, c.applyDegraded(ctx, work, lease, ObservationProviderMismatch, now)
		}
		if err = c.applyRequeue(context.WithoutCancel(ctx), work, lease, ObservationProviderUnavailable, now); err != nil {
			return true, err
		}
		return true, ErrObservationUnavailable
	}
	if !validStrictObservation(observation, *work.SecretVersion.Artifact, lease, now) {
		return true, c.applyDegraded(ctx, work, lease, ObservationProviderMismatch, now)
	}
	switch observation.Status {
	case secrets.ReadinessReady:
		return true, c.Store.ApplyCertificateObservationReady(ctx, lease, ObservationReadyOutcome{
			ObservedAt: observation.ObservedAt.UTC(), NextAt: now.Add(c.Config.PollInterval),
		}, now)
	case secrets.ReadinessPending:
		return true, c.applyDegradedAt(ctx, work, lease, ObservationSealedSecretNotReady, observation.ObservedAt, now)
	case secrets.ReadinessFailed:
		return true, c.applyDegradedAt(ctx, work, lease, ObservationSealedSecretSyncFailed, observation.ObservedAt, now)
	default:
		return true, c.applyDegraded(ctx, work, lease, ObservationProviderMismatch, now)
	}
}

func validStrictObservation(observation secrets.ReadinessObservation, expected secrets.Artifact, lease ObservationLease, now time.Time) bool {
	if observation.Artifact != expected || observation.ObservedAt.IsZero() ||
		observation.ObservedAt.Before(lease.ClaimedAt.Add(-CertificateObservationReadinessSkew)) ||
		observation.ObservedAt.After(now.Add(CertificateObservationReadinessSkew)) {
		return false
	}
	switch observation.Status {
	case secrets.ReadinessPending, secrets.ReadinessReady:
		return observation.FailureCode == ""
	case secrets.ReadinessFailed:
		return observationCodeRE.MatchString(observation.FailureCode)
	default:
		return false
	}
}

func (c *ObservationController) applyDegraded(ctx context.Context, work ObservationWork, lease ObservationLease, code ObservationFailureCode, now time.Time) error {
	return c.applyDegradedAt(ctx, work, lease, code, now, now)
}

func (c *ObservationController) applyDegradedAt(ctx context.Context, work ObservationWork, lease ObservationLease, code ObservationFailureCode, observedAt, now time.Time) error {
	delay := certificateObservationBackoff(c.Config.MinimumBackoff, c.Config.MaximumBackoff, work.ConsecutiveFailures+1)
	return c.Store.ApplyCertificateObservationDegraded(context.WithoutCancel(ctx), lease, ObservationDegradedOutcome{
		FailureCode: code, ObservedAt: observedAt.UTC(), NextAt: now.Add(delay),
	}, now)
}

func (c *ObservationController) applyRequeue(ctx context.Context, work ObservationWork, lease ObservationLease, code ObservationFailureCode, now time.Time) error {
	delay := certificateObservationBackoff(c.Config.MinimumBackoff, c.Config.MaximumBackoff, work.ConsecutiveFailures+1)
	return c.Store.RequeueCertificateObservation(ctx, lease, ObservationRequeueOutcome{FailureCode: code, NextAt: now.Add(delay)}, now)
}

func certificateObservationBackoff(minimum, maximum time.Duration, failures int) time.Duration {
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

func (c *ObservationController) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

type observationWorkHeartbeat struct {
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	lease  ObservationLease
	err    error
}

func (c *ObservationController) startWorkHeartbeat(parent context.Context, lease ObservationLease) (context.Context, *observationWorkHeartbeat) {
	ctx, cancel := context.WithCancel(parent)
	heartbeat := &observationWorkHeartbeat{cancel: cancel, done: make(chan struct{}), lease: lease}
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
				updated, err := c.Store.HeartbeatCertificateObservation(ctx, current, c.now(), c.Config.WorkLease)
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

func (h *observationWorkHeartbeat) stop() (ObservationLease, error) {
	h.cancel()
	<-h.done
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lease, h.err
}
