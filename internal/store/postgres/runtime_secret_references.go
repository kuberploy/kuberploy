package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/secrets"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func runtimeAndImageFromExactAppConfig(raw, expectedHash []byte) (domain.WorkloadRuntime, string, error) {
	_, runtime, image, err := appConfigMaterialFromExactAppConfig(raw, expectedHash)
	return runtime, image, err
}

func appConfigMaterialFromExactAppConfig(raw, expectedHash []byte) (map[string]any, domain.WorkloadRuntime, string, error) {
	if len(raw) == 0 {
		return nil, domain.WorkloadRuntime{}, "", base.ErrPreconditionFailed
	}
	digest := sha256.Sum256(raw)
	if len(expectedHash) != sha256.Size || !bytes.Equal(digest[:], expectedHash) {
		return nil, domain.WorkloadRuntime{}, "", base.ErrPreconditionFailed
	}
	parsed, runtime, diagnostics := appconfig.ParseAndValidate(raw)
	if len(diagnostics) != 0 || len(domain.ValidateWorkloadRuntime(runtime)) != 0 {
		return nil, domain.WorkloadRuntime{}, "", base.ErrPreconditionFailed
	}
	image, ok := appconfig.MaterializedImage(parsed)
	if !ok {
		return nil, domain.WorkloadRuntime{}, "", base.ErrPreconditionFailed
	}
	return parsed, runtime, image, nil
}

// validateRuntimeSecretReferencesTx re-resolves the immutable AppConfig's
// safe binding metadata under locks in the caller's transaction. It also
// repeats secrets.bind authorization at the durable boundary. No provider
// payload, ciphertext, base64 value, or Kubernetes object enters this path.
func validateRuntimeSecretReferencesTx(
	ctx context.Context,
	tx pgx.Tx,
	actor string,
	referencePlan *base.AppConfigReferencePlan,
	projectID, environmentID, applicationID string,
	runtime domain.WorkloadRuntime,
	middlewareRefs []domain.SecretBindingRef,
) (secrets.BindingReferencePlan, error) {
	if tx == nil || referencePlan == nil || referencePlan.Validate() != nil || len(domain.ValidateWorkloadRuntime(runtime)) != 0 {
		return secrets.BindingReferencePlan{}, base.ErrPreconditionFailed
	}
	var organizationID string
	var namespace string
	err := tx.QueryRow(ctx, `SELECT COALESCE(p.team_id::text,''),e.namespace
		FROM projects p
		JOIN environments e ON e.project_id=p.id AND e.id=$2
		JOIN applications a ON a.project_id=p.id AND a.id=$3
		WHERE p.id=$1
		FOR SHARE OF p,e,a`, projectID, environmentID, applicationID).Scan(&organizationID, &namespace)
	if err != nil {
		return secrets.BindingReferencePlan{}, classify(err)
	}
	scope := secrets.Scope{OrganizationID: organizationID, ProjectID: projectID, EnvironmentID: environmentID,
		ApplicationID: applicationID, Namespace: namespace}
	catalog, err := secrets.NewPostgreSQLBindingReferenceCatalogTx(tx)
	if err != nil {
		return secrets.BindingReferencePlan{}, err
	}
	resolved, err := secrets.ResolveAppConfigBindingReferences(ctx, catalog, scope, runtime, middlewareRefs)
	if err != nil {
		return secrets.BindingReferencePlan{}, classifyRuntimeSecretReferenceError(err)
	}
	digest, err := resolved.Digest()
	if err != nil || digest != referencePlan.RuntimeSecretDigest {
		return secrets.BindingReferencePlan{}, base.ErrPreconditionFailed
	}
	authorized := map[string]struct{}{}
	for _, use := range resolved.Uses {
		if _, exists := authorized[use.BindingID]; exists {
			continue
		}
		target := domain.AccessTarget{Type: "secret-binding", ID: use.BindingID, TeamID: scope.OrganizationID,
			ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID, Namespace: scope.Namespace,
			ApplicationID: scope.ApplicationID}
		if err = authorizeWith(ctx, tx, actor, domain.PermissionSecretsBind, target); err != nil {
			return secrets.BindingReferencePlan{}, err
		}
		authorized[use.BindingID] = struct{}{}
	}
	return resolved, nil
}

