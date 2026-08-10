package certificates

import (
	"context"
	"time"

	"github.com/kuberploy/kuberploy/internal/secrets"
)

const certificateFileRoot = "/var/run/secrets/kuberploy/certificates/"

type SecretLifecycle interface {
	Create(context.Context, secrets.CreateRequest) (secrets.MutationResult, error)
	Rotate(context.Context, secrets.RotateRequest) (secrets.MutationResult, error)
	Delete(context.Context, string, string, string) (secrets.Binding, error)
}

type SecretCatalog interface {
	Binding(context.Context, string) (secrets.Binding, error)
	Versions(context.Context, string) ([]secrets.Version, error)
}

type Service struct {
	Secrets SecretLifecycle
	Catalog SecretCatalog
	Store   Store
	Now     func() time.Time
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
	Binding     secrets.Binding `json:"binding"`
	Version     secrets.Version `json:"version"`
	Certificate Version         `json:"certificate"`
	Replay      bool            `json:"replay"`
}

func (s Service) Create(ctx context.Context, request CreateRequest) (MutationResult, error) {
	if request.Material != nil {
		defer request.Material.Destroy()
	}
	if s.Secrets == nil || s.Store == nil || request.Material == nil || !safeText(request.IdempotencyKey, 128) || !safeText(request.RequestID, 128) {
		return MutationResult{}, ErrInvalid
	}
	now := s.now()
	parsed, err := parseAndValidate(request.Material, now)
	if err != nil {
		return MutationResult{}, err
	}
	secretMaterial, err := certificateSecretMaterial(request.Material)
	if err != nil {
		return MutationResult{}, err
	}
	result, err := s.Secrets.Create(ctx, secrets.CreateRequest{
		ActorID: request.ActorID, Scope: request.Scope, Name: request.Name, Provider: secrets.ProviderSealedSecrets,
		Purpose: secrets.PurposeTLSCertificate, TargetSecretType: secrets.TargetSecretTLS, Deliveries: certificateDeliveries(),
		IdempotencyKey: request.IdempotencyKey, RequestID: request.RequestID, Material: secretMaterial,
	})
	if err != nil {
		return MutationResult{}, mapSecretError(err)
	}
	return s.record(ctx, result, parsed, request.ActorID)
}

func (s Service) Rotate(ctx context.Context, request RotateRequest) (MutationResult, error) {
	if request.Material != nil {
		defer request.Material.Destroy()
	}
	if s.Secrets == nil || s.Store == nil || request.Material == nil || !uuidRE.MatchString(request.BindingID) ||
		request.ExpectedActiveVersion <= 0 || !safeText(request.IdempotencyKey, 128) || !safeText(request.RequestID, 128) {
		return MutationResult{}, ErrInvalid
	}
	now := s.now()
	parsed, err := parseAndValidate(request.Material, now)
	if err != nil {
		return MutationResult{}, err
	}
	secretMaterial, err := certificateSecretMaterial(request.Material)
	if err != nil {
		return MutationResult{}, err
	}
	result, err := s.Secrets.Rotate(ctx, secrets.RotateRequest{
		ActorID: request.ActorID, BindingID: request.BindingID, ExpectedActiveVersion: request.ExpectedActiveVersion,
		TargetSecretType: secrets.TargetSecretTLS, Deliveries: certificateDeliveries(), IdempotencyKey: request.IdempotencyKey,
		RequestID: request.RequestID, Material: secretMaterial,
	})
	if err != nil {
		return MutationResult{}, mapSecretError(err)
	}
	return s.record(ctx, result, parsed, request.ActorID)
}

func (s Service) record(ctx context.Context, secretResult secrets.MutationResult, parsed parsedCertificate, actorID string) (MutationResult, error) {
	value := Version{
		BindingID: secretResult.Binding.ID, SecretVersionID: secretResult.Version.ID, Number: secretResult.Version.Number,
		LeafFingerprint: parsed.LeafFingerprint, PublicKeyFingerprint: parsed.PublicKeyFingerprint,
		DNSNames: parsed.DNSNames, IPAddresses: parsed.IPAddresses, NotBefore: parsed.NotBefore, NotAfter: parsed.NotAfter,
		CreatedBy: actorID, CreatedAt: secretResult.Version.CreatedAt.UTC(), SecretContentFingerprint: secretResult.Version.ContentFingerprint,
	}
	stored, _, err := s.Store.Record(ctx, value, secretResult.Binding, secretResult.Version)
	if err != nil {
		return MutationResult{}, err
	}
	return MutationResult{
		Binding: secretResult.Binding, Version: secretResult.Version, Certificate: stored, Replay: secretResult.Replay,
	}, nil
}

func (s Service) Delete(ctx context.Context, actorID, bindingID, requestID string) (secrets.Binding, error) {
	if s.Secrets == nil || s.Catalog == nil || s.Store == nil || !uuidRE.MatchString(actorID) || !uuidRE.MatchString(bindingID) || !safeText(requestID, 128) {
		return secrets.Binding{}, ErrInvalid
	}
	binding, err := s.Catalog.Binding(ctx, bindingID)
	if err != nil {
		return secrets.Binding{}, mapSecretError(err)
	}
	if binding.Purpose != secrets.PurposeTLSCertificate {
		return secrets.Binding{}, ErrNotFound
	}
	deleted, err := s.Secrets.Delete(ctx, actorID, bindingID, requestID)
	if err != nil {
		return secrets.Binding{}, mapSecretError(err)
	}
	return deleted, nil
}

func (s Service) Binding(ctx context.Context, bindingID string) (secrets.Binding, []Version, error) {
	if s.Catalog == nil || s.Store == nil || !uuidRE.MatchString(bindingID) {
		return secrets.Binding{}, nil, ErrInvalid
	}
	binding, err := s.Catalog.Binding(ctx, bindingID)
	if err != nil {
		return secrets.Binding{}, nil, mapSecretError(err)
	}
	if binding.Purpose != secrets.PurposeTLSCertificate {
		return secrets.Binding{}, nil, ErrNotFound
	}
	versions, err := s.Store.Versions(ctx, bindingID)
	return binding, versions, err
}

func certificateSecretMaterial(material *Material) (*secrets.Material, error) {
	if material == nil || material.destroyed {
		return nil, ErrMaterialGone
	}
	value, err := secrets.NewMaterial(map[string][]byte{
		"tls.crt": material.certificatePEM,
		"tls.key": material.privateKeyPEM,
	})
	if err != nil {
		return nil, ErrInvalid
	}
	return value, nil
}

func certificateDeliveries() []secrets.Delivery {
	return []secrets.Delivery{
		{SourceKey: "tls.crt", Kind: secrets.DeliveryFile, FilePath: certificateFileRoot + "tls.crt", FileMode: 0o400},
		{SourceKey: "tls.key", Kind: secrets.DeliveryFile, FilePath: certificateFileRoot + "tls.key", FileMode: 0o400},
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

func (s Service) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

var _ SecretLifecycle = (*secrets.Service)(nil)
