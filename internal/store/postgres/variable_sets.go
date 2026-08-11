package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/gitpublication"
	"github.com/kuberploy/kuberploy/internal/id"
	base "github.com/kuberploy/kuberploy/internal/store"
	"github.com/kuberploy/kuberploy/internal/variables"
)

func variableTarget(plan gitprojection.WritePlan) domain.AccessTarget {
	if plan.VariableScope == "project" {
		return domain.AccessTarget{Type: "project", ID: plan.ProjectID}
	}
	return domain.AccessTarget{Type: "environment", ID: plan.EnvironmentID}
}

func validateVariablePlanTx(ctx context.Context, tx pgx.Tx, plan gitprojection.WritePlan) (gitprojection.Binding, error) {
	binding, err := scanCentralGitBinding(tx.QueryRow(ctx, `SELECT `+centralGitBindingColumns+` FROM git_repository_bindings WHERE id=$1 FOR UPDATE`, plan.BindingID))
	if err != nil || plan.Validate(binding) != nil || plan.VariableScope == "" {
		return gitprojection.Binding{}, base.ErrPreconditionFailed
	}
	document, documentErr := scanCentralGitDocument(tx.QueryRow(ctx, `SELECT `+centralGitDocumentColumns+` FROM git_projected_documents WHERE binding_id=$1 AND generation=$2 AND path=$3 FOR SHARE`, binding.ID, binding.ProjectionGeneration, plan.VariablePath))
	if plan.Precondition == gitprojection.MutationCreateIfAbsent {
		if documentErr == nil || !errors.Is(documentErr, base.ErrNotFound) {
			return gitprojection.Binding{}, base.ErrPreconditionFailed
		}
	} else if plan.Precondition == gitprojection.MutationMatchETag {
		if documentErr != nil || !document.Valid || `"`+document.ContentSHA256+`"` != plan.ExpectedETag {
			return gitprojection.Binding{}, base.ErrPreconditionFailed
		}
	} else {
		return gitprojection.Binding{}, base.ErrPreconditionFailed
	}
	return binding, nil
}

