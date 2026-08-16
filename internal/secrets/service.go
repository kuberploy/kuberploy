package secrets

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/kuberploy/kuberploy/internal/id"
)

type Service struct {
	Store           Store
	Keys            FingerprintKeyProvider
	ExternalSecrets ExternalSecretsProvider
	SealedSecrets   SealedSecretsProvider
	Now             func() time.Time
}

type CreateRequest struct {
	ActorID          string
	Scope            Scope
	Name             string
	Provider         ProviderKind
	Deliveries       []Delivery
	IdempotencyKey   string
	RequestID        string
	Material         *Material
	TargetSecretType TargetSecretType
	Purpose          BindingPurpose
}

type RotateRequest struct {
	ActorID               string
	BindingID             string
	ExpectedActiveVersion int64
	Deliveries            []Delivery
	IdempotencyKey        string
	RequestID             string
	Material              *Material
	TargetSecretType      TargetSecretType
}

type MutationResult struct {
	Binding Binding `json:"binding"`
	Version Version `json:"version"`
	Replay  bool    `json:"replay"`
}

func (s Service) Create(ctx context.Context, request CreateRequest) (MutationResult, error) {
	if request.Material != nil {
		defer request.Material.Destroy()
	}
	request.TargetSecretType = normalizeTargetSecretType(request.TargetSecretType)
	request.Purpose = normalizeBindingPurpose(request.Purpose)
	if !validPurposeTarget(request.Purpose, request.Provider, request.TargetSecretType) {
		return MutationResult{}, ErrInvalid
	}
	now := s.now()
	deliveries, err := validateWriteRequest(request.ActorID, request.Scope, request.Name, request.Provider, request.Deliveries, request.IdempotencyKey, request.RequestID, request.Material)
	if err != nil || s.Store == nil || s.Keys == nil || !s.providerAvailable(request.Provider) {
		return MutationResult{}, ErrInvalid
	}
	key, err := s.Keys.ActiveKey(ctx)
	if err != nil {
		return MutationResult{}, ErrFingerprintKeyUnavailable
	}
	defer key.destroy()
	fingerprint, err := fingerprint(key, "create", request.Scope, request.Name, request.Provider, request.TargetSecretType, "", 0, deliveries, request.Material)
	if err != nil {
		return MutationResult{}, err
	}
	bindingID, versionID, eventID := id.New(), id.New(), id.New()
	binding := Binding{ID: bindingID, Scope: request.Scope, Name: request.Name, Provider: request.Provider, Purpose: request.Purpose,
		State: BindingProvisioning, CreatedBy: request.ActorID, CreatedAt: now, UpdatedAt: now}
	version := Version{ID: versionID, BindingID: bindingID, Number: 1, Provider: request.Provider, State: VersionStaging,
		Deliveries: deliveries, TargetSecretType: request.TargetSecretType, FingerprintKeyID: key.ID, ContentFingerprint: fingerprint,
		RequestFingerprint: fingerprint, CreatedAt: now, UpdatedAt: now}
	idempotency := Idempotency{ActorID: request.ActorID, Operation: "create", ApplicationID: request.Scope.ApplicationID, Key: request.IdempotencyKey,
		RequestFingerprint: fingerprint, BindingID: bindingID, VersionID: versionID, CreatedAt: now}
	event := Event{ID: eventID, BindingID: bindingID, VersionID: versionID, ActorID: request.ActorID, Kind: EventVersionStaging, RequestID: request.RequestID, OccurredAt: now}
	binding, version, replay, err := s.Store.BeginCreate(ctx, BeginCreate{Binding: binding, Version: version, Idempotency: idempotency, Event: event})
	if err != nil {
		return MutationResult{}, err
	}
	result := MutationResult{Binding: binding, Version: version, Replay: replay}
	if replay && version.State != VersionStaging {
		return result, nil
	}
	return s.stage(ctx, result, request.Material, request.ActorID, request.RequestID)
}

