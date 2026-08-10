package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/middlewareprofiles"
	"github.com/kuberploy/kuberploy/internal/secrets"
	"github.com/kuberploy/kuberploy/internal/store"
)

const maxSecretRequestBytes = 2 << 20

var secretIdempotencyKeyRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$`)
var secretRequestIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
var secretMaterialKeyRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,253}$`)

// RuntimeSecretBackend is separate from store.Store: runtime-secret plaintext
// may only enter the dedicated write-only lifecycle, while central Store is
// used solely to resolve ancestry and authorize it.
type RuntimeSecretBackend interface {
	ProviderAvailable(secrets.ProviderKind) bool
	AllowsNamespace(string) bool
	ResolveReferences(context.Context, secrets.Scope, domain.WorkloadRuntime) (secrets.BindingReferencePlan, error)
	Create(context.Context, secrets.CreateRequest) (secrets.MutationResult, error)
	Rotate(context.Context, secrets.RotateRequest) (secrets.MutationResult, error)
	Delete(context.Context, string, string, string) (secrets.Binding, error)
	Binding(context.Context, string) (secrets.Binding, error)
	ListBindings(context.Context, string, string) ([]secrets.Binding, error)
	Versions(context.Context, string) ([]secrets.Version, error)
}

type runtimeSecretBackend struct {
	service secrets.Service
	config  secrets.RuntimeConfig
}

// NewRuntimeSecretBackend is intentionally strict-Sealed-Secrets-only. The
// External Secrets adapter remains a non-production library seam until a
// concrete RemoteMaterialWriter and its authorization boundary are audited.
func NewRuntimeSecretBackend(service secrets.Service, config secrets.RuntimeConfig) (RuntimeSecretBackend, error) {
	if config.Validate() != nil || service.Store == nil || service.Keys == nil || service.SealedSecrets == nil || service.ExternalSecrets != nil {
		return nil, secrets.ErrInvalid
	}
	return &runtimeSecretBackend{service: service, config: config}, nil
}

func (b *runtimeSecretBackend) ProviderAvailable(provider secrets.ProviderKind) bool {
	return b != nil && provider == secrets.ProviderSealedSecrets && b.service.SealedSecrets != nil
}
func (b *runtimeSecretBackend) AllowsNamespace(namespace string) bool {
	return b != nil && b.config.AllowsNamespace(namespace)
}
func (b *runtimeSecretBackend) ResolveReferences(ctx context.Context, scope secrets.Scope, runtime domain.WorkloadRuntime) (secrets.BindingReferencePlan, error) {
	if b == nil || !b.config.AllowsNamespace(scope.Namespace) {
		return secrets.BindingReferencePlan{}, secrets.ErrRuntimeUnavailable
	}
	return secrets.ResolveWorkloadBindingReferences(ctx, b.service.Store, scope, runtime)
}
func (b *runtimeSecretBackend) Create(ctx context.Context, request secrets.CreateRequest) (secrets.MutationResult, error) {
	return b.service.Create(ctx, request)
}
func (b *runtimeSecretBackend) Rotate(ctx context.Context, request secrets.RotateRequest) (secrets.MutationResult, error) {
	return b.service.Rotate(ctx, request)
}
func (b *runtimeSecretBackend) Delete(ctx context.Context, actorID, bindingID, requestID string) (secrets.Binding, error) {
	return b.service.Delete(ctx, actorID, bindingID, requestID)
}
func (b *runtimeSecretBackend) Binding(ctx context.Context, bindingID string) (secrets.Binding, error) {
	return b.service.Store.Binding(ctx, bindingID)
}
func (b *runtimeSecretBackend) ListBindings(ctx context.Context, applicationID, environmentID string) ([]secrets.Binding, error) {
	return b.service.Store.ListBindings(ctx, applicationID, environmentID)
}
func (b *runtimeSecretBackend) Versions(ctx context.Context, bindingID string) ([]secrets.Version, error) {
	return b.service.Store.Versions(ctx, bindingID)
}

