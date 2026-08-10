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

	"github.com/kuberploy/kuberploy/internal/certificates"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/secrets"
	"github.com/kuberploy/kuberploy/internal/store"
)

const maxCertificateRequestBytes = 256 << 10

// CertificateBindingLister is deliberately metadata-only. The certificate
// service owns all PEM ingestion and validation; this seam only supplies the
// already-scoped catalog needed by the management collection route.
type CertificateBindingLister interface {
	ListBindings(context.Context, string, string) ([]secrets.Binding, error)
}

// CertificateManagementBackend is separate from RuntimeSecretBackend so the
// generic secret API can never rotate, list, or delete TLS private keys.
type CertificateManagementBackend interface {
	Create(context.Context, certificates.CreateRequest) (certificates.MutationResult, error)
	Rotate(context.Context, certificates.RotateRequest) (certificates.MutationResult, error)
	Delete(context.Context, string, string, string) (secrets.Binding, error)
	Binding(context.Context, string) (secrets.Binding, []certificates.Version, error)
	ListBindings(context.Context, string, string) ([]secrets.Binding, error)
}

type certificateManagementBackend struct {
	service certificates.Service
	lister  CertificateBindingLister
}

// NewCertificateManagementBackend adapts the existing certificate service
// without widening its write-only boundary. Production must still inject an
// independent exact readiness probe before any route or capability opens.
func NewCertificateManagementBackend(service certificates.Service, lister CertificateBindingLister) (CertificateManagementBackend, error) {
	if service.Secrets == nil || service.Catalog == nil || service.Store == nil || lister == nil {
		return nil, certificates.ErrInvalid
	}
	return &certificateManagementBackend{service: service, lister: lister}, nil
}

func (b *certificateManagementBackend) Create(ctx context.Context, request certificates.CreateRequest) (certificates.MutationResult, error) {
	return b.service.Create(ctx, request)
}

func (b *certificateManagementBackend) Rotate(ctx context.Context, request certificates.RotateRequest) (certificates.MutationResult, error) {
	return b.service.Rotate(ctx, request)
}

func (b *certificateManagementBackend) Delete(ctx context.Context, actorID, bindingID, requestID string) (secrets.Binding, error) {
	return b.service.Delete(ctx, actorID, bindingID, requestID)
}

func (b *certificateManagementBackend) Binding(ctx context.Context, bindingID string) (secrets.Binding, []certificates.Version, error) {
	return b.service.Binding(ctx, bindingID)
}

func (b *certificateManagementBackend) ListBindings(ctx context.Context, applicationID, environmentID string) ([]secrets.Binding, error) {
	return b.lister.ListBindings(ctx, applicationID, environmentID)
}

type certificatePEMValue []byte
type certificatePrivateKeyPEMValue []byte

func decodeBoundedPEM(data []byte, limit int) ([]byte, error) {
	var value string
	if len(data) == 0 || json.Unmarshal(data, &value) != nil || !utf8.ValidString(value) {
		return nil, certificates.ErrInvalid
	}
	decoded := []byte(value)
	value = ""
	if len(decoded) == 0 || len(decoded) > limit {
		clear(decoded)
		return nil, certificates.ErrInvalid
	}
	return decoded, nil
}

func (v *certificatePEMValue) UnmarshalJSON(data []byte) error {
	clear(*v)
	decoded, err := decodeBoundedPEM(data, certificates.MaxCertificatePEMBytes)
	if err != nil {
		return err
	}
	*v = decoded
	return nil
}

func (v *certificatePrivateKeyPEMValue) UnmarshalJSON(data []byte) error {
	clear(*v)
	decoded, err := decodeBoundedPEM(data, certificates.MaxPrivateKeyPEMBytes)
	if err != nil {
		return err
	}
	*v = decoded
	return nil
}

type certificateCreateRequest struct {
	EnvironmentID string                        `json:"environmentId"`
	Name          string                        `json:"name"`
	Certificate   certificatePEMValue           `json:"certificatePem"`
	PrivateKey    certificatePrivateKeyPEMValue `json:"privateKeyPem"`
}

func (r *certificateCreateRequest) Destroy() {
	clear(r.Certificate)
	clear(r.PrivateKey)
	r.Certificate = nil
	r.PrivateKey = nil
}

