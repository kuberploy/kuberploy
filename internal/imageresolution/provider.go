package imageresolution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	ociIndexMediaType       = "application/vnd.oci.image.index.v1+json"
	ociManifestMediaType    = "application/vnd.oci.image.manifest.v1+json"
	dockerIndexMediaType    = "application/vnd.docker.distribution.manifest.list.v2+json"
	dockerManifestMediaType = "application/vnd.docker.distribution.manifest.v2+json"
	manifestAccept          = ociIndexMediaType + ", " + ociManifestMediaType + ", " + dockerIndexMediaType + ", " + dockerManifestMediaType
	maximumReferencedBytes  = int64(1 << 40)
	maximumAnnotations      = 32
	maximumAnnotationKey    = 256
	maximumAnnotationValue  = 4 << 10
)

type ProviderConfig struct {
	Timeout          time.Duration
	MaximumBodyBytes int64
	MaximumManifests int
	UserAgent        string
}

func DefaultProviderConfig() ProviderConfig {
	return ProviderConfig{Timeout: 5 * time.Second, MaximumBodyBytes: 1 << 20, MaximumManifests: 64, UserAgent: "kuberploy-image-resolver/1"}
}

func (c ProviderConfig) valid() bool {
	return c.Timeout >= time.Second && c.Timeout <= 10*time.Second && c.MaximumBodyBytes >= 1024 && c.MaximumBodyBytes <= 4<<20 &&
		c.MaximumManifests >= 1 && c.MaximumManifests <= 128 && len(c.UserAgent) >= 1 && len(c.UserAgent) <= 256 &&
		strings.TrimSpace(c.UserAgent) == c.UserAgent && !strings.ContainsAny(c.UserAgent, "\x00\r\n")
}

type HTTPProvider struct {
	Credentials CredentialSource
	Config      ProviderConfig
	Transport   http.RoundTripper
}

type manifestEnvelope struct {
	SchemaVersion int                  `json:"schemaVersion"`
	MediaType     string               `json:"mediaType"`
	Manifests     []manifestDescriptor `json:"manifests"`
}

type manifestDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Platform    struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
		Variant      string `json:"variant"`
	} `json:"platform"`
}

type imageManifestEnvelope struct {
	SchemaVersion int                 `json:"schemaVersion"`
	MediaType     string              `json:"mediaType"`
	Config        contentDescriptor   `json:"config"`
	Layers        []contentDescriptor `json:"layers"`
}

type contentDescriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

type fetchedManifest struct {
	digest    string
	mediaType string
	body      []byte
}

