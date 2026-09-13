package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/builds"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/githubapp"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/gitssh"
	"github.com/kuberploy/kuberploy/internal/store"
	"github.com/kuberploy/kuberploy/internal/variablecompiler"
)

type SourceDeploymentBackend interface {
	AcceptSourceDeployment(context.Context, builds.SourceDeploymentCommand) (builds.SourceDeploymentAcceptance, error)
	SourceDeploymentReceipt(context.Context, string, string, string) (builds.SourceDeploymentAcceptance, error)
}

type sourceDeploymentRequest struct {
	Mode            builds.SourceDeploymentMode `json:"mode"`
	SourceAttemptID string                      `json:"sourceAttemptId,omitempty"`
}

func (s *Server) sourceDeployment(w http.ResponseWriter, r *http.Request) {
	key, ok := githubBuildIdempotencyKey(w, r)
	if !ok {
		return
	}
	if s.sourceDeployments == nil {
		githubBuildUnavailable(w, r, "Source deployment is not configured.")
		return
	}
	var input sourceDeploymentRequest
	if !decodeGitHubBuildJSON(w, r, &input) {
		return
	}
	if input.Mode != builds.SourceDeploymentDeploy && input.Mode != builds.SourceDeploymentRebuild ||
		(input.Mode == builds.SourceDeploymentDeploy && input.SourceAttemptID != "") ||
		(input.Mode == builds.SourceDeploymentRebuild && strings.TrimSpace(input.SourceAttemptID) == "") {
		mappedGitHubBuildError(w, r, builds.ErrInvalid)
		return
	}
	actor := currentUser(r.Context()).ID
	deployment, err := s.store.GetDeploymentForActor(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		mappedError(w, r, err)
		return
	}
	application, err := s.store.GetApplicationForActor(r.Context(), actor, deployment.ApplicationID)
	if err != nil {
		mappedError(w, r, err)
		return
	}
	if err = s.store.Authorize(r.Context(), actor, domain.PermissionBuildsManage,
		domain.AccessTarget{Type: "application", ID: deployment.ApplicationID}); err != nil {
		mappedError(w, r, err)
		return
	}
	// Recover immutable caller identity before mutable configuration or provider
	// resolution. Older releases included server state in their fingerprints;
	// their original receipts and snapshots remain valid and are never rewritten.
	receipt, receiptErr := s.sourceDeployments.SourceDeploymentReceipt(r.Context(), actor, deployment.ID, key)
	if receiptErr == nil {
		intent := receipt.Intent
		if intent.ActorID != actor || intent.ProjectID != application.ProjectID || intent.ApplicationID != deployment.ApplicationID ||
			intent.EnvironmentID != deployment.EnvironmentID || intent.DeploymentID != deployment.ID || intent.Mode != input.Mode ||
			intent.SourceAttemptID != input.SourceAttemptID {
			mappedGitHubBuildError(w, r, builds.ErrConflict)
			return
		}
		receipt.Replay = true
		writeSourceDeploymentAcceptance(w, receipt)
		return
	}
	if !errors.Is(receiptErr, builds.ErrNotFound) {
		mappedGitHubBuildError(w, r, receiptErr)
		return
	}
	var config domain.DeploymentConfig
	var bundle *gitprojection.Bundle
	startDraft := deployment.State == "stopped"
	if startDraft {
		config, err = s.store.GetDeploymentConfigForActor(r.Context(), actor, deployment.ID)
		if err != nil && !errors.Is(err, store.ErrConfigProjectionMissing) {
			mappedError(w, r, err)
			return
		}
	} else {
		deployment, config, bundle, err = s.currentConfig(r, "", 0)
		if err != nil {
			mappedError(w, r, err)
			return
		}
	}
	var intent []byte
	var digest string
	if len(config.RawYAML) != 0 {
		var diagnostics []appconfig.Diagnostic
		intent, digest, diagnostics = appconfig.AutoDeployIntentTemplate(config.RawYAML)
		if len(diagnostics) != 0 {
			mappedGitHubBuildError(w, r, builds.ErrInvalid)
			return
		}
		if bundle != nil {
			dependencies, _, dependencyErr := variablecompiler.CanonicalDependencyIntent(bundle.Dependencies, bundle.Documents)
			if dependencyErr != nil {
				mappedGitHubBuildError(w, r, builds.ErrInvalid)
				return
			}
			dependencyIntent := make([]appconfig.AutoDeployDependencyIntent, len(dependencies))
			for index, dependency := range dependencies {
				dependencyIntent[index] = appconfig.AutoDeployDependencyIntent{Path: dependency.Path, Present: dependency.Present,
					BlobID: dependency.BlobID, ContentSHA256: dependency.ContentSHA256}
			}
			intent, digest, err = appconfig.BindAutoDeployDependencies(intent, dependencyIntent)
			if err != nil {
				mappedGitHubBuildError(w, r, builds.ErrInvalid)
				return
			}
		}
	}
	projectionETag := ""
	if deployment.ConfigVersion > 0 && len(deployment.ConfigRaw) != 0 {
		projectionETag = domain.DeploymentConfigETag(deployment.ID, deployment.ConfigVersion, deployment.ConfigRaw)
	}
	// The receipt identifies the caller's request. Mutable deployment state is
	// fenced separately when accepting new work and must not invalidate replay.
	fp := "sha256:" + fingerprint(struct {
		DeploymentID string
		Request      sourceDeploymentRequest
	}{deployment.ID, input})
	accepted, err := s.sourceDeployments.AcceptSourceDeployment(r.Context(), builds.SourceDeploymentCommand{
		ActorID: actor, ProjectID: application.ProjectID, ApplicationID: deployment.ApplicationID,
		EnvironmentID: deployment.EnvironmentID, DeploymentID: deployment.ID, Mode: input.Mode,
		SourceAttemptID: input.SourceAttemptID, SourceDeploymentGeneration: deployment.Generation,
		SourceConfigETag: config.ETag, SourceProjectionETag: projectionETag, ConfigIntent: intent, TemplateDigest: digest,
		IdempotencyKey: key, Fingerprint: fp, RequestID: requestID(r.Context()), StartDraft: startDraft,
	})
	if err != nil {
		mappedGitHubBuildError(w, r, err)
		return
	}
	writeSourceDeploymentAcceptance(w, accepted)
}

