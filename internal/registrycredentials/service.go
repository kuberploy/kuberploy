package registrycredentials

import (
	"context"

	"github.com/kuberploy/kuberploy/internal/secrets"
)

const registryCredentialFileRoot = "/var/run/secrets/kuberploy/registry-credentials/"

type SecretLifecycle interface {
	Create(context.Context, secrets.CreateRequest) (secrets.MutationResult, error)
	Rotate(context.Context, secrets.RotateRequest) (secrets.MutationResult, error)
	Delete(context.Context, string, string, string) (secrets.Binding, error)
	DeleteWithIdempotency(context.Context, string, string, string, string) (secrets.Binding, error)
}

type SecretCatalog interface {
	Binding(context.Context, string) (secrets.Binding, error)
	Versions(context.Context, string) ([]secrets.Version, error)
}

type Service struct {
	Secrets SecretLifecycle
	Catalog SecretCatalog
	Store   Store
}

type CreateRequest struct {
	ActorID        string
	Scope          secrets.Scope
	Name           string
	IdempotencyKey string
	RequestID      string
	Material       *Material
}

type RotateRequest struct {
	ActorID               string
	BindingID             string
	ExpectedActiveVersion int64
	IdempotencyKey        string
	RequestID             string
	Material              *Material
}

type MutationResult struct {
	Binding    secrets.Binding `json:"binding"`
	Version    secrets.Version `json:"version"`
	Credential Version         `json:"credential"`
	Replay     bool            `json:"replay"`
}

func (s Service) Create(ctx context.Context, request CreateRequest) (MutationResult, error) {
	if request.Material != nil {
		defer request.Material.Destroy()
	}
	if s.Secrets == nil || s.Store == nil || request.Material == nil || !safeText(request.IdempotencyKey, 128) || !safeText(request.RequestID, 128) {
		return MutationResult{}, ErrInvalid
	}
	if request.Material.destroyed {
		return MutationResult{}, ErrMaterialGone
	}
	host, username := string(request.Material.host), string(request.Material.username)
	secretMaterial, err := registryCredentialSecretMaterial(request.Material)
	if err != nil {
		return MutationResult{}, err
	}
	result, err := s.Secrets.Create(ctx, secrets.CreateRequest{
		ActorID: request.ActorID, Scope: request.Scope, Name: request.Name, Provider: secrets.ProviderSealedSecrets,
		Purpose: secrets.PurposeRegistryPullCredential, TargetSecretType: secrets.TargetSecretDockerConfigJSON,
		Deliveries: registryCredentialDeliveries(), IdempotencyKey: request.IdempotencyKey, RequestID: request.RequestID,
		Material: secretMaterial,
	})
	if err != nil {
		return MutationResult{}, mapSecretError(err)
	}
	return s.record(ctx, result, host, username, request.ActorID)
}

func (s Service) Rotate(ctx context.Context, request RotateRequest) (MutationResult, error) {
	if request.Material != nil {
		defer request.Material.Destroy()
	}
	if s.Secrets == nil || s.Store == nil || request.Material == nil || !uuidRE.MatchString(request.BindingID) ||
		request.ExpectedActiveVersion <= 0 || !safeText(request.IdempotencyKey, 128) || !safeText(request.RequestID, 128) {
		return MutationResult{}, ErrInvalid
	}
	if request.Material.destroyed {
		return MutationResult{}, ErrMaterialGone
	}
	host, username := string(request.Material.host), string(request.Material.username)
	secretMaterial, err := registryCredentialSecretMaterial(request.Material)
	if err != nil {
		return MutationResult{}, err
	}
	result, err := s.Secrets.Rotate(ctx, secrets.RotateRequest{
		ActorID: request.ActorID, BindingID: request.BindingID, ExpectedActiveVersion: request.ExpectedActiveVersion,
		TargetSecretType: secrets.TargetSecretDockerConfigJSON, Deliveries: registryCredentialDeliveries(),
		IdempotencyKey: request.IdempotencyKey, RequestID: request.RequestID, Material: secretMaterial,
	})
	if err != nil {
		return MutationResult{}, mapSecretError(err)
	}
	return s.record(ctx, result, host, username, request.ActorID)
}

func (s Service) Delete(ctx context.Context, actorID, bindingID, requestID string) (secrets.Binding, error) {
	return s.delete(ctx, actorID, bindingID, "", requestID)
}

