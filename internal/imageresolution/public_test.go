package imageresolution

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"github.com/kuberploy/kuberploy/internal/imagepull"
)

func TestResolverPublicDockerHubTagWithoutRegistryPolicy(t *testing.T) {
	// Docker Hub serves Docker-style Bearer challenges, a multi-platform OCI
	// index including attestations, and an annotated platform image manifest.
	body := []byte(strings.TrimSuffix(string(validImageManifest(ociManifestMediaType)), "}") + `,"annotations":{"org.opencontainers.image.title":"nginx"}}`)
	digest := manifestResponse(body, ociManifestMediaType).Header.Get("Docker-Content-Digest")
	index := []byte(`{"schemaVersion":2,"mediaType":"` + ociIndexMediaType + `","manifests":[` +
		`{"mediaType":"` + ociManifestMediaType + `","digest":"sha256:` + strings.Repeat("b", 64) + `","size":100,"platform":{"os":"windows","architecture":"amd64","os.version":"10.0.26100.4946","os.features":["win32k"]}},` +
		`{"mediaType":"` + ociManifestMediaType + `","digest":"sha256:` + strings.Repeat("a", 64) + `","size":100,"platform":{"os":"unknown","architecture":"unknown"},"annotations":{"vnd.docker.reference.type":"attestation-manifest"}},` +
		`{"mediaType":"` + ociManifestMediaType + `","digest":"` + digest + `","size":` + stringInt(len(body)) + `,"platform":{"os":"linux","architecture":"amd64"},"annotations":{"org.opencontainers.image.version":"alpine"}}]}`)
	credentials := &resolverCredentialSource{}
	catalog := &resolverCatalog{}
	var manifestCalls, tokenCalls int
	provider := &HTTPProvider{Credentials: credentials, Config: DefaultProviderConfig(), Transport: resolverRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.URL.Scheme != "https" {
			t.Fatal("non-HTTPS request")
		}
		switch request.URL.Host {
		case "auth.docker.io":
			tokenCalls++
			if request.URL.Path != "/token" || request.URL.Query().Get("service") != "registry.docker.io" || request.URL.Query().Get("scope") != "repository:library/nginx:pull" || request.Header.Get("Authorization") != "" {
				t.Fatal("incorrect anonymous token request")
			}
			return publicTokenResponse(), nil
		case "registry-1.docker.io":
			manifestCalls++
			if request.Header.Get("Authorization") == "" {
				return publicChallenge("https://auth.docker.io/token", "registry.docker.io", "library/nginx"), nil
			}
			if request.Header.Get("Authorization") != "Bearer fixture-public-token" {
				t.Fatal("reusable credential sent to public registry")
			}
			switch request.URL.Path {
			case "/v2/library/nginx/manifests/alpine":
				return manifestResponse(index, ociIndexMediaType), nil
			case "/v2/library/nginx/manifests/" + digest:
				return manifestResponse(body, ociManifestMediaType), nil
			}
		}
		t.Fatal("unexpected public registry request")
		return nil, nil
	})}
	resolver := &Resolver{Catalog: catalog, Provider: provider, Config: RuntimeConfig{Platform: DefaultPlatform()}}
	if !resolver.Available() {
		t.Fatal("default public tag resolution is unavailable without an operator policy")
	}
	result, err := resolver.Resolve(t.Context(), "deployer", resolutionApplicationID, resolutionEnvironmentID, "docker.io/library/nginx:alpine")
	if err != nil || !result.Resolved || result.ImmutableImage != "docker.io/library/nginx@"+digest || catalog.calls != 1 || credentials.calls != 0 || tokenCalls != 2 || manifestCalls != 4 {
		t.Fatalf("result=%+v catalog=%d credentials=%d tokens=%d manifests=%d err=%v", result, catalog.calls, credentials.calls, tokenCalls, manifestCalls, err)
	}
}

