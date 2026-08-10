// Package builder defines the closed protocol and trusted execution primitives
// used by Kuberploy's isolated source-build Jobs.
package builder

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const (
	ProtocolVersion         = "builder.kuberploy.io/v1alpha1"
	MaxRequestBytes         = 256 << 10
	DefaultCheckoutRoot     = "/workspace"
	RegistryPushSecretRoot  = "/var/run/secrets/kuberploy/registry-push"
	RegistryCacheSecretRoot = "/var/run/secrets/kuberploy/registry-cache"
	BuildSecretRoot         = "/var/run/secrets/kuberploy/build"
	SSHSecretRoot           = "/var/run/secrets/kuberploy/ssh"
	SourceCredentialRoot    = "/var/run/secrets/kuberploy/source"
	DefaultBuildKitImage    = "docker.io/moby/buildkit:v0.32.2"
	DefaultDockerSocket     = "unix:///run/kuberploy/docker/docker.sock"
	DefaultBuildResult      = "/result/result.json"
	DefaultCheckoutResult   = "/result/checkout.json"
)

var (
	uuidPattern     = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	commitPattern   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	namePattern     = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,62}$`)
	buildArgPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)
	registryPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]*(?::[0-9]{1,5})?$`)
	repositoryPart  = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
	tagPattern      = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	digestPattern   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// BuildRequest is intentionally closed and bounded. Unknown JSON properties are
// rejected by DecodeBuildRequest so the controller and agent cannot silently
// disagree about security-sensitive behavior.
type BuildRequest struct {
	APIVersion     string              `json:"apiVersion"`
	OperationID    string              `json:"operationId"`
	Generation     int64               `json:"generation"`
	ProjectID      string              `json:"projectId"`
	ServiceID      string              `json:"serviceId"`
	Commit         string              `json:"commit"`
	ContextPath    string              `json:"contextPath"`
	DockerfilePath string              `json:"dockerfilePath"`
	Platforms      []string            `json:"platforms"`
	BuildKitImage  string              `json:"buildKitImage"`
	Destination    Destination         `json:"destination"`
	Registry       RegistryCredentials `json:"registry"`
	BuildArgs      []BuildArg          `json:"buildArgs"`
	SecretFiles    []FileReference     `json:"secretFiles"`
	SSHFiles       []FileReference     `json:"sshFiles"`
	Cache          CachePolicy         `json:"cache"`
	Profile        BuildProfile        `json:"profile"`
}

type Destination struct {
	Repository string `json:"repository"`
	Reference  string `json:"reference"`
}

func (d Destination) String() string { return d.Repository + ":" + d.Reference }

type RegistryCredentials struct {
	Server           string `json:"server"`
	RepositoryPrefix string `json:"repositoryPrefix"`
	UsernameFile     string `json:"usernameFile"`
	PasswordFile     string `json:"passwordFile"`
}

type BuildArg struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type FileReference struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

type CachePolicy struct {
	Schema          string   `json:"schema"`
	TrustLane       string   `json:"trustLane"`
	BuildDefinition string   `json:"buildDefinition"`
	Imports         []string `json:"imports"`
	CandidateExport string   `json:"candidateExport"`
	UsernameFile    string   `json:"usernameFile"`
	PasswordFile    string   `json:"passwordFile"`
}

type BuildProfile struct {
	Resource       string `json:"resource"`
	TimeoutSeconds int64  `json:"timeoutSeconds"`
	Egress         string `json:"egress"`
}

// CheckoutRequest is consumed only by the trusted checkout init container.
type CheckoutRequest struct {
	APIVersion      string `json:"apiVersion"`
	OperationID     string `json:"operationId"`
	Generation      int64  `json:"generation"`
	RepositoryURL   string `json:"repositoryUrl"`
	ApprovedHost    string `json:"approvedHost"`
	Commit          string `json:"commit"`
	UsernameFile    string `json:"usernameFile,omitempty"`
	AccessTokenFile string `json:"accessTokenFile,omitempty"`
}

