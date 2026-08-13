package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/domain"
	base "github.com/kuberploy/kuberploy/internal/store"
)

// validateSchedulingRuntimeTx closes the lookup-to-write race for API-owned
// mutations. Direct Git activation independently repeats the same comparison
// in projectionpolicy.
func validateSchedulingRuntimeTx(_ context.Context, _ pgx.Tx, _, _, applicationID string, runtime domain.WorkloadRuntime) error {
	if len(domain.ValidateWorkloadRuntime(runtime)) != 0 || len(domain.ValidateApplicationScheduling(runtime, applicationID)) != 0 {
		return base.ErrPreconditionFailed
	}
	return nil
}
