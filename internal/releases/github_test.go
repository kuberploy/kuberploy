package releases

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(status int, body string, headers map[string]string) *http.Response {
	h := make(http.Header)
	for k, v := range headers {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

func validComponentCharts(version, digest string) []domain.ManifestChart {
	names := []string{"kuberploy-argocd", "kuberploy-installer", "kuberploy-builder", "kuberploy-cert-manager", "kuberploy-edge", "kuberploy-external-dns", "kuberploy-external-secrets", "kuberploy-monitoring", "kuberploy-postgresql", "kuberploy-registry", "kuberploy-runtime", "kuberploy-sealed-secrets", "kuberploy-valkey"}
	charts := make([]domain.ManifestChart, 0, len(names))
	for _, name := range names {
		charts = append(charts, domain.ManifestChart{Name: name, Version: version, OCIReference: "ghcr.io/kuberploy/charts/" + name + ":" + version, OCIDigest: digest, Package: name + "-" + version + ".tgz", PackageSHA256: digest})
	}
	return charts
}

func validManifest() domain.ReleaseManifest {
	digest := "sha256:" + strings.Repeat("a", 64)
	commit := strings.Repeat("b", 40)
	version := "1.1.0"
	return domain.ReleaseManifest{
		Schema:        "https://raw.githubusercontent.com/kuberploy/kuberploy/" + commit + "/release/release-manifest.schema.json",
		SchemaVersion: "1.0.0",
		Release: domain.ManifestRelease{
			Tag:       "v" + version,
			Version:   version,
			CreatedAt: time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC),
			NotesURL:  "https://github.com/kuberploy/kuberploy/releases/tag/v" + version,
			Summary:   "Safe control-plane update",
		},
		Source:   domain.ManifestSource{Repository: "kuberploy/kuberploy", Commit: commit},
		Versions: domain.ManifestVersions{Kuberploy: version, API: version, Worker: version, Web: version, Migration: version, Upgrader: version, BuilderAgent: version, Chart: version},
		Compatibility: domain.ReleaseCompatibility{
			SupportedUpgradeFrom: ">=1.0.0 <1.1.0",
			Kubernetes:           domain.KubernetesCompatibility{Constraint: ">=1.34.0-0 <1.37.0-0", TestedMinors: []string{"1.34", "1.35", "1.36"}},
			Database: domain.DatabaseCompatibility{
				Engine: "postgresql", CurrentSchema: "002_platform_upgrades", MinimumUpgradeableSchema: "001_initial",
				MigrationSetSHA256: digest, Strategy: "prisma-migrate-deploy-with-advisory-lock",
				RollbackPolicy: "Only roll back to a schema-compatible control-plane release.",
			},
		},
		Artifacts: domain.ManifestArtifacts{
			Images: []domain.ManifestImage{
				{Component: "api", Reference: "ghcr.io/kuberploy/kuberploy-api", Digest: digest, Platforms: []string{"linux/amd64", "linux/arm64"}},
				{Component: "worker", Reference: "ghcr.io/kuberploy/kuberploy-worker", Digest: digest, Platforms: []string{"linux/amd64", "linux/arm64"}},
				{Component: "web", Reference: "ghcr.io/kuberploy/kuberploy-web", Digest: digest, Platforms: []string{"linux/amd64", "linux/arm64"}},
				{Component: "migration", Reference: "ghcr.io/kuberploy/kuberploy-migration", Digest: digest, Platforms: []string{"linux/amd64", "linux/arm64"}},
				{Component: "upgrader", Reference: "ghcr.io/kuberploy/kuberploy-upgrader", Digest: digest, Platforms: []string{"linux/amd64", "linux/arm64"}},
				{Component: "builder-agent", Reference: "ghcr.io/kuberploy/kuberploy-builder-agent", Digest: digest, Platforms: []string{"linux/amd64", "linux/arm64"}},
			},
			Chart:           domain.ManifestChart{Name: "kuberploy", Version: version, OCIReference: "ghcr.io/kuberploy/charts/kuberploy:" + version, OCIDigest: digest, Package: "kuberploy-" + version + ".tgz", PackageSHA256: digest},
			ComponentCharts: validComponentCharts(version, digest),
		},
		DependencyLock: domain.ManifestDependencyLock{File: "DEPENDENCIES.md", SHA256: digest},
	}
}

func githubPayload(t *testing.T, manifest []byte, mutate func(map[string]any)) string {
	t.Helper()
	sum := sha256.Sum256(manifest)
	release := map[string]any{"tag_name": "v1.1.0", "draft": false, "prerelease": false, "immutable": true, "published_at": "2026-08-06T00:00:00Z", "assets": []any{map[string]any{"id": 42, "name": "release-manifest.json", "state": "uploaded", "size": len(manifest), "digest": "sha256:" + hex.EncodeToString(sum[:])}}}
	if mutate != nil {
		mutate(release)
	}
	body, err := json.Marshal(release)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestGitHubCheckerVerifiesImmutableManifestAndConditionalETag(t *testing.T) {
	manifest, err := json.Marshal(validManifest())
	if err != nil {
		t.Fatal(err)
	}
	releaseBody := githubPayload(t, manifest, nil)
	var mu sync.Mutex
	releaseCalls, assetCalls := 0, 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.String() == latestReleaseURL {
			releaseCalls++
			if r.Header.Get("User-Agent") != "kuberploy-release-checker" {
				t.Error("missing fixed user agent")
			}
			if r.Header.Get("X-GitHub-Api-Version") != githubAPIVersion {
				t.Error("release checker used a stale GitHub API version")
			}
			if r.Header.Get("If-None-Match") == `"upstream"` {
				return response(304, "", nil), nil
			}
			return response(200, releaseBody, map[string]string{"ETag": `"upstream"`}), nil
		}
		if r.URL.String() == "https://api.github.com/repos/kuberploy/kuberploy/releases/assets/42" {
			assetCalls++
			if r.Header.Get("Accept") != "application/octet-stream" {
				t.Error("manifest request did not require bytes")
			}
			return response(200, string(manifest), nil), nil
		}
		t.Fatalf("unexpected URL %s", r.URL)
		return nil, nil
	})}
	checker := NewGitHubChecker(client)
	result, err := checker.Latest(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Release.Version != "1.1.0" || result.Release.ManifestDigest == "" || result.ETag != `"upstream"` {
		t.Fatalf("result %#v", result)
	}
	if !bytes.Equal(result.Release.ManifestBytes, manifest) {
		t.Fatal("checker did not retain the exact immutable manifest asset bytes")
	}
	cached, err := checker.Latest(context.Background(), `"upstream"`)
	if err != nil {
		t.Fatal(err)
	}
	if !cached.NotModified {
		t.Fatal("expected conditional not-modified result")
	}
	if releaseCalls != 2 || assetCalls != 1 {
		t.Fatalf("release calls=%d asset calls=%d", releaseCalls, assetCalls)
	}
}

