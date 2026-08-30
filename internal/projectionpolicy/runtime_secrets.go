package projectionpolicy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/secrets"
)

// RuntimeSecretReferencePolicy resolves only immutable, metadata-only
// SecretBindingRefs and reconciles their Git-current deletion guards in the
// caller's exact projection activation transaction. It never reads provider
// payloads, ciphertext, Kubernetes Secrets, or secret material.
type RuntimeSecretReferencePolicy struct {
	Config secrets.RuntimeConfig
}

func (p *RuntimeSecretReferencePolicy) ValidateCurrentTx(
	ctx context.Context,
	tx pgx.Tx,
	document AppConfigPolicyDocument,
	now time.Time,
) ([]gitprojection.Diagnostic, error) {
	if document.validate() != nil {
		return nil, gitprojection.ErrInvalid
	}
	scope, runtime := document.Scope(), document.Runtime()
	if err := p.validateDocumentIdentity(tx, scope, now); err != nil {
		return nil, err
	}
	middlewareReferences := document.MiddlewareSecretReferences()
	usesReferences := runtimeUsesSecretReferences(runtime) || len(middlewareReferences) != 0
	secretScope, err := p.validateScope(ctx, tx, scope, now)
	if err != nil {
		return nil, err
	}
	if usesReferences && !p.Config.AllowsNamespace(secretScope.Namespace) {
		return []gitprojection.Diagnostic{{
			Code:    "RuntimeSecretNamespaceUnavailable",
			Detail:  "Runtime-secret references are not enabled for this exact destination namespace.",
			Pointer: "/spec/runtime/env",
		}}, nil
	}
	catalog, err := secrets.NewPostgreSQLBindingReferenceCatalogTx(tx)
	if err != nil {
		return nil, err
	}
	plan, err := secrets.ResolveGitCurrentAppConfigBindingReferences(ctx, catalog, secretScope, runtime, middlewareReferences)
	if err != nil {
		if semanticRuntimeSecretReferenceError(err) {
			if secrets.IsMiddlewareReferenceError(err) {
				return []gitprojection.Diagnostic{{Code: "MiddlewareSecretReferenceUnresolved", Detail: "A BasicAuth runtime-secret reference is unavailable, inactive, or not authorized for this exact application destination.", Pointer: "/spec/middlewares"}}, nil
			}
			return []gitprojection.Diagnostic{{
				Code:    "RuntimeSecretReferenceUnresolved",
				Detail:  "A runtime-secret reference is unavailable, inactive, or not authorized for this exact application destination.",
				Pointer: "/spec/runtime/env",
			}}, nil
		}
		return nil, err
	}
	if err = secrets.ReplaceIndexedGitCurrentReferencesTx(ctx, tx, plan, scope.Binding.ID, scope.Path, scope.SourceRevision, scope.ContentSHA256, runtimeSecretPolicyRequestID(scope), now.UTC()); err != nil {
		if semanticRuntimeSecretReferenceError(err) {
			return []gitprojection.Diagnostic{{Code: "RuntimeSecretReferenceUnresolved", Detail: "A runtime-secret reference is unavailable, inactive, or not authorized for this exact application destination.", Pointer: "/spec/runtime/environment"}}, nil
		}
		// The finalizer may already have issued writes before a database error.
		// Returning any error forces the outer activation transaction to roll
		// back, preserving the previous desired state and every deletion guard.
		return nil, err
	}
	return nil, nil
}

func (p *RuntimeSecretReferencePolicy) ReconcileDeletedTx(ctx context.Context, tx pgx.Tx, scope DocumentScope, now time.Time) error {
	if err := p.validateDocumentIdentity(tx, scope, now); err != nil {
		return err
	}
	secretScope, err := p.validateDeletedScope(ctx, tx, scope, now)
	if err != nil {
		return err
	}
	plan := secrets.BindingReferencePlan{Scope: secretScope, Uses: []secrets.ResolvedBindingReference{}}
	return secrets.ReplaceIndexedGitCurrentReferencesTx(ctx, tx, plan, scope.Binding.ID, scope.Path, scope.SourceRevision, scope.ContentSHA256, runtimeSecretPolicyRequestID(scope), now.UTC())
}

