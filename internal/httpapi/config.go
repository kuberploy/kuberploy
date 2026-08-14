package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/appconfigpreview"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/externaldns"
	"github.com/kuberploy/kuberploy/internal/gitops"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/store"
	"github.com/kuberploy/kuberploy/internal/variablecompiler"
	"github.com/kuberploy/kuberploy/internal/variables"
	"go.yaml.in/yaml/v3"
)

type AppConfigRenderedPreviewBackend interface {
	Identity() (appconfigpreview.Identity, string, error)
	Render(context.Context, appconfigpreview.Request) (appconfigpreview.Result, error)
}

type GitProjectionBackend interface {
	PlanMutation(context.Context, string, string, string, string) (gitprojection.WritePlan, error)
	Bundle(context.Context, string, domain.Deployment, string, time.Duration) (gitprojection.Bundle, error)
}

type VariableSetBackend interface {
	VariableSets(context.Context, string, string) ([]gitprojection.VariableSetSnapshot, error)
	PlanVariableMutation(context.Context, string, string, string, string) (gitprojection.WritePlan, error)
}

type configDocumentResponse struct {
	ID               string         `json:"id"`
	DocumentID       string         `json:"documentId"`
	GitPath          string         `json:"gitPath"`
	Path             string         `json:"path"`
	DocumentKind     string         `json:"documentKind"`
	RawYAML          string         `json:"rawYaml"`
	Document         map[string]any `json:"document"`
	EditablePointers []string       `json:"editablePointers"`
	LockedPointers   []string       `json:"lockedPointers"`
}

type configBundleResponse struct {
	Kind                 string                          `json:"kind"`
	ETag                 string                          `json:"etag"`
	TargetHeadRevision   string                          `json:"targetHeadRevision"`
	IndexedRevision      string                          `json:"indexedRevision"`
	ConfigRevision       string                          `json:"configRevision"`
	Freshness            string                          `json:"freshness"`
	Documents            []configDocumentResponse        `json:"documents"`
	VariableDependencies []gitprojection.DependencyState `json:"variableDependencies,omitempty"`
	EffectiveVariables   []variables.Effective           `json:"effectiveVariables,omitempty"`
}

type configValidationResponse struct {
	Valid              bool                   `json:"valid"`
	Diagnostics        []appconfig.Diagnostic `json:"diagnostics"`
	EffectiveVariables []variables.Effective  `json:"effectiveVariables,omitempty"`
}

type configPreviewResponse struct {
	PreviewToken         string                     `json:"previewToken"`
	GitDiff              string                     `json:"gitDiff"`
	RenderedDiff         string                     `json:"renderedDiff"`
	SemanticChanges      []appconfig.SemanticChange `json:"semanticChanges"`
	Warnings             []string                   `json:"warnings"`
	ExpiresAt            time.Time                  `json:"expiresAt"`
	EffectiveVariables   []variables.Effective      `json:"effectiveVariables,omitempty"`
	RenderIdentity       appconfigpreview.Identity  `json:"renderIdentity"`
	RenderIdentityDigest string                     `json:"renderIdentityDigest"`
}

func resolveBundleVariables(bundle *gitprojection.Bundle, runtime domain.WorkloadRuntime) (variablecompiler.Resolution, error) {
	if bundle == nil {
		return variablecompiler.Resolution{Runtime: runtime}, nil
	}
	return variablecompiler.Resolve(bundle.Dependencies, bundle.Documents, runtime)
}

func applicationBundleDocument(bundle gitprojection.Bundle, applicationID string) (gitprojection.Document, bool) {
	var result gitprojection.Document
	found := false
	for _, document := range bundle.Documents {
		if document.ApplicationID != applicationID {
			continue
		}
		if found {
			return gitprojection.Document{}, false
		}
		result, found = document, true
	}
	return result, found
}

