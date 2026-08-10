package gitprojection

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/githubapp"
)

const (
	defaultProjectionLease       = 60 * time.Second
	defaultProjectionHeartbeat   = 15 * time.Second
	defaultProjectionWorkTimeout = 10 * time.Minute
	defaultProjectionPoll        = 5 * time.Minute
	defaultProjectionBackoff     = 5 * time.Second
	defaultProjectionMaxBackoff  = 5 * time.Minute
	defaultProjectionIdle        = time.Second
	defaultProjectionCleanup     = 10 * time.Second
)

// ShadowProjector prepares the exact provider-verified commit in a disposable
// worktree and activates only a complete PostgreSQL generation. Git and
// filesystem I/O happen while an expiring reconciliation lease is heartbeated;
// the Store fences activation and failure by owner and epoch.
type ShadowProjector struct {
	Manager     *MirrorManager
	Indexer     Indexer
	OperationID func() (string, error)
}

func (p ShadowProjector) Project(ctx context.Context, lease ReconciliationLease, binding Binding, head VerifiedHead, now time.Time) error {
	if p.Manager == nil || p.Indexer.Store == nil || lease.Validate() != nil || binding.Validate() != nil || head.ValidateFor(binding) != nil || now.IsZero() {
		return ErrInvalid
	}
	operationID, err := newProjectionOperationID(p.OperationID)
	if err != nil {
		return err
	}
	prepared, err := p.Manager.Prepare(ctx, binding, head, operationID)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultProjectionCleanup)
		defer cancel()
		_ = prepared.Close(cleanup)
	}()
	if binding.State == BindingDiverged {
		_, err = p.Indexer.FullReindex(ctx, lease, prepared, now)
		return err
	}
	_, err = p.Indexer.Index(ctx, lease, prepared, now)
	return err
}

// ProjectionProjector is the bounded shadow-index operation. It exists as a
// seam so coordinator lease/backoff behavior can be tested without Git I/O.
type ProjectionProjector interface {
	Project(context.Context, ReconciliationLease, Binding, VerifiedHead, time.Time) error
}

// Coordinator claims durable binding work, verifies the exact remote head and
// refreshes the shadow projection. Multiple worker replicas are safe: every
// completion and generation transition is fenced by the monotonically
// increasing lease epoch.
type Coordinator struct {
	Store             Store
	Provider          HeadVerifier
	Projector         ProjectionProjector
	Owner             string
	LeaseDuration     time.Duration
	HeartbeatInterval time.Duration
	WorkTimeout       time.Duration
	PollInterval      time.Duration
	MinimumBackoff    time.Duration
	MaximumBackoff    time.Duration
	IdleDelay         time.Duration
	JitterFraction    float64
	Random            func() float64
	Now               func() time.Time
	ReportError       func(error)
}

func (c *Coordinator) validate() error {
	if c == nil || c.Store == nil || c.Provider == nil || c.Projector == nil || !ownerRE.MatchString(c.Owner) {
		return ErrInvalid
	}
	lease, heartbeat, work, poll, minimum, maximum, idle := c.durations()
	if !validReconciliationLeaseDuration(lease) || heartbeat < time.Second || heartbeat >= lease/2 || work < lease || work > time.Hour ||
		poll < 15*time.Second || poll > 24*time.Hour || minimum < time.Second || maximum < minimum || maximum > time.Hour ||
		idle < 10*time.Millisecond || idle > time.Minute || c.JitterFraction < 0 || c.JitterFraction > 0.5 {
		return ErrInvalid
	}
	return nil
}

func (c *Coordinator) durations() (lease, heartbeat, work, poll, minimum, maximum, idle time.Duration) {
	lease = c.LeaseDuration
	if lease == 0 {
		lease = defaultProjectionLease
	}
	heartbeat = c.HeartbeatInterval
	if heartbeat == 0 {
		heartbeat = defaultProjectionHeartbeat
	}
	work = c.WorkTimeout
	if work == 0 {
		work = defaultProjectionWorkTimeout
	}
	poll = c.PollInterval
	if poll == 0 {
		poll = defaultProjectionPoll
	}
	minimum = c.MinimumBackoff
	if minimum == 0 {
		minimum = defaultProjectionBackoff
	}
	maximum = c.MaximumBackoff
	if maximum == 0 {
		maximum = defaultProjectionMaxBackoff
	}
	idle = c.IdleDelay
	if idle == 0 {
		idle = defaultProjectionIdle
	}
	return
}