func replaceRuntimeSecretReferencesTx(
	ctx context.Context,
	tx pgx.Tx,
	actor string,
	referencePlan *base.AppConfigReferencePlan,
	binding gitprojection.Binding,
	projectID, environmentID, applicationID string,
	runtime domain.WorkloadRuntime,
	middlewareRefs []domain.SecretBindingRef,
	raw []byte,
	requestID string,
	now time.Time,
) error {
	if referencePlan == nil {
		return removeRuntimeSecretReferencesTx(ctx, tx, actor, binding, projectID, environmentID, applicationID, raw, requestID, now)
	}
	resolved, err := validateRuntimeSecretReferencesTx(ctx, tx, actor, referencePlan, projectID, environmentID, applicationID, runtime, middlewareRefs)
	if err != nil {
		return err
	}
	referenceID, err := gitprojection.ApplicationPath(binding, applicationID)
	if err != nil || len(raw) == 0 || now.IsZero() {
		return base.ErrPreconditionFailed
	}
	digest := sha256.Sum256(raw)
	revision := "sha256:" + hex.EncodeToString(digest[:])
	if err = secrets.ReplaceGitCurrentReferencesTx(ctx, tx, resolved, actor, referenceID, revision, requestID, now.UTC()); err != nil {
		return classifyRuntimeSecretReferenceError(err)
	}
	return nil
}

// removeRuntimeSecretReferencesTx closes the last-reference transition. An
// ordinary AppConfig needs no resolver plan, but a Git write must still remove
// any prior Git-current deletion guards atomically. Platform-owned projects
// have no organization scope and therefore can only take the proven-empty
// no-op path; unexpected rows fail closed instead of being orphaned or deleted.
func removeRuntimeSecretReferencesTx(
	ctx context.Context,
	tx pgx.Tx,
	actor string,
	binding gitprojection.Binding,
	projectID, environmentID, applicationID string,
	raw []byte,
	requestID string,
	now time.Time,
) error {
	if tx == nil || len(raw) == 0 || now.IsZero() {
		return base.ErrPreconditionFailed
	}
	referenceID, err := gitprojection.ApplicationPath(binding, applicationID)
	if err != nil {
		return base.ErrPreconditionFailed
	}
	var organizationID string
	var namespace string
	if err = tx.QueryRow(ctx, `SELECT COALESCE(p.team_id::text,''),e.namespace
		FROM projects p
		JOIN environments e ON e.project_id=p.id AND e.id=$2
		JOIN applications a ON a.project_id=p.id AND a.id=$3
		WHERE p.id=$1
		FOR SHARE OF p,e,a`, projectID, environmentID, applicationID).Scan(&organizationID, &namespace); err != nil {
		return classify(err)
	}
	rows, err := tx.Query(ctx, `SELECT COALESCE(b.organization_id::text,''),b.project_id::text,b.environment_id::text,b.application_id::text,b.target_namespace
		FROM secret_binding_references r
		JOIN secret_bindings b ON b.id=r.binding_id
		WHERE r.kind='git-current' AND r.reference_id=$1
		ORDER BY b.id
		FOR UPDATE OF r,b`, referenceID)
	if err != nil {
		return classify(err)
	}
	found := false
	for rows.Next() {
		found = true
		var rowOrganization, rowProject, rowEnvironment, rowApplication, rowNamespace string
		if err = rows.Scan(&rowOrganization, &rowProject, &rowEnvironment, &rowApplication, &rowNamespace); err != nil {
			rows.Close()
			return classify(err)
		}
		if rowOrganization != organizationID || rowProject != projectID ||
			rowEnvironment != environmentID || rowApplication != applicationID || rowNamespace != namespace {
			rows.Close()
			return base.ErrPreconditionFailed
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return classify(err)
	}
	rows.Close()
	if !found {
		return nil
	}
	digest := sha256.Sum256(raw)
	plan := secrets.BindingReferencePlan{Scope: secrets.Scope{OrganizationID: organizationID, ProjectID: projectID,
		EnvironmentID: environmentID, ApplicationID: applicationID, Namespace: namespace}, Uses: []secrets.ResolvedBindingReference{}}
	if err = secrets.ReplaceGitCurrentReferencesTx(ctx, tx, plan, actor, referenceID,
		"sha256:"+hex.EncodeToString(digest[:]), requestID, now.UTC()); err != nil {
		return classifyRuntimeSecretReferenceError(err)
	}
	return nil
}

func classifyRuntimeSecretReferenceError(err error) error {
	if errors.Is(err, secrets.ErrInvalid) || errors.Is(err, secrets.ErrNotFound) || errors.Is(err, secrets.ErrConflict) ||
		errors.Is(err, secrets.ErrNotReady) || errors.Is(err, secrets.ErrProviderMismatch) {
		return base.ErrPreconditionFailed
	}
	return err
}
