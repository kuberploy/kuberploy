package gitssh

import "context"

// PrivateKeyEnvelope is opaque ciphertext plus the encryption-key version used
// to produce it. Lifecycle APIs never expose stored envelopes.
type PrivateKeyEnvelope struct {
	KeyVersion string
	Ciphertext []byte
}

// KeyEncryption encrypts serialized PKCS#8 private key bytes before storage.
// Implementations must not retain plaintext after Encrypt returns.
type KeyEncryption interface {
	Encrypt(ctx context.Context, plaintext []byte) (PrivateKeyEnvelope, error)
	Decrypt(ctx context.Context, envelope PrivateKeyEnvelope) ([]byte, error)
}

func (e PrivateKeyEnvelope) validate() error {
	if e.KeyVersion == "" || len(e.Ciphertext) == 0 {
		return ErrInvalidEnvelope
	}
	return nil
}

func cloneEnvelope(envelope PrivateKeyEnvelope) PrivateKeyEnvelope {
	return PrivateKeyEnvelope{
		KeyVersion: envelope.KeyVersion,
		Ciphertext: append([]byte(nil), envelope.Ciphertext...),
	}
}
