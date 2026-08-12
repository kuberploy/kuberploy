package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

const (
	ociIndexMediaType           = "application/vnd.oci.image.index.v1+json"
	ociManifestMediaType        = "application/vnd.oci.image.manifest.v1+json"
	dockerIndexMediaType        = "application/vnd.docker.distribution.manifest.list.v2+json"
	dockerManifestMediaType     = "application/vnd.docker.distribution.manifest.v2+json"
	dockerAttestationMediaType  = "application/vnd.docker.attestation.manifest.v1+json"
	maximumObservedManifestBody = 4 << 20
)

var errRegistryObservation = errors.New("registry Distribution observation is incomplete")

type DistributionObserverConfig struct {
	ExpectedOrigin      string
	AllowPlainHTTP      bool
	RequestTimeout      time.Duration
	PageSize            int
	MaximumPages        int
	MaximumRepositories int
	MaximumTags         int
	MaximumManifests    int
	MaximumBlobs        int
}

func DefaultDistributionObserverConfig() DistributionObserverConfig {
	return DistributionObserverConfig{
		RequestTimeout: 10 * time.Second, PageSize: 100, MaximumPages: 128,
		MaximumRepositories: 4096, MaximumTags: 65536, MaximumManifests: 65536, MaximumBlobs: 262144,
	}
}

func (c DistributionObserverConfig) validate() error {
	parsed, err := url.Parse(c.ExpectedOrigin)
	if err != nil || parsed.String() != c.ExpectedOrigin || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil ||
		(parsed.Scheme != "https" && !(c.AllowPlainHTTP && parsed.Scheme == "http")) ||
		c.RequestTimeout < time.Second || c.RequestTimeout > 30*time.Second || c.PageSize < 1 || c.PageSize > 1000 ||
		c.MaximumPages < 1 || c.MaximumPages > 1024 || c.MaximumRepositories < 1 || c.MaximumRepositories > 65536 ||
		c.MaximumTags < 1 || c.MaximumTags > 1_000_000 || c.MaximumManifests < 1 || c.MaximumManifests > 1_000_000 ||
		c.MaximumBlobs < 1 || c.MaximumBlobs > 4_000_000 {
		return ErrDistributionInvalidConfig
	}
	return nil
}

// DistributionObserver has read-only methods only. Managed and external
// targets may be observed, but this type cannot be converted into a deleter.
type DistributionObserver struct {
	target      domain.RegistryTarget
	baseURL     *url.URL
	credentials DistributionCredentialSource
	http        *http.Client
	config      DistributionObserverConfig
}

func NewDistributionObserver(target domain.RegistryTarget, config DistributionObserverConfig, credentials DistributionCredentialSource, transport http.RoundTripper) (*DistributionObserver, error) {
	if ValidateTarget(target) != nil || !validSafeIdentity(target.ID) || !validRepository(target.RepositoryPrefix) || credentials == nil || config.validate() != nil || target.Endpoint != config.ExpectedOrigin {
		return nil, ErrDistributionInvalidConfig
	}
	baseURL, err := url.Parse(config.ExpectedOrigin)
	if err != nil {
		return nil, ErrDistributionInvalidConfig
	}
	if transport == nil {
		transport = &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: config.RequestTimeout}).DialContext, TLSHandshakeTimeout: config.RequestTimeout}
	}
	client := &http.Client{Timeout: config.RequestTimeout, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect denied") }}
	return &DistributionObserver{target: target, baseURL: baseURL, credentials: credentials, http: client, config: config}, nil
}

func (o *DistributionObserver) Target() domain.RegistryTarget { return o.target }

type distributionDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Data        string            `json:"data,omitempty"`
	Platform    *struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
		Variant      string `json:"variant"`
	} `json:"platform,omitempty"`
}

type distributionManifestDocument struct {
	SchemaVersion int                      `json:"schemaVersion"`
	MediaType     string                   `json:"mediaType"`
	ArtifactType  string                   `json:"artifactType,omitempty"`
	Subject       *distributionDescriptor  `json:"subject,omitempty"`
	Annotations   map[string]string        `json:"annotations,omitempty"`
	Config        distributionDescriptor   `json:"config"`
	Layers        []distributionDescriptor `json:"layers"`
	Manifests     []distributionDescriptor `json:"manifests"`
}

