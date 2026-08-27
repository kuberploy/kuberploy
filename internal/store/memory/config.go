package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"time"

	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/middlewareprofiles"
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
	var middlewareRefs []domain.SecretBindingRef
	var diagnostics []appconfig.Diagnostic
	var refsErr error
	if len(in.CandidateRaw) != 0 {
		parsed, _, parsedDiagnostics := appconfig.ParseAndValidate(in.CandidateRaw)
		diagnostics = parsedDiagnostics
		middlewareRefs, refsErr = middlewareprofiles.AppConfigSecretReferences(parsed)
	}
	if len(diagnostics) != 0 || refsErr != nil {
		return base.ErrPreconditionFailed
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
	var referencePlan *base.AppConfigReferencePlan
	var err error
	if d.State == "stopped" && projection == nil {
		referencePlan, err = base.NormalizeLocalDraftAppConfigReferencePlan(references)
	} else {
		referencePlan, err = base.NormalizeAppConfigReferencePlan(projection, references)
	}
	if err != nil || (base.AppConfigUsesRuntimeSecrets(in.Runtime) || len(middlewareRefs) != 0) && referencePlan == nil {
		if err == nil {
			err = base.ErrPreconditionFailed
		}
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
	d, ok := s.deployments[in.DeploymentID]
	if !ok || !s.canAccessDeploymentLocked(actor, d) {
		return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrNotFound
	}
	if err := s.authorizeLocked(actor, domain.PermissionConfigWrite, domain.AccessTarget{Type: "deployment", ID: d.ID}); err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	var referencePlan *base.AppConfigReferencePlan
	var err error
	if d.State == "stopped" && projection == nil {
		referencePlan, err = base.NormalizeLocalDraftAppConfigReferencePlan(references)
	} else {
		referencePlan, err = base.NormalizeAppConfigReferencePlan(projection, references)
	}
	if err != nil {
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
	digest := sha256.Sum256(in.RawYAML)
	parsed, exactRuntime, diagnostics := appconfig.ParseAndValidate(in.RawYAML)
	exactImage, imageOK := appconfig.MaterializedImage(parsed)
	middlewareRefs, refsErr := middlewareprofiles.AppConfigSecretReferences(parsed)
	if !bytes.Equal(digest[:], in.CandidateHash) || len(diagnostics) != 0 || refsErr != nil || !imageOK {
		return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrPreconditionFailed
	}
	in.Runtime = exactRuntime
	if projection != nil {
		resolution, resolutionErr := s.resolveProjectedVariablesLocked(projectionBinding, in.Runtime)
		if resolutionErr != nil {
			return base.Result[domain.Deployment]{}, domain.Operation{}, resolutionErr
		}
		in.Runtime = resolution.Runtime
	}
	if (base.AppConfigUsesRuntimeSecrets(in.Runtime) || len(middlewareRefs) != 0) && referencePlan == nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrPreconditionFailed
	}
	for operationID, operation := range s.operations {
		if operation.TargetID == d.ID && operation.Status == "queued" {
			operation.Status, operation.UpdatedAt, operation.FinishedAt = "superseded", now, &now
			operation.Problem = &domain.ProblemData{Code: "Superseded", Detail: "A newer configuration was accepted."}
			s.operations[operationID] = operation
		}
	}
	draftSave := d.State == "stopped" && projection == nil
	d.Generation++
	opID := id.New()
	op := domain.Operation{ID: opID, Kind: "deployment.git-write", Status: "queued", TargetType: "deployment", TargetID: d.ID, RequestID: requestID, Generation: d.Generation, Progress: []domain.ProgressStep{{Name: "git-write", Status: "pending"}}, CreatedAt: now, UpdatedAt: now}
	if draftSave {
		op.Kind, op.Status = "deployment.config-draft-save", "succeeded"
		op.Progress = []domain.ProgressStep{{Name: "save-draft", Status: "succeeded", StartedAt: &now, FinishedAt: &now}}
		op.FinishedAt = &now
	}
	d.Runtime = cloneRuntime(in.Runtime)
	d.Image = exactImage
	d.Replicas, d.Port, d.Environment = domain.LegacyWorkloadFields(d.Runtime)
	d.ConfigRaw = append([]byte(nil), in.RawYAML...)
	d.ConfigVersion++
	d.State, d.OperationID, d.UpdatedAt = "pending-git", op.ID, now
	if draftSave {
		d.State = "stopped"
	} else if err := s.putGitWriteCommandLocked(actor, opID, d.ID, projection, d.ConfigRaw, "config("+d.ApplicationID+"): save AppConfig", now); err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	s.deployments[d.ID] = d
	s.deploymentInputs[op.ID] = d
	s.operations[op.ID] = op
	if !draftSave {
		s.outbox[op.ID] = &outboxRecord{message: domain.WorkMessage{OperationID: op.ID, Kind: op.Kind, ScopeID: d.EnvironmentID, Generation: d.Generation, TraceID: requestID}}
	}
	preview.ConsumedAt = &now
	s.configPreviews[previewKey] = preview
	s.idempotency[idemIdentity] = idemRecord{fingerprint: fingerprint, typ: "deployment", resourceID: d.ID, operationID: op.ID}
	s.audits++
	return base.Result[domain.Deployment]{Value: d}, op, nil
}