func DecodeBuildRequest(r io.Reader) (BuildRequest, error) {
	var request BuildRequest
	if err := decodeClosed(r, &request); err != nil {
		return request, err
	}
	if err := request.Validate(); err != nil {
		return request, err
	}
	return request, nil
}

func DecodeCheckoutRequest(r io.Reader) (CheckoutRequest, error) {
	var request CheckoutRequest
	if err := decodeClosed(r, &request); err != nil {
		return request, err
	}
	if err := request.Validate(); err != nil {
		return request, err
	}
	return request, nil
}

func decodeClosed(r io.Reader, dst any) error {
	limited := &io.LimitedReader{R: r, N: MaxRequestBytes + 1}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}
	if limited.N == 0 {
		return errors.New("request exceeds maximum size")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("request contains more than one JSON value")
		}
		return fmt.Errorf("decode trailing request data: %w", err)
	}
	return nil
}

func (r BuildRequest) Validate() error {
	if r.APIVersion != ProtocolVersion {
		return fmt.Errorf("apiVersion must be %q", ProtocolVersion)
	}
	if err := validateIdentity(r.OperationID, r.Generation); err != nil {
		return err
	}
	if !uuidPattern.MatchString(r.ProjectID) || !uuidPattern.MatchString(r.ServiceID) {
		return errors.New("projectId and serviceId must be canonical immutable UUIDs")
	}
	if !commitPattern.MatchString(r.Commit) {
		return errors.New("commit must be an exact lowercase 40-hex Git object ID")
	}
	if _, err := ResolveCheckoutPath(DefaultCheckoutRoot, r.ContextPath); err != nil {
		return fmt.Errorf("contextPath: %w", err)
	}
	if _, err := ResolveCheckoutPath(DefaultCheckoutRoot, r.DockerfilePath); err != nil {
		return fmt.Errorf("dockerfilePath: %w", err)
	}
	if len(r.Platforms) < 1 || len(r.Platforms) > 2 {
		return errors.New("platforms must contain one or two targets")
	}
	if !slices.IsSorted(r.Platforms) {
		return errors.New("platforms must use canonical sorted order")
	}
	if err := validateBuildKitImage(r.BuildKitImage); err != nil {
		return err
	}
	seenPlatforms := map[string]struct{}{}
	for _, platform := range r.Platforms {
		if platform != "linux/amd64" && platform != "linux/arm64" {
			return fmt.Errorf("unsupported platform %q", platform)
		}
		if _, duplicate := seenPlatforms[platform]; duplicate {
			return fmt.Errorf("duplicate platform %q", platform)
		}
		seenPlatforms[platform] = struct{}{}
	}
	if err := validateDestination(r.Destination); err != nil {
		return err
	}
	if err := r.Registry.validate(); err != nil {
		return err
	}
	expectedDestination := r.Registry.Server + "/" + r.Registry.RepositoryPrefix + "/projects/" + r.ProjectID + "/services/" + r.ServiceID + "/image"
	if r.Destination.Repository != expectedDestination {
		return errors.New("destination repository does not match the approved immutable project/service namespace")
	}
	expectedDestinationTag := "candidate-" + strings.ReplaceAll(r.OperationID, "-", "") + "-g" + fmt.Sprint(r.Generation) + "-" + r.Commit[:12]
	if r.Destination.Reference != expectedDestinationTag {
		return errors.New("destination reference must be the unique operation/generation/commit candidate tag")
	}
	if len(r.BuildArgs) > 64 {
		return errors.New("buildArgs exceeds 64 entries")
	}
	seenArgs := map[string]struct{}{}
	for index, arg := range r.BuildArgs {
		if !buildArgPattern.MatchString(arg.Name) {
			return fmt.Errorf("build argument %q has an invalid name", arg.Name)
		}
		if len(arg.Value) > 4096 || containsControl(arg.Value) {
			return fmt.Errorf("build argument %q has an invalid value", arg.Name)
		}
		if _, exists := seenArgs[arg.Name]; exists {
			return fmt.Errorf("duplicate build argument %q", arg.Name)
		}
		seenArgs[arg.Name] = struct{}{}
		if index > 0 && r.BuildArgs[index-1].Name >= arg.Name {
			return errors.New("buildArgs must use unique canonical name order")
		}
	}
	if err := validateFileReferences(r.SecretFiles, BuildSecretRoot, 32, "secretFiles"); err != nil {
		return err
	}
	if err := validateFileReferences(r.SSHFiles, SSHSecretRoot, 8, "sshFiles"); err != nil {
		return err
	}
	if r.Cache.Schema != "v1" || !namePattern.MatchString(r.Cache.TrustLane) || !digestPattern.MatchString(r.Cache.BuildDefinition) {
		return errors.New("cache schema, trust lane, or build definition is invalid")
	}
	if err := validateConfinedAbsolute(RegistryCacheSecretRoot, r.Cache.UsernameFile); err != nil {
		return fmt.Errorf("cache usernameFile: %w", err)
	}
	if err := validateConfinedAbsolute(RegistryCacheSecretRoot, r.Cache.PasswordFile); err != nil {
		return fmt.Errorf("cache passwordFile: %w", err)
	}
	if r.Registry.UsernameFile == r.Cache.UsernameFile || r.Registry.PasswordFile == r.Cache.PasswordFile {
		return errors.New("release-push and cache credentials must use distinct projected authorities")
	}
	if len(r.Cache.Imports) > 8 {
		return errors.New("cache imports exceeds 8 entries")
	}
	expectedCacheRepository := r.Registry.Server + "/" + r.Registry.RepositoryPrefix + "/projects/" + r.ProjectID + "/services/" + r.ServiceID + "/cache/" + r.Cache.Schema + "/" + r.Cache.TrustLane + "/" + platformScope(r.Platforms) + "/" + strings.TrimPrefix(r.Cache.BuildDefinition, "sha256:")
	seenCache := map[string]struct{}{}
	for index, ref := range r.Cache.Imports {
		if err := validateImageRef(ref); err != nil {
			return fmt.Errorf("cache import: %w", err)
		}
		repository, tag, err := splitTaggedReference(ref)
		if err != nil || repository != expectedCacheRepository {
			return errors.New("cache import does not match the approved project/service/trust/platform/build namespace")
		}
		var generation int64
		if _, err := fmt.Sscanf(tag, "generation-%d", &generation); err != nil || generation < 1 || generation >= r.Generation || tag != fmt.Sprintf("generation-%d", generation) {
			return errors.New("cache import must name an earlier immutable generation")
		}
		if _, duplicate := seenCache[ref]; duplicate {
			return fmt.Errorf("duplicate cache import %q", ref)
		}
		seenCache[ref] = struct{}{}
		if index > 0 && r.Cache.Imports[index-1] >= ref {
			return errors.New("cache imports must use unique canonical reference order")
		}
	}
	if err := validateImageRef(r.Cache.CandidateExport); err != nil {
		return fmt.Errorf("candidate cache export: %w", err)
	}
	expectedCandidate := expectedCacheRepository + ":candidate-" + strings.ReplaceAll(r.OperationID, "-", "") + "-g" + fmt.Sprint(r.Generation)
	if r.Cache.CandidateExport != expectedCandidate {
		return errors.New("candidate cache export must be the unique operation/generation reference in the approved cache namespace")
	}
	if _, duplicate := seenCache[r.Cache.CandidateExport]; duplicate {
		return errors.New("candidate cache export must be unique from cache imports")
	}
	if !namePattern.MatchString(r.Profile.Resource) || !namePattern.MatchString(r.Profile.Egress) {
		return errors.New("resource and egress profiles must be stable lowercase names")
	}
	if r.Profile.TimeoutSeconds < 60 || r.Profile.TimeoutSeconds > 7200 {
		return errors.New("timeoutSeconds must be between 60 and 7200")
	}
	return nil
}