type observedManifest struct {
	digest, mediaType, platformOS, platformArchitecture, platformVariant string
	size                                                                 int64
	kind                                                                 domain.RegistryManifestKind
	children                                                             []distributionDescriptor
	blobs                                                                []distributionDescriptor
}

// Observe returns one closed inventory and one closed catalog for every
// repository returned by _catalog or supplied as an operator/database root
// scope. Distribution cannot enumerate arbitrary untagged revisions, so roots
// must include all durable release/cache/authority roots and prior observed
// manifests. Offline GC uses the separate physical checkpoint, never this API
// observation, as proof of registry-wide blob reachability.
func (o *DistributionObserver) Observe(ctx context.Context, roots map[string][]string, revision int64, observedAt time.Time) (domain.RegistryInventoryObservation, []domain.RegistryCatalogSnapshot, error) {
	if o == nil || revision < 1 || observedAt.IsZero() {
		return domain.RegistryInventoryObservation{}, nil, errRegistryObservation
	}
	repositories, err := o.repositories(ctx)
	if err != nil {
		return domain.RegistryInventoryObservation{}, nil, err
	}
	set := make(map[string]struct{}, len(repositories)+len(roots))
	for _, repository := range repositories {
		set[repository] = struct{}{}
	}
	for repository, digests := range roots {
		if !repositoryInTarget(o.target, repository) {
			return domain.RegistryInventoryObservation{}, nil, ErrDistributionScopeMismatch
		}
		for _, digest := range digests {
			if !validDigest(digest) {
				return domain.RegistryInventoryObservation{}, nil, errRegistryObservation
			}
		}
		set[repository] = struct{}{}
	}
	repositories = repositories[:0]
	for repository := range set {
		if !repositoryInTarget(o.target, repository) {
			return domain.RegistryInventoryObservation{}, nil, ErrDistributionScopeMismatch
		}
		repositories = append(repositories, repository)
	}
	sort.Strings(repositories)
	if len(repositories) > o.config.MaximumRepositories {
		return domain.RegistryInventoryObservation{}, nil, errRegistryObservation
	}
	catalogs := make([]domain.RegistryCatalogSnapshot, 0, len(repositories))
	for _, repository := range repositories {
		catalog, observeErr := o.observeRepository(ctx, repository, roots[repository], revision, observedAt.UTC())
		if observeErr != nil {
			return domain.RegistryInventoryObservation{}, nil, observeErr
		}
		catalogs = append(catalogs, catalog)
	}
	inventory := domain.RegistryInventoryObservation{RegistryTargetID: o.target.ID, Revision: distributionObservationRevision(revision), Complete: true, Repositories: repositories, ObservedAt: observedAt.UTC()}
	return inventory, catalogs, nil
}

func (o *DistributionObserver) repositories(ctx context.Context) ([]string, error) {
	path := "/v2/_catalog?n=" + strconv.Itoa(o.config.PageSize)
	seen := make(map[string]struct{})
	for page := 0; path != ""; page++ {
		if page >= o.config.MaximumPages {
			return nil, errRegistryObservation
		}
		body, header, status, err := o.request(ctx, http.MethodGet, path, "application/json")
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			zeroBytes(body)
			return nil, distributionObservationStatus(status)
		}
		var response struct {
			Repositories []string `json:"repositories"`
		}
		if err = decodeBoundedDistributionJSON(body, &response); err != nil {
			zeroBytes(body)
			return nil, err
		}
		zeroBytes(body)
		for _, repository := range response.Repositories {
			if !repositoryInTarget(o.target, repository) {
				// A Distribution catalog is registry-wide. Repositories used by
				// platform images or another independently managed target are not
				// part of this target's lifecycle graph, so ignore them without
				// issuing any repository-scoped request. Durable roots remain
				// fail-closed in Observe above.
				continue
			}
			seen[repository] = struct{}{}
			if len(seen) > o.config.MaximumRepositories {
				return nil, errRegistryObservation
			}
		}
		path, err = o.nextPage(header.Get("Link"), "/v2/_catalog")
		if err != nil {
			return nil, err
		}
	}
	result := make([]string, 0, len(seen))
	for repository := range seen {
		result = append(result, repository)
	}
	sort.Strings(result)
	return result, nil
}

