package helmapps

import (
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

const (
	ProtectedCatalogContract       = "helm-approved-catalog.v1"
	MaximumProtectedCatalogEntries = 10_000
)

type ProtectedBindingResolverConfig struct {
	PlatformBindingID string
}

func (c ProtectedBindingResolverConfig) Validate() error {
	if !uuidRE.MatchString(c.PlatformBindingID) {
		return ErrInvalid
	}
	return nil
}

// ProtectedBindingResolutionSnapshot is the complete input to the memory
// resolver. It is intentionally explicit so tests cannot obtain readiness by
// supplying only a plausible binding object while omitting the active
// generation, invalid-document count, application scope, or approved catalog.
type ProtectedBindingResolutionSnapshot struct {
	Platform             gitprojection.Binding
	Environments         []gitprojection.Binding
	ActiveGenerations    []gitprojection.Generation
	InvalidDocumentCount int
	ApplicationProjects  map[string]string
	Catalog              []ApprovalDocument
}

// MemoryProtectedBindingResolver mirrors the PostgreSQL resolver's closed
// validation contract for hermetic tests and local development. Its snapshot
// is copied and revalidated on every read; it is not a production authority.
type MemoryProtectedBindingResolver struct {
	Config   ProtectedBindingResolverConfig
	Snapshot ProtectedBindingResolutionSnapshot
}

func (r *MemoryProtectedBindingResolver) ResolveProtectedBinding(_ context.Context,
	target ReleaseTarget) (ProtectedBindingSnapshot, error) {
	if r == nil {
		return ProtectedBindingSnapshot{}, ErrInvalid
	}
	return resolveProtectedBindingSnapshot(r.Config, target, cloneProtectedResolutionSnapshot(r.Snapshot))
}

type PostgresProtectedBindingResolver struct {
	begin  protectedResolverBeginner
	config ProtectedBindingResolverConfig
}

type protectedResolverBeginner interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}

func NewPostgresProtectedBindingResolver(pool *pgxpool.Pool,
	config ProtectedBindingResolverConfig) (*PostgresProtectedBindingResolver, error) {
	if pool == nil || config.Validate() != nil {
		return nil, ErrInvalid
	}
	return &PostgresProtectedBindingResolver{begin: pool, config: config}, nil
}

