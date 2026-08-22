package gitssh

import "testing"

func TestEncryptionFromEnvironment(t *testing.T) {
	t.Setenv(EncryptionSecretEnv, "")
	if value, err := EncryptionFromEnvironment(); err != nil || value != nil {
		t.Fatalf("disabled encryption = %#v, %v", value, err)
	}
	t.Setenv(EncryptionSecretEnv, "short")
	if _, err := EncryptionFromEnvironment(); err == nil {
		t.Fatal("short encryption secret accepted")
	}
	t.Setenv(EncryptionSecretEnv, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	t.Setenv(EncryptionKeyVersionEnv, "key-2026-08")
	value, err := EncryptionFromEnvironment()
	if err != nil || value == nil || value.keyVersion != "key-2026-08" {
		t.Fatalf("encryption = %#v, %v", value, err)
	}
}
