package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/gitops"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/store"
	"github.com/kuberploy/kuberploy/internal/variables"
)

type variableSetInput struct {
	RawYAML string `json:"rawYaml"`
}

type variableSetPreviewResponse struct {
	PreviewToken string                 `json:"previewToken"`
	Scope        string                 `json:"scope"`
	Path         string                 `json:"path"`
	GitDiff      string                 `json:"gitDiff"`
	Document     map[string]any         `json:"document"`
	Diagnostics  []variables.Diagnostic `json:"diagnostics"`
	ExpiresAt    time.Time              `json:"expiresAt"`
}

func (s *Server) variableSets(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.variableProjectionBackend(w, r)
	if !ok {
		return
	}
	snapshots, err := backend.VariableSets(r.Context(), currentUser(r.Context()).ID, r.PathValue("id"))
	if err != nil {
		mappedGitProjectionError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	collection(w, snapshots)
}

func (s *Server) previewVariableSet(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.variableProjectionBackend(w, r)
	if !ok || !s.variableProjectionReady(w, r) {
		return
	}
	var input variableSetInput
	if !decode(w, r, &input) {
		return
	}
	raw := []byte(input.RawYAML)
	document, diagnostics := variables.ParseAndValidate(raw)
	if len(diagnostics) != 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"valid": false, "diagnostics": diagnostics})
		return
	}
	expectedETag := strings.TrimSpace(r.Header.Get("If-Match"))
	if expectedETag != "" && !gitProjectionETagPattern.MatchString(expectedETag) {
		writeProblem(w, r, http.StatusBadRequest, "InvalidPrecondition", "Invalid precondition", "If-Match must contain the exact VariableSet content ETag.")
		return
	}
	actor, environmentID, scope := currentUser(r.Context()).ID, r.PathValue("id"), r.PathValue("scope")
	plan, err := backend.PlanVariableMutation(r.Context(), actor, environmentID, scope, expectedETag)
	if err != nil {
		mappedGitProjectionError(w, r, err)
		return
	}
	if plan.EnvironmentID != environmentID || plan.VariableScope != scope {
		mappedGitProjectionError(w, r, gitprojection.ErrProviderMismatch)
		return
	}
	rawToken := make([]byte, 32)
	if _, err = rand.Read(rawToken); err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "TokenGenerationFailed", "Preview failed", "A secure preview token could not be generated.")
		return
	}
	tokenHash, candidateHash := sha256.Sum256(rawToken), sha256.Sum256(raw)
	expiresAt := time.Now().UTC().Add(10 * time.Minute)
	if err = s.store.CreateVariableSetPreview(r.Context(), actor, plan, tokenHash[:], candidateHash[:], expiresAt); err != nil {
		mappedGitProjectionError(w, r, err)
		return
	}
	// Reload the indexed snapshot after persisting the preview authority. A
	// concurrent index may have advanced between planning and this read; never
	// return a token paired with a diff from a different Git revision.
	snapshots, snapshotErr := backend.VariableSets(r.Context(), actor, environmentID)
	if snapshotErr != nil {
		mappedGitProjectionError(w, r, snapshotErr)
		return
	}
	current := []byte(nil)
	matched := false
	for _, snapshot := range snapshots {
		if snapshot.Scope != scope {
			continue
		}
		matched = snapshot.BindingID == plan.BindingID && snapshot.ProjectID == plan.ProjectID &&
			snapshot.EnvironmentID == plan.EnvironmentID && snapshot.Path == plan.VariablePath && snapshot.IndexedRevision == plan.BaseRevision
		if plan.Precondition == gitprojection.MutationCreateIfAbsent {
			matched = matched && !snapshot.Present && snapshot.ETag == ""
		} else {
			matched = matched && snapshot.Present && snapshot.ETag == plan.ExpectedETag
			current = []byte(snapshot.RawYAML)
		}
		break
	}
	if !matched {
		mappedError(w, r, store.ErrConflict)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, variableSetPreviewResponse{PreviewToken: base64.RawURLEncoding.EncodeToString(rawToken), Scope: scope,
		Path: plan.VariablePath, GitDiff: gitops.PreviewAppConfig(plan.VariablePath, current, raw), Document: document.Parsed,
		Diagnostics: []variables.Diagnostic{}, ExpiresAt: expiresAt})
}

