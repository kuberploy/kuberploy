package memory

import (
	"bytes"
	"context"
	"encoding/hex"
	"reflect"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/id"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func (s *Store) GetDeploymentConfigForActor(_ context.Context, actor, deploymentID string) (domain.DeploymentConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.deployments[deploymentID]
	if !ok {
		return domain.DeploymentConfig{}, base.ErrNotFound
	}
	if err := s.authorizeLocked(actor, domain.PermissionConfigRead, domain.AccessTarget{Type: "deployment", ID: d.ID}); err != nil {
		return domain.DeploymentConfig{}, err
	}
	if len(d.ConfigRaw) == 0 {
		return domain.DeploymentConfig{}, base.ErrConfigProjectionMissing
	}
	version := d.ConfigVersion
	return domain.DeploymentConfig{DeploymentID: d.ID, RawYAML: append([]byte(nil), d.ConfigRaw...), Version: version, ETag: domain.DeploymentConfigETag(d.ID, version, d.ConfigRaw), UpdatedAt: d.UpdatedAt}, nil
}

func (s *Store) CreateDeploymentConfigPreview(_ context.Context, actor string, in domain.CreateConfigPreview, projection *gitprojection.WritePlan, references ...*base.AppConfigReferencePlan) error {
	referencePlan, err := base.NormalizeAppConfigReferencePlan(projection, references)
	if err != nil || base.AppConfigUsesRuntimeSecrets(in.Runtime) && referencePlan == nil {
		if err == nil {
			err = base.ErrPreconditionFailed
		}
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.deployments[in.DeploymentID]
	if !ok || !s.canAccessDeploymentLocked(actor, d) {
		return base.ErrNotFound
	}
	if err := s.authorizeLocked(actor, domain.PermissionConfigWrite, domain.AccessTarget{Type: "deployment", ID: d.ID}); err != nil {
		return err
	}
	version := d.ConfigVersion
	if projection == nil {
		if len(d.ConfigRaw) == 0 || domain.DeploymentConfigETag(d.ID, version, d.ConfigRaw) != in.BaseETag {
			return base.ErrPreconditionFailed
		}
	} else {
		if projection.EnvironmentID != d.EnvironmentID || projection.ApplicationID != d.ApplicationID ||
			projection.Precondition != gitprojection.MutationMatchETag || projection.ExpectedETag != in.BaseETag {
			return base.ErrPreconditionFailed
		}
		if _, err = s.validateProjectionPlanLocked(projection); err != nil {
			return err
		}
	}
	key := hex.EncodeToString(in.TokenHash)
	if _, exists := s.configPreviews[key]; exists {
		return base.ErrConflict
	}
	s.configPreviews[key] = domain.ConfigPreviewLease{ID: id.New(), DeploymentID: d.ID, ActorID: actor, TokenHash: append([]byte(nil), in.TokenHash...), BaseETag: in.BaseETag, CandidateHash: append([]byte(nil), in.CandidateHash...), ExpiresAt: in.ExpiresAt, CreatedAt: time.Now().UTC()}
	if projection != nil {
		s.configPreviewGit[key] = *projection
	}
	return nil
}

func (s *Store) SaveDeploymentConfig(_ context.Context, actor, key, fingerprint, requestID string, in domain.SaveDeploymentConfig, projection *gitprojection.WritePlan, references ...*base.AppConfigReferencePlan) (base.Result[domain.Deployment], domain.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idemIdentity := ik(actor, "deployments.config.save", key)
	old, replay := s.idempotency[idemIdentity]
	if replay {
		replayedDeployment, exists := s.deployments[old.resourceID]
		if !exists || !s.canAccessDeploymentLocked(actor, replayedDeployment) {
			return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrNotFound
		}
		if err := s.authorizeLocked(actor, domain.PermissionConfigWrite, domain.AccessTarget{Type: "deployment", ID: replayedDeployment.ID}); err != nil {
			return base.Result[domain.Deployment]{}, domain.Operation{}, err
		}
		if err := check(old, true, fingerprint); err != nil {
			return base.Result[domain.Deployment]{}, domain.Operation{}, err
		}
		return base.Result[domain.Deployment]{Value: replayedDeployment, Replay: true}, s.operations[old.operationID], nil
	}
	referencePlan, err := base.NormalizeAppConfigReferencePlan(projection, references)
	if err != nil || base.AppConfigUsesRuntimeSecrets(in.Runtime) && referencePlan == nil {
		if err == nil {
			err = base.ErrPreconditionFailed
		}
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	d, ok := s.deployments[in.DeploymentID]
	if !ok || !s.canAccessDeploymentLocked(actor, d) {
		return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrNotFound
	}
	if err := s.authorizeLocked(actor, domain.PermissionConfigWrite, domain.AccessTarget{Type: "deployment", ID: d.ID}); err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	version := d.ConfigVersion
	var projectionBinding gitprojection.Binding
	if projection == nil {
		if len(d.ConfigRaw) == 0 || domain.DeploymentConfigETag(d.ID, version, d.ConfigRaw) != in.BaseETag {
			return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrPreconditionFailed
		}
	} else {
		if projection.EnvironmentID != d.EnvironmentID || projection.ApplicationID != d.ApplicationID ||
			projection.Precondition != gitprojection.MutationMatchETag || projection.ExpectedETag != in.BaseETag {
			return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrPreconditionFailed
		}
		if projectionBinding, err = s.validateProjectionPlanLocked(projection); err != nil {
			return base.Result[domain.Deployment]{}, domain.Operation{}, err
		}
	}
	if projection != nil {
		resolution, resolutionErr := s.resolveProjectedVariablesLocked(projectionBinding, in.Runtime)
		if resolutionErr != nil {
			return base.Result[domain.Deployment]{}, domain.Operation{}, resolutionErr
		}
		in.Runtime = resolution.Runtime
	}
	previewKey := hex.EncodeToString(in.TokenHash)
	preview, ok := s.configPreviews[previewKey]
	if !ok || preview.ActorID != actor || preview.DeploymentID != d.ID || preview.BaseETag != in.BaseETag || !bytes.Equal(preview.CandidateHash, in.CandidateHash) {
		return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrPreviewInvalid
	}
	previewProjection, hasPreviewProjection := s.configPreviewGit[previewKey]
	if projection == nil && hasPreviewProjection || projection != nil && (!hasPreviewProjection || !reflect.DeepEqual(previewProjection, *projection)) {
		return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrPreviewInvalid
	}
	if preview.ConsumedAt != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrPreviewConsumed
	}
	now := time.Now().UTC()
	if !preview.ExpiresAt.After(now) {
		return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrPreviewExpired
	}
	for operationID, operation := range s.operations {
		if operation.TargetID == d.ID && operation.Status == "queued" {
			operation.Status, operation.UpdatedAt, operation.FinishedAt = "superseded", now, &now
			operation.Problem = &domain.ProblemData{Code: "Superseded", Detail: "A newer configuration was accepted."}
			s.operations[operationID] = operation
		}
	}
	d.Generation++
	opID := id.New()
	op := domain.Operation{ID: opID, Kind: "deployment.git-write", Status: "queued", TargetType: "deployment", TargetID: d.ID, RequestID: requestID, Generation: d.Generation, Progress: []domain.ProgressStep{{Name: "git-write", Status: "pending"}}, CreatedAt: now, UpdatedAt: now}
	d.Runtime = cloneRuntime(in.Runtime)
	d.Replicas, d.Port, d.Environment = domain.LegacyWorkloadFields(d.Runtime)
	d.ConfigRaw = append([]byte(nil), in.RawYAML...)
	d.ConfigVersion++
	d.State, d.OperationID, d.UpdatedAt = "pending-git", op.ID, now
	if err := s.putGitWriteCommandLocked(actor, opID, d.ID, projection, d.ConfigRaw, "config("+d.ApplicationID+"): save AppConfig", now); err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	s.deployments[d.ID] = d
	s.deploymentInputs[op.ID] = d
	s.operations[op.ID] = op
	s.outbox[op.ID] = &outboxRecord{message: domain.WorkMessage{OperationID: op.ID, Kind: op.Kind, ScopeID: d.EnvironmentID, Generation: d.Generation, TraceID: requestID}}
	preview.ConsumedAt = &now
	s.configPreviews[previewKey] = preview
	s.idempotency[idemIdentity] = idemRecord{fingerprint: fingerprint, typ: "deployment", resourceID: d.ID, operationID: op.ID}
	s.audits++
	return base.Result[domain.Deployment]{Value: d}, op, nil
}
