package helmapps

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ApprovalAdmissionRequest contains only immutable chart coordinates and
// digests plus the authenticated platform actor/idempotency identity. Schema
// and default-values documents are intentionally absent: they are extracted
// from the verified package by the service.
type ApprovalAdmissionRequest struct {
	ActorID            string
	IdempotencyKey     string
	Source             ChartSource
	OCIRepository      string
	ChartVersion       string
	ManifestDigest     string
	PackageDigest      string
	ValuesSchemaDigest string
}

func (r ApprovalAdmissionRequest) validate() error {
	source, err := r.canonicalSource()
	if !uuidRE.MatchString(r.ActorID) || !idempotencyRE.MatchString(r.IdempotencyKey) ||
		err != nil || !optionalDigest(r.ValuesSchemaDigest) ||
		!optionalDigest(r.ManifestDigest) || !optionalDigest(r.PackageDigest) ||
		(source.Kind == ChartSourceKindOCI && (!validDigest(r.ManifestDigest) || !validDigest(r.PackageDigest))) {
		return ErrInvalid
	}
	_, _, err = source.ChartIdentity()
	return err
}

func (r ApprovalAdmissionRequest) canonicalSource() (ChartSource, error) {
	if r.Source.Kind != "" || r.Source.OCI != nil || r.Source.HelmRepository != nil || r.Source.Git != nil {
		if r.Source.Validate() != nil {
			return ChartSource{}, ErrInvalid
		}
		return cloneChartSource(r.Source), nil
	}
	source := ChartSource{Kind: ChartSourceKindOCI, OCI: &OCIChartSource{
		Repository: r.OCIRepository, Version: r.ChartVersion, Digest: r.ManifestDigest,
	}}
	if source.Validate() != nil {
		return ChartSource{}, ErrInvalid
	}
	return source, nil
}

type ApprovalAdmissionStore interface {
	ApprovalAdmission(context.Context, string, string) (ApprovalDocument, error)
	AdmitApproval(context.Context, ApprovalDocument) (ApprovalDocument, bool, error)
	ApprovalAdmissionCatalog(context.Context, int) ([]ApprovalDocument, error)
}

// Catalog is intentionally independent of renderer, publisher, and Argo
// readiness. It exposes only already-admitted immutable approval documents.
func (s ApprovalAdmissionService) Catalog(ctx context.Context, limit int) ([]ApprovalDocument, error) {
	if ctx == nil || s.Store == nil || limit < 1 || limit > 100 {
		return nil, ErrInvalid
	}
	return s.Store.ApprovalAdmissionCatalog(ctx, limit)
}

type ApprovalAdmissionService struct {
	Store    ApprovalAdmissionStore
	Packages ChartPackageSource
	Now      func() time.Time
	NewID    func() string
}

func (s ApprovalAdmissionService) Validate() error {
	if s.Store == nil || s.Packages == nil || s.Now == nil || s.NewID == nil {
		return ErrInvalid
	}
	return nil
}