type certificateRotateRequest struct {
	ExpectedActiveVersion int64                         `json:"expectedActiveVersion"`
	Certificate           certificatePEMValue           `json:"certificatePem"`
	PrivateKey            certificatePrivateKeyPEMValue `json:"privateKeyPem"`
}

func (r *certificateRotateRequest) Destroy() {
	clear(r.Certificate)
	clear(r.PrivateKey)
	r.Certificate = nil
	r.PrivateKey = nil
}

type certificateBindingMetadataView struct {
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

type certificateVersionMetadataView struct {
	Number               int64     `json:"number"`
	LeafFingerprint      string    `json:"leafFingerprint"`
	PublicKeyFingerprint string    `json:"publicKeyFingerprint"`
	DNSNames             []string  `json:"dnsNames"`
	IPAddresses          []string  `json:"ipAddresses"`
	NotBefore            time.Time `json:"notBefore"`
	NotAfter             time.Time `json:"notAfter"`
	CreatedBy            string    `json:"createdBy"`
	CreatedAt            time.Time `json:"createdAt"`
}

type certificateBindingDetailView struct {
	certificateBindingMetadataView
	Versions []certificateVersionMetadataView `json:"versions"`
}

func safeCertificateBinding(binding secrets.Binding) certificateBindingMetadataView {
	return certificateBindingMetadataView{
		ID: binding.ID, ApplicationID: binding.Scope.ApplicationID, EnvironmentID: binding.Scope.EnvironmentID,
		Name: binding.Name, State: binding.State, ActiveVersion: binding.ActiveVersion, CreatedBy: binding.CreatedBy,
		CreatedAt: binding.CreatedAt, UpdatedAt: binding.UpdatedAt, DeleteStartedAt: nonzeroTime(binding.DeleteStarted),
		DeletedAt: nonzeroTime(binding.DeletedAt),
	}
}

func safeCertificateVersion(version certificates.Version) certificateVersionMetadataView {
	return certificateVersionMetadataView{
		Number: version.Number, LeafFingerprint: version.LeafFingerprint, PublicKeyFingerprint: version.PublicKeyFingerprint,
		DNSNames: append([]string(nil), version.DNSNames...), IPAddresses: append([]string(nil), version.IPAddresses...),
		NotBefore: version.NotBefore, NotAfter: version.NotAfter, CreatedBy: version.CreatedBy, CreatedAt: version.CreatedAt,
	}
}

func (s *Server) certificateReady(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.certificates == nil || s.certificateReadiness == nil {
			certificateBackendUnavailable(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.certificateReadiness.Probe(ctx); err != nil {
			certificateBackendUnavailable(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) createCertificateBinding(w http.ResponseWriter, r *http.Request) {
	if !certificateQueryEmpty(w, r) {
		return
	}
	key, ok := secretIdempotencyKey(w, r)
	if !ok {
		return
	}
	var input certificateCreateRequest
	defer input.Destroy()
	if !decodeCertificateRequest(w, r, &input) {
		return
	}
	applicationID := strings.TrimSpace(r.PathValue("id"))
	scope, target, err := s.resolveSecretScope(r.Context(), applicationID, strings.TrimSpace(input.EnvironmentID), applicationID)
	if err != nil {
		mappedCertificateError(w, r, err)
		return
	}
	if err = s.store.Authorize(r.Context(), currentUser(r.Context()).ID, domain.PermissionCertificatesCreate, target); err != nil {
		mappedError(w, r, err)
		return
	}
	material, err := certificates.NewMaterial([]byte(input.Certificate), []byte(input.PrivateKey))
	input.Destroy()
	if err != nil {
		mappedCertificateError(w, r, err)
		return
	}
	defer material.Destroy()
	result, err := s.certificates.Create(r.Context(), certificates.CreateRequest{
		ActorID: currentUser(r.Context()).ID, Scope: scope, Name: strings.TrimSpace(input.Name), IdempotencyKey: key,
		RequestID: safeSecretRequestID(r.Context()), Material: material,
	})
	if err != nil {
		mappedCertificateError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Location", "/v1/certificate-bindings/"+result.Binding.ID)
	writeJSON(w, http.StatusCreated, certificateBindingDetailView{
		certificateBindingMetadataView: safeCertificateBinding(result.Binding),
		Versions:                       []certificateVersionMetadataView{safeCertificateVersion(result.Certificate)},
	})
}

func (s *Server) listCertificateBindings(w http.ResponseWriter, r *http.Request) {
	applicationID := strings.TrimSpace(r.PathValue("id"))
	environmentID, ok := certificateEnvironmentQuery(w, r)
	if !ok {
		return
	}
	if environmentID == "" {
		application, err := s.store.GetApplication(r.Context(), applicationID)
		if err != nil {
			mappedError(w, r, err)
			return
		}
		if err = s.store.Authorize(r.Context(), currentUser(r.Context()).ID, domain.PermissionCertificatesRead, domain.AccessTarget{Type: "application", ID: application.ID}); err != nil {
			mappedError(w, r, err)
			return
		}
	} else {
		_, target, err := s.resolveSecretScope(r.Context(), applicationID, environmentID, applicationID)
		if err != nil {
			mappedCertificateError(w, r, err)
			return
		}
		if err = s.store.Authorize(r.Context(), currentUser(r.Context()).ID, domain.PermissionCertificatesRead, target); err != nil {
			mappedError(w, r, err)
			return
		}
	}
	bindings, err := s.certificates.ListBindings(r.Context(), applicationID, environmentID)
	if err != nil {
		mappedCertificateError(w, r, err)
		return
	}
	items := make([]certificateBindingMetadataView, 0, len(bindings))
	for _, binding := range bindings {
		if binding.Purpose != secrets.PurposeTLSCertificate {
			continue
		}
		resolved, target, resolveErr := s.resolveSecretScope(r.Context(), binding.Scope.ApplicationID, binding.Scope.EnvironmentID, binding.ID)
		if resolveErr != nil || resolved != binding.Scope || binding.Scope.ApplicationID != applicationID || environmentID != "" && binding.Scope.EnvironmentID != environmentID {
			mappedError(w, r, store.ErrNotFound)
			return
		}
		if err = s.store.Authorize(r.Context(), currentUser(r.Context()).ID, domain.PermissionCertificatesRead, target); err != nil {
			mappedError(w, r, err)
			return
		}
		items = append(items, safeCertificateBinding(binding))
	}
	collection(w, items)
}

func (s *Server) getCertificateBinding(w http.ResponseWriter, r *http.Request) {
	if !certificateQueryEmpty(w, r) {
		return
	}
	binding, versions, ok := s.authorizedCertificateBinding(w, r, domain.PermissionCertificatesRead)
	if !ok {
		return
	}
	views := make([]certificateVersionMetadataView, 0, len(versions))
	for _, version := range versions {
		views = append(views, safeCertificateVersion(version))
	}
	writeJSON(w, http.StatusOK, certificateBindingDetailView{certificateBindingMetadataView: safeCertificateBinding(binding), Versions: views})
}

func (s *Server) rotateCertificateBinding(w http.ResponseWriter, r *http.Request) {
	if !certificateQueryEmpty(w, r) {
		return
	}
	key, ok := secretIdempotencyKey(w, r)
	if !ok {
		return
	}
	binding, _, ok := s.authorizedCertificateBinding(w, r, domain.PermissionCertificatesRotate)
	if !ok {
		return
	}
	var input certificateRotateRequest
	defer input.Destroy()
	if !decodeCertificateRequest(w, r, &input) {
		return
	}
	material, err := certificates.NewMaterial([]byte(input.Certificate), []byte(input.PrivateKey))
	input.Destroy()
	if err != nil {
		mappedCertificateError(w, r, err)
		return
	}
	defer material.Destroy()
	result, err := s.certificates.Rotate(r.Context(), certificates.RotateRequest{
		ActorID: currentUser(r.Context()).ID, BindingID: binding.ID, ExpectedActiveVersion: input.ExpectedActiveVersion,
		IdempotencyKey: key, RequestID: safeSecretRequestID(r.Context()), Material: material,
	})
	if err != nil {
		mappedCertificateError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeJSON(w, http.StatusCreated, certificateBindingDetailView{
		certificateBindingMetadataView: safeCertificateBinding(result.Binding),
		Versions:                       []certificateVersionMetadataView{safeCertificateVersion(result.Certificate)},
	})
}

func (s *Server) deleteCertificateBinding(w http.ResponseWriter, r *http.Request) {
	if !certificateQueryEmpty(w, r) || !certificateBodyEmpty(w, r) {
		return
	}
	if _, ok := secretIdempotencyKey(w, r); !ok {
		return
	}
	binding, _, ok := s.authorizedCertificateBinding(w, r, domain.PermissionCertificatesDelete)
	if !ok {
		return
	}
	wasDeleted := binding.State == secrets.BindingDeleted
	if _, err := s.certificates.Delete(r.Context(), currentUser(r.Context()).ID, binding.ID, safeSecretRequestID(r.Context())); err != nil {
		mappedCertificateError(w, r, err)
		return
	}
	if wasDeleted {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) authorizedCertificateBinding(w http.ResponseWriter, r *http.Request, permission domain.Permission) (secrets.Binding, []certificates.Version, bool) {
	binding, versions, err := s.certificates.Binding(r.Context(), strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		mappedCertificateError(w, r, err)
		return secrets.Binding{}, nil, false
	}
	if binding.Purpose != secrets.PurposeTLSCertificate {
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

func decodeCertificateRequest(w http.ResponseWriter, r *http.Request, output any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeProblem(w, r, http.StatusUnsupportedMediaType, "UnsupportedMediaType", "Unsupported media type", "Certificate mutations require application/json.")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxCertificateRequestBytes)
	raw, err := io.ReadAll(r.Body)
	defer clear(raw)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeProblem(w, r, http.StatusRequestEntityTooLarge, "PayloadTooLarge", "Payload too large", "The certificate request exceeds the encoded request limit.")
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
		if errors.Is(err, certificates.ErrInvalid) {
			mappedCertificateError(w, r, err)
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

func certificateEnvironmentQuery(w http.ResponseWriter, r *http.Request) (string, bool) {
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

func certificateQueryEmpty(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.RawQuery != "" {
		writeProblem(w, r, http.StatusBadRequest, "InvalidQuery", "Invalid query", "This operation does not accept query parameters.")
		return false
	}
	return true
}

func certificateBodyEmpty(w http.ResponseWriter, r *http.Request) bool {
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

func certificateBackendUnavailable(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, http.StatusServiceUnavailable, "CertificateBindingsUnavailable", "Custom certificates unavailable", "The exact custom-certificate lifecycle and readiness boundary is not available for this installation.")
}

func mappedCertificateError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, secrets.ErrNotFound), errors.Is(err, certificates.ErrNotFound):
		writeProblem(w, r, http.StatusNotFound, "NotFound", "Not found", "The requested resource was not found.")
	case errors.Is(err, store.ErrForbidden):
		writeProblem(w, r, http.StatusForbidden, "Forbidden", "Forbidden", "You do not have access to perform this action.")
	case errors.Is(err, certificates.ErrInvalid), errors.Is(err, secrets.ErrInvalid):
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "The certificate request is invalid.")
	case errors.Is(err, certificates.ErrConflict), errors.Is(err, certificates.ErrNotReady), errors.Is(err, secrets.ErrConflict), errors.Is(err, secrets.ErrReferenced), errors.Is(err, secrets.ErrNotReady):
		writeProblem(w, r, http.StatusConflict, "CertificateBindingConflict", "Certificate conflict", "The request conflicts with the current immutable certificate state, idempotency record, readiness, or retained references.")
	case errors.Is(err, certificates.ErrUnavailable), errors.Is(err, secrets.ErrRuntimeUnavailable), errors.Is(err, secrets.ErrFingerprintKeyUnavailable):
		certificateBackendUnavailable(w, r)
	case errors.Is(err, secrets.ErrProviderOperation), errors.Is(err, secrets.ErrProviderMismatch):
		writeProblem(w, r, http.StatusBadGateway, "CertificateProviderFailed", "Certificate provider failed", "The certificate provider could not safely complete or verify the operation.")
	default:
		mappedError(w, r, err)
	}
}

var _ CertificateManagementBackend = (*certificateManagementBackend)(nil)
