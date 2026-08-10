package githubapp

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestJWTSignerCreatesBoundedRS256AppJWT(t *testing.T) {
	cfg := validTestConfig(t)
	clock := &fixedClock{now: time.Date(2026, 8, 9, 1, 2, 3, 987654321, time.UTC)}
	secrets := testSecrets(t, cfg)
	signer, err := NewJWTSigner(cfg, secrets, clock)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := signer.AppToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(credential.Reveal(), ".")
	if len(parts) != 3 {
		t.Fatalf("JWT segments=%d", len(parts))
	}
	decode := func(segment string, target any) {
		t.Helper()
		data, decodeErr := base64.RawURLEncoding.Strict().DecodeString(segment)
		if decodeErr != nil || json.Unmarshal(data, target) != nil {
			t.Fatalf("invalid JWT segment: %v", decodeErr)
		}
	}
	var header map[string]string
	decode(parts[0], &header)
	if len(header) != 2 || header["alg"] != "RS256" || header["typ"] != "JWT" {
		t.Fatalf("header=%#v", header)
	}
	var claims struct {
		Issuer   string `json:"iss"`
		IssuedAt int64  `json:"iat"`
		Expires  int64  `json:"exp"`
	}
	decode(parts[1], &claims)
	now := clock.now.UTC().Truncate(time.Second)
	if claims.Issuer != cfg.ClientID || claims.IssuedAt != now.Add(-time.Minute).Unix() || claims.Expires != now.Add(9*time.Minute).Unix() || claims.Expires > now.Add(10*time.Minute).Unix() {
		t.Fatalf("claims=%#v", claims)
	}
	signature, _ := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	privateKey, _ := privateKeyFixture(t)
	if err = rsa.VerifyPKCS1v15(&privateKey.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
		t.Fatalf("signature: %v", err)
	}
	if got := fmt.Sprintf("%s %#v", credential, credential); strings.Contains(got, credential.Reveal()) || !strings.Contains(got, "REDACTED") {
		t.Fatalf("credential formatting leaked: %q", got)
	}
	encoded, _ := json.Marshal(credential)
	if strings.Contains(string(encoded), credential.Reveal()) {
		t.Fatalf("credential JSON leaked: %s", encoded)
	}
	secrets.mu.Lock()
	defer secrets.mu.Unlock()
	for _, value := range secrets.last {
		if value != 0 {
			t.Fatal("SecretReader-owned private key buffer was not erased")
		}
	}
}

func TestJWTSignerRejectsMalformedOrWrongPrivateKeys(t *testing.T) {
	cfg := validTestConfig(t)
	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecPKCS8, _ := x509.MarshalPKCS8PrivateKey(ec)
	_, validPEM := privateKeyFixture(t)
	tests := map[string][]byte{
		"not PEM":         []byte("this-is-not-a-key"),
		"weak RSA":        pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(weak)}),
		"EC PKCS8":        pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecPKCS8}),
		"public key":      pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: x509.MarshalPKCS1PublicKey(&weak.PublicKey)}),
		"encrypted label": pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Headers: map[string]string{"Proc-Type": "4,ENCRYPTED"}, Bytes: x509.MarshalPKCS1PrivateKey(weak)}),
		"trailing data":   append(append([]byte(nil), validPEM...), []byte("not-whitespace")...),
		"leading data":    append([]byte("not-whitespace"), validPEM...),
		"multiple blocks": append(append([]byte(nil), validPEM...), validPEM...),
	}
	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			secrets := testSecrets(t, cfg)
			secrets.values[cfg.PrivateKeySecret] = value
			signer, createErr := NewJWTSigner(cfg, secrets, &fixedClock{now: time.Now()})
			if createErr != nil {
				t.Fatal(createErr)
			}
			if _, signErr := signer.AppToken(context.Background()); !errors.Is(signErr, ErrInvalidPrivateKey) {
				t.Fatalf("expected invalid private key, got %v", signErr)
			}
		})
	}
}

func TestJWTSignerAcceptsRSAInPKCS8AndRedactsSecretReaderErrors(t *testing.T) {
	cfg := validTestConfig(t)
	key, _ := privateKeyFixture(t)
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	secrets := testSecrets(t, cfg)
	secrets.values[cfg.PrivateKeySecret] = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
	signer, _ := NewJWTSigner(cfg, secrets, &fixedClock{now: time.Now()})
	if _, err = signer.AppToken(context.Background()); err != nil {
		t.Fatalf("PKCS8 RSA rejected: %v", err)
	}
	secretMarker := "super-secret-error-body"
	secrets.err = errors.New(secretMarker)
	if _, err = signer.AppToken(context.Background()); !errors.Is(err, ErrSecretUnavailable) || strings.Contains(err.Error(), secretMarker) {
		t.Fatalf("secret reader error leaked: %v", err)
	}
}
