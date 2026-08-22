package gitssh

import (
	"crypto/sha256"
	"errors"
	"os"
	"strings"
)

const (
	EncryptionSecretEnv     = "KUBERPLOY_GIT_SSH_ENCRYPTION_KEY"
	EncryptionKeyVersionEnv = "KUBERPLOY_GIT_SSH_ENCRYPTION_KEY_VERSION"
	DefaultKeyVersion       = "v1"
)

func EncryptionFromEnvironment() (*AESGCMEncryption, error) {
	secret := os.Getenv(EncryptionSecretEnv)
	if secret == "" {
		return nil, nil
	}
	if len(secret) < 32 || len(secret) > 4096 || strings.ContainsRune(secret, '\x00') {
		return nil, errors.New(EncryptionSecretEnv + " must contain 32 to 4096 non-NUL bytes")
	}
	version := strings.TrimSpace(os.Getenv(EncryptionKeyVersionEnv))
	if version == "" {
		version = DefaultKeyVersion
	}
	if len(version) > 64 || strings.ContainsAny(version, "\x00\r\n") {
		return nil, errors.New(EncryptionKeyVersionEnv + " is invalid")
	}
	key := sha256.Sum256([]byte(secret))
	return NewAESGCMEncryption(version, key[:])
}
