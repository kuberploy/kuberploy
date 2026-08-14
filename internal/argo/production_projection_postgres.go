package argo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/imagepull"
)

// PostgreSQLDesiredStateProjectionGate reconstructs approval exclusively from
// the exact active projection generation. The injected transaction-aware
// policy is the same closed composite used during projection activation, so
// edge, custom-certificate, external-DNS, runtime-secret, and registry-policy
// eligibility cannot be replaced with caller booleans at Argo claim time.
type PostgreSQLDesiredStateProjectionGate struct {
	pool                *pgxpool.Pool
	policy              gitprojection.PostgreSQLAppConfigPolicyValidator
	registryEligibility DesiredStateRegistryEligibilityResolver
	policyDigest        string
	now                 func() time.Time
}

func NewPostgreSQLDesiredStateProjectionGate(
	pool *pgxpool.Pool,
	policy gitprojection.PostgreSQLAppConfigPolicyValidator,
	registryEligibility DesiredStateRegistryEligibilityResolver,
	policyDigest string,
) (*PostgreSQLDesiredStateProjectionGate, error) {
	if pool == nil || policy == nil || registryEligibility == nil || !digestRE.MatchString(policyDigest) {
		return nil, ErrInvalid
	}
	return &PostgreSQLDesiredStateProjectionGate{pool: pool, policy: policy, registryEligibility: registryEligibility,
		policyDigest: policyDigest}, nil
}

