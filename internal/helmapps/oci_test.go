package helmapps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testOCIToken = "0123456789abcdef0123456789abcdef"

type recordingOCICredentialProvider struct {
	host, repository string
	credential       *OCIRegistryCredential
	calls            atomic.Int64
}

func (p *recordingOCICredentialProvider) AcquireOCIRegistryCredential(_ context.Context, host, repository string) (*OCIRegistryCredential, error) {
	p.calls.Add(1)
	if host != p.host || repository != p.repository || p.credential == nil {
		return nil, ErrOCIUnauthorized
	}
	return p.credential, nil
}

type ociRegistryFixture struct {
	t                  *testing.T
	server             *httptest.Server
	host               string
	repository         string
	packageBytes       []byte
	manifestBytes      []byte
	approval           Approval
	requireAuth        bool
	challenge          string
	manifestDigest     string
	manifestMediaType  string
	manifestStatus     int
	blobBytes          []byte
	blobStatus         int
	tokenMediaType     string
	tokenStatus        int
	tokenBody          string
	redirectManifest   bool
	blobRedirectURL    string
	tokenAuthorization string
	manifestRequests   atomic.Int64
	blobRequests       atomic.Int64
	tokenRequests      atomic.Int64
}

func newOCIRegistryFixture(t *testing.T, requireAuth bool) *ociRegistryFixture {
	t.Helper()
	files := testChartFiles()
	packageBytes := packageChart(t, files)
	manifestBytes := marshalOCIManifest(t, "", []ociDescriptor{{
		MediaType: HelmChartMediaType, Digest: digestBytes(packageBytes), Size: int64(len(packageBytes)),
	}})
	fixture := &ociRegistryFixture{t: t, repository: "platform/sample", packageBytes: packageBytes,
		manifestBytes: manifestBytes, requireAuth: requireAuth, manifestDigest: digestBytes(manifestBytes),
		manifestMediaType: OCIManifestMediaType, manifestStatus: http.StatusOK, blobBytes: append([]byte(nil), packageBytes...),
		blobStatus: http.StatusOK, tokenMediaType: "application/json", tokenStatus: http.StatusOK,
		tokenBody: `{"token":"` + testOCIToken + `"}`}
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fixture.serve(writer, request)
	}))
	fixture.server = server
	fixture.host = strings.TrimPrefix(server.URL, "https://")
	approval := testApproval(t, packageBytes, files)
	approval.OCIRepository = "oci://" + fixture.host + "/" + fixture.repository
	approval.ManifestDigest = fixture.manifestDigest
	if err := approval.Validate(); err != nil {
		server.Close()
		t.Fatalf("invalid OCI fixture approval: %v", err)
	}
	fixture.approval = approval
	t.Cleanup(server.Close)
	return fixture
}

