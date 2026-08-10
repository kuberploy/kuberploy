package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	testKeyOnce sync.Once
	testKey     *rsa.PrivateKey
	testKeyPEM  []byte
	testKeyErr  error
)

func privateKeyFixture(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	testKeyOnce.Do(func() {
		testKey, testKeyErr = rsa.GenerateKey(rand.Reader, 2048)
		if testKeyErr == nil {
			testKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(testKey)})
		}
	})
	if testKeyErr != nil {
		t.Fatal(testKeyErr)
	}
	return testKey, append([]byte(nil), testKeyPEM...)
}

func validTestConfig(t *testing.T) Config {
	t.Helper()
	_, key := privateKeyFixture(t)
	_ = key
	cfg := DefaultConfig()
	cfg.AppID = 12345
	cfg.ClientID = "Iv1_kuberploy_test"
	cfg.PrivateKeySecret = SecretRef{Name: "github-app", Key: "private-key.pem"}
	cfg.WebhookSecret = SecretRef{Name: "github-app", Key: "webhook-secret"}
	cfg.StateSigningSecret = SecretRef{Name: "github-app", Key: "state-secret"}
	cfg.MaximumTokenPermissions = Permissions{"metadata": PermissionRead, "contents": PermissionRead, "checks": PermissionWrite}
	return cfg
}

type fixedClock struct{ now time.Time }

func (c *fixedClock) Now() time.Time { return c.now }

type mapSecrets struct {
	values map[SecretRef][]byte
	err    error
	last   []byte
	mu     sync.Mutex
}

func (s *mapSecrets) ReadSecret(_ context.Context, ref SecretRef) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value := append([]byte(nil), s.values[ref]...)
	s.last = value
	return value, s.err
}

func testSecrets(t *testing.T, cfg Config) *mapSecrets {
	t.Helper()
	_, key := privateKeyFixture(t)
	return &mapSecrets{values: map[SecretRef][]byte{
		cfg.PrivateKeySecret:   key,
		cfg.WebhookSecret:      []byte("webhook-secret-with-at-least-32-bytes"),
		cfg.StateSigningSecret: []byte("state-signing-secret-with-at-least-32-bytes"),
	}}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func httpResponse(status int, body string, headers map[string]string) *http.Response {
	h := make(http.Header)
	for name, value := range headers {
		h.Set(name, value)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

type staticAppTokens struct {
	token Credential
	err   error
}

func (s staticAppTokens) AppToken(context.Context) (Credential, error) { return s.token, s.err }

func testAppToken() Credential { return newCredential("header.payload.signature-value") }

type memoryClaimer struct {
	mu     sync.Mutex
	claims map[string]OneTimeClaim
}

func (c *memoryClaimer) ClaimOnce(_ context.Context, claim OneTimeClaim) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.claims == nil {
		c.claims = make(map[string]OneTimeClaim)
	}
	key := claim.Kind + ":" + claim.ClaimKey
	if _, exists := c.claims[key]; exists {
		return false, nil
	}
	c.claims[key] = claim
	return true, nil
}