func (s *Server) currentConfig(r *http.Request, atLeastRevision string, wait time.Duration) (domain.Deployment, domain.DeploymentConfig, *gitprojection.Bundle, error) {
	actor := currentUser(r.Context()).ID
	deployment, err := s.store.GetDeploymentForActor(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		return domain.Deployment{}, domain.DeploymentConfig{}, nil, err
	}
	if s.gitProjection != nil {
		bundle, bundleErr := s.gitProjection.Bundle(r.Context(), actor, deployment, atLeastRevision, wait)
		if bundleErr != nil {
			return domain.Deployment{}, domain.DeploymentConfig{}, nil, bundleErr
		}
		applicationDocument, found := applicationBundleDocument(bundle, deployment.ApplicationID)
		if !found || !applicationDocument.Valid {
			return domain.Deployment{}, domain.DeploymentConfig{}, nil, gitprojection.ErrInvalid
		}
		_, applicationRuntime, diagnostics := appconfig.ParseAndValidate(applicationDocument.Raw)
		if len(diagnostics) != 0 {
			return domain.Deployment{}, domain.DeploymentConfig{}, nil, gitprojection.ErrInvalid
		}
		resolution, resolutionErr := resolveBundleVariables(&bundle, applicationRuntime)
		if resolutionErr != nil {
			return domain.Deployment{}, domain.DeploymentConfig{}, nil, gitprojection.ErrInvalid
		}
		deployment.Runtime = resolution.Runtime
		deployment.Replicas, deployment.Port, deployment.Environment = domain.LegacyWorkloadFields(deployment.Runtime)
		config := domain.DeploymentConfig{DeploymentID: deployment.ID, RawYAML: append([]byte(nil), applicationDocument.Raw...), ETag: bundle.ETag, UpdatedAt: bundle.IndexedAt}
		return deployment, config, &bundle, nil
	}
	if atLeastRevision != "" || wait != 0 {
		return domain.Deployment{}, domain.DeploymentConfig{}, nil, gitprojection.ErrInvalid
	}
	config, err := s.store.GetDeploymentConfigForActor(r.Context(), actor, deployment.ID)
	return deployment, config, nil, err
}

func configGitPath(deployment domain.Deployment) string {
	return "environments/" + deployment.EnvironmentID + "/apps/" + deployment.ApplicationID + "/app.yaml"
}

func (s *Server) deploymentConfig(w http.ResponseWriter, r *http.Request) {
	atLeastRevision, wait, ok := configReadFence(w, r, s.gitProjection != nil)
	if !ok {
		return
	}
	deployment, config, bundle, err := s.currentConfig(r, atLeastRevision, wait)
	if err != nil {
		mappedGitProjectionError(w, r, err)
		return
	}
	parsed, applicationRuntime, diagnostics := appconfig.ParseAndValidate(config.RawYAML)
	if len(diagnostics) > 0 {
		writeProblem(w, r, 500, "StoredConfigInvalid", "Stored configuration is invalid", "The stored AppConfig failed server validation.")
		return
	}
	path, targetHead, indexedRevision, configRevision, freshness := configGitPath(deployment), "", "", config.ETag, "projection-only"
	if bundle != nil {
		applicationDocument, found := applicationBundleDocument(*bundle, deployment.ApplicationID)
		if !found {
			mappedGitProjectionError(w, r, gitprojection.ErrInvalid)
			return
		}
		path = applicationDocument.Path
		targetHead, indexedRevision, configRevision = bundle.TargetHeadRevision, bundle.IndexedRevision, bundle.ConfigRevision
		freshness = "fresh"
		if bundle.Stale {
			freshness = "stale"
		}
	}
	resolution, resolutionErr := resolveBundleVariables(bundle, applicationRuntime)
	if resolutionErr != nil {
		mappedGitProjectionError(w, r, gitprojection.ErrInvalid)
		return
	}
	documents := []configDocumentResponse{{ID: appconfig.DocumentID, DocumentID: appconfig.DocumentID, GitPath: path, Path: path, DocumentKind: "AppConfig", RawYAML: string(config.RawYAML), Document: parsed, EditablePointers: append([]string(nil), appconfig.EditablePointers...), LockedPointers: append([]string(nil), appconfig.LockedPointers...)}}
	if bundle != nil {
		for _, dependency := range bundle.Documents {
			if dependency.ApplicationID != "" {
				continue
			}
			documents = append(documents, configDocumentResponse{ID: dependency.Path, DocumentID: dependency.Path, GitPath: dependency.Path, Path: dependency.Path, DocumentKind: "VariableSet", RawYAML: string(dependency.Raw), Document: dependency.Parsed, EditablePointers: []string{"/values"}, LockedPointers: []string{"/apiVersion", "/kind"}})
		}
	}
	w.Header().Set("ETag", config.ETag)
	w.Header().Set("Cache-Control", "private, no-store")
	response := configBundleResponse{Kind: "ConfigBundle", ETag: config.ETag, TargetHeadRevision: targetHead, IndexedRevision: indexedRevision, ConfigRevision: configRevision, Freshness: freshness, Documents: documents, EffectiveVariables: resolution.Effective}
	if bundle != nil {
		response.VariableDependencies = append([]gitprojection.DependencyState(nil), bundle.Dependencies...)
	}
	writeJSON(w, 200, response)
}

