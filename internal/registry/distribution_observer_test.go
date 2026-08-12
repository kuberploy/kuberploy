package registry

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

func TestDistributionObserverAcceptsExactBuildxAttestation(t *testing.T) {
	const subjectDigest = "sha256:70e84cdc49c9bc20fb1150d17e1aa76cb71be6b3a891900439352aeea5fe1bd2"
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","artifactType":"application/vnd.docker.attestation.manifest.v1+json","subject":{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + subjectDigest + `","size":2185},"config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","size":2,"data":"e30="},"layers":[{"mediaType":"application/vnd.in-toto+json","digest":"sha256:69ef63c3a804ea741a0bd0698984e6eb66fc35ff27513739442afde0e0d06c36","size":1390}]}`)
	sum := sha256.Sum256(body)
	digest := "sha256:" + fmt.Sprintf("%x", sum[:])
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v2/kuberploy-qualification/image/manifests/"+digest {
			t.Fatalf("path=%s", request.URL.Path)
		}
		w.Header().Set("Content-Type", ociManifestMediaType)
		w.Header().Set("Docker-Content-Digest", digest)
		_, _ = w.Write(body)
	}))
	defer server.Close()
	observer := testDistributionObserver(t, testManagedTarget(server.URL), nil)
	manifest, present, err := observer.fetchManifest(t.Context(), "kuberploy-qualification/image", digest)
	if err != nil || !present || manifest.kind != domain.RegistryManifestImage || len(manifest.children) != 0 || len(manifest.blobs) != 2 {
		t.Fatalf("manifest=%+v present=%v err=%v", manifest, present, err)
	}
}

func TestDistributionObserverRejectsUntrustedArtifactAndInlineData(t *testing.T) {
	for name, body := range map[string][]byte{
		"artifact":    []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","artifactType":"application/vnd.example.unknown","subject":{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:70e84cdc49c9bc20fb1150d17e1aa76cb71be6b3a891900439352aeea5fe1bd2","size":2185},"config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","size":2,"data":"e30="},"layers":[]}`),
		"inline-data": []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","size":2,"data":"e31="},"layers":[]}`),
	} {
		t.Run(name, func(t *testing.T) {
			sum := sha256.Sum256(body)
			digest := "sha256:" + fmt.Sprintf("%x", sum[:])
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", ociManifestMediaType)
				w.Header().Set("Docker-Content-Digest", digest)
				_, _ = w.Write(body)
			}))
			defer server.Close()
			observer := testDistributionObserver(t, testManagedTarget(server.URL), nil)
			if _, _, err := observer.fetchManifest(t.Context(), "kuberploy-qualification/image", digest); !errors.Is(err, errRegistryObservation) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func testDistributionObserver(t *testing.T, target domain.RegistryTarget, transport http.RoundTripper) *DistributionObserver {
	t.Helper()
	config := DefaultDistributionObserverConfig()
	config.ExpectedOrigin = target.Endpoint
	config.AllowPlainHTTP = true
	observer, err := NewDistributionObserver(target, config, testDistributionCredential(t), transport)
	if err != nil {
		t.Fatal(err)
	}
	return observer
}

func TestDistributionObserverAllowsExternalReadOnlyInventory(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		methods = append(methods, request.Method)
		if request.URL.RequestURI() != "/v2/_catalog?n=100" || request.Header.Get("Authorization") != "Bearer provider-secret-marker" {
			t.Errorf("request=%s auth=%q", request.URL.RequestURI(), request.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"repositories":[]}`)
	}))
	defer server.Close()
	target := testManagedTarget(server.URL)
	target.Mode = domain.RegistryTargetExternal
	observer := testDistributionObserver(t, target, nil)
	if _, implementsDelete := any(observer).(ManifestDeleter); implementsDelete {
		t.Fatal("read-only observer unexpectedly exposes manifest deletion")
	}
	inventory, catalogs, err := observer.Observe(context.Background(), map[string][]string{}, 1, time.Now().UTC())
	if err != nil || !inventory.Complete || len(inventory.Repositories) != 0 || len(catalogs) != 0 {
		t.Fatalf("inventory=%+v catalogs=%d err=%v", inventory, len(catalogs), err)
	}
	if len(methods) != 1 || methods[0] != http.MethodGet {
		t.Fatalf("methods=%v", methods)
	}
}

func TestDistributionObserverIgnoresCrossScopeCatalogAndRejectsUnsafePagination(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"catalog": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"repositories":["other/repository"]}`)
		},
		"pagination": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Link", `<https://attacker.invalid/v2/_catalog?n=100&last=x>; rel="next"`)
			_, _ = io.WriteString(w, `{"repositories":[]}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(handler)
			defer server.Close()
			observer := testDistributionObserver(t, testManagedTarget(server.URL), nil)
			inventory, catalogs, err := observer.Observe(t.Context(), nil, 1, time.Now().UTC())
			if name == "catalog" && (err != nil || len(inventory.Repositories) != 0 || len(catalogs) != 0) ||
				name == "pagination" && !errors.Is(err, errRegistryObservation) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestDistributionObserverStillRejectsCrossScopeDurableRoots(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v2/_catalog" {
			t.Fatalf("cross-scope root reached repository transport: %s", request.URL.Path)
		}
		_, _ = io.WriteString(w, `{"repositories":[]}`)
	}))
	defer server.Close()
	observer := testDistributionObserver(t, testManagedTarget(server.URL), nil)
	_, _, err := observer.Observe(t.Context(), map[string][]string{
		"other/repository": {"sha256:" + strings.Repeat("a", 64)},
	}, 1, time.Now().UTC())
	if !errors.Is(err, ErrDistributionScopeMismatch) {
		t.Fatalf("err=%v", err)
	}
}

func TestDistributionObserverRefusesRedirectAndBoundsBodies(t *testing.T) {
	var redirected atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer sink.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		http.Redirect(w, request, sink.URL+"/credential", http.StatusTemporaryRedirect)
	}))
	observer := testDistributionObserver(t, testManagedTarget(redirector.URL), nil)
	_, _, err := observer.Observe(t.Context(), nil, 1, time.Now().UTC())
	redirector.Close()
	if err == nil || redirected.Load() != 0 || strings.Contains(err.Error(), "provider-secret-marker") {
		t.Fatalf("redirected=%d err=%v", redirected.Load(), err)
	}

	oversized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", maximumObservedManifestBody+1))
	}))
	defer oversized.Close()
	observer = testDistributionObserver(t, testManagedTarget(oversized.URL), nil)
	_, _, err = observer.Observe(t.Context(), nil, 1, time.Now().UTC())
	var distributionError *DistributionError
	if !errors.As(err, &distributionError) || distributionError.Class != DistributionErrorResponseTooLarge {
		t.Fatalf("err=%v", err)
	}
}
