package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/registrycredentials"
	"github.com/kuberploy/kuberploy/internal/secrets"
	"github.com/kuberploy/kuberploy/internal/store"
)

const maxRegistryCredentialRequestBytes = 8 << 10

// RegistryCredentialBindingLister is deliberately metadata-only. The
// registry-credential service owns all host/username/password ingestion and
// validation; this seam only supplies the already-scoped catalog needed by
// the management collection route.
type RegistryCredentialBindingLister interface {
	ListBindings(context.Context, string, string) ([]secrets.Binding, error)
}

// RegistryCredentialManagementBackend is separate from RuntimeSecretBackend
// so the generic secret API can never list, rotate, or delete a registry
// password.
type RegistryCredentialManagementBackend interface {
	Create(context.Context, registrycredentials.CreateRequest) (registrycredentials.MutationResult, error)
	Rotate(context.Context, registrycredentials.RotateRequest) (registrycredentials.MutationResult, error)
	Delete(context.Context, string, string, string) (secrets.Binding, error)
	DeleteWithIdempotency(context.Context, string, string, string, string) (secrets.Binding, error)
	Binding(context.Context, string) (secrets.Binding, []registrycredentials.Version, error)
	ListBindings(context.Context, string, string) ([]secrets.Binding, error)
}

type registryCredentialManagementBackend struct {
	service registrycredentials.Service
	lister  RegistryCredentialBindingLister
}

// NewRegistryCredentialManagementBackend adapts the registry-credential
// service without widening its write-only boundary.
func NewRegistryCredentialManagementBackend(service registrycredentials.Service, lister RegistryCredentialBindingLister) (RegistryCredentialManagementBackend, error) {
	if service.Secrets == nil || service.Catalog == nil || service.Store == nil || lister == nil {
		return nil, registrycredentials.ErrInvalid
	}
	return &registryCredentialManagementBackend{service: service, lister: lister}, nil
}

func (b *registryCredentialManagementBackend) Create(ctx context.Context, request registrycredentials.CreateRequest) (registrycredentials.MutationResult, error) {
	return b.service.Create(ctx, request)
}

func (b *registryCredentialManagementBackend) Rotate(ctx context.Context, request registrycredentials.RotateRequest) (registrycredentials.MutationResult, error) {
	return b.service.Rotate(ctx, request)
}

func (b *registryCredentialManagementBackend) Delete(ctx context.Context, actorID, bindingID, requestID string) (secrets.Binding, error) {
	return b.service.Delete(ctx, actorID, bindingID, requestID)
}
func (b *registryCredentialManagementBackend) DeleteWithIdempotency(ctx context.Context, actorID, bindingID, idempotencyKey, requestID string) (secrets.Binding, error) {
	return b.service.DeleteWithIdempotency(ctx, actorID, bindingID, idempotencyKey, requestID)
}

func (b *registryCredentialManagementBackend) Binding(ctx context.Context, bindingID string) (secrets.Binding, []registrycredentials.Version, error) {
	return b.service.Binding(ctx, bindingID)
}

func (b *registryCredentialManagementBackend) ListBindings(ctx context.Context, applicationID, environmentID string) ([]secrets.Binding, error) {
	return b.lister.ListBindings(ctx, applicationID, environmentID)
}

type registryCredentialTextValue []byte

func decodeBoundedRegistryCredentialText(data []byte, limit int, allowEmpty bool) ([]byte, error) {
	var value string
	if len(data) == 0 || json.Unmarshal(data, &value) != nil || !utf8.ValidString(value) {
		return nil, registrycredentials.ErrInvalid
	}
	decoded := []byte(value)
	value = ""
	if (!allowEmpty && len(decoded) == 0) || len(decoded) > limit {
		clear(decoded)
		return nil, registrycredentials.ErrInvalid
	}
	return decoded, nil
}

func (v *registryCredentialTextValue) UnmarshalJSON(data []byte) error {
	clear(*v)
	decoded, err := decodeBoundedRegistryCredentialText(data, registrycredentials.MaxPasswordBytes, true)
	if err != nil {
		return err
	}
	*v = decoded
	return nil
}

