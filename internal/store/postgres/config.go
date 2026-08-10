package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/id"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func (s *Store) GetDeploymentConfigForActor(ctx context.Context, actor, deploymentID string) (domain.DeploymentConfig, error) {
	if err := authorizeWith(ctx, s.pool, actor, domain.PermissionConfigRead, domain.AccessTarget{Type: "deployment", ID: deploymentID}); err != nil {
		return domain.DeploymentConfig{}, err
	}
	var raw []byte
	var etag string
	var version int64
	var updated time.Time
	err := s.pool.QueryRow(ctx, `SELECT config_raw,config_etag,config_version,updated_at FROM deployments WHERE id=$1`, deploymentID).Scan(&raw, &etag, &version, &updated)
	if err != nil {
		return domain.DeploymentConfig{}, classify(err)
	}
	if len(raw) == 0 {
		return domain.DeploymentConfig{}, base.ErrConfigProjectionMissing
	}
	return domain.DeploymentConfig{DeploymentID: deploymentID, RawYAML: raw, ETag: etag, Version: version, UpdatedAt: updated}, nil
}

func (s *Store) CreateDeploymentConfigPreview(ctx context.Context, actor string, in domain.CreateConfigPreview, projection *gitprojection.WritePlan, references ...*base.AppConfigReferencePlan) error {
	referencePlan, err := base.NormalizeAppConfigReferencePlan(projection, references)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = authorizeWith(ctx, tx, actor, domain.PermissionConfigWrite, domain.AccessTarget{Type: "deployment", ID: in.DeploymentID}); err != nil {
		return err
	}
	var currentETag, projectID, environmentID, applicationID string
	err = tx.QueryRow(ctx, `SELECT d.config_etag,e.project_id,d.environment_id::text,d.application_id::text
		FROM deployments d JOIN environments e ON e.id=d.environment_id
		WHERE d.id=$1 FOR SHARE OF d`, in.DeploymentID).Scan(&currentETag, &projectID, &environmentID, &applicationID)
	if err != nil {
		return classify(err)
	}
	var projectionBinding gitprojection.Binding
	if projection == nil {
		if currentETag == "" || currentETag != in.BaseETag {
			return base.ErrPreconditionFailed
		}
	} else {
		if projection.EnvironmentID != environmentID || projection.ApplicationID != applicationID || projection.ProjectID != projectID ||
			projection.Precondition != gitprojection.MutationMatchETag || projection.ExpectedETag != in.BaseETag {
			return base.ErrPreconditionFailed
		}
		if projectionBinding, err = validateGitProjectionPlanTx(ctx, tx, projection); err != nil {
			return err
		}
	}
	var bindingID, baseRevision, gitPath, gitETag, chartDigest, policyVersion any
	if projection != nil {
		bindingID, baseRevision, gitETag, chartDigest, policyVersion = projection.BindingID, projection.BaseRevision, projection.ExpectedETag, projection.ChartDigest, projection.PolicyVersion
		gitPath, _ = gitprojection.ApplicationPath(projectionBinding, projection.ApplicationID)
	}
	if referencePlan != nil {
		candidateRuntime, candidateErr := runtimeFromExactAppConfig(in.CandidateRaw, in.CandidateHash)
		if candidateErr != nil {
			return candidateErr
		}
		if projection != nil {
			resolution, resolutionErr := resolveProjectedVariablesTx(ctx, tx, projectionBinding, candidateRuntime)
			if resolutionErr != nil {
				return resolutionErr
			}
			candidateRuntime = resolution.Runtime
		}
		if _, candidateErr = validateRuntimeSecretReferencesTx(ctx, tx, actor, referencePlan, projectID, environmentID, applicationID, candidateRuntime); candidateErr != nil {
			return candidateErr
		}
		if candidateErr = validateSchedulingRuntimeTx(ctx, tx, projectID, environmentID, applicationID, candidateRuntime); candidateErr != nil {
			return candidateErr
		}
	} else if len(in.CandidateRaw) != 0 {
		candidateRuntime, candidateErr := runtimeFromExactAppConfig(in.CandidateRaw, in.CandidateHash)
		if candidateErr != nil {
			return candidateErr
		}
		if base.AppConfigUsesRuntimeSecrets(candidateRuntime) {
			return base.ErrPreconditionFailed
		}
		if projection != nil {
			resolution, resolutionErr := resolveProjectedVariablesTx(ctx, tx, projectionBinding, candidateRuntime)
			if resolutionErr != nil {
				return resolutionErr
			}
			candidateRuntime = resolution.Runtime
		}
		if candidateErr = validateSchedulingRuntimeTx(ctx, tx, projectID, environmentID, applicationID, candidateRuntime); candidateErr != nil {
			return candidateErr
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO deployment_config_previews(id,actor_id,deployment_id,token_hash,base_etag,candidate_hash,expires_at,
		git_binding_id,git_base_revision,git_path,git_expected_etag,git_chart_digest,git_policy_version)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, id.New(), actor, in.DeploymentID, in.TokenHash, in.BaseETag, in.CandidateHash, in.ExpiresAt,
		bindingID, baseRevision, gitPath, gitETag, chartDigest, policyVersion)
	if err != nil {
		return classify(err)
	}
	return tx.Commit(ctx)
}

func (s *Store) SaveDeploymentConfig(ctx context.Context, actor, key, fingerprint, requestID string, in domain.SaveDeploymentConfig, projection *gitprojection.WritePlan, references ...*base.AppConfigReferencePlan) (base.Result[domain.Deployment], domain.Operation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, advisoryIdentity(actor, "deployments.config.save", key)); err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	if old, ok, findErr := findIdem(ctx, tx, actor, "deployments.config.save", key); findErr != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, findErr
	} else if ok {
		d, getErr := getDeployment(ctx, tx, old.resourceID)
		if getErr != nil {
			return base.Result[domain.Deployment]{}, domain.Operation{}, getErr
		}
		if getErr = authorizeWith(ctx, tx, actor, domain.PermissionConfigWrite, domain.AccessTarget{Type: "deployment", ID: d.ID}); getErr != nil {
			return base.Result[domain.Deployment]{}, domain.Operation{}, getErr
		}
		if old.fingerprint != fingerprint {
			return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrIdempotencyConflict
		}
		opID := d.OperationID
		if old.operationID != nil {
			opID = *old.operationID
		}
		op, getErr := getOperation(ctx, tx, opID)
		return base.Result[domain.Deployment]{Value: d, Replay: true}, op, getErr
	}
	referencePlan, err := base.NormalizeAppConfigReferencePlan(projection, references)
	if err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	if err = authorizeWith(ctx, tx, actor, domain.PermissionConfigWrite, domain.AccessTarget{Type: "deployment", ID: in.DeploymentID}); err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	var currentETag, projectID, environmentID, applicationID string
	var version, generation int64
	err = tx.QueryRow(ctx, `SELECT d.config_etag,d.config_version,d.generation,e.project_id,d.environment_id::text,d.application_id::text
		FROM deployments d JOIN environments e ON e.id=d.environment_id
		WHERE d.id=$1 FOR UPDATE OF d`, in.DeploymentID).Scan(&currentETag, &version, &generation, &projectID, &environmentID, &applicationID)
	if err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, classify(err)
	}
	var projectionBinding gitprojection.Binding
	if projection == nil {
		if currentETag == "" || currentETag != in.BaseETag {
			return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrPreconditionFailed
		}
	} else {
		if projection.ProjectID != projectID || projection.EnvironmentID != environmentID || projection.ApplicationID != applicationID ||
			projection.Precondition != gitprojection.MutationMatchETag || projection.ExpectedETag != in.BaseETag {
			return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrPreconditionFailed
		}
		if projectionBinding, err = validateGitProjectionPlanTx(ctx, tx, projection); err != nil {
			return base.Result[domain.Deployment]{}, domain.Operation{}, err
		}
	}
	var previewActor, previewDeployment, previewETag, previewBindingID, previewBaseRevision, previewPath, previewGitETag, previewChartDigest, previewPolicyVersion string
	var previewCandidate []byte
	var expires time.Time
	var consumed *time.Time
	err = tx.QueryRow(ctx, `SELECT actor_id,deployment_id,base_etag,candidate_hash,expires_at,consumed_at,
		COALESCE(git_binding_id::text,''),COALESCE(git_base_revision,''),COALESCE(git_path,''),COALESCE(git_expected_etag,''),
		COALESCE(git_chart_digest,''),COALESCE(git_policy_version,'')
		FROM deployment_config_previews WHERE token_hash=$1 FOR UPDATE`, in.TokenHash).Scan(&previewActor, &previewDeployment, &previewETag, &previewCandidate, &expires, &consumed,
		&previewBindingID, &previewBaseRevision, &previewPath, &previewGitETag, &previewChartDigest, &previewPolicyVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrPreviewInvalid
	}
	if err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	if previewActor != actor || previewDeployment != in.DeploymentID || previewETag != in.BaseETag || !bytes.Equal(previewCandidate, in.CandidateHash) {
		return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrPreviewInvalid
	}
	if projection == nil {
		if previewBindingID != "" {
			return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrPreviewInvalid
		}
	} else {
		expectedPath := "tenants/" + projection.ProjectID + "/environments/" + projection.EnvironmentID + "/apps/" + projection.ApplicationID + "/app.yaml"
		if previewBindingID != projection.BindingID || previewBaseRevision != projection.BaseRevision || previewPath != expectedPath ||
			previewGitETag != projection.ExpectedETag || previewChartDigest != projection.ChartDigest || previewPolicyVersion != projection.PolicyVersion {
			return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrPreviewInvalid
		}
	}
	if consumed != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrPreviewConsumed
	}
	now := time.Now().UTC()
	if !expires.After(now) {
		return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrPreviewExpired
	}
	candidateRuntime, err := runtimeFromExactAppConfig(in.RawYAML, in.CandidateHash)
	if err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	if base.AppConfigUsesRuntimeSecrets(candidateRuntime) && referencePlan == nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrPreconditionFailed
	}
	if projection != nil {
		resolution, resolutionErr := resolveProjectedVariablesTx(ctx, tx, projectionBinding, candidateRuntime)
		if resolutionErr != nil {
			return base.Result[domain.Deployment]{}, domain.Operation{}, resolutionErr
		}
		candidateRuntime = resolution.Runtime
	}
	if err = validateSchedulingRuntimeTx(ctx, tx, projectID, environmentID, applicationID, candidateRuntime); err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	if projection != nil {
		if err = replaceRuntimeSecretReferencesTx(ctx, tx, actor, referencePlan, projectionBinding, projectID, environmentID,
			applicationID, candidateRuntime, in.RawYAML, requestID, now); err != nil {
			return base.Result[domain.Deployment]{}, domain.Operation{}, err
		}
	}
	d, err := getDeployment(ctx, tx, in.DeploymentID)
	if err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE operations SET status='superseded',updated_at=$2,finished_at=$2,problem=jsonb_build_object('code','Superseded','detail','A newer configuration was accepted.') WHERE target_type='deployment' AND target_id=$1 AND status='queued'`, d.ID, now); err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	generation++
	opID := id.New()
	progress, _ := json.Marshal([]domain.ProgressStep{{Name: "git-write", Status: "pending"}})
	op := domain.Operation{ID: opID, Kind: "deployment.git-write", Status: "queued", TargetType: "deployment", TargetID: d.ID, RequestID: requestID, Generation: generation, Progress: []domain.ProgressStep{{Name: "git-write", Status: "pending"}}, CreatedAt: now, UpdatedAt: now}
	if _, err = tx.Exec(ctx, `INSERT INTO operations(id,kind,status,target_type,target_id,request_id,generation,progress,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$9)`, op.ID, op.Kind, op.Status, op.TargetType, op.TargetID, op.RequestID, op.Generation, progress, now); err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, classify(err)
	}
	d.Runtime = candidateRuntime
	d.Replicas, d.Port, d.Environment = domain.LegacyWorkloadFields(d.Runtime)
	d.ConfigRaw, d.Generation, d.OperationID, d.State, d.UpdatedAt = append([]byte(nil), in.RawYAML...), generation, op.ID, "pending-git", now
	environmentJSON, _ := json.Marshal(d.Environment)
	runtimeJSON, _ := json.Marshal(d.Runtime)
	newVersion := version + 1
	newETag := domain.DeploymentConfigETag(d.ID, newVersion, d.ConfigRaw)
	if _, err = tx.Exec(ctx, `UPDATE deployments SET replicas=$2,port=$3,environment=$4,runtime=$5,state='pending-git',operation_id=$6,generation=$7,config_raw=$8,config_etag=$9,config_version=$10,updated_at=$11 WHERE id=$1`, d.ID, d.Replicas, d.Port, environmentJSON, runtimeJSON, d.OperationID, generation, d.ConfigRaw, newETag, newVersion, now); err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, classify(err)
	}
	if err = insertGitWriteCommandTx(ctx, tx, actor, op.ID, d.ID, projection, d.ConfigRaw, "config("+d.ApplicationID+"): save AppConfig", now); err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	var routeJSON []byte
	if d.Route != nil {
		routeJSON, _ = json.Marshal(d.Route)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO deployment_operation_inputs(operation_id,deployment_id,image,replicas,port,environment,route,runtime,config_raw,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, op.ID, d.ID, d.Image, d.Replicas, d.Port, environmentJSON, routeJSON, runtimeJSON, d.ConfigRaw, now); err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO outbox(operation_id,kind,scope_id,generation,trace_id) VALUES($1,$2,$3,$4,$5)`, op.ID, op.Kind, d.EnvironmentID, op.Generation, requestID); err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE deployment_config_previews SET consumed_at=$2 WHERE token_hash=$1 AND consumed_at IS NULL`, in.TokenHash, now); err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	if err = audit(ctx, tx, actor, "deployment.config.accepted", "deployment", d.ID, requestID, map[string]any{"operationId": op.ID, "baseETag": in.BaseETag, "configVersion": newVersion}); err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	if err = putIdem(ctx, tx, actor, "deployments.config.save", key, fingerprint, "deployment", d.ID, &opID); err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, classify(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	return base.Result[domain.Deployment]{Value: d}, op, nil
}
