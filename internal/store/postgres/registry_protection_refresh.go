package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/registry"
	base "github.com/kuberploy/kuberploy/internal/store"
)

const registryProtectionSourceMaximumAge = 15 * time.Minute

type registryProtectionDeployment struct {
	id            string
	environmentID string
}

// RefreshRegistryProtection derives cleanup roots from the exact current Git
// projection, the fresh exact Argo-observed runtime revision, and nonterminal
// deployment operations. It writes incomplete checkpoints rather than
// guessing when any authority cannot be resolved. forceFresh refreshes an
// unchanged old checkpoint for a new preview; execution uses false so only a
// real authority-content change invalidates its plan token.
func (s *Store) RefreshRegistryProtection(ctx context.Context, targetID, serviceID string, now time.Time, forceFresh bool) error {
	if s == nil || s.pool == nil || targetID == "" || serviceID == "" || now.IsZero() {
		return base.ErrRegistryObservationIncomplete
	}
	now = databaseTime(now)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = registryAdvisoryLock(ctx, tx, targetID, serviceID, "protection-refresh"); err != nil {
		return err
	}
	target, err := registryTarget(ctx, tx, targetID)
	if err != nil {
		return err
	}
	policy, err := serviceRegistryPolicy(ctx, tx, targetID, serviceID)
	if err != nil {
		return err
	}
	if err = registry.ValidatePolicyForTarget(target, policy); err != nil {
		return err
	}
	deployments, err := registryProtectionDeployments(ctx, tx, serviceID)
	if err != nil {
		return err
	}
	gitInputs, gitComplete, err := registryGitProtectionInputs(ctx, tx, serviceID, deployments)
	if err != nil {
		return err
	}
	runtimeInputs, runtimeComplete, err := registryRuntimeProtectionInputs(ctx, tx, serviceID, deployments, now)
	if err != nil {
		return err
	}
	operationInputs, operationsComplete, err := registryOperationProtectionInputs(ctx, tx, serviceID)
	if err != nil {
		return err
	}
	snapshots := []domain.RegistryProtectionSnapshot{
		registry.BuildProtectionSnapshot(target, policy, domain.RegistryAuthorityGitIntent, gitInputs, gitComplete, now),
		registry.BuildProtectionSnapshot(target, policy, domain.RegistryAuthorityRuntime, runtimeInputs, runtimeComplete, now),
		registry.BuildProtectionSnapshot(target, policy, domain.RegistryAuthorityOperations, operationInputs, operationsComplete, now),
	}
	for _, snapshot := range snapshots {
		if err = replaceRegistryProtectionIfChanged(ctx, tx, snapshot, now, forceFresh); err != nil {
			return err
		}
	}
	return classify(tx.Commit(ctx))
}