func (s Service) DeleteWithIdempotency(ctx context.Context, actorID, bindingID, idempotencyKey, requestID string) (secrets.Binding, error) {
	return s.delete(ctx, actorID, bindingID, idempotencyKey, requestID)
}

func (s Service) delete(ctx context.Context, actorID, bindingID, idempotencyKey, requestID string) (secrets.Binding, error) {
	if s.Secrets == nil || s.Catalog == nil || s.Store == nil || !uuidRE.MatchString(actorID) || !uuidRE.MatchString(bindingID) || !safeText(requestID, 128) {
		return secrets.Binding{}, ErrInvalid
	}
	binding, err := s.Catalog.Binding(ctx, bindingID)
	if err != nil {
		return secrets.Binding{}, mapSecretError(err)
	}
	if binding.Purpose != secrets.PurposeRegistryPullCredential {
		return secrets.Binding{}, ErrNotFound
	}
	var deleted secrets.Binding
	if idempotencyKey == "" {
		deleted, err = s.Secrets.Delete(ctx, actorID, bindingID, requestID)
	} else {
		deleted, err = s.Secrets.DeleteWithIdempotency(ctx, actorID, bindingID, idempotencyKey, requestID)
	}
	if err != nil {
		return secrets.Binding{}, mapSecretError(err)
	}
	return deleted, nil
}

func (s Service) record(ctx context.Context, secretResult secrets.MutationResult, host, username, actorID string) (MutationResult, error) {
	value := Version{
		BindingID: secretResult.Binding.ID, SecretVersionID: secretResult.Version.ID, Number: secretResult.Version.Number,
		Host: host, Username: username, CreatedBy: actorID, CreatedAt: secretResult.Version.CreatedAt.UTC(),
		SecretContentFingerprint: secretResult.Version.ContentFingerprint,
	}
	stored, _, err := s.Store.Record(ctx, value, secretResult.Binding, secretResult.Version)
	if err != nil {
		return MutationResult{}, err
	}
	return MutationResult{Binding: secretResult.Binding, Version: secretResult.Version, Credential: stored, Replay: secretResult.Replay}, nil
}

func (s Service) Binding(ctx context.Context, bindingID string) (secrets.Binding, []Version, error) {
	if s.Catalog == nil || s.Store == nil || !uuidRE.MatchString(bindingID) {
		return secrets.Binding{}, nil, ErrInvalid
	}
	binding, err := s.Catalog.Binding(ctx, bindingID)
	if err != nil {
		return secrets.Binding{}, nil, mapSecretError(err)
	}
	if binding.Purpose != secrets.PurposeRegistryPullCredential {
		return secrets.Binding{}, nil, ErrNotFound
	}
	versions, err := s.Store.Versions(ctx, bindingID)
	return binding, versions, err
}

func registryCredentialSecretMaterial(material *Material) (*secrets.Material, error) {
	if material == nil || material.destroyed {
		return nil, ErrMaterialGone
	}
	config, err := material.dockerConfigJSON()
	if err != nil {
		return nil, err
	}
	defer clear(config)
	value, err := secrets.NewMaterial(map[string][]byte{dockerConfigJSONKey: config})
	if err != nil {
		return nil, ErrInvalid
	}
	return value, nil
}

// registryCredentialDeliveries mirrors the certificate wrapper's own
// synthetic file-delivery bookkeeping: this key is never actually mounted
// into an App's own container (the produced Secret is referenced only via a
// Pod's imagePullSecrets, resolved at deployment materialization, exactly
// like a custom TLS certificate is referenced only via a route's tls
// secretRef and never mounted into the App it protects). A Delivery entry
// exists here purely because every material key must have exactly one, per
// the runtime-secret lifecycle's own invariant.
func registryCredentialDeliveries() []secrets.Delivery {
	return []secrets.Delivery{
		{SourceKey: dockerConfigJSONKey, Kind: secrets.DeliveryFile, FilePath: registryCredentialFileRoot + "dockerconfigjson", FileMode: 0o400},
	}
}

func mapSecretError(err error) error {
	switch err {
	case secrets.ErrInvalid:
		return ErrInvalid
	case secrets.ErrNotFound:
		return ErrNotFound
	case secrets.ErrConflict, secrets.ErrReferenced, secrets.ErrNotReady:
		return ErrConflict
	default:
		return ErrUnavailable
	}
}

var _ SecretLifecycle = (*secrets.Service)(nil)
