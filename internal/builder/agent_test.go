package builder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBuildProgressContainsOnlyFixedLifecycleMessages(t *testing.T) {
	var output bytes.Buffer
	agent := NewAgent(&recordingExecutor{})
	agent.Progress = &output
	agent.reportProgress("Build request accepted.")
	agent.reportProgress("Release image build and push started.")

	if got, want := output.String(), "Build request accepted.\nRelease image build and push started.\n"; got != want {
		t.Fatalf("progress output = %q, want %q", got, want)
	}
	for _, forbidden := range []string{"password", "token", "secret", "--build-arg", "registry.example"} {
		if strings.Contains(strings.ToLower(output.String()), forbidden) {
			t.Fatalf("progress output exposed forbidden material %q", forbidden)
		}
	}
}

func TestBuildKitDaemonSharesOnlyTheIsolatedDinDPodNetwork(t *testing.T) {
	executor := &sequenceExecutor{results: []CommandResult{{}, {Output: "BuildKit v0.32.2"}}}
	agent := NewAgent(executor)
	if err := agent.createBuilder(context.Background(), "/result/push-auth", "kp-test", DefaultBuildKitImage); err != nil {
		t.Fatal(err)
	}
	if len(executor.invocations) != 2 {
		t.Fatalf("invocations = %d, want 2", len(executor.invocations))
	}
	create := strings.Join(executor.invocations[0].Argv, "\n")
	for _, required := range []string{"--driver\ndocker-container", "--driver-opt\nimage=" + DefaultBuildKitImage, "--driver-opt\nnetwork=host"} {
		if !strings.Contains(create, required) {
			t.Fatalf("BuildKit create invocation missing %q: %#v", required, executor.invocations[0].Argv)
		}
	}
}

func TestRegistryTransportFailureRetriesInsideTheSamePod(t *testing.T) {
	executor := &sequenceExecutor{
		results: []CommandResult{{Output: `failed to push registry.example.test/app:tag: failed to do request: dial tcp 10.0.0.10:443: connect: connection refused`}, {Output: "push complete"}},
		errors:  []error{errors.New("exit status 1"), nil},
	}
	var progress bytes.Buffer
	agent := NewAgent(executor)
	agent.RegistryRetryInterval = time.Nanosecond
	agent.Progress = &progress
	invocation := Invocation{Argv: []string{"docker", "buildx", "build"}}
	result, err := agent.executeRegistryBuild(context.Background(), invocation)
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "push complete" || len(executor.invocations) != 2 {
		t.Fatalf("result=%q invocations=%d, want successful second attempt", result.Output, len(executor.invocations))
	}
	if !strings.Contains(progress.String(), "retrying build in the same isolated Pod") {
		t.Fatalf("missing fixed retry progress message: %q", progress.String())
	}
}

func TestDockerfileFailureIsNotRetriedAsRegistryTransport(t *testing.T) {
	executor := &sequenceExecutor{
		results: []CommandResult{{Output: `executor failed running [/bin/sh -c exit 1]: exit code: 1`}},
		errors:  []error{errors.New("exit status 1")},
	}
	agent := NewAgent(executor)
	_, err := agent.executeRegistryBuild(context.Background(), Invocation{Argv: []string{"docker", "buildx", "build"}})
	if err == nil {
		t.Fatal("Dockerfile failure unexpectedly succeeded")
	}
	if len(executor.invocations) != 1 {
		t.Fatalf("Dockerfile failure invocations=%d, want 1", len(executor.invocations))
	}
}

type recordingExecutor struct {
	invocations []Invocation
}

type sequenceExecutor struct {
	invocations []Invocation
	errors      []error
	results     []CommandResult
}

func (e *sequenceExecutor) Execute(_ context.Context, invocation Invocation) (CommandResult, error) {
	e.invocations = append(e.invocations, cloneInvocation(invocation))
	index := len(e.invocations) - 1
	if index < len(e.errors) {
		result := CommandResult{}
		if index < len(e.results) {
			result = e.results[index]
		}
		return result, e.errors[index]
	}
	if index < len(e.results) {
		return e.results[index], nil
	}
	return CommandResult{}, nil
}