func (o *DistributionObserver) tags(ctx context.Context, repository string) ([]string, error) {
	basePath := "/v2/" + escapedRepository(repository) + "/tags/list"
	path := basePath + "?n=" + strconv.Itoa(o.config.PageSize)
	seen := make(map[string]struct{})
	for page := 0; path != ""; page++ {
		if page >= o.config.MaximumPages {
			return nil, errRegistryObservation
		}
		body, header, status, err := o.request(ctx, http.MethodGet, path, "application/json")
		if err != nil {
			return nil, err
		}
		if status == http.StatusNotFound {
			zeroBytes(body)
			return nil, nil
		}
		if status != http.StatusOK {
			zeroBytes(body)
			return nil, distributionObservationStatus(status)
		}
		var response struct {
			Name string   `json:"name"`
			Tags []string `json:"tags"`
		}
		if err = decodeBoundedDistributionJSON(body, &response); err != nil || response.Name != repository {
			zeroBytes(body)
			return nil, errRegistryObservation
		}
		zeroBytes(body)
		for _, tag := range response.Tags {
			if !validDistributionTag(tag) {
				return nil, errRegistryObservation
			}
			seen[tag] = struct{}{}
			if len(seen) > o.config.MaximumTags {
				return nil, errRegistryObservation
			}
		}
		path, err = o.nextPage(header.Get("Link"), basePath)
		if err != nil {
			return nil, err
		}
	}
	result := make([]string, 0, len(seen))
	for tag := range seen {
		result = append(result, tag)
	}
	sort.Strings(result)
	return result, nil
}

func validDistributionTag(tag string) bool {
	if tag == "" || len(tag) > 128 || tag[0] == '.' || tag[0] == '-' || strings.ContainsAny(tag, "/\x00\r\n") {
		return false
	}
	for _, r := range tag {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '.' || r == '-') {
			return false
		}
	}
	return true
}

