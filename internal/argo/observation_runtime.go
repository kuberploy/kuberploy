package argo

import (
	"context"
	"errors"
	"regexp"
	"time"
)

const (
	minimumObservationLease = 15 * time.Second
	maximumObservationLease = 5 * time.Minute
	maximumObservationPoll  = 15 * time.Minute
)

var (
	observationOwnerRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$`)
	failureCodeRE      = regexp.MustCompile(`^[a-z][a-z0-9.-]{0,62}$`)
)

type ObservationLease struct {
	Namespace string
	Owner     string
	Epoch     int64
	Until     time.Time
}

func (l ObservationLease) Validate() error {
	if !kubeRE.MatchString(l.Namespace) || !observationOwnerRE.MatchString(l.Owner) || l.Epoch <= 0 || l.Until.IsZero() {
		return ErrInvalid
	}
	return nil
}

type ObservationWork struct {
	Lease               ObservationLease
	ConsecutiveFailures int
}

func (w ObservationWork) Validate() error {
	if w.Lease.Validate() != nil || w.ConsecutiveFailures < 0 || w.ConsecutiveFailures > 32 {
		return ErrInvalid
	}
	return nil
}

type ObservationOutcome struct {
	SnapshotVersion     string
	ConsecutiveFailures int
	FailureCode         string
	NextPollAt          time.Time
}

func (o ObservationOutcome) Validate() error {
	if o.NextPollAt.IsZero() || o.ConsecutiveFailures < 0 || o.ConsecutiveFailures > 32 || len(o.SnapshotVersion) > 128 ||
		stringsContainsControl(o.SnapshotVersion) {
		return ErrInvalid
	}
	if o.ConsecutiveFailures == 0 {
		if o.FailureCode != "" || o.SnapshotVersion == "" {
			return ErrInvalid
		}
		return nil
	}
	if !failureCodeRE.MatchString(o.FailureCode) || o.SnapshotVersion != "" {
		return ErrInvalid
	}
	return nil
}

func stringsContainsControl(value string) bool {
	for _, character := range value {
		if character == 0 || character == '\r' || character == '\n' {
			return true
		}
	}
	return false
}

type ObservationRuntimeStore interface {
	ObservationStore
	ClaimObservation(context.Context, string, string, time.Time, time.Duration) (ObservationWork, error)
	HeartbeatObservation(context.Context, ObservationLease, time.Time, time.Duration) (ObservationLease, error)
	PutObservationFenced(context.Context, ObservationLease, Observation, time.Time) error
	FinishObservation(context.Context, ObservationLease, ObservationOutcome, time.Time) error
}

type ObservationCoordinator struct {
	Store             ObservationRuntimeStore
	Source            KubernetesApplicationSource
	Resolver          ObservationTargetResolver
	Namespace         string
	Owner             string
	LeaseDuration     time.Duration
	HeartbeatInterval time.Duration
	WorkTimeout       time.Duration
	PollInterval      time.Duration
	MinimumBackoff    time.Duration
	MaximumBackoff    time.Duration
	IdleDelay         time.Duration
	Now               func() time.Time
	ReportError       func(error)
}

func (c *ObservationCoordinator) Validate() error {
	if c == nil || c.Store == nil || c.Source == nil || c.Resolver == nil || !kubeRE.MatchString(c.Namespace) || !observationOwnerRE.MatchString(c.Owner) ||
		c.LeaseDuration < minimumObservationLease || c.LeaseDuration > maximumObservationLease || c.HeartbeatInterval <= 0 || c.HeartbeatInterval >= c.LeaseDuration ||
		c.WorkTimeout < c.LeaseDuration || c.WorkTimeout > 30*time.Minute || c.PollInterval < 15*time.Second || c.PollInterval > maximumObservationPoll ||
		c.MinimumBackoff <= 0 || c.MaximumBackoff < c.MinimumBackoff || c.MaximumBackoff > maximumObservationPoll || c.IdleDelay <= 0 || c.IdleDelay > time.Minute {
		return ErrInvalid
	}
	return nil
}

func (c *ObservationCoordinator) Run(ctx context.Context) error {
	if err := c.Validate(); err != nil {
		return err
	}
	for {
		worked, err := c.RunOnce(ctx)
		if err != nil && c.ReportError != nil {
			c.ReportError(err)
		}
		delay := c.IdleDelay
		if worked && err == nil {
			delay = time.Millisecond
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

func (c *ObservationCoordinator) RunOnce(ctx context.Context) (bool, error) {
	if err := c.Validate(); err != nil {
		return false, err
	}
	now := c.now()
	work, err := c.Store.ClaimObservation(ctx, c.Namespace, c.Owner, now, c.LeaseDuration)
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrLeaseHeld) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	workCtx, cancel := context.WithTimeout(ctx, c.WorkTimeout)
	heartbeatDone := make(chan error, 1)
	go func() { heartbeatDone <- c.heartbeat(workCtx, work.Lease) }()

	observer := KubernetesObserver{Source: c.Source, Resolver: c.Resolver,
		Store: fencedObservationSink{store: c.Store, lease: work.Lease, now: c.now}, Namespace: c.Namespace, Now: c.now}
	batch, observeErr := observer.PollOnce(workCtx)
	cancel()
	heartbeatErr := <-heartbeatDone
	finishedAt := c.now()
	if heartbeatErr != nil && !errors.Is(heartbeatErr, context.Canceled) {
		return true, heartbeatErr
	}
	if observeErr != nil {
		failures := work.ConsecutiveFailures + 1
		if failures > 32 {
			failures = 32
		}
		outcome := ObservationOutcome{ConsecutiveFailures: failures, FailureCode: observationFailureCode(observeErr),
			NextPollAt: finishedAt.Add(c.backoff(failures))}
		if finishErr := c.Store.FinishObservation(ctx, work.Lease, outcome, finishedAt); finishErr != nil {
			return true, finishErr
		}
		return true, observeErr
	}
	outcome := ObservationOutcome{SnapshotVersion: batch.SnapshotVersion, NextPollAt: finishedAt.Add(c.PollInterval)}
	if err = c.Store.FinishObservation(ctx, work.Lease, outcome, finishedAt); err != nil {
		return true, err
	}
	return true, nil
}

func (c *ObservationCoordinator) heartbeat(ctx context.Context, lease ObservationLease) error {
	ticker := time.NewTicker(c.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			var err error
			lease, err = c.Store.HeartbeatObservation(ctx, lease, c.now(), c.LeaseDuration)
			if err != nil {
				return err
			}
		}
	}
}

func (c *ObservationCoordinator) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func (c *ObservationCoordinator) backoff(failures int) time.Duration {
	delay := c.MinimumBackoff
	for index := 1; index < failures && delay < c.MaximumBackoff; index++ {
		if delay > c.MaximumBackoff/2 {
			return c.MaximumBackoff
		}
		delay *= 2
	}
	if delay > c.MaximumBackoff {
		return c.MaximumBackoff
	}
	return delay
}

func observationFailureCode(err error) string {
	if errors.Is(err, ErrInvalid) {
		return "observation-invalid"
	}
	if errors.Is(err, ErrLeaseLost) {
		return "observation-lease-lost"
	}
	return "observation-unavailable"
}

type fencedObservationSink struct {
	store ObservationRuntimeStore
	lease ObservationLease
	now   func() time.Time
}

func (s fencedObservationSink) PutObservation(ctx context.Context, value Observation) error {
	return s.store.PutObservationFenced(ctx, s.lease, value, s.now().UTC())
}