func (e *recordingExecutor) Execute(_ context.Context, invocation Invocation) (CommandResult, error) {
	e.invocations = append(e.invocations, cloneInvocation(invocation))
	return CommandResult{}, nil
}

func TestBuildPhasesUseDisjointRegistryAuthoritiesAndNoShell(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "Dockerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := validBuildRequest()
	request.BuildArgs = []BuildArg{{Name: "PUBLIC_VALUE", Value: `hello;$(touch /tmp/pwned)`}}
	agent := NewAgent(&recordingExecutor{})
	agent.CheckoutRoot = workspace
	cacheInvocation, err := agent.cacheInvocation(request, "/result/cache-auth", "kp-test", request.Cache.Imports)
	if err != nil {
		t.Fatal(err)
	}
	pushInvocation, err := agent.buildInvocation(request, "/result/push-auth", "kp-test", "/result/meta.json")
	if err != nil {
		t.Fatal(err)
	}
	cacheJoined := strings.Join(cacheInvocation.Argv, "\n")
	for _, expected := range []string{
		"type=registry,ref=" + request.Cache.Imports[0],
		"type=registry,ref=" + request.Cache.CandidateExport + ",mode=max,ignore-error=true,image-manifest=true,oci-mediatypes=true",
		"type=cacheonly",
		"PUBLIC_VALUE=hello;$(touch /tmp/pwned)",
		"id=npmrc,src=" + BuildSecretRoot + "/npmrc",
		"default=" + SSHSecretRoot + "/id_ed25519",
	} {
		if !strings.Contains(cacheJoined, expected) {
			t.Fatalf("missing exact cache argument %q in %#v", expected, cacheInvocation.Argv)
		}
	}
	pushJoined := strings.Join(pushInvocation.Argv, "\n")
	for _, expected := range []string{"--push", "--metadata-file", request.Destination.String(), "PUBLIC_VALUE=hello;$(touch /tmp/pwned)"} {
		if !strings.Contains(pushJoined, expected) {
			t.Fatalf("missing exact push argument %q in %#v", expected, pushInvocation.Argv)
		}
	}
	if !strings.Contains(cacheJoined, "/result/cache-auth") || strings.Contains(cacheJoined, "/result/push-auth") ||
		!strings.Contains(pushJoined, "/result/push-auth") || strings.Contains(pushJoined, "/result/cache-auth") {
		t.Fatal("cache and release-push Docker configurations crossed phase boundaries")
	}
	cacheBuildxConfig := environmentValue(cacheInvocation.Env, "BUILDX_CONFIG")
	pushBuildxConfig := environmentValue(pushInvocation.Env, "BUILDX_CONFIG")
	if cacheBuildxConfig == "" || cacheBuildxConfig != pushBuildxConfig || cacheBuildxConfig != "/result/buildx" {
		t.Fatalf("phases cannot resolve the same credential-free Buildx state: cache=%q push=%q", cacheBuildxConfig, pushBuildxConfig)
	}
	for _, forbidden := range []string{"--push", request.Destination.String(), "/result/push-auth"} {
		if strings.Contains(cacheJoined, forbidden) {
			t.Fatalf("cache phase received release-push authority %q: %#v", forbidden, cacheInvocation.Argv)
		}
	}
	for _, forbidden := range []string{"--cache-from", "--cache-to", request.Cache.CandidateExport, "/result/cache-auth"} {
		if strings.Contains(pushJoined, forbidden) {
			t.Fatalf("release-push phase received cache authority %q: %#v", forbidden, pushInvocation.Argv)
		}
	}
	if slices.Contains(cacheInvocation.Argv, "sh") || slices.Contains(cacheInvocation.Argv, "-c") ||
		slices.Contains(pushInvocation.Argv, "sh") || slices.Contains(pushInvocation.Argv, "-c") {
		t.Fatalf("shell appeared in phase argv: cache=%#v push=%#v", cacheInvocation.Argv, pushInvocation.Argv)
	}
	if _, err := os.Stat("/tmp/pwned"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("hostile argument was executed")
	}
}

