package helmapps

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
)

type helmRepositoryFixture struct {
	server        *httptest.Server
	host          string
	packageBytes  []byte
	indexYAML     string
	indexStatus   int
	chartStatus   int
	indexRedirect string
	chartRedirect string
	indexRequests atomic.Int64
	chartRequests atomic.Int64
}

func newHelmRepositoryFixture(t *testing.T) *helmRepositoryFixture {
	t.Helper()
	fixture := &helmRepositoryFixture{
		packageBytes: []byte("immutable classic Helm chart package"),
		indexStatus:  http.StatusOK,
		chartStatus:  http.StatusOK,
	}
	fixture.server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/stable/index.yaml":
			fixture.indexRequests.Add(1)
			if fixture.indexRedirect != "" {
				http.Redirect(writer, request, fixture.indexRedirect, http.StatusFound)
				return
			}
			writer.WriteHeader(fixture.indexStatus)
			_, _ = writer.Write([]byte(fixture.indexYAML))
		case "/stable/sample-1.2.3.tgz":
			fixture.chartRequests.Add(1)
			if fixture.chartRedirect != "" {
				http.Redirect(writer, request, fixture.chartRedirect, http.StatusTemporaryRedirect)
				return
			}
			writer.WriteHeader(fixture.chartStatus)
			_, _ = writer.Write(fixture.packageBytes)
		default:
			http.NotFound(writer, request)
		}
	}))
	fixture.host = strings.TrimPrefix(fixture.server.URL, "https://")
	fixture.setIndex("sample-1.2.3.tgz", digestBytes(fixture.packageBytes), "1.2.3")
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (f *helmRepositoryFixture) setIndex(chartURL, digest, version string) {
	f.indexYAML = fmt.Sprintf(`apiVersion: v1
entries:
  sample:
    - name: sample
      version: %s
      urls:
        - %s
      digest: %s
`, version, chartURL, digest)
}

func (f *helmRepositoryFixture) source() HelmRepositoryChartSource {
	return HelmRepositoryChartSource{RepositoryURL: f.server.URL + "/stable", ChartName: "sample", Version: "1.2.3"}
}

func (f *helmRepositoryFixture) resolver(hosts ...string) HelmRepositoryResolver {
	if len(hosts) == 0 {
		hosts = []string{f.host}
	}
	sort.Strings(hosts)
	return HelmRepositoryResolver{Client: f.server.Client(), AllowedHosts: hosts}
}

func TestHelmRepositoryResolverFetchesExactImmutableChart(t *testing.T) {
	fixture := newHelmRepositoryFixture(t)
	resolved, err := fixture.resolver().Resolve(t.Context(), fixture.source())
	if err != nil {
		t.Fatalf("resolve classic repository chart: %v", err)
	}
	expectedPackageDigest := digestBytes(fixture.packageBytes)
	if resolved.Artifact.ManifestDigest != digestBytes([]byte(fixture.indexYAML)) ||
		resolved.Artifact.PackageDigest != expectedPackageDigest ||
		!equalBytes(resolved.Artifact.PackageBytes, fixture.packageBytes) {
		t.Fatalf("unexpected artifact: %#v", resolved.Artifact)
	}
	if resolved.Source.RepositoryURL != fixture.source().RepositoryURL || resolved.Source.ChartName != "sample" ||
		resolved.Source.Version != "1.2.3" || resolved.Source.ChartURL != fixture.server.URL+"/stable/sample-1.2.3.tgz" ||
		resolved.Source.IndexDigest != resolved.Artifact.ManifestDigest || resolved.Source.PackageDigest != expectedPackageDigest {
		t.Fatalf("unexpected resolved source: %#v", resolved.Source)
	}
	if fixture.indexRequests.Load() != 1 || fixture.chartRequests.Load() != 1 {
		t.Fatalf("requests index=%d chart=%d", fixture.indexRequests.Load(), fixture.chartRequests.Load())
	}

	fixture.setIndex("sample-1.2.3.tgz", "", "1.2.3")
	if _, err = fixture.resolver().Resolve(t.Context(), fixture.source()); err != nil {
		t.Fatalf("optional index digest rejected: %v", err)
	}
}