func (s *Server) validateDeploymentConfig(w http.ResponseWriter, r *http.Request) {
	deployment, config, bundle, err := s.currentConfig(r, "", 0)
	if err != nil {
		mappedGitProjectionError(w, r, err)
		return
	}
	var change appconfig.Change
	if !decode(w, r, &change) {
		return
	}
	candidate := appconfig.Apply(config.RawYAML, change)
	candidate = s.materializeSchedulingCandidate(r.Context(), currentUser(r.Context()).ID, deployment, config.RawYAML, candidate)
	candidate = s.materializeMiddlewareCandidate(r.Context(), currentUser(r.Context()).ID, deployment, config.RawYAML, candidate)
	candidate.Diagnostics = append(candidate.Diagnostics, s.externalDNSRouteDiagnostics(r.Context(), currentUser(r.Context()).ID, deployment, candidate)...)
	sslipDiagnostics, _ := s.sslipRouteDiagnostics(r.Context(), currentUser(r.Context()).ID, deployment, candidate)
	candidate.Diagnostics = append(candidate.Diagnostics, sslipDiagnostics...)
	if len(candidate.Diagnostics) == 0 {
		resolution, resolutionErr := resolveBundleVariables(bundle, candidate.Runtime)
		if resolutionErr != nil {
			candidate.Diagnostics = append(candidate.Diagnostics, appconfig.Diagnostic{Code: "VariableDependencyInvalid", Detail: "The exact inherited VariableSet snapshot is invalid.", Pointer: "/spec/runtime/env"})
		} else if _, referenceErr := s.resolveAppConfigReferencePlan(r.Context(), currentUser(r.Context()).ID, deployment, resolution.Runtime, candidate.Parsed); referenceErr != nil {
			candidate.Diagnostics = append(candidate.Diagnostics, appConfigReferenceDiagnostic(referenceErr))
		}
	}
	resolution, _ := resolveBundleVariables(bundle, candidate.Runtime)
	writeJSON(w, 200, configValidationResponse{Valid: len(candidate.Diagnostics) == 0, Diagnostics: nonNilDiagnostics(candidate.Diagnostics), EffectiveVariables: resolution.Effective})
}

