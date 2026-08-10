package registry

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

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

func TestDistributionObserverRejectsCrossScopeCatalogAndPagination(t *testing.T) {
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
			_, _, err := observer.Observe(t.Context(), nil, 1, time.Now().UTC())
			if name == "catalog" && !errors.Is(err, ErrDistributionScopeMismatch) || name == "pagination" && !errors.Is(err, errRegistryObservation) {
				t.Fatalf("err=%v", err)
			}
		})
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