func environmentValue(environment []string, key string) string {
	prefix := key + "="
	for _, entry := range environment {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}

func TestCheckoutPathsRejectEverySymlinkPosition(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "Dockerfile"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Run("root", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, "workspace")
		if err := os.Symlink(outside, root); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveExistingCheckoutPath(root, "Dockerfile", false); err == nil {
			t.Fatal("symlink checkout root was accepted")
		}
	})
	t.Run("intermediate", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(root, "context")); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveExistingCheckoutPath(root, "context/Dockerfile", false); err == nil {
			t.Fatal("intermediate symlink was accepted")
		}
	})
	t.Run("final", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Symlink(filepath.Join(outside, "Dockerfile"), filepath.Join(root, "Dockerfile")); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveExistingCheckoutPath(root, "Dockerfile", false); err == nil {
			t.Fatal("final symlink was accepted")
		}
	})
}

func TestCacheImportPreflightFiltersUnavailableRefsBeforeSingleBuild(t *testing.T) {
	executor := &sequenceExecutor{errors: []error{errors.New("cache missing"), nil}}
	agent := NewAgent(executor)
	imports := []string{"registry.example.test/cache:first", "registry.example.test/cache:second"}
	available := agent.availableCacheImports(context.Background(), "/result/auth", imports)
	if !reflect.DeepEqual(available, imports[1:]) {
		t.Fatalf("available imports = %v", available)
	}
	if len(executor.invocations) != len(imports) {
		t.Fatal("cache preflight did not inspect each candidate exactly once")
	}
}

func TestCachePromotionUsesDerivedGenerationReferenceAndVerifiesDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("d", 64)
	executor := &sequenceExecutor{results: []CommandResult{{Stdout: `{"digest":"` + digest + `"}`}, {}, {Stdout: `{"digest":"` + digest + `"}`}}}
	agent := NewAgent(executor)
	request := validBuildRequest()
	cache, err := agent.promoteCache(context.Background(), "/result/auth", request)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Split(request.Cache.CandidateExport, ":candidate-")[0] + ":generation-2"
	if cache == nil || cache.Reference != want || cache.Digest != digest {
		t.Fatalf("cache=%#v", cache)
	}
	if len(executor.invocations) != 3 {
		t.Fatalf("invocations=%d", len(executor.invocations))
	}
	promotion := executor.invocations[1]
	joined := strings.Join(promotion.Argv, " ")
	if !strings.Contains(joined, "imagetools create --prefer-index=false --tag "+want+" "+request.Cache.CandidateExport) || strings.Contains(joined, "username") || strings.Contains(joined, "password") {
		t.Fatalf("unsafe promotion invocation: %#v", promotion)
	}
}

func TestCachePromotionCannotCrossRepositoryAndFailureIsDegraded(t *testing.T) {
	request := validBuildRequest()
	request.Cache.CandidateExport = "attacker.test/stolen/cache:candidate-11111111111141118111111111111111-g2"
	if _, err := promotedCacheReference(request); err == nil {
		t.Fatal("cross-repository candidate produced a final cache reference")
	}

	digest := "sha256:" + strings.Repeat("d", 64)
	executor := &sequenceExecutor{
		results: []CommandResult{{Stdout: `{"digest":"` + digest + `"}`}, {}},
		errors:  []error{nil, errors.New("registry unavailable")},
	}
	cache, err := NewAgent(executor).promoteCache(context.Background(), "/result/auth", validBuildRequest())
	if err == nil || cache != nil {
		t.Fatalf("failed promotion cache=%#v err=%v", cache, err)
	}
}

func TestAllMissingCacheImportsProduceColdBuildInput(t *testing.T) {
	executor := &sequenceExecutor{errors: []error{errors.New("missing"), errors.New("missing")}}
	imports := []string{"registry.example.test/cache:first", "registry.example.test/cache:second"}
	if available := NewAgent(executor).availableCacheImports(context.Background(), "/result/auth", imports); len(available) != 0 {
		t.Fatalf("missing imports were retained: %v", available)
	}
}

