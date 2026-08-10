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
	"github.com/kuberploy/kuberploy/internal/store"
)

type distributionCredentialSourceFunc func(context.Context, string) (DistributionAuthorization, error)

func (f distributionCredentialSourceFunc) Authorization(ctx context.Context, targetID string) (DistributionAuthorization, error) {
	return f(ctx, targetID)
}

func testDistributionCredential(t *testing.T) DistributionCredentialSource {
	t.Helper()
	return distributionCredentialSourceFunc(func(_ context.Context, targetID string) (DistributionAuthorization, error) {
		if targetID != "registry-1" {
			t.Fatalf("unexpected credential target %q", targetID)
		}
		return NewDistributionBearerAuthorization([]byte("provider-secret-marker"))
	})
}

func testManagedTarget(endpoint string) domain.RegistryTarget {
	return domain.RegistryTarget{
		ID: "registry-1", Name: "managed", Mode: domain.RegistryTargetManaged,
		Endpoint: endpoint, RepositoryPrefix: "kuberploy/team-a",
	}
}

func testDistributionClient(t *testing.T, target domain.RegistryTarget, credentials DistributionCredentialSource, transport http.RoundTripper) *DistributionClient {
	t.Helper()
	cfg := DefaultDistributionClientConfig()
	cfg.AllowPlainHTTP = true
	cfg.ExpectedOrigin = target.Endpoint
	client, err := NewDistributionClient(target, cfg, credentials, transport)
	if err != nil {
		t.Fatalf("new Distribution client: %v", err)
	}
	return client
}

func TestDistributionDeleteManifestVerifiesExactDigestAndAbsence(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.EscapedPath())
		if got := r.Header.Get("Authorization"); got != "Bearer provider-secret-marker" {
			t.Errorf("authorization = %q", got)
		}
		if !strings.Contains(r.Header.Get("Accept"), "application/vnd.oci.image.index.v1+json") {
			t.Errorf("missing OCI Accept header: %q", r.Header.Get("Accept"))
		}
		switch len(requests) {
		case 1:
			w.Header().Set("Docker-Content-Digest", digest)
			w.WriteHeader(http.StatusOK)
		case 2:
			w.WriteHeader(http.StatusAccepted)
		case 3:
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected extra request")
		}
	}))
	defer server.Close()
	client := testDistributionClient(t, testManagedTarget(server.URL), testDistributionCredential(t), nil)

	result, err := client.DeleteManifest(context.Background(), "registry-1", "kuberploy/team-a/service", digest)
	if err != nil {
		t.Fatalf("delete manifest: %v", err)
	}
	if result.Outcome != ManifestDeleted {
		t.Fatalf("outcome = %q", result.Outcome)
	}
	wantPath := "/v2/kuberploy/team-a/service/manifests/" + digest
	if len(requests) != 3 || requests[0] != "HEAD "+wantPath || requests[1] != "DELETE "+wantPath || requests[2] != "HEAD "+wantPath {
		t.Fatalf("requests = %#v", requests)
	}
}

func TestDistributionDeleteManifestMissingIsIdempotent(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	client := testDistributionClient(t, testManagedTarget(server.URL), testDistributionCredential(t), nil)
	digest := "sha256:" + strings.Repeat("b", 64)

	result, err := client.DeleteManifest(context.Background(), "registry-1", "kuberploy/team-a/service", digest)
	if err != nil || result.Outcome != ManifestAlreadyMissing || calls.Load() != 1 {
		t.Fatalf("result=%+v calls=%d err=%v", result, calls.Load(), err)
	}
}

func TestDistributionDeleteManifestRejectsMismatchedDigestHeader(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("c", 64))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client := testDistributionClient(t, testManagedTarget(server.URL), testDistributionCredential(t), nil)

	_, err := client.DeleteManifest(context.Background(), "registry-1", "kuberploy/team-a/service", "sha256:"+strings.Repeat("d", 64))
	if !errors.Is(err, ErrDistributionManifestUnconfirmed) || calls.Load() != 1 {
		t.Fatalf("calls=%d err=%v", calls.Load(), err)
	}
}