func writeSourceDeploymentAcceptance(w http.ResponseWriter, accepted builds.SourceDeploymentAcceptance) {
	if accepted.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Location", "/v1/builds/"+accepted.Attempt.ID)
	writeJSON(w, http.StatusAccepted, struct {
		Build    buildAttemptView `json:"build"`
		IntentID string           `json:"intentId"`
		Sequence int64            `json:"sequence"`
	}{safeBuildAttempt(accepted.Attempt), accepted.Intent.ID, accepted.Intent.Sequence})
}

func (b *buildBackend) SourceDeploymentReceipt(ctx context.Context, actorID, deploymentID, key string) (builds.SourceDeploymentAcceptance, error) {
	return b.store.SourceDeploymentReceipt(ctx, actorID, deploymentID, key)
}

func (b *buildBackend) AcceptSourceDeployment(ctx context.Context, command builds.SourceDeploymentCommand) (builds.SourceDeploymentAcceptance, error) {
	definitions, err := b.store.DefinitionsForService(ctx, command.ApplicationID)
	if err != nil {
		return builds.SourceDeploymentAcceptance{}, err
	}
	if len(definitions) != 1 {
		return builds.SourceDeploymentAcceptance{}, builds.ErrConflict
	}
	definition := definitions[0]
	if command.Mode == builds.SourceDeploymentRebuild {
		source, sourceErr := b.store.Attempt(ctx, command.SourceAttemptID)
		if sourceErr != nil {
			return builds.SourceDeploymentAcceptance{}, sourceErr
		}
		if source.ProjectID != command.ProjectID || source.ServiceID != command.ApplicationID || source.State != builds.AttemptSucceeded {
			return builds.SourceDeploymentAcceptance{}, builds.ErrConflict
		}
		definition, command.CommitSHA = source.SourceSnapshot, source.CommitSHA
	}
	command.DefinitionID, command.ExpectedDefinitionDigest = definition.ID, definition.DefinitionDigest
	resolution, err := b.resolver.ResolveBuildDefinition(ctx, command.ActorID, command.ProjectID, command.ApplicationID, definition.Spec.Registry.TargetID)
	if err != nil || resolution.Registry.TargetID != definition.Spec.Registry.TargetID {
		if err == nil {
			err = builds.ErrUnauthorized
		}
		return builds.SourceDeploymentAcceptance{}, err
	}
	if len(definition.Spec.SecretFiles) > 0 || len(definition.Spec.SSHFiles) > 0 {
		profiles, ok := b.resolver.(BuildSecretProfileResolver)
		if !ok {
			return builds.SourceDeploymentAcceptance{}, builds.ErrInfrastructure
		}
		selection, selectionErr := profiles.ResolveSecretFiles(definition.ServiceID, definition.Spec.SecretFiles, definition.Spec.SSHFiles)
		if selectionErr != nil {
			return builds.SourceDeploymentAcceptance{}, builds.ErrInfrastructure
		}
		resolution.Execution.BuildSecret, resolution.Execution.SSHSecret = selection.BuildSecret, selection.SSHSecret
	}
	if command.Mode == builds.SourceDeploymentDeploy {
		command.CommitSHA, err = b.resolveSourceHead(ctx, definition)
		if err != nil {
			return builds.SourceDeploymentAcceptance{}, err
		}
	}
	if definition.SourceKind == builds.SourceGitSSH {
		resolution.Execution, err = executionForGitSSHSource(resolution.Execution, definition.GitSSH)
		if err != nil {
			return builds.SourceDeploymentAcceptance{}, err
		}
	}
	command.Execution, command.AcceptedAt = resolution.Execution, b.clock()
	return b.store.AcceptSourceDeployment(ctx, command)
}

