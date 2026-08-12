package imageresolution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kuberploy/kuberploy/internal/imagepull"
)

type resolverRoundTripper func(*http.Request) (*http.Response, error)

func (f resolverRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type resolverCredentialSource struct {
	calls int
}

func (s *resolverCredentialSource) Authorization(_ context.Context, profile imagepull.Profile) (authorization, error) {
	s.calls++
	if profile.Name != resolutionProfile().Name {
		return authorization{}, errors.New("secret-profile-marker")
	}
	return authorization{scheme: "Bearer", value: []byte("secret-token-marker")}, nil
}

func manifestResponse(body []byte, mediaType string) *http.Response {
	sum := sha256.Sum256(body)
	header := make(http.Header)
	header.Set("Content-Type", mediaType)
	header.Set("Docker-Content-Digest", "sha256:"+hex.EncodeToString(sum[:]))
	return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}
}

func validImageManifest(mediaType string) []byte {
	configMedia := "application/vnd.oci.image.config.v1+json"
	if mediaType == dockerManifestMediaType {
		configMedia = "application/vnd.docker.container.image.v1+json"
	}
	return []byte(`{"schemaVersion":2,"mediaType":"` + mediaType + `","config":{"mediaType":"` + configMedia + `","digest":"sha256:` + strings.Repeat("9", 64) + `","size":123},"layers":[]}`)
}

func validImageManifestWithLargeLayer(mediaType string) []byte {
	configMedia := "application/vnd.oci.image.config.v1+json"
	return []byte(`{"schemaVersion":2,"mediaType":"` + mediaType + `","config":{"mediaType":"` + configMedia + `","digest":"sha256:` + strings.Repeat("9", 64) + `","size":123},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":"sha256:` + strings.Repeat("8", 64) + `","size":20313073}]}`)
}

func TestHTTPProviderResolvesVerifiedManifestWithExactCredentialScope(t *testing.T) {
	body := validImageManifest(ociManifestMediaType)
	want := manifestResponse(body, ociManifestMediaType).Header.Get("Docker-Content-Digest")
	credentials := &resolverCredentialSource{}
	provider := &HTTPProvider{Credentials: credentials, Config: DefaultProviderConfig(), Transport: resolverRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://registry.example.test:5000/v2/tenant/service/manifests/stable" ||
			request.Header.Get("Authorization") != "Bearer secret-token-marker" || request.Header.Get("Accept") != manifestAccept {
			t.Fatalf("unsafe request: %s headers=%v", request.URL, request.Header)
		}
		return manifestResponse(body, ociManifestMediaType), nil
	})}
	digest, err := provider.ResolveTag(t.Context(), resolutionSource(), TagReference{Server: "registry.example.test:5000", Repository: "tenant/service", Tag: "stable"},
		&ProviderAuthority{Profile: pointerProfile(resolutionProfile())}, DefaultPlatform())
	if err != nil || digest != want || credentials.calls != 1 {
		t.Fatalf("digest=%q calls=%d err=%v", digest, credentials.calls, err)
	}
}

