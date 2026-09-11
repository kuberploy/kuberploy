package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/autodeploy"
	"github.com/kuberploy/kuberploy/internal/builds"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/imagepull"
	"github.com/kuberploy/kuberploy/internal/store"
	"github.com/kuberploy/kuberploy/internal/variablecompiler"
)

type sourceDeploymentFence struct {
	IntentID string
	Sequence int64
}

type sourceDeploymentFenceKey struct{}

func (s *Server) SubmitAutoDeployment(ctx context.Context, submission autodeploy.Submission) (autodeploy.SubmissionReceipt, error) {
	if s == nil || s.store == nil || s.gitProjection == nil || submission.Validate() != nil {
		return autodeploy.SubmissionReceipt{}, autodeploy.ErrInvalid
	}
	fingerprintValue := "sha256:" + fingerprint(struct {
		ActorID, AttemptID, PolicyID, Image, TemplateDigest, SourceDeploymentID, SourceConfigETag string
		PolicyRevision, SourceDeploymentGeneration                                                int64
	}{submission.ActorID, submission.AttemptID, submission.PolicyID, submission.Image, submission.TemplateDigest,
		submission.SourceDeploymentID, submission.SourceConfigETag, submission.PolicyRevision, submission.SourceDeploymentGeneration})
	// Recover an exact accepted command before consulting mutable runtime state.
	fence, _ := ctx.Value(sourceDeploymentFenceKey{}).(sourceDeploymentFence)
	if replayed, operation, replayErr := s.store.SaveDeploymentConfig(ctx, submission.ActorID, submission.IdempotencyKey,
		fingerprintValue, submission.RequestID, domain.SaveDeploymentConfig{DeploymentID: submission.SourceDeploymentID,
			SourceDeploymentIntentID: fence.IntentID, SourceDeploymentSequence: fence.Sequence}, &gitprojection.WritePlan{}); replayErr == nil && replayed.Replay {
		return autodeploy.SubmissionReceipt{OperationID: operation.ID, DeploymentID: replayed.Value.ID, Replay: true}, nil
	} else if errors.Is(replayErr, store.ErrIdempotencyConflict) || errors.Is(replayErr, store.ErrForbidden) || errors.Is(replayErr, store.ErrNotFound) {
		return autodeploy.SubmissionReceipt{}, replayErr
	}
	if problem := s.deploymentMutationRuntimeProblem(ctx); problem != "" {
		return autodeploy.SubmissionReceipt{}, autodeploy.ErrNotReady
	}
	deployment, err := s.store.GetDeploymentForActor(ctx, submission.ActorID, submission.SourceDeploymentID)
	if err != nil {
		return autodeploy.SubmissionReceipt{}, err
	}
	if deployment.ID != submission.SourceDeploymentID || deployment.ApplicationID != submission.ApplicationID ||
		deployment.EnvironmentID != submission.EnvironmentID || deployment.Generation < submission.SourceDeploymentGeneration {
		return autodeploy.SubmissionReceipt{}, autodeploy.ErrConflict
	}
	bundle, err := s.gitProjection.Bundle(ctx, submission.ActorID, deployment, "", 0)
	applicationDocument, found := applicationBundleDocument(bundle, deployment.ApplicationID)
	if err != nil || !found || !applicationDocument.Valid {
		return autodeploy.SubmissionReceipt{}, autodeploy.ErrNotReady
	}
	if fence.IntentID != "" && bundle.ETag != submission.SourceConfigETag {
		return autodeploy.SubmissionReceipt{}, autodeploy.ErrConflict
	}
	dependencies, _, dependencyErr := variablecompiler.CanonicalDependencyIntent(bundle.Dependencies, bundle.Documents)
	if dependencyErr != nil {
		return autodeploy.SubmissionReceipt{}, autodeploy.ErrNotReady
	}
	dependencyIntent := make([]appconfig.AutoDeployDependencyIntent, len(dependencies))
	for index, dependency := range dependencies {
		dependencyIntent[index] = appconfig.AutoDeployDependencyIntent{Path: dependency.Path, Present: dependency.Present,
			BlobID: dependency.BlobID, ContentSHA256: dependency.ContentSHA256}
	}
	current := append([]byte(nil), applicationDocument.Raw...)
	candidate := appconfig.ApplyAutoDeployImageWithDependencies(current, submission.ConfigIntent, submission.TemplateDigest, submission.Image, dependencyIntent)
	if len(candidate.Diagnostics) != 0 {
		return autodeploy.SubmissionReceipt{}, autodeploy.ErrConflict
	}
	candidate = s.materializeSchedulingCandidate(ctx, submission.ActorID, deployment, current, candidate)
	candidate = s.materializeMiddlewareCandidate(ctx, submission.ActorID, deployment, current, candidate)
	if len(candidate.Diagnostics) != 0 {
		return autodeploy.SubmissionReceipt{}, autodeploy.ErrConflict
	}
	repository, _, _ := strings.Cut(submission.Image, "@")
	var registryPull *domain.RegistryPullReference
	if s.registryPulls != nil {
		resolved, present, resolveErr := s.registryPulls.ResolveRegistryPull(ctx, s.registryPullConfig, submission.ApplicationID, submission.EnvironmentID, repository)
		if resolveErr != nil {
			return autodeploy.SubmissionReceipt{}, resolveErr
		}
		if present {
			if s.registryPullReadiness == nil {
				return autodeploy.SubmissionReceipt{}, imagepull.ErrUnavailable
			}
			probeContext, cancel := context.WithTimeout(ctx, 2*time.Second)
			readyErr := s.registryPullReadiness.Probe(probeContext)
			cancel()
			if readyErr != nil {
				return autodeploy.SubmissionReceipt{}, imagepull.ErrUnavailable
			}
			registryPull = &resolved
		}
	}
	sslipHostname := ""
	if appconfig.AutoDeployUsesSSLIP(candidate) {
		sslipHostname, err = s.resolveSSLIPDeploymentHostname(ctx, submission.ActorID, submission.ApplicationID, submission.EnvironmentID)
		if err != nil {
			return autodeploy.SubmissionReceipt{}, err
		}
	}
	candidate = appconfig.WithAutoDeployDerived(current, candidate, registryPull, sslipHostname)
	candidate.Diagnostics = append(candidate.Diagnostics, s.externalDNSRouteDiagnostics(ctx, submission.ActorID, deployment, candidate)...)
	sslipDiagnostics, _ := s.sslipRouteDiagnostics(ctx, submission.ActorID, deployment, candidate)
	candidate.Diagnostics = append(candidate.Diagnostics, sslipDiagnostics...)
	if len(candidate.Diagnostics) != 0 {
		return autodeploy.SubmissionReceipt{}, autodeploy.ErrConflict
	}
	resolution, resolutionErr := resolveBundleVariables(&bundle, candidate.Runtime)
	if resolutionErr != nil {
		return autodeploy.SubmissionReceipt{}, autodeploy.ErrConflict
	}
	references, err := s.resolveAppConfigReferencePlan(ctx, submission.ActorID, deployment, resolution.Runtime, candidate.Parsed)
	if err != nil {
		return autodeploy.SubmissionReceipt{}, err
	}
	plan, err := s.gitProjection.PlanMutation(ctx, submission.ActorID, submission.EnvironmentID, submission.ApplicationID, bundle.ETag)
	if err != nil {
		return autodeploy.SubmissionReceipt{}, err
	}
	previewToken := make([]byte, 32)
	if _, err = rand.Read(previewToken); err != nil {
		return autodeploy.SubmissionReceipt{}, err
	}
	tokenHash := sha256.Sum256(previewToken)
	for index := range previewToken {
		previewToken[index] = 0
	}
	expiresAt := time.Now().UTC().Add(10 * time.Minute)
	if err = s.store.CreateDeploymentConfigPreview(ctx, submission.ActorID, domain.CreateConfigPreview{DeploymentID: deployment.ID,
		BaseETag: bundle.ETag, TokenHash: tokenHash[:], CandidateHash: candidate.Hash, CandidateRaw: candidate.Raw,
		Runtime: candidate.Runtime, ExpiresAt: expiresAt}, &plan, references); err != nil {
		return autodeploy.SubmissionReceipt{}, err
	}
	result, operation, err := s.store.SaveDeploymentConfig(ctx, submission.ActorID, submission.IdempotencyKey, fingerprintValue,
		submission.RequestID, domain.SaveDeploymentConfig{DeploymentID: deployment.ID, BaseETag: bundle.ETag, TokenHash: tokenHash[:],
			CandidateHash: candidate.Hash, RawYAML: candidate.Raw, Runtime: candidate.Runtime,
			SourceDeploymentIntentID: fence.IntentID, SourceDeploymentSequence: fence.Sequence}, &plan, references)
	if err != nil {
		return autodeploy.SubmissionReceipt{}, err
	}
	return autodeploy.SubmissionReceipt{OperationID: operation.ID, DeploymentID: result.Value.ID, Replay: result.Replay}, nil
}

