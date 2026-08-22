package gitssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"strings"

	"golang.org/x/crypto/ssh"
)

type Service struct {
	repository repository
	encryption KeyEncryption
}

type repository interface {
	create(context.Context, keyRecord) (KeyMetadata, error)
	rotate(context.Context, keyRecord) (KeyMetadata, error)
	revoke(context.Context, Scope, string) (KeyMetadata, error)
	active(context.Context, Scope, string) (keyRecord, error)
	List(context.Context, Scope, string) ([]KeyMetadata, error)
}

type idempotentRepository interface {
	mutateIdempotent(context.Context, MutationRequest, *keyRecord) (MutationResult, error)
}

func NewService(repository repository, encryption KeyEncryption) (*Service, error) {
	if repository == nil {
		return nil, errors.New("Git SSH key repository is required")
	}
	if encryption == nil {
		return nil, errors.New("Git SSH key encryption is required")
	}
	return &Service{repository: repository, encryption: encryption}, nil
}

func (s *Service) Create(ctx context.Context, request CreateRequest) (KeyMetadata, error) {
	if err := validateIdentity(request.Scope, request.OwnerID); err != nil {
		return KeyMetadata{}, err
	}
	record, err := s.generateRecord(ctx, request.Scope, strings.TrimSpace(request.OwnerID))
	if err != nil {
		return KeyMetadata{}, err
	}
	return s.repository.create(ctx, record)
}

func (s *Service) Rotate(ctx context.Context, scope Scope, ownerID string) (KeyMetadata, error) {
	if err := validateIdentity(scope, ownerID); err != nil {
		return KeyMetadata{}, err
	}
	record, err := s.generateRecord(ctx, scope, strings.TrimSpace(ownerID))
	if err != nil {
		return KeyMetadata{}, err
	}
	return s.repository.rotate(ctx, record)
}

func (s *Service) Revoke(ctx context.Context, scope Scope, ownerID string) (KeyMetadata, error) {
	if err := validateIdentity(scope, ownerID); err != nil {
		return KeyMetadata{}, err
	}
	return s.repository.revoke(ctx, scope, strings.TrimSpace(ownerID))
}

func (s *Service) Active(ctx context.Context, scope Scope, ownerID string) (KeyMetadata, error) {
	record, err := s.repository.active(ctx, scope, strings.TrimSpace(ownerID))
	return record.metadata, err
}

func (s *Service) List(ctx context.Context, scope Scope, ownerID string) ([]KeyMetadata, error) {
	return s.repository.List(ctx, scope, strings.TrimSpace(ownerID))
}

func (s *Service) Mutate(ctx context.Context, request MutationRequest) (MutationResult, error) {
	request.ActorID = strings.TrimSpace(request.ActorID)
	request.OwnerID = strings.TrimSpace(request.OwnerID)
	if err := request.validate(); err != nil {
		return MutationResult{}, err
	}
	persistent, ok := s.repository.(idempotentRepository)
	if !ok {
		return MutationResult{}, errors.New("Git SSH repository does not support durable idempotency")
	}
	var record *keyRecord
	if request.Operation == OperationCreate || request.Operation == OperationRotate {
		generated, err := s.generateRecord(ctx, request.Scope, request.OwnerID)
		if err != nil {
			return MutationResult{}, err
		}
		record = &generated
	}
	return persistent.mutateIdempotent(ctx, request, record)
}

// PrivateKey returns PKCS#8 private-key bytes only to the internal checkout
// boundary. HTTP lifecycle handlers expose public metadata and never call it.
func (s *Service) PrivateKey(ctx context.Context, scope Scope, ownerID string) ([]byte, error) {
	record, err := s.repository.active(ctx, scope, strings.TrimSpace(ownerID))
	if err != nil {
		return nil, err
	}
	return s.encryption.Decrypt(ctx, record.envelope)
}

func (s *Service) generateRecord(ctx context.Context, scope Scope, ownerID string) (keyRecord, error) {
	publicRaw, privateRaw, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return keyRecord{}, err
	}
	defer zero(privateRaw)

	privateBlock, err := ssh.MarshalPrivateKey(privateRaw, "")
	if err != nil {
		return keyRecord{}, err
	}
	privatePEM := pem.EncodeToMemory(privateBlock)
	if len(privatePEM) == 0 {
		return keyRecord{}, errors.New("Git SSH private key serialization failed")
	}
	defer zero(privatePEM)

	envelope, err := s.encryption.Encrypt(ctx, privatePEM)
	if err != nil {
		return keyRecord{}, err
	}
	if err := envelope.validate(); err != nil {
		return keyRecord{}, err
	}

	publicSSH, err := ssh.NewPublicKey(publicRaw)
	if err != nil {
		return keyRecord{}, err
	}
	return keyRecord{
		metadata: KeyMetadata{
			Scope:       scope,
			OwnerID:     ownerID,
			PublicKey:   strings.TrimSpace(string(ssh.MarshalAuthorizedKey(publicSSH))),
			Fingerprint: ssh.FingerprintSHA256(publicSSH),
		},
		envelope: cloneEnvelope(envelope),
	}, nil
}

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
