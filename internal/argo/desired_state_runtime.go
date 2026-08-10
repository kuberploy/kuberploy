package argo

import (
	"context"
	"errors"
	"time"

	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

type DesiredStateRuntimeWorker struct {
	Store interface {
		DesiredStateStore
		DesiredStateReadinessStore
	}
	Writer        *DesiredStateWriter
	Observation   DesiredStateRuntimeWorkerObservation
	LeaseDuration time.Duration
	PollInterval  time.Duration
	Now           func() time.Time
}

func (w *DesiredStateRuntimeWorker) validate() error {
	if w == nil || w.Store == nil || w.Writer == nil || w.Writer.validate() != nil || w.Observation.Validate() != nil ||
		w.Observation.DesiredStateRuntimeIdentity != w.Writer.Identity {
		return ErrInvalid
	}
	leaseDuration := w.LeaseDuration
	if leaseDuration == 0 {
		leaseDuration = maximumDesiredStateLease
	}
	poll := w.PollInterval
	if poll == 0 {
		poll = time.Second
	}
	if !validDesiredStateLeaseDuration(leaseDuration) || poll < 250*time.Millisecond || poll > time.Minute {
		return ErrInvalid
	}
	return nil
}

func (w *DesiredStateRuntimeWorker) now() time.Time {
	if w.Now != nil {
		return w.Now().UTC()
	}
	return time.Now().UTC()
}

func (w *DesiredStateRuntimeWorker) ProcessOne(ctx context.Context) (bool, error) {
	if w.validate() != nil {
		return false, ErrInvalid
	}
	leaseDuration := w.LeaseDuration
	if leaseDuration == 0 {
		leaseDuration = maximumDesiredStateLease
	}
	work, err := w.Store.ClaimDesiredState(ctx, w.Observation.WorkerID, w.Observation.DesiredStateWorkerIdentity, w.now(), leaseDuration)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, err = w.Writer.CommitClaim(ctx, work.Lease)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, ErrLeaseLost) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true, err
	}
	current, readErr := w.Store.DesiredStateCommand(ctx, work.Command.ID)
	if readErr != nil {
		return true, errors.Join(err, readErr)
	}
	now := w.now()
	if current.Lease == nil || !sameDesiredStateLeaseFence(*current.Lease, work.Lease) || !current.Lease.Until.After(now) {
		return true, errors.Join(err, ErrLeaseLost)
	}
	// CommitClaim heartbeats while provider and Git I/O are in flight. Its
	// latest lease deadline is authoritative even though the claim returned to
	// this method still contains the original deadline.
	lease := *current.Lease
	failureCode := desiredStateFailureCode(err)
	if current.State == DesiredStateClaimed && IsPermanentDesiredStateError(err) {
		_, finishErr := w.Store.FailDesiredState(ctx, lease, failureCode, now)
		if finishErr != nil {
			return true, errors.Join(err, finishErr)
		}
		return true, nil
	}
	next := now.Add(desiredStateBackoff(current.ConsecutiveFailures))
	_, retryErr := w.Store.RetryDesiredState(ctx, lease, DesiredStateRetry{FailureCode: failureCode, NextAttemptAt: next}, now)
	if retryErr != nil {
		return true, errors.Join(err, retryErr)
	}
	// The command-level error is durably represented by the retry/failure
	// receipt. It must not make the whole worker falsely unhealthy.
	return true, nil
}

func (w *DesiredStateRuntimeWorker) Run(ctx context.Context) error {
	if w.validate() != nil {
		return ErrInvalid
	}
	now := w.now()
	observation := w.Observation
	observation.ObservedAt = now
	if observation.StartedAt.After(now) {
		return ErrInvalid
	}
	readiness, err := w.Store.AcquireDesiredStateReadiness(ctx, observation, DesiredStateReadinessLease)
	if err != nil {
		return err
	}
	runContext, cancel := context.WithCancel(ctx)
	heartbeatErrors := make(chan error, 1)
	heartbeatDone := make(chan struct{})
	go func(current DesiredStateRuntimeLease) {
		defer close(heartbeatDone)
		ticker := time.NewTicker(DesiredStateHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-runContext.Done():
				return
			case <-ticker.C:
				updated, heartbeatErr := w.Store.HeartbeatDesiredStateReadiness(runContext, current, w.now(), DesiredStateReadinessLease)
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
	pollDuration := w.PollInterval
	if pollDuration == 0 {
		pollDuration = time.Second
	}
	poll := time.NewTicker(pollDuration)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err = <-heartbeatErrors:
			return err
		case <-poll.C:
			if _, err = w.ProcessOne(runContext); err != nil {
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

func sameDesiredStateLeaseFence(left, right DesiredStateLease) bool {
	return left.CommandID == right.CommandID && left.Owner == right.Owner && left.Epoch == right.Epoch &&
		left.Contract == right.Contract && left.ConfigDigest == right.ConfigDigest
}

func desiredStateFailureCode(err error) string {
	switch {
	case errors.Is(err, gitprojection.ErrProviderMismatch):
		return "provider-head-mismatch"
	case errors.Is(err, gitprojection.ErrMissingRef):
		return "missing-ref"
	case errors.Is(err, gitprojection.ErrDiverged):
		return "binding-diverged"
	case errors.Is(err, ErrConflict), errors.Is(err, gitprojection.ErrConflict):
		return "stale-git-base"
	case errors.Is(err, ErrInvalid), errors.Is(err, gitprojection.ErrInvalid):
		return "invalid-command"
	default:
		return "git-write-transient"
	}
}

func desiredStateBackoff(failures int) time.Duration {
	if failures < 0 {
		failures = 0
	}
	if failures > 8 {
		failures = 8
	}
	return time.Second * time.Duration(1<<failures)
}
