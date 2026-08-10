package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/gitpublication"
	base "github.com/kuberploy/kuberploy/internal/store"
	"github.com/kuberploy/kuberploy/internal/variablecompiler"
)

const centralGitDocumentColumns = `binding_id::text,generation,path,COALESCE(application_id::text,''),source_revision,
	config_revision,blob_id,content_sha256,raw,parsed,valid,diagnostics,schema_version,parser_version,indexed_at`

func scanCentralGitDocument(row pgx.Row) (gitprojection.Document, error) {
	var document gitprojection.Document
	var parsed, diagnostics []byte
	err := row.Scan(&document.BindingID, &document.Generation, &document.Path, &document.ApplicationID, &document.SourceRevision,
		&document.ConfigRevision, &document.BlobID, &document.ContentSHA256, &document.Raw, &parsed, &document.Valid,
		&diagnostics, &document.SchemaVersion, &document.ParserVersion, &document.IndexedAt)
	if err != nil {
		return gitprojection.Document{}, classify(err)
	}
	if len(parsed) > 0 && string(parsed) != "null" {
		if err = json.Unmarshal(parsed, &document.Parsed); err != nil {
			return gitprojection.Document{}, base.ErrConflict
		}
	}
	if err = json.Unmarshal(diagnostics, &document.Diagnostics); err != nil {
		return gitprojection.Document{}, base.ErrConflict
	}
	return document, nil
}

func projectedVariableDocumentsTx(ctx context.Context, tx pgx.Tx, binding gitprojection.Binding) ([]string, []gitprojection.Document, error) {
	paths, err := gitprojection.DependencyPaths(binding)
	if err != nil {
		return nil, nil, err
	}
	documents := make([]gitprojection.Document, 0, len(paths))
	for _, dependencyPath := range paths {
		dependency, queryErr := scanCentralGitDocument(tx.QueryRow(ctx, `SELECT `+centralGitDocumentColumns+`
			FROM git_projected_documents WHERE binding_id=$1 AND generation=$2 AND path=$3 FOR SHARE`,
			binding.ID, binding.ProjectionGeneration, dependencyPath))
		if errors.Is(queryErr, base.ErrNotFound) {
			continue
		}
		if queryErr != nil || dependency.Validate(binding) != nil || !dependency.Valid {
			return nil, nil, base.ErrPreconditionFailed
		}
		documents = append(documents, dependency)
	}
	return paths, documents, nil
}

func resolveProjectedVariablesTx(ctx context.Context, tx pgx.Tx, binding gitprojection.Binding, runtime domain.WorkloadRuntime) (variablecompiler.Resolution, error) {
	paths, documents, err := projectedVariableDocumentsTx(ctx, tx, binding)
	if err != nil {
		return variablecompiler.Resolution{}, err
	}
	states, err := variablecompiler.States(paths, documents)
	if err != nil {
		return variablecompiler.Resolution{}, base.ErrPreconditionFailed
	}
	resolution, err := variablecompiler.Resolve(states, documents, runtime)
	if err != nil {
		return variablecompiler.Resolution{}, base.ErrPreconditionFailed
	}
	return resolution, nil
}