func TestDistributionDeleteManifestRejectsAmbiguousDigestHeaders(t *testing.T) {
	digest := "sha256:" + strings.Repeat("d", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Docker-Content-Digest", digest)
		w.Header().Add("Docker-Content-Digest", "sha256:"+strings.Repeat("e", 64))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client := testDistributionClient(t, testManagedTarget(server.URL), testDistributionCredential(t), nil)
	if _, err := client.DeleteManifest(context.Background(), "registry-1", "kuberploy/team-a/service", digest); !errors.Is(err, ErrDistributionManifestUnconfirmed) {
		t.Fatalf("err = %v", err)
	}
}

func TestDistributionClientRefusesRedirectWithoutCredentialForwarding(t *testing.T) {
	var redirected atomic.Int32
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("credential reached redirect target")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer redirectTarget.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL+"/stolen", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client := testDistributionClient(t, testManagedTarget(server.URL), testDistributionCredential(t), nil)

	_, err := client.DeleteManifest(context.Background(), "registry-1", "kuberploy/team-a/service", "sha256:"+strings.Repeat("e", 64))
	var providerErr *DistributionError
	if !errors.As(err, &providerErr) || providerErr.Class != DistributionErrorRedirect || redirected.Load() != 0 {
		t.Fatalf("redirected=%d err=%v", redirected.Load(), err)
	}
}

func TestDistributionClientBoundsProviderBodyAndRedactsIt(t *testing.T) {
	secretBody := strings.Repeat("provider-error-secret", 4000)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("f", 64))
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, secretBody)
	}))
	defer server.Close()
	client := testDistributionClient(t, testManagedTarget(server.URL), testDistributionCredential(t), nil)

	_, err := client.DeleteManifest(context.Background(), "registry-1", "kuberploy/team-a/service", "sha256:"+strings.Repeat("f", 64))
	var providerErr *DistributionError
	if !errors.As(err, &providerErr) || providerErr.Class != DistributionErrorResponseTooLarge {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "provider-error-secret") || strings.Contains(err.Error(), "provider-secret-marker") {
		t.Fatalf("secret leaked in error: %v", err)
	}
}

func TestDistributionClientClassifiesRateLimitWithoutResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.Header().Set("X-Request-Id", "registry_request-1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"errors":[{"message":"provider-secret"}]}`)
	}))
	defer server.Close()
	client := testDistributionClient(t, testManagedTarget(server.URL), testDistributionCredential(t), nil)
	now := time.Date(2026, 8, 9, 1, 2, 3, 0, time.UTC)
	client.now = func() time.Time { return now }

	_, err := client.DeleteManifest(context.Background(), "registry-1", "kuberploy/team-a/service", "sha256:"+strings.Repeat("1", 64))
	var providerErr *DistributionError
	if !errors.As(err, &providerErr) || providerErr.Class != DistributionErrorRateLimit || !providerErr.Retryable() || providerErr.RetryAt != now.Add(30*time.Second) || providerErr.RequestID != "registry_request-1" {
		t.Fatalf("err = %#v", providerErr)
	}
	if strings.Contains(err.Error(), "provider-secret") {
		t.Fatalf("provider body leaked: %v", err)
	}
}

func TestDistributionClientRejectsScopeEndpointAndExternalTargets(t *testing.T) {
	credentials := testDistributionCredential(t)
	cfg := DefaultDistributionClientConfig()
	badEndpoints := []string{
		"http://registry.example.test",
		"https://user:pass@registry.example.test",
		"https://registry.example.test/tenant/path",
		"https://registry.example.test:99999",
		" https://registry.example.test",
		"file:///var/lib/registry",
	}
	for _, endpoint := range badEndpoints {
		cfg.ExpectedOrigin = endpoint
		if _, err := NewDistributionClient(testManagedTarget(endpoint), cfg, credentials, nil); !errors.Is(err, ErrDistributionInvalidConfig) {
			t.Errorf("endpoint %q: err=%v", endpoint, err)
		}
	}
	external := testManagedTarget("https://registry.example.test")
	external.Mode = domain.RegistryTargetExternal
	cfg.ExpectedOrigin = external.Endpoint
	if _, err := NewDistributionClient(external, cfg, credentials, nil); !errors.Is(err, store.ErrRegistryExternalLifecycle) {
		t.Fatalf("external target err=%v", err)
	}

	var calls atomic.Int32
	transport := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("should not be called")
	})
	client := testDistributionClient(t, testManagedTarget("http://registry.example.test"), credentials, transport)
	digest := "sha256:" + strings.Repeat("2", 64)
	for _, request := range []struct{ target, repository, digest string }{
		{"other-target", "kuberploy/team-a/service", digest},
		{"registry-1", "other/team/service", digest},
		{"registry-1", "kuberploy/team-a/../other", digest},
		{"registry-1", "kuberploy/team-a/service", "latest"},
	} {
		if _, err := client.DeleteManifest(context.Background(), request.target, request.repository, request.digest); !errors.Is(err, ErrDistributionScopeMismatch) {
			t.Errorf("request %+v: err=%v", request, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("out-of-scope request reached transport")
	}
}

func TestDistributionClientDoesNotLeakCredentialSourceError(t *testing.T) {
	credentials := distributionCredentialSourceFunc(func(context.Context, string) (DistributionAuthorization, error) {
		return DistributionAuthorization{}, errors.New("credential-provider-secret-marker")
	})
	client := testDistributionClient(t, testManagedTarget("http://registry.example.test"), credentials, roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("transport should not be reached")
		return nil, nil
	}))
	_, err := client.DeleteManifest(context.Background(), "registry-1", "kuberploy/team-a/service", "sha256:"+strings.Repeat("3", 64))
	if !errors.Is(err, ErrDistributionCredentialUnavailable) || strings.Contains(err.Error(), "credential-provider-secret-marker") {
		t.Fatalf("err = %v", err)
	}
}