func (p *HTTPProvider) ResolveTag(ctx context.Context, source AuthorizedSource, reference TagReference, authority *ProviderAuthority, platform Platform) (string, error) {
	if p == nil || !p.Config.valid() || authority == nil || !platform.valid() || source.Validate(source.Policy.ServiceID) != nil ||
		reference.Server == "" || reference.Repository != source.Policy.Repository || !tagPattern.MatchString(reference.Tag) {
		return "", ErrInvalid
	}
	server, err := canonicalRegistryServer(source.Target.Endpoint)
	if err != nil || server != reference.Server {
		return "", ErrConflict
	}
	if authority.Anonymous == (authority.Profile != nil) || !authority.Anonymous && p.Credentials == nil {
		return "", ErrUnavailable
	}
	if authority.Anonymous && strings.Contains(server, ":") {
		return "", ErrConflict
	}
	if authority.Profile != nil && (authority.Profile.TargetID != source.Target.ID || authority.Profile.RegistryServer != server) {
		return "", ErrConflict
	}
	if authority.Token != nil && authority.Token.TargetID != source.Target.ID {
		return "", ErrConflict
	}
	client := &http.Client{Transport: p.transport(), Timeout: p.Config.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	root, err := p.fetch(ctx, client, source, reference.Repository, reference.Tag, authority)
	if err != nil {
		return "", err
	}
	defer clear(root.body)
	if isImageManifest(root.mediaType) {
		if validateImageManifest(root.body, root.mediaType, p.Config.MaximumManifests, p.Config.MaximumBodyBytes) != nil {
			return "", ErrConflict
		}
		return root.digest, nil
	}
	if !isImageIndex(root.mediaType) {
		return "", ErrConflict
	}
	descriptor, err := selectPlatform(root.body, root.mediaType, platform, p.Config.MaximumManifests, p.Config.MaximumBodyBytes)
	if err != nil {
		return "", err
	}
	child, err := p.fetch(ctx, client, source, reference.Repository, descriptor.Digest, authority)
	if err != nil {
		return "", err
	}
	defer clear(child.body)
	if child.digest != descriptor.Digest || child.mediaType != descriptor.MediaType || !isImageManifest(child.mediaType) ||
		descriptor.Size != int64(len(child.body)) || validateImageManifest(child.body, child.mediaType, p.Config.MaximumManifests, p.Config.MaximumBodyBytes) != nil {
		return "", ErrConflict
	}
	return child.digest, nil
}

func (p *HTTPProvider) fetch(ctx context.Context, client *http.Client, source AuthorizedSource, repository, reference string, authority *ProviderAuthority) (fetchedManifest, error) {
	requestContext, cancel := context.WithTimeout(ctx, p.Config.Timeout)
	defer cancel()
	endpoint, err := manifestURL(source.Target.Endpoint, repository, reference)
	if err != nil {
		return fetchedManifest{}, err
	}
	initialHeader := ""
	if authority.Profile != nil {
		value, credentialErr := p.Credentials.Authorization(requestContext, *authority.Profile)
		if credentialErr != nil {
			value.destroy()
			if requestContext.Err() != nil {
				return fetchedManifest{}, requestContext.Err()
			}
			return fetchedManifest{}, ErrUnavailable
		}
		header, ok := value.header()
		if !ok {
			value.destroy()
			return fetchedManifest{}, ErrUnavailable
		}
		initialHeader = header
		value.destroy()
	}
	response, err := p.doRequest(requestContext, client, endpoint, manifestAccept, initialHeader)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if ctx.Err() != nil {
			return fetchedManifest{}, ctx.Err()
		}
		if requestContext.Err() != nil {
			return fetchedManifest{}, context.DeadlineExceeded
		}
		return fetchedManifest{}, ErrUnavailable
	}
	if response == nil || response.Body == nil {
		return fetchedManifest{}, ErrUnavailable
	}
	if response.StatusCode == http.StatusUnauthorized {
		challenge, challengeErr := parseBearerChallenge(response.Header, authority.Token, repository)
		if drainErr := drainBounded(response.Body, p.Config.MaximumBodyBytes); drainErr != nil {
			_ = response.Body.Close()
			return fetchedManifest{}, ErrUnavailable
		}
		_ = response.Body.Close()
		if challengeErr != nil {
			return fetchedManifest{}, challengeErr
		}
		token, tokenErr := p.fetchToken(requestContext, client, source, challenge, authority)
		if tokenErr != nil {
			return fetchedManifest{}, tokenErr
		}
		defer clear(token)
		response, err = p.doRequest(requestContext, client, endpoint, manifestAccept, "Bearer "+string(token))
		if err != nil {
			return fetchedManifest{}, ErrUnavailable
		}
	}
	return p.readManifestResponse(response)
}

func (p *HTTPProvider) doRequest(ctx context.Context, client *http.Client, endpoint, accept, authorization string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, ErrUnavailable
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", p.Config.UserAgent)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	response, err := client.Do(req)
	req.Header.Del("Authorization")
	return response, err
}

