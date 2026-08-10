package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/helmapps"
	"github.com/kuberploy/kuberploy/internal/store"
)

type HelmApplicationBackend interface {
	helmapps.ReleaseService
	helmapps.ReleaseValuesService
	Capabilities(context.Context) (helmapps.Capabilities, error)
}

type HelmApprovalAdmissionBackend interface {
	Catalog(context.Context, int) ([]helmapps.ApprovalDocument, error)
	Admit(context.Context, helmapps.ApprovalAdmissionRequest) (helmapps.ApprovalDocument, bool, error)
}

type HelmRenderedManifestPreviewBackend interface {
	Preview(context.Context, helmapps.ReleaseTarget) (helmapps.RenderedManifestPreview, error)
}

type helmApprovalKeyView struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
}

type helmApprovalView struct {
	ID                 string          `json:"id"`
	Revision           int64           `json:"revision"`
	Repository         string          `json:"repository"`
	Version            string          `json:"version"`
	ManifestDigest     string          `json:"manifestDigest"`
	PackageDigest      string          `json:"packageDigest"`
	ValuesSchemaDigest string          `json:"valuesSchemaDigest"`
	RendererImage      string          `json:"rendererImage"`
	RendererVersion    string          `json:"rendererVersion"`
	PolicyVersion      string          `json:"policyVersion"`
	DocumentsDigest    string          `json:"documentsDigest"`
	ValuesSchema       json.RawMessage `json:"valuesSchema"`
	DefaultValuesYAML  string          `json:"defaultValuesYaml"`
	CreatedAt          time.Time       `json:"createdAt"`
}

type helmReleaseRevisionView struct {
	ID                       string              `json:"id"`
	Generation               int64               `json:"generation"`
	ReleaseName              string              `json:"releaseName"`
	Action                   string              `json:"action"`
	DesiredEnabled           bool                `json:"desiredEnabled"`
	ParentRevisionID         string              `json:"parentRevisionId,omitempty"`
	RollbackSourceRevisionID string              `json:"rollbackSourceRevisionId,omitempty"`
	Approval                 helmApprovalKeyView `json:"approval"`
	RenderCommandID          string              `json:"renderCommandId,omitempty"`
	ValuesDigest             string              `json:"valuesDigest"`
	IntentDigest             string              `json:"intentDigest"`
	RequestID                string              `json:"requestId"`
	CreatedAt                time.Time           `json:"createdAt"`
}

type helmReleaseStatusView struct {
	Revision            helmReleaseRevisionView `json:"revision"`
	Phase               string                  `json:"phase"`
	RenderState         string                  `json:"renderState,omitempty"`
	PayloadIntentID     string                  `json:"payloadIntentId,omitempty"`
	PayloadState        string                  `json:"payloadState,omitempty"`
	PayloadRevision     string                  `json:"payloadRevision,omitempty"`
	ApplicationIntentID string                  `json:"applicationIntentId,omitempty"`
	ApplicationState    string                  `json:"applicationState,omitempty"`
	ApplicationRevision string                  `json:"applicationRevision,omitempty"`
	FailureCode         string                  `json:"failureCode,omitempty"`
}

type helmRenderedResourceView struct {
	APIVersion     string `json:"apiVersion"`
	Kind           string `json:"kind"`
	Namespace      string `json:"namespace"`
	Name           string `json:"name"`
	SanitizedYAML  string `json:"sanitizedYaml,omitempty"`
	PreviewOmitted bool   `json:"previewOmitted"`
}

type helmRenderedManifestPreviewView struct {
	ReleaseRevisionID string                     `json:"releaseRevisionId"`
	Generation        int64                      `json:"generation"`
	ManifestDigest    string                     `json:"manifestDigest"`
	InventoryDigest   string                     `json:"inventoryDigest"`
	ResourceCount     int                        `json:"resourceCount"`
	PreviewBytes      int                        `json:"previewBytes"`
	Resources         []helmRenderedResourceView `json:"resources"`
}

