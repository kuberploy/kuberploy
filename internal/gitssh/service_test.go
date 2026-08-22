package gitssh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

type recordingEncryption struct {
	calls     int
	plaintext [][]byte
}

func (e *recordingEncryption) Encrypt(_ context.Context, plaintext []byte) (PrivateKeyEnvelope, error) {
	e.calls++
	e.plaintext = append(e.plaintext, append([]byte(nil), plaintext...))
	return PrivateKeyEnvelope{KeyVersion: "test-v1", Ciphertext: append([]byte("encrypted:"), plaintext[:8]...)}, nil
}

func (e *recordingEncryption) Decrypt(_ context.Context, envelope PrivateKeyEnvelope) ([]byte, error) {
	if len(e.plaintext) == 0 || envelope.KeyVersion != "test-v1" {
		return nil, ErrInvalidEnvelope
	}
	return append([]byte(nil), e.plaintext[len(e.plaintext)-1]...), nil
}

func newTestService(t *testing.T) (*Service, *MemoryRepository, *recordingEncryption) {
	t.Helper()
	repository := NewMemoryRepository()
	encryption := &recordingEncryption{}
	service, err := NewService(repository, encryption)
	if err != nil {
		t.Fatal(err)
	}
	return service, repository, encryption
}

func TestCreateGeneratesUniqueEd25519Keys(t *testing.T) {
	service, _, _ := newTestService(t)
	first, err := service.Create(context.Background(), CreateRequest{Scope: ScopeApp, OwnerID: "app-1"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Create(context.Background(), CreateRequest{Scope: ScopeApp, OwnerID: "app-2"})
	if err != nil {
		t.Fatal(err)
	}
	if first.PublicKey == second.PublicKey || first.Fingerprint == second.Fingerprint {
		t.Fatal("independent keys must be unique")
	}
	if !strings.HasPrefix(first.PublicKey, "ssh-ed25519 ") {
		t.Fatalf("public key = %q", first.PublicKey)
	}
	if first.Revision != 1 || first.Status != StatusActive {
		t.Fatalf("metadata = %#v", first)
	}
}

func TestScopeAndOwnerValidation(t *testing.T) {
	service, _, _ := newTestService(t)
	tests := []CreateRequest{
		{Scope: "team", OwnerID: "owner-1"},
		{Scope: ScopeApp, OwnerID: ""},
		{Scope: ScopeProject, OwnerID: "  "},
	}
	for _, test := range tests {
		if _, err := service.Create(context.Background(), test); err == nil {
			t.Fatalf("Create(%#v) succeeded", test)
		}
	}
	for _, scope := range []Scope{ScopeApp, ScopeProject} {
		if _, err := service.Create(context.Background(), CreateRequest{Scope: scope, OwnerID: string(scope) + "-1"}); err != nil {
			t.Fatalf("Create(%s): %v", scope, err)
		}
	}
}

func TestRotateRevokesOldRevision(t *testing.T) {
	service, _, _ := newTestService(t)
	first, err := service.Create(context.Background(), CreateRequest{Scope: ScopeProject, OwnerID: "project-1"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Rotate(context.Background(), ScopeProject, "project-1")
	if err != nil {
		t.Fatal(err)
	}
	if second.Revision != 2 || second.Status != StatusActive || second.Fingerprint == first.Fingerprint {
		t.Fatalf("rotated metadata = %#v", second)
	}
	all, err := service.List(context.Background(), ScopeProject, "project-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Status != StatusRevoked || all[1].Status != StatusActive {
		t.Fatalf("revisions = %#v", all)
	}
}

func TestRevoke(t *testing.T) {
	service, _, _ := newTestService(t)
	if _, err := service.Create(context.Background(), CreateRequest{Scope: ScopeApp, OwnerID: "app-1"}); err != nil {
		t.Fatal(err)
	}
	revoked, err := service.Revoke(context.Background(), ScopeApp, "app-1")
	if err != nil {
		t.Fatal(err)
	}
	if revoked.Revision != 1 || revoked.Status != StatusRevoked {
		t.Fatalf("revoked metadata = %#v", revoked)
	}
	if _, err := service.Active(context.Background(), ScopeApp, "app-1"); !errors.Is(err, ErrActiveKeyNotFound) {
		t.Fatalf("Active error = %v", err)
	}
	if _, err := service.Revoke(context.Background(), ScopeApp, "app-1"); !errors.Is(err, ErrActiveKeyNotFound) {
		t.Fatalf("second Revoke error = %v", err)
	}
}

func TestEncryptionInvokedAndPrivateMaterialNotExposed(t *testing.T) {
	service, repository, encryption := newTestService(t)
	metadata, err := service.Create(context.Background(), CreateRequest{Scope: ScopeApp, OwnerID: "app-1"})
	if err != nil {
		t.Fatal(err)
	}
	if encryption.calls != 1 || len(encryption.plaintext) != 1 {
		t.Fatalf("encryption calls = %d", encryption.calls)
	}
	if _, err := ssh.ParseRawPrivateKey(encryption.plaintext[0]); err != nil {
		t.Fatalf("encryption input is not OpenSSH PEM: %v", err)
	}

	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(string(encoded))
	if strings.Contains(lower, "private") || strings.Contains(lower, "ciphertext") || bytes.Contains(encoded, encryption.plaintext[0]) {
		t.Fatalf("public metadata exposed private material: %s", encoded)
	}

	repository.mu.RLock()
	stored := repository.records[repositoryKey(ScopeApp, "app-1")][0]
	repository.mu.RUnlock()
	if stored.envelope.KeyVersion != "test-v1" || !bytes.HasPrefix(stored.envelope.Ciphertext, []byte("encrypted:")) {
		t.Fatalf("stored envelope = %#v", stored.envelope)
	}
	if bytes.Equal(stored.envelope.Ciphertext, encryption.plaintext[0]) {
		t.Fatal("repository stored plaintext private key")
	}
}

func TestPrivateKeyDecryptsOnlyForInternalCheckout(t *testing.T) {
	service, _, _ := newTestService(t)
	if _, err := service.Create(context.Background(), CreateRequest{Scope: ScopeApp, OwnerID: "app-1"}); err != nil {
		t.Fatal(err)
	}
	privateKey, err := service.PrivateKey(context.Background(), ScopeApp, "app-1")
	if err != nil {
		t.Fatal(err)
	}
	defer zero(privateKey)
	if _, err = ssh.ParseRawPrivateKey(privateKey); err != nil {
		t.Fatalf("decrypted private key is not OpenSSH PEM: %v", err)
	}
}
