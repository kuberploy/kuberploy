package gitpublication

import (
	"context"
	"errors"
	"time"
)

type Reconciler struct {
	Store        ReconcileStore
	Service      Service
	Batch        int
	PollInterval time.Duration
	ReportError  func(error)
}

func (r *Reconciler) validate() error {
	if r == nil || r.Store == nil || r.Service.Store == nil || r.Service.Provider == nil ||
		r.Batch < 1 || r.Batch > 100 || r.PollInterval < time.Second || r.PollInterval > time.Hour {
		return ErrInvalid
	}
	return nil
}

func (r *Reconciler) RunOnce(ctx context.Context) (int, error) {
	if err := r.validate(); err != nil {
		return 0, err
	}
	publications, err := r.Store.PendingPublications(ctx, r.Batch)
	if err != nil {
		return 0, err
	}
	var observed int
	var observedErrors []error
	for _, publication := range publications {
		if _, err = r.Service.Observe(ctx, publication.OperationID); err != nil {
			if errors.Is(err, ErrConflict) {
				continue
			}
			observedErrors = append(observedErrors, err)
			continue
		}
		observed++
	}
	return observed, errors.Join(observedErrors...)
}

func (r *Reconciler) Run(ctx context.Context) error {
	if err := r.validate(); err != nil {
		return err
	}
	ticker := time.NewTicker(r.PollInterval)
	defer ticker.Stop()
	for {
		if _, err := r.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) && r.ReportError != nil {
			r.ReportError(err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