func (o *DistributionObserver) observeRepository(ctx context.Context, repository string, roots []string, revision int64, observedAt time.Time) (domain.RegistryCatalogSnapshot, error) {
	tags, err := o.tags(ctx, repository)
	if err != nil {
		return domain.RegistryCatalogSnapshot{}, err
	}
	queue := append([]string(nil), roots...)
	for _, tag := range tags {
		digest, resolveErr := o.resolveTag(ctx, repository, tag)
		if resolveErr != nil {
			return domain.RegistryCatalogSnapshot{}, resolveErr
		}
		queue = append(queue, digest)
	}
	sort.Strings(queue)
	queue = compactStrings(queue)
	manifests := make(map[string]observedManifest)
	blobs := make(map[string]distributionDescriptor)
	for len(queue) > 0 {
		digest := queue[0]
		queue = queue[1:]
		if _, done := manifests[digest]; done {
			continue
		}
		manifest, present, fetchErr := o.fetchManifest(ctx, repository, digest)
		if fetchErr != nil {
			return domain.RegistryCatalogSnapshot{}, fetchErr
		}
		if !present {
			continue
		}
		manifests[digest] = manifest
		if len(manifests) > o.config.MaximumManifests {
			return domain.RegistryCatalogSnapshot{}, errRegistryObservation
		}
		for _, child := range manifest.children {
			queue = append(queue, child.Digest)
		}
		for _, blob := range manifest.blobs {
			if existing, ok := blobs[blob.Digest]; ok && (existing.Size != blob.Size || existing.MediaType != blob.MediaType) {
				return domain.RegistryCatalogSnapshot{}, errRegistryObservation
			}
			blobs[blob.Digest] = blob
			if len(blobs) > o.config.MaximumBlobs {
				return domain.RegistryCatalogSnapshot{}, errRegistryObservation
			}
		}
		sort.Strings(queue)
		queue = compactStrings(queue)
	}
	for _, blob := range blobs {
		if err = o.confirmBlob(ctx, repository, blob); err != nil {
			return domain.RegistryCatalogSnapshot{}, err
		}
	}
	snapshot := domain.RegistryCatalogSnapshot{Observation: domain.RegistryCatalogObservation{
		RegistryTargetID: o.target.ID, Repository: repository, Revision: revision, Complete: true, ObservedAt: observedAt,
		ManifestCount: len(manifests), BlobCount: len(blobs),
	}}
	digests := make([]string, 0, len(manifests))
	for digest := range manifests {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	for _, digest := range digests {
		manifest := manifests[digest]
		snapshot.Manifests = append(snapshot.Manifests, domain.RegistryManifest{RegistryTargetID: o.target.ID, Repository: repository,
			Digest: digest, Kind: manifest.kind, MediaType: manifest.mediaType, SizeBytes: manifest.size,
			PlatformOS: manifest.platformOS, PlatformArchitecture: manifest.platformArchitecture, PlatformVariant: manifest.platformVariant,
			Present: true, FirstObservedAt: observedAt, LastObservedAt: observedAt, LastObservationRevision: revision})
		for _, child := range manifest.children {
			if _, present := manifests[child.Digest]; !present {
				return domain.RegistryCatalogSnapshot{}, errRegistryObservation
			}
			snapshot.Children = append(snapshot.Children, domain.RegistryManifestLink{Repository: repository, ParentDigest: digest, ChildDigest: child.Digest})
		}
		for _, blob := range manifest.blobs {
			snapshot.BlobLinks = append(snapshot.BlobLinks, domain.RegistryManifestBlobLink{Repository: repository, ManifestDigest: digest, BlobDigest: blob.Digest})
		}
	}
	digests = digests[:0]
	for digest := range blobs {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	for _, digest := range digests {
		blob := blobs[digest]
		snapshot.Blobs = append(snapshot.Blobs, domain.RegistryBlob{RegistryTargetID: o.target.ID, Repository: repository,
			Digest: digest, MediaType: blob.MediaType, SizeBytes: blob.Size, Present: true,
			FirstObservedAt: observedAt, LastObservedAt: observedAt, LastObservationRevision: revision})
	}
	sort.Slice(snapshot.Children, func(i, j int) bool {
		return snapshot.Children[i].ParentDigest+snapshot.Children[i].ChildDigest < snapshot.Children[j].ParentDigest+snapshot.Children[j].ChildDigest
	})
	sort.Slice(snapshot.BlobLinks, func(i, j int) bool {
		return snapshot.BlobLinks[i].ManifestDigest+snapshot.BlobLinks[i].BlobDigest < snapshot.BlobLinks[j].ManifestDigest+snapshot.BlobLinks[j].BlobDigest
	})
	return snapshot, ValidateCatalog(snapshot)
}

func (o *DistributionObserver) resolveTag(ctx context.Context, repository, tag string) (string, error) {
	body, header, status, err := o.request(ctx, http.MethodGet, "/v2/"+escapedRepository(repository)+"/manifests/"+url.PathEscape(tag), distributionManifestAccept)
	defer zeroBytes(body)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", distributionObservationStatus(status)
	}
	digest := header.Get("Docker-Content-Digest")
	if !validDigest(digest) || verifyDistributionDigest(body, digest) != nil {
		return "", errRegistryObservation
	}
	return digest, nil
}

func (o *DistributionObserver) fetchManifest(ctx context.Context, repository, digest string) (observedManifest, bool, error) {
	body, header, status, err := o.request(ctx, http.MethodGet, "/v2/"+escapedRepository(repository)+"/manifests/"+url.PathEscape(digest), distributionManifestAccept)
	defer zeroBytes(body)
	if err != nil {
		return observedManifest{}, false, err
	}
	if status == http.StatusNotFound {
		return observedManifest{}, false, nil
	}
	if status != http.StatusOK || header.Get("Docker-Content-Digest") != digest || verifyDistributionDigest(body, digest) != nil {
		return observedManifest{}, false, distributionObservationStatus(status)
	}
	mediaType := strings.TrimSpace(strings.Split(header.Get("Content-Type"), ";")[0])
	var document distributionManifestDocument
	if err = decodeBoundedDistributionJSON(body, &document); err != nil || document.SchemaVersion != 2 ||
		document.MediaType != "" && document.MediaType != mediaType {
		return observedManifest{}, false, errRegistryObservation
	}
	manifest := observedManifest{digest: digest, mediaType: mediaType, size: int64(len(body))}
	switch mediaType {
	case ociIndexMediaType, dockerIndexMediaType:
		manifest.kind = domain.RegistryManifestIndex
		if len(document.Manifests) == 0 || document.Config.Digest != "" || len(document.Layers) != 0 ||
			document.ArtifactType != "" || document.Subject != nil {
			return observedManifest{}, false, errRegistryObservation
		}
		for _, child := range document.Manifests {
			if !validManifestDescriptor(child, true) {
				return observedManifest{}, false, errRegistryObservation
			}
			manifest.children = append(manifest.children, child)
		}
	case ociManifestMediaType, dockerManifestMediaType:
		manifest.kind = domain.RegistryManifestImage
		if !validBlobDescriptor(document.Config) || len(document.Manifests) != 0 {
			return observedManifest{}, false, errRegistryObservation
		}
		if document.ArtifactType != "" || document.Subject != nil {
			if document.ArtifactType != dockerAttestationMediaType || document.Subject == nil ||
				!validManifestDescriptor(*document.Subject, false) {
				return observedManifest{}, false, errRegistryObservation
			}
			// Docker Buildx publishes provenance as a standard OCI manifest whose
			// subject is the image manifest. Validate the exact subject descriptor,
			// but do not turn the reverse artifact relationship into an OCI index
			// child edge. The enclosing index already retains both manifests.
		}
		manifest.blobs = append(manifest.blobs, document.Config)
		for _, layer := range document.Layers {
			if !validBlobDescriptor(layer) {
				return observedManifest{}, false, errRegistryObservation
			}
			manifest.blobs = append(manifest.blobs, layer)
		}
	default:
		return observedManifest{}, false, errRegistryObservation
	}
	return manifest, true, nil
}

func validManifestDescriptor(descriptor distributionDescriptor, platform bool) bool {
	if !validDigest(descriptor.Digest) || descriptor.Size < 0 || !validDescriptorData(descriptor) ||
		(descriptor.MediaType != ociManifestMediaType && descriptor.MediaType != dockerManifestMediaType && descriptor.MediaType != ociIndexMediaType && descriptor.MediaType != dockerIndexMediaType) {
		return false
	}
	if descriptor.Platform != nil {
		if !platform || len(descriptor.Platform.OS) > 64 || len(descriptor.Platform.Architecture) > 64 || len(descriptor.Platform.Variant) > 64 {
			return false
		}
		// BuildKit's historical local-cache index uses this exact non-runtime
		// marker. It carries no selectable architecture, but its child digest is
		// still an ordinary verified OCI manifest. Keep every other partial
		// platform descriptor fail-closed.
		if descriptor.Platform.OS == "darwin" && descriptor.Platform.Architecture == "" && descriptor.Platform.Variant == "" {
			return true
		}
		if descriptor.Platform.OS == "" || descriptor.Platform.Architecture == "" {
			return false
		}
	}
	return true
}

func validBlobDescriptor(descriptor distributionDescriptor) bool {
	return validDigest(descriptor.Digest) && descriptor.Size >= 0 && validDescriptorData(descriptor) && descriptor.MediaType != "" && len(descriptor.MediaType) <= 256 && !strings.ContainsAny(descriptor.MediaType, "\x00\r\n")
}

func validDescriptorData(descriptor distributionDescriptor) bool {
	if descriptor.Data == "" {
		return true
	}
	if descriptor.Size > maximumObservedManifestBody {
		return false
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(descriptor.Data)
	if err != nil {
		return false
	}
	defer zeroBytes(decoded)
	return int64(len(decoded)) == descriptor.Size && verifyDistributionDigest(decoded, descriptor.Digest) == nil
}

func (o *DistributionObserver) confirmBlob(ctx context.Context, repository string, descriptor distributionDescriptor) error {
	body, header, status, err := o.request(ctx, http.MethodHead, "/v2/"+escapedRepository(repository)+"/blobs/"+url.PathEscape(descriptor.Digest), "")
	zeroBytes(body)
	if err != nil {
		return err
	}
	if status != http.StatusOK || header.Get("Docker-Content-Digest") != "" && header.Get("Docker-Content-Digest") != descriptor.Digest {
		return distributionObservationStatus(status)
	}
	if length := header.Get("Content-Length"); length != "" {
		value, parseErr := strconv.ParseInt(length, 10, 64)
		if parseErr != nil || value != descriptor.Size {
			return errRegistryObservation
		}
	}
	return nil
}

func (o *DistributionObserver) request(ctx context.Context, method, path, accept string) ([]byte, http.Header, int, error) {
	if ctx == nil || (method != http.MethodGet && method != http.MethodHead) || !strings.HasPrefix(path, "/v2/") || strings.ContainsAny(path, "\x00\r\n") {
		return nil, nil, 0, ErrDistributionScopeMismatch
	}
	authorization, err := o.credentials.Authorization(ctx, o.target.ID)
	if err != nil {
		return nil, nil, 0, ErrDistributionCredentialUnavailable
	}
	defer authorization.destroy()
	header, ok := authorization.header()
	if !ok {
		return nil, nil, 0, ErrDistributionCredentialUnavailable
	}
	request, err := http.NewRequestWithContext(ctx, method, o.baseURL.String()+path, nil)
	if err != nil || request.URL.Scheme != o.baseURL.Scheme || request.URL.Host != o.baseURL.Host {
		return nil, nil, 0, ErrDistributionScopeMismatch
	}
	request.Header.Set("Authorization", header)
	request.Header.Set("User-Agent", "kuberploy-registry-observer/1")
	if accept != "" {
		request.Header.Set("Accept", accept)
	}
	response, err := o.http.Do(request)
	if err != nil {
		return nil, nil, 0, &DistributionError{Class: DistributionErrorTransport}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maximumObservedManifestBody+1))
	if err != nil {
		zeroBytes(body)
		return nil, nil, 0, &DistributionError{StatusCode: response.StatusCode, Class: DistributionErrorInvalidResponse}
	}
	if len(body) > maximumObservedManifestBody {
		zeroBytes(body)
		return nil, nil, 0, &DistributionError{StatusCode: response.StatusCode, Class: DistributionErrorResponseTooLarge}
	}
	return body, response.Header.Clone(), response.StatusCode, nil
}

func (o *DistributionObserver) nextPage(link, exactPath string) (string, error) {
	if link == "" {
		return "", nil
	}
	parts := strings.Split(link, ";")
	if len(parts) != 2 || strings.TrimSpace(parts[1]) != `rel="next"` {
		return "", errRegistryObservation
	}
	raw := strings.TrimSpace(parts[0])
	if len(raw) < 3 || raw[0] != '<' || raw[len(raw)-1] != '>' {
		return "", errRegistryObservation
	}
	parsed, err := url.Parse(raw[1 : len(raw)-1])
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.User != nil || parsed.Fragment != "" || parsed.Path != exactPath {
		return "", errRegistryObservation
	}
	query := parsed.Query()
	if len(query) != 2 || len(query["n"]) != 1 || query.Get("n") != strconv.Itoa(o.config.PageSize) || len(query["last"]) != 1 || query.Get("last") == "" {
		return "", errRegistryObservation
	}
	return parsed.RequestURI(), nil
}

func escapedRepository(repository string) string {
	parts := strings.Split(repository, "/")
	for index := range parts {
		parts[index] = url.PathEscape(parts[index])
	}
	return strings.Join(parts, "/")
}

func verifyDistributionDigest(body []byte, expected string) error {
	sum := sha256.Sum256(body)
	if "sha256:"+hex.EncodeToString(sum[:]) != expected {
		return errRegistryObservation
	}
	return nil
}

func decodeBoundedDistributionJSON(body []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return errRegistryObservation
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errRegistryObservation
	}
	return nil
}

func compactStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

func distributionObservationRevision(revision int64) string {
	return "registry-runtime-" + strconv.FormatInt(revision, 10)
}

func distributionObservationStatus(status int) error {
	class := DistributionErrorUnknown
	switch {
	case status == http.StatusUnauthorized:
		class = DistributionErrorAuthentication
	case status == http.StatusForbidden:
		class = DistributionErrorForbidden
	case status == http.StatusTooManyRequests:
		class = DistributionErrorRateLimit
	case status >= 500:
		class = DistributionErrorTransient
	case status == http.StatusNotFound:
		class = DistributionErrorInvalidResponse
	case status == 0:
		class = DistributionErrorInvalidResponse
	}
	return &DistributionError{StatusCode: status, Class: class}
}