func validateBuildKitImage(value string) error {
	if len(value) < len("a/b:v0.32.2") || len(value) > 512 || strings.TrimSpace(value) != value || strings.Contains(value, "@") {
		return errors.New("buildKitImage must be an explicit v0.32.2 image reference")
	}
	repository, tag, err := splitTaggedReference(value)
	if err != nil || repository == "" || tag != "v0.32.2" {
		return errors.New("buildKitImage must be an explicit v0.32.2 image reference")
	}
	return nil
}

func (r CheckoutRequest) Validate() error {
	if r.APIVersion != ProtocolVersion {
		return fmt.Errorf("apiVersion must be %q", ProtocolVersion)
	}
	if err := validateIdentity(r.OperationID, r.Generation); err != nil {
		return err
	}
	if !commitPattern.MatchString(r.Commit) {
		return errors.New("commit must be an exact lowercase 40-hex Git object ID")
	}
	u, err := url.Parse(r.RepositoryURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("repositoryUrl must be a credential-free HTTPS URL without query or fragment")
	}
	if !registryPattern.MatchString(r.ApprovedHost) || !strings.EqualFold(u.Host, r.ApprovedHost) {
		return errors.New("repositoryUrl host must exactly match the controller-approved host")
	}
	if len(r.RepositoryURL) > 2048 || containsControl(r.RepositoryURL) {
		return errors.New("repositoryUrl is invalid")
	}
	if (r.UsernameFile == "") != (r.AccessTokenFile == "") {
		return errors.New("source username and token files must be supplied together")
	}
	if r.UsernameFile != "" {
		if err := validateConfinedAbsolute(SourceCredentialRoot, r.UsernameFile); err != nil {
			return fmt.Errorf("usernameFile: %w", err)
		}
		if err := validateConfinedAbsolute(SourceCredentialRoot, r.AccessTokenFile); err != nil {
			return fmt.Errorf("accessTokenFile: %w", err)
		}
	}
	return nil
}

