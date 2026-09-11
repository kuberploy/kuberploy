package main

import (
	"context"
	"time"

	"github.com/kuberploy/kuberploy/internal/builds"
)

type sourceDeploymentRuntime struct {
	controller *builds.SourceDeploymentController
}

func (r *sourceDeploymentRuntime) Run(ctx context.Context) error {
	for {
		processed, err := r.controller.ReconcileNext(ctx)
		if err != nil {
			return err
		}
		if processed {
			continue
		}
		timer := time.NewTimer(builds.SourceDeploymentClaimPollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}