func validateGitProjectionPlanTx(ctx context.Context, tx pgx.Tx, plan *gitprojection.WritePlan) (gitprojection.Binding, error) {
	if plan == nil {
		return gitprojection.Binding{}, gitprojection.ErrInvalid
	}
	binding, err := scanCentralGitBinding(tx.QueryRow(ctx, `SELECT `+centralGitBindingColumns+`
		FROM git_repository_bindings WHERE kind='environment' AND environment_id=$1 FOR UPDATE`, plan.EnvironmentID))
	if err != nil || plan.Validate(binding) != nil {
		return gitprojection.Binding{}, base.ErrPreconditionFailed
	}
	documentPath, err := gitprojection.ApplicationPath(binding, plan.ApplicationID)
	if err != nil {
		return gitprojection.Binding{}, base.ErrPreconditionFailed
	}
	document, documentErr := scanCentralGitDocument(tx.QueryRow(ctx, `SELECT `+centralGitDocumentColumns+`
		FROM git_projected_documents WHERE binding_id=$1 AND generation=$2 AND path=$3 FOR SHARE`,
		binding.ID, binding.ProjectionGeneration, documentPath))
	dependencyPaths, dependencies, dependencyErr := projectedVariableDocumentsTx(ctx, tx, binding)
	if dependencyErr != nil {
		return gitprojection.Binding{}, base.ErrPreconditionFailed
	}
	switch plan.Precondition {
	case gitprojection.MutationCreateIfAbsent:
		if documentErr == nil {
			return gitprojection.Binding{}, base.ErrPreconditionFailed
		}
		if !errors.Is(documentErr, base.ErrNotFound) {
			return gitprojection.Binding{}, documentErr
		}
	case gitprojection.MutationMatchETag:
		if documentErr != nil || document.Validate(binding) != nil {
			return gitprojection.Binding{}, base.ErrPreconditionFailed
		}
		etag, etagErr := gitprojection.StrongETagWithDependencies(binding, []gitprojection.Document{document}, dependencyPaths, dependencies, plan.ChartDigest, plan.PolicyVersion)
		if etagErr != nil || etag != plan.ExpectedETag {
			return gitprojection.Binding{}, base.ErrPreconditionFailed
		}
	default:
		return gitprojection.Binding{}, base.ErrPreconditionFailed
	}
	return binding, nil
}

func insertGitWriteCommandTx(ctx context.Context, tx pgx.Tx, actor, operationID, deploymentID string, plan *gitprojection.WritePlan, content []byte, message string, now time.Time) error {
	if plan == nil {
		return nil
	}
	binding, err := validateGitProjectionPlanTx(ctx, tx, plan)
	if err != nil {
		return err
	}
	command, err := gitprojection.NewWriteCommand(operationID, deploymentID, actor, *plan, binding, content, message, now)
	if err != nil {
		return err
	}
	var protection domain.EnvironmentProtectionPolicy
	if err = tx.QueryRow(ctx, `SELECT protection_policy FROM environments WHERE id=$1 FOR SHARE`, plan.EnvironmentID).Scan(&protection); err != nil {
		return classify(err)
	}
	mode := gitpublication.ModePullRequest
	if protection == domain.EnvironmentDevelopment {
		mode = gitpublication.ModeDirect
	} else if protection != domain.EnvironmentProtected {
		return base.ErrConflict
	}
	command.PublicationMode = gitprojection.PublicationMode(mode)
	if command.Validate(binding) != nil {
		return gitprojection.ErrInvalid
	}
	_, err = tx.Exec(ctx, `INSERT INTO git_deployment_write_commands(operation_id,deployment_id,actor_id,binding_id,project_id,
		environment_id,application_id,target_ref,path,base_revision,precondition,expected_etag,chart_digest,policy_version,
		content,content_sha256,message,publication_mode,state,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,'pending',$19,$19)`,
		command.OperationID, command.DeploymentID, command.ActorID, command.Plan.BindingID, command.Plan.ProjectID,
		command.Plan.EnvironmentID, command.Plan.ApplicationID, command.TargetRef, command.Path, command.Plan.BaseRevision,
		command.Plan.Precondition, command.Plan.ExpectedETag, command.Plan.ChartDigest, command.Plan.PolicyVersion,
		command.Content, command.ContentSHA256, command.Message, mode, command.CreatedAt)
	if err != nil {
		return classify(err)
	}
	if mode == gitpublication.ModePullRequest {
		publication, publicationErr := gitpublication.NewPublication(operationID, binding.ID, gitpublication.Repository{
			InstallationID: binding.Repository.InstallationID, ID: binding.Repository.RepositoryID,
			Owner: binding.Repository.Owner, Name: binding.Repository.Name,
		}, binding.TargetRef, plan.BaseRevision, now)
		if publicationErr != nil {
			return publicationErr
		}
		if publicationErr = insertGitPublicationTx(ctx, tx, publication); publicationErr != nil {
			return publicationErr
		}
	}
	return nil
}