func (b *buildBackend) resolveSourceHead(ctx context.Context, definition builds.BuildDefinition) (string, error) {
	if definition.SourceKind == builds.SourceGitSSH {
		if definition.GitSSH == nil || b.gitSSH == nil {
			return "", builds.ErrInfrastructure
		}
		resolved, err := b.gitSSH.ResolveRemoteRef(ctx, gitssh.RemoteRefRequest{Scope: gitssh.Scope(definition.GitSSH.KeyScope),
			OwnerID: definition.GitSSH.KeyOwnerID, KeyRevision: definition.GitSSH.KeyRevision, RepositoryURL: definition.GitSSH.RepositoryURL,
			Ref: definition.TriggerRef, KnownHosts: []byte(definition.GitSSH.KnownHosts)})
		if err != nil {
			return "", err
		}
		if resolved.Ref != definition.TriggerRef || !manualBuildCommitRE.MatchString(resolved.CommitSHA) || resolved.ObservedAt.IsZero() {
			return "", builds.ErrUnauthorized
		}
		return resolved.CommitSHA, nil
	}
	if definition.SourceKind != builds.SourceGitHub || b.provider == nil {
		return "", builds.ErrInfrastructure
	}
	installation, err := b.store.Installation(ctx, definition.InstallationID)
	if err != nil {
		return "", err
	}
	repository, err := b.store.Repository(ctx, definition.RepositoryID)
	if err != nil {
		return "", err
	}
	if installation.Lifecycle != builds.InstallationActive || repository.Lifecycle != builds.RepositoryActive ||
		repository.InstallationID != installation.ID || repository.Identity.OwnerID != installation.Account.ID ||
		!strings.EqualFold(repository.Identity.OwnerLogin, installation.Account.Login) {
		return "", builds.ErrUnauthorized
	}
	required := githubapp.Permissions{"metadata": githubapp.PermissionRead, "contents": githubapp.PermissionRead}
	if _, err = b.provider.VerifyInstallation(ctx, installation.GitHubInstallationID, installation.Account, required); err != nil {
		return "", err
	}
	token, err := b.provider.MintInstallationToken(ctx, githubapp.TokenRequest{InstallationID: installation.GitHubInstallationID,
		Account: installation.Account, Repositories: []githubapp.RepositoryIdentity{repository.Identity}, Permissions: required})
	if err != nil {
		return "", err
	}
	resolved, err := b.provider.ResolveRemoteRef(ctx, token, repository.Identity, definition.TriggerRef)
	if err != nil {
		return "", err
	}
	if resolved.Ref != definition.TriggerRef || !manualBuildCommitRE.MatchString(resolved.CommitSHA) || resolved.ResolvedAt.IsZero() {
		return "", builds.ErrUnauthorized
	}
	return resolved.CommitSHA, nil
}

var _ SourceDeploymentBackend = (*buildBackend)(nil)
