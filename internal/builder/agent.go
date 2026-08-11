package builder

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type Agent struct {
	Executor     CommandExecutor
	DockerBinary string
	DockerSocket string
	CheckoutRoot string
	RuntimeRoot  string
	WaitInterval time.Duration
	Now          func() time.Time
	// Progress receives only fixed, server-owned lifecycle messages. Raw tool
	// output remains private because an untrusted Dockerfile can print mounted
	// secrets or other credential-bearing process output.
	Progress io.Writer
}

func NewAgent(executor CommandExecutor) *Agent {
	return &Agent{
		Executor:     executor,
		DockerBinary: "docker",
		DockerSocket: DefaultDockerSocket,
		CheckoutRoot: DefaultCheckoutRoot,
		RuntimeRoot:  "/result",
		WaitInterval: 250 * time.Millisecond,
		Now:          time.Now,
	}
}

func (a *Agent) Run(ctx context.Context, request BuildRequest) (BuildResult, error) {
	if err := request.Validate(); err != nil {
		return BuildResult{}, err
	}
	if a.Executor == nil {
		return BuildResult{}, errors.New("command executor is required")
	}
	a.reportProgress("Build request accepted.")
	if err := validateUnixSocket(a.DockerSocket); err != nil {
		return BuildResult{}, err
	}
	started := a.Now().UTC()
	runtimeDirectory, err := os.MkdirTemp(a.RuntimeRoot, ".builder-runtime-*")
	if err != nil {
		return BuildResult{}, fmt.Errorf("create private runtime: %w", err)
	}
	if err := os.Chmod(runtimeDirectory, 0o700); err != nil {
		_ = os.RemoveAll(runtimeDirectory)
		return BuildResult{}, fmt.Errorf("secure private runtime: %w", err)
	}
	defer os.RemoveAll(runtimeDirectory)
	for _, directory := range []string{filepath.Join(runtimeDirectory, "home"), filepath.Join(runtimeDirectory, "tmp"), filepath.Join(runtimeDirectory, "xdg")} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			return BuildResult{}, errors.New("create private tool runtime")
		}
	}

	pushDockerConfig := filepath.Join(runtimeDirectory, "docker-push-config")
	if err := writeDockerAuth(pushDockerConfig, request.Registry); err != nil {
		return BuildResult{}, err
	}
	defer os.RemoveAll(pushDockerConfig)
	cacheDockerConfig := filepath.Join(runtimeDirectory, "docker-cache-config")
	if err := writeDockerAuth(cacheDockerConfig, cacheRegistryCredentials(request)); err != nil {
		return BuildResult{}, err
	}
	defer os.RemoveAll(cacheDockerConfig)

	if err := a.waitForDaemon(ctx, pushDockerConfig); err != nil {
		return BuildResult{}, err
	}
	a.reportProgress("Isolated Docker daemon ready.")
	builderName := deterministicBuilderName(request.OperationID, request.Generation)
	if err := a.createBuilder(ctx, pushDockerConfig, builderName, request.BuildKitImage); err != nil {
		return BuildResult{}, err
	}
	a.reportProgress("Pinned BuildKit builder ready.")
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = a.Executor.Execute(cleanupCtx, Invocation{Argv: a.dockerArgs(pushDockerConfig, "buildx", "rm", "--force", builderName), Env: dockerEnvironment(pushDockerConfig)})
	}()

	metadataPath := filepath.Join(runtimeDirectory, "metadata.json")
	cacheImports := a.availableCacheImports(ctx, cacheDockerConfig, request.Cache.Imports)
	warnings := []Warning{}
	if len(cacheImports) != len(request.Cache.Imports) {
		warnings = addWarning(warnings, WarningCacheDegraded)
		if len(request.Cache.Imports) > 0 && len(cacheImports) == 0 {
			warnings = addWarning(warnings, WarningColdBuild)
		}
	}
	// Build the best-effort registry cache under its cache-only credential
	// authority. The final image push is a separate invocation with the release
	// push authority; BuildKit's local content store carries successful work
	// between the two invocations without sharing registry credentials.
	cacheInvocation, err := a.cacheInvocation(request, cacheDockerConfig, builderName, cacheImports)
	if err != nil {
		return BuildResult{}, err
	}
	a.reportProgress("Registry cache build started.")
	cacheResult, cacheBuildErr := a.Executor.Execute(ctx, cacheInvocation)
	if cacheBuildErr != nil || outputShowsCacheDegradation(cacheResult.Output) {
		warnings = addWarning(warnings, WarningCacheDegraded)
		a.reportProgress("Registry cache degraded; continuing with the release build.")
	} else {
		a.reportProgress("Registry cache build completed.")
	}
	invocation, err := a.buildInvocation(request, pushDockerConfig, builderName, metadataPath)
	if err != nil {
		return BuildResult{}, err
	}
	a.reportProgress("Release image build and push started.")
	_, buildErr := a.Executor.Execute(ctx, invocation)
	if buildErr != nil {
		return BuildResult{}, commandError("build and final image push", buildErr)
	}
	a.reportProgress("Release image push completed.")

	digest, err := readBuildDigest(metadataPath)
	if err != nil {
		return BuildResult{}, err
	}
	platforms, inspectedDigest, err := a.inspectPushedImage(ctx, pushDockerConfig, request.Destination.String())
	if err != nil {
		return BuildResult{}, err
	}
	if digest != inspectedDigest {
		return BuildResult{}, fmt.Errorf("pushed digest verification failed: metadata and registry differ")
	}
	expectedPlatforms := canonicalPlatforms(request.Platforms)
	if !slices.Equal(expectedPlatforms, platforms) {
		return BuildResult{}, fmt.Errorf("pushed platform verification failed: expected %v, received %v", expectedPlatforms, platforms)
	}
	a.reportProgress("Published digest and platforms verified.")
	cache, cacheErr := a.promoteCache(ctx, cacheDockerConfig, request)
	if cacheErr != nil {
		warnings = addWarning(warnings, WarningCacheDegraded)
		a.reportProgress("Cache promotion degraded; the release image remains valid.")
	} else {
		a.reportProgress("Cache promotion completed.")
	}
	a.reportProgress("Build completed.")

	return BuildResult{
		APIVersion:  ProtocolVersion,
		OperationID: request.OperationID,
		Generation:  request.Generation,
		Status:      "Succeeded",
		Image: Image{
			Reference: request.Destination.Repository + "@" + digest,
			Digest:    digest,
			Platforms: platforms,
		},
		Cache:       cache,
		Warnings:    warnings,
		StartedAt:   started,
		CompletedAt: a.Now().UTC(),
	}, nil
}