type registryCredentialCreateRequest struct {
	EnvironmentID string                      `json:"environmentId"`
	Name          string                      `json:"name"`
	Host          registryCredentialTextValue `json:"host"`
	Username      registryCredentialTextValue `json:"username"`
	Password      registryCredentialTextValue `json:"password"`
	Email         registryCredentialTextValue `json:"email"`
}

func (r *registryCredentialCreateRequest) Destroy() {
	clear(r.Host)
	clear(r.Username)
	clear(r.Password)
	clear(r.Email)
	r.Host, r.Username, r.Password, r.Email = nil, nil, nil, nil
}

type registryCredentialRotateRequest struct {
	ExpectedActiveVersion int64                       `json:"expectedActiveVersion"`
	Host                  registryCredentialTextValue `json:"host"`
	Username              registryCredentialTextValue `json:"username"`
	Password              registryCredentialTextValue `json:"password"`
	Email                 registryCredentialTextValue `json:"email"`
}

func (r *registryCredentialRotateRequest) Destroy() {
	clear(r.Host)
	clear(r.Username)
	clear(r.Password)
	clear(r.Email)
	r.Host, r.Username, r.Password, r.Email = nil, nil, nil, nil
}

type registryCredentialBindingMetadataView struct {
	ID              string               `json:"id"`
	ApplicationID   string               `json:"applicationId"`
	EnvironmentID   string               `json:"environmentId"`
	Name            string               `json:"name"`
	State           secrets.BindingState `json:"state"`
	ActiveVersion   int64                `json:"activeVersion,omitempty"`
	CreatedBy       string               `json:"createdBy"`
	CreatedAt       time.Time            `json:"createdAt"`
	UpdatedAt       time.Time            `json:"updatedAt"`
	DeleteStartedAt *time.Time           `json:"deleteStartedAt,omitempty"`
	DeletedAt       *time.Time           `json:"deletedAt,omitempty"`
}

type registryCredentialVersionMetadataView struct {
	Number    int64     `json:"number"`
	Host      string    `json:"host"`
	Username  string    `json:"username"`
	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
}

type registryCredentialBindingDetailView struct {
	registryCredentialBindingMetadataView
	Versions []registryCredentialVersionMetadataView `json:"versions"`
}

func safeRegistryCredentialBinding(binding secrets.Binding) registryCredentialBindingMetadataView {
	return registryCredentialBindingMetadataView{
		ID: binding.ID, ApplicationID: binding.Scope.ApplicationID, EnvironmentID: binding.Scope.EnvironmentID,
		Name: binding.Name, State: binding.State, ActiveVersion: binding.ActiveVersion, CreatedBy: binding.CreatedBy,
		CreatedAt: binding.CreatedAt, UpdatedAt: binding.UpdatedAt, DeleteStartedAt: nonzeroTime(binding.DeleteStarted),
		DeletedAt: nonzeroTime(binding.DeletedAt),
	}
}

func safeRegistryCredentialVersion(version registrycredentials.Version) registryCredentialVersionMetadataView {
	return registryCredentialVersionMetadataView{
		Number: version.Number, Host: version.Host, Username: version.Username,
		CreatedBy: version.CreatedBy, CreatedAt: version.CreatedAt,
	}
}