func (p *HTTPProvider) readManifestResponse(response *http.Response) (fetchedManifest, error) {
	if response == nil || response.Body == nil {
		return fetchedManifest{}, ErrUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength > p.Config.MaximumBodyBytes || response.Header.Get("Content-Encoding") != "" {
		_ = drainBounded(response.Body, p.Config.MaximumBodyBytes)
		return fetchedManifest{}, ErrUnavailable
	}
	mediaValues := response.Header.Values("Content-Type")
	digestValues := response.Header.Values("Docker-Content-Digest")
	if len(mediaValues) != 1 || len(digestValues) != 1 || !closedMediaType(mediaValues[0]) || !digestPattern.MatchString(digestValues[0]) {
		return fetchedManifest{}, ErrConflict
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, p.Config.MaximumBodyBytes+1))
	if err != nil || int64(len(body)) > p.Config.MaximumBodyBytes || len(body) == 0 {
		clear(body)
		return fetchedManifest{}, ErrUnavailable
	}
	sum := sha256.Sum256(body)
	computed := "sha256:" + hex.EncodeToString(sum[:])
	if computed != digestValues[0] {
		clear(body)
		return fetchedManifest{}, ErrConflict
	}
	return fetchedManifest{digest: computed, mediaType: mediaValues[0], body: body}, nil
}

type bearerChallenge struct {
	realm   string
	service string
	scope   string
}

func parseBearerChallenge(header http.Header, authority *TokenAuthority, repository string) (bearerChallenge, error) {
	values := header.Values("WWW-Authenticate")
	if len(values) != 1 || len(values[0]) > 2048 || authority == nil || !validRepository(repository) || !strings.HasPrefix(values[0], "Bearer ") {
		return bearerChallenge{}, ErrUnavailable
	}
	parameters, err := parseChallengeParameters(strings.TrimPrefix(values[0], "Bearer "))
	if err != nil || len(parameters) != 3 {
		return bearerChallenge{}, ErrConflict
	}
	wantScope := "repository:" + repository + ":pull"
	if parameters["realm"] != authority.RealmURL || parameters["service"] != authority.Service || parameters["scope"] != wantScope {
		return bearerChallenge{}, ErrConflict
	}
	return bearerChallenge{realm: parameters["realm"], service: parameters["service"], scope: parameters["scope"]}, nil
}

func parseChallengeParameters(raw string) (map[string]string, error) {
	result := make(map[string]string, 3)
	for len(raw) > 0 {
		raw = strings.TrimSpace(raw)
		equals := strings.IndexByte(raw, '=')
		if equals < 1 || equals > 16 {
			return nil, ErrConflict
		}
		key := raw[:equals]
		raw = raw[equals+1:]
		if raw == "" || raw[0] != '"' || result[key] != "" || (key != "realm" && key != "service" && key != "scope") {
			return nil, ErrConflict
		}
		raw = raw[1:]
		end := strings.IndexByte(raw, '"')
		if end < 1 || strings.ContainsAny(raw[:end], "\\\x00\r\n") {
			return nil, ErrConflict
		}
		result[key] = raw[:end]
		raw = raw[end+1:]
		if raw == "" {
			break
		}
		if raw[0] != ',' {
			return nil, ErrConflict
		}
		raw = raw[1:]
	}
	return result, nil
}