func (s *Server) previewDeploymentConfig(w http.ResponseWriter, r *http.Request) {
	baseETag, ok := requireIfMatch(w, r)
	if !ok {
		return
	}
	deployment, config, bundle, err := s.currentConfig(r, "", 0)
	if err != nil {
		mappedGitProjectionError(w, r, err)
		return
	}
	if config.ETag != baseETag {
		mappedError(w, r, store.ErrPreconditionFailed)
		return
	}
	var change appconfig.Change
	if !decode(w, r, &change) {
		return
	}
	candidate := appconfig.Apply(config.RawYAML, change)
	candidate = s.materializeSchedulingCandidate(r.Context(), currentUser(r.Context()).ID, deployment, config.RawYAML, candidate)
	candidate = s.materializeMiddlewareCandidate(r.Context(), currentUser(r.Context()).ID, deployment, config.RawYAML, candidate)
	externalDNSUsed := false
	sslipUsed := false
	if len(candidate.Diagnostics) == 0 {
		diagnostics, used := s.externalDNSRouteDiagnosticsWithUsage(r.Context(), currentUser(r.Context()).ID, deployment, candidate)
		candidate.Diagnostics = append(candidate.Diagnostics, diagnostics...)
		externalDNSUsed = used
	}
	if len(candidate.Diagnostics) == 0 {
		diagnostics, used := s.sslipRouteDiagnostics(r.Context(), currentUser(r.Context()).ID, deployment, candidate)
		candidate.Diagnostics = append(candidate.Diagnostics, diagnostics...)
		sslipUsed = used
	}
	var references *store.AppConfigReferencePlan
	resolution, resolutionErr := resolveBundleVariables(bundle, candidate.Runtime)
	if resolutionErr != nil {
		candidate.Diagnostics = append(candidate.Diagnostics, appconfig.Diagnostic{Code: "VariableDependencyInvalid", Detail: "The exact inherited VariableSet snapshot is invalid.", Pointer: "/spec/runtime/env"})
	}
	if len(candidate.Diagnostics) == 0 {
		references, err = s.resolveAppConfigReferencePlan(r.Context(), currentUser(r.Context()).ID, deployment, resolution.Runtime, candidate.Parsed)
		if err != nil {
			if runtimeSecretReferenceUnavailable(err) {
				mappedSecretError(w, r, err)
				return
			}
			candidate.Diagnostics = append(candidate.Diagnostics, appConfigReferenceDiagnostic(err))
		}
	}
	if len(candidate.Diagnostics) > 0 {
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, 422, configValidationResponse{Valid: false, Diagnostics: candidate.Diagnostics})
		return
	}
	if s.appConfigRenderedPreviews == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "AppConfigRendererUnavailable", "Rendered preview unavailable", "The exact pinned kuberploy-runtime renderer is not configured; no preview authority was issued.")
		return
	}
	renderIdentity, renderIdentityDigest, identityErr := s.appConfigRenderedPreviews.Identity()
	if identityErr != nil || renderIdentity.Validate() != nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "AppConfigRendererUnavailable", "Rendered preview unavailable", "The exact pinned kuberploy-runtime renderer identity could not be verified; no preview authority was issued.")
		return
	}
	currentParsed, currentRuntime, currentDiagnostics := appconfig.ParseAndValidate(config.RawYAML)
	if len(currentDiagnostics) != 0 || currentParsed == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "AppConfigRendererUnavailable", "Rendered preview unavailable", "The current AppConfig cannot be rendered under the exact runtime identity.")
		return
	}
	currentResolution, currentResolutionErr := resolveBundleVariables(bundle, currentRuntime)
	if currentResolutionErr != nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "AppConfigRendererUnavailable", "Rendered preview unavailable", "The current effective VariableSet snapshot cannot be rendered.")
		return
	}
	currentValues, valuesErr := effectiveRenderValues(currentParsed, currentResolution.Effective)
	if valuesErr != nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "AppConfigRendererUnavailable", "Rendered preview unavailable", "The current effective AppConfig values could not be materialized.")
		return
	}
	candidateValues, valuesErr := effectiveRenderValues(candidate.Parsed, resolution.Effective)
	if valuesErr != nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "AppConfigRendererUnavailable", "Rendered preview unavailable", "The candidate effective AppConfig values could not be materialized.")
		return
	}
	environment, environmentErr := s.store.GetEnvironmentForActor(r.Context(), currentUser(r.Context()).ID, deployment.EnvironmentID)
	if environmentErr != nil {
		mappedError(w, r, environmentErr)
		return
	}
	releaseName := "kp-preview-" + strings.ReplaceAll(deployment.ApplicationID, "-", "")[:12]
	rendered, renderErr := s.appConfigRenderedPreviews.Render(r.Context(), appconfigpreview.Request{
		Namespace: environment.Namespace, ReleaseName: releaseName, ProjectID: environment.ProjectID,
		EnvironmentID: deployment.EnvironmentID, ApplicationID: deployment.ApplicationID,
		CurrentValues: currentValues, CandidateValues: candidateValues,
	})
	if renderErr != nil || rendered.IdentityDigest != renderIdentityDigest || rendered.Identity != renderIdentity {
		writeProblem(w, r, http.StatusServiceUnavailable, "AppConfigRendererUnavailable", "Rendered preview unavailable", "Deterministic rendering under the exact pinned kuberploy-runtime identity did not complete; no preview authority was issued.")
		return
	}
	rawToken := make([]byte, 32)
	if _, err = rand.Read(rawToken); err != nil {
		writeProblem(w, r, 500, "TokenGenerationFailed", "Preview failed", "A secure preview token could not be generated.")
		return
	}
	tokenHash, err := appconfigpreview.PreviewTokenHash(rawToken, renderIdentityDigest)
	if err != nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "AppConfigRendererUnavailable", "Rendered preview unavailable", "The exact rendered-preview authority is invalid.")
		return
	}
	expires := time.Now().UTC().Add(10 * time.Minute)
	var projection *gitprojection.WritePlan
	if s.gitProjection != nil {
		plan, planErr := s.gitProjection.PlanMutation(r.Context(), currentUser(r.Context()).ID, deployment.EnvironmentID, deployment.ApplicationID, baseETag)
		if planErr != nil {
			mappedGitProjectionError(w, r, planErr)
			return
		}
		projection = &plan
	}
	if err = s.store.CreateDeploymentConfigPreview(r.Context(), currentUser(r.Context()).ID, domain.CreateConfigPreview{DeploymentID: deployment.ID, BaseETag: baseETag, TokenHash: tokenHash[:], CandidateHash: candidate.Hash, CandidateRaw: candidate.Raw, Runtime: candidate.Runtime, ExpiresAt: expires}, projection, references); err != nil {
		mappedDeploymentConfigTransactionError(w, r, err)
		return
	}
	warnings := []string{}
	if len(candidate.Changes) == 0 {
		warnings = append(warnings, "No semantic AppConfig changes were detected.")
	}
	if externalDNSUsed {
		warnings = append(warnings, "The selected External DNS integration revision is protected-Git materialized and freshly observed ready.")
	}
	if sslipUsed {
		warnings = append(warnings, "sslip.io is a third-party convenience hostname for test or backoffice use; use an owned domain for production availability.")
	}
	path := configGitPath(deployment)
	if bundle != nil {
		applicationDocument, found := applicationBundleDocument(*bundle, deployment.ApplicationID)
		if !found {
			mappedGitProjectionError(w, r, gitprojection.ErrInvalid)
			return
		}
		path = applicationDocument.Path
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, configPreviewResponse{PreviewToken: base64.RawURLEncoding.EncodeToString(rawToken), GitDiff: gitops.PreviewAppConfig(path, config.RawYAML, candidate.Raw), RenderedDiff: rendered.RenderedDiff, SemanticChanges: candidate.Changes, Warnings: warnings, ExpiresAt: expires, EffectiveVariables: resolution.Effective, RenderIdentity: renderIdentity, RenderIdentityDigest: renderIdentityDigest})
}

