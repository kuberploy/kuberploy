package projectionpolicy

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/imagepull"
)

const registryPullPointer = "/spec/delivery/registryPull"

// RegistryPullReferencePolicy resolves only the locked target/profile identity
// present in AppConfig. Operator credential references are loaded from the
// exact target and profile; they never enter the policy document or Git.
type RegistryPullReferencePolicy struct {
	Config imagepull.RuntimeConfig
}

func (p *RegistryPullReferencePolicy) ValidateCurrentTx(
	ctx context.Context,
	tx pgx.Tx,
	document AppConfigPolicyDocument,
	now time.Time,
) ([]gitprojection.Diagnostic, error) {
	if tx == nil || now.IsZero() || document.validate() != nil {
		return nil, gitprojection.ErrInvalid
	}
	delivery := document.Delivery()
	if !delivery.HasRegistryPull {
		// Absence is the public-image form and deliberately stages no credential
		// artifact. A forged omission cannot disclose credentials: the runtime
		// chart renders no imagePullSecret and the pull fails as an ordinary
		// unauthenticated registry request.
		return nil, nil
	}
	if p == nil || p.Config.Validate() != nil || !p.Config.Enabled {
		return registryPullDiagnostic(
			"RegistryPullProfileUnavailable",
			"Private-image pull profiles are not enabled by the active operator configuration.",
			registryPullPointer,
		), nil
	}
	scope := document.Scope()
	if !p.Config.AllowsNamespace(scope.Namespace) {
		return registryPullDiagnostic(
			"RegistryPullNamespaceUnavailable",
			"Private-image pulls are not enabled for this exact destination namespace.",
			registryPullPointer,
		), nil
	}
	requested := delivery.RegistryPull
	profile, found := p.Config.ProfileForTarget(requested.TargetID)
	if !found {
		return registryPullDiagnostic(
			"RegistryPullProfileUnavailable",
			"The referenced registry target has no active operator-owned pull profile.",
			registryPullPointer+"/targetId",
		), nil
	}
	if requested.ProfileName != profile.Name || requested.ProfileRevision != profile.Revision {
		return registryPullDiagnostic(
			"RegistryPullProfileMismatch",
			"The locked pull profile name or revision does not match the active operator configuration.",
			registryPullPointer,
		), nil
	}
	target, policy, server, err := imagepull.ExactRegistryPolicyTx(ctx, tx, requested.TargetID, scope.ApplicationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return registryPullDiagnostic(
			"RegistryPullPolicyUnavailable",
			"No exact registry policy exists for this application and registry target.",
			registryPullPointer+"/targetId",
		), nil
	}
	if err != nil {
		return nil, err
	}
	if target.ID != requested.TargetID || policy.RegistryTargetID != requested.TargetID || policy.ServiceID != scope.ApplicationID ||
		server != profile.RegistryServer || target.PullCredentialRef == "" || target.PullCredentialRef != profile.CredentialRef {
		return registryPullDiagnostic(
			"RegistryPullPolicyMismatch",
			"The registry target, application policy, and operator pull profile do not match exactly.",
			registryPullPointer,
		), nil
	}
	if delivery.Repository != profile.RegistryServer+"/"+policy.Repository {
		return registryPullDiagnostic(
			"RegistryPullRepositoryMismatch",
			"The release repository is not the exact repository authorized by this application's registry policy.",
			"/spec/delivery/release/repository",
		), nil
	}
	desired, err := imagepull.Desired(p.Config, scope.Binding.EnvironmentID, scope.Namespace, requested.TargetID)
	if err != nil || desired.ProfileName != requested.ProfileName || desired.ProfileRevision != requested.ProfileRevision ||
		desired.PullCredentialRef != target.PullCredentialRef {
		return registryPullDiagnostic(
			"RegistryPullProfileMismatch",
			"The locked pull profile cannot be derived from the exact environment, target, and operator configuration.",
			registryPullPointer,
		), nil
	}
	artifact, err := imagepull.EnsureArtifactTx(ctx, tx, desired, now.UTC())
	if err != nil {
		// Never downgrade an artifact write failure to a diagnostic. The ensure
		// operation may have rotated an older active revision before a later SQL
		// error, so the outer activation transaction must roll back atomically.
		return nil, err
	}
	if artifact.Validate() != nil || !artifact.Active || artifact.DesiredArtifact != desired {
		return nil, gitprojection.ErrConflict
	}
	return nil, nil
}

