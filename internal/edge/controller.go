package edge

import (
	"context"
	"errors"
	"sync"
	"time"
)

const (
	maximumTargetObservationDuration = 10 * time.Minute
	durableStoreTimeout              = 10 * time.Second
)

type RuntimeController struct {
	Store       Store
	Observer    TargetObserver
	Config      RuntimeConfig
	WorkerID    string
	WorkerEpoch int64
	Now         func() time.Time
	ReportError func(string, error)
}

func (c *RuntimeController) Validate() error {
	if c == nil || c.Store == nil || c.Observer == nil || c.Config.Validate() != nil || !c.Config.Enabled ||
		!workerIDPattern.MatchString(c.WorkerID) || c.WorkerEpoch <= 0 {
		return ErrUnavailable
	}
	return nil
}

func (c *RuntimeController) Run(ctx context.Context) error {
	if err := c.Validate(); err != nil {
		return err
	}
	digest, err := c.Config.Digest()
	if err != nil {
		return ErrUnavailable
	}
	targets, err := c.Config.DesiredTargets()
	if err != nil {
		return ErrUnavailable
	}
	startedAt := c.now()
	if err = c.Store.SynchronizeTargets(ctx, digest, targets, startedAt); err != nil {
		return err
	}
	readiness := Readiness{WorkerID: c.WorkerID, WorkerEpoch: c.WorkerEpoch, Contract: RuntimeContract,
		ConfigDigest: digest, TargetCount: len(targets), StartedAt: startedAt, ObservedAt: startedAt,
		LeaseUntil: startedAt.Add(c.Config.ReadinessMaxAge)}
	if err = c.Store.RecordReadiness(ctx, readiness); err != nil {
		return err
	}
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	readinessDone := make(chan error, 1)
	go func() {
		readinessDone <- c.runReadiness(runContext, readiness)
		cancel()
	}()
	reconcileErr := c.runReconciliation(runContext, digest)
	cancel()
	readinessErr := <-readinessDone
	if reconcileErr != nil && !errors.Is(reconcileErr, context.Canceled) {
		return reconcileErr
	}
	if readinessErr != nil && !errors.Is(readinessErr, context.Canceled) {
		return readinessErr
	}
	return ctx.Err()
}

func (c *RuntimeController) runReadiness(ctx context.Context, readiness Readiness) error {
	ticker := time.NewTicker(c.Config.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			now := c.now()
			readiness.ObservedAt, readiness.LeaseUntil = now, now.Add(c.Config.ReadinessMaxAge)
			if err := c.Store.RecordReadiness(ctx, readiness); err != nil {
				return err
			}
		}
	}
}

func (c *RuntimeController) runReconciliation(ctx context.Context, configDigest string) error {
	for {
		worked, err := c.Reconcile(ctx, configDigest)
		if err != nil {
			return err
		}
		if worked {
			continue
		}
		timer := time.NewTimer(c.Config.MinimumBackoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *RuntimeController) Reconcile(ctx context.Context, configDigest string) (bool, error) {
	if c.Validate() != nil || !validDigest(configDigest) {
		return false, ErrUnavailable
	}
	expectedDigest, err := c.Config.Digest()
	if err != nil || expectedDigest != configDigest {
		return false, ErrUnavailable
	}
	lease, found, err := c.Store.ClaimTarget(ctx, c.WorkerID, RuntimeContract, configDigest, c.now(), c.Config.WorkLease)
	if err != nil || !found {
		return found, err
	}
	profile, configured := c.Config.ProfileForTarget(lease.Target.DesiredTarget)
	if !configured {
		return c.permanentFailure(ctx, lease, "profile-mismatch")
	}
	observationContext, cancelObservation := context.WithTimeout(ctx, maximumTargetObservationDuration)
	workContext, heartbeat := c.startHeartbeat(observationContext, lease)
	var receipt ObservationReceipt
	switch profile.Kind {
	case KindTraefik:
		receipt, err = c.Observer.ObserveTraefik(workContext, *profile.Traefik)
	case KindCertManager:
		receipt, err = c.Observer.ObserveCertManager(workContext, *profile.CertManager)
	case KindExternalDNS:
		receipt, err = c.Observer.ObserveExternalDNS(workContext, *profile.ExternalDNS)
	default:
		err = ErrInvalid
	}
	latest, heartbeatErr := heartbeat.stop()
	cancelObservation()
	if heartbeatErr != nil {
		return true, heartbeatErr
	}
	if err != nil {
		code := observationFailureCode(err)
		if errors.Is(err, ErrObservation) || errors.Is(err, ErrNotFound) {
			return c.retryFailure(ctx, latest, code, false, err)
		}
		worked, recordErr := c.retryFailure(ctx, latest, code, false, err)
		if recordErr != nil {
			return worked, recordErr
		}
		return true, ErrUnavailable
	}
	if receipt.Validate(latest.Target.DesiredTarget) != nil {
		return c.permanentFailure(ctx, latest, "observation-receipt-invalid")
	}
	now := c.now()
	if _, err = c.Store.RecordTargetReady(ctx, latest, receipt, now, now.Add(c.Config.PollInterval)); err != nil {
		if errors.Is(err, ErrIdentityChanged) {
			return c.permanentFailure(ctx, latest, "resource-identity-changed")
		}
		return true, err
	}
	return true, nil
}

func (c *RuntimeController) permanentFailure(ctx context.Context, lease Lease, code string) (bool, error) {
	return c.retryFailure(ctx, lease, code, true, ErrInvalid)
}

func (c *RuntimeController) retryFailure(ctx context.Context, lease Lease, code string, permanent bool, reported error) (bool, error) {
	now := c.now()
	failures := min(30, lease.Target.ConsecutiveFailures+1)
	next := now.Add(exponentialBackoff(c.Config.MinimumBackoff, c.Config.MaximumBackoff, failures))
	recordContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), durableStoreTimeout)
	defer cancel()
	_, err := c.Store.RecordTargetRetry(recordContext, lease, code, permanent, next, now)
	if err == nil && c.ReportError != nil {
		c.ReportError(code, reported)
	}
	return true, err
}

func exponentialBackoff(minimum, maximum time.Duration, failures int) time.Duration {
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

type targetHeartbeat struct {
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	lease  Lease
	err    error
}

func (c *RuntimeController) startHeartbeat(parent context.Context, lease Lease) (context.Context, *targetHeartbeat) {
	ctx, cancel := context.WithCancel(parent)
	heartbeat := &targetHeartbeat{cancel: cancel, done: make(chan struct{}), lease: lease}
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
				updated, err := c.Store.HeartbeatTarget(ctx, current, c.now(), c.Config.WorkLease)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
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

func (h *targetHeartbeat) stop() (Lease, error) {
	h.cancel()
	<-h.done
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lease, h.err
}

type RuntimeReadinessProbe struct {
	Store  Store
	Config RuntimeConfig
	Now    func() time.Time
}

func (p *RuntimeReadinessProbe) Probe(ctx context.Context) error {
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
	return p.Store.RuntimeReady(ctx, RuntimeContract, digest, p.Config.TargetCount(), now, p.Config.ReadinessMaxAge)
}