func (s *Server) saveDeploymentConfig(w http.ResponseWriter, r *http.Request) {
	baseETag, ok := requireIfMatch(w, r)
	if !ok {
		return
	}
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	previewToken := strings.TrimSpace(r.Header.Get("Preview-Token"))
	rawToken, err := base64.RawURLEncoding.DecodeString(previewToken)
	if err != nil || len(rawToken) != 32 {
		writeProblem(w, r, 400, "PreviewTokenRequired", "Preview token required", "Provide the exact Preview-Token returned for this draft.")
		return
	}
	var change appconfig.Change
	if !decode(w, r, &change) {
		return
	}
	if s.appConfigRenderedPreviews == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "AppConfigRendererUnavailable", "Rendered preview unavailable", "The exact pinned kuberploy-runtime renderer is not configured.")
		return
	}
	_, renderIdentityDigest, identityErr := s.appConfigRenderedPreviews.Identity()
	if identityErr != nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "AppConfigRendererUnavailable", "Rendered preview unavailable", "The exact rendered-preview authority is no longer available.")
		return
	}
	tokenHash, err := appconfigpreview.PreviewTokenHash(rawToken, renderIdentityDigest)
	if err != nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "AppConfigRendererUnavailable", "Rendered preview unavailable", "The exact rendered-preview authority is invalid.")
		return
	}
	fp := fingerprint(struct {
		DeploymentID, BaseETag, TokenHash string
		Change                            appconfig.Change
	}{r.PathValue("id"), baseETag, hex.EncodeToString(tokenHash[:]), change})
	if runtimeProblem := s.deploymentMutationRuntimeProblem(r.Context()); runtimeProblem != "" {
		invalidPlan := &gitprojection.WritePlan{}
		result, operation, replayErr := s.store.SaveDeploymentConfig(r.Context(), currentUser(r.Context()).ID, key, fp, requestID(r.Context()), domain.SaveDeploymentConfig{
			DeploymentID: r.PathValue("id"), BaseETag: baseETag, TokenHash: tokenHash[:],
		}, invalidPlan)
		if replayErr == nil && result.Replay {
			w.Header().Set("Idempotent-Replay", "true")
			writeJSON(w, 202, operation)
			return
		}
		if errors.Is(replayErr, store.ErrIdempotencyConflict) || errors.Is(replayErr, store.ErrForbidden) || errors.Is(replayErr, store.ErrNotFound) {
			mappedError(w, r, replayErr)
			return
		}
		writeDeploymentMutationRuntimeProblem(w, r, runtimeProblem)
		return
	}
	deployment, config, bundle, err := s.currentConfig(r, "", 0)
	if err != nil {
		mappedGitProjectionError(w, r, err)
		return
	}
	input := domain.SaveDeploymentConfig{DeploymentID: deployment.ID, BaseETag: baseETag, TokenHash: tokenHash[:]}
	if config.ETag != baseETag {
		// A replay is resolved before projection validation by the store. Passing
		// nil here cannot create a projection-mode operation because the current
		// strong Git ETag no longer matches the central legacy validator.
		result, operation, saveErr := s.store.SaveDeploymentConfig(r.Context(), currentUser(r.Context()).ID, key, fp, requestID(r.Context()), input, nil)
		if saveErr != nil {
			mappedError(w, r, saveErr)
			return
		}
		if result.Replay {
			w.Header().Set("Idempotent-Replay", "true")
		}
		writeJSON(w, 202, operation)
		return
	}
	candidate := appconfig.Apply(config.RawYAML, change)
	candidate = s.materializeSchedulingCandidate(r.Context(), currentUser(r.Context()).ID, deployment, config.RawYAML, candidate)
	candidate = s.materializeMiddlewareCandidate(r.Context(), currentUser(r.Context()).ID, deployment, config.RawYAML, candidate)
	candidate.Diagnostics = append(candidate.Diagnostics, s.externalDNSRouteDiagnostics(r.Context(), currentUser(r.Context()).ID, deployment, candidate)...)
	sslipDiagnostics, _ := s.sslipRouteDiagnostics(r.Context(), currentUser(r.Context()).ID, deployment, candidate)
	candidate.Diagnostics = append(candidate.Diagnostics, sslipDiagnostics...)
	if len(candidate.Diagnostics) > 0 {
		writeJSON(w, 422, configValidationResponse{Valid: false, Diagnostics: candidate.Diagnostics})
		return
	}
	resolution, resolutionErr := resolveBundleVariables(bundle, candidate.Runtime)
	if resolutionErr != nil {
		writeJSON(w, 422, configValidationResponse{Valid: false, Diagnostics: []appconfig.Diagnostic{{Code: "VariableDependencyInvalid", Detail: "The exact inherited VariableSet snapshot is invalid.", Pointer: "/spec/runtime/env"}}})
		return
	}
	references, referenceErr := s.resolveAppConfigReferencePlan(r.Context(), currentUser(r.Context()).ID, deployment, resolution.Runtime, candidate.Parsed)
	if referenceErr != nil {
		if runtimeSecretReferenceUnavailable(referenceErr) {
			mappedSecretError(w, r, referenceErr)
			return
		}
		writeJSON(w, 422, configValidationResponse{Valid: false, Diagnostics: []appconfig.Diagnostic{appConfigReferenceDiagnostic(referenceErr)}})
		return
	}
	input.CandidateHash, input.RawYAML, input.Runtime = candidate.Hash, candidate.Raw, candidate.Runtime
	var projection *gitprojection.WritePlan
	if s.gitProjection != nil {
		plan, planErr := s.gitProjection.PlanMutation(r.Context(), currentUser(r.Context()).ID, deployment.EnvironmentID, deployment.ApplicationID, baseETag)
		if planErr != nil {
			mappedGitProjectionError(w, r, planErr)
			return
		}
		projection = &plan
	}
	result, operation, err := s.store.SaveDeploymentConfig(r.Context(), currentUser(r.Context()).ID, key, fp, requestID(r.Context()), input, projection, references)
	if err != nil {
		mappedDeploymentConfigTransactionError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeJSON(w, 202, operation)
}