func (a *Agent) reportProgress(message string) {
	if a.Progress != nil {
		_, _ = fmt.Fprintln(a.Progress, message)
	}
}

func (a *Agent) dockerArgs(configDirectory string, args ...string) []string {
	prefix := []string{a.DockerBinary, "--host", a.DockerSocket, "--config", configDirectory}
	return append(prefix, args...)
}

func (a *Agent) waitForDaemon(ctx context.Context, configDirectory string) error {
	interval := a.WaitInterval
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	for {
		_, err := a.Executor.Execute(ctx, Invocation{Argv: a.dockerArgs(configDirectory, "info", "--format", "{{.ServerVersion}}"), Env: dockerEnvironment(configDirectory)})
		if err == nil {
			return nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("wait for isolated Docker daemon: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func (a *Agent) createBuilder(ctx context.Context, configDirectory, name, buildKitImage string) error {
	create := Invocation{Argv: a.dockerArgs(configDirectory,
		"buildx", "create",
		"--name", name,
		"--driver", "docker-container",
		"--driver-opt", "image="+buildKitImage,
		"--use",
	), Env: dockerEnvironment(configDirectory)}
	if _, err := a.Executor.Execute(ctx, create); err != nil {
		return commandError("create isolated Buildx builder", err)
	}
	bootstrap := Invocation{Argv: a.dockerArgs(configDirectory, "buildx", "inspect", "--builder", name, "--bootstrap"), Env: dockerEnvironment(configDirectory)}
	result, err := a.Executor.Execute(ctx, bootstrap)
	if err != nil {
		return commandError("bootstrap pinned BuildKit", err)
	}
	if !strings.Contains(result.Output, "v0.32.2") {
		return errors.New("Buildx did not report the locked BuildKit v0.32.2 runtime")
	}
	return nil
}

func (a *Agent) buildInvocation(request BuildRequest, configDirectory, builderName, metadataPath string) (Invocation, error) {
	contextPath, err := resolveExistingCheckoutPath(a.CheckoutRoot, request.ContextPath, true)
	if err != nil {
		return Invocation{}, fmt.Errorf("context path: %w", err)
	}
	dockerfilePath, err := resolveExistingCheckoutPath(a.CheckoutRoot, request.DockerfilePath, false)
	if err != nil {
		return Invocation{}, fmt.Errorf("dockerfile path: %w", err)
	}
	args := a.dockerArgs(configDirectory,
		"buildx", "build",
		"--builder", builderName,
		"--file", dockerfilePath,
		"--platform", strings.Join(request.Platforms, ","),
		"--tag", request.Destination.String(),
		"--push",
		"--metadata-file", metadataPath,
	)
	args = appendBuildInputs(args, request, contextPath)
	return Invocation{Argv: args, Env: dockerEnvironment(configDirectory)}, nil
}

func (a *Agent) cacheInvocation(request BuildRequest, configDirectory, builderName string, cacheImports []string) (Invocation, error) {
	contextPath, err := resolveExistingCheckoutPath(a.CheckoutRoot, request.ContextPath, true)
	if err != nil {
		return Invocation{}, fmt.Errorf("context path: %w", err)
	}
	dockerfilePath, err := resolveExistingCheckoutPath(a.CheckoutRoot, request.DockerfilePath, false)
	if err != nil {
		return Invocation{}, fmt.Errorf("dockerfile path: %w", err)
	}
	args := a.dockerArgs(configDirectory,
		"buildx", "build",
		"--builder", builderName,
		"--file", dockerfilePath,
		"--platform", strings.Join(request.Platforms, ","),
		"--output", "type=cacheonly",
	)
	for _, ref := range cacheImports {
		args = append(args, "--cache-from", "type=registry,ref="+ref)
	}
	args = append(args, "--cache-to", "type=registry,ref="+request.Cache.CandidateExport+",mode=max,ignore-error=true,image-manifest=true,oci-mediatypes=true")
	args = appendBuildInputs(args, request, contextPath)
	return Invocation{Argv: args, Env: dockerEnvironment(configDirectory)}, nil
}

func appendBuildInputs(args []string, request BuildRequest, contextPath string) []string {
	for _, arg := range request.BuildArgs {
		args = append(args, "--build-arg", arg.Name+"="+arg.Value)
	}
	for _, secret := range request.SecretFiles {
		args = append(args, "--secret", "id="+secret.ID+",src="+secret.Path)
	}
	for _, ssh := range request.SSHFiles {
		args = append(args, "--ssh", ssh.ID+"="+ssh.Path)
	}
	args = append(args, contextPath)
	return args
}

func cacheRegistryCredentials(request BuildRequest) RegistryCredentials {
	return RegistryCredentials{Server: request.Registry.Server, RepositoryPrefix: request.Registry.RepositoryPrefix,
		UsernameFile: request.Cache.UsernameFile, PasswordFile: request.Cache.PasswordFile}
}

func (a *Agent) availableCacheImports(ctx context.Context, configDirectory string, imports []string) []string {
	available := make([]string, 0, len(imports))
	for _, ref := range imports {
		_, err := a.Executor.Execute(ctx, Invocation{Argv: a.dockerArgs(configDirectory,
			"buildx", "imagetools", "inspect", ref, "--format", "{{json .Manifest}}",
		), Env: dockerEnvironment(configDirectory)})
		if err == nil {
			available = append(available, ref)
		}
	}
	return available
}

func (a *Agent) promoteCache(ctx context.Context, configDirectory string, request BuildRequest) (*Cache, error) {
	finalReference, err := promotedCacheReference(request)
	if err != nil {
		return nil, err
	}
	candidateDigest, err := a.inspectManifestDigest(ctx, configDirectory, request.Cache.CandidateExport)
	if err != nil {
		return nil, err
	}
	invocation := Invocation{
		Argv: a.dockerArgs(configDirectory, "buildx", "imagetools", "create", "--tag", finalReference, request.Cache.CandidateExport),
		Env:  dockerEnvironment(configDirectory),
	}
	if _, err = a.Executor.Execute(ctx, invocation); err != nil {
		return nil, commandError("promote registry cache candidate", err)
	}
	finalDigest, err := a.inspectManifestDigest(ctx, configDirectory, finalReference)
	if err != nil {
		return nil, err
	}
	if finalDigest != candidateDigest {
		return nil, errors.New("promoted cache digest verification failed")
	}
	return &Cache{Reference: finalReference, Digest: finalDigest}, nil
}

func promotedCacheReference(request BuildRequest) (string, error) {
	if err := request.Validate(); err != nil {
		return "", err
	}
	repository, _, err := splitTaggedReference(request.Cache.CandidateExport)
	if err != nil {
		return "", errors.New("validated cache candidate has no tag")
	}
	return repository + ":generation-" + fmt.Sprint(request.Generation), nil
}

func (a *Agent) inspectManifestDigest(ctx context.Context, configDirectory, reference string) (string, error) {
	result, err := a.Executor.Execute(ctx, Invocation{
		Argv: a.dockerArgs(configDirectory, "buildx", "imagetools", "inspect", reference, "--format", "{{json .Manifest}}"),
		Env:  dockerEnvironment(configDirectory),
	})
	if err != nil {
		return "", commandError("inspect registry cache manifest", err)
	}
	var manifest struct {
		Digest string `json:"digest"`
	}
	if err := json.Unmarshal([]byte(result.Stdout), &manifest); err != nil || !digestPattern.MatchString(manifest.Digest) {
		return "", errors.New("registry cache inspection returned an invalid digest")
	}
	return manifest.Digest, nil
}

func (a *Agent) inspectPushedImage(ctx context.Context, configDirectory, reference string) ([]string, string, error) {
	result, err := a.Executor.Execute(ctx, Invocation{Argv: a.dockerArgs(configDirectory,
		"buildx", "imagetools", "inspect", reference, "--format", "{{json .}}",
	), Env: dockerEnvironment(configDirectory)})
	if err != nil {
		return nil, "", commandError("inspect pushed image", err)
	}
	var inspection struct {
		Manifest struct {
			Digest string `json:"digest"`
		} `json:"manifest"`
		Image json.RawMessage `json:"image"`
	}
	if err := json.Unmarshal([]byte(result.Stdout), &inspection); err != nil {
		return nil, "", errors.New("inspect pushed image returned invalid JSON")
	}
	if !digestPattern.MatchString(inspection.Manifest.Digest) {
		return nil, "", errors.New("inspect pushed image returned an invalid digest")
	}
	platforms, err := inspectionPlatforms(inspection.Image)
	if err != nil {
		return nil, "", err
	}
	return platforms, inspection.Manifest.Digest, nil
}

func inspectionPlatforms(encoded json.RawMessage) ([]string, error) {
	var single struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
		Variant      string `json:"variant"`
	}
	if err := json.Unmarshal(encoded, &single); err != nil {
		return nil, errors.New("inspect pushed image returned invalid platform JSON")
	}
	if single.OS != "" || single.Architecture != "" {
		platform := single.OS + "/" + single.Architecture
		if single.Variant != "" && single.Architecture != "arm64" {
			platform += "/" + single.Variant
		}
		if platform != "linux/amd64" && platform != "linux/arm64" {
			return nil, fmt.Errorf("inspect pushed image returned unexpected platform %q", platform)
		}
		return []string{platform}, nil
	}
	var images map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &images); err != nil || len(images) == 0 {
		return nil, errors.New("inspect pushed image returned no platforms")
	}
	platforms := make([]string, 0, len(images))
	for platform := range images {
		if platform != "linux/amd64" && platform != "linux/arm64" {
			return nil, fmt.Errorf("inspect pushed image returned unexpected platform %q", platform)
		}
		platforms = append(platforms, platform)
	}
	slices.Sort(platforms)
	return platforms, nil
}

func readBuildDigest(path string) (string, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read Buildx metadata: %w", err)
	}
	if len(encoded) > MaxResultBytes {
		return "", errors.New("Buildx metadata exceeds maximum size")
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &metadata); err != nil {
		return "", errors.New("Buildx metadata is invalid JSON")
	}
	var digest string
	if value, found := metadata["containerimage.digest"]; found {
		_ = json.Unmarshal(value, &digest)
	}
	if !digestPattern.MatchString(digest) {
		return "", errors.New("Buildx metadata did not contain a valid pushed digest")
	}
	return digest, nil
}