func (g *PostgreSQLDesiredStateProjectionGate) ApproveDesiredStateProjection(ctx context.Context, target DesiredStateTarget) (DesiredStateProjectionApproval, error) {
	if g == nil || g.pool == nil || g.policy == nil || !digestRE.MatchString(g.policyDigest) || target.Validate() != nil ||
		target.Environment.Binding.CredentialMode != gitprojection.CredentialGitHubApp {
		return DesiredStateProjectionApproval{}, ErrInvalid
	}
	tx, err := g.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return DesiredStateProjectionApproval{}, classifyPostgres(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	approval, err := g.approveActiveTx(ctx, tx, target, g.currentTime())
	if err != nil {
		return DesiredStateProjectionApproval{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return DesiredStateProjectionApproval{}, classifyPostgres(err)
	}
	return approval, nil
}

func (g *PostgreSQLDesiredStateProjectionGate) ValidateDesiredStateClaim(ctx context.Context, command DesiredStateCommand, mode DesiredStateClaimMode) error {
	if g == nil || g.pool == nil || g.registryEligibility == nil || command.Validate() != nil ||
		(mode != DesiredStateClaimActive && mode != DesiredStateClaimRecovery) {
		return ErrInvalid
	}
	// Once the write-base receipt is durable, the exact Git mutation may already
	// have succeeded even when its acknowledgement did not reach PostgreSQL.
	// Recovery therefore authenticates the immutable command and its retained
	// projection receipt without consulting the newer mutable active generation.
	if mode == DesiredStateClaimRecovery && command.WriteBaseRevision != "" &&
		(command.State == DesiredStateClaimed || command.State == DesiredStateGitCommitted) {
		return g.validateRecoveryReceipt(ctx, command)
	}
	target, err := g.targetForActiveCommand(ctx, command)
	if err != nil {
		return err
	}
	approval, err := g.ApproveDesiredStateProjection(ctx, target)
	if err != nil {
		return err
	}
	if approval.Contract != DesiredStateProjectionApprovalContract || approval.BindingID != command.EnvironmentBindingID ||
		approval.IndexedRevision != command.EnvironmentRevision || approval.ProjectionGeneration != command.EnvironmentGeneration ||
		approval.CatalogDigest != command.CatalogDigest || !approval.AppConfigsValid || !approval.DependenciesValid ||
		!approval.SecretReferencesResolved || approval.RegistryReferencesResolved {
		return ErrInvalid
	}
	content, err := RenderEnvironment(target, approval.Applications, approval.Deployments)
	if err != nil || contentDigest(content) != command.ContentSHA256 || !constantTimeBytesEqual(content, command.Content) {
		return ErrInvalid
	}
	resolved, err := g.registryEligibility.ResolveRegistryReferences(ctx, target, approval, g.currentTime())
	if err != nil {
		return err
	}
	if !resolved {
		return ErrRegistryReferencesNotReady
	}
	return nil
}

func (g *PostgreSQLDesiredStateProjectionGate) productionDesiredStateClaimGate() {}

func (g *PostgreSQLDesiredStateProjectionGate) currentTime() time.Time {
	if g.now != nil {
		return g.now().UTC()
	}
	return time.Now().UTC()
}

func (g *PostgreSQLDesiredStateProjectionGate) approveActiveTx(ctx context.Context, tx pgx.Tx, target DesiredStateTarget, now time.Time) (DesiredStateProjectionApproval, error) {
	binding := target.Environment.Binding
	var project domain.Project
	var environment domain.Environment
	var teamID *string
	var generationStarted, generationActivated time.Time
	err := tx.QueryRow(ctx, `SELECT p.id::text,p.name,p.slug,p.team_id::text,p.created_at,
		e.id::text,e.project_id::text,e.name,e.slug,e.namespace,e.argo_project,e.created_at,
		g.started_at,g.activated_at
	FROM git_repository_bindings b
	JOIN git_projection_generations g ON g.binding_id=b.id AND g.generation=b.projection_generation
	JOIN projects p ON p.id=b.project_id
	JOIN environments e ON e.id=b.environment_id AND e.project_id=p.id
	WHERE b.id=$1 AND b.kind='environment' AND b.scope_id=$2 AND b.project_id=$3 AND b.environment_id=$4
	  AND b.provider=$5 AND b.installation_id=$6 AND b.repository_id=$7
	  AND b.repository_owner=$8 AND b.repository_name=$9 AND b.target_ref=$10 AND b.path_prefix=$11
	  AND b.credential_mode='github-app' AND b.credential_secret_name=''
	  AND b.state='ready' AND b.target_head_revision=$12 AND b.indexed_revision=$12
	  AND b.projection_generation=$13 AND b.parser_version=$14
	  AND g.state='active' AND g.head_revision=$12 AND g.parser_version=$14
	  AND e.namespace=$15 AND e.argo_project=$16
	FOR SHARE OF b,g,p,e`, binding.ID, binding.ScopeID, binding.ProjectID, binding.EnvironmentID,
		binding.Repository.Provider, binding.Repository.InstallationID, binding.Repository.RepositoryID,
		binding.Repository.Owner, binding.Repository.Name, binding.TargetRef, binding.Prefix, binding.IndexedRevision,
		binding.ProjectionGeneration, binding.ParserVersion, target.Environment.Environment.Namespace,
		target.Environment.Environment.ArgoProject).Scan(
		&project.ID, &project.Name, &project.Slug, &teamID, &project.CreatedAt,
		&environment.ID, &environment.ProjectID, &environment.Name, &environment.Slug, &environment.Namespace,
		&environment.ArgoProject, &environment.CreatedAt, &generationStarted, &generationActivated,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return DesiredStateProjectionApproval{}, ErrConflict
	}
	if err != nil {
		return DesiredStateProjectionApproval{}, classifyPostgres(err)
	}
	if teamID != nil {
		project.TeamID = *teamID
	}
	if project != target.Environment.Project || environment != target.Environment.Environment {
		return DesiredStateProjectionApproval{}, ErrConflict
	}
	documents, err := desiredStatePolicyDocumentsTx(ctx, tx, binding)
	if err != nil {
		return DesiredStateProjectionApproval{}, err
	}
	generation := gitprojection.Generation{BindingID: binding.ID, Number: binding.ProjectionGeneration,
		HeadRevision: binding.IndexedRevision, ParserVersion: binding.ParserVersion, State: gitprojection.ProjectionStaging,
		StartedAt: generationStarted.UTC()}
	input := gitprojection.AppConfigPolicyInput{Binding: binding, Generation: generation, Current: documents, Previous: []gitprojection.Document{}}
	if input.Validate() != nil {
		return DesiredStateProjectionApproval{}, ErrConflict
	}
	validation, err := g.policy.ValidateAppConfigsTx(ctx, tx, input, now.UTC())
	if err != nil || validation.ValidateFor(input) != nil {
		if err != nil {
			if errors.Is(err, imagepull.ErrConflict) {
				return DesiredStateProjectionApproval{}, ErrConflict
			}
			if errors.Is(err, imagepull.ErrInvalid) {
				return DesiredStateProjectionApproval{}, ErrInvalid
			}
			return DesiredStateProjectionApproval{}, classifyPostgres(err)
		}
		return DesiredStateProjectionApproval{}, ErrConflict
	}
	if desiredStatePolicyValidationReady(validation) != nil {
		return DesiredStateProjectionApproval{}, ErrConflict
	}
	applications, deployments, err := desiredStateResourcesTx(ctx, tx, project.ID, environment.ID, documents)
	if err != nil {
		return DesiredStateProjectionApproval{}, err
	}
	digest, err := desiredStateCatalogDigest(g.policyDigest, binding, documents, applications, deployments, generationActivated)
	if err != nil {
		return DesiredStateProjectionApproval{}, err
	}
	return DesiredStateProjectionApproval{Contract: DesiredStateProjectionApprovalContract, BindingID: binding.ID,
		IndexedRevision: binding.IndexedRevision, ProjectionGeneration: binding.ProjectionGeneration, CatalogDigest: digest,
		Applications: applications, Deployments: deployments, AppConfigsValid: true, DependenciesValid: true,
		SecretReferencesResolved: true, RegistryReferencesResolved: false}, nil
}

func desiredStatePolicyValidationReady(validation gitprojection.AppConfigPolicyValidation) error {
	for _, diagnostics := range validation.Diagnostics {
		if len(diagnostics) != 0 {
			return ErrConflict
		}
	}
	return nil
}

func desiredStatePolicyDocumentsTx(ctx context.Context, tx pgx.Tx, binding gitprojection.Binding) ([]gitprojection.Document, error) {
	rows, err := tx.Query(ctx, `SELECT path,COALESCE(application_id::text,''),source_revision,config_revision,blob_id,
		content_sha256,raw,valid,diagnostics,schema_version,parser_version,indexed_at
	FROM git_projected_documents
	WHERE binding_id=$1 AND generation=$2
	ORDER BY path
	FOR SHARE`, binding.ID, binding.ProjectionGeneration)
	if err != nil {
		return nil, classifyPostgres(err)
	}
	defer rows.Close()
	documents := make([]gitprojection.Document, 0)
	for rows.Next() {
		if len(documents) >= maximumDesiredStatePolicyDocuments {
			return nil, ErrInvalid
		}
		document := gitprojection.Document{BindingID: binding.ID, Generation: binding.ProjectionGeneration}
		var diagnosticsJSON []byte
		if err = rows.Scan(&document.Path, &document.ApplicationID, &document.SourceRevision, &document.ConfigRevision,
			&document.BlobID, &document.ContentSHA256, &document.Raw, &document.Valid, &diagnosticsJSON,
			&document.SchemaVersion, &document.ParserVersion, &document.IndexedAt); err != nil {
			return nil, classifyPostgres(err)
		}
		if err = json.Unmarshal(diagnosticsJSON, &document.Diagnostics); err != nil || document.Diagnostics == nil {
			return nil, ErrConflict
		}
		document, err = revalidateDesiredStatePolicyDocument(binding, document)
		if err != nil {
			return nil, ErrConflict
		}
		documents = append(documents, document)
	}
	if err = rows.Err(); err != nil {
		return nil, classifyPostgres(err)
	}
	return documents, nil
}

// revalidateDesiredStatePolicyDocument separates immutable schema validity
// from dynamic policy observations. Projection activation persists both kinds
// of diagnostics, but runtime observations may recover without another Git
// revision. Reparse the exact indexed AppConfig bytes and let the current
// transaction-aware policy below authoritatively re-evaluate every dynamic
// reference. Dependency documents remain strict because their diagnostics are
// compile-time desired-state failures, not recoverable runtime observations.
func revalidateDesiredStatePolicyDocument(binding gitprojection.Binding, document gitprojection.Document) (gitprojection.Document, error) {
	if document.Validate(binding) != nil || document.SourceRevision != binding.IndexedRevision {
		return gitprojection.Document{}, ErrConflict
	}
	if document.ApplicationID == "" {
		if !document.Valid {
			return gitprojection.Document{}, ErrConflict
		}
		return document, nil
	}
	parsed, _, diagnostics := appconfig.ParseAndValidate(document.Raw)
	if parsed == nil || len(diagnostics) != 0 {
		return gitprojection.Document{}, ErrConflict
	}
	if len(gitprojection.AppConfigBindingDiagnostics(parsed, binding, document.ApplicationID)) != 0 {
		return gitprojection.Document{}, ErrConflict
	}
	document.Parsed = parsed
	document.Valid = true
	document.Diagnostics = []gitprojection.Diagnostic{}
	if document.Validate(binding) != nil {
		return gitprojection.Document{}, ErrConflict
	}
	return document, nil
}

func desiredStateResourcesTx(ctx context.Context, tx pgx.Tx, projectID, environmentID string, documents []gitprojection.Document) ([]domain.Application, []domain.Deployment, error) {
	applicationIDs := make([]string, 0, len(documents))
	configRevisions := make(map[string]string, len(documents))
	for _, document := range documents {
		if document.ApplicationID != "" {
			applicationIDs = append(applicationIDs, document.ApplicationID)
			configRevisions[document.ApplicationID] = document.ConfigRevision
		}
	}
	if len(applicationIDs) == 0 {
		return []domain.Application{}, []domain.Deployment{}, nil
	}
	slices.Sort(applicationIDs)
	for index := 1; index < len(applicationIDs); index++ {
		if applicationIDs[index] == applicationIDs[index-1] {
			return nil, nil, ErrConflict
		}
	}
	rows, err := tx.Query(ctx, `SELECT id::text,project_id::text,name,slug,created_at
		FROM applications WHERE project_id=$1 AND id=ANY($2::uuid[]) ORDER BY id FOR SHARE`, projectID, applicationIDs)
	if err != nil {
		return nil, nil, classifyPostgres(err)
	}
	applications := make([]domain.Application, 0, len(applicationIDs))
	for rows.Next() {
		var application domain.Application
		if err = rows.Scan(&application.ID, &application.ProjectID, &application.Name, &application.Slug, &application.CreatedAt); err != nil {
			rows.Close()
			return nil, nil, classifyPostgres(err)
		}
		applications = append(applications, application)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, nil, classifyPostgres(err)
	}
	rows.Close()
	if len(applications) != len(applicationIDs) {
		return nil, nil, ErrConflict
	}
	rows, err = tx.Query(ctx, `SELECT id::text,environment_id::text,application_id::text
		FROM deployments WHERE environment_id=$1 ORDER BY id FOR SHARE`, environmentID)
	if err != nil {
		return nil, nil, classifyPostgres(err)
	}
	deployments := make([]domain.Deployment, 0, len(applications))
	seenApplications := make(map[string]struct{}, len(applications))
	for rows.Next() {
		var deployment domain.Deployment
		if err = rows.Scan(&deployment.ID, &deployment.EnvironmentID, &deployment.ApplicationID); err != nil {
			rows.Close()
			return nil, nil, classifyPostgres(err)
		}
		if _, found := slices.BinarySearch(applicationIDs, deployment.ApplicationID); !found {
			rows.Close()
			return nil, nil, ErrConflict
		}
		if _, duplicate := seenApplications[deployment.ApplicationID]; duplicate {
			rows.Close()
			return nil, nil, ErrConflict
		}
		deployment.DesiredRevision = configRevisions[deployment.ApplicationID]
		if !commitRE.MatchString(deployment.DesiredRevision) {
			rows.Close()
			return nil, nil, ErrConflict
		}
		seenApplications[deployment.ApplicationID] = struct{}{}
		deployments = append(deployments, deployment)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, nil, classifyPostgres(err)
	}
	rows.Close()
	if len(deployments) != len(applications) {
		return nil, nil, ErrConflict
	}
	return applications, deployments, nil
}

func desiredStateCatalogDigest(policyDigest string, binding gitprojection.Binding, documents []gitprojection.Document,
	applications []domain.Application, deployments []domain.Deployment, activatedAt time.Time) (string, error) {
	if !digestRE.MatchString(policyDigest) || binding.Validate() != nil || activatedAt.IsZero() {
		return "", ErrInvalid
	}
	type documentReceipt struct {
		Path           string `json:"path"`
		ApplicationID  string `json:"applicationId,omitempty"`
		SourceRevision string `json:"sourceRevision"`
		ConfigRevision string `json:"configRevision"`
		BlobID         string `json:"blobId"`
		ContentSHA256  string `json:"contentSha256"`
		SchemaVersion  string `json:"schemaVersion"`
		ParserVersion  string `json:"parserVersion"`
	}
	type deploymentReceipt struct {
		ID            string `json:"id"`
		ApplicationID string `json:"applicationId"`
		EnvironmentID string `json:"environmentId"`
	}
	documentReceipts := make([]documentReceipt, len(documents))
	for index, document := range documents {
		if document.Validate(binding) != nil || !document.Valid {
			return "", ErrInvalid
		}
		documentReceipts[index] = documentReceipt{document.Path, document.ApplicationID, document.SourceRevision,
			document.ConfigRevision, document.BlobID, document.ContentSHA256, document.SchemaVersion, document.ParserVersion}
	}
	deploymentReceipts := make([]deploymentReceipt, len(deployments))
	for index, deployment := range deployments {
		deploymentReceipts[index] = deploymentReceipt{deployment.ID, deployment.ApplicationID, deployment.EnvironmentID}
	}
	canonical := struct {
		Contract      string               `json:"contract"`
		PolicyDigest  string               `json:"policyDigest"`
		BindingID     string               `json:"bindingId"`
		Revision      string               `json:"revision"`
		Generation    int64                `json:"generation"`
		ParserVersion string               `json:"parserVersion"`
		ActivatedAt   time.Time            `json:"activatedAt"`
		Documents     []documentReceipt    `json:"documents"`
		Applications  []domain.Application `json:"applications"`
		Deployments   []deploymentReceipt  `json:"deployments"`
	}{DesiredStateProjectionApprovalContract, policyDigest, binding.ID, binding.IndexedRevision,
		binding.ProjectionGeneration, binding.ParserVersion, activatedAt.UTC(), documentReceipts, applications, deploymentReceipts}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", ErrInvalid
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func (g *PostgreSQLDesiredStateProjectionGate) targetForActiveCommand(ctx context.Context, command DesiredStateCommand) (DesiredStateTarget, error) {
	projectionStore, err := gitprojection.NewPostgreSQLStore(g.pool)
	if err != nil {
		return DesiredStateTarget{}, ErrInvalid
	}
	environmentBinding, err := projectionStore.Binding(ctx, command.EnvironmentBindingID)
	if err != nil {
		return DesiredStateTarget{}, classifyPostgres(err)
	}
	platformBinding, err := projectionStore.Binding(ctx, command.PlatformBindingID)
	if err != nil {
		return DesiredStateTarget{}, classifyPostgres(err)
	}
	var project domain.Project
	var environment domain.Environment
	var teamID *string
	err = g.pool.QueryRow(ctx, `SELECT p.id::text,p.name,p.slug,p.team_id::text,p.created_at,
		e.id::text,e.project_id::text,e.name,e.slug,e.namespace,e.argo_project,e.created_at
	FROM projects p JOIN environments e ON e.project_id=p.id
	WHERE p.id=$1 AND e.id=$2`, command.ProjectID, command.EnvironmentID).Scan(
		&project.ID, &project.Name, &project.Slug, &teamID, &project.CreatedAt,
		&environment.ID, &environment.ProjectID, &environment.Name, &environment.Slug,
		&environment.Namespace, &environment.ArgoProject, &environment.CreatedAt)
	if err != nil {
		return DesiredStateTarget{}, classifyPostgres(err)
	}
	if teamID != nil {
		project.TeamID = *teamID
	}
	target := DesiredStateTarget{Environment: EnvironmentTarget{Project: project, Environment: environment,
		Binding: environmentBinding, ArgoNamespace: command.ArgoNamespace, Runtime: command.Runtime}, PlatformBinding: platformBinding}
	if target.Validate() != nil || environmentBinding.CredentialMode != gitprojection.CredentialGitHubApp ||
		environmentBinding.IndexedRevision != command.EnvironmentRevision || environmentBinding.ProjectionGeneration != command.EnvironmentGeneration ||
		platformBinding.ID != command.PlatformBindingID || platformBinding.ClusterID != command.ClusterID ||
		platformBinding.TargetRef != command.PlatformTargetRef || environmentBinding.TargetRef != command.EnvironmentTargetRef ||
		command.DestinationNamespace != environment.Namespace || command.ArgoProject != environment.ArgoProject {
		return DesiredStateTarget{}, ErrInvalid
	}
	return target, nil
}

func (g *PostgreSQLDesiredStateProjectionGate) validateRecoveryReceipt(ctx context.Context, command DesiredStateCommand) error {
	var valid bool
	err := g.pool.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM argo_desired_state_commands c
		JOIN git_projection_generations generation
		  ON generation.binding_id=c.environment_binding_id AND generation.generation=c.environment_generation
		WHERE c.id=$1 AND c.generation=$2 AND c.project_id=$3 AND c.environment_id=$4
		  AND c.platform_binding_id=$5 AND c.environment_binding_id=$6 AND c.cluster_id=$7
		  AND c.platform_target_ref=$8 AND c.environment_target_ref=$9
		  AND c.environment_revision=$10 AND c.environment_generation=$11 AND c.path=$12
		  AND c.argo_namespace=$13 AND c.destination_namespace=$14 AND c.argo_project=$15
		  AND c.base_revision=$16 AND c.write_base_revision=$17 AND c.write_base_observed_at=$18
		  AND c.precondition=$19 AND c.expected_etag=$20 AND c.catalog_digest=$21
		  AND c.chart_repository=$22 AND c.chart_name=$23 AND c.chart_version=$24
		  AND c.chart_digest=$25 AND c.renderer_image=$26 AND c.chart_digest_enforcement=$27
		  AND c.content=$28 AND c.content_sha256=$29 AND c.message=$30
		  AND c.state=$31 AND c.committed_revision=$32 AND c.write_base_revision<>''
		  AND ((c.state='claimed' AND c.committed_revision='') OR
		       (c.state='git-committed' AND c.committed_revision<>''))
		  AND generation.state='active'
		  AND generation.head_revision=c.environment_revision
		  AND NOT EXISTS(SELECT 1 FROM git_projected_documents document
		    WHERE document.binding_id=c.environment_binding_id AND document.generation=c.environment_generation AND NOT document.valid)
	)`, command.ID, command.Generation, command.ProjectID, command.EnvironmentID,
		command.PlatformBindingID, command.EnvironmentBindingID, command.ClusterID,
		command.PlatformTargetRef, command.EnvironmentTargetRef, command.EnvironmentRevision, command.EnvironmentGeneration,
		command.Path, command.ArgoNamespace, command.DestinationNamespace, command.ArgoProject,
		command.BaseRevision, command.WriteBaseRevision, command.WriteBaseObservedAt,
		command.Precondition, command.ExpectedETag, command.CatalogDigest,
		command.Runtime.ChartRepository, command.Runtime.ChartName, command.Runtime.ChartVersion,
		command.Runtime.ChartDigest, command.Runtime.RendererImage, command.DigestEnforcement,
		command.Content, command.ContentSHA256, command.Message, command.State, command.CommittedRevision).Scan(&valid)
	if err != nil {
		return classifyPostgres(err)
	}
	if !valid {
		return ErrInvalid
	}
	return nil
}

var _ DesiredStateProjectionGate = (*PostgreSQLDesiredStateProjectionGate)(nil)
var _ ProductionDesiredStateClaimGate = (*PostgreSQLDesiredStateProjectionGate)(nil)
