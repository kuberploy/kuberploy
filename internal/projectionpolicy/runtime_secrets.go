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
	if scope.OrganizationID == "" {
		if usesReferences {
			return []gitprojection.Diagnostic{{
				Code:    "RuntimeSecretReferenceUnresolved",
				Detail:  "Runtime-secret references require an exact organization-owned application destination.",
				Pointer: "/spec/runtime/env",
			}}, nil
		}
		exists, err := gitCurrentReferencesExistTx(ctx, tx, scope.Path)
		if err != nil {
			return nil, err
		}
		if exists {
			// Without an immutable organization scope there is no safe plan with
			// which to prove that a prior deletion guard may be removed.
			return nil, gitprojection.ErrConflict
		}
		return nil, nil
	}
	secretScope, err := p.validateScope(tx, scope, now)
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
	plan, err := secrets.ResolveWorkloadBindingReferences(ctx, catalog, secretScope, runtime)
	if err != nil {
		if semanticRuntimeSecretReferenceError(err) {
			return []gitprojection.Diagnostic{{
				Code:    "RuntimeSecretReferenceUnresolved",
				Detail:  "A runtime-secret reference is unavailable, inactive, or not authorized for this exact application destination.",
				Pointer: "/spec/runtime/env",
			}}, nil
		}
		return nil, err
	}
	if len(middlewareReferences) != 0 {
		middlewarePlan, resolveErr := secrets.ResolveMiddlewareBindingReferences(ctx, catalog, secretScope, middlewareReferences)
		if resolveErr != nil {
			if semanticRuntimeSecretReferenceError(resolveErr) {
				return []gitprojection.Diagnostic{{Code: "MiddlewareSecretReferenceUnresolved", Detail: "A BasicAuth runtime-secret reference is unavailable, inactive, or not authorized for this exact application destination.", Pointer: "/spec/middlewares"}}, nil
			}
			return nil, resolveErr
		}
		plan, err = secrets.MergeBindingReferencePlans(plan, middlewarePlan)
		if err != nil {
			return nil, err
		}
	}
	if err = secrets.ReplaceGitCurrentReferencesTx(ctx, tx, plan, "", scope.Path, scope.SourceRevision, runtimeSecretPolicyRequestID(scope), now.UTC()); err != nil {
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
	if scope.OrganizationID == "" {
		exists, err := gitCurrentReferencesExistTx(ctx, tx, scope.Path)
		if err != nil {
			return err
		}
		if exists {
			return gitprojection.ErrConflict
		}
		return nil
	}
	secretScope, err := p.validateScope(tx, scope, now)
	if err != nil {
		return err
	}
	plan := secrets.BindingReferencePlan{Scope: secretScope, Uses: []secrets.ResolvedBindingReference{}}
	return secrets.ReplaceGitCurrentReferencesTx(ctx, tx, plan, "", scope.Path, scope.SourceRevision, runtimeSecretPolicyRequestID(scope), now.UTC())
}

func (p *RuntimeSecretReferencePolicy) validateScope(tx pgx.Tx, scope DocumentScope, now time.Time) (secrets.Scope, error) {
	if err := p.validateDocumentIdentity(tx, scope, now); err != nil || scope.OrganizationID == "" {
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
		scope.SourceRevision != scope.Binding.TargetHeadRevision || scope.ConfigRevision == "" {
		return gitprojection.ErrInvalid
	}
	expectedPath, err := gitprojection.ApplicationPath(scope.Binding, scope.ApplicationID)
	if err != nil || expectedPath != scope.Path {
		return gitprojection.ErrInvalid
	}
	return nil
}

func gitCurrentReferencesExistTx(ctx context.Context, tx pgx.Tx, referenceID string) (bool, error) {
	if tx == nil || referenceID == "" {
		return false, gitprojection.ErrInvalid
	}
	var bindingID string
	err := tx.QueryRow(ctx, `SELECT binding_id::text
		FROM secret_binding_references
		WHERE kind='git-current' AND reference_id=$1
		ORDER BY binding_id
		LIMIT 1
		FOR UPDATE`, referenceID).Scan(&bindingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return bindingID != "", nil
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