func (s ApprovalAdmissionService) Admit(ctx context.Context, request ApprovalAdmissionRequest) (ApprovalDocument, bool, error) {
	if ctx == nil || s.Validate() != nil || request.validate() != nil {
		return ApprovalDocument{}, false, ErrInvalid
	}
	stored, replayErr := s.Store.ApprovalAdmission(ctx, request.ActorID, request.IdempotencyKey)
	if replayErr == nil {
		if !approvalAdmissionRequestMatches(stored.Approval, request) {
			return ApprovalDocument{}, false, ErrConflict
		}
		return cloneApprovalDocument(stored), true, nil
	}
	if !errors.Is(replayErr, ErrNotFound) {
		return ApprovalDocument{}, false, replayErr
	}
	now := s.Now().UTC()
	source, _ := request.canonicalSource()
	chartName, chartVersion, _ := source.ChartIdentity()
	repository := source.DisplayRepository()
	if source.Kind != ChartSourceKindOCI {
		repository = syntheticChartRepository(source.Kind, chartName)
	}
	manifestDigest, packageDigest, schemaDigest := request.ManifestDigest, request.PackageDigest, request.ValuesSchemaDigest
	if manifestDigest == "" {
		if source.Kind == ChartSourceKindGit {
			manifestDigest, _ = digestJSON(source)
		} else {
			manifestDigest = unknownChartDigest
		}
	}
	if packageDigest == "" {
		packageDigest = unknownChartDigest
	}
	if schemaDigest == "" {
		schemaDigest = unknownChartDigest
	}
	approval := Approval{ApprovalKey: ApprovalKey{ID: s.NewID(), Revision: 1},
		Source: source, OCIRepository: repository, ChartVersion: chartVersion,
		ManifestDigest: manifestDigest, PackageDigest: packageDigest,
		ValuesSchemaDigest: schemaDigest, RendererImage: RendererImage,
		RendererVersion: HelmVersion, PolicyVersion: PolicyVersion, CreatedBy: request.ActorID,
		IdempotencyKey: request.IdempotencyKey, CreatedAt: now}
	if approval.Validate() != nil {
		return ApprovalDocument{}, false, ErrInvalid
	}
	artifact, err := s.Packages.Fetch(ctx, approval)
	if err != nil {
		return ApprovalDocument{}, false, err
	}
	if (request.ManifestDigest != "" && artifact.ManifestDigest != request.ManifestDigest) ||
		(request.PackageDigest != "" && artifact.PackageDigest != request.PackageDigest) ||
		digestBytes(artifact.PackageBytes) != artifact.PackageDigest {
		return ApprovalDocument{}, false, ErrUnsafeChart
	}
	approval.ManifestDigest, approval.PackageDigest = artifact.ManifestDigest, artifact.PackageDigest
	discoveredSchemaDigest, discoverErr := chartPackageValuesSchemaDigest(artifact.PackageBytes, chartName)
	if discoverErr != nil || request.ValuesSchemaDigest != "" && request.ValuesSchemaDigest != discoveredSchemaDigest {
		return ApprovalDocument{}, false, ErrUnsafeChart
	}
	approval.ValuesSchemaDigest = discoveredSchemaDigest
	if approval.Validate() != nil {
		return ApprovalDocument{}, false, ErrUnsafeChart
	}
	bundle, err := InspectChartPackage(approval, artifact.PackageBytes)
	if err != nil {
		return ApprovalDocument{}, false, err
	}
	documentsDigest, err := approvalDocumentsDigest(approval.ApprovalKey,
		bundle.ValuesSchemaJSON, bundle.DefaultValuesYAML)
	if err != nil {
		return ApprovalDocument{}, false, err
	}
	document := ApprovalDocument{Approval: approval,
		ValuesSchemaJSON:  append([]byte(nil), bundle.ValuesSchemaJSON...),
		DefaultValuesYAML: append([]byte(nil), bundle.DefaultValuesYAML...),
		PackageBytes:      append([]byte(nil), artifact.PackageBytes...),
		DocumentsDigest:   documentsDigest, CreatedAt: now}
	if document.Validate() != nil {
		return ApprovalDocument{}, false, ErrInvalid
	}
	return s.Store.AdmitApproval(ctx, document)
}

type PostgresApprovalAdmissionStore struct{ pool *pgxpool.Pool }

func NewPostgresApprovalAdmissionStore(pool *pgxpool.Pool) (*PostgresApprovalAdmissionStore, error) {
	if pool == nil {
		return nil, ErrInvalid
	}
	return &PostgresApprovalAdmissionStore{pool: pool}, nil
}

func (s *PostgresApprovalAdmissionStore) ApprovalAdmission(ctx context.Context,
	actorID, idempotencyKey string) (ApprovalDocument, error) {
	if !uuidRE.MatchString(actorID) || !idempotencyRE.MatchString(idempotencyKey) {
		return ApprovalDocument{}, ErrInvalid
	}
	document, err := scanApprovalDocument(s.pool.QueryRow(ctx, approvalDocumentSelect+`
		WHERE a.created_by=$1 AND a.idempotency_key=$2`, actorID, idempotencyKey))
	return cloneApprovalDocument(document), classifyPostgres(err)
}

