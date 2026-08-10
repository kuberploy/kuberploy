package main

import (
	"context"
	"errors"
	"time"

	"github.com/kuberploy/kuberploy/internal/autodeploy"
)

type autoDeployRuntime struct {
	controller *autodeploy.Controller
	readiness  autodeploy.RuntimeReadinessStore
	identity   autodeploy.RuntimeIdentity
	workerID   string
	now        func() time.Time
}

func (r *autoDeployRuntime) Run(ctx context.Context) error {
	if r == nil || r.controller == nil || r.readiness == nil || r.identity.Validate() != nil || r.workerID == "" || len(r.workerID) > 128 {
		return autodeploy.ErrInvalid
	}
	now := time.Now().UTC()
	if r.now != nil {
		now = r.now().UTC()
	}
	observation := autodeploy.RuntimeObservation{WorkerID: r.workerID, RuntimeIdentity: r.identity, StartedAt: now, ObservedAt: now}
	lease, err := r.readiness.AcquireRuntimeReadiness(ctx, observation, autodeploy.RuntimeReadinessLease)
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	heartbeatErr := make(chan error, 1)
	go func(current autodeploy.RuntimeLease) {
		ticker := time.NewTicker(autodeploy.RuntimeHeartbeatPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				observedAt := time.Now().UTC()
				if r.now != nil {
					observedAt = r.now().UTC()
				}
				updated, heartbeat := r.readiness.HeartbeatRuntimeReadiness(runCtx, current, observedAt, autodeploy.RuntimeReadinessLease)
				if heartbeat != nil {
					select {
					case heartbeatErr <- heartbeat:
					default:
					}
					cancel()
					return
				}
				current = updated
			}
		}
	}(lease)

	for {
		processed, reconcileErr := r.controller.ReconcileNext(runCtx)
		if reconcileErr != nil {
			cancel()
			select {
			case readinessErr := <-heartbeatErr:
				return errors.Join(reconcileErr, readinessErr)
			default:
				return reconcileErr
			}
		}
		if processed {
			continue
		}
		timer := time.NewTimer(autodeploy.RuntimeClaimPollInterval)
		select {
		case readinessErr := <-heartbeatErr:
			if !timer.Stop() {
				<-timer.C
			}
			return readinessErr
		case <-runCtx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			select {
			case readinessErr := <-heartbeatErr:
				return readinessErr
			default:
				return runCtx.Err()
			}
		case <-timer.C:
		}
	}
}