func mappedDeploymentConfigTransactionError(w http.ResponseWriter, r *http.Request, err error) {
	// The transaction repeats certificate observation and provider readiness
	// under locks after the HTTP precheck. A disappearing observation is an
	// expected retryable dependency failure, not an internal server error.
	if runtimeSecretReferenceUnavailable(err) {
		mappedSecretError(w, r, err)
		return
	}
	mappedError(w, r, err)
}

var (
	configETagPattern        = regexp.MustCompile(`^"(?:cfg-sha256-|sha256:)[0-9a-f]{64}"$`)
	gitProjectionETagPattern = regexp.MustCompile(`^"sha256:[0-9a-f]{64}"$`)
	gitRevisionPattern       = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	canonicalSecondsPattern  = regexp.MustCompile(`^(?:0|[1-9][0-9]?)$`)
)

func requireIfMatch(w http.ResponseWriter, r *http.Request) (string, bool) {
	value := strings.TrimSpace(r.Header.Get("If-Match"))
	if value == "" {
		writeProblem(w, r, 428, "PreconditionRequired", "Precondition required", "Provide the strong ETag from GET /config in If-Match.")
		return "", false
	}
	if !configETagPattern.MatchString(value) {
		writeProblem(w, r, 400, "InvalidPrecondition", "Invalid precondition", "If-Match must contain the exact strong Kuberploy configuration ETag.")
		return "", false
	}
	return value, true
}