func (s *Store) CreateVariableSetPreview(ctx context.Context, actor string, plan gitprojection.WritePlan, tokenHash, candidateHash []byte, expires time.Time) error {
	if len(tokenHash) != 32 || len(candidateHash) != 32 || !expires.After(time.Now().UTC()) {
		return base.ErrPreviewInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = authorizeWith(ctx, tx, actor, domain.PermissionConfigWrite, variableTarget(plan)); err != nil {
		return err
	}
	if _, err = validateVariablePlanTx(ctx, tx, plan); err != nil {
		return err
	}
	now := time.Now().UTC()
	_, err = tx.Exec(ctx, `INSERT INTO preview_authorities(token_hash,preview_kind,actor_id,binding_id,project_id,environment_id,variable_scope,path,base_revision,base_etag,policy_version,candidate_hash,expires_at,created_at)
		VALUES($1,'variable-set',$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, tokenHash, actor, plan.BindingID, plan.ProjectID, plan.EnvironmentID, plan.VariableScope, plan.VariablePath, plan.BaseRevision, plan.ExpectedETag, plan.PolicyVersion, candidateHash, expires, now)
	if err != nil {
		return classify(err)
	}
	return tx.Commit(ctx)
}

func (s *Store) VariableSetPreviewAuthority(ctx context.Context, actor string, tokenHash []byte) (gitprojection.WritePlan, []byte, error) {
	if len(tokenHash) != sha256.Size {
		return gitprojection.WritePlan{}, nil, base.ErrPreviewInvalid
	}
	var plan gitprojection.WritePlan
	var candidateHash []byte
	err := s.pool.QueryRow(ctx, `SELECT p.binding_id::text,p.project_id::text,p.environment_id::text,p.variable_scope,p.path,p.base_revision,
		p.base_etag,p.candidate_hash,p.policy_version FROM preview_authorities p
		WHERE p.token_hash=$1 AND p.preview_kind='variable-set' AND p.actor_id=$2`, tokenHash, actor).Scan(&plan.BindingID, &plan.ProjectID, &plan.EnvironmentID,
		&plan.VariableScope, &plan.VariablePath, &plan.BaseRevision, &plan.ExpectedETag, &candidateHash, &plan.PolicyVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return gitprojection.WritePlan{}, nil, base.ErrPreviewInvalid
	}
	if err != nil {
		return gitprojection.WritePlan{}, nil, err
	}
	plan.Precondition = gitprojection.MutationCreateIfAbsent
	if plan.ExpectedETag != "" {
		plan.Precondition = gitprojection.MutationMatchETag
	}
	return plan, candidateHash, nil
}

func (s *Store) SaveVariableSet(ctx context.Context, actor, key, fingerprint, requestID string, plan gitprojection.WritePlan, tokenHash, candidateHash, raw []byte) (base.Result[domain.Operation], error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return base.Result[domain.Operation]{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, advisoryIdentity(actor, "variable-sets.save", key)); err != nil {
		return base.Result[domain.Operation]{}, err
	}
	if old, ok, findErr := findIdem(ctx, tx, actor, "variable-sets.save", key); findErr != nil {
		return base.Result[domain.Operation]{}, findErr
	} else if ok {
		if old.fingerprint != fingerprint {
			return base.Result[domain.Operation]{}, base.ErrIdempotencyConflict
		}
		if old.operationID == nil {
			return base.Result[domain.Operation]{}, base.ErrConflict
		}
		op, getErr := getOperation(ctx, tx, *old.operationID)
		if getErr != nil {
			return base.Result[domain.Operation]{}, getErr
		}
		return base.Result[domain.Operation]{Value: op, Replay: true}, nil
	}
	if len(tokenHash) != 32 || len(candidateHash) != 32 || len(raw) == 0 {
		return base.Result[domain.Operation]{}, base.ErrPreviewInvalid
	}
	parsed, diagnostics := variables.ParseAndValidate(raw)
	if len(diagnostics) != 0 || parsed.Values == nil {
		return base.Result[domain.Operation]{}, gitprojection.ErrInvalid
	}
	sum := sha256.Sum256(raw)
	if !bytes.Equal(sum[:], candidateHash) {
		return base.Result[domain.Operation]{}, base.ErrPreviewInvalid
	}
	if err = authorizeWith(ctx, tx, actor, domain.PermissionConfigWrite, variableTarget(plan)); err != nil {
		return base.Result[domain.Operation]{}, err
	}
	binding, err := validateVariablePlanTx(ctx, tx, plan)
	if err != nil {
		return base.Result[domain.Operation]{}, err
	}
	var previewActor, previewBinding, previewProject, previewEnvironment, previewScope, previewPath, previewBase, previewETag, previewPolicy string
	var previewCandidate []byte
	var expires time.Time
	var consumed *time.Time
	err = tx.QueryRow(ctx, `SELECT actor_id::text,binding_id::text,project_id::text,environment_id::text,variable_scope,path,base_revision,base_etag,policy_version,candidate_hash,expires_at,consumed_at FROM preview_authorities WHERE token_hash=$1 AND preview_kind='variable-set' FOR UPDATE`, tokenHash).Scan(&previewActor, &previewBinding, &previewProject, &previewEnvironment, &previewScope, &previewPath, &previewBase, &previewETag, &previewPolicy, &previewCandidate, &expires, &consumed)
	if errors.Is(err, pgx.ErrNoRows) {
		return base.Result[domain.Operation]{}, base.ErrPreviewInvalid
	}
	if err != nil {
		return base.Result[domain.Operation]{}, err
	}
	if previewActor != actor || previewBinding != plan.BindingID || previewProject != plan.ProjectID || previewEnvironment != plan.EnvironmentID || previewScope != plan.VariableScope || previewPath != plan.VariablePath || previewBase != plan.BaseRevision || previewETag != plan.ExpectedETag || previewPolicy != plan.PolicyVersion || !bytes.Equal(previewCandidate, candidateHash) {
		return base.Result[domain.Operation]{}, base.ErrPreviewInvalid
	}
	now := time.Now().UTC()
	if consumed != nil {
		return base.Result[domain.Operation]{}, base.ErrPreviewConsumed
	}
	if !expires.After(now) {
		return base.Result[domain.Operation]{}, base.ErrPreviewExpired
	}
	targetID := plan.EnvironmentID
	if plan.VariableScope == "project" {
		targetID = plan.ProjectID
	}
	op := domain.Operation{ID: id.New(), Kind: "variable-set.git-write", Status: "queued", TargetType: plan.VariableScope, TargetID: targetID, RequestID: requestID, Generation: 1, Progress: []domain.ProgressStep{{Name: "git-write", Status: "pending"}}, CreatedAt: now, UpdatedAt: now}
	progress, _ := json.Marshal(op.Progress)
	if _, err = tx.Exec(ctx, `INSERT INTO operations(id,kind,status,target_type,target_id,request_id,generation,progress,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,1,$7,$8,$8)`, op.ID, op.Kind, op.Status, op.TargetType, op.TargetID, op.RequestID, progress, now); err != nil {
		return base.Result[domain.Operation]{}, classify(err)
	}
	command, err := gitprojection.NewVariableWriteCommand(op.ID, actor, plan, binding, raw, "variables("+plan.VariableScope+"): save VariableSet", fingerprint, now)
	if err != nil {
		return base.Result[domain.Operation]{}, err
	}
	var protection domain.EnvironmentProtectionPolicy
	if err = tx.QueryRow(ctx, `SELECT protection_policy FROM environments WHERE id=$1 FOR SHARE`, plan.EnvironmentID).Scan(&protection); err != nil {
		return base.Result[domain.Operation]{}, classify(err)
	}
	mode := gitpublication.ModePullRequest
	if protection == domain.EnvironmentDevelopment {
		mode = gitpublication.ModeDirect
	} else if protection != domain.EnvironmentProtected {
		return base.Result[domain.Operation]{}, base.ErrConflict
	}
	command.PublicationMode = gitprojection.PublicationMode(mode)
	if command.Validate(binding) != nil {
		return base.Result[domain.Operation]{}, gitprojection.ErrInvalid
	}
	_, err = tx.Exec(ctx, `INSERT INTO git_variable_write_commands(operation_id,actor_id,binding_id,project_id,environment_id,scope,target_ref,path,base_revision,precondition,expected_etag,parser_version,content,content_sha256,message,publication_mode,state,request_digest,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,'pending',$17,$18,$18)`, command.OperationID, command.ActorID, plan.BindingID, plan.ProjectID, plan.EnvironmentID, plan.VariableScope, command.TargetRef, command.Path, plan.BaseRevision, plan.Precondition, plan.ExpectedETag, plan.PolicyVersion, command.Content, command.ContentSHA256, command.Message, mode, command.RequestDigest, now)
	if err != nil {
		return base.Result[domain.Operation]{}, classify(err)
	}
	if mode == gitpublication.ModePullRequest {
		publication, pubErr := gitpublication.NewPublication(op.ID, binding.ID, gitpublication.Repository{InstallationID: binding.Repository.InstallationID, ID: binding.Repository.RepositoryID, Owner: binding.Repository.Owner, Name: binding.Repository.Name}, binding.TargetRef, plan.BaseRevision, now)
		if pubErr != nil {
			return base.Result[domain.Operation]{}, pubErr
		}
		if pubErr = insertGitPublicationTx(ctx, tx, publication); pubErr != nil {
			return base.Result[domain.Operation]{}, pubErr
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO outbox(operation_id,kind,scope_id,generation,trace_id) VALUES($1,$2,$3,1,$4)`, op.ID, op.Kind, plan.EnvironmentID, requestID); err != nil {
		return base.Result[domain.Operation]{}, err
	}
	if err = putIdem(ctx, tx, actor, "variable-sets.save", key, fingerprint, plan.VariableScope, targetID, &op.ID); err != nil {
		return base.Result[domain.Operation]{}, classify(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE preview_authorities SET consumed_at=$2 WHERE token_hash=$1 AND preview_kind='variable-set' AND consumed_at IS NULL`, tokenHash, now); err != nil {
		return base.Result[domain.Operation]{}, err
	}
	if err = audit(ctx, tx, actor, "variable-set.accepted", plan.VariableScope, targetID, requestID, map[string]any{"operationId": op.ID, "bindingId": plan.BindingID, "path": plan.VariablePath, "baseRevision": plan.BaseRevision, "publicationMode": mode}); err != nil {
		return base.Result[domain.Operation]{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return base.Result[domain.Operation]{}, classify(err)
	}
	return base.Result[domain.Operation]{Value: op}, nil
}