func writeDockerAuth(directory string, credentials RegistryCredentials) error {
	username, err := readCredentialFile(credentials.UsernameFile)
	if err != nil {
		return fmt.Errorf("read registry username: %w", err)
	}
	defer zeroBytes(username)
	password, err := readCredentialFile(credentials.PasswordFile)
	if err != nil {
		return fmt.Errorf("read registry password: %w", err)
	}
	defer zeroBytes(password)
	if err := os.Mkdir(directory, 0o700); err != nil {
		return fmt.Errorf("create private Docker config: %w", err)
	}
	credential := make([]byte, 0, len(username)+1+len(password))
	credential = append(credential, username...)
	credential = append(credential, ':')
	credential = append(credential, password...)
	defer zeroBytes(credential)
	auth := make([]byte, base64.StdEncoding.EncodedLen(len(credential)))
	base64.StdEncoding.Encode(auth, credential)
	defer zeroBytes(auth)
	encoded := make([]byte, 0, len(auth)+len(credentials.Server)+32)
	encoded = append(encoded, `{"auths":{"`...)
	encoded = append(encoded, credentials.Server...)
	encoded = append(encoded, `":{"auth":"`...)
	encoded = append(encoded, auth...)
	encoded = append(encoded, `"}}}`...)
	defer zeroBytes(encoded)
	configPath := filepath.Join(directory, "config.json")
	if err := os.WriteFile(configPath, encoded, 0o600); err != nil {
		return fmt.Errorf("write private Docker config: %w", err)
	}
	return nil
}