func registryProtectionDeployments(ctx context.Context, q registryDB, serviceID string) ([]registryProtectionDeployment, error) {
	rows, err := q.Query(ctx, `SELECT id::text,environment_id::text FROM deployments WHERE application_id=$1 ORDER BY id`, serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []registryProtectionDeployment
	for rows.Next() {
		var deployment registryProtectionDeployment
		if err = rows.Scan(&deployment.id, &deployment.environmentID); err != nil {
			return nil, err
		}
		out = append(out, deployment)
	}
	return out, rows.Err()
}

func registryGitProtectionInputs(ctx context.Context, q registryDB, serviceID string, deployments []registryProtectionDeployment) ([]registry.ProtectionInput, bool, error) {
	inputs := make([]registry.ProtectionInput, 0, len(deployments))
	for _, deployment := range deployments {
		rows, err := q.Query(ctx, `SELECT b.state,d.config_revision,
			d.parsed #>> '{spec,delivery,release,repository}',
			d.parsed #>> '{spec,delivery,release,digest}',d.indexed_at
			FROM git_repository_bindings b
			JOIN git_projected_documents d ON d.binding_id=b.id AND d.generation=b.projection_generation
			WHERE b.kind='environment' AND b.environment_id=$1 AND d.application_id=$2 AND d.valid=true
			ORDER BY d.path`, deployment.environmentID, serviceID)
		if err != nil {
			return nil, false, err
		}
		var state, revision, repository, digest string
		var indexedAt time.Time
		count := 0
		for rows.Next() {
			count++
			if err = rows.Scan(&state, &revision, &repository, &digest, &indexedAt); err != nil {
				rows.Close()
				return nil, false, err
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, false, err
		}
		if count != 1 || state != "ready" || revision == "" || repository == "" || digest == "" {
			return nil, false, nil
		}
		inputs = append(inputs, registry.ProtectionInput{ReferenceKey: "deployment/" + deployment.id,
			Image: repository + "@" + digest, SourceRevision: revision, CreatedAt: indexedAt})
	}
	return inputs, true, nil
}

func registryRuntimeProtectionInputs(ctx context.Context, q registryDB, serviceID string, deployments []registryProtectionDeployment, now time.Time) ([]registry.ProtectionInput, bool, error) {
	inputs := make([]registry.ProtectionInput, 0, len(deployments))
	for _, deployment := range deployments {
		rows, err := q.Query(ctx, `SELECT observation.sync_status,observation.health_status,
			observation.observed_revision,observation.observed_at,
			document.parsed #>> '{spec,delivery,release,repository}',
			document.parsed #>> '{spec,delivery,release,digest}',document.indexed_at
			FROM argo_application_observations observation
			JOIN git_repository_bindings binding ON binding.kind='environment'
				AND binding.environment_id=observation.environment_id AND binding.state='ready'
			JOIN LATERAL (
				SELECT projected.parsed,projected.indexed_at
				FROM git_projected_documents projected
				WHERE projected.binding_id=binding.id AND projected.application_id=$2
					AND projected.valid=true AND projected.config_revision=observation.observed_revision
				ORDER BY projected.generation DESC,projected.path LIMIT 1
			) document ON true
			WHERE observation.deployment_id=$1 ORDER BY document.indexed_at DESC`, deployment.id, serviceID)
		if err != nil {
			return nil, false, err
		}
		var syncStatus, healthStatus, revision, repository, digest string
		var observedAt, indexedAt time.Time
		count := 0
		for rows.Next() {
			count++
			if err = rows.Scan(&syncStatus, &healthStatus, &revision, &observedAt, &repository, &digest, &indexedAt); err != nil {
				rows.Close()
				return nil, false, err
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, false, err
		}
		if count != 1 || syncStatus != "synced" || healthStatus != "healthy" || revision == "" || repository == "" || digest == "" ||
			observedAt.Before(now.Add(-registryProtectionSourceMaximumAge)) || observedAt.After(now.Add(time.Minute)) {
			return nil, false, nil
		}
		inputs = append(inputs, registry.ProtectionInput{ReferenceKey: "deployment/" + deployment.id,
			Image: repository + "@" + digest, SourceRevision: revision, CreatedAt: indexedAt})
	}
	return inputs, true, nil
}

func registryOperationProtectionInputs(ctx context.Context, q registryDB, serviceID string) ([]registry.ProtectionInput, bool, error) {
	rows, err := q.Query(ctx, `SELECT operation.id::text,operation.generation,input.image,
		operation.created_at,operation.updated_at
		FROM operations operation
		JOIN deployment_operation_inputs input ON input.operation_id=operation.id
		JOIN deployments deployment ON deployment.id=input.deployment_id
		WHERE deployment.application_id=$1 AND operation.status IN ('queued','running')
		ORDER BY operation.id`, serviceID)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var inputs []registry.ProtectionInput
	for rows.Next() {
		var operationID, image string
		var generation int64
		var createdAt, updatedAt time.Time
		if err = rows.Scan(&operationID, &generation, &image, &createdAt, &updatedAt); err != nil {
			return nil, false, err
		}
		inputs = append(inputs, registry.ProtectionInput{ReferenceKey: "operation/" + operationID, Image: image,
			SourceRevision: strconv.FormatInt(generation, 10) + "/" + updatedAt.UTC().Format(time.RFC3339Nano), CreatedAt: createdAt})
	}
	return inputs, true, rows.Err()
}

func replaceRegistryProtectionIfChanged(ctx context.Context, tx pgx.Tx, snapshot domain.RegistryProtectionSnapshot, now time.Time, forceFresh bool) error {
	var revision string
	var complete bool
	var observedAt time.Time
	err := tx.QueryRow(ctx, `SELECT revision,complete,observed_at FROM registry_authority_observations
		WHERE registry_target_id=$1 AND service_id=$2 AND authority=$3`, snapshot.Observation.RegistryTargetID,
		snapshot.Observation.ServiceID, snapshot.Observation.Authority).Scan(&revision, &complete, &observedAt)
	sameContent := err == nil && complete == snapshot.Observation.Complete &&
		(revision == snapshot.Observation.Revision || strings.HasPrefix(revision, snapshot.Observation.Revision+":"))
	if sameContent && (!forceFresh || !observedAt.Before(now.Add(-10*time.Minute))) {
		return nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return classify(err)
	}
	snapshot.Observation.Revision += ":" + strconv.FormatInt(now.UnixMicro(), 10)
	if err = replaceRegistryProtectionSnapshotTx(ctx, tx, snapshot); err != nil {
		return fmt.Errorf("refresh registry %s protection: %w", snapshot.Observation.Authority, err)
	}
	return nil
}

var _ registry.ProtectionRefresher = (*Store)(nil)
