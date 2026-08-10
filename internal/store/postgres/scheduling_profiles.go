package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/scheduling"
	base "github.com/kuberploy/kuberploy/internal/store"
)

// validateSchedulingRuntimeTx closes the lookup-to-write race for API-owned
// mutations. Direct Git activation independently repeats the same comparison
// in projectionpolicy.
func validateSchedulingRuntimeTx(ctx context.Context, tx pgx.Tx, projectID, environmentID, applicationID string, runtime domain.WorkloadRuntime) error {
	if runtime.SchedulingProfile == nil && !scheduling.HasEffectiveMaterial(runtime) {
		return nil
	}
	var teamID string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(team_id::text,'') FROM projects WHERE id=$1 FOR SHARE`, projectID).Scan(&teamID); err != nil {
		return classify(err)
	}
	matched, err := scheduling.MatchesRuntimeTx(ctx, tx, runtime, scheduling.Target{TeamID: teamID, ProjectID: projectID, EnvironmentID: environmentID}, applicationID)
	if err != nil {
		return err
	}
	if !matched {
		return base.ErrPreconditionFailed
	}
	return nil
}
