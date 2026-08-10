package argo

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/imagepull"
	"github.com/kuberploy/kuberploy/internal/projectionpolicy"
)

const maximumDesiredStatePolicyDocuments = 1_000

// PostgreSQLRegistryEligibilityResolver is the production eligibility
// boundary between an indexed AppConfig generation and Argo command planning.
// It never accepts a policy document or Kubernetes Secret name from a caller:
// documents are reloaded from the exact active generation under row locks and
// converted to projectionpolicy.AppConfigPolicyDocument in the transaction.
type PostgreSQLRegistryEligibilityResolver struct {
	pool               *pgxpool.Pool
	maximumArtifactAge time.Duration
}

func NewPostgreSQLRegistryEligibilityResolver(pool *pgxpool.Pool, maximumArtifactAge time.Duration) (*PostgreSQLRegistryEligibilityResolver, error) {
	if pool == nil || maximumArtifactAge <= 0 || maximumArtifactAge > 15*time.Minute {
		return nil, ErrInvalid
	}
	return &PostgreSQLRegistryEligibilityResolver{pool: pool, maximumArtifactAge: maximumArtifactAge}, nil
}

// ResolveRegistryReferences proves that the caller's catalog contains exactly
// the application AppConfigs in the provider-verified active generation, then
// evaluates each typed document with RegistryPullArtifactEligibleTx. Public
// documents need no artifact. Missing, inactive, stale, or profile-mismatched
// private artifacts return false without manufacturing a partial approval.
func (r *PostgreSQLRegistryEligibilityResolver) ResolveRegistryReferences(
	ctx context.Context,
	target DesiredStateTarget,
	approval DesiredStateProjectionApproval,
	now time.Time,
) (bool, error) {
	if r == nil || r.pool == nil || target.Validate() != nil || approval.validateFor(target, false) != nil ||
		now.IsZero() || r.maximumArtifactAge <= 0 || r.maximumArtifactAge > 15*time.Minute {
		return false, ErrInvalid
	}
	expectedApplications, err := desiredStateCatalogApplications(target, approval.Applications, approval.Deployments)
	if err != nil {
		return false, err
	}

	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, classifyPostgres(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	binding := target.Environment.Binding
	var namespace string
	err = tx.QueryRow(ctx, `SELECT e.namespace
		FROM git_repository_bindings b
		JOIN git_projection_generations g ON g.binding_id=b.id AND g.generation=b.projection_generation
		JOIN projects p ON p.id=b.project_id
		JOIN environments e ON e.id=b.environment_id AND e.project_id=p.id
		WHERE b.id=$1 AND b.kind='environment' AND b.scope_id=$2 AND b.project_id=$3 AND b.environment_id=$4
		  AND b.provider=$5 AND b.installation_id=$6 AND b.repository_id=$7
		  AND b.repository_owner=$8 AND b.repository_name=$9 AND b.target_ref=$10 AND b.path_prefix=$11
		  AND b.credential_mode=$12 AND b.credential_secret_name=$13
		  AND b.state='ready' AND b.target_head_revision=$14 AND b.indexed_revision=$14
		  AND b.projection_generation=$15 AND b.parser_version=$16
		  AND g.state='active' AND g.head_revision=$14 AND g.parser_version=$16
		  AND e.namespace=$17
		FOR SHARE OF b,g,p,e`,
		binding.ID, binding.ScopeID, binding.ProjectID, binding.EnvironmentID,
		binding.Repository.Provider, binding.Repository.InstallationID, binding.Repository.RepositoryID,
		binding.Repository.Owner, binding.Repository.Name, binding.TargetRef, binding.Prefix,
		binding.CredentialMode, binding.CredentialSecretName, binding.IndexedRevision,
		binding.ProjectionGeneration, binding.ParserVersion, target.Environment.Environment.Namespace).Scan(&namespace)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, classifyPostgres(err)
	}
	if namespace != target.Environment.Environment.Namespace {
		return false, ErrConflict
	}

	rows, err := tx.Query(ctx, `SELECT d.path,d.application_id::text,d.source_revision,d.config_revision,d.blob_id,
		d.content_sha256,d.raw,d.valid,d.schema_version,d.parser_version,d.indexed_at
		FROM git_projected_documents d
		JOIN applications a ON a.id=d.application_id AND a.project_id=$3
		WHERE d.binding_id=$1 AND d.generation=$2 AND d.application_id IS NOT NULL
		ORDER BY d.path
		FOR SHARE OF d,a`, binding.ID, binding.ProjectionGeneration, binding.ProjectID)
	if err != nil {
		return false, classifyPostgres(err)
	}
	defer rows.Close()

	seen := make(map[string]struct{}, len(expectedApplications))
	documents := make([]gitprojection.Document, 0, len(expectedApplications))
	for rows.Next() {
		if len(seen) >= maximumDesiredStatePolicyDocuments {
			return false, ErrInvalid
		}
		var document gitprojection.Document
		document.BindingID, document.Generation = binding.ID, binding.ProjectionGeneration
		if err = rows.Scan(&document.Path, &document.ApplicationID, &document.SourceRevision, &document.ConfigRevision,
			&document.BlobID, &document.ContentSHA256, &document.Raw, &document.Valid, &document.SchemaVersion,
			&document.ParserVersion, &document.IndexedAt); err != nil {
			return false, classifyPostgres(err)
		}
		document.Diagnostics = nil
		if document.Validate(binding) != nil || !document.Valid || document.SourceRevision != binding.IndexedRevision ||
			document.Generation != approval.ProjectionGeneration {
			return false, ErrConflict
		}
		if _, expected := expectedApplications[document.ApplicationID]; !expected {
			return false, ErrConflict
		}
		if _, duplicate := seen[document.ApplicationID]; duplicate {
			return false, ErrConflict
		}
		seen[document.ApplicationID] = struct{}{}
		documents = append(documents, document)
	}
	if err = rows.Err(); err != nil {
		return false, classifyPostgres(err)
	}
	rows.Close()
	if len(seen) != len(expectedApplications) {
		return false, ErrConflict
	}

	// pgx permits only one active result stream per transaction connection, so
	// finish and close the exact document catalog before issuing artifact
	// eligibility queries under the same serializable transaction.
	for _, document := range documents {
		parsed, runtime, diagnostics := appconfig.ParseAndValidate(document.Raw)
		if parsed == nil || len(diagnostics) != 0 {
			return false, ErrConflict
		}
		policyDocument, documentErr := projectionpolicy.NewAppConfigPolicyDocument(projectionpolicy.DocumentScope{
			Binding: binding, Namespace: namespace, ApplicationID: document.ApplicationID, Path: document.Path,
			SourceRevision: document.SourceRevision, ConfigRevision: document.ConfigRevision,
		}, parsed, runtime)
		if documentErr != nil {
			return false, ErrConflict
		}
		eligible, eligibilityErr := projectionpolicy.RegistryPullArtifactEligibleTx(
			ctx, tx, policyDocument, now.UTC(), r.maximumArtifactAge,
		)
		if eligibilityErr != nil {
			return false, classifyRegistryEligibilityError(eligibilityErr)
		}
		if !eligible {
			return false, nil
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return false, classifyPostgres(err)
	}
	return true, nil
}

func desiredStateCatalogApplications(
	target DesiredStateTarget,
	applications []domain.Application,
	deployments []domain.Deployment,
) (map[string]struct{}, error) {
	if len(applications) > maximumDesiredStatePolicyDocuments || len(deployments) > maximumDesiredStatePolicyDocuments {
		return nil, ErrInvalid
	}
	expected := make(map[string]struct{}, len(applications))
	for _, application := range applications {
		if !uuidRE.MatchString(application.ID) || application.ProjectID != target.Environment.Project.ID {
			return nil, ErrInvalid
		}
		if _, duplicate := expected[application.ID]; duplicate {
			return nil, ErrInvalid
		}
		expected[application.ID] = struct{}{}
	}
	seenDeployments := make(map[string]struct{}, len(deployments))
	for _, deployment := range deployments {
		if !uuidRE.MatchString(deployment.ID) || deployment.EnvironmentID != target.Environment.Environment.ID {
			return nil, ErrInvalid
		}
		if _, exists := expected[deployment.ApplicationID]; !exists {
			return nil, ErrInvalid
		}
		if _, duplicate := seenDeployments[deployment.ID]; duplicate {
			return nil, ErrInvalid
		}
		seenDeployments[deployment.ID] = struct{}{}
	}
	return expected, nil
}

func classifyRegistryEligibilityError(err error) error {
	if errors.Is(err, gitprojection.ErrInvalid) || errors.Is(err, imagepull.ErrInvalid) {
		return ErrInvalid
	}
	if errors.Is(err, gitprojection.ErrConflict) || errors.Is(err, imagepull.ErrConflict) {
		return ErrConflict
	}
	return classifyPostgres(err)
}

var _ DesiredStateRegistryEligibilityResolver = (*PostgreSQLRegistryEligibilityResolver)(nil)