func (s *Server) registryCredentialsReady(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.registryCredentials == nil {
			registryCredentialBackendUnavailable(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) createRegistryCredentialBinding(w http.ResponseWriter, r *http.Request) {
	if !registryCredentialQueryEmpty(w, r) {
		return
	}
	key, ok := secretIdempotencyKey(w, r)
	if !ok {
		return
	}
	var input registryCredentialCreateRequest
	defer input.Destroy()
	if !decodeRegistryCredentialRequest(w, r, &input) {
		return
	}
	applicationID := strings.TrimSpace(r.PathValue("id"))
	scope, target, err := s.resolveSecretScope(r.Context(), applicationID, strings.TrimSpace(input.EnvironmentID), applicationID)
	if err != nil {
		mappedRegistryCredentialError(w, r, err)
		return
	}
	if err = s.store.Authorize(r.Context(), currentUser(r.Context()).ID, domain.PermissionRegistryCredentialsCreate, target); err != nil {
		mappedError(w, r, err)
		return
	}
	material, err := registrycredentials.NewMaterial(input.Host, input.Username, input.Password, input.Email)
	input.Destroy()
	if err != nil {
		mappedRegistryCredentialError(w, r, err)
		return
	}
	defer material.Destroy()
	result, err := s.registryCredentials.Create(r.Context(), registrycredentials.CreateRequest{
		ActorID: currentUser(r.Context()).ID, Scope: scope, Name: strings.TrimSpace(input.Name), IdempotencyKey: key,
		RequestID: safeSecretRequestID(r.Context()), Material: material,
	})
	if err != nil {
		mappedRegistryCredentialError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Location", "/v1/registry-credential-bindings/"+result.Binding.ID)
	writeJSON(w, http.StatusCreated, registryCredentialBindingDetailView{
		registryCredentialBindingMetadataView: safeRegistryCredentialBinding(result.Binding),
		Versions:                              []registryCredentialVersionMetadataView{safeRegistryCredentialVersion(result.Credential)},
	})
}

func (s *Server) listRegistryCredentialBindings(w http.ResponseWriter, r *http.Request) {
	applicationID := strings.TrimSpace(r.PathValue("id"))
	environmentID, ok := registryCredentialEnvironmentQuery(w, r)
	if !ok {
		return
	}
	if environmentID == "" {
		application, err := s.store.GetApplication(r.Context(), applicationID)
		if err != nil {
			mappedError(w, r, err)
			return
		}
		if err = s.store.Authorize(r.Context(), currentUser(r.Context()).ID, domain.PermissionRegistryCredentialsRead, domain.AccessTarget{Type: "application", ID: application.ID}); err != nil {
			mappedError(w, r, err)
			return
		}
	} else {
		_, target, err := s.resolveSecretScope(r.Context(), applicationID, environmentID, applicationID)
		if err != nil {
			mappedRegistryCredentialError(w, r, err)
			return
		}
		if err = s.store.Authorize(r.Context(), currentUser(r.Context()).ID, domain.PermissionRegistryCredentialsRead, target); err != nil {
			mappedError(w, r, err)
			return
		}
	}
	bindings, err := s.registryCredentials.ListBindings(r.Context(), applicationID, environmentID)
	if err != nil {
		mappedRegistryCredentialError(w, r, err)
		return
	}
	items := make([]registryCredentialBindingMetadataView, 0, len(bindings))
	for _, binding := range bindings {
		if binding.Purpose != secrets.PurposeRegistryPullCredential {
			continue
		}
		resolved, target, resolveErr := s.resolveSecretScope(r.Context(), binding.Scope.ApplicationID, binding.Scope.EnvironmentID, binding.ID)
		if resolveErr != nil || resolved != binding.Scope || binding.Scope.ApplicationID != applicationID || environmentID != "" && binding.Scope.EnvironmentID != environmentID {
			mappedError(w, r, store.ErrNotFound)
			return
		}
		if err = s.store.Authorize(r.Context(), currentUser(r.Context()).ID, domain.PermissionRegistryCredentialsRead, target); err != nil {
			mappedError(w, r, err)
			return
		}
		items = append(items, safeRegistryCredentialBinding(binding))
	}
	collection(w, items)
}

func (s *Server) getRegistryCredentialBinding(w http.ResponseWriter, r *http.Request) {
	if !registryCredentialQueryEmpty(w, r) {
		return
	}
	binding, versions, ok := s.authorizedRegistryCredentialBinding(w, r, domain.PermissionRegistryCredentialsRead)
	if !ok {
		return
	}
	views := make([]registryCredentialVersionMetadataView, 0, len(versions))
	for _, version := range versions {
		views = append(views, safeRegistryCredentialVersion(version))
	}
	writeJSON(w, http.StatusOK, registryCredentialBindingDetailView{registryCredentialBindingMetadataView: safeRegistryCredentialBinding(binding), Versions: views})
}

func (s *Server) rotateRegistryCredentialBinding(w http.ResponseWriter, r *http.Request) {
	if !registryCredentialQueryEmpty(w, r) {
		return
	}
	key, ok := secretIdempotencyKey(w, r)
	if !ok {
		return
	}
	binding, _, ok := s.authorizedRegistryCredentialBinding(w, r, domain.PermissionRegistryCredentialsRotate)
	if !ok {
		return
	}
	var input registryCredentialRotateRequest
	defer input.Destroy()
	if !decodeRegistryCredentialRequest(w, r, &input) {
		return
	}
	material, err := registrycredentials.NewMaterial(input.Host, input.Username, input.Password, input.Email)
	input.Destroy()
	if err != nil {
		mappedRegistryCredentialError(w, r, err)
		return
	}
	defer material.Destroy()
	result, err := s.registryCredentials.Rotate(r.Context(), registrycredentials.RotateRequest{
		ActorID: currentUser(r.Context()).ID, BindingID: binding.ID, ExpectedActiveVersion: input.ExpectedActiveVersion,
		IdempotencyKey: key, RequestID: safeSecretRequestID(r.Context()), Material: material,
	})
	if err != nil {
		mappedRegistryCredentialError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeJSON(w, http.StatusCreated, registryCredentialBindingDetailView{
		registryCredentialBindingMetadataView: safeRegistryCredentialBinding(result.Binding),
		Versions:                              []registryCredentialVersionMetadataView{safeRegistryCredentialVersion(result.Credential)},
	})
}

func (s *Server) deleteRegistryCredentialBinding(w http.ResponseWriter, r *http.Request) {
	if !registryCredentialQueryEmpty(w, r) || !registryCredentialBodyEmpty(w, r) {
		return
	}
	key, ok := secretIdempotencyKey(w, r)
	if !ok {
		return
	}
	binding, _, ok := s.authorizedRegistryCredentialBinding(w, r, domain.PermissionRegistryCredentialsDelete)
	if !ok {
		return
	}
	wasDeleted := binding.State == secrets.BindingDeleted
	if _, err := s.registryCredentials.DeleteWithIdempotency(r.Context(), currentUser(r.Context()).ID, binding.ID, key, safeSecretRequestID(r.Context())); err != nil {
		mappedRegistryCredentialError(w, r, err)
		return
	}
	if wasDeleted {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) authorizedRegistryCredentialBinding(w http.ResponseWriter, r *http.Request, permission domain.Permission) (secrets.Binding, []registrycredentials.Version, bool) {
	binding, versions, err := s.registryCredentials.Binding(r.Context(), strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		mappedRegistryCredentialError(w, r, err)
		return secrets.Binding{}, nil, false
	}
	if binding.Purpose != secrets.PurposeRegistryPullCredential {
		mappedError(w, r, store.ErrNotFound)
		return secrets.Binding{}, nil, false
	}
	resolved, target, err := s.resolveSecretScope(r.Context(), binding.Scope.ApplicationID, binding.Scope.EnvironmentID, binding.ID)
	if err != nil || resolved != binding.Scope {
		mappedError(w, r, store.ErrNotFound)
		return secrets.Binding{}, nil, false
	}
	if err = s.store.Authorize(r.Context(), currentUser(r.Context()).ID, permission, target); err != nil {
		mappedError(w, r, err)
		return secrets.Binding{}, nil, false
	}
	return binding, versions, true
}

func decodeRegistryCredentialRequest(w http.ResponseWriter, r *http.Request, output any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeProblem(w, r, http.StatusUnsupportedMediaType, "UnsupportedMediaType", "Unsupported media type", "Registry credential mutations require application/json.")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRegistryCredentialRequestBytes)
	raw, err := io.ReadAll(r.Body)
	defer clear(raw)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeProblem(w, r, http.StatusRequestEntityTooLarge, "PayloadTooLarge", "Payload too large", "The registry credential request exceeds the encoded request limit.")
		} else {
			writeProblem(w, r, http.StatusBadRequest, "InvalidJSON", "Invalid JSON", "The request body is not valid JSON.")
		}
		return false
	}
	if len(raw) == 0 || !utf8.Valid(raw) || !uniqueTopLevelSecretFields(raw) {
		writeProblem(w, r, http.StatusBadRequest, "InvalidJSON", "Invalid JSON", "The request body must be one valid UTF-8 JSON object with unique fields.")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(output); err != nil {
		if errors.Is(err, registrycredentials.ErrInvalid) {
			mappedRegistryCredentialError(w, r, err)
		} else {
			writeProblem(w, r, http.StatusBadRequest, "InvalidJSON", "Invalid JSON", "The request body is not valid for this operation.")
		}
		return false
	}
	if err = decoder.Decode(&struct{}{}); err != io.EOF {
		writeProblem(w, r, http.StatusBadRequest, "InvalidJSON", "Invalid JSON", "The request must contain exactly one JSON object.")
		return false
	}
	return true
}

func registryCredentialEnvironmentQuery(w http.ResponseWriter, r *http.Request) (string, bool) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(values) > 1 {
		writeProblem(w, r, http.StatusBadRequest, "InvalidQuery", "Invalid query", "Only one environmentId query parameter is allowed.")
		return "", false
	}
	for key, entries := range values {
		if key != "environmentId" || len(entries) != 1 {
			writeProblem(w, r, http.StatusBadRequest, "InvalidQuery", "Invalid query", "Only one environmentId query parameter is allowed.")
			return "", false
		}
	}
	if !values.Has("environmentId") {
		return "", true
	}
	value := strings.TrimSpace(values.Get("environmentId"))
	if value == "" || value != values.Get("environmentId") {
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "environmentId must be one non-empty UUID when provided.")
		return "", false
	}
	return value, true
}

func registryCredentialQueryEmpty(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.RawQuery != "" {
		writeProblem(w, r, http.StatusBadRequest, "InvalidQuery", "Invalid query", "This operation does not accept query parameters.")
		return false
	}
	return true
}

func registryCredentialBodyEmpty(w http.ResponseWriter, r *http.Request) bool {
	if r.Body == nil {
		return true
	}
	var one [1]byte
	n, err := r.Body.Read(one[:])
	clear(one[:])
	if n != 0 || err != nil && err != io.EOF {
		writeProblem(w, r, http.StatusBadRequest, "UnexpectedBody", "Unexpected request body", "This operation does not accept a request body.")
		return false
	}
	return true
}

func registryCredentialBackendUnavailable(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, http.StatusServiceUnavailable, "RegistryCredentialBindingsUnavailable", "Registry pull credentials unavailable", "The registry pull credential lifecycle is not available for this installation.")
}