func configReadFence(w http.ResponseWriter, r *http.Request, projectionEnabled bool) (string, time.Duration, bool) {
	query := r.URL.Query()
	atLeast := strings.TrimSpace(query.Get("atLeastRevision"))
	rawWait := strings.TrimSpace(query.Get("waitSeconds"))
	if !projectionEnabled && (atLeast != "" || rawWait != "") {
		writeProblem(w, r, 422, "GitProjectionDisabled", "Git projection is disabled", "Revision convergence parameters are available only when Git projection mode is enabled.")
		return "", 0, false
	}
	if atLeast == "" {
		if rawWait != "" {
			writeProblem(w, r, 422, "InvalidRevisionFence", "Invalid revision fence", "waitSeconds requires atLeastRevision.")
			return "", 0, false
		}
		return "", 0, true
	}
	if !gitRevisionPattern.MatchString(atLeast) {
		writeProblem(w, r, 422, "InvalidRevisionFence", "Invalid revision fence", "atLeastRevision must be an exact lowercase Git commit ID.")
		return "", 0, false
	}
	waitSeconds := int(gitprojection.DefaultBundleWait / time.Second)
	if rawWait != "" {
		if !canonicalSecondsPattern.MatchString(rawWait) {
			writeProblem(w, r, 422, "InvalidRevisionFence", "Invalid revision fence", "waitSeconds must be a canonical integer from 0 through 10.")
			return "", 0, false
		}
		parsed, err := strconv.Atoi(rawWait)
		if err != nil || parsed < 0 || parsed > int(gitprojection.MaximumBundleWait/time.Second) || strconv.Itoa(parsed) != rawWait {
			writeProblem(w, r, 422, "InvalidRevisionFence", "Invalid revision fence", "waitSeconds must be a canonical integer from 0 through 10.")
			return "", 0, false
		}
		waitSeconds = parsed
	}
	return atLeast, time.Duration(waitSeconds) * time.Second, true
}

func mappedGitProjectionError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrForbidden), errors.Is(err, store.ErrConflict),
		errors.Is(err, store.ErrIdempotencyConflict), errors.Is(err, store.ErrPreconditionFailed),
		errors.Is(err, store.ErrPreviewExpired), errors.Is(err, store.ErrPreviewConsumed), errors.Is(err, store.ErrPreviewInvalid),
		errors.Is(err, store.ErrConfigProjectionMissing):
		mappedError(w, r, err)
	case errors.Is(err, gitprojection.ErrNotFound):
		writeProblem(w, r, 404, "GitConfigNotFound", "Git configuration not found", "The authorized indexed Git tree does not contain this application's AppConfig path.")
	case errors.Is(err, gitprojection.ErrPreconditionRequired):
		writeProblem(w, r, 428, "PreconditionRequired", "Precondition required", "Provide the exact strong Git bundle ETag in If-Match.")
	case errors.Is(err, gitprojection.ErrConflict):
		writeProblem(w, r, 412, "PreconditionFailed", "Precondition failed", "The indexed Git AppConfig changed. Reload the Git bundle and retry with its exact ETag.")
	case errors.Is(err, gitprojection.ErrStale), errors.Is(err, gitprojection.ErrRuntimeNotReady), errors.Is(err, gitprojection.ErrLeaseHeld), errors.Is(err, gitprojection.ErrLeaseLost):
		writeProblem(w, r, 503, "GitProjectionNotReady", "Git projection not ready", "The requested Git revision is not indexed within the bounded wait or the exact projection runtime is unavailable.")
	case errors.Is(err, gitprojection.ErrProtectionUnavailable):
		writeProblem(w, r, 503, "ProtectedGitPolicyUnavailable", "Protected Git policy unavailable", "The exact target branch protection and App-only writer policy could not be freshly verified.")
	case errors.Is(err, gitprojection.ErrInvalid), errors.Is(err, gitprojection.ErrProviderMismatch), errors.Is(err, gitprojection.ErrDiverged), errors.Is(err, gitprojection.ErrMissingRef):
		writeProblem(w, r, 503, "GitProjectionUnavailable", "Git projection unavailable", "The authorized Git binding is not in a safe state for this request.")
	default:
		writeProblem(w, r, 503, "GitProjectionUnavailable", "Git projection unavailable", "The indexed Git configuration could not be read or safely updated.")
	}
}

func nonNilDiagnostics(value []appconfig.Diagnostic) []appconfig.Diagnostic {
	if value == nil {
		return []appconfig.Diagnostic{}
	}
	return value
}

