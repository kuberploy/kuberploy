package secrets

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func projectedCertificatePEM(t *testing.T) []byte {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "projected-sealed-secret-test"},
		NotBefore: testTime.Add(-time.Hour), NotAfter: testTime.Add(time.Hour), KeyUsage: x509.KeyUsageKeyEncipherment,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestManagedSealingCertificateReadsActiveControllerCertificate(t *testing.T) {
	encoded := projectedCertificatePEM(t)
	defer clear(encoded)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/v1/cert.pem" || request.Header.Get("Accept") == "" {
			http.Error(response, "invalid request", http.StatusBadRequest)
			return
		}
		response.Header().Set("Content-Type", "application/x-pem-file")
		_, _ = response.Write(encoded)
	}))
	defer server.Close()

	source := managedSealingCertificate{endpoint: server.URL + "/v1/cert.pem", client: server.Client()}
	key, err := source.ActivePublicKey(t.Context(), testTime)
	if err != nil || key.key == nil || !digestRE.MatchString(key.fingerprint) {
		t.Fatalf("key=%#v err=%v", key, err)
	}
}

func TestManagedSealingCertificateFailsClosed(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"status": func(response http.ResponseWriter, _ *http.Request) {
			http.Error(response, "unavailable", http.StatusServiceUnavailable)
		},
		"invalid": func(response http.ResponseWriter, _ *http.Request) {
			_, _ = response.Write([]byte("not a certificate"))
		},
		"oversized": func(response http.ResponseWriter, _ *http.Request) {
			_, _ = response.Write(bytes.Repeat([]byte{'x'}, maximumSealingCertificateBytes+1))
		},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(handler)
			defer server.Close()
			source := managedSealingCertificate{endpoint: server.URL, client: server.Client()}
			if _, err := source.ActivePublicKey(t.Context(), testTime); !errors.Is(err, ErrProviderOperation) {
				t.Fatalf("error=%v", err)
			}
		})
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := (managedSealingCertificate{endpoint: "http://controller.invalid", client: http.DefaultClient}).ActivePublicKey(canceled, testTime); !errors.Is(err, ErrProviderOperation) {
		t.Fatalf("canceled error=%v", err)
	}
}

func TestProjectedSealingCertificateReadsPrivateAtomicProjection(t *testing.T) {
	directory := t.TempDir()
	version := filepath.Join(directory, "..2026_08_09")
	if err := os.Mkdir(version, 0o700); err != nil {
		t.Fatal(err)
	}
	encoded := projectedCertificatePEM(t)
	defer clear(encoded)
	if err := os.WriteFile(filepath.Join(version, DefaultSealedSecretsCertificateKey), encoded, 0o440); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(filepath.Join(version, DefaultSealedSecretsCertificateKey), os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(version), filepath.Join(directory, "..data")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, DefaultSealedSecretsCertificateKey)
	if err := os.Symlink(filepath.Join("..data", DefaultSealedSecretsCertificateKey), path); err != nil {
		t.Fatal(err)
	}
	key, err := readProjectedSealingCertificate(context.Background(), path, testTime)
	if err != nil || key.key == nil || !digestRE.MatchString(key.fingerprint) {
		t.Fatalf("key=%#v err=%v", key, err)
	}
}

func TestProjectedSealingCertificateFailsClosed(t *testing.T) {
	directory := t.TempDir()
	encoded := projectedCertificatePEM(t)
	defer clear(encoded)
	public := filepath.Join(directory, "public.crt")
	invalid := filepath.Join(directory, "invalid.crt")
	oversized := filepath.Join(directory, "oversized.crt")
	if err := os.WriteFile(public, encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(invalid, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oversized, bytes.Repeat([]byte{'x'}, maximumSealingCertificateBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.crt")
	if err := os.WriteFile(outside, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(directory, "escape.crt")
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{public, invalid, oversized, escape, filepath.Join(directory, "missing.crt"), "relative.crt"} {
		if _, err := readProjectedSealingCertificate(context.Background(), path, testTime); !errors.Is(err, ErrProviderOperation) {
			t.Fatalf("path=%q err=%v", path, err)
		}
	}
	private := filepath.Join(directory, "private.crt")
	if err := os.WriteFile(private, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readProjectedSealingCertificate(context.Background(), private, testTime.Add(2*time.Hour)); !errors.Is(err, ErrProviderOperation) {
		t.Fatalf("expired certificate err=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readProjectedSealingCertificate(ctx, private, testTime); !errors.Is(err, ErrProviderOperation) {
		t.Fatalf("canceled context err=%v", err)
	}
}
