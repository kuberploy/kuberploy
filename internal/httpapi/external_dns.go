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
	"github.com/kuberploy/kuberploy/internal/externaldns"
	"github.com/kuberploy/kuberploy/internal/store"
)

const (
	defaultExternalDNSLimit = 50
	maximumExternalDNSLimit = 100
	maxExternalDNSBodyBytes = 64 << 10
)

type ExternalDNSManagementService interface {
	Integrations(context.Context, string) ([]domain.ExternalDNSIntegration, error)
	Create(context.Context, string, string, string, string, externaldns.IntegrationInput) (store.Result[domain.ExternalDNSIntegration], error)
	Update(context.Context, string, string, string, string, string, externaldns.IntegrationInput) (store.Result[domain.ExternalDNSIntegration], error)
	Deactivate(context.Context, string, string, string, string, string) (store.Result[domain.ExternalDNSIntegration], error)
	EnvironmentCatalog(context.Context, string, string) ([]domain.ExternalDNSCatalogItem, error)
	ApplicationCatalog(context.Context, string, string, string) ([]domain.ExternalDNSCatalogItem, error)
	ValidateApplicationRoute(context.Context, string, string, string, string, string) error
}

type externalDNSHTTP struct {
	service         ExternalDNSManagementService
	readiness       ReadinessProbe
	runtimeObserved bool
}

func newExternalDNSHTTP(service ExternalDNSManagementService, readiness ReadinessProbe, runtimeObserved bool) *externalDNSHTTP {
	return &externalDNSHTTP{service: service, readiness: readiness, runtimeObserved: runtimeObserved}
}

type externalDNSIntegrationRequest struct {
	Slug                     string   `json:"slug"`
	Name                     string   `json:"name"`
	Mode                     string   `json:"mode"`
	ProviderKind             string   `json:"providerKind"`
	TXTOwnerID               string   `json:"txtOwnerId"`
	AllowedDomainSuffixes    []string `json:"allowedDomainSuffixes"`
	SyncPolicy               string   `json:"syncPolicy,omitempty"`
	DestructiveSyncConfirmed bool     `json:"destructiveSyncConfirmed,omitempty"`
	CredentialSecretRef      string   `json:"credentialSecretRef,omitempty"`
	ProviderConfigRef        string   `json:"providerConfigRef,omitempty"`
	EgressConfigRef          string   `json:"egressConfigRef,omitempty"`
	OperatorProfileRef       string   `json:"operatorProfileRef,omitempty"`
	EnvironmentIDs           []string `json:"environmentIds"`
}

func (request externalDNSIntegrationRequest) serviceInput() externaldns.IntegrationInput {
	return externaldns.IntegrationInput{
		Slug: request.Slug, Name: request.Name, Mode: request.Mode, ProviderKind: request.ProviderKind,
		TXTOwnerID: request.TXTOwnerID, AllowedDomainSuffixes: request.AllowedDomainSuffixes,
		SyncPolicy: request.SyncPolicy, DestructiveSyncConfirmed: request.DestructiveSyncConfirmed,
		CredentialSecretRef: request.CredentialSecretRef, ProviderConfigRef: request.ProviderConfigRef,
		EgressConfigRef: request.EgressConfigRef, OperatorProfileRef: request.OperatorProfileRef,
		EnvironmentIDs: request.EnvironmentIDs,
	}
}

type externalDNSIntegrationView struct {
	ID                       string     `json:"id"`
	Slug                     string     `json:"slug"`
	Name                     string     `json:"name"`
	Mode                     string     `json:"mode"`
	ProviderKind             string     `json:"providerKind"`
	TXTOwnerID               string     `json:"txtOwnerId"`
	AllowedDomainSuffixes    []string   `json:"allowedDomainSuffixes"`
	SyncPolicy               string     `json:"syncPolicy"`
	DestructiveSyncConfirmed bool       `json:"destructiveSyncConfirmed"`
	CredentialSecretRef      string     `json:"credentialSecretRef,omitempty"`
	ProviderConfigRef        string     `json:"providerConfigRef,omitempty"`
	EgressConfigRef          string     `json:"egressConfigRef,omitempty"`
	OperatorProfileRef       string     `json:"operatorProfileRef,omitempty"`
	EnvironmentIDs           []string   `json:"environmentIds"`
	CreatedBy                string     `json:"createdBy"`
	CreatedAt                time.Time  `json:"createdAt"`
	UpdatedAt                time.Time  `json:"updatedAt"`
	RuntimeRevision          int64      `json:"runtimeRevision"`
	Lifecycle                string     `json:"lifecycle"`
	DeactivatedAt            *time.Time `json:"deactivatedAt,omitempty"`
	ProtectedGitState        string     `json:"protectedGitState"`
	ProtectedGitRevision     int64      `json:"protectedGitRevision,omitempty"`
	ProtectedGitObservedAt   *time.Time `json:"protectedGitObservedAt,omitempty"`
}

