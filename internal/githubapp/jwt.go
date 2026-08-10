package githubapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"time"
)

// AppTokenSource supplies a short-lived GitHub App JWT.
type AppTokenSource interface {
	AppToken(context.Context) (Credential, error)
}

// JWTSigner parses the operator-owned key on demand and creates RS256 App JWTs.
// It does not cache private key material in the long-lived process heap.
type JWTSigner struct {
	clientID string
	keyRef   SecretRef
	secrets  SecretReader
	clock    Clock
	lifetime time.Duration
	backdate time.Duration
}

func NewJWTSigner(cfg Config, secrets SecretReader, clock Clock) (*JWTSigner, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if secrets == nil {
		return nil, fmt.Errorf("%w: secret reader is required", ErrInvalidConfig)
	}
	return &JWTSigner{
		clientID: cfg.ClientID, keyRef: cfg.PrivateKeySecret,
		secrets: secrets, clock: clockOrSystem(clock), lifetime: cfg.JWTLifetime,
		backdate: cfg.JWTBackdate,
	}, nil
}

func (s *JWTSigner) AppToken(ctx context.Context) (Credential, error) {
	keyBytes, err := s.secrets.ReadSecret(ctx, s.keyRef)
	if err != nil {
		zeroBytes(keyBytes)
		return Credential{}, ErrSecretUnavailable
	}
	defer zeroBytes(keyBytes)
	key, err := parseRSAPrivateKey(keyBytes)
	if err != nil {
		return Credential{}, err
	}
	now := s.clock.Now().UTC().Truncate(time.Second)
	header, _ := json.Marshal(struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
	}{Algorithm: "RS256", Type: "JWT"})
	claims, _ := json.Marshal(struct {
		Issuer   string `json:"iss"`
		IssuedAt int64  `json:"iat"`
		Expires  int64  `json:"exp"`
	}{Issuer: s.clientID, IssuedAt: now.Add(-s.backdate).Unix(), Expires: now.Add(s.lifetime).Unix()})
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(nil, key, crypto.SHA256, digest[:])
	if err != nil {
		return Credential{}, fmt.Errorf("%w: signing failed", ErrInvalidPrivateKey)
	}
	return newCredential(unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)), nil
}

func parseRSAPrivateKey(data []byte) (*rsa.PrivateKey, error) {
	trimmed := bytes.TrimSpace(data)
	if !bytes.HasPrefix(trimmed, []byte("-----BEGIN RSA PRIVATE KEY-----")) && !bytes.HasPrefix(trimmed, []byte("-----BEGIN PRIVATE KEY-----")) {
		return nil, ErrInvalidPrivateKey
	}
	block, rest := pem.Decode(trimmed)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 || len(block.Headers) != 0 {
		return nil, ErrInvalidPrivateKey
	}
	var key *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		parsed, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, ErrInvalidPrivateKey
		}
		key = parsed
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, ErrInvalidPrivateKey
		}
		var ok bool
		key, ok = parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, ErrInvalidPrivateKey
		}
	default:
		return nil, ErrInvalidPrivateKey
	}
	if key.N == nil || key.N.BitLen() < 2048 || key.E < 3 || key.Validate() != nil {
		return nil, ErrInvalidPrivateKey
	}
	return key, nil
}

func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