func TestDistributionAuthorizationRejectsHeaderInjectionAndOversize(t *testing.T) {
	for _, token := range [][]byte{nil, []byte("token\r\nstolen: true"), []byte("token with space"), make([]byte, (16<<10)+1)} {
		if _, err := NewDistributionBearerAuthorization(token); !errors.Is(err, ErrDistributionCredentialUnavailable) {
			t.Errorf("bearer %q: err=%v", token, err)
		}
	}
	if _, err := NewDistributionBasicAuthorization("user:other", []byte("password")); !errors.Is(err, ErrDistributionCredentialUnavailable) {
		t.Fatalf("basic username injection err=%v", err)
	}
	authorization, err := NewDistributionBasicAuthorization("registry-worker", []byte("password"))
	if err != nil {
		t.Fatal(err)
	}
	header, ok := authorization.header()
	if !ok || !strings.HasPrefix(header, "Basic ") || strings.Contains(header, "password") {
		t.Fatalf("invalid Basic authorization rendering")
	}
	authorization.destroy()
}

func TestDistributionClientErasesCallerOwnedCredentialAndHonorsCancellation(t *testing.T) {
	var issued DistributionAuthorization
	credentials := distributionCredentialSourceFunc(func(context.Context, string) (DistributionAuthorization, error) {
		var err error
		issued, err = NewDistributionBearerAuthorization([]byte("ephemeral-provider-token"))
		return issued, err
	})
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	client := testDistributionClient(t, testManagedTarget("http://registry.example.test"), credentials, transport)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.DeleteManifest(ctx, "registry-1", "kuberploy/team-a/service", "sha256:"+strings.Repeat("4", 64))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	for _, b := range issued.value {
		if b != 0 {
			t.Fatal("caller-owned authorization was not erased")
		}
	}
}

func TestDistributionClientConfigurationIsBounded(t *testing.T) {
	credentials := testDistributionCredential(t)
	target := testManagedTarget("https://registry.example.test")
	for name, mutate := range map[string]func(*DistributionClientConfig){
		"short timeout":    func(c *DistributionClientConfig) { c.RequestTimeout = time.Millisecond },
		"long timeout":     func(c *DistributionClientConfig) { c.RequestTimeout = time.Minute },
		"small body":       func(c *DistributionClientConfig) { c.MaxResponseBytes = 1 },
		"large body":       func(c *DistributionClientConfig) { c.MaxResponseBytes = 2 << 20 },
		"header injection": func(c *DistributionClientConfig) { c.UserAgent = "agent\r\nstolen: true" },
		"blank user agent": func(c *DistributionClientConfig) { c.UserAgent = " " },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := DefaultDistributionClientConfig()
			cfg.ExpectedOrigin = target.Endpoint
			mutate(&cfg)
			if _, err := NewDistributionClient(target, cfg, credentials, nil); !errors.Is(err, ErrDistributionInvalidConfig) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	cfg := DefaultDistributionClientConfig()
	cfg.ExpectedOrigin = "https://other-registry.example.test"
	if _, err := NewDistributionClient(target, cfg, credentials, nil); !errors.Is(err, ErrDistributionInvalidConfig) {
		t.Fatalf("mismatched expected origin err = %v", err)
	}
}

func TestDistributionRepositoryGrammarMatchesOCIBounds(t *testing.T) {
	valid := []string{"a", "team/service", "a.b/c_d/e__f/g---h"}
	invalid := []string{"", "UPPER", "/a", "a/", "a//b", "a..b", "a___b", "a_-b", "a:b", strings.Repeat("a", 256)}
	for _, repository := range valid {
		if !validRepository(repository) {
			t.Errorf("expected valid repository %q", repository)
		}
	}
	for _, repository := range invalid {
		if validRepository(repository) {
			t.Errorf("expected invalid repository %q", repository)
		}
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }
