package helmapps

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	MaximumHelmRepositoryIndexSize  = 2 << 20
	maximumHelmRepositoryHosts      = 64
	maximumHelmRepositoryIndexNodes = 32768
)

// ResolvedHelmRepositorySource records the exact classic repository source
// selected from one immutable index response. PackageDigest is always the
// digest of the downloaded package, even when the index omits its digest.
type ResolvedHelmRepositorySource struct {
	RepositoryURL string
	ChartName     string
	Version       string
	ChartURL      string
	IndexDigest   string
	PackageDigest string
}

// ResolvedHelmRepositoryChart contains package bytes plus exact source
// metadata suitable for a later admission boundary.
type ResolvedHelmRepositoryChart struct {
	Artifact ChartArtifact
	Source   ResolvedHelmRepositorySource
}

// HelmRepositoryResolver resolves classic Helm repository indexes without
// credentials or redirects. AllowedHosts is a sorted exact host[:port]
// allowlist shared by repository and chart package requests.
type HelmRepositoryResolver struct {
	Client       *http.Client
	AllowedHosts []string
}

// HelmRepositoryHTTPResolver keeps the transport boundary explicit for
// callers that name resolvers by protocol.
type HelmRepositoryHTTPResolver = HelmRepositoryResolver

func (r HelmRepositoryResolver) Resolve(ctx context.Context, source HelmRepositoryChartSource) (ResolvedHelmRepositoryChart, error) {
	if ctx == nil || source.Validate() != nil || r.validate() != nil {
		return ResolvedHelmRepositoryChart{}, ErrInvalid
	}
	repositoryURL, err := url.Parse(source.RepositoryURL)
	if err != nil || !containsExactHost(r.AllowedHosts, repositoryURL.Host) {
		return ResolvedHelmRepositoryChart{}, ErrInvalid
	}
	indexURL := *repositoryURL
	indexURL.Path = strings.TrimSuffix(indexURL.Path, "/") + "/index.yaml"

	client := *r.Client
	client.Jar = nil
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	if client.Timeout <= 0 || client.Timeout > time.Minute {
		client.Timeout = time.Minute
	}

	indexBytes, err := helmRepositoryGET(ctx, &client, indexURL.String(), MaximumHelmRepositoryIndexSize)
	if err != nil {
		return ResolvedHelmRepositoryChart{}, err
	}
	indexDigest := digestBytes(indexBytes)
	entry, err := selectHelmRepositoryEntry(indexBytes, source)
	if err != nil {
		return ResolvedHelmRepositoryChart{}, err
	}
	chartURL, err := resolveHelmRepositoryChartURL(&indexURL, entry.URLs, r.AllowedHosts)
	if err != nil {
		return ResolvedHelmRepositoryChart{}, err
	}
	packageBytes, err := helmRepositoryGET(ctx, &client, chartURL, MaximumChartSize)
	if err != nil {
		return ResolvedHelmRepositoryChart{}, err
	}
	packageDigest := digestBytes(packageBytes)
	expectedDigest, err := normalizeHelmRepositoryDigest(entry.Digest)
	if err != nil || expectedDigest != "" && expectedDigest != packageDigest {
		clear(packageBytes)
		return ResolvedHelmRepositoryChart{}, ErrUnsafeChart
	}

	metadata := ResolvedHelmRepositorySource{
		RepositoryURL: source.RepositoryURL,
		ChartName:     source.ChartName,
		Version:       source.Version,
		ChartURL:      chartURL,
		IndexDigest:   indexDigest,
		PackageDigest: packageDigest,
	}
	return ResolvedHelmRepositoryChart{
		Artifact: ChartArtifact{
			ManifestDigest: indexDigest,
			PackageDigest:  packageDigest,
			PackageBytes:   packageBytes,
		},
		Source: metadata,
	}, nil
}

func (r HelmRepositoryResolver) validate() error {
	if r.Client == nil || len(r.AllowedHosts) == 0 || len(r.AllowedHosts) > maximumHelmRepositoryHosts ||
		!sort.StringsAreSorted(r.AllowedHosts) {
		return ErrInvalid
	}
	for index, host := range r.AllowedHosts {
		if !validOCIHost(host) || index > 0 && host == r.AllowedHosts[index-1] {
			return ErrInvalid
		}
	}
	return nil
}