func helmApprovalDocumentView(document helmapps.ApprovalDocument) helmApprovalView {
	approval := document.Approval
	return helmApprovalView{ID: approval.ID, Revision: approval.Revision,
		Repository: approval.OCIRepository, Version: approval.ChartVersion,
		ManifestDigest: approval.ManifestDigest, PackageDigest: approval.PackageDigest,
		ValuesSchemaDigest: approval.ValuesSchemaDigest, RendererImage: approval.RendererImage,
		RendererVersion: approval.RendererVersion, PolicyVersion: approval.PolicyVersion,
		DocumentsDigest: document.DocumentsDigest, ValuesSchema: append(json.RawMessage(nil), document.ValuesSchemaJSON...),
		DefaultValuesYAML: string(document.DefaultValuesYAML), CreatedAt: document.CreatedAt}
}

func helmReleaseView(value helmapps.ReleaseRevision) helmReleaseRevisionView {
	return helmReleaseRevisionView{ID: value.ID, Generation: value.Generation,
		ReleaseName: value.ReleaseName, Action: string(value.Action), DesiredEnabled: value.DesiredEnabled,
		ParentRevisionID: value.ParentRevisionID, RollbackSourceRevisionID: value.RollbackSourceRevisionID,
		Approval:        helmApprovalKeyView{ID: value.Approval.ID, Revision: value.Approval.Revision},
		RenderCommandID: value.RenderCommandID, ValuesDigest: value.ValuesDigest,
		IntentDigest: value.IntentDigest, RequestID: value.RequestID, CreatedAt: value.CreatedAt}
}

func helmStatusView(value helmapps.ReleaseStatus) helmReleaseStatusView {
	return helmReleaseStatusView{Revision: helmReleaseView(value.Revision), Phase: string(value.Phase),
		RenderState: value.RenderState, PayloadIntentID: value.PayloadIntentID,
		PayloadState: value.PayloadState, PayloadRevision: value.PayloadRevision,
		ApplicationIntentID: value.ApplicationIntentID, ApplicationState: value.ApplicationState,
		ApplicationRevision: value.ApplicationRevision, FailureCode: value.FailureCode}
}

func (s *Server) helmApprovalCatalog(w http.ResponseWriter, r *http.Request) {
	_, target, ok := s.authorizedHelmTarget(w, r, domain.PermissionHelmRead)
	if !ok || !s.helmConfigured(w, r) {
		return
	}
	_ = target
	limit, ok := parseHelmLimit(w, r, 50)
	if !ok {
		return
	}
	documents, err := s.helmApplications.ApprovalCatalog(r.Context(), limit)
	if err != nil {
		writeHelmError(w, r, err)
		return
	}
	items := make([]helmApprovalView, 0, len(documents))
	for _, document := range documents {
		if document.Validate() != nil {
			writeHelmError(w, r, helmapps.ErrConflict)
			return
		}
		items = append(items, helmApprovalDocumentView(document))
	}
	w.Header().Set("Cache-Control", "private, no-store")
	collection(w, items)
}

func (s *Server) platformHelmApprovals(w http.ResponseWriter, r *http.Request) {
	if s.helmApprovals == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "HelmApprovalAdmissionUnavailable", "Helm approval admission unavailable", "The verified OCI Helm approval boundary is not configured.")
		return
	}
	if r.Method == http.MethodGet {
		limit, ok := parseHelmLimit(w, r, 50)
		if !ok {
			return
		}
		documents, err := s.helmApprovals.Catalog(r.Context(), limit)
		if err != nil {
			writeHelmError(w, r, err)
			return
		}
		items := make([]helmApprovalView, 0, len(documents))
		for _, document := range documents {
			if document.Validate() != nil {
				writeHelmError(w, r, helmapps.ErrConflict)
				return
			}
			items = append(items, helmApprovalDocumentView(document))
		}
		w.Header().Set("Cache-Control", "private, no-store")
		collection(w, items)
		return
	}
	key, ok := helmIdempotencyKey(w, r)
	if !ok {
		return
	}
	var input struct {
		Repository         string `json:"repository"`
		Version            string `json:"version"`
		ManifestDigest     string `json:"manifestDigest"`
		PackageDigest      string `json:"packageDigest"`
		ValuesSchemaDigest string `json:"valuesSchemaDigest"`
	}
	if !decodeHelmRequest(w, r, &input) {
		return
	}
	document, replay, err := s.helmApprovals.Admit(r.Context(), helmapps.ApprovalAdmissionRequest{
		ActorID: currentUser(r.Context()).ID, IdempotencyKey: key,
		OCIRepository: input.Repository, ChartVersion: input.Version,
		ManifestDigest: input.ManifestDigest, PackageDigest: input.PackageDigest,
		ValuesSchemaDigest: input.ValuesSchemaDigest})
	if err != nil {
		writeHelmError(w, r, err)
		return
	}
	if replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusCreated, helmApprovalDocumentView(document))
}