func TestGitHubCheckerRejectsUntrustedReleaseStates(t *testing.T) {
	manifest, _ := json.Marshal(validManifest())
	tests := map[string]func(map[string]any){"draft": func(v map[string]any) { v["draft"] = true }, "prerelease": func(v map[string]any) { v["prerelease"] = true }, "mutable": func(v map[string]any) { v["immutable"] = false }, "unstable-tag": func(v map[string]any) { v["tag_name"] = "v1.1.0-rc.1" }}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return response(200, githubPayload(t, manifest, mutate), nil), nil
			})}
			if _, err := NewGitHubChecker(client).Latest(context.Background(), ""); err == nil {
				t.Fatal("expected release rejection")
			}
		})
	}
}

func TestGitHubCheckerDistinguishesMissingStableRelease(t *testing.T) {
	checker := NewGitHubChecker(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != latestReleaseURL {
			t.Fatalf("unexpected URL %s", r.URL)
		}
		return response(http.StatusNotFound, `{"message":"Not Found"}`, nil), nil
	})})
	if _, err := checker.Latest(context.Background(), ""); !errors.Is(err, ErrNoStableRelease) {
		t.Fatalf("missing stable release error=%v", err)
	}
}

func TestGitHubCheckerRejectsDigestMismatchAndUnknownManifestFields(t *testing.T) {
	base, _ := json.Marshal(validManifest())
	t.Run("digest", func(t *testing.T) {
		body := githubPayload(t, base, func(v map[string]any) {
			assets := v["assets"].([]any)
			assets[0].(map[string]any)["digest"] = "sha256:" + strings.Repeat("0", 64)
		})
		client := twoResponseClient(body, string(base))
		if _, err := NewGitHubChecker(client).Latest(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "SHA-256") {
			t.Fatalf("expected digest error, got %v", err)
		}
	})
	t.Run("closed-schema", func(t *testing.T) {
		var raw map[string]any
		_ = json.Unmarshal(base, &raw)
		raw["unexpected"] = true
		changed, _ := json.Marshal(raw)
		client := twoResponseClient(githubPayload(t, changed, nil), string(changed))
		if _, err := NewGitHubChecker(client).Latest(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("expected closed schema error, got %v", err)
		}
	})
}

