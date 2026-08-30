package environmentfoundation

import (
	"context"
	"errors"
	"sync"
	"time"
)

type Controller struct {
	Store                          Store
	Publisher                      ProtectedPublisher
	Profile                        Profile
	WorkerID                       string
	WorkerEpoch                    int64
	WorkLease                      time.Duration
	MinimumBackoff, MaximumBackoff time.Duration
	Now                            func() time.Time
}

func (c *Controller) Validate() error {
	if c == nil || c.Store == nil || c.Publisher == nil || c.Profile.Validate() != nil || !workerIDRE.MatchString(c.WorkerID) || c.WorkerEpoch < 1 ||
		c.WorkLease < MinimumLease || c.WorkLease > MaximumLease || c.MinimumBackoff < time.Second || c.MaximumBackoff < c.MinimumBackoff || c.MaximumBackoff > 24*time.Hour {
		return ErrUnavailable
	}
	id := c.Publisher.Identity()
	if id.Validate() != nil || id.ConfigDigest != c.Profile.PublisherConfigDigest {
		return ErrUnavailable
	}
	return nil
}

func (c *Controller) Reconcile(ctx context.Context) (bool, error) {
	if c.Validate() != nil {
		return false, ErrUnavailable
	}
	profileDigest, err := c.Profile.Digest()
	if err != nil {
		return false, ErrUnavailable
	}
	publisher := c.Publisher.Identity()
	lease, found, err := c.Store.ClaimIntent(ctx, c.WorkerID, profileDigest, publisher.ConfigDigest, c.now(), c.WorkLease)
	if err != nil || !found {
		return found, err
	}
	request := publicationFor(lease.Intent)
	if request.Validate(lease.Intent, publisher) != nil {
		return c.fail(ctx, lease, "publisher-contract", true)
	}
	workContext, heartbeat := c.startHeartbeat(ctx, lease)
	receipt, publishErr := c.Publisher.Publish(workContext, lease, request)
	latest, heartbeatErr := heartbeat.stop()
	if heartbeatErr != nil {
		return true, heartbeatErr
	}
	current, currentErr := c.Store.Intent(context.WithoutCancel(ctx), latest.Intent.ID)
	if currentErr != nil || current.LeaseOwner != latest.Owner || current.LeaseEpoch != latest.Epoch ||
		current.LeaseUntil == nil || !current.LeaseUntil.Equal(latest.Until) {
		if currentErr != nil {
			return true, currentErr
		}
		return true, ErrLeaseLost
	}
	latest.Intent = current
	if publishErr != nil {
		if errors.Is(publishErr, ErrInvalid) {
			return c.fail(ctx, latest, "publisher-request-invalid", true)
		}
		if errors.Is(publishErr, errRebaseRequired) {
			return c.fail(ctx, latest, "protected-git-rebase", true)
		}
		return c.fail(ctx, latest, "protected-git-unavailable", false)
	}
	now := c.now()
	if receipt.Validate(latest.Intent) != nil || receipt.ObservedAt.After(now) {
		return c.fail(ctx, latest, "publisher-receipt", true)
	}
	_, err = c.Store.RecordReady(context.WithoutCancel(ctx), latest, receipt, now)
	return true, err
}

func (c *Controller) Heartbeat(ctx context.Context, lease Lease) (Lease, error) {
	if c.Validate() != nil {
		return Lease{}, ErrUnavailable
	}
	return c.Store.HeartbeatIntent(ctx, lease, c.now(), c.WorkLease)
}

func (c *Controller) RecordReadiness(ctx context.Context, activeCount int, startedAt, leaseUntil time.Time) error {
	if c.Validate() != nil {
		return ErrUnavailable
	}
	d, err := c.Profile.Digest()
	if err != nil {
		return ErrUnavailable
	}
	now := c.now()
	return c.Store.RecordReadiness(ctx, Readiness{WorkerID: c.WorkerID, WorkerEpoch: c.WorkerEpoch, Contract: Contract, ProfileDigest: d, PublisherConfigDigest: c.Profile.PublisherConfigDigest, ActiveIntentCount: activeCount, StartedAt: startedAt.UTC(), ObservedAt: now, LeaseUntil: leaseUntil.UTC()})
}

func (c *Controller) fail(ctx context.Context, lease Lease, code string, permanent bool) (bool, error) {
	now := c.now()
	failures := lease.Intent.ConsecutiveFailures + 1
	delay := c.MinimumBackoff
	for i := 1; i < failures && delay < c.MaximumBackoff/2; i++ {
		delay *= 2
	}
	if delay > c.MaximumBackoff {
		delay = c.MaximumBackoff
	}
	_, err := c.Store.RecordRetry(context.WithoutCancel(ctx), lease, code, permanent, now.Add(delay), now)
	if err != nil {
		return true, err
	}
	if !permanent {
		return true, ErrUnavailable
	}
	return true, nil
}
func (c *Controller) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

type publicationHeartbeat struct {
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	lease  Lease
	err    error
}

func (c *Controller) startHeartbeat(parent context.Context, lease Lease) (context.Context, *publicationHeartbeat) {
	ctx, cancel := context.WithCancel(parent)
	h := &publicationHeartbeat{cancel: cancel, done: make(chan struct{}), lease: lease}
	interval := c.WorkLease / 3
	go func() {
		defer close(h.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.mu.Lock()
				current := h.lease
				h.mu.Unlock()
				updated, err := c.Store.HeartbeatIntent(ctx, current, c.now(), c.WorkLease)
				if err != nil {
					h.mu.Lock()
					h.err = err
					h.mu.Unlock()
					cancel()
					return
				}
				h.mu.Lock()
				h.lease = updated
				h.mu.Unlock()
			}
		}
	}()
	return ctx, h
}

func (h *publicationHeartbeat) stop() (Lease, error) {
	h.cancel()
	<-h.done
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lease, h.err
}
