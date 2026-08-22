package gitssh

import (
	"bytes"
	"context"
	"testing"
)

func TestAESGCMEncryptionRoundTripAndTamperRejection(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, AES256KeyBytes)
	encryption, err := NewAESGCMEncryption("git-ssh-v1", key)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("private PKCS8 material")
	envelope, err := encryption.Encrypt(context.Background(), plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(envelope.Ciphertext, plaintext) {
		t.Fatal("ciphertext contains plaintext")
	}
	decrypted, err := encryption.Decrypt(context.Background(), envelope)
	if err != nil {
		t.Fatal(err)
	}
	defer zero(decrypted)
	if !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("decrypted = %q", decrypted)
	}

	tampered := cloneEnvelope(envelope)
	tampered.Ciphertext[len(tampered.Ciphertext)-1] ^= 1
	if _, err = encryption.Decrypt(context.Background(), tampered); err != ErrInvalidEnvelope {
		t.Fatalf("tampered decrypt error = %v", err)
	}
	tampered = cloneEnvelope(envelope)
	tampered.KeyVersion = "git-ssh-v2"
	if _, err = encryption.Decrypt(context.Background(), tampered); err != ErrInvalidEnvelope {
		t.Fatalf("wrong-version decrypt error = %v", err)
	}
}

func TestAESGCMEncryptionRejectsInvalidKey(t *testing.T) {
	if _, err := NewAESGCMEncryption("git-ssh-v1", []byte("short")); err == nil {
		t.Fatal("short key accepted")
	}
}