func safeExternalDNSIntegration(item domain.ExternalDNSIntegration) externalDNSIntegrationView {
	return externalDNSIntegrationView{
		ID: item.ID, Slug: item.Slug, Name: item.Name, Mode: item.Mode, ProviderKind: item.ProviderKind,
		TXTOwnerID: item.TXTOwnerID, AllowedDomainSuffixes: append([]string(nil), item.AllowedDomainSuffixes...),
		SyncPolicy: item.SyncPolicy, DestructiveSyncConfirmed: item.DestructiveSyncConfirmed,
		CredentialSecretRef: item.CredentialSecretRef, ProviderConfigRef: item.ProviderConfigRef,
		EgressConfigRef: item.EgressConfigRef, OperatorProfileRef: item.OperatorProfileRef,
		EnvironmentIDs: append([]string(nil), item.EnvironmentIDs...), CreatedBy: item.CreatedBy,
		CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
		RuntimeRevision: item.RuntimeRevision, Lifecycle: item.Lifecycle, DeactivatedAt: item.DeactivatedAt,
		ProtectedGitState: item.ProtectedGitState, ProtectedGitRevision: item.ProtectedGitRevision, ProtectedGitObservedAt: item.ProtectedGitObservedAt,
	}
}

func (h *externalDNSHTTP) deactivateIntegration(w http.ResponseWriter, r *http.Request) {
	externalDNSResponseHeaders(w)
	if h == nil || h.service == nil {
		externalDNSBackendUnavailable(w, r)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if !validUUID(id) {
		writeProblem(w, r, http.StatusNotFound, "ExternalDNSIntegrationNotFound", "External DNS integration not found", "The requested External DNS integration does not exist.")
		return
	}
	key, ok := secretIdempotencyKey(w, r)
	if !ok {
		return
	}
	result, err := h.service.Deactivate(r.Context(), currentUser(r.Context()).ID, key, fingerprint(struct {
		ID string `json:"id"`
	}{id}), safeSecretRequestID(r.Context()), id)
	if err != nil {
		mappedExternalDNSError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeJSON(w, http.StatusOK, safeExternalDNSIntegration(result.Value))
}

type externalDNSCatalogResponse struct {
	Items               []externalDNSCatalogItemView `json:"items"`
	Truncated           bool                         `json:"truncated"`
	ConfigurationState  string                       `json:"configurationState"`
	ControllerReadiness string                       `json:"controllerReadiness"`
	RuntimeAvailable    bool                         `json:"runtimeAvailable"`
}

type externalDNSCatalogItemView struct {
	ID                    string   `json:"id"`
	Slug                  string   `json:"slug"`
	Name                  string   `json:"name"`
	Mode                  string   `json:"mode"`
	ProviderKind          string   `json:"providerKind"`
	AllowedDomainSuffixes []string `json:"allowedDomainSuffixes"`
	RuntimeRevision       int64    `json:"runtimeRevision"`
	RuntimeAvailable      bool     `json:"runtimeAvailable"`
}

func (h *externalDNSHTTP) integrations(w http.ResponseWriter, r *http.Request) {
	externalDNSResponseHeaders(w)
	if h == nil || h.service == nil {
		externalDNSBackendUnavailable(w, r)
		return
	}
	actor := currentUser(r.Context()).ID
	if r.Method == http.MethodGet {
		limit, ok := externalDNSLimit(w, r)
		if !ok {
			return
		}
		items, err := h.service.Integrations(r.Context(), actor)
		if err != nil {
			mappedExternalDNSError(w, r, err)
			return
		}
		truncated := len(items) > limit
		if truncated {
			items = items[:limit]
		}
		views := make([]externalDNSIntegrationView, 0, len(items))
		for _, item := range items {
			views = append(views, safeExternalDNSIntegration(item))
		}
		writeJSON(w, http.StatusOK, struct {
			Items     []externalDNSIntegrationView `json:"items"`
			Truncated bool                         `json:"truncated"`
		}{Items: views, Truncated: truncated})
		return
	}
	key, ok := secretIdempotencyKey(w, r)
	if !ok {
		return
	}
	var request externalDNSIntegrationRequest
	if !decodeExternalDNSRequest(w, r, &request) {
		return
	}
	result, err := h.service.Create(r.Context(), actor, key, fingerprint(request), safeSecretRequestID(r.Context()), request.serviceInput())
	if err != nil {
		mappedExternalDNSError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Location", "/v1/external-dns/integrations/"+result.Value.ID)
	writeJSON(w, http.StatusCreated, safeExternalDNSIntegration(result.Value))
}

func (h *externalDNSHTTP) updateIntegration(w http.ResponseWriter, r *http.Request) {
	externalDNSResponseHeaders(w)
	if h == nil || h.service == nil {
		externalDNSBackendUnavailable(w, r)
		return
	}
	integrationID := strings.TrimSpace(r.PathValue("id"))
	if !validUUID(integrationID) {
		writeProblem(w, r, http.StatusNotFound, "ExternalDNSIntegrationNotFound", "External DNS integration not found", "The requested External DNS integration does not exist.")
		return
	}
	key, ok := secretIdempotencyKey(w, r)
	if !ok {
		return
	}
	var request externalDNSIntegrationRequest
	if !decodeExternalDNSRequest(w, r, &request) {
		return
	}
	result, err := h.service.Update(r.Context(), currentUser(r.Context()).ID, key, fingerprint(request), safeSecretRequestID(r.Context()), integrationID, request.serviceInput())
	if err != nil {
		mappedExternalDNSError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeJSON(w, http.StatusOK, safeExternalDNSIntegration(result.Value))
}

func (h *externalDNSHTTP) status(w http.ResponseWriter, r *http.Request) {
	externalDNSResponseHeaders(w)
	if h == nil || h.service == nil {
		writeJSON(w, http.StatusOK, externalDNSStatus(false, false))
		return
	}
	items, err := h.service.Integrations(r.Context(), currentUser(r.Context()).ID)
	if err != nil {
		mappedExternalDNSError(w, r, err)
		return
	}
	configured := false
	for _, item := range items {
		if item.Lifecycle == "" || item.Lifecycle == "active" {
			configured = true
			break
		}
	}
	writeJSON(w, http.StatusOK, externalDNSStatus(configured, h.runtimeReady(r.Context())))
}

func (h *externalDNSHTTP) runtimeReady(ctx context.Context) bool {
	if h == nil || !h.runtimeObserved || h.readiness == nil {
		return false
	}
	probeContext, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return h.readiness.Probe(probeContext) == nil
}

func externalDNSStatus(configured, ready bool) map[string]any {
	state := "empty"
	if configured {
		state = "configured"
	}
	readiness, detail := externaldns.ReadinessUnobserved, "One or more current External DNS integration revisions have not completed protected Git publication and exact live controller observation."
	if ready {
		readiness = externaldns.ReadinessReady
		detail = "Every current External DNS integration revision is protected-Git materialized and freshly observed with its exact provider, credential, provider-config, egress, policy, and domain identities."
	}
	return map[string]any{
		"configurationState": state, "controllerReadiness": readiness,
		"runtimeAvailable": ready,
		"detail":           detail,
	}
}

func (h *externalDNSHTTP) environmentCatalog(w http.ResponseWriter, r *http.Request) {
	environmentID := strings.TrimSpace(r.PathValue("id"))
	if !validUUID(environmentID) {
		writeProblem(w, r, http.StatusNotFound, "EnvironmentNotFound", "Environment not found", "The requested environment does not exist.")
		return
	}
	h.catalog(w, r, "", environmentID)
}

func (h *externalDNSHTTP) applicationCatalog(w http.ResponseWriter, r *http.Request) {
	applicationID := strings.TrimSpace(r.PathValue("id"))
	if !validUUID(applicationID) {
		writeProblem(w, r, http.StatusNotFound, "ApplicationNotFound", "Application not found", "The requested application does not exist.")
		return
	}
	environmentID := strings.TrimSpace(r.URL.Query().Get("environmentId"))
	if !validUUID(environmentID) || len(r.URL.Query()["environmentId"]) != 1 {
		writeProblem(w, r, http.StatusBadRequest, "EnvironmentRequired", "Environment required", "Provide exactly one canonical UUID environmentId query parameter.")
		return
	}
	h.catalog(w, r, applicationID, environmentID)
}

func (h *externalDNSHTTP) catalog(w http.ResponseWriter, r *http.Request, applicationID, environmentID string) {
	externalDNSResponseHeaders(w)
	if h == nil || h.service == nil {
		externalDNSBackendUnavailable(w, r)
		return
	}
	limit, ok := externalDNSLimit(w, r)
	if !ok {
		return
	}
	actor := currentUser(r.Context()).ID
	var items []domain.ExternalDNSCatalogItem
	var err error
	if applicationID == "" {
		items, err = h.service.EnvironmentCatalog(r.Context(), actor, environmentID)
	} else {
		items, err = h.service.ApplicationCatalog(r.Context(), actor, applicationID, environmentID)
	}
	if err != nil {
		mappedExternalDNSError(w, r, err)
		return
	}
	truncated := len(items) > limit
	if truncated {
		items = items[:limit]
	}
	ready := h.runtimeReady(r.Context())
	views := make([]externalDNSCatalogItemView, 0, len(items))
	for _, item := range items {
		views = append(views, externalDNSCatalogItemView{
			ID: item.ID, Slug: item.Slug, Name: item.Name, Mode: item.Mode, ProviderKind: item.ProviderKind,
			AllowedDomainSuffixes: append([]string(nil), item.AllowedDomainSuffixes...), RuntimeRevision: item.RuntimeRevision,
			RuntimeAvailable: ready,
		})
	}
	writeJSON(w, http.StatusOK, externalDNSCatalogResponse{
		Items: views, Truncated: truncated, ConfigurationState: configurationState(len(items) > 0),
		ControllerReadiness: externalDNSReadiness(ready), RuntimeAvailable: ready,
	})
}

func externalDNSReadiness(ready bool) string {
	if ready {
		return externaldns.ReadinessReady
	}
	return externaldns.ReadinessUnobserved
}

func configurationState(configured bool) string {
	if configured {
		return "configured"
	}
	return "empty"
}

func externalDNSLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	rawValues, present := r.URL.Query()["limit"]
	if !present {
		return defaultExternalDNSLimit, true
	}
	if len(rawValues) != 1 {
		writeProblem(w, r, http.StatusBadRequest, "InvalidExternalDNSLimit", "Invalid external-dns limit", "limit must be exactly one integer between 1 and 100.")
		return 0, false
	}
	limit, err := strconv.Atoi(rawValues[0])
	if err != nil || limit < 1 || limit > maximumExternalDNSLimit {
		writeProblem(w, r, http.StatusBadRequest, "InvalidExternalDNSLimit", "Invalid external-dns limit", "limit must be exactly one integer between 1 and 100.")
		return 0, false
	}
	return limit, true
}

func externalDNSResponseHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Pragma", "no-cache")
}

func externalDNSNoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		externalDNSResponseHeaders(w)
		next.ServeHTTP(w, r)
	})
}