type helmValuesInput struct {
	ApprovalID       string `json:"approvalId"`
	ApprovalRevision int64  `json:"approvalRevision"`
	ValuesYAML       string `json:"valuesYaml"`
}

func (s *Server) helmValuesPreview(w http.ResponseWriter, r *http.Request) {
	target, _, ok := s.authorizedHelmTarget(w, r, domain.PermissionHelmRead)
	if !ok || !s.helmConfigured(w, r) {
		return
	}
	var input helmValuesInput
	if !decodeHelmRequest(w, r, &input) {
		return
	}
	preview, err := s.helmApplications.PreviewValues(r.Context(), target,
		helmapps.ApprovalKey{ID: strings.TrimSpace(input.ApprovalID), Revision: input.ApprovalRevision}, []byte(input.ValuesYAML))
	if err != nil {
		writeHelmError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, map[string]any{"approval": helmApprovalKeyView{ID: preview.Approval.ID, Revision: preview.Approval.Revision},
		"normalizedValuesYaml": preview.NormalizedValuesYAML, "valuesDigest": preview.ValuesDigest,
		"currentValuesDigest": preview.CurrentValuesDigest, "effectiveValues": preview.EffectiveValues,
		"changedPaths": preview.ChangedPaths})
}

func (s *Server) helmUpsert(w http.ResponseWriter, r *http.Request) {
	target, _, ok := s.authorizedHelmTarget(w, r, domain.PermissionHelmDeploy)
	if !ok || !s.helmMutationReady(w, r, false) {
		return
	}
	key, ok := helmIdempotencyKey(w, r)
	if !ok {
		return
	}
	var input helmValuesInput
	if !decodeHelmRequest(w, r, &input) {
		return
	}
	value, replay, err := s.helmApplications.Upsert(r.Context(), helmapps.UpsertReleaseRequest{
		Target: target, Actor: helmActor(r, key), Approval: helmapps.ApprovalKey{ID: strings.TrimSpace(input.ApprovalID), Revision: input.ApprovalRevision},
		ValuesYAML: []byte(input.ValuesYAML)}, time.Now().UTC())
	s.writeHelmMutation(w, r, value, replay, err)
}

func (s *Server) helmHead(w http.ResponseWriter, r *http.Request) {
	target, _, ok := s.authorizedHelmTarget(w, r, domain.PermissionHelmRead)
	if !ok || !s.helmConfigured(w, r) {
		return
	}
	status, err := s.helmApplications.Head(r.Context(), target)
	if err != nil {
		writeHelmError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, helmStatusView(status))
}

func (s *Server) helmHistory(w http.ResponseWriter, r *http.Request) {
	target, _, ok := s.authorizedHelmTarget(w, r, domain.PermissionHelmRead)
	if !ok || !s.helmConfigured(w, r) {
		return
	}
	limit, ok := parseHelmLimit(w, r, 25)
	if !ok {
		return
	}
	history, err := s.helmApplications.History(r.Context(), target, limit)
	if err != nil {
		writeHelmError(w, r, err)
		return
	}
	items := make([]helmReleaseStatusView, len(history))
	for index := range history {
		items[index] = helmStatusView(history[index])
	}
	w.Header().Set("Cache-Control", "private, no-store")
	collection(w, items)
}