func effectiveRenderValues(parsed map[string]any, effective []variables.Effective) ([]byte, error) {
	encoded, err := json.Marshal(parsed)
	if err != nil || len(encoded) == 0 {
		return nil, appconfigpreview.ErrInvalid
	}
	var cloned map[string]any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err = decoder.Decode(&cloned); err != nil || cloned == nil {
		return nil, appconfigpreview.ErrInvalid
	}
	clonedValue, err := normalizeRenderNumbers(cloned)
	if err != nil {
		return nil, appconfigpreview.ErrInvalid
	}
	cloned, _ = clonedValue.(map[string]any)
	values := make(map[string]any)
	for _, value := range effective {
		if value.Value != nil {
			values[value.Name] = *value.Value
		}
	}
	if len(values) == 0 {
		delete(cloned, "values")
	} else {
		cloned["values"] = values
	}
	raw, err := yaml.Marshal(cloned)
	if err != nil || len(raw) == 0 || len(raw) > appconfigpreview.MaximumInputBytes {
		return nil, appconfigpreview.ErrInvalid
	}
	return raw, nil
}

func normalizeRenderNumbers(value any) (any, error) {
	switch typed := value.(type) {
	case json.Number:
		if integer, err := typed.Int64(); err == nil {
			return integer, nil
		}
		decimal, err := typed.Float64()
		if err != nil || math.IsNaN(decimal) || math.IsInf(decimal, 0) {
			return nil, appconfigpreview.ErrInvalid
		}
		return decimal, nil
	case map[string]any:
		for key, nested := range typed {
			normalized, err := normalizeRenderNumbers(nested)
			if err != nil {
				return nil, err
			}
			typed[key] = normalized
		}
		return typed, nil
	case []any:
		for index, nested := range typed {
			normalized, err := normalizeRenderNumbers(nested)
			if err != nil {
				return nil, err
			}
			typed[index] = normalized
		}
		return typed, nil
	default:
		return value, nil
	}
}

func (s *Server) externalDNSRouteDiagnostics(ctx context.Context, actor string, deployment domain.Deployment, candidate appconfig.Candidate) []appconfig.Diagnostic {
	diagnostics, _ := s.externalDNSRouteDiagnosticsWithUsage(ctx, actor, deployment, candidate)
	return diagnostics
}

func (s *Server) externalDNSRouteDiagnosticsWithUsage(ctx context.Context, actor string, deployment domain.Deployment, candidate appconfig.Candidate) ([]appconfig.Diagnostic, bool) {
	if len(candidate.Diagnostics) != 0 || candidate.Parsed == nil {
		return nil, false
	}
	spec, _ := candidate.Parsed["spec"].(map[string]any)
	routes, _ := spec["routes"].([]any)
	diagnostics := make([]appconfig.Diagnostic, 0)
	used := false
	for index, rawRoute := range routes {
		route, _ := rawRoute.(map[string]any)
		dns, _ := route["dns"].(map[string]any)
		if mode, _ := dns["mode"].(string); mode != "externalDns" {
			continue
		}
		used = true
		pointer := "/spec/routes/" + strconv.Itoa(index)
		if s.externalDNS == nil || s.externalDNS.service == nil {
			diagnostics = append(diagnostics, appconfig.Diagnostic{Code: "ExternalDNSConfigurationUnavailable", Detail: "External DNS integration configuration is unavailable.", Pointer: pointer + "/dns/integrationRef"})
			continue
		}
		integrationRef, _ := dns["integrationRef"].(string)
		hostname, _ := route["host"].(string)
		err := s.externalDNS.service.ValidateApplicationRoute(ctx, actor, deployment.ApplicationID, deployment.EnvironmentID, integrationRef, hostname)
		switch {
		case err == nil && !s.externalDNS.runtimeReady(ctx):
			diagnostics = append(diagnostics, appconfig.Diagnostic{Code: "ExternalDNSRuntimeUnavailable", Detail: "The exact External DNS integration revision is not freshly observed and ready.", Pointer: pointer + "/dns/integrationRef"})
		case err == nil:
		case errors.Is(err, externaldns.ErrIntegrationReference), errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrForbidden):
			diagnostics = append(diagnostics, appconfig.Diagnostic{Code: "ExternalDNSIntegrationUnavailable", Detail: "Select an External DNS integration authorized for this application and environment.", Pointer: pointer + "/dns/integrationRef"})
		case errors.Is(err, externaldns.ErrHostnameNotAllowed):
			diagnostics = append(diagnostics, appconfig.Diagnostic{Code: "ExternalDNSHostnameNotAllowed", Detail: "The route hostname must be within an allowed domain suffix for the selected integration.", Pointer: pointer + "/host"})
		default:
			diagnostics = append(diagnostics, appconfig.Diagnostic{Code: "ExternalDNSCatalogUnavailable", Detail: "The authorized External DNS catalog could not be verified.", Pointer: pointer + "/dns/integrationRef"})
		}
	}
	return diagnostics, used
}