func decodeExternalDNSRequest(w http.ResponseWriter, r *http.Request, output any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeProblem(w, r, http.StatusUnsupportedMediaType, "UnsupportedMediaType", "Unsupported media type", "External-dns mutations require application/json.")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxExternalDNSBodyBytes)
	raw, err := io.ReadAll(r.Body)
	defer clear(raw)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeProblem(w, r, http.StatusRequestEntityTooLarge, "PayloadTooLarge", "Payload too large", "The external-dns request exceeds the encoded request limit.")
		} else {
			writeProblem(w, r, http.StatusBadRequest, "InvalidJSON", "Invalid JSON", "The request body is not valid JSON.")
		}
		return false
	}
	if len(raw) == 0 || !utf8.Valid(raw) || !uniqueTopLevelSecretFields(raw) {
		writeProblem(w, r, http.StatusBadRequest, "InvalidJSON", "Invalid JSON", "The request must be one valid UTF-8 JSON object without duplicate fields.")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(output); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "InvalidJSON", "Invalid JSON", "The request body is not valid for this operation.")
		return false
	}
	if err = decoder.Decode(&struct{}{}); err != io.EOF {
		writeProblem(w, r, http.StatusBadRequest, "InvalidJSON", "Invalid JSON", "The request must contain exactly one JSON object.")
		return false
	}
	return true
}

func externalDNSBackendUnavailable(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, http.StatusServiceUnavailable, "ExternalDNSConfigurationUnavailable", "External DNS configuration unavailable", "External DNS integration configuration is not enabled for this installation.")
}

func mappedExternalDNSError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, externaldns.ErrInvalid):
		writeProblem(w, r, http.StatusUnprocessableEntity, "ExternalDNSValidationFailed", "External DNS integration is invalid", "Provide a valid operational integration, exact environment assignments, safe references, and explicit destructive-sync confirmation.")
	case errors.Is(err, externaldns.ErrIntegrationReference):
		writeProblem(w, r, http.StatusUnprocessableEntity, "ExternalDNSIntegrationUnavailable", "External DNS integration unavailable", "Select an integration authorized for this application and environment.")
	case errors.Is(err, externaldns.ErrHostnameNotAllowed):
		writeProblem(w, r, http.StatusUnprocessableEntity, "ExternalDNSHostnameNotAllowed", "Hostname is not allowed", "The route hostname must be within an allowed domain suffix for the selected integration.")
	default:
		mappedError(w, r, err)
	}
}