func (c *Coordinator) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

// Run is a durable, independent loop. ErrNotFound is normal idle state;
// provider and Git failures are recorded on the claimed binding and do not
// stop reconciliation of other bindings.
func (c *Coordinator) Run(ctx context.Context) error {
	if err := c.validate(); err != nil {
		return err
	}
	_, _, _, _, minimum, maximum, idle := c.durations()
	delay := minimum
	for {
		worked, err := c.ReconcileNext(ctx)
		if err == nil {
			delay = minimum
			if worked {
				continue
			}
			if err = waitProjection(ctx, idle); err != nil {
				return err
			}
			continue
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil {
			return ctx.Err()
		}
		if c.ReportError != nil {
			c.ReportError(err)
		}
		if err = waitProjection(ctx, c.jitter(delay)); err != nil {
			return err
		}
		delay = min(delay*2, maximum)
	}
}

// ReconcileNext claims and processes at most one binding. A work failure is
// durably scheduled and therefore returns (true,nil); only claim/lease/storage
// failures escape to the process-level loop.
func (c *Coordinator) ReconcileNext(ctx context.Context) (bool, error) {
	if err := c.validate(); err != nil {
		return false, err
	}
	leaseDuration, heartbeatInterval, workTimeout, pollInterval, minimum, maximum, _ := c.durations()
	claimedAt := c.now()
	work, err := c.Store.ClaimReconciliation(ctx, c.Owner, claimedAt, leaseDuration)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if work.Validate() != nil || !work.Lease.Until.Equal(claimedAt.Add(leaseDuration)) {
		if work.Lease.Validate() == nil {
			cleanup, cancel := context.WithTimeout(context.Background(), defaultProjectionCleanup)
			defer cancel()
			_ = c.Store.ReleaseReconciliation(cleanup, work.Lease, claimedAt)
		}
		return true, ErrProviderMismatch
	}

	workCtx, cancelWork := context.WithTimeout(ctx, workTimeout)
	heartbeatDone := make(chan error, 1)
	go c.heartbeat(workCtx, cancelWork, work.Lease, heartbeatInterval, leaseDuration, heartbeatDone)

	lastCommit, failureCode, operationErr := c.reconcile(workCtx, work)
	cancelWork()
	heartbeatErr := <-heartbeatDone
	if heartbeatErr != nil {
		return true, heartbeatErr
	}
	finishedAt := c.now()
	if ctx.Err() != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), defaultProjectionCleanup)
		defer cancel()
		_ = c.Store.ReleaseReconciliation(cleanup, work.Lease, finishedAt)
		return true, ctx.Err()
	}

	outcome := ReconciliationOutcome{LastCommit: lastCommit}
	if operationErr == nil {
		outcome.NextPollAt = finishedAt.Add(c.jitter(pollInterval))
	} else {
		outcome.ConsecutiveFailure = min(work.ConsecutiveFailure+1, 32)
		outcome.FailureCode = failureCode
		outcome.NextPollAt = finishedAt.Add(c.jitter(exponentialProjectionBackoff(minimum, maximum, outcome.ConsecutiveFailure)))
		var providerError *githubapp.APIError
		if errors.As(operationErr, &providerError) && providerError.RetryAt.After(outcome.NextPollAt) {
			retryAt := providerError.RetryAt.UTC()
			if maximum := finishedAt.Add(maximumProjectionPoll); retryAt.After(maximum) {
				retryAt = maximum
			}
			outcome.NextPollAt = retryAt
		}
	}
	finishCtx, cancelFinish := context.WithTimeout(ctx, defaultProjectionCleanup)
	defer cancelFinish()
	if err = c.Store.FinishReconciliation(finishCtx, work.Lease, outcome, finishedAt); err != nil {
		return true, err
	}
	return true, nil
}