func twoResponseClient(release, manifest string) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == latestReleaseURL {
			return response(200, release, nil), nil
		}
		return response(200, manifest, nil), nil
	})}
}

type countingChecker struct {
	calls  int
	result FetchResult
}

func (c *countingChecker) Latest(context.Context, string) (FetchResult, error) {
	c.calls++
	if c.calls > 1 {
		return FetchResult{NotModified: true, ETag: c.result.ETag}, nil
	}
	return c.result, nil
}
func TestServiceCachesAndRevalidatesWithETag(t *testing.T) {
	checker := &countingChecker{result: FetchResult{Release: domain.ReleaseInfo{Version: "1.1.0"}, ETag: `"one"`}}
	svc := NewService(checker, nil, time.Minute)
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	first, err := svc.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if checker.calls != 1 {
		t.Fatal(checker.calls)
	}
	if _, err = svc.Latest(context.Background()); err != nil || checker.calls != 1 {
		t.Fatalf("cache miss err=%v calls=%d", err, checker.calls)
	}
	now = now.Add(2 * time.Minute)
	second, err := svc.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if checker.calls != 2 || !second.LastCheckedAt.After(first.LastCheckedAt) {
		t.Fatalf("revalidation failed %#v %#v", first, second)
	}
}

func TestExplicitRCManifestAndOrderingAreAcceptedWithoutWeakeningStableRanges(t *testing.T) {
	manifest := validManifest()
	version := "1.1.0-rc.153"
	manifest.Release.Version = version
	manifest.Release.Tag = "v" + version
	manifest.Release.NotesURL = "https://github.com/kuberploy/kuberploy/releases/tag/v" + version
	manifest.Versions = domain.ManifestVersions{Kuberploy: version, API: version, Worker: version, Web: version, Migration: version, Upgrader: version, BuilderAgent: version, Chart: version}
	manifest.Artifacts.Chart.Version = version
	manifest.Artifacts.Chart.OCIReference = "ghcr.io/kuberploy/charts/kuberploy:" + version
	manifest.Artifacts.Chart.Package = "kuberploy-" + version + ".tgz"
	for index := range manifest.Artifacts.ComponentCharts {
		chart := &manifest.Artifacts.ComponentCharts[index]
		chart.Version = version
		chart.OCIReference = "ghcr.io/kuberploy/charts/" + chart.Name + ":" + version
		chart.Package = chart.Name + "-" + version + ".tgz"
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if _, err = ParseExactManifest(raw, "sha256:"+hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("explicit RC manifest rejected: %v", err)
	}
	for _, comparison := range []struct {
		a, b string
		want int
	}{{"1.1.0-rc.152", "1.1.0-rc.153", -1}, {"1.1.0-rc.153", "1.1.0", -1}, {"1.1.0", "1.1.0-rc.153", 1}} {
		got, compareErr := CompareVersions(comparison.a, comparison.b)
		if compareErr != nil || got != comparison.want {
			t.Fatalf("CompareVersions(%q,%q)=%d,%v want %d", comparison.a, comparison.b, got, compareErr, comparison.want)
		}
	}
	if compatible, rangeErr := SupportsUpgrade("0.1.0-rc.152", ">=0.1.0 <0.2.0"); rangeErr != nil || !compatible {
		t.Fatalf("RC qualification range compatible=%v err=%v", compatible, rangeErr)
	}
	for _, invalid := range []string{"1.1.0-rc.0", "1.1.0-beta.1", "1.1.0-rc.1+meta"} {
		if ValidReleaseVersion(invalid) {
			t.Fatalf("unsupported release version %q accepted", invalid)
		}
	}
}