func TestHTTPProviderSelectsExactlyOneBoundedPlatformManifest(t *testing.T) {
	child := validImageManifestWithLargeLayer(ociManifestMediaType)
	childDigest := manifestResponse(child, ociManifestMediaType).Header.Get("Docker-Content-Digest")
	index := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[` +
		`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:` + strings.Repeat("1", 64) + `","size":123,"platform":{"os":"unknown","architecture":"unknown"},"annotations":{"vnd.docker.reference.type":"attestation-manifest","vnd.docker.reference.digest":"sha256:` + strings.Repeat("2", 64) + `"}},` +
		`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + childDigest + `","size":` + stringInt(len(child)) + `,"platform":{"os":"linux","architecture":"amd64"}}]}`)
	var requests atomic.Int32
	provider := &HTTPProvider{Config: DefaultProviderConfig(), Transport: resolverRoundTripper(func(request *http.Request) (*http.Response, error) {
		if requests.Add(1) == 1 {
			return manifestResponse(index, ociIndexMediaType), nil
		}
		if !strings.HasSuffix(request.URL.Path, "/manifests/"+childDigest) {
			t.Fatalf("child URL=%s", request.URL)
		}
		return manifestResponse(child, ociManifestMediaType), nil
	})}
	digest, err := provider.ResolveTag(t.Context(), anonymousResolutionSource(), TagReference{Server: "registry.example.test", Repository: "tenant/service", Tag: "multi"},
		&ProviderAuthority{Anonymous: true}, DefaultPlatform())
	if err != nil || digest != childDigest || requests.Load() != 2 {
		t.Fatalf("digest=%q requests=%d err=%v", digest, requests.Load(), err)
	}
}

func TestSelectPlatformRejectsUnknownDescriptorFieldsAndUnsafeAnnotations(t *testing.T) {
	digest := "sha256:" + strings.Repeat("1", 64)
	for name, descriptor := range map[string]string{
		"unknown field":       `{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + digest + `","size":123,"platform":{"os":"linux","architecture":"amd64"},"credentials":"secret"}`,
		"unsafe annotation":   `{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + digest + `","size":123,"platform":{"os":"linux","architecture":"amd64"},"annotations":{"unsafe key":"value"}}`,
		"oversize annotation": `{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + digest + `","size":123,"platform":{"os":"linux","architecture":"amd64"},"annotations":{"safe.key":"` + strings.Repeat("a", maximumAnnotationValue+1) + `"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			index := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[` + descriptor + `]}`)
			if _, err := selectPlatform(index, ociIndexMediaType, DefaultPlatform(), 64, 1<<20); !errors.Is(err, ErrConflict) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestHTTPProviderRejectsRedirectDigestForgeryOversizeAndIndexAmbiguity(t *testing.T) {
	manifest := validImageManifest(ociManifestMediaType)
	for name, response := range map[string]*http.Response{
		"redirect": {StatusCode: http.StatusTemporaryRedirect, Header: http.Header{"Location": {"https://evil.example.test/stolen"}}, Body: io.NopCloser(strings.NewReader(""))},
		"digest mismatch": func() *http.Response {
			value := manifestResponse(manifest, ociManifestMediaType)
			value.Header.Set("Docker-Content-Digest", "sha256:"+strings.Repeat("f", 64))
			return value
		}(),
		"oversize": {StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {ociManifestMediaType}, "Docker-Content-Digest": {"sha256:" + strings.Repeat("a", 64)}}, Body: io.NopCloser(io.LimitReader(&infiniteA{}, 2<<20)), ContentLength: 2 << 20},
	} {
		t.Run(name, func(t *testing.T) {
			provider := &HTTPProvider{Config: DefaultProviderConfig(), Transport: resolverRoundTripper(func(*http.Request) (*http.Response, error) { return response, nil })}
			_, err := provider.ResolveTag(t.Context(), anonymousResolutionSource(), TagReference{Server: "registry.example.test", Repository: "tenant/service", Tag: "latest"}, &ProviderAuthority{Anonymous: true}, DefaultPlatform())
			if err == nil || strings.Contains(err.Error(), "evil.example.test") {
				t.Fatalf("err=%v", err)
			}
		})
	}
	childDigest := "sha256:" + strings.Repeat("2", 64)
	index := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[` +
		`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + childDigest + `","size":100,"platform":{"os":"linux","architecture":"amd64"}},` +
		`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + childDigest + `","size":100,"platform":{"os":"linux","architecture":"amd64"}}]}`)
	provider := &HTTPProvider{Config: DefaultProviderConfig(), Transport: resolverRoundTripper(func(*http.Request) (*http.Response, error) { return manifestResponse(index, ociIndexMediaType), nil })}
	if _, err := provider.ResolveTag(t.Context(), anonymousResolutionSource(), TagReference{Server: "registry.example.test", Repository: "tenant/service", Tag: "ambiguous"}, &ProviderAuthority{Anonymous: true}, DefaultPlatform()); !errors.Is(err, ErrConflict) {
		t.Fatalf("ambiguous platform err=%v", err)
	}
}

func TestHTTPProviderNeverSendsCredentialOnAnonymousRequest(t *testing.T) {
	credentials := &resolverCredentialSource{}
	body := validImageManifest(ociManifestMediaType)
	provider := &HTTPProvider{Credentials: credentials, Config: DefaultProviderConfig(), Transport: resolverRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "" {
			t.Fatal("credential was sent for explicit anonymous authority")
		}
		return manifestResponse(body, ociManifestMediaType), nil
	})}
	if _, err := provider.ResolveTag(t.Context(), anonymousResolutionSource(), TagReference{Server: "registry.example.test", Repository: "tenant/service", Tag: "public"}, &ProviderAuthority{Anonymous: true}, DefaultPlatform()); err != nil || credentials.calls != 0 {
		t.Fatalf("calls=%d err=%v", credentials.calls, err)
	}
}

func TestHTTPProviderPerformsOnlyExactOperatorBoundBearerChallenge(t *testing.T) {
	body := validImageManifest(ociManifestMediaType)
	source := anonymousResolutionSource()
	tokenAuthority := &TokenAuthority{TargetID: resolutionTargetID, RealmURL: "https://auth.example.test/token", Service: "registry.example.test"}
	var requests []string
	provider := &HTTPProvider{Config: DefaultProviderConfig(), Transport: resolverRoundTripper(func(request *http.Request) (*http.Response, error) {
		requests = append(requests, request.URL.String()+" auth="+request.Header.Get("Authorization"))
		switch len(requests) {
		case 1:
			header := make(http.Header)
			header.Set("WWW-Authenticate", `Bearer realm="https://auth.example.test/token",service="registry.example.test",scope="repository:tenant/service:pull"`)
			return &http.Response{StatusCode: http.StatusUnauthorized, Header: header, Body: io.NopCloser(strings.NewReader(`{"errors":[]}`))}, nil
		case 2:
			if request.URL.Host != "auth.example.test" || request.Header.Get("Authorization") != "" || request.URL.Query().Get("service") != "registry.example.test" || request.URL.Query().Get("scope") != "repository:tenant/service:pull" {
				t.Fatalf("unsafe token request: %s headers=%v", request.URL, request.Header)
			}
			tokenBody := `{"token":"short-lived-token","access_token":"short-lived-token","expires_in":300}`
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(tokenBody)), ContentLength: int64(len(tokenBody))}, nil
		case 3:
			if request.URL.Host != "registry.example.test" || request.Header.Get("Authorization") != "Bearer short-lived-token" {
				t.Fatalf("unsafe manifest retry: %s headers=%v", request.URL, request.Header)
			}
			return manifestResponse(body, ociManifestMediaType), nil
		default:
			t.Fatalf("unexpected request %d", len(requests))
			return nil, nil
		}
	})}
	digest, err := provider.ResolveTag(t.Context(), source, TagReference{Server: "registry.example.test", Repository: "tenant/service", Tag: "public"},
		&ProviderAuthority{Anonymous: true, Token: tokenAuthority}, DefaultPlatform())
	if err != nil || digest == "" || len(requests) != 3 {
		t.Fatalf("digest=%q requests=%v err=%v", digest, requests, err)
	}
}