func TestCacheReuseClassificationIsClosedAndCannotBeForgedByBuildOutput(t *testing.T) {
	tests := []struct {
		name      string
		requested int
		available int
		result    CommandResult
		degraded  bool
		want      CacheReuse
	}{
		{name: "not requested", want: CacheReuseNotRequested},
		{name: "unavailable", requested: 2, want: CacheReuseUnavailable},
		{name: "degraded", requested: 2, available: 1, degraded: true, want: CacheReuseUnavailable},
		{name: "truncated", requested: 2, available: 2, result: CommandResult{Output: "#8 CACHED\n", Truncated: true}, want: CacheReuseUnknown},
		{name: "hit", requested: 2, available: 2, result: CommandResult{Output: "#8 [stage 2/2] RUN true\n#8 CACHED\n"}, want: CacheReuseHit},
		{name: "miss", requested: 2, available: 2, result: CommandResult{Output: "#8 DONE 0.1s\n"}, want: CacheReuseMiss},
		{name: "hostile dockerfile output", requested: 2, available: 2, result: CommandResult{Output: "#8 0.1 #99 CACHED\n#8 DONE 0.1s\n"}, want: CacheReuseMiss},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyCacheReuse(test.requested, test.available, test.result, test.degraded); got != test.want {
				t.Fatalf("cache reuse = %q, want %q", got, test.want)
			}
		})
	}
}

func TestPrivateDockerConfigNeverPlacesCredentialsInArgvOrEnv(t *testing.T) {
	directory := t.TempDir()
	usernamePath := filepath.Join(directory, "username")
	passwordPath := filepath.Join(directory, "password")
	const username = "registry-user"
	const password = "very-private-password"
	if err := os.WriteFile(usernamePath, []byte(username+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(passwordPath, []byte(password+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configDirectory := filepath.Join(directory, "config")
	if err := writeDockerAuth(configDirectory, RegistryCredentials{Server: "registry.example.test", UsernameFile: usernamePath, PasswordFile: passwordPath}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(configDirectory, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("Docker config mode is %o", info.Mode().Perm())
	}
	agent := NewAgent(&recordingExecutor{})
	invocation := Invocation{Argv: agent.dockerArgs(configDirectory, "info")}
	encoded, _ := json.Marshal(invocation)
	if strings.Contains(string(encoded), username) || strings.Contains(string(encoded), password) {
		t.Fatal("credential leaked into command argv or environment")
	}
}

func TestUnavailableCacheCredentialDegradesWithoutAffectingPushAuthority(t *testing.T) {
	ready, warnings := prepareCacheDockerAuth(filepath.Join(t.TempDir(), "cache"), RegistryCredentials{
		Server: "registry.example.test", UsernameFile: "/missing/cache-username", PasswordFile: "/missing/cache-password",
	}, true)
	if ready || !slices.Equal(warnings, []Warning{WarningCacheDegraded, WarningColdBuild}) {
		t.Fatalf("ready=%v warnings=%v", ready, warnings)
	}
}

func TestBuilderNameDeterministicPerOperationGeneration(t *testing.T) {
	one := deterministicBuilderName("11111111-1111-4111-8111-111111111111", 7)
	two := deterministicBuilderName("11111111-1111-4111-8111-111111111111", 7)
	three := deterministicBuilderName("11111111-1111-4111-8111-111111111111", 8)
	if one != two || one == three {
		t.Fatalf("builder naming is not deterministic and generation-scoped: %q %q %q", one, two, three)
	}
}

func TestInspectionPlatformsHandlesSingleAndIndexExactly(t *testing.T) {
	single, err := inspectionPlatforms(json.RawMessage(`{"architecture":"amd64","os":"linux"}`))
	if err != nil || !reflect.DeepEqual(single, []string{"linux/amd64"}) {
		t.Fatalf("single manifest platforms = %v, %v", single, err)
	}
	index, err := inspectionPlatforms(json.RawMessage(`{"linux/arm64":{},"linux/amd64":{}}`))
	if err != nil || !reflect.DeepEqual(index, []string{"linux/amd64", "linux/arm64"}) {
		t.Fatalf("index platforms = %v, %v", index, err)
	}
	if _, err := inspectionPlatforms(json.RawMessage(`{"linux/amd64":{},"linux/386":{}}`)); err == nil {
		t.Fatal("unexpected extra platform was accepted")
	}
}
