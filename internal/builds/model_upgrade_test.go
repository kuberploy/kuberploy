package builds

import (
	"strings"
	"testing"

	"github.com/kuberploy/kuberploy/internal/builder"
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

func TestPushAttemptRuntimeRestoresAuthorizedSecretBindings(t *testing.T) {
	definition := validDefinition(t, testNow, RegistryExternal)
	definition.Spec.Execution.BuildSecret = "legacy-build-secret"
	definition.Spec.Execution.SSHSecret = "legacy-ssh-secret"
	definition.Spec.SecretFiles = []builder.FileReference{{ID: "npmrc", Path: builder.BuildSecretRoot + "/npmrc"}}
	definition.Spec.SSHFiles = []builder.FileReference{{ID: "github", Path: builder.SSHSecretRoot + "/id_ed25519"}}
	definition, err := PrepareDefinition(definition, testNow)
	if err != nil {
		t.Fatal(err)
	}

	runtime := testWorkerRuntimeConfig()
	runtime.BuildSecret = "current-build-secret"
	runtime.SSHSecret = "current-ssh-secret"
	runtime.BuildSecretProfiles = []BuildSecretProfile{{
		ID: "npmrc", Label: "Private npm registry", ApplicationIDs: []string{testServiceID},
		File: builder.FileReference{ID: "npmrc", Path: builder.BuildSecretRoot + "/npmrc"},
	}}
	runtime.SSHSecretProfiles = []BuildSecretProfile{{
		ID: "github", Label: "GitHub deploy key", ApplicationIDs: []string{testServiceID},
		File: builder.FileReference{ID: "github", Path: builder.SSHSecretRoot + "/id_ed25519"},
	}}

	resolved, err := attemptDefinitions([]BuildDefinition{definition}, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolved[0].Execution.BuildSecret; got != runtime.BuildSecret {
		t.Fatalf("build Secret = %q, want current runtime Secret %q", got, runtime.BuildSecret)
	}
	if got := resolved[0].Execution.SSHSecret; got != runtime.SSHSecret {
		t.Fatalf("SSH Secret = %q, want current runtime Secret %q", got, runtime.SSHSecret)
	}

	attempt, err := newAttemptWithExecution(definition, resolved[0].Execution, repositoryFixture(testNow), EnqueuePush{
		ClaimKey: "sha256:" + strings.Repeat("a", 64), CommitSHA: strings.Repeat("b", 40), GitRef: "refs/heads/main", ResolvedAt: testNow,
	}, 1, nil, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.PlanRequest.Build.SecretFiles == nil || len(attempt.PlanRequest.Build.SecretFiles) != 1 || attempt.PlanRequest.Build.SSHFiles == nil || len(attempt.PlanRequest.Build.SSHFiles) != 1 {
		t.Fatalf("secret files were not preserved: %#v", attempt.PlanRequest.Build)
	}
	if attempt.PlanRequest.BuildSecret != runtime.BuildSecret || attempt.PlanRequest.SSHSecret != runtime.SSHSecret {
		t.Fatalf("attempt plan Secret bindings = %q/%q, want %q/%q", attempt.PlanRequest.BuildSecret, attempt.PlanRequest.SSHSecret, runtime.BuildSecret, runtime.SSHSecret)
	}
}