func marshalOCIManifest(t *testing.T, artifactType string, layers []ociDescriptor) []byte {
	t.Helper()
	encoded, err := json.Marshal(helmOCIManifest{SchemaVersion: 2, MediaType: OCIManifestMediaType,
		ArtifactType: artifactType, Config: ociDescriptor{MediaType: HelmConfigMediaType,
			Digest: digestBytes([]byte("{}")), Size: 2}, Layers: layers})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func (f *ociRegistryFixture) replaceManifest(raw []byte) {
	f.manifestBytes = raw
	f.manifestDigest = digestBytes(raw)
	f.approval.ManifestDigest = f.manifestDigest
}

func (f *ociRegistryFixture) defaultChallenge() string {
	return fmt.Sprintf(`Bearer realm="%s/token",service="registry.test",scope="repository:%s:pull"`,
		f.server.URL, f.repository)
}

func (f *ociRegistryFixture) serve(writer http.ResponseWriter, request *http.Request) {
	manifestPath := "/v2/" + f.repository + "/manifests/" + f.approval.ManifestDigest
	blobPath := "/v2/" + f.repository + "/blobs/" + f.approval.PackageDigest
	switch request.URL.Path {
	case "/token":
		f.tokenRequests.Add(1)
		username, password, basic := request.BasicAuth()
		authorized := basic && username == "registry-user" && password == "registry-password"
		if f.tokenAuthorization != "" {
			authorized = !basic && request.Header.Get("Authorization") == f.tokenAuthorization
		}
		if request.Method != http.MethodGet || !authorized ||
			request.URL.Query().Get("service") != "registry.test" ||
			request.URL.Query().Get("scope") != "repository:"+f.repository+":pull" || len(request.URL.Query()) != 2 {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		writer.Header().Set("Content-Type", f.tokenMediaType)
		writer.WriteHeader(f.tokenStatus)
		_, _ = writer.Write([]byte(f.tokenBody))
	case manifestPath:
		f.manifestRequests.Add(1)
		if f.redirectManifest {
			http.Redirect(writer, request, f.server.URL+"/redirected", http.StatusTemporaryRedirect)
			return
		}
		if f.rejectRegistryAuthorization(writer, request) {
			return
		}
		if request.Method != http.MethodGet || request.URL.RawQuery != "" || request.Header.Get("Accept") != OCIManifestMediaType {
			http.Error(writer, "bad request", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", f.manifestMediaType)
		writer.Header().Set("Docker-Content-Digest", f.manifestDigest)
		writer.WriteHeader(f.manifestStatus)
		_, _ = writer.Write(f.manifestBytes)
	case blobPath:
		f.blobRequests.Add(1)
		if f.rejectRegistryAuthorization(writer, request) {
			return
		}
		if f.blobRedirectURL != "" {
			writer.Header().Set("Location", f.blobRedirectURL)
			writer.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		if request.Method != http.MethodGet || request.URL.RawQuery != "" || request.Header.Get("Accept") != "application/octet-stream" {
			http.Error(writer, "bad request", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/octet-stream")
		writer.WriteHeader(f.blobStatus)
		_, _ = writer.Write(f.blobBytes)
	default:
		http.NotFound(writer, request)
	}
}

func TestOCIHTTPPackageSourceTransportsBearerCredentialOnlyToExactAuthHost(t *testing.T) {
	fixture := newOCIRegistryFixture(t, true)
	fixture.tokenAuthorization = "Bearer operator-identity-token"
	credential := &OCIRegistryCredential{BearerToken: []byte("operator-identity-token"), AuthHost: fixture.host,
		ExpiresAt: time.Now().UTC().Add(time.Hour)}
	provider := &recordingOCICredentialProvider{host: fixture.host, repository: fixture.repository, credential: credential}
	if _, err := fixture.source(provider).Fetch(t.Context(), fixture.approval); err != nil {
		t.Fatalf("fetch with bearer credential: %v", err)
	}
	if credential.BearerToken != nil || credential.AuthHost != "" {
		t.Fatal("bearer credential was not destroyed")
	}
}

func TestOCIHTTPPackageSourceUsesBasicCredentialOnlyForExactRegistryHost(t *testing.T) {
	fixture := newOCIRegistryFixture(t, false)
	fixture.requireAuth = true
	provider := &recordingOCICredentialProvider{host: fixture.host, repository: fixture.repository,
		credential: &OCIRegistryCredential{Username: []byte("registry-user"), Password: []byte("registry-password"),
			AuthHost: fixture.host, ExpiresAt: time.Now().UTC().Add(time.Hour)}}
	original := fixture.server.Config.Handler
	fixture.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/token" {
			username, password, ok := request.BasicAuth()
			if !ok || username != "registry-user" || password != "registry-password" {
				writer.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
				writer.WriteHeader(http.StatusUnauthorized)
				return
			}
			fixture.requireAuth = false
			defer func() { fixture.requireAuth = true }()
		}
		original.ServeHTTP(writer, request)
	})
	if _, err := fixture.source(provider).Fetch(t.Context(), fixture.approval); err != nil {
		t.Fatalf("fetch exact Basic registry: %v", err)
	}
	if provider.calls.Load() != 1 || fixture.tokenRequests.Load() != 0 {
		t.Fatalf("credential calls=%d token calls=%d", provider.calls.Load(), fixture.tokenRequests.Load())
	}
}

func (f *ociRegistryFixture) rejectRegistryAuthorization(writer http.ResponseWriter, request *http.Request) bool {
	if !f.requireAuth || request.Header.Get("Authorization") == "Bearer "+testOCIToken {
		return false
	}
	challenge := f.challenge
	if challenge == "" {
		challenge = f.defaultChallenge()
	}
	writer.Header().Set("WWW-Authenticate", challenge)
	writer.WriteHeader(http.StatusUnauthorized)
	return true
}

func (f *ociRegistryFixture) source(credentials OCIRegistryCredentialProvider) OCIHTTPPackageSource {
	authHosts := []string(nil)
	if f.requireAuth {
		authHosts = []string{f.host}
	}
	return OCIHTTPPackageSource{Client: f.server.Client(), AllowedRegistryHosts: []string{f.host},
		AllowedAuthHosts: authHosts, Credentials: credentials}
}

func TestOCIHTTPPackageSourceFetchesExactApprovedArtifact(t *testing.T) {
	fixture := newOCIRegistryFixture(t, true)
	credential := &OCIRegistryCredential{Username: []byte("registry-user"), Password: []byte("registry-password"),
		AuthHost: fixture.host, ExpiresAt: time.Now().UTC().Add(time.Hour)}
	provider := &recordingOCICredentialProvider{host: fixture.host, repository: fixture.repository, credential: credential}

	artifact, err := fixture.source(provider).Fetch(context.Background(), fixture.approval)
	if err != nil {
		t.Fatalf("fetch approved chart: %v", err)
	}
	if artifact.ManifestDigest != fixture.approval.ManifestDigest || artifact.PackageDigest != fixture.approval.PackageDigest ||
		!equalBytes(artifact.PackageBytes, fixture.packageBytes) {
		t.Fatalf("unexpected artifact: %#v", artifact)
	}
	if fixture.manifestRequests.Load() != 2 || fixture.blobRequests.Load() != 1 || fixture.tokenRequests.Load() != 1 || provider.calls.Load() != 1 {
		t.Fatalf("unexpected request counts: manifest=%d blob=%d token=%d credential=%d", fixture.manifestRequests.Load(),
			fixture.blobRequests.Load(), fixture.tokenRequests.Load(), provider.calls.Load())
	}
	if credential.Username != nil || credential.Password != nil || !credential.ExpiresAt.IsZero() {
		t.Fatal("credential bytes were not destroyed after the token exchange")
	}
}

func TestOCIHTTPPackageSourceAcceptsHelmManifestWithHeaderOnlyMediaType(t *testing.T) {
	fixture := newOCIRegistryFixture(t, false)
	var manifest map[string]any
	if err := json.Unmarshal(fixture.manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	delete(manifest, "mediaType")
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	fixture.replaceManifest(raw)
	if _, err = fixture.source(nil).Fetch(t.Context(), fixture.approval); err != nil {
		t.Fatalf("fetch Helm-pushed manifest without redundant JSON mediaType: %v", err)
	}
}

func TestOCIHTTPPackageSourceFollowsOneExactCredentialFreeBlobRedirect(t *testing.T) {
	fixture := newOCIRegistryFixture(t, true)
	var redirected atomic.Int64
	redirectServer := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		redirected.Add(1)
		if request.Method != http.MethodGet || request.URL.Path != "/immutable/chart.tgz" ||
			request.URL.Query().Get("signature") != "provider-signed" || len(request.URL.Query()) != 1 ||
			request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" ||
			request.Header.Get("Accept") != "application/octet-stream" {
			http.Error(writer, "unsafe redirect request", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/octet-stream")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(fixture.packageBytes)
	}))
	t.Cleanup(redirectServer.Close)
	redirectHost := strings.TrimPrefix(redirectServer.URL, "https://")
	fixture.blobRedirectURL = redirectServer.URL + "/immutable/chart.tgz?signature=provider-signed"
	credential := &OCIRegistryCredential{Username: []byte("registry-user"), Password: []byte("registry-password"),
		AuthHost: fixture.host, ExpiresAt: time.Now().UTC().Add(time.Hour)}
	provider := &recordingOCICredentialProvider{host: fixture.host, repository: fixture.repository, credential: credential}
	source := fixture.source(provider)
	source.AllowedRedirectHosts = []string{redirectHost}
	artifact, err := source.Fetch(t.Context(), fixture.approval)
	if err != nil || redirected.Load() != 1 || !equalBytes(artifact.PackageBytes, fixture.packageBytes) {
		t.Fatalf("redirected fetch error=%v requests=%d bytes=%d", err, redirected.Load(), len(artifact.PackageBytes))
	}
}

func TestOCIHTTPPackageSourceRejectsUnsafeBlobRedirects(t *testing.T) {
	tests := []struct {
		name     string
		location func(*httptest.Server) string
		allow    func(*httptest.Server) []string
		handler  http.Handler
	}{
		{name: "unapproved host", location: func(server *httptest.Server) string { return server.URL + "/chart" }},
		{name: "http", location: func(server *httptest.Server) string {
			return strings.Replace(server.URL, "https://", "http://", 1) + "/chart"
		},
			allow: func(server *httptest.Server) []string { return []string{strings.TrimPrefix(server.URL, "https://")} }},
		{name: "userinfo", location: func(server *httptest.Server) string {
			return strings.Replace(server.URL, "https://", "https://user@", 1) + "/chart"
		},
			allow: func(server *httptest.Server) []string { return []string{strings.TrimPrefix(server.URL, "https://")} }},
		{name: "fragment", location: func(server *httptest.Server) string { return server.URL + "/chart#fragment" },
			allow: func(server *httptest.Server) []string { return []string{strings.TrimPrefix(server.URL, "https://")} }},
		{name: "redirect chain", location: func(server *httptest.Server) string { return server.URL + "/chart" },
			allow: func(server *httptest.Server) []string { return []string{strings.TrimPrefix(server.URL, "https://")} },
			handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				http.Redirect(writer, request, "/second", http.StatusTemporaryRedirect)
			})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newOCIRegistryFixture(t, false)
			handler := test.handler
			if handler == nil {
				handler = http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusOK) })
			}
			redirectServer := httptest.NewTLSServer(handler)
			t.Cleanup(redirectServer.Close)
			fixture.blobRedirectURL = test.location(redirectServer)
			source := fixture.source(nil)
			if test.allow != nil {
				source.AllowedRedirectHosts = test.allow(redirectServer)
			}
			if _, err := source.Fetch(t.Context(), fixture.approval); !errors.Is(err, ErrOCIUnavailable) {
				t.Fatalf("Fetch() error = %v, want %v", err, ErrOCIUnavailable)
			}
		})
	}
}

