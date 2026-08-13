package helmapps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	OCIManifestMediaType = "application/vnd.oci.image.manifest.v1+json"
	HelmConfigMediaType  = "application/vnd.cncf.helm.config.v1+json"
	HelmChartMediaType   = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
	maximumOCIManifest   = 1 << 20
	maximumOCIToken      = 16 << 10
	maximumOCIAuthHeader = 8 << 10
	maximumOCIHostCount  = 64
)

var (
	ErrOCIUnauthorized = errors.New("approved OCI registry authorization failed")
	ErrOCIUnavailable  = errors.New("approved OCI registry is unavailable")
)

type OCIRegistryCredential struct {
	Username, Password []byte
	BearerToken        []byte
	AuthHost           string
	ExpiresAt          time.Time
}

func (c *OCIRegistryCredential) Destroy() {
	if c == nil {
		return
	}
	clear(c.Username)
	clear(c.Password)
	clear(c.BearerToken)
	c.Username, c.Password, c.BearerToken, c.AuthHost, c.ExpiresAt = nil, nil, nil, "", time.Time{}
}

func (*OCIRegistryCredential) String() string   { return "<redacted OCI registry credential>" }
func (*OCIRegistryCredential) GoString() string { return "<redacted OCI registry credential>" }
func (*OCIRegistryCredential) LogValue() slog.Value {
	return slog.StringValue("<redacted OCI registry credential>")
}

func (c OCIRegistryCredential) validate(now time.Time) error {
	basic := len(c.Username) > 0 || len(c.Password) > 0
	bearer := len(c.BearerToken) > 0
	if basic == bearer || !validOCIHost(c.AuthHost) ||
		(!c.ExpiresAt.IsZero() && !c.ExpiresAt.After(now)) {
		return ErrInvalid
	}
	if basic && (len(c.Username) < 1 || len(c.Username) > 1024 || len(c.Password) < 1 || len(c.Password) > maximumOCIToken ||
		containsControl(string(c.Username)) || containsControl(string(c.Password))) {
		return ErrInvalid
	}
	if bearer && (len(c.BearerToken) > maximumOCIToken || containsControl(string(c.BearerToken)) ||
		strings.IndexFunc(string(c.BearerToken), func(character rune) bool { return character == ' ' || character == '\t' }) >= 0) {
		return ErrInvalid
	}
	return nil
}

type OCIRegistryCredentialProvider interface {
	AcquireOCIRegistryCredential(context.Context, string, string) (*OCIRegistryCredential, error)
}

// OCIHTTPPackageSource implements the minimal read-only OCI Distribution
// surface required by an approved Helm chart. AllowedRegistryHosts and
// AllowedAuthHosts are exact host[:port] allowlists; redirects are rejected.
// The source requests the approved manifest digest rather than a mutable tag.
type OCIHTTPPackageSource struct {
	Client               *http.Client
	AllowedRegistryHosts []string
	AllowedAuthHosts     []string
	Credentials          OCIRegistryCredentialProvider
}