func helmRepositoryGET(ctx context.Context, client *http.Client, requestURL string, maximum int) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil || request.URL.Scheme != "https" || request.URL.User != nil {
		return nil, ErrInvalid
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: classic Helm repository request failed", ErrUnavailable)
	}
	defer closeBounded(response.Body)
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return nil, ErrUnsafeChart
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: classic Helm repository status %d", ErrUnavailable, response.StatusCode)
	}
	if response.ContentLength > int64(maximum) {
		return nil, ErrUnsafeChart
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, int64(maximum)+1))
	if err != nil {
		return nil, fmt.Errorf("%w: classic Helm repository response failed", ErrUnavailable)
	}
	if len(content) == 0 || len(content) > maximum {
		clear(content)
		return nil, ErrUnsafeChart
	}
	return content, nil
}

type helmRepositoryIndex struct {
	APIVersion string                           `yaml:"apiVersion"`
	Entries    map[string][]helmRepositoryEntry `yaml:"entries"`
}

type helmRepositoryEntry struct {
	Name    string   `yaml:"name"`
	Version string   `yaml:"version"`
	URLs    []string `yaml:"urls"`
	Digest  string   `yaml:"digest"`
}

func selectHelmRepositoryEntry(raw []byte, source HelmRepositoryChartSource) (helmRepositoryEntry, error) {
	normalized, err := normalizeDocument(raw, MaximumHelmRepositoryIndexSize)
	if err != nil {
		return helmRepositoryEntry{}, ErrUnsafeChart
	}
	node, err := decodeSingleYAML(normalized, true)
	if err != nil || node.Kind != yaml.MappingNode ||
		validateYAMLTree(node, maximumHelmRepositoryIndexNodes, MaximumYAMLDepth) != nil {
		return helmRepositoryEntry{}, ErrUnsafeChart
	}
	var index helmRepositoryIndex
	if err = node.Decode(&index); err != nil || index.APIVersion != "v1" || index.Entries == nil {
		return helmRepositoryEntry{}, ErrUnsafeChart
	}
	entries, present := index.Entries[source.ChartName]
	if !present {
		return helmRepositoryEntry{}, ErrNotFound
	}
	var selected helmRepositoryEntry
	found := false
	for _, entry := range entries {
		if entry.Version != source.Version {
			continue
		}
		if found || entry.Name != source.ChartName || !validExactChartVersion(entry.Version) || len(entry.URLs) != 1 {
			return helmRepositoryEntry{}, ErrUnsafeChart
		}
		selected, found = entry, true
	}
	if !found {
		return helmRepositoryEntry{}, ErrNotFound
	}
	return selected, nil
}

func resolveHelmRepositoryChartURL(indexURL *url.URL, candidates []string, allowedHosts []string) (string, error) {
	if indexURL == nil || len(candidates) != 1 {
		return "", ErrUnsafeChart
	}
	raw := candidates[0]
	if !boundedExact(raw, MaximumChartSourceRepositoryLength) || containsControl(raw) || strings.Contains(raw, "\\") {
		return "", ErrUnsafeChart
	}
	reference, err := url.Parse(raw)
	if err != nil || reference.Opaque != "" || reference.User != nil || reference.RawQuery != "" || reference.ForceQuery ||
		reference.Fragment != "" || reference.RawPath != "" || reference.Path == "" || reference.Path == "/" ||
		!validURLPath(reference.Path) {
		return "", ErrUnsafeChart
	}
	if reference.Scheme != "" && reference.Scheme != "https" || reference.Scheme == "" && reference.Host != "" {
		return "", ErrUnsafeChart
	}
	resolved := indexURL.ResolveReference(reference)
	if resolved.Scheme != "https" || resolved.User != nil || resolved.Host == "" || resolved.Hostname() == "" ||
		resolved.RawQuery != "" || resolved.ForceQuery || resolved.Fragment != "" || resolved.RawPath != "" ||
		!validURLPath(resolved.Path) || !containsExactHost(allowedHosts, resolved.Host) {
		return "", ErrUnsafeChart
	}
	return resolved.String(), nil
}

func normalizeHelmRepositoryDigest(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if len(value) == 64 && strings.Trim(value, "0123456789abcdef") == "" {
		return "sha256:" + value, nil
	}
	if validDigest(value) {
		return value, nil
	}
	return "", ErrUnsafeChart
}