// resolveAppConfigReferencePlan authorizes and resolves only safe immutable
// metadata before a preview or write. PostgreSQL repeats the same resolution
// and secrets.bind authorization under row locks in the Git command
// transaction; this result is only a TOCTOU-resistant digest expectation.
func (s *Server) resolveAppConfigReferencePlan(ctx context.Context, actor string, deployment domain.Deployment, runtime domain.WorkloadRuntime) (*store.AppConfigReferencePlan, error) {
	usesReferences := false
	for _, variable := range runtime.Env {
		if variable.ValueFrom != nil {
			usesReferences = true
			break
		}
	}
	// Ordinary AppConfigs must not acquire a runtime-secret dependency merely
	// because the subsystem is configured. In particular, platform-owned
	// projects legitimately have no team scope to resolve when there are no
	// secret references.
	if !usesReferences {
		return nil, nil
	}
	if s.runtimeSecrets == nil || s.runtimeSecretReadiness == nil || s.gitProjection == nil || s.gitReadiness == nil {
		return nil, secrets.ErrRuntimeUnavailable
	}
	probeContext, cancel := context.WithTimeout(ctx, 2*time.Second)
	if err := s.runtimeSecretReadiness.Probe(probeContext); err != nil {
		cancel()
		return nil, secrets.ErrRuntimeUnavailable
	}
	if err := s.gitReadiness.Probe(probeContext); err != nil {
		cancel()
		return nil, secrets.ErrRuntimeUnavailable
	}
	cancel()
	scope, target, err := s.resolveSecretScope(ctx, deployment.ApplicationID, deployment.EnvironmentID, deployment.ApplicationID)
	if err != nil {
		return nil, err
	}
	if !s.runtimeSecrets.AllowsNamespace(scope.Namespace) {
		return nil, secrets.ErrRuntimeUnavailable
	}
	authorized := map[string]struct{}{}
	for _, variable := range runtime.Env {
		if variable.ValueFrom == nil {
			continue
		}
		bindingID := variable.ValueFrom.SecretBindingRef.BindingID
		if _, exists := authorized[bindingID]; exists {
			continue
		}
		target.ID = bindingID
		if err = s.store.Authorize(ctx, actor, domain.PermissionSecretsBind, target); err != nil {
			return nil, err
		}
		authorized[bindingID] = struct{}{}
	}
	resolved, err := s.runtimeSecrets.ResolveReferences(ctx, scope, runtime)
	if err != nil {
		return nil, err
	}
	digest, err := resolved.Digest()
	if err != nil {
		return nil, err
	}
	return &store.AppConfigReferencePlan{RuntimeSecretDigest: digest}, nil
}

func (s *Server) validateMiddlewareSecretReferences(ctx context.Context, actor string, deployment domain.Deployment, parsed map[string]any) error {
	refs, err := middlewareprofiles.AppConfigSecretReferences(parsed)
	if err != nil || len(refs) == 0 {
		return err
	}
	if s.runtimeSecrets == nil || s.runtimeSecretReadiness == nil || s.gitProjection == nil || s.gitReadiness == nil {
		return secrets.ErrRuntimeUnavailable
	}
	probeContext, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if s.runtimeSecretReadiness.Probe(probeContext) != nil || s.gitReadiness.Probe(probeContext) != nil {
		return secrets.ErrRuntimeUnavailable
	}
	scope, target, err := s.resolveSecretScope(ctx, deployment.ApplicationID, deployment.EnvironmentID, deployment.ApplicationID)
	if err != nil {
		return err
	}
	if !s.runtimeSecrets.AllowsNamespace(scope.Namespace) {
		return secrets.ErrRuntimeUnavailable
	}
	authorized := map[string]struct{}{}
	for _, ref := range refs {
		if _, exists := authorized[ref.BindingID]; exists {
			continue
		}
		target.ID = ref.BindingID
		if err = s.store.Authorize(ctx, actor, domain.PermissionSecretsBind, target); err != nil {
			return err
		}
		authorized[ref.BindingID] = struct{}{}
	}
	_, err = secrets.ResolveMiddlewareBindingReferences(ctx, s.runtimeSecrets, scope, refs)
	return err
}

type secretValues map[string][]byte