func (p *HTTPProvider) fetchToken(ctx context.Context, client *http.Client, source AuthorizedSource, challenge bearerChallenge, authority *ProviderAuthority) ([]byte, error) {
	realm, err := url.Parse(challenge.realm)
	if err != nil || authority.Token == nil || challenge.realm != authority.Token.RealmURL {
		return nil, ErrConflict
	}
	query := realm.Query()
	query.Set("service", challenge.service)
	query.Set("scope", challenge.scope)
	realm.RawQuery = query.Encode()
	authHeader := ""
	registryServer, _ := canonicalRegistryServer(source.Target.Endpoint)
	if authority.Profile != nil && realm.Host == registryServer {
		value, credentialErr := p.Credentials.Authorization(ctx, *authority.Profile)
		if credentialErr != nil {
			value.destroy()
			return nil, ErrUnavailable
		}
		authHeader, _ = value.header()
		value.destroy()
	}
	response, err := p.doRequest(ctx, client, realm.String(), "application/json", authHeader)
	if err != nil || response == nil || response.Body == nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, ErrUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength > 16<<10 || response.Header.Get("Content-Encoding") != "" ||
		len(response.Header.Values("Content-Type")) != 1 || response.Header.Get("Content-Type") != "application/json" {
		_ = drainBounded(response.Body, 16<<10)
		return nil, ErrUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (16<<10)+1))
	if err != nil || len(body) < 1 || len(body) > 16<<10 || rejectDuplicateJSONKeys(body, 8) != nil {
		clear(body)
		return nil, ErrUnavailable
	}
	defer clear(body)
	var payload struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		IssuedAt    string `json:"issued_at"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&payload) != nil || decoder.Decode(&struct{}{}) != io.EOF || payload.Token == "" && payload.AccessToken == "" ||
		payload.Token != "" && payload.AccessToken != "" && payload.Token != payload.AccessToken ||
		payload.ExpiresIn < 1 || payload.ExpiresIn > 3600 || len(payload.IssuedAt) > 64 {
		return nil, ErrConflict
	}
	token := payload.Token
	if token == "" {
		token = payload.AccessToken
	}
	value := authorization{scheme: "Bearer", value: []byte(token)}
	if _, ok := value.header(); !ok {
		value.destroy()
		return nil, ErrConflict
	}
	return value.value, nil
}

func drainBounded(reader io.Reader, maximum int64) error {
	read, err := io.Copy(io.Discard, io.LimitReader(reader, maximum+1))
	if err != nil || read > maximum {
		return ErrUnavailable
	}
	return nil
}

func (p *HTTPProvider) transport() http.RoundTripper {
	if p.Transport != nil {
		return p.Transport
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = (&net.Dialer{Timeout: min(p.Config.Timeout, 3*time.Second), KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = min(p.Config.Timeout, 3*time.Second)
	transport.ResponseHeaderTimeout = p.Config.Timeout
	transport.ExpectContinueTimeout = time.Second
	transport.MaxResponseHeaderBytes = 32 << 10
	transport.MaxIdleConnsPerHost = 2
	return transport
}

func manifestURL(endpoint, repository, reference string) (string, error) {
	server, err := canonicalRegistryServer(endpoint)
	if err != nil || !validRepository(repository) || (!tagPattern.MatchString(reference) && !digestPattern.MatchString(reference)) {
		return "", ErrConflict
	}
	u := &url.URL{Scheme: "https", Host: server}
	segments := append([]string{"v2"}, strings.Split(repository, "/")...)
	segments = append(segments, "manifests", reference)
	for _, segment := range segments {
		u.Path += "/" + segment
		u.RawPath += "/" + url.PathEscape(segment)
	}
	return u.String(), nil
}

func selectPlatform(raw []byte, mediaType string, platform Platform, maximum int, maximumBody int64) (manifestDescriptor, error) {
	var envelope manifestEnvelope
	if decodeStrictJSON(raw, &envelope, 12) != nil || envelope.SchemaVersion != 2 || envelope.MediaType != mediaType || !isImageIndex(mediaType) ||
		len(envelope.Manifests) < 1 || len(envelope.Manifests) > maximum {
		return manifestDescriptor{}, ErrConflict
	}
	var selected *manifestDescriptor
	for index := range envelope.Manifests {
		descriptor := envelope.Manifests[index]
		if !isImageManifest(descriptor.MediaType) || !digestPattern.MatchString(descriptor.Digest) || descriptor.Size < 1 || descriptor.Size > maximumBody ||
			!validAnnotations(descriptor.Annotations) {
			return manifestDescriptor{}, ErrConflict
		}
		if descriptor.Platform.OS == platform.OS && descriptor.Platform.Architecture == platform.Architecture && descriptor.Platform.Variant == platform.Variant {
			if selected != nil {
				return manifestDescriptor{}, ErrConflict
			}
			copy := descriptor
			selected = &copy
		}
	}
	if selected == nil {
		return manifestDescriptor{}, ErrNotFound
	}
	return *selected, nil
}

func validateImageManifest(raw []byte, mediaType string, maximumLayers int, maximumBody int64) error {
	var envelope imageManifestEnvelope
	if decodeStrictJSON(raw, &envelope, 12) != nil || envelope.SchemaVersion != 2 || envelope.MediaType != mediaType || !isImageManifest(mediaType) ||
		len(envelope.Layers) > maximumLayers || !validContentDescriptor(envelope.Config, true) {
		return ErrConflict
	}
	for _, layer := range envelope.Layers {
		if !validContentDescriptor(layer, false) {
			return ErrConflict
		}
	}
	return nil
}

func validContentDescriptor(descriptor contentDescriptor, config bool) bool {
	if !digestPattern.MatchString(descriptor.Digest) || descriptor.Size < 1 || descriptor.Size > maximumReferencedBytes {
		return false
	}
	if config {
		return descriptor.MediaType == "application/vnd.oci.image.config.v1+json" || descriptor.MediaType == "application/vnd.docker.container.image.v1+json"
	}
	switch descriptor.MediaType {
	case "application/vnd.oci.image.layer.v1.tar", "application/vnd.oci.image.layer.v1.tar+gzip", "application/vnd.oci.image.layer.v1.tar+zstd",
		"application/vnd.docker.image.rootfs.diff.tar.gzip", "application/vnd.docker.image.rootfs.foreign.diff.tar.gzip":
		return true
	default:
		return false
	}
}

func validAnnotations(values map[string]string) bool {
	if len(values) > maximumAnnotations {
		return false
	}
	for key, value := range values {
		if key == "" || len(key) > maximumAnnotationKey || len(value) > maximumAnnotationValue ||
			strings.TrimSpace(key) != key || strings.ContainsAny(key, "\x00\r\n\t ") || strings.ContainsAny(value, "\x00\r\n") {
			return false
		}
	}
	return true
}

func decodeStrictJSON(raw []byte, target any, maximumDepth int) error {
	if rejectDuplicateJSONKeys(raw, maximumDepth) != nil {
		return ErrConflict
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return ErrConflict
	}
	if token, err := decoder.Token(); err != io.EOF || token != nil {
		return ErrConflict
	}
	return nil
}

func rejectDuplicateJSONKeys(raw []byte, maximumDepth int) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var walk func(int) error
	walk = func(depth int) error {
		if depth > maximumDepth {
			return ErrConflict
		}
		token, err := decoder.Token()
		if err != nil {
			return ErrConflict
		}
		delimiter, composite := token.(json.Delim)
		if !composite {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]struct{}{}
			for decoder.More() {
				keyToken, keyErr := decoder.Token()
				key, ok := keyToken.(string)
				if keyErr != nil || !ok {
					return ErrConflict
				}
				if _, duplicate := seen[key]; duplicate {
					return ErrConflict
				}
				seen[key] = struct{}{}
				if err = walk(depth + 1); err != nil {
					return err
				}
			}
			end, endErr := decoder.Token()
			if endErr != nil || end != json.Delim('}') {
				return ErrConflict
			}
		case '[':
			for decoder.More() {
				if err = walk(depth + 1); err != nil {
					return err
				}
			}
			end, endErr := decoder.Token()
			if endErr != nil || end != json.Delim(']') {
				return ErrConflict
			}
		default:
			return ErrConflict
		}
		return nil
	}
	if err := walk(0); err != nil {
		return err
	}
	if token, err := decoder.Token(); err != io.EOF || token != nil {
		return ErrConflict
	}
	return nil
}

func closedMediaType(value string) bool { return isImageIndex(value) || isImageManifest(value) }
func isImageIndex(value string) bool {
	return value == ociIndexMediaType || value == dockerIndexMediaType
}
func isImageManifest(value string) bool {
	return value == ociManifestMediaType || value == dockerManifestMediaType
}