func (s Service) Rotate(ctx context.Context, request RotateRequest) (MutationResult, error) {
	if request.Material != nil {
		defer request.Material.Destroy()
	}
	request.TargetSecretType = normalizeTargetSecretType(request.TargetSecretType)
	if s.Store == nil || s.Keys == nil || !uuidRE.MatchString(request.ActorID) || !uuidRE.MatchString(request.BindingID) ||
		request.ExpectedActiveVersion <= 0 || !idempotencyRE.MatchString(request.IdempotencyKey) ||
		!requestIDRE.MatchString(request.RequestID) || request.Material == nil {
		return MutationResult{}, ErrInvalid
	}
	deliveries, err := normalizeDeliveries(request.Deliveries)
	if err != nil || validateMaterialDeliveries(request.Material, deliveries) != nil {
		return MutationResult{}, ErrInvalid
	}
	binding, err := s.Store.Binding(ctx, request.BindingID)
	if err != nil {
		return MutationResult{}, err
	}
	if binding.State != BindingReady || binding.ActiveVersion != request.ExpectedActiveVersion || !s.providerAvailable(binding.Provider) {
		return MutationResult{}, ErrConflict
	}
	if !validPurposeTarget(binding.Purpose, binding.Provider, request.TargetSecretType) {
		return MutationResult{}, ErrConflict
	}
	key, err := s.Keys.ActiveKey(ctx)
	if err != nil {
		return MutationResult{}, ErrFingerprintKeyUnavailable
	}
	defer key.destroy()
	fingerprint, err := fingerprint(key, "rotate", binding.Scope, binding.Name, binding.Provider, request.TargetSecretType, binding.ID, request.ExpectedActiveVersion, deliveries, request.Material)
	if err != nil {
		return MutationResult{}, err
	}
	now := s.now()
	versionID := id.New()
	version := Version{ID: versionID, BindingID: binding.ID, Provider: binding.Provider, State: VersionStaging, Deliveries: deliveries,
		TargetSecretType: request.TargetSecretType, FingerprintKeyID: key.ID, ContentFingerprint: fingerprint,
		RequestFingerprint: fingerprint, CreatedAt: now, UpdatedAt: now}
	idempotency := Idempotency{ActorID: request.ActorID, Operation: "rotate", ApplicationID: binding.Scope.ApplicationID, Key: request.IdempotencyKey,
		RequestFingerprint: fingerprint, BindingID: binding.ID, VersionID: versionID, CreatedAt: now}
	event := Event{ID: id.New(), BindingID: binding.ID, VersionID: versionID, ActorID: request.ActorID, Kind: EventVersionStaging, RequestID: request.RequestID, OccurredAt: now}
	binding, version, replay, err := s.Store.BeginRotation(ctx, BeginRotation{BindingID: binding.ID, ExpectedActiveVersion: request.ExpectedActiveVersion, Version: version, Idempotency: idempotency, Event: event})
	if err != nil {
		return MutationResult{}, err
	}
	result := MutationResult{Binding: binding, Version: version, Replay: replay}
	if replay && version.State != VersionStaging {
		return result, nil
	}
	return s.stage(ctx, result, request.Material, request.ActorID, request.RequestID)
}

func normalizeTargetSecretType(value TargetSecretType) TargetSecretType {
	if value == "" {
		return TargetSecretOpaque
	}
	return value
}

func normalizeBindingPurpose(value BindingPurpose) BindingPurpose {
	if value == "" {
		return PurposeRuntimeSecret
	}
	return value
}

func validPurposeTarget(purpose BindingPurpose, provider ProviderKind, targetType TargetSecretType) bool {
	return purpose == PurposeRuntimeSecret && targetType == TargetSecretOpaque ||
		purpose == PurposeTLSCertificate && provider == ProviderSealedSecrets && targetType == TargetSecretTLS
}

func (s Service) stage(ctx context.Context, result MutationResult, material *Material, actorID, requestID string) (MutationResult, error) {
	request := StageRequest{Binding: result.Binding, Version: result.Version, TargetSecretName: TargetSecretName(result.Binding, result.Version.Number),
		ExplicitKeys: deliveryKeys(result.Version.Deliveries), ImmutableTarget: true}
	if result.Binding.Provider == ProviderSealedSecrets {
		request.SealingScope = StrictSealingScope
	} else {
		request.ExternalRefreshPolicy = ExternalRefreshCreatedOnce
	}
	if request.validate() != nil {
		return MutationResult{}, ErrInvalid
	}
	var artifact Artifact
	var err error
	switch result.Binding.Provider {
	case ProviderExternalSecrets:
		artifact, err = s.ExternalSecrets.StageExternalSecret(ctx, request, material)
	case ProviderSealedSecrets:
		artifact, err = s.SealedSecrets.StageStrictSealedSecret(ctx, request, material)
	default:
		err = ErrInvalid
	}
	now := s.now()
	if err != nil || artifact.ValidateFor(result.Binding, result.Version.Number) != nil ||
		artifact.TargetSecretType != result.Version.TargetSecretType {
		failure := Event{ID: id.New(), BindingID: result.Binding.ID, VersionID: result.Version.ID, ActorID: actorID, Kind: EventVersionFailed, RequestID: requestID, OccurredAt: now}
		failed, failErr := s.Store.FailVersion(ctx, result.Version.ID, "provider-stage-failed", failure, now)
		if failErr == nil {
			result.Version = failed
			if result.Binding.State == BindingProvisioning {
				result.Binding.State, result.Binding.UpdatedAt = BindingFailed, now
			}
		}
		if err != nil {
			return result, ErrProviderOperation
		}
		return result, ErrProviderMismatch
	}
	event := Event{ID: id.New(), BindingID: result.Binding.ID, VersionID: result.Version.ID, ActorID: actorID, Kind: EventVersionAwaitingReadiness, RequestID: requestID, OccurredAt: now}
	version, err := s.Store.CompleteStage(ctx, result.Version.ID, artifact, event, now)
	if err != nil {
		return result, err
	}
	result.Version = version
	return result, nil
}