func (s *PostgresApprovalAdmissionStore) ApprovalAdmissionCatalog(ctx context.Context,
	limit int) ([]ApprovalDocument, error) {
	if limit < 1 || limit > 100 {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, approvalDocumentSelect+`
		ORDER BY a.created_at DESC,a.approval_id,a.revision DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]ApprovalDocument, 0, limit)
	for rows.Next() {
		document, scanErr := scanApprovalDocument(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, cloneApprovalDocument(document))
	}
	return result, rows.Err()
}

func (s *PostgresApprovalAdmissionStore) AdmitApproval(ctx context.Context,
	document ApprovalDocument) (ApprovalDocument, bool, error) {
	if document.Validate() != nil || len(document.PackageBytes) == 0 ||
		digestBytes(document.PackageBytes) != document.Approval.PackageDigest {
		return ApprovalDocument{}, false, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ApprovalDocument{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		document.Approval.CreatedBy+":"+document.Approval.IdempotencyKey); err != nil {
		return ApprovalDocument{}, false, err
	}
	stored, scanErr := scanApprovalDocument(tx.QueryRow(ctx, approvalDocumentSelect+`
		WHERE a.created_by=$1 AND a.idempotency_key=$2 FOR UPDATE OF a`,
		document.Approval.CreatedBy, document.Approval.IdempotencyKey))
	if scanErr == nil {
		if !approvalDocumentReplayEqual(stored, document) {
			return ApprovalDocument{}, false, ErrConflict
		}
		if err = tx.Commit(ctx); err != nil {
			return ApprovalDocument{}, false, classifyPostgres(err)
		}
		return cloneApprovalDocument(stored), true, nil
	}
	if !errors.Is(scanErr, pgx.ErrNoRows) {
		return ApprovalDocument{}, false, classifyPostgres(scanErr)
	}
	identityDigest, _ := document.Approval.IdentityDigest()
	source, _ := document.Approval.CanonicalSource()
	sourceJSON, _ := marshalChartSource(source)
	chartName, _, _ := source.ChartIdentity()
	_, err = tx.Exec(ctx, `INSERT INTO helm_chart_approvals(
		approval_id,revision,source_kind,source_json,chart_name,oci_repository,chart_version,manifest_digest,package_digest,
		values_schema_digest,renderer_image,renderer_version,policy_version,identity_digest,
		created_by,idempotency_key,created_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
		document.Approval.ID, document.Approval.Revision, string(source.Kind), sourceJSON, chartName, document.Approval.OCIRepository,
		document.Approval.ChartVersion, document.Approval.ManifestDigest, document.Approval.PackageDigest,
		document.Approval.ValuesSchemaDigest, document.Approval.RendererImage,
		document.Approval.RendererVersion, document.Approval.PolicyVersion, identityDigest,
		document.Approval.CreatedBy, document.Approval.IdempotencyKey, document.Approval.CreatedAt)
	if err != nil {
		return ApprovalDocument{}, false, classifyPostgres(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO helm_chart_approval_documents(
		approval_id,approval_revision,values_schema_json,default_values_yaml,package_bytes,
		values_schema_digest,documents_digest,created_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, document.Approval.ID, document.Approval.Revision,
		document.ValuesSchemaJSON, document.DefaultValuesYAML, document.PackageBytes,
		document.Approval.ValuesSchemaDigest, document.DocumentsDigest, document.CreatedAt)
	if err != nil {
		return ApprovalDocument{}, false, classifyPostgres(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return ApprovalDocument{}, false, classifyPostgres(err)
	}
	return cloneApprovalDocument(document), false, nil
}

func approvalDocumentReplayEqual(left, right ApprovalDocument) bool {
	leftSource, leftSourceErr := left.Approval.CanonicalSource()
	rightSource, rightSourceErr := right.Approval.CanonicalSource()
	leftSourceJSON, leftJSONErr := marshalChartSource(leftSource)
	rightSourceJSON, rightJSONErr := marshalChartSource(rightSource)
	return leftSourceErr == nil && rightSourceErr == nil && leftJSONErr == nil && rightJSONErr == nil &&
		equalBytes(leftSourceJSON, rightSourceJSON) &&
		left.Approval.OCIRepository == right.Approval.OCIRepository &&
		left.Approval.ChartVersion == right.Approval.ChartVersion &&
		left.Approval.ManifestDigest == right.Approval.ManifestDigest &&
		left.Approval.PackageDigest == right.Approval.PackageDigest &&
		left.Approval.ValuesSchemaDigest == right.Approval.ValuesSchemaDigest &&
		left.Approval.RendererImage == right.Approval.RendererImage &&
		left.Approval.RendererVersion == right.Approval.RendererVersion &&
		left.Approval.PolicyVersion == right.Approval.PolicyVersion &&
		left.Approval.CreatedBy == right.Approval.CreatedBy &&
		left.Approval.IdempotencyKey == right.Approval.IdempotencyKey &&
		equalBytes(left.ValuesSchemaJSON, right.ValuesSchemaJSON) &&
		equalBytes(left.DefaultValuesYAML, right.DefaultValuesYAML) &&
		(len(left.PackageBytes) == 0 || len(right.PackageBytes) == 0 || equalBytes(left.PackageBytes, right.PackageBytes))
}

func approvalAdmissionRequestMatches(approval Approval, request ApprovalAdmissionRequest) bool {
	approvalSource, approvalSourceErr := approval.CanonicalSource()
	requestSource, requestSourceErr := request.canonicalSource()
	approvalSourceJSON, approvalJSONErr := marshalChartSource(approvalSource)
	requestSourceJSON, requestJSONErr := marshalChartSource(requestSource)
	return approval.Validate() == nil && approval.CreatedBy == request.ActorID &&
		approvalSourceErr == nil && requestSourceErr == nil && approvalJSONErr == nil && requestJSONErr == nil &&
		equalBytes(approvalSourceJSON, requestSourceJSON) && approval.IdempotencyKey == request.IdempotencyKey &&
		(request.ManifestDigest == "" || approval.ManifestDigest == request.ManifestDigest) &&
		(request.PackageDigest == "" || approval.PackageDigest == request.PackageDigest) &&
		(request.ValuesSchemaDigest == "" || approval.ValuesSchemaDigest == request.ValuesSchemaDigest)
}