func (v *secretValues) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return secrets.ErrInvalid
	}
	values := make(secretValues)
	total := 0
	for decoder.More() {
		keyToken, tokenErr := decoder.Token()
		key, keyOK := keyToken.(string)
		if tokenErr != nil || !keyOK || !secretMaterialKeyRE.MatchString(key) || len(values) >= secrets.MaxMaterialKeys {
			values.Destroy()
			return secrets.ErrInvalid
		}
		if _, duplicate := values[key]; duplicate {
			values.Destroy()
			return secrets.ErrInvalid
		}
		var raw json.RawMessage
		if err = decoder.Decode(&raw); err != nil {
			values.Destroy()
			return secrets.ErrInvalid
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil || !utf8.ValidString(value) {
			clear(raw)
			values.Destroy()
			return secrets.ErrInvalid
		}
		clear(raw)
		decoded := []byte(value)
		value = ""
		if len(decoded) == 0 || len(decoded) > secrets.MaxValueBytes || total+len(decoded) > secrets.MaxMaterialBytes {
			clear(decoded)
			values.Destroy()
			return secrets.ErrInvalid
		}
		values[key] = decoded
		total += len(decoded)
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') || len(values) == 0 {
		values.Destroy()
		return secrets.ErrInvalid
	}
	if err = decoder.Decode(&struct{}{}); err != io.EOF {
		values.Destroy()
		return secrets.ErrInvalid
	}
	*v = values
	return nil
}

func (v secretValues) Destroy() {
	for key, value := range v {
		clear(value)
		delete(v, key)
	}
}

type secretCreateRequest struct {
	EnvironmentID string               `json:"environmentId"`
	Name          string               `json:"name"`
	Provider      secrets.ProviderKind `json:"provider"`
	Deliveries    []secrets.Delivery   `json:"deliveries"`
	Values        secretValues         `json:"values"`
}

type secretRotateRequest struct {
	ExpectedActiveVersion int64              `json:"expectedActiveVersion"`
	Deliveries            []secrets.Delivery `json:"deliveries"`
	Values                secretValues       `json:"values"`
}

type secretBindingMetadataView struct {
	ID              string               `json:"id"`
	ApplicationID   string               `json:"applicationId"`
	EnvironmentID   string               `json:"environmentId"`
	Name            string               `json:"name"`
	Provider        secrets.ProviderKind `json:"provider"`
	State           secrets.BindingState `json:"state"`
	ActiveVersion   int64                `json:"activeVersion,omitempty"`
	CreatedBy       string               `json:"createdBy"`
	CreatedAt       time.Time            `json:"createdAt"`
	UpdatedAt       time.Time            `json:"updatedAt"`
	DeleteStartedAt *time.Time           `json:"deleteStartedAt,omitempty"`
	DeletedAt       *time.Time           `json:"deletedAt,omitempty"`
}

type secretVersionView struct {
	ID                  string               `json:"id"`
	Number              int64                `json:"number"`
	State               secrets.VersionState `json:"state"`
	Deliveries          []secrets.Delivery   `json:"deliveries"`
	FailureCode         string               `json:"failureCode,omitempty"`
	StagedAt            *time.Time           `json:"stagedAt,omitempty"`
	ReadinessObservedAt *time.Time           `json:"readinessObservedAt,omitempty"`
	ActivatedAt         *time.Time           `json:"activatedAt,omitempty"`
	RetainedAt          *time.Time           `json:"retainedAt,omitempty"`
	CreatedAt           time.Time            `json:"createdAt"`
	UpdatedAt           time.Time            `json:"updatedAt"`
}

type secretBindingDetailView struct {
	secretBindingMetadataView
	Versions []secretVersionView `json:"versions"`
}

func safeSecretBinding(binding secrets.Binding) secretBindingMetadataView {
	return secretBindingMetadataView{
		ID: binding.ID, ApplicationID: binding.Scope.ApplicationID, EnvironmentID: binding.Scope.EnvironmentID,
		Name: binding.Name, Provider: binding.Provider, State: binding.State, ActiveVersion: binding.ActiveVersion,
		CreatedBy: binding.CreatedBy, CreatedAt: binding.CreatedAt, UpdatedAt: binding.UpdatedAt,
		DeleteStartedAt: nonzeroTime(binding.DeleteStarted), DeletedAt: nonzeroTime(binding.DeletedAt),
	}
}

