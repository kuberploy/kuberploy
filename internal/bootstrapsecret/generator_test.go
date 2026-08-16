package bootstrapsecret

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateCreatesOnceAndNeverSendsTokenOutsideSecretData(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "service-account-token")
	caFile := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(tokenFile, []byte("service-account-bearer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	created := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/namespaces/kuberploy/secrets" ||
			r.Header.Get("Authorization") != "Bearer service-account-bearer" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if created {
			w.WriteHeader(http.StatusConflict)
			return
		}
		var body struct {
			Metadata struct {
				Name      string            `json:"name"`
				Namespace string            `json:"namespace"`
				Labels    map[string]string `json:"labels"`
			} `json:"metadata"`
			Type string            `json:"type"`
			Data map[string]string `json:"data"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Metadata.Name != "kuberploy-bootstrap" || body.Metadata.Namespace != "kuberploy" ||
			body.Type != "Opaque" || len(body.Data) != 1 || body.Data["token"] == "" ||
			body.Metadata.Labels["kuberploy.io/purpose"] != "first-administrator" {
			t.Errorf("unexpected secret envelope: %#v", body)
		}
		created = true
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	certificate := server.Certificate()
	if certificate == nil {
		t.Fatal("test server certificate unavailable")
	}
	if err := os.WriteFile(caFile, pemCertificate(t, certificate), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{APIServer: server.URL, Namespace: "kuberploy", SecretName: "kuberploy-bootstrap", SecretKey: "token", TokenFile: tokenFile, CAFile: caFile}
	first, err := Generate(context.Background(), cfg)
	if err != nil || !first.Created || len(first.Token) != 56 || first.Token[:13] != "kp_bootstrap_" {
		t.Fatalf("first generation = %#v, %v", first, err)
	}
	second, err := Generate(context.Background(), cfg)
	if err != nil || second.Created || second.Token != "" {
		t.Fatalf("replay generation = %#v, %v", second, err)
	}
}

func TestGenerateDoesNotFollowCredentialBearingRedirect(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "service-account-token")
	caFile := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(tokenFile, []byte("service-account-bearer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	redirected := make(chan struct{}, 1)
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected <- struct{}{}
		w.WriteHeader(http.StatusCreated)
	}))
	defer target.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	certificate := redirect.Certificate()
	if certificate == nil {
		t.Fatal("redirect server certificate unavailable")
	}
	if err := os.WriteFile(caFile, pemCertificate(t, certificate), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{APIServer: redirect.URL, Namespace: "kuberploy", SecretName: "kuberploy-bootstrap", SecretKey: "token", TokenFile: tokenFile, CAFile: caFile}
	if _, err := Generate(context.Background(), cfg); err == nil {
		t.Fatal("redirect response unexpectedly succeeded")
	}
	select {
	case <-redirected:
		t.Fatal("bootstrap request followed redirect to a second authority")
	default:
	}
}

func TestConfigRejectsNonExactAuthorityAndObjectCoordinates(t *testing.T) {
	t.Parallel()
	base := Config{APIServer: "https://10.0.0.1:443", Namespace: "kuberploy", SecretName: "kuberploy-bootstrap", SecretKey: "token", TokenFile: "/token", CAFile: "/ca"}
	mutations := []func(*Config){
		func(c *Config) { c.APIServer = "http://10.0.0.1:443" },
		func(c *Config) { c.APIServer = "https://10.0.0.1:443/api" },
		func(c *Config) { c.APIServer = "https://user@10.0.0.1:443" },
		func(c *Config) { c.APIServer = "https://10.0.0.1:99999" },
		func(c *Config) { c.Namespace = "other/namespace" },
		func(c *Config) { c.SecretName = "UPPER" },
		func(c *Config) { c.SecretKey = "bad/key" },
	}
	for i, mutate := range mutations {
		candidate := base
		mutate(&candidate)
		if err := candidate.Validate(); err == nil {
			t.Fatalf("mutation %d was accepted", i)
		}
	}
}

func pemCertificate(t *testing.T, certificate *x509.Certificate) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
}
