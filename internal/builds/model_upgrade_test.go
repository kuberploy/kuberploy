package builds

import (
	"strings"
	"testing"
)

func TestLegacyDefinitionWithoutBuildKitUsesCurrentAttemptRuntime(t *testing.T) {
	definition := validDefinition(t, testNow, RegistryExternal)
	definition.Spec.Execution.BuildKitImage = ""
	legacyDigest, err := legacyDefinitionDigestWithoutBuildKit(definition.Spec)
	if err != nil {
		t.Fatal(err)
	}
	definition.DefinitionDigest = legacyDigest
	if err = definition.validate(); err != nil {
		t.Fatalf("valid legacy definition rejected: %v", err)
	}

	runtime := testWorkerRuntimeConfig()
	runtime.BuildKitImage = "registry.test/system/buildkit:v0.32.2"
	resolved, err := attemptDefinitions([]BuildDefinition{definition}, runtime)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := newAttemptWithExecution(definition, resolved[0].Execution, repositoryFixture(testNow), EnqueuePush{
		ClaimKey: "sha256:" + strings.Repeat("a", 64), CommitSHA: strings.Repeat("b", 40), GitRef: "refs/heads/main", ResolvedAt: testNow,
	}, 1, nil, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := attempt.PlanRequest.Build.BuildKitImage; got != runtime.BuildKitImage {
		t.Fatalf("attempt BuildKit image = %q, want current runtime %q", got, runtime.BuildKitImage)
	}
	if definition.Spec.Execution.BuildKitImage != "" {
		t.Fatal("legacy definition was mutated")
	}

	definition.DefinitionDigest = "sha256:" + strings.Repeat("c", 64)
	if err = definition.validate(); err == nil {
		t.Fatal("substituted legacy digest accepted")
	}
	definition.DefinitionDigest = legacyDigest
	definition.Spec.Execution.BuilderAgentImage = "registry.test/other@sha256:" + strings.Repeat("d", 64)
	if err = definition.validate(); err == nil {
		t.Fatal("substituted legacy execution settings accepted")
	}
}