func (s *Server) saveVariableSet(w http.ResponseWriter, r *http.Request) {
	backend, configured := s.variableProjectionBackend(w, r)
	if !configured {
		return
	}
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	previewToken := strings.TrimSpace(r.Header.Get("Preview-Token"))
	rawToken, err := base64.RawURLEncoding.DecodeString(previewToken)
	if err != nil || len(rawToken) != 32 {
		writeProblem(w, r, http.StatusBadRequest, "PreviewTokenRequired", "Preview token required", "Provide the exact Preview-Token returned for this VariableSet draft.")
		return
	}
	var input variableSetInput
	if !decode(w, r, &input) {
		return
	}
	actor, environmentID, scope := currentUser(r.Context()).ID, r.PathValue("id"), r.PathValue("scope")
	tokenHash, candidateHash := sha256.Sum256(rawToken), sha256.Sum256([]byte(input.RawYAML))
	authority, previewCandidate, err := s.store.VariableSetPreviewAuthority(r.Context(), actor, tokenHash[:])
	if err != nil {
		mappedError(w, r, err)
		return
	}
	if authority.EnvironmentID != environmentID || authority.VariableScope != scope || !bytes.Equal(previewCandidate, candidateHash[:]) {
		mappedError(w, r, store.ErrPreviewInvalid)
		return
	}
	requestDigest := variableSaveRequestFingerprint(actor, environmentID, scope, authority, tokenHash[:], candidateHash[:])
	if !s.variableProjectionRuntimeReady(r.Context()) {
		result, replayErr := s.store.SaveVariableSet(r.Context(), actor, key, requestDigest, requestID(r.Context()), gitprojection.WritePlan{}, tokenHash[:], candidateHash[:], nil)
		if replayErr == nil && result.Replay {
			w.Header().Set("Idempotent-Replay", "true")
			w.Header().Set("Location", "/v1/operations/"+result.Value.ID)
			writeJSON(w, http.StatusAccepted, result.Value)
			return
		}
		if errors.Is(replayErr, store.ErrIdempotencyConflict) {
			mappedError(w, r, replayErr)
			return
		}
		writeProblem(w, r, http.StatusServiceUnavailable, "GitProjectionRuntimeUnavailable", "Git projection unavailable", "No matching Git projection worker has reported a fresh exact runtime observation.")
		return
	}
	// The preview's immutable authority is revalidated transactionally by the
	// store. This exact comparison also rejects any caller-selected path/mode.
	currentPlan, planErr := backend.PlanVariableMutation(r.Context(), actor, environmentID, scope, authority.ExpectedETag)
	if planErr != nil || !reflect.DeepEqual(currentPlan, authority) {
		result, replayErr := s.store.SaveVariableSet(r.Context(), actor, key, requestDigest, requestID(r.Context()), gitprojection.WritePlan{}, tokenHash[:], candidateHash[:], nil)
		if replayErr == nil && result.Replay {
			w.Header().Set("Idempotent-Replay", "true")
			w.Header().Set("Location", "/v1/operations/"+result.Value.ID)
			writeJSON(w, http.StatusAccepted, result.Value)
			return
		}
		if planErr != nil {
			mappedGitProjectionError(w, r, planErr)
		} else {
			mappedGitProjectionError(w, r, gitprojection.ErrConflict)
		}
		return
	}
	result, err := s.store.SaveVariableSet(r.Context(), actor, key, requestDigest, requestID(r.Context()), authority, tokenHash[:], candidateHash[:], []byte(input.RawYAML))
	if err != nil {
		mappedGitProjectionError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Location", "/v1/operations/"+result.Value.ID)
	writeJSON(w, http.StatusAccepted, result.Value)
}

func variableSaveRequestFingerprint(actor, environmentID, scope string, authority gitprojection.WritePlan, tokenHash, candidateHash []byte) string {
	return "sha256:" + fingerprint(struct {
		ActorID, EnvironmentID, Scope, BindingID, ProjectID, Path, BaseRevision, Precondition, ExpectedETag, PolicyVersion string
		TokenHash, CandidateHash                                                                                           string
	}{actor, environmentID, scope, authority.BindingID, authority.ProjectID, authority.VariablePath, authority.BaseRevision,
		string(authority.Precondition), authority.ExpectedETag, authority.PolicyVersion, hex.EncodeToString(tokenHash), hex.EncodeToString(candidateHash)})
}

func (s *Server) variableProjectionConfigured(w http.ResponseWriter, r *http.Request) bool {
	_, ok := s.gitProjection.(VariableSetBackend)
	if !ok {
		writeProblem(w, r, http.StatusServiceUnavailable, "GitProjectionUnavailable", "Git projection unavailable", "Git-backed VariableSet management is not configured.")
		return false
	}
	return true
}

func (s *Server) variableProjectionBackend(w http.ResponseWriter, r *http.Request) (VariableSetBackend, bool) {
	if !s.variableProjectionConfigured(w, r) {
		return nil, false
	}
	backend, _ := s.gitProjection.(VariableSetBackend)
	return backend, true
}

func (s *Server) variableProjectionRuntimeReady(ctx context.Context) bool {
	if s.gitProjection == nil || s.gitReadiness == nil {
		return false
	}
	probeContext, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return s.gitReadiness.Probe(probeContext) == nil
}

func (s *Server) variableProjectionReady(w http.ResponseWriter, r *http.Request) bool {
	if !s.variableProjectionConfigured(w, r) {
		return false
	}
	if !s.variableProjectionRuntimeReady(r.Context()) {
		writeProblem(w, r, http.StatusServiceUnavailable, "GitProjectionRuntimeUnavailable", "Git projection unavailable", "No matching Git projection worker has reported a fresh exact runtime observation.")
		return false
	}
	return true
}