func (s *Server) SubmitSourceDeployment(ctx context.Context, submission builds.SourceDeploymentSubmission) (builds.SourceDeploymentSubmissionReceipt, error) {
	if submission.Validate() != nil {
		return builds.SourceDeploymentSubmissionReceipt{}, builds.ErrInvalid
	}
	if submission.StartDraft {
		return s.startSourceDeploymentDraft(ctx, submission)
	}
	auto := autodeploy.Submission{ActorID: submission.ActorID, RequestID: submission.RequestID,
		AttemptID: submission.AttemptID, PolicyID: submission.IntentID, PolicyRevision: submission.Sequence,
		ProjectID: submission.ProjectID, ApplicationID: submission.ApplicationID, EnvironmentID: submission.EnvironmentID,
		Image: submission.Image, ConfigIntent: submission.ConfigIntent, TemplateDigest: submission.TemplateDigest,
		SourceDeploymentID: submission.DeploymentID, SourceDeploymentGeneration: submission.SourceDeploymentGeneration,
		SourceConfigETag: submission.SourceConfigETag}
	auto.IdempotencyKey = autodeploy.IdempotencyKey(auto.PolicyID, auto.PolicyRevision, auto.AttemptID)
	receipt, err := s.SubmitAutoDeployment(context.WithValue(ctx, sourceDeploymentFenceKey{}, sourceDeploymentFence{
		IntentID: submission.IntentID, Sequence: submission.Sequence}), auto)
	if err != nil {
		return builds.SourceDeploymentSubmissionReceipt{}, err
	}
	return builds.SourceDeploymentSubmissionReceipt{OperationID: receipt.OperationID, DeploymentID: receipt.DeploymentID}, nil
}

