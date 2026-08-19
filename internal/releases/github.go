package releases

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

const latestReleaseURL = "https://api.github.com/repos/kuberploy/kuberploy/releases/latest"
const releaseListURL = "https://api.github.com/repos/kuberploy/kuberploy/releases?per_page=100"
const maxReleaseBytes = int64(1 << 20)
const maxManifestBytes = int64(256 << 10)
const githubAPIVersion = "2026-03-10"

type GitHubChecker struct {
	client  *http.Client
	timeout time.Duration
}

func NewGitHubChecker(client *http.Client) *GitHubChecker {
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 3 {
				return errors.New("too many GitHub redirects")
			}
			switch strings.ToLower(req.URL.Hostname()) {
			case "api.github.com", "github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com":
				return nil
			default:
				return fmt.Errorf("refusing release redirect to %s", req.URL.Hostname())
			}
		}}
	}
	return &GitHubChecker{client: client, timeout: 8 * time.Second}
}

type githubRelease struct {
	TagName     string        `json:"tag_name"`
	Draft       bool          `json:"draft"`
	Prerelease  bool          `json:"prerelease"`
	Immutable   bool          `json:"immutable"`
	PublishedAt time.Time     `json:"published_at"`
	Assets      []githubAsset `json:"assets"`
}
type githubAsset struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	State  string `json:"state"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

func (g *GitHubChecker) Latest(ctx context.Context, cachedETag string) (FetchResult, error) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestReleaseURL, nil)
	if err != nil {
		return FetchResult{}, err
	}
	setGitHubHeaders(req)
	if cachedETag != "" {
		req.Header.Set("If-None-Match", cachedETag)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return FetchResult{}, fmt.Errorf("fetch canonical GitHub release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return FetchResult{ETag: cachedETag, NotModified: true}, nil
	}
	if resp.StatusCode == http.StatusNotFound {
		return g.latestStableFromList(ctx, "")
	}
	if resp.StatusCode != http.StatusOK {
		return FetchResult{}, fmt.Errorf("canonical GitHub release returned HTTP %d", resp.StatusCode)
	}
	body, err := readBounded(resp.Body, maxReleaseBytes)
	if err != nil {
		return FetchResult{}, fmt.Errorf("read GitHub release: %w", err)
	}
	var release githubRelease
	if err = json.Unmarshal(body, &release); err != nil {
		return FetchResult{}, fmt.Errorf("decode GitHub release: %w", err)
	}
	version := strings.TrimPrefix(release.TagName, "v")
	if !ValidStableVersion(version) || release.Draft || release.Prerelease {
		// GitHub's /releases/latest contract excludes prereleases, but a
		// malformed or manually reclassified RC can still be returned there.
		// Consult the ordered release list so an older real stable release is
		// not hidden behind that bad metadata.
		return g.latestStableFromList(ctx, resp.Header.Get("ETag"))
	}
	return g.verifyStableRelease(ctx, release, resp.Header.Get("ETag"))
}

func (g *GitHubChecker) latestStableFromList(ctx context.Context, etag string) (FetchResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseListURL, nil)
	if err != nil {
		return FetchResult{}, err
	}
	setGitHubHeaders(req)
	resp, err := g.client.Do(req)
	if err != nil {
		return FetchResult{}, fmt.Errorf("fetch canonical GitHub release list: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return FetchResult{}, fmt.Errorf("canonical GitHub release list returned HTTP %d", resp.StatusCode)
	}
	body, err := readBounded(resp.Body, maxReleaseBytes*4)
	if err != nil {
		return FetchResult{}, fmt.Errorf("read GitHub release list: %w", err)
	}
	var releases []githubRelease
	if err = json.Unmarshal(body, &releases); err != nil {
		return FetchResult{}, fmt.Errorf("decode GitHub release list: %w", err)
	}
	for _, release := range releases {
		version := strings.TrimPrefix(release.TagName, "v")
		if !ValidStableVersion(version) || release.Draft || release.Prerelease {
			continue
		}
		return g.verifyStableRelease(ctx, release, etag)
	}
	return FetchResult{}, ErrNoStableRelease
}

func (g *GitHubChecker) verifyStableRelease(ctx context.Context, release githubRelease, etag string) (FetchResult, error) {
	version := strings.TrimPrefix(release.TagName, "v")
	if !release.Immutable || release.PublishedAt.IsZero() {
		return FetchResult{}, errors.New("latest GitHub release is not a published immutable stable release")
	}
	var asset *githubAsset
	for i := range release.Assets {
		if release.Assets[i].Name == "release-manifest.json" {
			if asset != nil {
				return FetchResult{}, errors.New("release has multiple release-manifest.json assets")
			}
			asset = &release.Assets[i]
		}
	}
	if asset == nil || asset.ID <= 0 || asset.State != "uploaded" || asset.Size <= 0 || asset.Size > maxManifestBytes || !ManifestDigestValid(asset.Digest) {
		return FetchResult{}, errors.New("release-manifest.json asset metadata is missing or invalid")
	}
	manifestURL := "https://api.github.com/repos/kuberploy/kuberploy/releases/assets/" + strconv.FormatInt(asset.ID, 10)
	manifestReq, err := http.NewRequestWithContext(ctx, http.MethodGet, manifestURL, nil)
	if err != nil {
		return FetchResult{}, err
	}
	setGitHubHeaders(manifestReq)
	manifestReq.Header.Set("Accept", "application/octet-stream")
	manifestResp, err := g.client.Do(manifestReq)
	if err != nil {
		return FetchResult{}, fmt.Errorf("fetch release manifest: %w", err)
	}
	defer manifestResp.Body.Close()
	if manifestResp.StatusCode != http.StatusOK {
		return FetchResult{}, fmt.Errorf("release manifest returned HTTP %d", manifestResp.StatusCode)
	}
	manifestBytes, err := readBounded(manifestResp.Body, maxManifestBytes)
	if err != nil {
		return FetchResult{}, fmt.Errorf("read release manifest: %w", err)
	}
	if int64(len(manifestBytes)) != asset.Size {
		return FetchResult{}, errors.New("release manifest byte length does not match immutable asset metadata")
	}
	sum := sha256.Sum256(manifestBytes)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if digest != asset.Digest {
		return FetchResult{}, errors.New("release manifest SHA-256 does not match immutable asset digest")
	}
	manifest, err := ParseExactManifest(manifestBytes, digest)
	if err != nil {
		return FetchResult{}, err
	}
	if manifest.Release.Version != version || manifest.Release.Tag != release.TagName {
		return FetchResult{}, errors.New("release tag and manifest version do not match")
	}
	return FetchResult{Release: domain.ReleaseInfo{Tag: release.TagName, Version: version, ManifestDigest: digest, Manifest: manifest, ManifestBytes: append([]byte(nil), manifestBytes...), PublishedAt: release.PublishedAt.UTC()}, ETag: etag}, nil
}

func setGitHubHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	req.Header.Set("User-Agent", "kuberploy-release-checker")
}
func readBounded(r io.Reader, max int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("response exceeds %d bytes", max)
	}
	return body, nil
}