func mappedRegistryCredentialError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, secrets.ErrNotFound), errors.Is(err, registrycredentials.ErrNotFound):
		writeProblem(w, r, http.StatusNotFound, "NotFound", "Not found", "The requested resource was not found.")
	case errors.Is(err, store.ErrForbidden):
		writeProblem(w, r, http.StatusForbidden, "Forbidden", "Forbidden", "You do not have access to perform this action.")
	case errors.Is(err, registrycredentials.ErrInvalid), errors.Is(err, secrets.ErrInvalid):
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "The registry credential request is invalid.")
	case errors.Is(err, registrycredentials.ErrConflict), errors.Is(err, registrycredentials.ErrNotReady), errors.Is(err, secrets.ErrConflict), errors.Is(err, secrets.ErrReferenced), errors.Is(err, secrets.ErrNotReady):
		writeProblem(w, r, http.StatusConflict, "RegistryCredentialBindingConflict", "Registry credential conflict", "The request conflicts with the current immutable registry credential state, idempotency record, readiness, or retained references.")
	case errors.Is(err, registrycredentials.ErrUnavailable), errors.Is(err, secrets.ErrRuntimeUnavailable), errors.Is(err, secrets.ErrFingerprintKeyUnavailable):
		registryCredentialBackendUnavailable(w, r)
	case errors.Is(err, secrets.ErrProviderOperation), errors.Is(err, secrets.ErrProviderMismatch):
		writeProblem(w, r, http.StatusBadGateway, "RegistryCredentialProviderFailed", "Registry credential provider failed", "The registry credential provider could not safely complete or verify the operation.")
	default:
		mappedError(w, r, err)
	}
}

var _ RegistryCredentialManagementBackend = (*registryCredentialManagementBackend)(nil)