func TestHTTPProviderRejectsUnboundChallengeAndMalformedToken(t *testing.T) {
	for name, challenge := range map[string]string{
		"wrong realm":   `Bearer realm="https://evil.example.test/token",service="registry.example.test",scope="repository:tenant/service:pull"`,
		"wrong service": `Bearer realm="https://auth.example.test/token",service="other",scope="repository:tenant/service:pull"`,
		"wrong scope":   `Bearer realm="https://auth.example.test/token",service="registry.example.test",scope="repository:other/service:pull"`,
		"extra":         `Bearer realm="https://auth.example.test/token",service="registry.example.test",scope="repository:tenant/service:pull",account="caller"`,
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			provider := &HTTPProvider{Config: DefaultProviderConfig(), Transport: resolverRoundTripper(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{"Www-Authenticate": {challenge}}, Body: io.NopCloser(strings.NewReader("denied"))}, nil
			})}
			authority := &ProviderAuthority{Anonymous: true, Token: &TokenAuthority{TargetID: resolutionTargetID, RealmURL: "https://auth.example.test/token", Service: "registry.example.test"}}
			if _, err := provider.ResolveTag(t.Context(), anonymousResolutionSource(), TagReference{Server: "registry.example.test", Repository: "tenant/service", Tag: "latest"}, authority, DefaultPlatform()); err == nil || calls.Load() != 1 {
				t.Fatalf("calls=%d err=%v", calls.Load(), err)
			}
		})
	}
	var calls atomic.Int32
	provider := &HTTPProvider{Config: DefaultProviderConfig(), Transport: resolverRoundTripper(func(*http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{"Www-Authenticate": {`Bearer realm="https://auth.example.test/token",service="registry.example.test",scope="repository:tenant/service:pull"`}}, Body: io.NopCloser(strings.NewReader("denied"))}, nil
		}
		body := `{"token":"one","access_token":"two","expires_in":99999}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}, nil
	})}
	authority := &ProviderAuthority{Anonymous: true, Token: &TokenAuthority{TargetID: resolutionTargetID, RealmURL: "https://auth.example.test/token", Service: "registry.example.test"}}
	if _, err := provider.ResolveTag(t.Context(), anonymousResolutionSource(), TagReference{Server: "registry.example.test", Repository: "tenant/service", Tag: "latest"}, authority, DefaultPlatform()); !errors.Is(err, ErrConflict) || calls.Load() != 2 {
		t.Fatalf("calls=%d err=%v", calls.Load(), err)
	}
}

func TestHTTPProviderStrictlyRejectsIncompleteOrUnknownManifestEnvelope(t *testing.T) {
	for name, body := range map[string][]byte{
		"missing config":    []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[]}`),
		"unknown field":     append(bytes.TrimSuffix(validImageManifest(ociManifestMediaType), []byte("}")), []byte(`,"credentials":"secret"}`)...),
		"duplicate field":   []byte(`{"schemaVersion":2,"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:` + strings.Repeat("9", 64) + `","size":1},"layers":[]}`),
		"bad config digest": []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"latest","size":1},"layers":[]}`),
	} {
		t.Run(name, func(t *testing.T) {
			provider := &HTTPProvider{Config: DefaultProviderConfig(), Transport: resolverRoundTripper(func(*http.Request) (*http.Response, error) { return manifestResponse(body, ociManifestMediaType), nil })}
			if _, err := provider.ResolveTag(t.Context(), anonymousResolutionSource(), TagReference{Server: "registry.example.test", Repository: "tenant/service", Tag: "bad"}, &ProviderAuthority{Anonymous: true}, DefaultPlatform()); !errors.Is(err, ErrConflict) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func pointerProfile(value imagepull.Profile) *imagepull.Profile { return &value }

func stringInt(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	position := len(digits)
	for value > 0 {
		position--
		digits[position] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[position:])
}

type infiniteA struct{}

func (*infiniteA) Read(value []byte) (int, error) {
	for index := range value {
		value[index] = 'a'
	}
	return len(value), nil
}