func (r *PostgresProtectedBindingResolver) ResolveProtectedBinding(ctx context.Context,
	target ReleaseTarget) (ProtectedBindingSnapshot, error) {
	if r == nil || r.begin == nil || r.config.Validate() != nil || target.Validate() != nil || ctx == nil {
		return ProtectedBindingSnapshot{}, ErrInvalid
	}
	tx, err := r.begin.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ProtectedBindingSnapshot{}, classifyPostgres(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	platform, err := scanProtectedResolutionBinding(tx.QueryRow(ctx, protectedResolutionBindingSelect+` WHERE id=$1`,
		r.config.PlatformBindingID))
	if err != nil {
		return ProtectedBindingSnapshot{}, classifyPostgres(err)
	}
	environmentRows, err := tx.Query(ctx, protectedResolutionBindingSelect+`
		WHERE kind='environment' AND scope_id=$1 ORDER BY id LIMIT 2`, target.EnvironmentID)
	if err != nil {
		return ProtectedBindingSnapshot{}, classifyPostgres(err)
	}
	environments := make([]gitprojection.Binding, 0, 2)
	for environmentRows.Next() {
		binding, scanErr := scanProtectedResolutionBinding(environmentRows)
		if scanErr != nil {
			environmentRows.Close()
			return ProtectedBindingSnapshot{}, classifyPostgres(scanErr)
		}
		environments = append(environments, binding)
	}
	err = environmentRows.Err()
	environmentRows.Close()
	if err != nil {
		return ProtectedBindingSnapshot{}, classifyPostgres(err)
	}

	var applicationProject string
	if err = tx.QueryRow(ctx, `SELECT project_id::text FROM applications WHERE id=$1`,
		target.ApplicationID).Scan(&applicationProject); err != nil {
		return ProtectedBindingSnapshot{}, classifyPostgres(err)
	}

	activeGenerations := make([]gitprojection.Generation, 0, 2)
	invalidDocuments := 0
	if len(environments) == 1 {
		generationRows, queryErr := tx.Query(ctx, `SELECT binding_id::text,generation,head_revision,
			parser_version,state,started_at,activated_at FROM git_projection_generations
			WHERE binding_id=$1 AND generation=$2 AND state='active' LIMIT 2`, environments[0].ID,
			environments[0].ProjectionGeneration)
		if queryErr != nil {
			return ProtectedBindingSnapshot{}, classifyPostgres(queryErr)
		}
		for generationRows.Next() {
			var generation gitprojection.Generation
			if scanErr := generationRows.Scan(&generation.BindingID, &generation.Number, &generation.HeadRevision,
				&generation.ParserVersion, &generation.State, &generation.StartedAt, &generation.ActivatedAt); scanErr != nil {
				generationRows.Close()
				return ProtectedBindingSnapshot{}, classifyPostgres(scanErr)
			}
			activeGenerations = append(activeGenerations, generation)
		}
		err = generationRows.Err()
		generationRows.Close()
		if err != nil {
			return ProtectedBindingSnapshot{}, classifyPostgres(err)
		}
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM git_projected_documents
			WHERE binding_id=$1 AND generation=$2 AND NOT valid`, environments[0].ID,
			environments[0].ProjectionGeneration).Scan(&invalidDocuments); err != nil {
			return ProtectedBindingSnapshot{}, classifyPostgres(err)
		}
	}

	catalogRows, err := tx.Query(ctx, approvalDocumentSelect+`
		ORDER BY a.approval_id,a.revision LIMIT $1`, MaximumProtectedCatalogEntries+1)
	if err != nil {
		return ProtectedBindingSnapshot{}, classifyPostgres(err)
	}
	catalog := make([]ApprovalDocument, 0)
	for catalogRows.Next() {
		document, scanErr := scanApprovalDocument(catalogRows)
		if scanErr != nil {
			catalogRows.Close()
			return ProtectedBindingSnapshot{}, classifyPostgres(scanErr)
		}
		catalog = append(catalog, document)
	}
	err = catalogRows.Err()
	catalogRows.Close()
	if err != nil {
		return ProtectedBindingSnapshot{}, classifyPostgres(err)
	}

	resolved, err := resolveProtectedBindingSnapshot(r.config, target, ProtectedBindingResolutionSnapshot{
		Platform: platform, Environments: environments, ActiveGenerations: activeGenerations,
		InvalidDocumentCount: invalidDocuments, ApplicationProjects: map[string]string{target.ApplicationID: applicationProject},
		Catalog: catalog,
	})
	if err != nil {
		return ProtectedBindingSnapshot{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ProtectedBindingSnapshot{}, classifyPostgres(err)
	}
	return resolved, nil
}

const protectedResolutionBindingSelect = `SELECT id::text,kind,scope_id::text,
	COALESCE(project_id::text,''),COALESCE(environment_id::text,''),
	provider,installation_id,repository_id,repository_owner,repository_name,target_ref,path_prefix,
	credential_mode,credential_secret_name,state,target_head_revision,indexed_revision,projection_generation,
	parser_version,target_head_observed_at,indexed_at,created_at,updated_at FROM git_repository_bindings`

type protectedBindingRow interface{ Scan(...any) error }

func scanProtectedResolutionBinding(row protectedBindingRow) (gitprojection.Binding, error) {
	var binding gitprojection.Binding
	var targetRevision, indexedRevision *string
	var targetAt, indexedAt *time.Time
	err := row.Scan(&binding.ID, &binding.Kind, &binding.ScopeID, &binding.ProjectID,
		&binding.EnvironmentID, &binding.Repository.Provider,
		&binding.Repository.InstallationID, &binding.Repository.RepositoryID,
		&binding.Repository.Owner, &binding.Repository.Name, &binding.TargetRef, &binding.Prefix,
		&binding.CredentialMode, &binding.CredentialSecretName, &binding.State,
		&targetRevision, &indexedRevision, &binding.ProjectionGeneration, &binding.ParserVersion,
		&targetAt, &indexedAt, &binding.CreatedAt, &binding.UpdatedAt)
	if err != nil {
		return gitprojection.Binding{}, err
	}
	if targetRevision != nil {
		binding.TargetHeadRevision = *targetRevision
	}
	if indexedRevision != nil {
		binding.IndexedRevision = *indexedRevision
	}
	if targetAt != nil {
		binding.TargetHeadObservedAt = targetAt.UTC()
	}
	if indexedAt != nil {
		binding.IndexedAt = indexedAt.UTC()
	}
	if binding.Validate() != nil {
		return gitprojection.Binding{}, ErrConflict
	}
	return binding, nil
}

func resolveProtectedBindingSnapshot(config ProtectedBindingResolverConfig, target ReleaseTarget,
	state ProtectedBindingResolutionSnapshot) (ProtectedBindingSnapshot, error) {
	if config.Validate() != nil || target.Validate() != nil || state.Platform.Validate() != nil ||
		state.InvalidDocumentCount < 0 || len(state.Environments) != 1 ||
		len(state.ActiveGenerations) != 1 || state.ApplicationProjects[target.ApplicationID] != target.ProjectID {
		return ProtectedBindingSnapshot{}, ErrConflict
	}
	platform, environment, generation := state.Platform, state.Environments[0], state.ActiveGenerations[0]
	if environment.Validate() != nil || platform.ID != config.PlatformBindingID || platform.Kind != gitprojection.BindingPlatform ||
		platform.ScopeID != platform.ID ||
		platform.CredentialMode != gitprojection.CredentialGitHubApp || platform.CredentialSecretName != "" ||
		(platform.State != gitprojection.BindingReady && platform.State != gitprojection.BindingIndexing) ||
		!gitCommitRE.MatchString(platform.TargetHeadRevision) || platform.TargetHeadObservedAt.IsZero() ||
		environment.Kind != gitprojection.BindingEnvironment || environment.ScopeID != target.EnvironmentID ||
		environment.ProjectID != target.ProjectID || environment.EnvironmentID != target.EnvironmentID ||
		environment.CredentialMode != gitprojection.CredentialGitHubApp || environment.CredentialSecretName != "" ||
		environment.State != gitprojection.BindingReady || environment.TargetHeadRevision == "" ||
		environment.TargetHeadRevision != environment.IndexedRevision || environment.ProjectionGeneration < 1 ||
		environment.TargetHeadObservedAt.IsZero() || environment.IndexedAt.IsZero() ||
		generation.Validate() != nil || generation.State != gitprojection.ProjectionActive ||
		generation.BindingID != environment.ID || generation.Number != environment.ProjectionGeneration ||
		generation.HeadRevision != environment.IndexedRevision || generation.ParserVersion != environment.ParserVersion ||
		state.InvalidDocumentCount != 0 {
		return ProtectedBindingSnapshot{}, ErrUnavailable
	}
	catalogDigest, err := protectedCatalogDigest(state.Catalog)
	if err != nil {
		return ProtectedBindingSnapshot{}, err
	}
	result := ProtectedBindingSnapshot{PlatformBindingID: platform.ID, EnvironmentBindingID: environment.ID,
		PlatformTargetRef:    platform.TargetRef,
		EnvironmentTargetRef: environment.TargetRef, EnvironmentRevision: environment.IndexedRevision,
		EnvironmentGeneration: environment.ProjectionGeneration, CatalogDigest: catalogDigest,
		PlannedBaseRevision: platform.TargetHeadRevision}
	if result.Validate() != nil {
		return ProtectedBindingSnapshot{}, ErrConflict
	}
	return result, nil
}

func protectedCatalogDigest(documents []ApprovalDocument) (string, error) {
	if len(documents) < 1 || len(documents) > MaximumProtectedCatalogEntries {
		return "", ErrUnavailable
	}
	type entry struct {
		ApprovalID      string `json:"approvalId"`
		Revision        int64  `json:"revision"`
		IdentityDigest  string `json:"identityDigest"`
		DocumentsDigest string `json:"documentsDigest"`
	}
	entries := make([]entry, len(documents))
	seen := make(map[ApprovalKey]struct{}, len(documents))
	for index, document := range documents {
		if document.Validate() != nil {
			return "", ErrConflict
		}
		key := document.Approval.ApprovalKey
		if _, exists := seen[key]; exists {
			return "", ErrConflict
		}
		seen[key] = struct{}{}
		identity, err := document.Approval.IdentityDigest()
		if err != nil {
			return "", ErrConflict
		}
		entries[index] = entry{ApprovalID: key.ID, Revision: key.Revision,
			IdentityDigest: identity, DocumentsDigest: document.DocumentsDigest}
	}
	slices.SortFunc(entries, func(left, right entry) int {
		if left.ApprovalID != right.ApprovalID {
			if left.ApprovalID < right.ApprovalID {
				return -1
			}
			return 1
		}
		if left.Revision < right.Revision {
			return -1
		}
		if left.Revision > right.Revision {
			return 1
		}
		return 0
	})
	return digestJSON(struct {
		Contract string  `json:"contract"`
		Entries  []entry `json:"entries"`
	}{Contract: ProtectedCatalogContract, Entries: entries})
}

func cloneProtectedResolutionSnapshot(value ProtectedBindingResolutionSnapshot) ProtectedBindingResolutionSnapshot {
	value.Environments = slices.Clone(value.Environments)
	value.ActiveGenerations = slices.Clone(value.ActiveGenerations)
	projects := value.ApplicationProjects
	value.ApplicationProjects = make(map[string]string, len(value.ApplicationProjects))
	for key, project := range projects {
		value.ApplicationProjects[key] = project
	}
	catalog := value.Catalog
	value.Catalog = make([]ApprovalDocument, len(catalog))
	for index, document := range catalog {
		value.Catalog[index] = cloneApprovalDocument(document)
	}
	return value
}

var _ ProtectedBindingResolver = (*MemoryProtectedBindingResolver)(nil)
var _ ProtectedBindingResolver = (*PostgresProtectedBindingResolver)(nil)
