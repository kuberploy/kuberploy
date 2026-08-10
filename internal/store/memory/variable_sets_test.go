package memory

import (
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func TestVariableSetSaveBindsRequestDigestIndependentlyOfCandidateBytes(t *testing.T) {
	ctx := t.Context()
	store := New()
	admin := bootstrapAccessAdmin(t, store)
	project, err := store.CreateProject(ctx, admin.ID, "variable-project", "variable-project", domain.CreateProject{Name: "Variables", Slug: "variables"})
	if err != nil {
		t.Fatal(err)
	}
	environment, err := store.CreateEnvironment(ctx, admin.ID, "variable-environment", "variable-environment", domain.CreateEnvironment{
		ProjectID: project.Value.ID, Name: "Protected", Slug: "protected", ProtectionPolicy: domain.EnvironmentProtected,
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := readyEnvironmentBinding(t, project.Value.ID, environment.Value.ID, time.Now().UTC().Add(-time.Minute))
	if err = store.PutBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}
	paths, err := gitprojection.DependencyPaths(binding)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("apiVersion: variables.kuberploy.io/v1alpha1\nkind: VariableSet\nvalues:\n  REGION: ap-southeast-1\n")
	candidateHash := sha256.Sum256(raw)
	type saved struct {
		operationID string
		command     gitprojection.WriteCommand
	}
	save := func(scope, path, key, fingerprint, tokenSeed string) saved {
		t.Helper()
		plan := gitprojection.WritePlan{BindingID: binding.ID, ProjectID: project.Value.ID, EnvironmentID: environment.Value.ID,
			BaseRevision: binding.IndexedRevision, Precondition: gitprojection.MutationCreateIfAbsent, PolicyVersion: binding.ParserVersion,
			VariableScope: scope, VariablePath: path}
		tokenHash := sha256.Sum256([]byte(tokenSeed))
		if previewErr := store.CreateVariableSetPreview(ctx, admin.ID, plan, tokenHash[:], candidateHash[:], time.Now().UTC().Add(10*time.Minute)); previewErr != nil {
			t.Fatal(previewErr)
		}
		result, saveErr := store.SaveVariableSet(ctx, admin.ID, key, fingerprint, "request-"+scope, plan, tokenHash[:], candidateHash[:], raw)
		if saveErr != nil {
			t.Fatal(saveErr)
		}
		command, commandErr := store.AcceptedGitWriteCommand(result.Value.ID)
		if commandErr != nil {
			t.Fatal(commandErr)
		}
		return saved{operationID: result.Value.ID, command: command}
	}
	projectDigest := "sha256:" + strings.Repeat("1", 64)
	environmentDigest := "sha256:" + strings.Repeat("2", 64)
	projectSave := save("project", paths[0], "project-save", projectDigest, "project-preview")
	environmentSave := save("environment", paths[1], "environment-save", environmentDigest, "environment-preview")
	if projectSave.command.ContentSHA256 != environmentSave.command.ContentSHA256 {
		t.Fatal("identical candidate bytes produced different content digests")
	}
	if projectSave.command.RequestDigest != projectDigest || environmentSave.command.RequestDigest != environmentDigest ||
		projectSave.command.RequestDigest == projectSave.command.ContentSHA256 || projectSave.command.RequestDigest == environmentSave.command.RequestDigest {
		t.Fatalf("request authority was not target/key-specific: project=%#v environment=%#v", projectSave.command, environmentSave.command)
	}
	// A preview pins the parser policy that authorized it. Advancing only the
	// binding policy must invalidate new work while leaving accepted receipts
	// replayable.
	driftPlan := projectSave.command.Plan
	driftToken := sha256.Sum256([]byte("parser-drift-preview"))
	if err = store.CreateVariableSetPreview(ctx, admin.ID, driftPlan, driftToken[:], candidateHash[:], time.Now().UTC().Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	changedBinding := store.gitBindings[environment.Value.ID]
	changedBinding.ParserVersion = "variables/v2"
	store.gitBindings[environment.Value.ID] = changedBinding
	pinned, _, err := store.VariableSetPreviewAuthority(ctx, admin.ID, driftToken[:])
	if err != nil || pinned.PolicyVersion != driftPlan.PolicyVersion {
		t.Fatalf("preview policy was not immutable: plan=%#v err=%v", pinned, err)
	}
	if _, err = store.SaveVariableSet(ctx, admin.ID, "parser-drift-save", "sha256:"+strings.Repeat("3", 64), "parser-drift", pinned, driftToken[:], candidateHash[:], raw); !errors.Is(err, base.ErrPreconditionFailed) {
		t.Fatalf("preview survived parser policy advancement: %v", err)
	}
	// Lost-response replay is an immutable receipt lookup: later revocation and
	// projection readiness changes must not make the accepted result disappear.
	for grantID := range store.accessGrants {
		delete(store.accessGrants, grantID)
	}
	changedBinding = store.gitBindings[environment.Value.ID]
	changedBinding.State = gitprojection.BindingIndexing
	store.gitBindings[environment.Value.ID] = changedBinding
	replayed, err := store.SaveVariableSet(ctx, admin.ID, "project-save", projectDigest, "lost-response-replay", gitprojection.WritePlan{}, nil, nil, nil)
	if err != nil || !replayed.Replay || replayed.Value.ID != projectSave.operationID {
		t.Fatalf("accepted replay depended on current plan/preview/parser material: %#v err=%v", replayed, err)
	}

	projectPlan := projectSave.command.Plan
	token := sha256.Sum256([]byte("unused-replay-token"))
	if _, err = store.SaveVariableSet(ctx, admin.ID, "project-save", environmentDigest, "substituted-replay", projectPlan, token[:], candidateHash[:], raw); !errors.Is(err, base.ErrIdempotencyConflict) {
		t.Fatalf("same key with substituted target digest accepted: %v", err)
	}
}