func (s *Server) startSourceDeploymentDraft(ctx context.Context, submission builds.SourceDeploymentSubmission) (builds.SourceDeploymentSubmissionReceipt, error) {
	if s == nil || s.store == nil || s.gitProjection == nil {
		return builds.SourceDeploymentSubmissionReceipt{}, builds.ErrInvalid
	}
	if s.deploymentMutationRuntimeProblem(ctx) != "" {
		return builds.SourceDeploymentSubmissionReceipt{}, autodeploy.ErrNotReady
	}
	deployment, err := s.store.GetDeploymentForActor(ctx, submission.ActorID, submission.DeploymentID)
	if err != nil {
		return builds.SourceDeploymentSubmissionReceipt{}, err
	}
	if deployment.State != "stopped" || deployment.Generation != submission.SourceDeploymentGeneration ||
		deployment.ApplicationID != submission.ApplicationID || deployment.EnvironmentID != submission.EnvironmentID {
		return builds.SourceDeploymentSubmissionReceipt{}, builds.ErrConflict
	}
	runtime := deployment.Runtime
	if len(runtime.Ports) == 0 {
		runtime = domain.DefaultWorkloadRuntime(3000, nil)
	}
	create := domain.CreateDeployment{EnvironmentID: submission.EnvironmentID, ApplicationID: submission.ApplicationID,
		Image: submission.Image, Runtime: runtime, Route: deployment.Route,
		SourceDeploymentIntentID: submission.IntentID, SourceDeploymentSequence: submission.Sequence,
		SourceDeploymentGeneration: submission.SourceDeploymentGeneration}
	repository, _, _ := strings.Cut(submission.Image, "@")
	if s.registryPulls != nil {
		resolved, present, resolveErr := s.registryPulls.ResolveRegistryPull(ctx, s.registryPullConfig, submission.ApplicationID, submission.EnvironmentID, repository)
		if resolveErr != nil {
			return builds.SourceDeploymentSubmissionReceipt{}, resolveErr
		}
		if present {
			if s.registryPullReadiness == nil {
				return builds.SourceDeploymentSubmissionReceipt{}, imagepull.ErrUnavailable
			}
			probeContext, cancel := context.WithTimeout(ctx, 2*time.Second)
			readyErr := s.registryPullReadiness.Probe(probeContext)
			cancel()
			if readyErr != nil {
				return builds.SourceDeploymentSubmissionReceipt{}, imagepull.ErrUnavailable
			}
			create.RegistryPull = &resolved
		}
	}
	var exactConfigParsed map[string]any
	if len(deployment.ConfigRaw) != 0 {
		candidate := appconfig.ApplyAutoDeployImage(deployment.ConfigRaw, submission.ConfigIntent, submission.TemplateDigest, submission.Image)
		candidate = s.materializeSchedulingCandidate(ctx, submission.ActorID, deployment, deployment.ConfigRaw, candidate)
		candidate = s.materializeMiddlewareCandidate(ctx, submission.ActorID, deployment, deployment.ConfigRaw, candidate)
		if len(candidate.Diagnostics) != 0 {
			return builds.SourceDeploymentSubmissionReceipt{}, builds.ErrConflict
		}
		sslipHostname := ""
		if appconfig.AutoDeployUsesSSLIP(candidate) {
			sslipHostname, err = s.resolveSSLIPDeploymentHostname(ctx, submission.ActorID, submission.ApplicationID, submission.EnvironmentID)
			if err != nil {
				return builds.SourceDeploymentSubmissionReceipt{}, err
			}
		}
		candidate = appconfig.WithAutoDeployDerived(deployment.ConfigRaw, candidate, create.RegistryPull, sslipHostname)
		candidate.Diagnostics = append(candidate.Diagnostics, s.externalDNSRouteDiagnostics(ctx, submission.ActorID, deployment, candidate)...)
		sslipDiagnostics, _ := s.sslipRouteDiagnostics(ctx, submission.ActorID, deployment, candidate)
		candidate.Diagnostics = append(candidate.Diagnostics, sslipDiagnostics...)
		if len(candidate.Diagnostics) != 0 {
			return builds.SourceDeploymentSubmissionReceipt{}, builds.ErrConflict
		}
		create.ConfigRaw, create.Runtime, exactConfigParsed = candidate.Raw, candidate.Runtime, candidate.Parsed
	}
	resolvedRuntime, err := s.resolveSchedulingRuntime(ctx, submission.ActorID, deployment, create.Runtime, true)
	if err != nil {
		return builds.SourceDeploymentSubmissionReceipt{}, err
	}
	create.Runtime = resolvedRuntime
	plan, err := s.gitProjection.PlanMutation(ctx, submission.ActorID, submission.EnvironmentID, submission.ApplicationID, "")
	if err != nil {
		return builds.SourceDeploymentSubmissionReceipt{}, err
	}
	references, err := s.resolveAppConfigReferencePlan(ctx, submission.ActorID, deployment, create.Runtime, exactConfigParsed)
	if err != nil {
		return builds.SourceDeploymentSubmissionReceipt{}, err
	}
	key := autodeploy.IdempotencyKey(submission.IntentID, submission.Sequence, submission.AttemptID)
	fingerprintValue := "sha256:" + fingerprint(struct {
		IntentID, AttemptID, Image string
		Sequence, Generation       int64
	}{submission.IntentID, submission.AttemptID, submission.Image, submission.Sequence, submission.SourceDeploymentGeneration})
	result, operation, err := s.store.CreateDeployment(ctx, submission.ActorID, key, fingerprintValue, submission.RequestID, create, &plan, references)
	if err != nil {
		return builds.SourceDeploymentSubmissionReceipt{}, err
	}
	return builds.SourceDeploymentSubmissionReceipt{OperationID: operation.ID, DeploymentID: result.Value.ID}, nil
}

var _ autodeploy.CanonicalDeploymentPipeline = (*Server)(nil)
var _ builds.SourceDeploymentPipeline = (*Server)(nil)