func (s OCIHTTPPackageSource) Fetch(ctx context.Context, approval Approval) (ChartArtifact, error) {
	if approval.Validate() != nil || ctx == nil || s.validate() != nil {
		return ChartArtifact{}, ErrInvalid
	}
	host, repository, err := splitOCIRepository(approval.OCIRepository)
	if err != nil || !containsExactHost(s.AllowedRegistryHosts, host) {
		return ChartArtifact{}, ErrInvalid
	}
	client := *s.Client
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	if client.Timeout <= 0 || client.Timeout > time.Minute {
		client.Timeout = time.Minute
	}
	session := ociFetchSession{source: s, client: &client, host: host, repository: repository}
	defer session.destroy()
	if s.Credentials != nil {
		session.credential, err = s.Credentials.AcquireOCIRegistryCredential(ctx, host, repository)
		if err != nil {
			return ChartArtifact{}, ErrOCIUnauthorized
		}
		if session.credential != nil && session.credential.validate(time.Now().UTC()) != nil {
			return ChartArtifact{}, ErrOCIUnauthorized
		}
	}
	manifestURL := "https://" + host + "/v2/" + repository + "/manifests/" + url.PathEscape(approval.ManifestDigest)
	manifestBytes, header, err := session.get(ctx, manifestURL, OCIManifestMediaType, maximumOCIManifest)
	if err != nil {
		return ChartArtifact{}, err
	}
	if digestBytes(manifestBytes) != approval.ManifestDigest ||
		header.Get("Docker-Content-Digest") != approval.ManifestDigest ||
		mediaType(header.Get("Content-Type")) != OCIManifestMediaType {
		return ChartArtifact{}, ErrUnsafeChart
	}
	manifest, err := decodeHelmOCIManifest(manifestBytes)
	if err != nil || manifest.Config.MediaType != HelmConfigMediaType ||
		len(manifest.Layers) != 1 || manifest.Layers[0].MediaType != HelmChartMediaType ||
		manifest.Layers[0].Digest != approval.PackageDigest || manifest.Layers[0].Size < 1 ||
		manifest.Layers[0].Size > MaximumChartSize {
		return ChartArtifact{}, ErrUnsafeChart
	}
	blobURL := "https://" + host + "/v2/" + repository + "/blobs/" + url.PathEscape(approval.PackageDigest)
	packageBytes, _, err := session.get(ctx, blobURL, "application/octet-stream", MaximumChartSize)
	if err != nil {
		return ChartArtifact{}, err
	}
	if int64(len(packageBytes)) != manifest.Layers[0].Size || digestBytes(packageBytes) != approval.PackageDigest {
		clear(packageBytes)
		return ChartArtifact{}, ErrUnsafeChart
	}
	return ChartArtifact{ManifestDigest: approval.ManifestDigest,
		PackageDigest: approval.PackageDigest, PackageBytes: packageBytes}, nil
}

func (s OCIHTTPPackageSource) validate() error {
	if s.Client == nil || len(s.AllowedRegistryHosts) < 1 ||
		len(s.AllowedRegistryHosts) > maximumOCIHostCount ||
		len(s.AllowedAuthHosts) > maximumOCIHostCount {
		return ErrInvalid
	}
	for _, hosts := range [][]string{s.AllowedRegistryHosts, s.AllowedAuthHosts} {
		if !sort.StringsAreSorted(hosts) {
			return ErrInvalid
		}
		for index, host := range hosts {
			if !validOCIHost(host) || index > 0 && host == hosts[index-1] {
				return ErrInvalid
			}
		}
	}
	return nil
}

type ociFetchSession struct {
	source     OCIHTTPPackageSource
	client     *http.Client
	host       string
	repository string
	token      []byte
	credential *OCIRegistryCredential
}

func (s *ociFetchSession) destroy() {
	clear(s.token)
	s.token = nil
	if s.credential != nil {
		s.credential.Destroy()
		s.credential = nil
	}
}

func (s *ociFetchSession) get(ctx context.Context, requestURL, accept string, maximum int) ([]byte, http.Header, error) {
	response, err := s.request(ctx, requestURL, accept)
	if err != nil {
		return nil, nil, err
	}
	if response.StatusCode == http.StatusUnauthorized {
		challenge := response.Header.Get("WWW-Authenticate")
		closeBounded(response.Body)
		if err = s.authorize(ctx, challenge); err != nil {
			return nil, nil, err
		}
		response, err = s.request(ctx, requestURL, accept)
		if err != nil {
			return nil, nil, err
		}
	}
	defer closeBounded(response.Body)
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			return nil, nil, ErrOCIUnauthorized
		}
		return nil, nil, fmt.Errorf("%w: registry status %d", ErrOCIUnavailable, response.StatusCode)
	}
	if response.ContentLength > int64(maximum) {
		return nil, nil, ErrUnsafeChart
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, int64(maximum)+1))
	if err != nil {
		return nil, nil, ErrOCIUnavailable
	}
	if len(content) < 1 || len(content) > maximum {
		clear(content)
		return nil, nil, ErrUnsafeChart
	}
	return content, response.Header.Clone(), nil
}