func validateIdentity(operationID string, generation int64) error {
	if !uuidPattern.MatchString(operationID) {
		return errors.New("operationId must be a canonical immutable UUID")
	}
	if generation < 1 || generation > 1_000_000_000 {
		return errors.New("generation must be between 1 and 1000000000")
	}
	return nil
}

func ResolveCheckoutPath(root, relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) || containsControl(relative) || len(relative) > 1024 {
		return "", errors.New("must be a non-empty bounded relative path")
	}
	cleaned := filepath.Clean(relative)
	if cleaned == "." {
		return filepath.Clean(root), nil
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", errors.New("must remain under the checkout root")
	}
	resolved := filepath.Join(root, cleaned)
	if err := validateConfinedAbsolute(root, resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

func validateConfinedAbsolute(root, path string) error {
	if !filepath.IsAbs(root) || !filepath.IsAbs(path) || filepath.Clean(path) != path || containsControl(path) {
		return errors.New("must be a clean absolute path")
	}
	relative, err := filepath.Rel(filepath.Clean(root), path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("must name a file below its dedicated credential root")
	}
	return nil
}

func validateDestination(destination Destination) error {
	if err := validateImageRef(destination.Repository + ":" + destination.Reference); err != nil {
		return fmt.Errorf("destination: %w", err)
	}
	if !tagPattern.MatchString(destination.Reference) {
		return errors.New("destination reference must be a mutable tag; digests are result-only")
	}
	return nil
}

func (r RegistryCredentials) validate() error {
	if !registryPattern.MatchString(r.Server) {
		return errors.New("registry server is invalid")
	}
	if r.RepositoryPrefix == "" || len(r.RepositoryPrefix) > 160 || strings.HasPrefix(r.RepositoryPrefix, "/") || strings.HasSuffix(r.RepositoryPrefix, "/") {
		return errors.New("registry repositoryPrefix is invalid")
	}
	for _, part := range strings.Split(r.RepositoryPrefix, "/") {
		if !repositoryPart.MatchString(part) {
			return errors.New("registry repositoryPrefix is invalid")
		}
	}
	if err := validateConfinedAbsolute(RegistryPushSecretRoot, r.UsernameFile); err != nil {
		return fmt.Errorf("registry usernameFile: %w", err)
	}
	if err := validateConfinedAbsolute(RegistryPushSecretRoot, r.PasswordFile); err != nil {
		return fmt.Errorf("registry passwordFile: %w", err)
	}
	return nil
}

func validateFileReferences(refs []FileReference, root string, maximum int, field string) error {
	if len(refs) > maximum {
		return fmt.Errorf("%s exceeds %d entries", field, maximum)
	}
	seen := map[string]struct{}{}
	for index, ref := range refs {
		if !namePattern.MatchString(ref.ID) {
			return fmt.Errorf("%s ID %q is invalid", field, ref.ID)
		}
		if err := validateConfinedAbsolute(root, ref.Path); err != nil {
			return fmt.Errorf("%s path for %q: %w", field, ref.ID, err)
		}
		if _, duplicate := seen[ref.ID]; duplicate {
			return fmt.Errorf("duplicate %s ID %q", field, ref.ID)
		}
		seen[ref.ID] = struct{}{}
		if index > 0 && refs[index-1].ID >= ref.ID {
			return fmt.Errorf("%s must use unique canonical ID order", field)
		}
	}
	return nil
}

func validateImageRef(ref string) error {
	if ref == "" || len(ref) > 512 || containsControl(ref) || strings.ContainsAny(ref, " @") {
		return errors.New("image reference is empty, too long, or contains forbidden characters")
	}
	lastSlash := strings.LastIndexByte(ref, '/')
	if lastSlash < 1 || lastSlash == len(ref)-1 {
		return errors.New("image reference must include a registry and repository")
	}
	registry := ref[:strings.IndexByte(ref, '/')]
	if !registryPattern.MatchString(registry) {
		return errors.New("image reference registry is invalid")
	}
	tail := ref[lastSlash+1:]
	separator := strings.LastIndexAny(tail, ":@")
	if separator <= 0 || separator == len(tail)-1 {
		return errors.New("image reference must include an explicit tag or digest")
	}
	path := ref[strings.IndexByte(ref, '/')+1:lastSlash+1] + tail[:separator]
	for _, part := range strings.Split(strings.TrimSuffix(path, "/"), "/") {
		if !repositoryPart.MatchString(part) {
			return fmt.Errorf("repository component %q is invalid", part)
		}
	}
	version := tail[separator+1:]
	if tail[separator] == '@' {
		if !digestPattern.MatchString(version) {
			return errors.New("digest is invalid")
		}
	} else if !tagPattern.MatchString(version) {
		return errors.New("tag is invalid")
	}
	return nil
}

func splitTaggedReference(ref string) (string, string, error) {
	lastSlash := strings.LastIndexByte(ref, '/')
	lastColon := strings.LastIndexByte(ref, ':')
	if lastColon <= lastSlash || lastColon == len(ref)-1 || strings.Contains(ref, "@") {
		return "", "", errors.New("reference must have an explicit tag")
	}
	return ref[:lastColon], ref[lastColon+1:], nil
}

func platformScope(platforms []string) string {
	canonical := canonicalPlatforms(platforms)
	for index := range canonical {
		canonical[index] = strings.TrimPrefix(canonical[index], "linux/")
	}
	return strings.Join(canonical, "-")
}

func containsControl(value string) bool {
	return strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0
}

func canonicalPlatforms(platforms []string) []string {
	result := slices.Clone(platforms)
	slices.Sort(result)
	return result
}