func readCredentialFile(path string) ([]byte, error) {
	value, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(value) == 0 || len(value) > 64<<10 {
		zeroBytes(value)
		return nil, errors.New("credential file is empty or exceeds 64 KiB")
	}
	if len(value) > 0 && value[len(value)-1] == '\n' {
		value = value[:len(value)-1]
	}
	if len(value) > 0 && value[len(value)-1] == '\r' {
		value = value[:len(value)-1]
	}
	if len(value) == 0 || bytes.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		zeroBytes(value)
		return nil, errors.New("credential contains forbidden control characters")
	}
	return value, nil
}

func zeroBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}

func validateUnixSocket(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "unix" || u.Host != "" || !filepath.IsAbs(u.Path) || filepath.Clean(u.Path) != u.Path {
		return errors.New("Docker daemon endpoint must be a clean absolute unix:// socket")
	}
	return nil
}

func dockerEnvironment(configDirectory string) []string {
	runtimeDirectory := filepath.Dir(configDirectory)
	return []string{
		"HOME=" + filepath.Join(runtimeDirectory, "home"),
		"TMPDIR=" + filepath.Join(runtimeDirectory, "tmp"),
		"XDG_CONFIG_HOME=" + filepath.Join(runtimeDirectory, "xdg"),
	}
}