func (s *Server) helmRenderedPreview(w http.ResponseWriter, r *http.Request) {
	target, _, ok := s.authorizedHelmTarget(w, r, domain.PermissionHelmRead)
	if !ok {
		return
	}
	if s.helmRenderedPreviews == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "HelmRenderedPreviewUnavailable", "Helm rendered preview unavailable", "The bounded rendered-resource preview is not configured.")
		return
	}
	preview, err := s.helmRenderedPreviews.Preview(r.Context(), target)
	if err != nil {
		writeHelmError(w, r, err)
		return
	}
	previewBytes := 0
	if preview.ResourceCount != len(preview.Resources) || preview.ResourceCount < 1 ||
		preview.ResourceCount > helmapps.MaximumResources {
		writeHelmError(w, r, helmapps.ErrConflict)
		return
	}
	for _, resource := range preview.Resources {
		if resource.PreviewOmitted {
			if resource.SanitizedYAML != "" {
				writeHelmError(w, r, helmapps.ErrConflict)
				return
			}
			continue
		}
		if resource.SanitizedYAML == "" || len(resource.SanitizedYAML) > helmapps.MaximumSanitizedResourcePreviewBytes {
			writeHelmError(w, r, helmapps.ErrConflict)
			return
		}
		previewBytes += len(resource.SanitizedYAML)
	}
	if previewBytes != preview.PreviewBytes || previewBytes > helmapps.MaximumSanitizedManifestPreviewBytes {
		writeHelmError(w, r, helmapps.ErrConflict)
		return
	}
	resources := make([]helmRenderedResourceView, len(preview.Resources))
	for index, resource := range preview.Resources {
		resources[index] = helmRenderedResourceView{APIVersion: resource.APIVersion,
			Kind: resource.Kind, Namespace: resource.Namespace, Name: resource.Name,
			SanitizedYAML: resource.SanitizedYAML, PreviewOmitted: resource.PreviewOmitted}
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, helmRenderedManifestPreviewView{
		ReleaseRevisionID: preview.ReleaseRevisionID, Generation: preview.Generation,
		ManifestDigest: preview.ManifestDigest, InventoryDigest: preview.InventoryDigest,
		ResourceCount: preview.ResourceCount, PreviewBytes: preview.PreviewBytes, Resources: resources})
}

func (s *Server) helmRetry(w http.ResponseWriter, r *http.Request) {
	target, _, ok := s.authorizedHelmTarget(w, r, domain.PermissionHelmRetry)
	if !ok || !s.helmMutationReady(w, r, false) {
		return
	}
	key, ok := helmIdempotencyKey(w, r)
	if !ok || !decodeHelmEmptyRequest(w, r) {
		return
	}
	value, replay, err := s.helmApplications.Retry(r.Context(), helmapps.RetryReleaseRequest{Target: target, Actor: helmActor(r, key)}, time.Now().UTC())
	s.writeHelmMutation(w, r, value, replay, err)
}

func (s *Server) helmDisable(w http.ResponseWriter, r *http.Request) {
	target, _, ok := s.authorizedHelmTarget(w, r, domain.PermissionHelmDeploy)
	if !ok || !s.helmMutationReady(w, r, false) {
		return
	}
	key, ok := helmIdempotencyKey(w, r)
	if !ok || !decodeHelmEmptyRequest(w, r) {
		return
	}
	value, replay, err := s.helmApplications.Disable(r.Context(), helmapps.DisableReleaseRequest{Target: target, Actor: helmActor(r, key)}, time.Now().UTC())
	s.writeHelmMutation(w, r, value, replay, err)
}