func (s Service) AddReference(ctx context.Context, actorID, requestID string, reference Reference) error {
	if s.Store == nil || !uuidRE.MatchString(actorID) || !requestIDRE.MatchString(requestID) {
		return ErrInvalid
	}
	reference.CreatedAt = s.now()
	event := Event{ID: id.New(), BindingID: reference.BindingID, VersionID: reference.VersionID, ActorID: actorID,
		Kind: EventReferenceAdded, RequestID: requestID, OccurredAt: reference.CreatedAt}
	return s.Store.AddReference(ctx, reference, event)
}

func (s Service) RemoveReference(ctx context.Context, actorID, bindingID string, kind ReferenceKind, referenceID, requestID string) error {
	if s.Store == nil || !uuidRE.MatchString(actorID) || !uuidRE.MatchString(bindingID) || !kind.valid() || !safeOpaque(referenceID, 256) || !requestIDRE.MatchString(requestID) {
		return ErrInvalid
	}
	references, err := s.Store.References(ctx, bindingID)
	if err != nil {
		return err
	}
	versionID := ""
	for _, reference := range references {
		if reference.Kind == kind && reference.Reference == referenceID {
			versionID = reference.VersionID
			break
		}
	}
	if versionID == "" {
		return ErrNotFound
	}
	event := Event{ID: id.New(), BindingID: bindingID, VersionID: versionID, ActorID: actorID,
		Kind: EventReferenceRemoved, RequestID: requestID, OccurredAt: s.now()}
	return s.Store.RemoveReference(ctx, bindingID, kind, referenceID, event)
}

func (s Service) ReconcileVersion(ctx context.Context, versionID, requestID string) (MutationResult, error) {
	if s.Store == nil || !uuidRE.MatchString(versionID) || !requestIDRE.MatchString(requestID) {
		return MutationResult{}, ErrInvalid
	}
	version, err := s.Store.Version(ctx, versionID)
	if err != nil {
		return MutationResult{}, err
	}
	binding, err := s.Store.Binding(ctx, version.BindingID)
	if err != nil {
		return MutationResult{}, err
	}
	result := MutationResult{Binding: binding, Version: version}
	if version.State == VersionActive || version.State == VersionRetained {
		return result, nil
	}
	if version.State != VersionAwaitingReadiness || version.Artifact == nil {
		return result, ErrNotReady
	}
	var observation ReadinessObservation
	switch version.Provider {
	case ProviderExternalSecrets:
		if s.ExternalSecrets == nil {
			return result, ErrInvalid
		}
		observation, err = s.ExternalSecrets.ObserveExternalSecret(ctx, *version.Artifact)
	case ProviderSealedSecrets:
		if s.SealedSecrets == nil {
			return result, ErrInvalid
		}
		observation, err = s.SealedSecrets.ObserveStrictSealedSecret(ctx, *version.Artifact)
	}
	if err != nil {
		return result, ErrProviderOperation
	}
	if observation.validate(*version.Artifact) != nil {
		return result, ErrProviderMismatch
	}
	// Provider timestamps are evidence metadata, not a database clock. Lifecycle
	// ordering uses the controller's clock so a skewed provider cannot move
	// durable timestamps backwards or arbitrarily into the future.
	observedAt := s.now()
	switch observation.Status {
	case ReadinessPending:
		return result, ErrNotReady
	case ReadinessFailed:
		event := Event{ID: id.New(), BindingID: binding.ID, VersionID: version.ID, Kind: EventVersionFailed, RequestID: requestID, OccurredAt: observedAt}
		failed, failErr := s.Store.FailVersion(ctx, version.ID, observation.FailureCode, event, observedAt)
		result.Version = failed
		return result, failErr
	case ReadinessReady:
		event := Event{ID: id.New(), BindingID: binding.ID, VersionID: version.ID, Kind: EventVersionActive, RequestID: requestID, OccurredAt: observedAt}
		binding, version, err = s.Store.ActivateVersion(ctx, version.ID, observedAt, event)
		return MutationResult{Binding: binding, Version: version}, err
	default:
		return result, ErrProviderMismatch
	}
}

func (s Service) Delete(ctx context.Context, actorID, bindingID, requestID string) (Binding, error) {
	keyDigest := sha256.Sum256([]byte("legacy-delete\x00" + actorID + "\x00" + bindingID + "\x00" + requestID))
	return s.DeleteWithIdempotency(ctx, actorID, bindingID, "legacy-delete-"+hex.EncodeToString(keyDigest[:]), requestID)
}