func TestResolverPublicUnknownRegistrySupportsAnonymousManifestAndChallenge(t *testing.T) {
	for _, challenge := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "challenge"}[challenge], func(t *testing.T) {
			credentials := &resolverCredentialSource{}
			provider := &HTTPProvider{Credentials: credentials, Config: DefaultProviderConfig(), Transport: resolverRoundTripper(func(request *http.Request) (*http.Response, error) {
				if request.URL.Host == "auth.public.example.test" {
					if request.Header.Get("Authorization") != "" {
						t.Fatal("credential sent to anonymous token service")
					}
					// expires_in is optional in the registry token protocol.
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(`{"token":"fixture-public-token"}`))}, nil
				}
				if request.URL.Host != "public.example.test" || request.URL.Path != "/v2/team/app/manifests/v1" {
					t.Fatal("incorrect public registry request")
				}
				if challenge && request.Header.Get("Authorization") == "" {
					return publicChallenge("https://auth.public.example.test/token", "public.example.test", "team/app"), nil
				}
				return manifestResponse(validImageManifest(ociManifestMediaType), ociManifestMediaType), nil
			})}
			resolver := &Resolver{Catalog: &resolverCatalog{sources: []AuthorizedSource{resolutionSource()}}, Provider: provider,
				Config: RuntimeConfig{Profiles: []imagepull.Profile{resolutionProfile()}, Platform: DefaultPlatform()}}
			result, err := resolver.Resolve(t.Context(), "deployer", resolutionApplicationID, resolutionEnvironmentID, "public.example.test/team/app:v1")
			if err != nil || !result.Resolved || credentials.calls != 0 {
				t.Fatalf("result=%+v credentials=%d err=%v", result, credentials.calls, err)
			}
		})
	}
}

func TestResolverPublicTagDoesNotBypassConfiguredRegistryFailure(t *testing.T) {
	credentials := &resolverCredentialSource{}
	requests := 0
	provider := &HTTPProvider{Credentials: credentials, Config: DefaultProviderConfig(), Transport: resolverRoundTripper(func(*http.Request) (*http.Response, error) {
		requests++
		return publicChallenge("https://auth.example.test/token", "registry.example.test", "tenant/service"), nil
	})}
	resolver := &Resolver{Catalog: &resolverCatalog{sources: []AuthorizedSource{resolutionSource()}}, Provider: provider,
		Config: RuntimeConfig{Profiles: []imagepull.Profile{resolutionProfile()}, Platform: DefaultPlatform()}}
	if _, err := resolver.Resolve(t.Context(), "deployer", resolutionApplicationID, resolutionEnvironmentID, "registry.example.test:5000/tenant/service:v1"); !errors.Is(err, ErrUnavailable) || requests != 1 || credentials.calls != 1 {
		t.Fatalf("private policy failure bypassed: requests=%d credentials=%d err=%v", requests, credentials.calls, err)
	}
}

func TestPublicRegistryRejectsUnsafeChallengeAndRedirect(t *testing.T) {
	for name, response := range map[string]*http.Response{
		"http realm":       publicChallenge("http://auth.example.test/token", "public.example.test", "team/app"),
		"loopback realm":   publicChallenge("https://127.0.0.1/token", "public.example.test", "team/app"),
		"private realm":    publicChallenge("https://10.0.0.1/token", "public.example.test", "team/app"),
		"metadata realm":   publicChallenge("https://169.254.169.254/token", "public.example.test", "team/app"),
		"credentials":      publicChallenge("https://user:pass@auth.example.test/token", "public.example.test", "team/app"),
		"realm port":       publicChallenge("https://auth.example.test:8443/token", "public.example.test", "team/app"),
		"query injection":  publicChallenge("https://auth.example.test/token?scope=extra", "public.example.test", "team/app"),
		"extra repository": publicChallenge("https://auth.example.test/token", "public.example.test", "other/app"),
		"push scope":       publicChallenge("https://auth.example.test/token", "public.example.test", "team/app:pull,push"),
		"private redirect": {StatusCode: http.StatusFound, Header: http.Header{"Location": {"https://10.0.0.1/v2/team/app/manifests/v1"}}, Body: io.NopCloser(strings.NewReader("redirect"))},
	} {
		t.Run(name, func(t *testing.T) {
			requests := 0
			provider := &HTTPProvider{Config: DefaultProviderConfig(), Transport: resolverRoundTripper(func(*http.Request) (*http.Response, error) {
				requests++
				return response, nil
			})}
			resolver := &Resolver{Catalog: &resolverCatalog{}, Provider: provider, Config: RuntimeConfig{Platform: DefaultPlatform()}}
			if _, err := resolver.Resolve(t.Context(), "deployer", resolutionApplicationID, resolutionEnvironmentID, "public.example.test/team/app:v1"); err == nil || requests != 1 {
				t.Fatalf("requests=%d err=%v", requests, err)
			}
		})
	}
}