func (s *Server) helmRollback(w http.ResponseWriter, r *http.Request) {
	target, _, ok := s.authorizedHelmTarget(w, r, domain.PermissionHelmRollback)
	if !ok || !s.helmMutationReady(w, r, true) {
		return
	}
	key, ok := helmIdempotencyKey(w, r)
	if !ok {
		return
	}
	var input struct {
		SourceRevisionID string `json:"sourceRevisionId"`
	}
	if !decodeHelmRequest(w, r, &input) {
		return
	}
	value, replay, err := s.helmApplications.Rollback(r.Context(), helmapps.RollbackReleaseRequest{
		Target: target, Actor: helmActor(r, key), SourceRevisionID: strings.TrimSpace(input.SourceRevisionID)}, time.Now().UTC())
	s.writeHelmMutation(w, r, value, replay, err)
}

func (s *Server) authorizedHelmTarget(w http.ResponseWriter, r *http.Request, permission domain.Permission) (helmapps.ReleaseTarget, domain.AccessTarget, bool) {
	applicationID, environmentID := strings.TrimSpace(r.PathValue("id")), strings.TrimSpace(r.PathValue("environmentId"))
	if !validUUID(applicationID) || !validUUID(environmentID) {
		mappedError(w, r, store.ErrNotFound)
		return helmapps.ReleaseTarget{}, domain.AccessTarget{}, false
	}
	application, err := s.store.GetApplication(r.Context(), applicationID)
	if err != nil {
		mappedError(w, r, err)
		return helmapps.ReleaseTarget{}, domain.AccessTarget{}, false
	}
	environment, err := s.store.GetEnvironment(r.Context(), environmentID)
	if err != nil || application.ProjectID == "" || application.ProjectID != environment.ProjectID {
		mappedError(w, r, store.ErrNotFound)
		return helmapps.ReleaseTarget{}, domain.AccessTarget{}, false
	}
	project, err := s.store.GetProject(r.Context(), application.ProjectID)
	if err != nil {
		mappedError(w, r, err)
		return helmapps.ReleaseTarget{}, domain.AccessTarget{}, false
	}
	accessTarget := domain.AccessTarget{Type: "application", ID: application.ID, TeamID: project.TeamID,
		ProjectID: project.ID, EnvironmentID: environment.ID, Namespace: environment.Namespace, ApplicationID: application.ID}
	if err = s.store.Authorize(r.Context(), currentUser(r.Context()).ID, permission, accessTarget); err != nil {
		mappedError(w, r, err)
		return helmapps.ReleaseTarget{}, domain.AccessTarget{}, false
	}
	return helmapps.ReleaseTarget{ProjectID: project.ID, EnvironmentID: environment.ID, ApplicationID: application.ID}, accessTarget, true
}

func (s *Server) helmConfigured(w http.ResponseWriter, r *http.Request) bool {
	if s.helmApplications == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "HelmApplicationsUnavailable", "Helm applications unavailable", "Approved external Helm applications are not configured.")
		return false
	}
	return true
}

func (s *Server) helmMutationReady(w http.ResponseWriter, r *http.Request, rollback bool) bool {
	if !s.helmConfigured(w, r) {
		return false
	}
	capabilities, err := s.helmApplications.Capabilities(r.Context())
	if err != nil || !capabilities.HelmDeployments || rollback && !capabilities.HelmRollbacks {
		writeProblem(w, r, http.StatusServiceUnavailable, "HelmRuntimeNotReady", "Helm runtime not ready", "The exact renderer, protected publisher, and Argo readiness fences are not all fresh.")
		return false
	}
	return true
}

func helmActor(r *http.Request, key string) helmapps.ReleaseActor {
	return helmapps.ReleaseActor{ID: currentUser(r.Context()).ID, IdempotencyKey: key, RequestID: requestID(r.Context())}
}

func (s *Server) writeHelmMutation(w http.ResponseWriter, r *http.Request, value helmapps.ReleaseRevision, replay bool, err error) {
	if err != nil {
		writeHelmError(w, r, err)
		return
	}
	if replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Location", "/v1/applications/"+value.Target.ApplicationID+"/environments/"+value.Target.EnvironmentID+"/helm/release")
	writeJSON(w, http.StatusAccepted, helmReleaseView(value))
}