func TestOCIHTTPPackageSourceRejectsUnapprovedRegistryBehavior(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ociRegistryFixture, *OCIHTTPPackageSource)
		want   error
	}{
		{name: "manifest header digest", mutate: func(f *ociRegistryFixture, _ *OCIHTTPPackageSource) {
			f.manifestDigest = digestBytes([]byte("different"))
		}, want: ErrUnsafeChart},
		{name: "manifest media type", mutate: func(f *ociRegistryFixture, _ *OCIHTTPPackageSource) {
			f.manifestMediaType = "application/json"
		}, want: ErrUnsafeChart},
		{name: "embedded manifest media type", mutate: func(f *ociRegistryFixture, _ *OCIHTTPPackageSource) {
			var manifest map[string]any
			if err := json.Unmarshal(f.manifestBytes, &manifest); err != nil {
				t.Fatal(err)
			}
			manifest["mediaType"] = "application/json"
			raw, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			f.replaceManifest(raw)
		}, want: ErrUnsafeChart},
		{name: "extra chart layer", mutate: func(f *ociRegistryFixture, _ *OCIHTTPPackageSource) {
			layer := ociDescriptor{MediaType: HelmChartMediaType, Digest: f.approval.PackageDigest, Size: int64(len(f.packageBytes))}
			f.replaceManifest(marshalOCIManifest(t, "", []ociDescriptor{layer, layer}))
		}, want: ErrUnsafeChart},
		{name: "artifact type", mutate: func(f *ociRegistryFixture, _ *OCIHTTPPackageSource) {
			layer := ociDescriptor{MediaType: HelmChartMediaType, Digest: f.approval.PackageDigest, Size: int64(len(f.packageBytes))}
			f.replaceManifest(marshalOCIManifest(t, "application/vnd.example.chart", []ociDescriptor{layer}))
		}, want: ErrUnsafeChart},
		{name: "wrong layer identity", mutate: func(f *ociRegistryFixture, _ *OCIHTTPPackageSource) {
			layer := ociDescriptor{MediaType: HelmChartMediaType, Digest: digestBytes([]byte("other package")), Size: int64(len(f.packageBytes))}
			f.replaceManifest(marshalOCIManifest(t, "", []ociDescriptor{layer}))
		}, want: ErrUnsafeChart},
		{name: "wrong blob content", mutate: func(f *ociRegistryFixture, _ *OCIHTTPPackageSource) {
			f.blobBytes[0] ^= 0xff
		}, want: ErrUnsafeChart},
		{name: "redirect", mutate: func(f *ociRegistryFixture, _ *OCIHTTPPackageSource) {
			f.redirectManifest = true
		}, want: ErrOCIUnavailable},
		{name: "duplicate registry allowlist", mutate: func(f *ociRegistryFixture, source *OCIHTTPPackageSource) {
			source.AllowedRegistryHosts = []string{f.host, f.host}
		}, want: ErrInvalid},
		{name: "unsorted registry allowlist", mutate: func(f *ociRegistryFixture, source *OCIHTTPPackageSource) {
			source.AllowedRegistryHosts = []string{"z.example.com", f.host}
		}, want: ErrInvalid},
		{name: "duplicate redirect allowlist", mutate: func(_ *ociRegistryFixture, source *OCIHTTPPackageSource) {
			source.AllowedRedirectHosts = []string{"cdn.example.com", "cdn.example.com"}
		}, want: ErrInvalid},
		{name: "unsorted redirect allowlist", mutate: func(_ *ociRegistryFixture, source *OCIHTTPPackageSource) {
			source.AllowedRedirectHosts = []string{"z.example.com", "a.example.com"}
		}, want: ErrInvalid},
		{name: "oversized redirect allowlist", mutate: func(_ *ociRegistryFixture, source *OCIHTTPPackageSource) {
			source.AllowedRedirectHosts = make([]string, maximumOCIHostCount+1)
			for index := range source.AllowedRedirectHosts {
				source.AllowedRedirectHosts[index] = fmt.Sprintf("%03d.example.com", index)
			}
		}, want: ErrInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newOCIRegistryFixture(t, false)
			source := fixture.source(nil)
			test.mutate(fixture, &source)
			_, err := source.Fetch(context.Background(), fixture.approval)
			if !errors.Is(err, test.want) {
				t.Fatalf("Fetch() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestOCIHTTPPackageSourceRejectsUnsafeBearerExchange(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ociRegistryFixture, *OCIHTTPPackageSource, *recordingOCICredentialProvider)
	}{
		{name: "unapproved auth host", mutate: func(_ *ociRegistryFixture, source *OCIHTTPPackageSource, _ *recordingOCICredentialProvider) {
			source.AllowedAuthHosts = []string{"auth.example.com"}
		}},
		{name: "wrong scope", mutate: func(f *ociRegistryFixture, _ *OCIHTTPPackageSource, _ *recordingOCICredentialProvider) {
			f.challenge = fmt.Sprintf(`Bearer realm="%s/token",service="registry.test",scope="repository:other/sample:pull"`, f.server.URL)
		}},
		{name: "unknown challenge parameter", mutate: func(f *ociRegistryFixture, _ *OCIHTTPPackageSource, _ *recordingOCICredentialProvider) {
			f.challenge = f.defaultChallenge() + `,account="caller-controlled"`
		}},
		{name: "auth realm query", mutate: func(f *ociRegistryFixture, _ *OCIHTTPPackageSource, _ *recordingOCICredentialProvider) {
			f.challenge = fmt.Sprintf(`Bearer realm="%s/token?audience=other",service="registry.test",scope="repository:%s:pull"`, f.server.URL, f.repository)
		}},
		{name: "trailing challenge comma", mutate: func(f *ociRegistryFixture, _ *OCIHTTPPackageSource, _ *recordingOCICredentialProvider) {
			f.challenge = f.defaultChallenge() + ","
		}},
		{name: "non-json token", mutate: func(f *ociRegistryFixture, _ *OCIHTTPPackageSource, _ *recordingOCICredentialProvider) {
			f.tokenMediaType = "text/plain"
		}},
		{name: "ambiguous tokens", mutate: func(f *ociRegistryFixture, _ *OCIHTTPPackageSource, _ *recordingOCICredentialProvider) {
			f.tokenBody = `{"token":"` + testOCIToken + `","access_token":"` + testOCIToken + `"}`
		}},
		{name: "control in credential", mutate: func(_ *ociRegistryFixture, _ *OCIHTTPPackageSource, provider *recordingOCICredentialProvider) {
			provider.credential.Username = []byte("registry\nuser")
		}},
		{name: "credential auth host substitution", mutate: func(_ *ociRegistryFixture, _ *OCIHTTPPackageSource, provider *recordingOCICredentialProvider) {
			provider.credential.AuthHost = "other.example.com"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newOCIRegistryFixture(t, true)
			provider := &recordingOCICredentialProvider{host: fixture.host, repository: fixture.repository,
				credential: &OCIRegistryCredential{Username: []byte("registry-user"), Password: []byte("registry-password"),
					AuthHost: fixture.host, ExpiresAt: time.Now().Add(time.Hour)}}
			source := fixture.source(provider)
			test.mutate(fixture, &source, provider)
			_, err := source.Fetch(context.Background(), fixture.approval)
			if !errors.Is(err, ErrOCIUnauthorized) {
				t.Fatalf("Fetch() error = %v, want %v", err, ErrOCIUnauthorized)
			}
		})
	}
}

type countingChartPackageSource struct {
	artifact ChartArtifact
	calls    atomic.Int64
}

func (s *countingChartPackageSource) Fetch(context.Context, Approval) (ChartArtifact, error) {
	s.calls.Add(1)
	return cloneChartArtifact(s.artifact), nil
}

func TestCachedChartPackageSourceReturnsIsolatedDigestBoundCopies(t *testing.T) {
	files := testChartFiles()
	packageBytes := packageChart(t, files)
	approval := testApproval(t, packageBytes, files)
	upstream := &countingChartPackageSource{artifact: ChartArtifact{ManifestDigest: approval.ManifestDigest,
		PackageDigest: approval.PackageDigest, PackageBytes: append([]byte(nil), packageBytes...)}}
	cache := &CachedChartPackageSource{Upstream: upstream, MaxBytes: 2 * MaximumChartSize}

	first, err := cache.Fetch(context.Background(), approval)
	if err != nil {
		t.Fatal(err)
	}
	first.PackageBytes[0] ^= 0xff
	second, err := cache.Fetch(context.Background(), approval)
	if err != nil {
		t.Fatal(err)
	}
	if upstream.calls.Load() != 1 || !equalBytes(second.PackageBytes, packageBytes) || &first.PackageBytes[0] == &second.PackageBytes[0] {
		t.Fatalf("cache failed to isolate immutable package copies; calls=%d", upstream.calls.Load())
	}
}