func (s *ociFetchSession) request(ctx context.Context, requestURL, accept string) (*http.Response, error) {
	parsed, err := url.Parse(requestURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host != s.host || parsed.User != nil ||
		parsed.Fragment != "" || parsed.RawQuery != "" {
		return nil, ErrInvalid
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, ErrInvalid
	}
	request.Header.Set("Accept", accept)
	request.Header.Set("User-Agent", "kuberploy-helm-fetcher/"+ProtectedGitPolicy)
	if len(s.token) != 0 {
		request.Header.Set("Authorization", "Bearer "+string(s.token))
	} else if s.credential != nil && len(s.credential.Username) != 0 && s.credential.AuthHost == s.host {
		request.SetBasicAuth(string(s.credential.Username), string(s.credential.Password))
	}
	response, err := s.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrOCIUnavailable
	}
	return response, nil
}

func (s *ociFetchSession) authorize(ctx context.Context, header string) error {
	if len(header) < len("Bearer ") || len(header) > maximumOCIAuthHeader ||
		!strings.EqualFold(header[:len("Bearer ")], "Bearer ") {
		return ErrOCIUnauthorized
	}
	parameters, err := parseOCIAuthParameters(header[len("Bearer "):])
	if err != nil || parameters["realm"] == "" || parameters["scope"] != "repository:"+s.repository+":pull" {
		return ErrOCIUnauthorized
	}
	for key := range parameters {
		switch key {
		case "realm", "scope", "service":
		default:
			return ErrOCIUnauthorized
		}
	}
	realm, err := url.Parse(parameters["realm"])
	if err != nil || realm.Scheme != "https" || realm.User != nil || realm.Fragment != "" ||
		realm.Opaque != "" || realm.RawQuery != "" || !containsExactHost(s.source.AllowedAuthHosts, realm.Host) {
		return ErrOCIUnauthorized
	}
	query := realm.Query()
	if service := parameters["service"]; service != "" {
		query.Set("service", service)
	}
	query.Set("scope", parameters["scope"])
	realm.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return ErrOCIUnauthorized
	}
	request.Header.Set("Accept", "application/json")
	if s.credential != nil {
		if s.credential.AuthHost != realm.Host {
			return ErrOCIUnauthorized
		}
		if len(s.credential.BearerToken) != 0 {
			request.Header.Set("Authorization", "Bearer "+string(s.credential.BearerToken))
		} else {
			request.SetBasicAuth(string(s.credential.Username), string(s.credential.Password))
		}
	}
	response, err := s.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrOCIUnavailable
	}
	defer closeBounded(response.Body)
	if response.StatusCode != http.StatusOK || response.ContentLength > maximumOCIToken ||
		mediaType(response.Header.Get("Content-Type")) != "application/json" {
		return ErrOCIUnauthorized
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maximumOCIToken+1))
	if err != nil || len(raw) < 2 || len(raw) > maximumOCIToken {
		return ErrOCIUnauthorized
	}
	defer clear(raw)
	value, err := decodeStrictJSON(raw)
	object, ok := value.(map[string]any)
	if err != nil || !ok {
		return ErrOCIUnauthorized
	}
	for key := range object {
		switch key {
		case "token", "access_token", "expires_in", "issued_at":
		default:
			return ErrOCIUnauthorized
		}
	}
	token, _ := object["token"].(string)
	accessToken, _ := object["access_token"].(string)
	if (token == "") == (accessToken == "") {
		return ErrOCIUnauthorized
	}
	if token == "" {
		token = accessToken
	}
	if len(token) < 16 || len(token) > maximumOCIToken || containsControl(token) {
		return ErrOCIUnauthorized
	}
	clear(s.token)
	s.token = append([]byte(nil), token...)
	token, accessToken = "", ""
	return nil
}

type ociDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type helmOCIManifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	ArtifactType  string            `json:"artifactType,omitempty"`
	Config        ociDescriptor     `json:"config"`
	Layers        []ociDescriptor   `json:"layers"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

func decodeHelmOCIManifest(raw []byte) (helmOCIManifest, error) {
	if len(raw) < 2 || len(raw) > maximumOCIManifest {
		return helmOCIManifest{}, ErrUnsafeChart
	}
	if _, err := decodeStrictJSON(raw); err != nil {
		return helmOCIManifest{}, ErrUnsafeChart
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var manifest helmOCIManifest
	if err := decoder.Decode(&manifest); err != nil || manifest.SchemaVersion != 2 ||
		(manifest.MediaType != "" && manifest.MediaType != OCIManifestMediaType) || !validOCIDescriptor(manifest.Config, 1<<20) ||
		manifest.ArtifactType != "" ||
		len(manifest.Layers) < 1 || len(manifest.Layers) > 4 ||
		len(manifest.Annotations) > 64 || len(manifest.Config.Annotations) > 64 {
		return helmOCIManifest{}, ErrUnsafeChart
	}
	for _, layer := range manifest.Layers {
		if !validOCIDescriptor(layer, MaximumChartSize) || len(layer.Annotations) > 64 {
			return helmOCIManifest{}, ErrUnsafeChart
		}
	}
	return manifest, nil
}

func validOCIDescriptor(value ociDescriptor, maximum int64) bool {
	return value.MediaType != "" && len(value.MediaType) <= 255 && validDigest(value.Digest) &&
		value.Size > 0 && value.Size <= maximum
}

func splitOCIRepository(repository string) (string, string, error) {
	if !canonicalOCIRepository(repository) {
		return "", "", ErrInvalid
	}
	value := strings.TrimPrefix(repository, "oci://")
	host, path, found := strings.Cut(value, "/")
	if !found || !validOCIHost(host) || path == "" || strings.Contains(path, "//") {
		return "", "", ErrInvalid
	}
	return host, path, nil
}

func validOCIHost(host string) bool {
	if host == "" || len(host) > 255 || strings.ToLower(host) != host ||
		strings.ContainsAny(host, "@/?#[]\\\x00\r\n") {
		return false
	}
	name := host
	if before, after, found := strings.Cut(host, ":"); found {
		name = before
		port, err := strconv.Atoi(after)
		if err != nil || port < 1 || port > 65535 {
			return false
		}
	}
	if name == "" || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") || strings.Contains(name, "..") {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if !(character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-') {
				return false
			}
		}
	}
	return true
}

func containsExactHost(hosts []string, host string) bool {
	index := sort.SearchStrings(hosts, host)
	return index < len(hosts) && hosts[index] == host
}

func parseOCIAuthParameters(value string) (map[string]string, error) {
	result := map[string]string{}
	for len(value) != 0 {
		value = strings.TrimSpace(value)
		keyEnd := strings.IndexByte(value, '=')
		if keyEnd < 1 {
			return nil, ErrOCIUnauthorized
		}
		key := strings.ToLower(strings.TrimSpace(value[:keyEnd]))
		value = value[keyEnd+1:]
		if value == "" || value[0] != '"' || key == "" {
			return nil, ErrOCIUnauthorized
		}
		value = value[1:]
		var parsed strings.Builder
		closed := false
		for index := 0; index < len(value); index++ {
			character := value[index]
			switch character {
			case '\\':
				if index+1 >= len(value) || value[index+1] != '\\' && value[index+1] != '"' {
					return nil, ErrOCIUnauthorized
				}
				index++
				parsed.WriteByte(value[index])
			case '"':
				value = value[index+1:]
				closed = true
				index = len(value)
			default:
				if character < 0x20 || character == 0x7f {
					return nil, ErrOCIUnauthorized
				}
				parsed.WriteByte(character)
			}
		}
		if !closed || parsed.Len() > 4096 {
			return nil, ErrOCIUnauthorized
		}
		if _, duplicate := result[key]; duplicate {
			return nil, ErrOCIUnauthorized
		}
		result[key] = parsed.String()
		value = strings.TrimSpace(value)
		if value == "" {
			break
		}
		if value[0] != ',' {
			return nil, ErrOCIUnauthorized
		}
		value = value[1:]
		if strings.TrimSpace(value) == "" {
			return nil, ErrOCIUnauthorized
		}
	}
	return result, nil
}

func mediaType(value string) string {
	parsed, _, err := mime.ParseMediaType(value)
	if err != nil {
		return ""
	}
	return parsed
}

func closeBounded(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 4<<10))
	_ = body.Close()
}

type cachedChartArtifact struct {
	value    ChartArtifact
	lastUsed uint64
}

// CachedChartPackageSource is a bounded, process-local digest cache. It never
// caches credentials or mutable references and rechecks every approval before
// returning bytes. Evicted package memory is cleared.
type CachedChartPackageSource struct {
	Upstream ChartPackageSource
	MaxBytes int

	mu      sync.Mutex
	entries map[string]cachedChartArtifact
	bytes   int
	clock   uint64
}

func (c *CachedChartPackageSource) Fetch(ctx context.Context, approval Approval) (ChartArtifact, error) {
	if c == nil || c.Upstream == nil || approval.Validate() != nil ||
		c.MaxBytes < MaximumChartSize || c.MaxBytes > 1<<30 {
		return ChartArtifact{}, ErrInvalid
	}
	key := approval.ManifestDigest + "/" + approval.PackageDigest
	c.mu.Lock()
	c.clock++
	if entry, present := c.entries[key]; present && entry.value.ManifestDigest == approval.ManifestDigest &&
		entry.value.PackageDigest == approval.PackageDigest && digestBytes(entry.value.PackageBytes) == approval.PackageDigest {
		entry.lastUsed = c.clock
		c.entries[key] = entry
		result := cloneChartArtifact(entry.value)
		c.mu.Unlock()
		return result, nil
	}
	c.mu.Unlock()
	artifact, err := c.Upstream.Fetch(ctx, approval)
	if err != nil {
		return ChartArtifact{}, err
	}
	if artifact.ManifestDigest != approval.ManifestDigest || artifact.PackageDigest != approval.PackageDigest ||
		len(artifact.PackageBytes) < 1 || len(artifact.PackageBytes) > MaximumChartSize ||
		digestBytes(artifact.PackageBytes) != approval.PackageDigest {
		clear(artifact.PackageBytes)
		return ChartArtifact{}, ErrUnsafeChart
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]cachedChartArtifact)
	}
	c.clock++
	if previous, present := c.entries[key]; present {
		c.bytes -= len(previous.value.PackageBytes)
		clear(previous.value.PackageBytes)
	}
	stored := cloneChartArtifact(artifact)
	c.entries[key] = cachedChartArtifact{value: stored, lastUsed: c.clock}
	c.bytes += len(stored.PackageBytes)
	for c.bytes > c.MaxBytes {
		oldestKey := ""
		oldest := ^uint64(0)
		for candidateKey, candidate := range c.entries {
			if candidate.lastUsed < oldest {
				oldestKey, oldest = candidateKey, candidate.lastUsed
			}
		}
		evicted := c.entries[oldestKey]
		delete(c.entries, oldestKey)
		c.bytes -= len(evicted.value.PackageBytes)
		clear(evicted.value.PackageBytes)
	}
	return cloneChartArtifact(artifact), nil
}

func cloneChartArtifact(value ChartArtifact) ChartArtifact {
	value.PackageBytes = append([]byte(nil), value.PackageBytes...)
	return value
}