func deterministicBuilderName(operationID string, generation int64) string {
	hash := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", operationID, generation)))
	return "kp-" + hex.EncodeToString(hash[:])[:20]
}

func outputShowsCacheDegradation(output string) bool {
	lower := strings.ToLower(output)
	mentionsCache := strings.Contains(lower, "cache import") || strings.Contains(lower, "cache export") || strings.Contains(lower, "cache-to") || strings.Contains(lower, "cache-from")
	mentionsFailure := strings.Contains(lower, "warning") || strings.Contains(lower, "error") || strings.Contains(lower, "failed") || strings.Contains(lower, "unavailable")
	return mentionsCache && mentionsFailure
}

// resolveExistingCheckoutPath rejects symlinks in the checkout root and every
// traversed component. Checkout and build run in separate containers and the
// agent receives the workspace read-only, so the verified tree cannot be
// replaced between this walk and Buildx opening it.
func resolveExistingCheckoutPath(root, relative string, wantDirectory bool) (string, error) {
	resolved, err := ResolveCheckoutPath(root, relative)
	if err != nil {
		return "", err
	}
	root = filepath.Clean(root)
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return "", fmt.Errorf("inspect checkout root: %w", err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return "", errors.New("checkout root must be a real directory")
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil {
		return "", errors.New("resolve path below checkout root")
	}
	current := root
	if rel != "." {
		for _, component := range strings.Split(rel, string(filepath.Separator)) {
			current = filepath.Join(current, component)
			info, err := os.Lstat(current)
			if err != nil {
				return "", fmt.Errorf("inspect %q: %w", component, err)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("component %q must not be a symbolic link", component)
			}
		}
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", fmt.Errorf("inspect final path: %w", err)
	}
	if wantDirectory && !info.IsDir() {
		return "", errors.New("must be a directory")
	}
	if !wantDirectory && !info.Mode().IsRegular() {
		return "", errors.New("must be a regular file")
	}
	return resolved, nil
}