func TestHelmRepositoryResolverRejectsWrongVersionAndDigest(t *testing.T) {
	fixture := newHelmRepositoryFixture(t)
	fixture.setIndex("sample-1.2.3.tgz", digestBytes(fixture.packageBytes), "1.2.4")
	if _, err := fixture.resolver().Resolve(t.Context(), fixture.source()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong version error=%v", err)
	}
	fixture.setIndex("sample-1.2.3.tgz", "sha256:"+strings.Repeat("a", 64), "1.2.3")
	if _, err := fixture.resolver().Resolve(t.Context(), fixture.source()); !errors.Is(err, ErrUnsafeChart) {
		t.Fatalf("wrong digest error=%v", err)
	}
}

func TestHelmRepositoryResolverRejectsRedirectsAndHostEscape(t *testing.T) {
	fixture := newHelmRepositoryFixture(t)
	fixture.indexRedirect = fixture.server.URL + "/stable/index.yaml"
	if _, err := fixture.resolver().Resolve(t.Context(), fixture.source()); !errors.Is(err, ErrUnsafeChart) {
		t.Fatalf("index redirect error=%v", err)
	}
	fixture.indexRedirect = ""
	fixture.chartRedirect = fixture.server.URL + "/stable/sample-1.2.3.tgz"
	if _, err := fixture.resolver().Resolve(t.Context(), fixture.source()); !errors.Is(err, ErrUnsafeChart) {
		t.Fatalf("chart redirect error=%v", err)
	}
	fixture.chartRedirect = ""
	escape := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("escaped chart host received request")
	}))
	t.Cleanup(escape.Close)
	fixture.setIndex(escape.URL+"/sample-1.2.3.tgz", digestBytes(fixture.packageBytes), "1.2.3")
	if _, err := fixture.resolver().Resolve(t.Context(), fixture.source()); !errors.Is(err, ErrUnsafeChart) {
		t.Fatalf("host escape error=%v", err)
	}
}

func TestHelmRepositoryResolverRejectsOversizedContent(t *testing.T) {
	fixture := newHelmRepositoryFixture(t)
	original := fixture.server.Config.Handler
	fixture.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/stable/sample-1.2.3.tgz" {
			writer.Header().Set("Content-Length", fmt.Sprint(MaximumChartSize+1))
			writer.WriteHeader(http.StatusOK)
			return
		}
		original.ServeHTTP(writer, request)
	})
	if _, err := fixture.resolver().Resolve(t.Context(), fixture.source()); !errors.Is(err, ErrUnsafeChart) {
		t.Fatalf("oversized package error=%v", err)
	}
}

func TestHelmRepositoryResolverRejectsCredentialsAndTraversal(t *testing.T) {
	fixture := newHelmRepositoryFixture(t)
	source := fixture.source()
	source.RepositoryURL = "https://user:secret@" + fixture.host + "/stable"
	if _, err := fixture.resolver().Resolve(t.Context(), source); !errors.Is(err, ErrInvalid) {
		t.Fatalf("embedded repository credential error=%v", err)
	}
	source = fixture.source()
	source.RepositoryURL = fixture.server.URL + "/a/../stable"
	if _, err := fixture.resolver().Resolve(t.Context(), source); !errors.Is(err, ErrInvalid) {
		t.Fatalf("repository traversal error=%v", err)
	}
	for _, chartURL := range []string{
		"https://user:secret@" + fixture.host + "/stable/sample-1.2.3.tgz",
		"../sample-1.2.3.tgz",
		"charts/../sample-1.2.3.tgz",
	} {
		fixture.setIndex(chartURL, digestBytes(fixture.packageBytes), "1.2.3")
		if _, err := fixture.resolver().Resolve(t.Context(), fixture.source()); !errors.Is(err, ErrUnsafeChart) {
			t.Fatalf("unsafe chart URL %q error=%v", chartURL, err)
		}
	}
}
