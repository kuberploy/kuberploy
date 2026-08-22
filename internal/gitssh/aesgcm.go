package gitssh

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"io"
)

const AES256KeyBytes = 32

// AESGCMEncryption encrypts Git private keys with one operator-managed key.
// KeyVersion is stored beside ciphertext so a future keyring can rotate keys
// without changing the database record format.
type AESGCMEncryption struct {
	keyVersion string
	aead       cipher.AEAD
	random     io.Reader
}

func NewAESGCMEncryption(keyVersion string, key []byte) (*AESGCMEncryption, error) {
	if keyVersion == "" || len(key) != AES256KeyBytes {
		return nil, errors.New("Git SSH encryption requires a key version and 32-byte key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &AESGCMEncryption{keyVersion: keyVersion, aead: aead, random: rand.Reader}, nil
}

func (e *AESGCMEncryption) Encrypt(ctx context.Context, plaintext []byte) (PrivateKeyEnvelope, error) {
	if err := ctx.Err(); err != nil {
		return PrivateKeyEnvelope{}, err
	}
	nonce := make([]byte, e.aead.NonceSize())
	if _, err := io.ReadFull(e.random, nonce); err != nil {
		return PrivateKeyEnvelope{}, err
	}
	ciphertext := e.aead.Seal(nonce, nonce, plaintext, []byte(e.keyVersion))
	return PrivateKeyEnvelope{KeyVersion: e.keyVersion, Ciphertext: ciphertext}, nil
}

func (e *AESGCMEncryption) Decrypt(ctx context.Context, envelope PrivateKeyEnvelope) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if envelope.validate() != nil || envelope.KeyVersion != e.keyVersion || len(envelope.Ciphertext) <= e.aead.NonceSize() {
		return nil, ErrInvalidEnvelope
	}
	nonce := envelope.Ciphertext[:e.aead.NonceSize()]
	plaintext, err := e.aead.Open(nil, nonce, envelope.Ciphertext[e.aead.NonceSize():], []byte(envelope.KeyVersion))
	if err != nil {
		return nil, ErrInvalidEnvelope
	}
	return plaintext, nil
}
