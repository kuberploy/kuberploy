package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/domain"
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

func classifyRuntimeSecretReferenceError(err error) error {
	if errors.Is(err, secrets.ErrInvalid) || errors.Is(err, secrets.ErrNotFound) || errors.Is(err, secrets.ErrConflict) ||
		errors.Is(err, secrets.ErrNotReady) || errors.Is(err, secrets.ErrProviderMismatch) {
		return base.ErrPreconditionFailed
	}
	return err
}