func safeSecretVersion(version secrets.Version) secretVersionView {
	return secretVersionView{
		ID: version.ID, Number: version.Number, State: version.State, Deliveries: append([]secrets.Delivery(nil), version.Deliveries...),
		FailureCode: version.FailureCode, StagedAt: nonzeroTime(version.StagedAt), ReadinessObservedAt: nonzeroTime(version.ReadinessObservedAt),
		ActivatedAt: nonzeroTime(version.ActivatedAt), RetainedAt: nonzeroTime(version.RetainedAt), CreatedAt: version.CreatedAt, UpdatedAt: version.UpdatedAt,
	}
}

func nonzeroTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	copy := value.UTC()
	return &copy
}

func (s *Server) secretNoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) runtimeSecretReady(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.runtimeSecrets == nil || s.runtimeSecretReadiness == nil {
			secretBackendUnavailable(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.runtimeSecretReadiness.Probe(ctx); err != nil {
			secretBackendUnavailable(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) createSecretBinding(w http.ResponseWriter, r *http.Request) {
	applicationID := strings.TrimSpace(r.PathValue("id"))
	key, ok := secretIdempotencyKey(w, r)
	if !ok {
		return
	}
	if s.runtimeSecrets == nil {
		secretBackendUnavailable(w, r)
		return
	}
	var input secretCreateRequest
	defer func() { input.Values.Destroy() }()
	if !decodeSecretRequest(w, r, &input) {
		return
	}
	scope, target, err := s.resolveSecretScope(r.Context(), applicationID, strings.TrimSpace(input.EnvironmentID), applicationID)
	if err != nil {
		mappedSecretError(w, r, err)
		return
	}
	if !s.runtimeSecrets.AllowsNamespace(scope.Namespace) {
		secretBackendUnavailable(w, r)
		return
	}
	if err = s.store.Authorize(r.Context(), currentUser(r.Context()).ID, domain.PermissionSecretsCreate, target); err != nil {
		mappedError(w, r, err)
		return
	}
	if input.Provider != secrets.ProviderExternalSecrets && input.Provider != secrets.ProviderSealedSecrets {
		mappedSecretError(w, r, secrets.ErrInvalid)
		return
	}
	if !s.runtimeSecrets.ProviderAvailable(input.Provider) {
		writeProblem(w, r, http.StatusServiceUnavailable, "SecretProviderUnavailable", "Secret provider unavailable", "The selected runtime-secret provider is not configured.")
		return
	}
	material, err := secrets.NewMaterial(map[string][]byte(input.Values))
	input.Values.Destroy()
	if err != nil {
		mappedSecretError(w, r, err)
		return
	}
	defer material.Destroy()
	result, err := s.runtimeSecrets.Create(r.Context(), secrets.CreateRequest{ActorID: currentUser(r.Context()).ID, Scope: scope,
		Name: strings.TrimSpace(input.Name), Provider: input.Provider, Deliveries: input.Deliveries, IdempotencyKey: key,
		RequestID: safeSecretRequestID(r.Context()), Material: material})
	if err != nil {
		mappedSecretError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Location", "/v1/secret-bindings/"+result.Binding.ID)
	writeJSON(w, http.StatusCreated, secretBindingDetailView{secretBindingMetadataView: safeSecretBinding(result.Binding), Versions: []secretVersionView{safeSecretVersion(result.Version)}})
}

func (s *Server) listSecretBindings(w http.ResponseWriter, r *http.Request) {
	applicationID := strings.TrimSpace(r.PathValue("id"))
	environmentID := strings.TrimSpace(r.URL.Query().Get("environmentId"))
	if r.URL.Query().Has("environmentId") && environmentID == "" {
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "environmentId must be a non-empty UUID when provided.")
		return
	}
	if environmentID == "" {
		application, err := s.store.GetApplication(r.Context(), applicationID)
		if err != nil {
			mappedError(w, r, err)
			return
		}
		if err = s.store.Authorize(r.Context(), currentUser(r.Context()).ID, domain.PermissionSecretsRead, domain.AccessTarget{Type: "application", ID: application.ID}); err != nil {
			mappedError(w, r, err)
			return
		}
	} else {
		resolved, target, err := s.resolveSecretScope(r.Context(), applicationID, environmentID, applicationID)
		if err != nil {
			mappedSecretError(w, r, err)
			return
		}
		if s.runtimeSecrets == nil || !s.runtimeSecrets.AllowsNamespace(resolved.Namespace) {
			secretBackendUnavailable(w, r)
			return
		}
		if err = s.store.Authorize(r.Context(), currentUser(r.Context()).ID, domain.PermissionSecretsRead, target); err != nil {
			mappedError(w, r, err)
			return
		}
	}
	if s.runtimeSecrets == nil {
		secretBackendUnavailable(w, r)
		return
	}
	bindings, err := s.runtimeSecrets.ListBindings(r.Context(), applicationID, environmentID)
	if err != nil {
		mappedSecretError(w, r, err)
		return
	}
	items := make([]secretBindingMetadataView, 0, len(bindings))
	for _, binding := range bindings {
		if binding.Purpose != secrets.PurposeRuntimeSecret {
			continue
		}
		if !s.runtimeSecrets.AllowsNamespace(binding.Scope.Namespace) {
			secretBackendUnavailable(w, r)
			return
		}
		resolved, target, resolveErr := s.resolveSecretScope(r.Context(), binding.Scope.ApplicationID, binding.Scope.EnvironmentID, binding.ID)
		if resolveErr != nil || resolved != binding.Scope || binding.Scope.ApplicationID != applicationID || environmentID != "" && binding.Scope.EnvironmentID != environmentID {
			mappedError(w, r, store.ErrNotFound)
			return
		}
		if authorizeErr := s.store.Authorize(r.Context(), currentUser(r.Context()).ID, domain.PermissionSecretsRead, target); authorizeErr != nil {
			mappedError(w, r, authorizeErr)
			return
		}
		items = append(items, safeSecretBinding(binding))
	}
	collection(w, items)
}

func (s *Server) getSecretBinding(w http.ResponseWriter, r *http.Request) {
	binding, versions, ok := s.authorizedSecretBinding(w, r, domain.PermissionSecretsRead)
	if !ok {
		return
	}
	views := make([]secretVersionView, 0, len(versions))
	for _, version := range versions {
		views = append(views, safeSecretVersion(version))
	}
	writeJSON(w, http.StatusOK, secretBindingDetailView{secretBindingMetadataView: safeSecretBinding(binding), Versions: views})
}

func (s *Server) rotateSecretBinding(w http.ResponseWriter, r *http.Request) {
	key, ok := secretIdempotencyKey(w, r)
	if !ok {
		return
	}
	binding, _, ok := s.authorizedSecretBinding(w, r, domain.PermissionSecretsRotate)
	if !ok {
		return
	}
	var input secretRotateRequest
	defer func() { input.Values.Destroy() }()
	if !decodeSecretRequest(w, r, &input) {
		return
	}
	material, err := secrets.NewMaterial(map[string][]byte(input.Values))
	input.Values.Destroy()
	if err != nil {
		mappedSecretError(w, r, err)
		return
	}
	defer material.Destroy()
	result, err := s.runtimeSecrets.Rotate(r.Context(), secrets.RotateRequest{ActorID: currentUser(r.Context()).ID, BindingID: binding.ID,
		ExpectedActiveVersion: input.ExpectedActiveVersion, Deliveries: input.Deliveries, IdempotencyKey: key,
		RequestID: safeSecretRequestID(r.Context()), Material: material})
	if err != nil {
		mappedSecretError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeJSON(w, http.StatusCreated, secretBindingDetailView{secretBindingMetadataView: safeSecretBinding(result.Binding), Versions: []secretVersionView{safeSecretVersion(result.Version)}})
}

func (s *Server) deleteSecretBinding(w http.ResponseWriter, r *http.Request) {
	if _, ok := secretIdempotencyKey(w, r); !ok {
		return
	}
	binding, _, ok := s.authorizedSecretBinding(w, r, domain.PermissionSecretsDelete)
	if !ok {
		return
	}
	wasDeleted := binding.State == secrets.BindingDeleted
	if _, err := s.runtimeSecrets.Delete(r.Context(), currentUser(r.Context()).ID, binding.ID, safeSecretRequestID(r.Context())); err != nil {
		mappedSecretError(w, r, err)
		return
	}
	if wasDeleted {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) authorizedSecretBinding(w http.ResponseWriter, r *http.Request, permission domain.Permission) (secrets.Binding, []secrets.Version, bool) {
	if s.runtimeSecrets == nil {
		secretBackendUnavailable(w, r)
		return secrets.Binding{}, nil, false
	}
	binding, err := s.runtimeSecrets.Binding(r.Context(), strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		mappedSecretError(w, r, err)
		return secrets.Binding{}, nil, false
	}
	if binding.Purpose != secrets.PurposeRuntimeSecret {
		mappedError(w, r, store.ErrNotFound)
		return secrets.Binding{}, nil, false
	}
	if !s.runtimeSecrets.AllowsNamespace(binding.Scope.Namespace) {
		secretBackendUnavailable(w, r)
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
	versions, err := s.runtimeSecrets.Versions(r.Context(), binding.ID)
	if err != nil {
		mappedSecretError(w, r, err)
		return secrets.Binding{}, nil, false
	}
	return binding, versions, true
}

func (s *Server) resolveSecretScope(ctx context.Context, applicationID, environmentID, targetID string) (secrets.Scope, domain.AccessTarget, error) {
	application, err := s.store.GetApplication(ctx, applicationID)
	if err != nil {
		return secrets.Scope{}, domain.AccessTarget{}, err
	}
	environment, err := s.store.GetEnvironment(ctx, environmentID)
	if err != nil {
		return secrets.Scope{}, domain.AccessTarget{}, err
	}
	if application.ProjectID == "" || application.ProjectID != environment.ProjectID {
		return secrets.Scope{}, domain.AccessTarget{}, store.ErrNotFound
	}
	project, err := s.store.GetProject(ctx, application.ProjectID)
	if err != nil {
		return secrets.Scope{}, domain.AccessTarget{}, err
	}
	scope := secrets.Scope{OrganizationID: project.TeamID, ProjectID: project.ID, EnvironmentID: environment.ID,
		ApplicationID: application.ID, Namespace: environment.Namespace}
	if scope.Validate() != nil {
		return secrets.Scope{}, domain.AccessTarget{}, secrets.ErrInvalid
	}
	target := domain.AccessTarget{Type: "secret-binding", ID: targetID, TeamID: project.TeamID, ProjectID: project.ID,
		EnvironmentID: environment.ID, Namespace: environment.Namespace, ApplicationID: application.ID}
	return scope, target, nil
}

func decodeSecretRequest(w http.ResponseWriter, r *http.Request, output any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeProblem(w, r, http.StatusUnsupportedMediaType, "UnsupportedMediaType", "Unsupported media type", "Runtime-secret mutations require application/json.")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxSecretRequestBytes)
	raw, err := io.ReadAll(r.Body)
	defer clear(raw)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeProblem(w, r, http.StatusRequestEntityTooLarge, "PayloadTooLarge", "Payload too large", "The runtime-secret request exceeds the encoded request limit.")
		} else {
			writeProblem(w, r, http.StatusBadRequest, "InvalidJSON", "Invalid JSON", "The request body is not valid JSON.")
		}
		return false
	}
	if len(raw) == 0 || !utf8.Valid(raw) {
		writeProblem(w, r, http.StatusBadRequest, "InvalidJSON", "Invalid JSON", "The request body must be one valid UTF-8 JSON object.")
		return false
	}
	if !uniqueTopLevelSecretFields(raw) {
		writeProblem(w, r, http.StatusBadRequest, "InvalidJSON", "Invalid JSON", "Duplicate request fields are not allowed.")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(output); err != nil {
		if errors.Is(err, secrets.ErrInvalid) {
			mappedSecretError(w, r, err)
			return false
		}
		writeProblem(w, r, http.StatusBadRequest, "InvalidJSON", "Invalid JSON", "The request body is not valid for this operation.")
		return false
	}
	if err = decoder.Decode(&struct{}{}); err != io.EOF {
		writeProblem(w, r, http.StatusBadRequest, "InvalidJSON", "Invalid JSON", "The request must contain exactly one JSON object.")
		return false
	}
	return true
}

func uniqueTopLevelSecretFields(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return false
	}
	seen := map[string]struct{}{}
	for decoder.More() {
		keyToken, tokenErr := decoder.Token()
		key, ok := keyToken.(string)
		if tokenErr != nil || !ok {
			return false
		}
		if _, duplicate := seen[key]; duplicate {
			return false
		}
		seen[key] = struct{}{}
		var value json.RawMessage
		if err = decoder.Decode(&value); err != nil {
			clear(value)
			return false
		}
		clear(value)
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim('}') {
		return false
	}
	return decoder.Decode(&struct{}{}) == io.EOF
}

func secretIdempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	values := r.Header.Values("Idempotency-Key")
	if len(values) != 1 || !secretIdempotencyKeyRE.MatchString(values[0]) {
		writeProblem(w, r, http.StatusBadRequest, "IdempotencyKeyRequired", "Idempotency key required", "Provide an Idempotency-Key containing 16 to 128 safe ASCII characters.")
		return "", false
	}
	return values[0], true
}

func safeSecretRequestID(ctx context.Context) string {
	value := requestID(ctx)
	if secretRequestIDRE.MatchString(value) {
		return value
	}
	return id.New()
}

func secretBackendUnavailable(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, http.StatusServiceUnavailable, "SecretBindingsUnavailable", "Runtime secrets unavailable", "Runtime-secret providers and their controller are not configured for this installation.")
}

func runtimeSecretReferenceUnavailable(err error) bool {
	return errors.Is(err, secrets.ErrRuntimeUnavailable) || errors.Is(err, store.ErrForbidden) || errors.Is(err, store.ErrNotFound)
}

func runtimeSecretReferenceDiagnostic(err error) appconfig.Diagnostic {
	code := "RuntimeSecretReferenceUnresolved"
	detail := "A runtime-secret reference is unavailable, inactive, or not authorized for this exact application destination."
	if errors.Is(err, secrets.ErrRuntimeUnavailable) {
		code = "RuntimeSecretRuntimeUnavailable"
		detail = "Runtime-secret references require a fresh exact strict-Sealed worker and matching Git projection policy runtime."
	}
	return appconfig.Diagnostic{Code: code, Detail: detail, Pointer: "/spec/runtime/env"}
}

func mappedSecretError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, secrets.ErrNotFound):
		writeProblem(w, r, http.StatusNotFound, "NotFound", "Not found", "The requested resource was not found.")
	case errors.Is(err, store.ErrForbidden):
		writeProblem(w, r, http.StatusForbidden, "Forbidden", "Forbidden", "You do not have access to perform this action.")
	case errors.Is(err, secrets.ErrInvalid):
		writeProblem(w, r, http.StatusUnprocessableEntity, "ValidationFailed", "Validation failed", "The runtime-secret request is invalid.")
	case errors.Is(err, secrets.ErrReferenced):
		writeProblem(w, r, http.StatusConflict, "SecretBindingReferenced", "Runtime secret is referenced", "Remove every current Git, current release, and retained release reference before deletion.")
	case errors.Is(err, secrets.ErrConflict):
		writeProblem(w, r, http.StatusConflict, "SecretBindingConflict", "Runtime secret conflict", "The request conflicts with the current immutable runtime-secret state or idempotency record.")
	case errors.Is(err, secrets.ErrNotReady):
		writeProblem(w, r, http.StatusConflict, "SecretVersionNotReady", "Runtime secret is not ready", "The selected runtime-secret version has not passed provider readiness checks.")
	case errors.Is(err, secrets.ErrFingerprintKeyUnavailable):
		writeProblem(w, r, http.StatusServiceUnavailable, "SecretFingerprintKeyUnavailable", "Runtime secrets unavailable", "The runtime-secret fingerprint key is unavailable.")
	case errors.Is(err, secrets.ErrRuntimeUnavailable):
		secretBackendUnavailable(w, r)
	case errors.Is(err, secrets.ErrProviderOperation):
		writeProblem(w, r, http.StatusBadGateway, "SecretProviderFailed", "Secret provider failed", "The runtime-secret provider could not complete the operation.")
	case errors.Is(err, secrets.ErrProviderMismatch):
		writeProblem(w, r, http.StatusBadGateway, "SecretProviderMismatch", "Secret provider rejected", "The runtime-secret provider returned an invalid or mismatched observation.")
	default:
		mappedError(w, r, err)
	}
}

var _ RuntimeSecretBackend = (*runtimeSecretBackend)(nil)