func parseHelmLimit(w http.ResponseWriter, r *http.Request, fallback int) (int, bool) {
	query := r.URL.Query()
	for key, values := range query {
		if key != "limit" || len(values) != 1 || values[0] == "" {
			writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "Use only one bounded limit query parameter.")
			return 0, false
		}
	}
	if query.Get("limit") == "" {
		return fallback, true
	}
	value, err := strconv.Atoi(query.Get("limit"))
	if err != nil || value < 1 || value > 100 || strconv.Itoa(value) != query.Get("limit") {
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "limit must be a canonical integer from 1 through 100.")
		return 0, false
	}
	return value, true
}

func helmIdempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	values := r.Header.Values("Idempotency-Key")
	if len(values) != 1 || !setupHTTPIdempotencyRE.MatchString(values[0]) {
		writeProblem(w, r, http.StatusBadRequest, "IdempotencyKeyRequired", "Idempotency key required", "Provide one Idempotency-Key header containing 16 to 128 safe ASCII characters.")
		return "", false
	}
	return values[0], true
}

func decodeHelmEmptyRequest(w http.ResponseWriter, r *http.Request) bool {
	var input struct{}
	return decodeHelmRequest(w, r, &input)
}

func decodeHelmRequest(w http.ResponseWriter, r *http.Request, output any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeProblem(w, r, http.StatusUnsupportedMediaType, "UnsupportedMediaType", "Unsupported media type", "Helm mutations require application/json.")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaximumHelmHTTPRequestBytes)
	raw, err := io.ReadAll(r.Body)
	defer clear(raw)
	if err != nil || len(raw) < 2 || !utf8.Valid(raw) || !uniqueJSONObject(raw) {
		writeProblem(w, r, http.StatusBadRequest, "InvalidJSON", "Invalid JSON", "The request body must be one bounded UTF-8 JSON object without duplicate fields.")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(output); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "InvalidJSON", "Invalid JSON", "The request body contains an unknown or invalid field.")
		return false
	}
	var trailing any
	if err = decoder.Decode(&trailing); err != io.EOF {
		writeProblem(w, r, http.StatusBadRequest, "InvalidJSON", "Invalid JSON", "The request body must contain exactly one object.")
		return false
	}
	return true
}

const MaximumHelmHTTPRequestBytes = helmapps.MaximumValuesSize + 16<<10

func writeHelmError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, helmapps.ErrNotFound):
		writeProblem(w, r, http.StatusNotFound, "HelmReleaseNotFound", "Helm release not found", "The requested approved Helm release was not found.")
	case errors.Is(err, helmapps.ErrConflict), errors.Is(err, helmapps.ErrLeaseLost):
		writeProblem(w, r, http.StatusConflict, "HelmReleaseConflict", "Helm release conflict", "The desired release conflicts with the current immutable history or publication state.")
	case errors.Is(err, helmapps.ErrInvalid), errors.Is(err, helmapps.ErrUnsafeYAML), errors.Is(err, helmapps.ErrUnsafeChart):
		writeProblem(w, r, http.StatusUnprocessableEntity, "HelmValidationFailed", "Helm validation failed", "The approval, values.yaml, or requested transition violates the approved Helm contract.")
	case errors.Is(err, helmapps.ErrUnavailable):
		writeProblem(w, r, http.StatusServiceUnavailable, "HelmRuntimeNotReady", "Helm runtime not ready", "The approved Helm runtime is unavailable.")
	case errors.Is(err, helmapps.ErrOCIUnauthorized), errors.Is(err, helmapps.ErrOCIUnavailable):
		writeProblem(w, r, http.StatusServiceUnavailable, "HelmRegistryUnavailable", "Helm registry unavailable", "The approved OCI registry or its operator-owned credentials are unavailable.")
	default:
		writeProblem(w, r, http.StatusInternalServerError, "HelmPersistenceFailed", "Helm request failed", "The approved Helm request could not be completed.")
	}
}