func (s Service) DeleteWithIdempotency(ctx context.Context, actorID, bindingID, idempotencyKey, requestID string) (Binding, error) {
	if s.Store == nil || !uuidRE.MatchString(actorID) || !uuidRE.MatchString(bindingID) ||
		!idempotencyRE.MatchString(idempotencyKey) || !requestIDRE.MatchString(requestID) {
		return Binding{}, ErrInvalid
	}
	binding, err := s.Store.Binding(ctx, bindingID)
	if err != nil {
		return Binding{}, err
	}
	versions, err := s.Store.Versions(ctx, bindingID)
	if err != nil {
		return Binding{}, err
	}
	if len(versions) == 0 || versions[0].BindingID != bindingID {
		return Binding{}, ErrConflict
	}
	now := s.now()
	start := Event{ID: id.New(), BindingID: bindingID, ActorID: actorID, Kind: EventBindingDeleting, RequestID: requestID, OccurredAt: now}
	requestFingerprint := sha256.Sum256([]byte(fmt.Sprintf("delete\x00%s\x00%s\x00%s\x00%s", actorID, binding.Scope.ApplicationID, binding.ID, binding.Purpose)))
	prepared, preparedVersions, replay, started, err := s.Store.PrepareDelete(ctx, DeleteCommand{ActorID: actorID, BindingID: bindingID,
		Idempotency: Idempotency{ActorID: actorID, Operation: "delete", ApplicationID: binding.Scope.ApplicationID, Key: idempotencyKey,
			RequestFingerprint: requestFingerprint, BindingID: bindingID, VersionID: versions[0].ID, CreatedAt: now}, Event: start, Now: now})
	if err != nil {
		return Binding{}, err
	}
	binding, versions = prepared, preparedVersions
	// A different delete request must not run provider cleanup concurrently
	// with the request that owns the deleting transition. Matching retries are
	// allowed to resume cleanup after the original process lost its response.
	if binding.State == BindingDeleting && !replay && !started {
		return binding, ErrConflict
	}
	if binding.State == BindingDeleted {
		return binding, nil
	}
	for _, version := range versions {
		if version.Artifact == nil {
			continue
		}
		var observation DeleteObservation
		switch version.Provider {
		case ProviderExternalSecrets:
			if s.ExternalSecrets == nil {
				return binding, ErrInvalid
			}
			observation, err = s.ExternalSecrets.DeleteExternalSecret(ctx, *version.Artifact)
		case ProviderSealedSecrets:
			if s.SealedSecrets == nil {
				return binding, ErrInvalid
			}
			observation, err = s.SealedSecrets.DeleteStrictSealedSecret(ctx, *version.Artifact)
		}
		if err != nil {
			return binding, ErrProviderOperation
		}
		if observation.validate(*version.Artifact) != nil {
			return binding, ErrProviderMismatch
		}
	}
	completedAt := s.now()
	finish := Event{ID: id.New(), BindingID: bindingID, ActorID: actorID, Kind: EventBindingDeleted, RequestID: requestID, OccurredAt: completedAt}
	return s.Store.CompleteDelete(ctx, bindingID, finish, completedAt)
}

func (s Service) providerAvailable(provider ProviderKind) bool {
	return provider == ProviderExternalSecrets && s.ExternalSecrets != nil || provider == ProviderSealedSecrets && s.SealedSecrets != nil
}

func (s Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func validateWriteRequest(actorID string, scope Scope, name string, provider ProviderKind, input []Delivery, idem, requestID string, material *Material) ([]Delivery, error) {
	if !uuidRE.MatchString(actorID) || scope.Validate() != nil || !dnsLabelRE.MatchString(name) || !provider.valid() ||
		!idempotencyRE.MatchString(idem) || !requestIDRE.MatchString(requestID) || material == nil {
		return nil, ErrInvalid
	}
	deliveries, err := normalizeDeliveries(input)
	if err != nil || validateMaterialDeliveries(material, deliveries) != nil {
		return nil, ErrInvalid
	}
	return deliveries, nil
}

func validateMaterialDeliveries(material *Material, deliveries []Delivery) error {
	keys, err := material.keys()
	if err != nil {
		return err
	}
	used := make(map[string]struct{}, len(keys))
	available := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		available[key] = struct{}{}
	}
	for _, delivery := range deliveries {
		if _, ok := available[delivery.SourceKey]; !ok {
			return ErrInvalid
		}
		used[delivery.SourceKey] = struct{}{}
	}
	if len(used) != len(available) {
		return ErrInvalid
	}
	return nil
}

func sameFingerprint(left, right [32]byte) bool {
	return subtle.ConstantTimeCompare(left[:], right[:]) == 1
}