// ReconcileDeletedTx is intentionally a metadata-only no-op in schema 025.
// Artifacts are shared by every application using an environment/target pair,
// and old immutable Secrets are rollback material. Safe deactivation and GC
// require a later exact desired-reference plus observed-workload retention
// table; one deleted AppConfig is not proof that either can be removed.
func (p *RegistryPullReferencePolicy) ReconcileDeletedTx(_ context.Context, tx pgx.Tx, scope DocumentScope, now time.Time) error {
	if tx == nil || now.IsZero() || !validDocumentScope(scope) {
		return gitprojection.ErrInvalid
	}
	return nil
}

// ResolveRegistryPullTx is the server-side writer seam. It derives locked pull
// metadata from the exact application/environment relationship, release
// repository, durable registry policy/target, and operator runtime profile.
// An unmatched repository is public and returns present=false. A private match
// with missing, ambiguous, or mismatched operator metadata fails closed.
func ResolveRegistryPullTx(
	ctx context.Context,
	tx pgx.Tx,
	config imagepull.RuntimeConfig,
	applicationID, environmentID, releaseRepository string,
) (reference RegistryPullReference, present bool, err error) {
	resolved, present, err := imagepull.ResolveReferenceTx(ctx, tx, config, applicationID, environmentID, releaseRepository)
	if err != nil {
		return RegistryPullReference{}, false, err
	}
	return RegistryPullReference{TargetID: resolved.TargetID, ProfileName: resolved.ProfileName, ProfileRevision: resolved.ProfileRevision}, present, nil
}

// RegistryPullArtifactEligibleTx is the exact Argo approval query. Public
// documents are resolved immediately. Private documents require the one active
// artifact for the same environment, namespace, target, profile name and
// profile revision to be ready and observed within maxAge. Worker readiness is
// intentionally a separate global feature check.
func RegistryPullArtifactEligibleTx(
	ctx context.Context,
	tx pgx.Tx,
	document AppConfigPolicyDocument,
	now time.Time,
	maxAge time.Duration,
) (bool, error) {
	if tx == nil || document.validate() != nil || now.IsZero() || maxAge <= 0 || maxAge > 15*time.Minute {
		return false, imagepull.ErrInvalid
	}
	delivery := document.Delivery()
	if !delivery.HasRegistryPull {
		return true, nil
	}
	scope, pull := document.Scope(), delivery.RegistryPull
	var active bool
	var state imagepull.RuntimeState
	var observedAt *time.Time
	err := tx.QueryRow(ctx, `SELECT active,runtime_state,last_observed_at
		FROM runtime_registry_pull_artifacts
		WHERE environment_id=$1 AND namespace=$2 AND registry_target_id=$3
		  AND profile_name=$4 AND profile_revision=$5
		FOR SHARE`, scope.Binding.EnvironmentID, scope.Namespace, pull.TargetID, pull.ProfileName, pull.ProfileRevision).
		Scan(&active, &state, &observedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return active && state == imagepull.StateReady && observedAt != nil &&
		!observedAt.Before(now.UTC().Add(-maxAge)) && !observedAt.After(now.UTC()), nil
}

func registryPullDiagnostic(code, detail, pointer string) []gitprojection.Diagnostic {
	return []gitprojection.Diagnostic{{Code: code, Detail: detail, Pointer: pointer}}
}

var _ ReferencePolicy = (*RegistryPullReferencePolicy)(nil)
