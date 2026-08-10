package builds

import (
	"context"
	"errors"
	"sync"
	"time"
)

type pendingDeliveryStore interface {
	PendingDeliveries(context.Context, time.Time, int) ([]string, error)
}

type deliveryResumer interface {
	ResumeDelivery(context.Context, string) (WebhookOutcome, error)
}

type buildReconciler interface {
	ReconcileNext(context.Context) (ReconcileResult, error)
}

type releaseProjectionReconciler interface {
	ReconcileNext(context.Context) (ReleaseProjectionResult, error)
}

// WorkerRunner keeps authenticated-receipt recovery and build reconciliation
// independent from each other and from the existing GitOps worker loop. Each
// loop owns its own child context and exponential error backoff.
type WorkerRunner struct {
	Store             pendingDeliveryStore
	Deliveries        deliveryResumer
	Builds            buildReconciler
	Releases          releaseProjectionReconciler
	DeliveryOwner     string
	BuildOwner        string
	ReleaseOwner      string
	DeliveryBatch     int
	IdleDelay         time.Duration
	MinimumErrorDelay time.Duration
	MaximumErrorDelay time.Duration
	Now               func() time.Time
	ReportError       func(string, error)
}

func (r *WorkerRunner) Run(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalid
	}
	if err := r.validate(); err != nil {
		return err
	}
	deliveryContext, cancelDeliveries := context.WithCancel(ctx)
	buildContext, cancelBuilds := context.WithCancel(ctx)
	releaseContext, cancelReleases := context.WithCancel(ctx)
	defer cancelDeliveries()
	defer cancelBuilds()
	defer cancelReleases()

	var workers sync.WaitGroup
	workers.Add(3)
	go func() {
		defer workers.Done()
		r.runDeliveryLoop(deliveryContext)
	}()
	go func() {
		defer workers.Done()
		r.runBuildLoop(buildContext)
	}()
	go func() {
		defer workers.Done()
		r.runReleaseLoop(releaseContext)
	}()
	<-ctx.Done()
	cancelDeliveries()
	cancelBuilds()
	cancelReleases()
	workers.Wait()
	return nil
}

// ValidateRuntime proves that all independently required worker loops and
// their distinct lease identities are configured before readiness is emitted.
func (r *WorkerRunner) ValidateRuntime() error { return r.validate() }

func (r *WorkerRunner) validate() error {
	if r == nil || r.Store == nil || r.Deliveries == nil || r.Builds == nil || r.Releases == nil ||
		r.DeliveryOwner == r.BuildOwner || r.DeliveryOwner == r.ReleaseOwner || r.BuildOwner == r.ReleaseOwner ||
		!validOwnerLease(r.DeliveryOwner, 5*time.Second) || !validOwnerLease(r.BuildOwner, 5*time.Second) || !validOwnerLease(r.ReleaseOwner, 5*time.Second) ||
		r.DeliveryBatch < 1 || r.DeliveryBatch > 1000 || r.IdleDelay < 10*time.Millisecond || r.IdleDelay > time.Minute ||
		r.MinimumErrorDelay < 10*time.Millisecond || r.MaximumErrorDelay < r.MinimumErrorDelay || r.MaximumErrorDelay > 5*time.Minute {
		return ErrInvalid
	}
	return nil
}

func (r *WorkerRunner) runReleaseLoop(ctx context.Context) {
	backoff := r.MinimumErrorDelay
	for {
		worked, err := r.reconcileReleaseOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		delay := r.IdleDelay
		if err != nil {
			r.report("build-release-project", err)
			delay = backoff
			backoff = nextErrorBackoff(backoff, r.MaximumErrorDelay)
		} else {
			backoff = r.MinimumErrorDelay
			if worked {
				delay = 0
			}
		}
		if !waitWorkerLoop(ctx, delay) {
			return
		}
	}
}

func (r *WorkerRunner) runDeliveryLoop(ctx context.Context) {
	backoff := r.MinimumErrorDelay
	for {
		worked, err := r.resumePendingOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		delay := r.IdleDelay
		if err != nil {
			r.report("github-delivery-resume", err)
			delay = backoff
			backoff = nextErrorBackoff(backoff, r.MaximumErrorDelay)
		} else {
			backoff = r.MinimumErrorDelay
			if worked {
				delay = 0
			}
		}
		if !waitWorkerLoop(ctx, delay) {
			return
		}
	}
}

func (r *WorkerRunner) runBuildLoop(ctx context.Context) {
	backoff := r.MinimumErrorDelay
	for {
		worked, err := r.reconcileBuildOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		delay := r.IdleDelay
		if err != nil {
			r.report("source-build-reconcile", err)
			delay = backoff
			backoff = nextErrorBackoff(backoff, r.MaximumErrorDelay)
		} else {
			backoff = r.MinimumErrorDelay
			if worked {
				delay = 0
			}
		}
		if !waitWorkerLoop(ctx, delay) {
			return
		}
	}
}

func (r *WorkerRunner) resumePendingOnce(ctx context.Context) (bool, error) {
	pending, err := r.Store.PendingDeliveries(ctx, r.now(), r.DeliveryBatch)
	if err != nil {
		return false, err
	}
	var firstError error
	for _, claimKey := range pending {
		if _, resumeErr := r.Deliveries.ResumeDelivery(ctx, claimKey); resumeErr != nil && ctx.Err() == nil && firstError == nil {
			firstError = resumeErr
		}
	}
	return len(pending) > 0, firstError
}

func (r *WorkerRunner) reconcileBuildOnce(ctx context.Context) (bool, error) {
	_, err := r.Builds.ReconcileNext(ctx)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (r *WorkerRunner) reconcileReleaseOnce(ctx context.Context) (bool, error) {
	_, err := r.Releases.ReconcileNext(ctx)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (r *WorkerRunner) now() time.Time {
	if r.Now == nil {
		return time.Now().UTC()
	}
	return r.Now().UTC()
}

func (r *WorkerRunner) report(loop string, err error) {
	if r.ReportError != nil {
		r.ReportError(loop, err)
	}
}

func nextErrorBackoff(current, maximum time.Duration) time.Duration {
	if current <= 0 || current >= maximum {
		return maximum
	}
	next := current * 2
	if next < current || next > maximum {
		return maximum
	}
	return next
}

func waitWorkerLoop(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return false
		default:
			return true
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