func TestResolverPublicRegistryFollowsHTTPSManifestRedirect(t *testing.T) {
	body := validImageManifest(ociManifestMediaType)
	digest := manifestResponse(body, ociManifestMediaType).Header.Get("Docker-Content-Digest")
	requests := 0
	provider := &HTTPProvider{Config: DefaultProviderConfig(), Transport: resolverRoundTripper(func(request *http.Request) (*http.Response, error) {
		requests++
		switch request.URL.Host {
		case "registry.k8s.io":
			return publicRedirect("https://asia-east1-docker.pkg.dev/v2/k8s-artifacts-prod/images/pause/manifests/3.10"), nil
		case "asia-east1-docker.pkg.dev":
			if request.URL.Path != "/v2/k8s-artifacts-prod/images/pause/manifests/3.10" || request.Header.Get("Authorization") != "" {
				t.Fatal("incorrect redirected manifest request")
			}
			return manifestResponse(body, ociManifestMediaType), nil
		default:
			t.Fatal("unexpected redirect destination")
			return nil, nil
		}
	})}
	resolver := &Resolver{Catalog: &resolverCatalog{}, Provider: provider, Config: RuntimeConfig{Platform: DefaultPlatform()}}
	result, err := resolver.Resolve(t.Context(), "deployer", resolutionApplicationID, resolutionEnvironmentID, "registry.k8s.io/pause:3.10")
	if err != nil || result.ImmutableImage != "registry.k8s.io/pause@"+digest || requests != 2 {
		t.Fatalf("result=%+v requests=%d err=%v", result, requests, err)
	}
}

func TestPublicRegistryRedirectStripsAuthorizationAcrossHosts(t *testing.T) {
	requests, tokenCalls := 0, 0
	provider := &HTTPProvider{Config: DefaultProviderConfig(), Transport: resolverRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == "auth.public.example.test" {
			tokenCalls++
			return publicTokenResponse(), nil
		}
		requests++
		switch request.URL.Path {
		case "/v2/team/app/manifests/v1":
			if request.Header.Get("Authorization") == "" {
				return publicChallenge("https://auth.public.example.test/token", "public.example.test", "team/app"), nil
			}
			return publicRedirect("https://public.example.test/same-host"), nil
		case "/same-host":
			if request.Header.Get("Authorization") != "Bearer fixture-public-token" {
				t.Fatal("same-host redirect lost its pull authorization")
			}
			return publicRedirect("https://cdn.public.example.test/redirected?version=1"), nil
		case "/redirected":
			if request.Header.Get("Authorization") != "" {
				t.Fatal("public token leaked to a different host")
			}
			return publicRedirect("https://public.example.test/final"), nil
		case "/final":
			if request.Header.Get("Authorization") != "" {
				t.Fatal("cross-host redirect chain restored authorization")
			}
			return manifestResponse(validImageManifest(ociManifestMediaType), ociManifestMediaType), nil
		default:
			t.Fatal("unexpected manifest request")
			return nil, nil
		}
	})}
	resolver := &Resolver{Catalog: &resolverCatalog{}, Provider: provider, Config: RuntimeConfig{Platform: DefaultPlatform()}}
	if _, err := resolver.Resolve(t.Context(), "deployer", resolutionApplicationID, resolutionEnvironmentID, "public.example.test/team/app:v1"); err != nil || requests != 5 || tokenCalls != 1 {
		t.Fatalf("requests=%d tokens=%d err=%v", requests, tokenCalls, err)
	}
}

func TestPublicRegistryRedirectBoundsAndPrivatePolicy(t *testing.T) {
	for name, location := range map[string]string{
		"loop": "https://public.example.test/v2/team/app/manifests/v1",
		"http": "http://other.example.test/manifest", "private": "https://10.0.0.1/manifest",
		"metadata": "https://169.254.169.254/manifest", "userinfo": "https://user:password@other.example.test/manifest",
		"port": "https://other.example.test:8443/manifest", "fragment": "https://other.example.test/manifest#fragment",
	} {
		t.Run(name, func(t *testing.T) {
			requests := 0
			provider := &HTTPProvider{Config: DefaultProviderConfig(), Transport: resolverRoundTripper(func(*http.Request) (*http.Response, error) {
				requests++
				return publicRedirect(location), nil
			})}
			resolver := &Resolver{Catalog: &resolverCatalog{}, Provider: provider, Config: RuntimeConfig{Platform: DefaultPlatform()}}
			_, err := resolver.Resolve(t.Context(), "deployer", resolutionApplicationID, resolutionEnvironmentID, "public.example.test/team/app:v1")
			wantRequests := 1
			if name == "loop" {
				wantRequests = 6
			}
			if err == nil || requests != wantRequests {
				t.Fatalf("requests=%d want=%d err=%v", requests, wantRequests, err)
			}
		})
	}
	t.Run("configured registry", func(t *testing.T) {
		credentials := &resolverCredentialSource{}
		requests := 0
		provider := &HTTPProvider{Credentials: credentials, Config: DefaultProviderConfig(), Transport: resolverRoundTripper(func(request *http.Request) (*http.Response, error) {
			requests++
			if request.URL.Host != "registry.example.test:5000" {
				t.Fatal("configured registry credentials escaped their endpoint")
			}
			return publicRedirect("https://public.example.test/manifest"), nil
		})}
		resolver := &Resolver{Catalog: &resolverCatalog{sources: []AuthorizedSource{resolutionSource()}}, Provider: provider,
			Config: RuntimeConfig{Profiles: []imagepull.Profile{resolutionProfile()}, Platform: DefaultPlatform()}}
		if _, err := resolver.Resolve(t.Context(), "deployer", resolutionApplicationID, resolutionEnvironmentID, "registry.example.test:5000/tenant/service:v1"); err == nil || requests != 1 || credentials.calls != 1 {
			t.Fatalf("requests=%d credentials=%d err=%v", requests, credentials.calls, err)
		}
	})
}