func (p *RuntimeSecretReferencePolicy) validateScope(ctx context.Context, tx pgx.Tx, scope DocumentScope, now time.Time) (secrets.Scope, error) {
	if err := p.validateDocumentIdentity(tx, scope, now); err != nil {
		return secrets.Scope{}, err
	}
	// Re-resolve the durable ownership relationship independently of the
	// caller-supplied scope. A NULL team is the authoritative personal-project
	// identity, not an absence of tenant authority.
	var durableOrganizationID *string
	var durableNamespace string
	err := tx.QueryRow(ctx, `SELECT p.team_id::text,e.namespace
		FROM projects p
		JOIN environments e ON e.id=$2 AND e.project_id=p.id
		JOIN applications a ON a.id=$3 AND a.project_id=p.id
		WHERE p.id=$1
		FOR SHARE OF p,e,a`, scope.Binding.ProjectID, scope.Binding.EnvironmentID, scope.ApplicationID).
		Scan(&durableOrganizationID, &durableNamespace)
	if errors.Is(err, pgx.ErrNoRows) {
		return secrets.Scope{}, gitprojection.ErrInvalid
	}
	if err != nil {
		return secrets.Scope{}, err
	}
	expectedOrganizationID := ""
	if durableOrganizationID != nil {
		expectedOrganizationID = *durableOrganizationID
	}
	if scope.OrganizationID != expectedOrganizationID || scope.Namespace != durableNamespace {
		return secrets.Scope{}, gitprojection.ErrInvalid
	}
	secretScope := secrets.Scope{OrganizationID: scope.OrganizationID, ProjectID: scope.Binding.ProjectID,
		EnvironmentID: scope.Binding.EnvironmentID, ApplicationID: scope.ApplicationID, Namespace: scope.Namespace}
	if secretScope.Validate() != nil {
		return secrets.Scope{}, gitprojection.ErrInvalid
	}
	return secretScope, nil
}

func (p *RuntimeSecretReferencePolicy) validateDeletedScope(ctx context.Context, tx pgx.Tx, scope DocumentScope, now time.Time) (secrets.Scope, error) {
	if err := p.validateDocumentIdentity(tx, scope, now); err != nil {
		return secrets.Scope{}, err
	}
	// App deletion may precede asynchronous Git projection activation. The
	// previous indexed document and exact binding still authorize removal of
	// that path's Git-current guards, so only durable project/environment
	// ownership remains available for this deletion check.
	var durableOrganizationID *string
	var durableNamespace string
	err := tx.QueryRow(ctx, `SELECT p.team_id::text,e.namespace
		FROM projects p
		JOIN environments e ON e.id=$2 AND e.project_id=p.id
		WHERE p.id=$1
		FOR SHARE OF p,e`, scope.Binding.ProjectID, scope.Binding.EnvironmentID).
		Scan(&durableOrganizationID, &durableNamespace)
	if errors.Is(err, pgx.ErrNoRows) {
		return secrets.Scope{}, gitprojection.ErrInvalid
	}
	if err != nil {
		return secrets.Scope{}, err
	}
	expectedOrganizationID := ""
	if durableOrganizationID != nil {
		expectedOrganizationID = *durableOrganizationID
	}
	if scope.OrganizationID != expectedOrganizationID || scope.Namespace != durableNamespace {
		return secrets.Scope{}, gitprojection.ErrInvalid
	}
	secretScope := secrets.Scope{OrganizationID: scope.OrganizationID, ProjectID: scope.Binding.ProjectID,
		EnvironmentID: scope.Binding.EnvironmentID, ApplicationID: scope.ApplicationID, Namespace: scope.Namespace}
	if secretScope.Validate() != nil {
		return secrets.Scope{}, gitprojection.ErrInvalid
	}
	return secretScope, nil
}

func (p *RuntimeSecretReferencePolicy) validateDocumentIdentity(tx pgx.Tx, scope DocumentScope, now time.Time) error {
	if p == nil || p.Config.Validate() != nil || tx == nil || now.IsZero() || scope.Binding.Validate() != nil ||
		scope.Binding.Kind != gitprojection.BindingEnvironment || scope.ApplicationID == "" || scope.Path == "" ||
		scope.SourceRevision != scope.Binding.TargetHeadRevision || scope.ConfigRevision == "" || !policyDigestRE.MatchString(scope.ContentSHA256) {
		return gitprojection.ErrInvalid
	}
	expectedPath, err := gitprojection.ApplicationPath(scope.Binding, scope.ApplicationID)
	if err != nil || expectedPath != scope.Path {
		return gitprojection.ErrInvalid
	}
	return nil
}

func runtimeUsesSecretReferences(runtime domain.WorkloadRuntime) bool {
	for _, variable := range runtime.Env {
		if variable.ValueFrom != nil {
			return true
		}
	}
	return false
}

func semanticRuntimeSecretReferenceError(err error) bool {
	return errors.Is(err, secrets.ErrInvalid) || errors.Is(err, secrets.ErrNotFound) || errors.Is(err, secrets.ErrConflict) ||
		errors.Is(err, secrets.ErrNotReady) || errors.Is(err, secrets.ErrProviderMismatch)
}

func runtimeSecretPolicyRequestID(scope DocumentScope) string {
	digest := sha256.Sum256([]byte(scope.Binding.ID + "\x00" + scope.Path + "\x00" + scope.SourceRevision + "\x00" + scope.ConfigRevision))
	return "runtime-secret-git:" + hex.EncodeToString(digest[:16])
}

var _ ReferencePolicy = (*RuntimeSecretReferencePolicy)(nil)