func (c *Coordinator) heartbeat(ctx context.Context, cancel context.CancelFunc, lease ReconciliationLease, interval, duration time.Duration, done chan<- error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			done <- nil
			return
		case <-ticker.C:
			if ctx.Err() != nil {
				done <- nil
				return
			}
			var err error
			lease, err = c.Store.HeartbeatReconciliation(ctx, lease, c.now(), duration)
			if err != nil {
				if ctx.Err() != nil {
					done <- nil
					return
				}
				cancel()
				done <- err
				return
			}
		}
	}
}

func (c *Coordinator) reconcile(ctx context.Context, work ReconciliationWork) (string, string, error) {
	head, err := c.Provider.VerifyTargetHead(ctx, work.Binding, ObservationPoll)
	if err != nil {
		if errors.Is(err, ErrMissingRef) {
			now := c.now()
			if stateErr := c.Store.SetBindingState(ctx, work.Binding.ID, work.Binding.TargetHeadRevision, BindingMissingRef, now); stateErr != nil {
				return work.Binding.TargetHeadRevision, projectionFailureCode(stateErr), stateErr
			}
			return work.Binding.TargetHeadRevision, "missing-ref", err
		}
		return work.Binding.TargetHeadRevision, projectionFailureCode(err), err
	}
	if head.ValidateFor(work.Binding) != nil {
		return work.Binding.TargetHeadRevision, "provider-mismatch", ErrProviderMismatch
	}
	binding, _, err := c.Store.RecordVerifiedHead(ctx, head)
	if err != nil {
		return work.Binding.TargetHeadRevision, projectionFailureCode(err), err
	}
	if !work.BindingChanged && binding.State == BindingReady && binding.IndexedRevision == head.Commit {
		return head.Commit, "", nil
	}
	if err = c.Projector.Project(ctx, work.Lease, binding, head, c.now()); err != nil {
		return head.Commit, projectionFailureCode(err), err
	}
	projected, err := c.Store.Binding(ctx, binding.ID)
	if err != nil {
		return head.Commit, projectionFailureCode(err), err
	}
	if projected.State != BindingReady || projected.IndexedRevision != head.Commit || projected.TargetHeadRevision != head.Commit || projected.ProjectionGeneration <= 0 {
		return head.Commit, "projection-incomplete", ErrConflict
	}
	return head.Commit, "", nil
}

func (c *Coordinator) jitter(base time.Duration) time.Duration {
	fraction := c.JitterFraction
	if fraction == 0 {
		fraction = 0.2
	}
	random := 0.5
	if c.Random != nil {
		random = c.Random()
	}
	if random < 0 || random > 1 || random != random {
		random = 0.5
	}
	multiplier := 1 - fraction + 2*fraction*random
	result := time.Duration(float64(base) * multiplier)
	if result < time.Millisecond {
		return time.Millisecond
	}
	return result
}

func exponentialProjectionBackoff(minimum, maximum time.Duration, failures int) time.Duration {
	value := minimum
	for i := 1; i < failures && value < maximum; i++ {
		if value > maximum/2 {
			return maximum
		}
		value *= 2
	}
	return min(value, maximum)
}

func projectionFailureCode(err error) string {
	if err == nil {
		return ""
	}
	var provider *githubapp.APIError
	switch {
	case errors.Is(err, ErrMissingRef):
		return "missing-ref"
	case errors.Is(err, ErrProviderMismatch):
		return "provider-mismatch"
	case errors.Is(err, ErrLeaseLost):
		return "lease-lost"
	case errors.Is(err, ErrDiverged):
		return "diverged"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &provider):
		code := "github-" + strings.ReplaceAll(string(provider.Class), "_", "-")
		if errorRE.MatchString(code) {
			return code
		}
		return "github-failed"
	default:
		return "projection-failed"
	}
}

func newProjectionOperationID(source func() (string, error)) (string, error) {
	if source != nil {
		value, err := source()
		if err != nil || !uuidRE.MatchString(value) {
			return "", ErrInvalid
		}
		return value, nil
	}
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("create projection operation identity: %w", err)
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	encoded := hex.EncodeToString(value[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}

func waitProjection(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var _ ProjectionProjector = ShadowProjector{}