func publicRedirect(location string) *http.Response {
	return &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: http.Header{"Location": {location}}, Body: io.NopCloser(strings.NewReader("redirect"))}
}

func TestPublicRegistryDialerRejectsPrivateAndReboundDNS(t *testing.T) {
	for name, addresses := range map[string][]string{
		"loopback": {"127.0.0.1"}, "private": {"10.1.2.3"}, "metadata": {"169.254.169.254"},
		"shared": {"100.100.100.200"}, "ipv6 private": {"fd00::1"}, "ipv6 loopback": {"::1"},
		"mapped private": {"::ffff:192.168.1.1"}, "mixed rebinding": {"8.8.8.8", "10.1.2.3"},
	} {
		t.Run(name, func(t *testing.T) {
			dials := 0
			dialer := publicRegistryDialer(func(context.Context, string, string) ([]netip.Addr, error) {
				var result []netip.Addr
				for _, address := range addresses {
					result = append(result, netip.MustParseAddr(address))
				}
				return result, nil
			}, func(context.Context, string, string) (net.Conn, error) {
				dials++
				return nil, nil
			})
			if _, err := dialer(t.Context(), "tcp", "public.example.test:443"); !errors.Is(err, ErrConflict) || dials != 0 {
				t.Fatalf("dials=%d err=%v", dials, err)
			}
		})
	}
}

func TestPublicRegistryDialerPinsValidatedAddressAndSupportsIPv6(t *testing.T) {
	for _, address := range []string{"8.8.8.8", "2606:4700::1111"} {
		lookups, dials := 0, 0
		dialer := publicRegistryDialer(func(_ context.Context, network, host string) ([]netip.Addr, error) {
			lookups++
			if network != "ip" || host != "public.example.test" {
				t.Fatal("unexpected DNS query")
			}
			return []netip.Addr{netip.MustParseAddr(address)}, nil
		}, func(_ context.Context, network, destination string) (net.Conn, error) {
			dials++
			if network != "tcp" || destination != net.JoinHostPort(address, "443") {
				t.Fatal("dial did not pin the validated public address")
			}
			return nil, nil
		})
		if _, err := dialer(t.Context(), "tcp", "public.example.test:443"); err != nil || lookups != 1 || dials != 1 {
			t.Fatalf("lookups=%d dials=%d err=%v", lookups, dials, err)
		}
	}
}

func TestResolverPublicTagStillRequiresCurrentScopeAuthorization(t *testing.T) {
	for _, denied := range []error{ErrNotFound, ErrForbidden, context.Canceled} {
		provider := &resolverProvider{}
		resolver := &Resolver{Catalog: &resolverCatalog{err: denied}, Provider: provider, Config: RuntimeConfig{Platform: DefaultPlatform()}}
		if _, err := resolver.Resolve(t.Context(), "deployer", resolutionApplicationID, resolutionEnvironmentID, "docker.io/library/nginx:alpine"); !errors.Is(err, denied) || provider.calls != 0 {
			t.Fatalf("provider=%d err=%v", provider.calls, err)
		}
	}
}

func publicChallenge(realm, service, repository string) *http.Response {
	return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{"Www-Authenticate": {`Bearer realm="` + realm + `",service="` + service + `",scope="repository:` + repository + `:pull"`}}, Body: io.NopCloser(strings.NewReader(`{"errors":[]}`))}
}

func publicTokenResponse() *http.Response {
	body := `{"token":"fixture-public-token","access_token":"fixture-public-token","expires_in":300,"issued_at":"2026-09-13T00:00:00Z"}`
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}
}
