package gitprojection_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitops"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

const (
	bindingID     = "11111111-1111-4111-8111-111111111111"
	projectID     = "22222222-2222-4222-8222-222222222222"
	environmentID = "33333333-3333-4333-8333-333333333333"
	applicationID = "44444444-4444-4444-8444-444444444444"
	operationID   = "55555555-5555-4555-8555-555555555555"
)

func runGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	command.Env = append(os.Environ(), "LC_ALL=C", "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

type repositoryFixture struct {
	remote, seed, head string
	binding            gitprojection.Binding
	config             []byte
}

type verifierFunc func(context.Context, gitprojection.Binding, gitprojection.ObservationSource) (gitprojection.VerifiedHead, error)

func (f verifierFunc) VerifyTargetHead(ctx context.Context, binding gitprojection.Binding, source gitprojection.ObservationSource) (gitprojection.VerifiedHead, error) {
	return f(ctx, binding, source)
}

func seedRepository(t *testing.T, includeSymlink bool) repositoryFixture {
	t.Helper()
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	seed := filepath.Join(base, "seed")
	os.Mkdir(seed, 0o750) //nolint:errcheck
	runGit(t, base, "init", "--bare", remote)
	runGit(t, seed, "init", "--initial-branch=main")
	binding, err := gitprojection.NewGitHubEnvironmentBinding(bindingID, projectID, environmentID,
		gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 7, RepositoryID: 8, Owner: "kuberploy", Name: "environment"}, "refs/heads/main", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	applicationPath, err := gitprojection.ApplicationPath(binding, applicationID)
	if err != nil {
		t.Fatal(err)
	}
	project := domain.Project{ID: projectID, Slug: "demo"}
	environment := domain.Environment{ID: environmentID, ProjectID: projectID, Slug: "dev"}
	application := domain.Application{ID: applicationID, ProjectID: projectID, Name: "API", Slug: "api"}
	deployment := domain.Deployment{Image: "registry.example/api@sha256:" + strings.Repeat("a", 64), Runtime: domain.DefaultWorkloadRuntime(8080, nil)}
	config, err := gitops.RenderAppConfig(project, environment, application, deployment)
	if err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(seed, filepath.FromSlash(applicationPath))
	if err = os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(full, config, 0o640); err != nil {
		t.Fatal(err)
	}
	dependencies, _ := gitprojection.DependencyPaths(binding)
	for _, dependency := range dependencies {
		fullDependency := filepath.Join(seed, filepath.FromSlash(dependency))
		if err = os.MkdirAll(filepath.Dir(fullDependency), 0o750); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(fullDependency, []byte("apiVersion: variables.kuberploy.io/v1alpha1\nkind: VariableSet\nvalues: {}\n"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if includeSymlink {
		if err = os.Symlink("/etc/passwd", filepath.Join(seed, "untrusted-link")); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, seed, "add", "--all")
	runGit(t, seed, "commit", "-m", "initial")
	head := runGit(t, seed, "rev-parse", "HEAD")
	runGit(t, seed, "remote", "add", "origin", remote)
	runGit(t, seed, "push", "origin", "HEAD:refs/heads/main")
	binding.TargetHeadRevision = head
	binding.TargetHeadObservedAt = time.Now().UTC()
	binding.State = gitprojection.BindingIndexing
	binding.UpdatedAt = binding.TargetHeadObservedAt
	return repositoryFixture{remote: remote, seed: seed, head: head, binding: binding, config: config}
}

func verified(binding gitprojection.Binding, commit, request string, now time.Time) gitprojection.VerifiedHead {
	return gitprojection.VerifiedHead{BindingID: binding.ID, Repository: binding.Repository, TargetRef: binding.TargetRef, Commit: commit, Source: gitprojection.ObservationPoll, ProviderRequest: request, ObservedAt: now.UTC()}
}

func claimProjectionLease(t *testing.T, store gitprojection.Store, now time.Time) gitprojection.ReconciliationLease {
	t.Helper()
	work, err := store.ClaimReconciliation(t.Context(), "projection-test-owner", now, 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return work.Lease
}

func TestProjectionRejectsInvalidRepositoryAndTemporalRegression(t *testing.T) {
	now := time.Now().UTC()
	if _, err := gitprojection.NewEnvironmentBinding(bindingID, projectID, environmentID,
		gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 7, RepositoryID: 8, Owner: "kuberploy", Name: "."},
		"refs/heads/main", "kuberploy-git-writer", now); !errors.Is(err, gitprojection.ErrInvalid) {
		t.Fatalf("dot repository name accepted: %v", err)
	}
	binding, err := gitprojection.NewEnvironmentBinding(bindingID, projectID, environmentID,
		gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 7, RepositoryID: 8, Owner: "kuberploy", Name: "environment"},
		"refs/heads/main", "kuberploy-git-writer", now)
	if err != nil {
		t.Fatal(err)
	}
	store := gitprojection.NewMemoryStore()
	if err = store.PutBinding(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	if err = store.SetBindingState(t.Context(), binding.ID, "", gitprojection.BindingWaiting, now.Add(-time.Second)); !errors.Is(err, gitprojection.ErrConflict) {
		t.Fatalf("binding timestamp regressed: %v", err)
	}
	if _, _, err = store.RecordVerifiedHead(t.Context(), verified(binding, strings.Repeat("a", 40), "old-provider-read", now.Add(-time.Second))); !errors.Is(err, gitprojection.ErrConflict) {
		t.Fatalf("pre-binding provider observation accepted: %v", err)
	}
}

func TestBindingCredentialModesAreExplicitAndMutuallyExclusive(t *testing.T) {
	now := time.Now().UTC()
	repository := gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 7, RepositoryID: 8, Owner: "kuberploy", Name: "environment"}
	binding, err := gitprojection.NewGitHubEnvironmentBinding(bindingID, projectID, environmentID, repository, "refs/heads/main", now)
	if err != nil || binding.CredentialMode != gitprojection.CredentialGitHubApp || binding.CredentialSecretName != "" {
		t.Fatalf("GitHub App binding=%#v err=%v", binding, err)
	}
	binding.CredentialSecretName = "attacker-secret"
	if err = binding.Validate(); !errors.Is(err, gitprojection.ErrInvalid) {
		t.Fatalf("GitHub App binding accepted a Secret: %v", err)
	}
	binding, err = gitprojection.NewEnvironmentBinding(bindingID, projectID, environmentID, repository, "refs/heads/main", "legacy-secret", now)
	if err != nil || binding.CredentialMode != gitprojection.CredentialLegacySecret {
		t.Fatalf("legacy binding=%#v err=%v", binding, err)
	}
	binding.CredentialSecretName = ""
	if err = binding.Validate(); !errors.Is(err, gitprojection.ErrInvalid) {
		t.Fatalf("legacy binding accepted no Secret: %v", err)
	}
	first, err := gitprojection.NewGitHubEnvironmentBinding(bindingID, projectID, environmentID, repository, "refs/heads/main", now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := gitprojection.NewGitHubEnvironmentBinding("66666666-6666-4666-8666-666666666666", projectID, environmentID,
		gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 7, RepositoryID: 9, Owner: "kuberploy", Name: "other"}, "refs/heads/main", now)
	if err != nil {
		t.Fatal(err)
	}
	store := gitprojection.NewMemoryStore()
	if err = store.PutBinding(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	bindings, err := store.BindingsForScope(t.Context(), gitprojection.BindingEnvironment, environmentID)
	if err != nil || len(bindings) != 1 || bindings[0].ID != first.ID {
		t.Fatalf("scope catalog=%#v err=%v", bindings, err)
	}
	bindings[0].Repository.Name = "mutated"
	stored, err := store.Binding(t.Context(), first.ID)
	if err != nil || stored.Repository.Name != repository.Name {
		t.Fatalf("scope catalog leaked mutable state: %#v err=%v", stored, err)
	}
	if err = store.PutBinding(t.Context(), second); !errors.Is(err, gitprojection.ErrConflict) {
		t.Fatalf("second environment authority accepted: %v", err)
	}
	legacy, err := gitprojection.NewEnvironmentBinding("77777777-7777-4777-8777-777777777777", projectID,
		"88888888-8888-4888-8888-888888888888", repository, "refs/heads/main", "legacy-secret", now)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.PutBinding(t.Context(), legacy); err != nil {
		t.Fatal(err)
	}
	work, claimErr := store.ClaimReconciliation(t.Context(), "credential-mode-test", now, time.Minute)
	if claimErr != nil || work.Binding.ID != first.ID {
		t.Fatalf("claim=%#v err=%v", work, claimErr)
	}
	if _, claimErr = store.ClaimReconciliation(t.Context(), "credential-mode-test-2", now, time.Minute); !errors.Is(claimErr, gitprojection.ErrNotFound) {
		t.Fatalf("legacy binding entered reconciliation: %v", claimErr)
	}
}

func TestMirrorManagerHardensSymlinksAndUsesNormalCASPush(t *testing.T) {
	fixture := seedRepository(t, true)
	manager := &gitprojection.MirrorManager{Root: filepath.Join(t.TempDir(), "cache"), AllowLocalTests: true, LocalRemote: fixture.remote, Timeout: 10 * time.Second}
	prepared, err := manager.Prepare(t.Context(), fixture.binding, verified(fixture.binding, fixture.head, "request-1", time.Now()), operationID)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close(context.Background()) //nolint:errcheck
	linkInfo, err := os.Lstat(filepath.Join(prepared.WorktreePath, "untrusted-link"))
	if err != nil {
		t.Fatal(err)
	}
	if linkInfo.Mode()&os.ModeSymlink != 0 {
		t.Fatal("Git symlink was materialized as a filesystem symlink")
	}
	updated := []byte(strings.Replace(string(fixture.config), "replicas: 1", "replicas: 2", 1))
	path, _ := gitprojection.ApplicationPath(fixture.binding, applicationID)
	revision, err := prepared.Commit(t.Context(), gitprojection.Mutation{BindingID: bindingID, OperationID: operationID, Path: path, BaseRevision: fixture.head, ExpectedETag: `"sha256:` + strings.Repeat("b", 64) + `"`, Content: updated, Message: "deploy: update API"})
	if err != nil {
		t.Fatal(err)
	}
	if revision == fixture.head || runGit(t, fixture.remote, "rev-parse", "refs/heads/main") != revision {
		t.Fatal("normal push did not advance exact target ref")
	}
}

func TestPreparedCandidateCommitNeverAdvancesTargetAndRecoversExactRef(t *testing.T) {
	fixture := seedRepository(t, false)
	manager := &gitprojection.MirrorManager{Root: filepath.Join(t.TempDir(), "cache"), AllowLocalTests: true, LocalRemote: fixture.remote}
	prepared, err := manager.Prepare(t.Context(), fixture.binding, verified(fixture.binding, fixture.head, "candidate-request-1", time.Now()), operationID)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := gitprojection.ApplicationPath(fixture.binding, applicationID)
	updated := []byte(strings.Replace(string(fixture.config), "replicas: 1", "replicas: 2", 1))
	mutation := gitprojection.Mutation{BindingID: bindingID, OperationID: operationID, Path: path, BaseRevision: fixture.head,
		ExpectedETag: `"sha256:` + strings.Repeat("b", 64) + `"`, Content: updated, Message: "deploy: open protected review"}
	candidateRef := "refs/heads/kuberploy/operations/" + operationID
	revision, err := prepared.CommitCandidate(t.Context(), mutation, candidateRef)
	if err != nil {
		t.Fatal(err)
	}
	if err = prepared.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if target := runGit(t, fixture.remote, "rev-parse", fixture.binding.TargetRef); target != fixture.head {
		t.Fatalf("candidate advanced target to %s", target)
	}
	if candidate := runGit(t, fixture.remote, "rev-parse", candidateRef); candidate != revision {
		t.Fatalf("candidate=%s want=%s", candidate, revision)
	}

	retry, err := manager.Prepare(t.Context(), fixture.binding, verified(fixture.binding, fixture.head, "candidate-request-2", time.Now()), operationID)
	if err != nil {
		t.Fatal(err)
	}
	defer retry.Close(context.Background()) //nolint:errcheck
	recovered, err := retry.CommitCandidate(t.Context(), mutation, candidateRef)
	if err != nil || recovered != revision {
		t.Fatalf("recovered=%s want=%s err=%v", recovered, revision, err)
	}
	if target := runGit(t, fixture.remote, "rev-parse", fixture.binding.TargetRef); target != fixture.head {
		t.Fatalf("candidate replay advanced target to %s", target)
	}
}

func TestPreparedCandidateCommitRejectsSubstitutedDeterministicRef(t *testing.T) {
	fixture := seedRepository(t, false)
	candidateRef := "refs/heads/kuberploy/operations/" + operationID
	runGit(t, fixture.seed, "branch", "attacker-candidate", fixture.head)
	runGit(t, fixture.seed, "push", "origin", "attacker-candidate:"+candidateRef)
	manager := &gitprojection.MirrorManager{Root: filepath.Join(t.TempDir(), "cache"), AllowLocalTests: true, LocalRemote: fixture.remote}
	prepared, err := manager.Prepare(t.Context(), fixture.binding, verified(fixture.binding, fixture.head, "candidate-substitution", time.Now()), operationID)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close(context.Background()) //nolint:errcheck
	path, _ := gitprojection.ApplicationPath(fixture.binding, applicationID)
	mutation := gitprojection.Mutation{BindingID: bindingID, OperationID: operationID, Path: path, BaseRevision: fixture.head,
		ExpectedETag: `"sha256:` + strings.Repeat("b", 64) + `"`, Content: fixture.config, Message: "must reject substituted ref"}
	if _, err = prepared.CommitCandidate(t.Context(), mutation, candidateRef); !errors.Is(err, gitprojection.ErrConflict) {
		t.Fatalf("substituted candidate accepted: %v", err)
	}
	if target := runGit(t, fixture.remote, "rev-parse", fixture.binding.TargetRef); target != fixture.head {
		t.Fatalf("substitution advanced target to %s", target)
	}
}

func TestPreparedCommitRefusesConcurrentTargetMovement(t *testing.T) {
	fixture := seedRepository(t, false)
	manager := &gitprojection.MirrorManager{Root: filepath.Join(t.TempDir(), "cache"), AllowLocalTests: true, LocalRemote: fixture.remote}
	prepared, err := manager.Prepare(t.Context(), fixture.binding, verified(fixture.binding, fixture.head, "request-1", time.Now()), operationID)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close(context.Background()) //nolint:errcheck
	if err = os.WriteFile(filepath.Join(fixture.seed, "README.md"), []byte("race\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	runGit(t, fixture.seed, "add", "README.md")
	runGit(t, fixture.seed, "commit", "-m", "race")
	racingHead := runGit(t, fixture.seed, "rev-parse", "HEAD")
	runGit(t, fixture.seed, "push", "origin", "HEAD:refs/heads/main")
	path, _ := gitprojection.ApplicationPath(fixture.binding, applicationID)
	_, err = prepared.Commit(t.Context(), gitprojection.Mutation{BindingID: bindingID, OperationID: operationID, Path: path, BaseRevision: fixture.head, ExpectedETag: `"sha256:` + strings.Repeat("b", 64) + `"`, Content: fixture.config, Message: "must conflict"})
	if !errors.Is(err, gitprojection.ErrConflict) {
		t.Fatalf("expected ref CAS conflict, got %v", err)
	}
	if got := runGit(t, fixture.remote, "rev-parse", "refs/heads/main"); got != racingHead {
		t.Fatalf("concurrent head overwritten: %s", got)
	}
}

func TestPreparedCommitCreateIfAbsentUsesRealPathAbsence(t *testing.T) {
	fixture := seedRepository(t, false)
	manager := &gitprojection.MirrorManager{Root: filepath.Join(t.TempDir(), "cache"), AllowLocalTests: true, LocalRemote: fixture.remote}
	prepared, err := manager.Prepare(t.Context(), fixture.binding, verified(fixture.binding, fixture.head, "create-provider-read", time.Now()), operationID)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close(context.Background()) //nolint:errcheck

	existing, _ := gitprojection.ApplicationPath(fixture.binding, applicationID)
	mutation := gitprojection.Mutation{BindingID: bindingID, OperationID: operationID, Path: existing, BaseRevision: fixture.head,
		Precondition: gitprojection.MutationCreateIfAbsent, Content: fixture.config, Message: "create existing"}
	if _, err = prepared.Commit(t.Context(), mutation); !errors.Is(err, gitprojection.ErrConflict) {
		t.Fatalf("create-if-absent overwrote existing path: %v", err)
	}

	newApplication := "66666666-6666-4666-8666-666666666666"
	created, err := gitprojection.ApplicationPath(fixture.binding, newApplication)
	if err != nil {
		t.Fatal(err)
	}
	mutation.Path, mutation.Message = created, "create new application"
	revision, err := prepared.Commit(t.Context(), mutation)
	if err != nil {
		t.Fatal(err)
	}
	if revision == fixture.head || runGit(t, fixture.remote, "show", revision+":"+created) != strings.TrimSpace(string(fixture.config)) {
		t.Fatal("create-if-absent did not commit the exact new path")
	}
}

func TestPreparedRepositoryVerifiesExactPinnedPathContentETag(t *testing.T) {
	fixture := seedRepository(t, false)
	manager := &gitprojection.MirrorManager{Root: filepath.Join(t.TempDir(), "cache"), AllowLocalTests: true, LocalRemote: fixture.remote}
	prepared, err := manager.Prepare(t.Context(), fixture.binding, verified(fixture.binding, fixture.head, "etag-provider-read", time.Now()), operationID)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close(t.Context()) //nolint:errcheck
	applicationPath, _ := gitprojection.ApplicationPath(fixture.binding, applicationID)
	digest := sha256.Sum256(fixture.config)
	expected := `"sha256:` + hex.EncodeToString(digest[:]) + `"`
	if err = prepared.VerifyPathContentETag(t.Context(), applicationPath, expected); err != nil {
		t.Fatalf("exact pinned blob ETag was rejected: %v", err)
	}
	if err = prepared.VerifyPathContentETag(t.Context(), applicationPath, `"sha256:`+strings.Repeat("0", 64)+`"`); !errors.Is(err, gitprojection.ErrConflict) {
		t.Fatalf("forged pinned blob ETag error=%v", err)
	}
	if err = prepared.VerifyPathContentETag(t.Context(), "outside/app.yaml", expected); !errors.Is(err, gitprojection.ErrInvalid) {
		t.Fatalf("path outside binding prefix error=%v", err)
	}
}

func TestPreparedRepositoryVerifiesAncestorAndProtectedPathAbsence(t *testing.T) {
	fixture := seedRepository(t, false)
	if err := os.WriteFile(filepath.Join(fixture.seed, "unrelated.txt"), []byte("later protected change\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	runGit(t, fixture.seed, "add", "unrelated.txt")
	runGit(t, fixture.seed, "commit", "-m", "later protected change")
	descendant := runGit(t, fixture.seed, "rev-parse", "HEAD")
	runGit(t, fixture.seed, "push", "origin", "HEAD:refs/heads/main")
	manager := &gitprojection.MirrorManager{Root: filepath.Join(t.TempDir(), "cache"), AllowLocalTests: true, LocalRemote: fixture.remote}
	prepared, err := manager.Prepare(t.Context(), fixture.binding, verified(fixture.binding, descendant, "receipt-provider-read", time.Now()), operationID)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close(t.Context()) //nolint:errcheck
	if err = prepared.VerifyAncestor(t.Context(), fixture.head); err != nil {
		t.Fatalf("exact planned ancestor was rejected: %v", err)
	}
	if err = prepared.VerifyAncestor(t.Context(), strings.Repeat("f", 40)); !errors.Is(err, gitprojection.ErrConflict) {
		t.Fatalf("unrelated planned base error=%v", err)
	}
	if err = prepared.VerifyAncestor(t.Context(), "main"); !errors.Is(err, gitprojection.ErrInvalid) {
		t.Fatalf("mutable ancestor ref error=%v", err)
	}
	existing, _ := gitprojection.ApplicationPath(fixture.binding, applicationID)
	if err = prepared.VerifyPathAbsent(t.Context(), existing); !errors.Is(err, gitprojection.ErrConflict) {
		t.Fatalf("existing protected path reported absent: %v", err)
	}
	missing, _ := gitprojection.ApplicationPath(fixture.binding, "66666666-6666-4666-8666-666666666666")
	if err = prepared.VerifyPathAbsent(t.Context(), missing); err != nil {
		t.Fatalf("missing protected path was rejected: %v", err)
	}
	for _, unsafePath := range []string{"../escape.yaml", "outside/app.yaml", fixture.binding.Prefix + "/../escape.yaml", fixture.binding.Prefix + "/notes/sibling.yaml"} {
		if err = prepared.VerifyPathAbsent(t.Context(), unsafePath); !errors.Is(err, gitprojection.ErrInvalid) {
			t.Fatalf("unsafe absence path %q error=%v", unsafePath, err)
		}
	}

	const (
		platformBindingID = "77777777-7777-4777-8777-777777777777"
		clusterID         = "88888888-8888-4888-8888-888888888888"
	)
	platform, err := gitprojection.NewGitHubPlatformBinding(platformBindingID, clusterID, fixture.binding.Repository, "refs/heads/main", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	platform.TargetHeadRevision, platform.TargetHeadObservedAt = descendant, time.Now().UTC()
	platform.State, platform.UpdatedAt = gitprojection.BindingIndexing, platform.TargetHeadObservedAt
	platformManager := &gitprojection.MirrorManager{Root: filepath.Join(t.TempDir(), "platform-cache"), AllowLocalTests: true, LocalRemote: fixture.remote}
	platformPrepared, err := platformManager.Prepare(t.Context(), platform, verified(platform, descendant, "platform-receipt-provider-read", time.Now()), operationID)
	if err != nil {
		t.Fatal(err)
	}
	defer platformPrepared.Close(t.Context()) //nolint:errcheck
	argoPath := gitprojection.PlatformPrefix(clusterID) + "/argocd/environments/" + environmentID + ".yaml"
	if err = platformPrepared.VerifyPathAbsent(t.Context(), argoPath); err != nil {
		t.Fatalf("server-owned Argo protected path was rejected: %v", err)
	}
	sibling := gitprojection.PlatformPrefix(clusterID) + "/argocd/projects/" + projectID + ".yaml"
	if err = platformPrepared.VerifyPathAbsent(t.Context(), sibling); !errors.Is(err, gitprojection.ErrInvalid) {
		t.Fatalf("syntactically safe platform sibling was accepted: %v", err)
	}
	if err = platformPrepared.VerifyPathContentETag(t.Context(), sibling, `"sha256:`+strings.Repeat("0", 64)+`"`); !errors.Is(err, gitprojection.ErrInvalid) {
		t.Fatalf("platform sibling ETag proof was accepted: %v", err)
	}
}

func TestPreparedCommitMatchETagRequiresExistingPath(t *testing.T) {
	fixture := seedRepository(t, false)
	manager := &gitprojection.MirrorManager{Root: filepath.Join(t.TempDir(), "cache"), AllowLocalTests: true, LocalRemote: fixture.remote}
	prepared, err := manager.Prepare(t.Context(), fixture.binding, verified(fixture.binding, fixture.head, "update-provider-read", time.Now()), operationID)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close(context.Background()) //nolint:errcheck
	missing, _ := gitprojection.ApplicationPath(fixture.binding, "66666666-6666-4666-8666-666666666666")
	_, err = prepared.Commit(t.Context(), gitprojection.Mutation{BindingID: bindingID, OperationID: operationID, Path: missing, BaseRevision: fixture.head,
		Precondition: gitprojection.MutationMatchETag, ExpectedETag: `"sha256:` + strings.Repeat("b", 64) + `"`, Content: fixture.config, Message: "update absent"})
	if !errors.Is(err, gitprojection.ErrConflict) {
		t.Fatalf("match-etag created an absent path: %v", err)
	}
}

func TestPreparedRepositoryRecoversOnlyExactOperationCommit(t *testing.T) {
	fixture := seedRepository(t, false)
	root := filepath.Join(t.TempDir(), "cache")
	manager := &gitprojection.MirrorManager{Root: root, AllowLocalTests: true, LocalRemote: fixture.remote}
	prepared, err := manager.Prepare(t.Context(), fixture.binding, verified(fixture.binding, fixture.head, "write-provider-read", time.Now()), operationID)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := gitprojection.ApplicationPath(fixture.binding, applicationID)
	updated := []byte(strings.Replace(string(fixture.config), "replicas: 1", "replicas: 2", 1))
	mutation := gitprojection.Mutation{BindingID: bindingID, OperationID: operationID, Path: path, BaseRevision: fixture.head,
		Precondition: gitprojection.MutationMatchETag, ExpectedETag: `"sha256:` + strings.Repeat("b", 64) + `"`, Content: updated, Message: "deploy update"}
	revision, err := prepared.Commit(t.Context(), mutation)
	if err != nil {
		t.Fatal(err)
	}
	if err = prepared.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	recovery, err := manager.Prepare(t.Context(), fixture.binding, verified(fixture.binding, revision, "recovery-provider-read", time.Now()), "77777777-7777-4777-8777-777777777777")
	if err != nil {
		t.Fatal(err)
	}
	defer recovery.Close(context.Background()) //nolint:errcheck
	found, present, err := recovery.FindOperationCommit(t.Context(), mutation)
	if err != nil || !present || found != revision {
		t.Fatalf("recovered=%q present=%v err=%v", found, present, err)
	}
	mutation.Content = fixture.config
	if _, _, err = recovery.FindOperationCommit(t.Context(), mutation); err == nil {
		t.Fatal("operation trailer accepted with different durable content")
	}
	mutation.Content = updated
	mutation.OperationID = "88888888-8888-4888-8888-888888888888"
	if found, present, err = recovery.FindOperationCommit(t.Context(), mutation); err != nil || present || found != "" {
		t.Fatalf("foreign operation recovered=%q present=%v err=%v", found, present, err)
	}
}

func TestWritePlanRequiresReadyExactIndexedBinding(t *testing.T) {
	fixture := seedRepository(t, false)
	fixture.binding.IndexedRevision = fixture.head
	fixture.binding.IndexedAt = fixture.binding.UpdatedAt
	fixture.binding.ProjectionGeneration = 1
	fixture.binding.State = gitprojection.BindingReady
	plan := gitprojection.WritePlan{BindingID: fixture.binding.ID, ProjectID: fixture.binding.ProjectID, EnvironmentID: fixture.binding.EnvironmentID,
		ApplicationID: applicationID, BaseRevision: fixture.head, Precondition: gitprojection.MutationCreateIfAbsent,
		ChartDigest: "sha256:" + strings.Repeat("c", 64), PolicyVersion: "runtime-policy-v1"}
	if err := plan.Validate(fixture.binding); err != nil {
		t.Fatal(err)
	}
	plan.Precondition = gitprojection.MutationMatchETag
	if err := plan.Validate(fixture.binding); !errors.Is(err, gitprojection.ErrInvalid) {
		t.Fatalf("match-etag without an ETag accepted: %v", err)
	}
	plan.ExpectedETag = `"sha256:` + strings.Repeat("d", 64) + `"`
	command, err := gitprojection.NewWriteCommand(operationID, "99999999-9999-4999-8999-999999999999", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", plan, fixture.binding, fixture.config, "save config", time.Now())
	if err != nil || command.Mutation().EffectivePrecondition() != gitprojection.MutationMatchETag {
		t.Fatalf("command=%#v err=%v", command, err)
	}
}

func TestHeadServiceRejectsProviderIdentitySubstitutionAndMarksMissingRef(t *testing.T) {
	fixture := seedRepository(t, false)
	store := gitprojection.NewMemoryStore()
	fixture.binding.TargetHeadRevision = ""
	fixture.binding.TargetHeadObservedAt = time.Time{}
	fixture.binding.State = gitprojection.BindingWaiting
	fixture.binding.UpdatedAt = time.Now().UTC()
	if err := store.PutBinding(t.Context(), fixture.binding); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	service := gitprojection.Service{Store: store, Now: func() time.Time { return now }, Provider: verifierFunc(func(_ context.Context, binding gitprojection.Binding, source gitprojection.ObservationSource) (gitprojection.VerifiedHead, error) {
		head := verified(binding, fixture.head, "provider-substitution", now)
		head.Repository.RepositoryID++
		return head, nil
	})}
	if _, _, err := service.ObserveTargetHead(t.Context(), fixture.binding.ID, gitprojection.ObservationPoll); !errors.Is(err, gitprojection.ErrProviderMismatch) {
		t.Fatalf("provider repository substitution accepted: %v", err)
	}
	service.Provider = verifierFunc(func(context.Context, gitprojection.Binding, gitprojection.ObservationSource) (gitprojection.VerifiedHead, error) {
		return gitprojection.VerifiedHead{}, gitprojection.ErrMissingRef
	})
	if _, _, err := service.ObserveTargetHead(t.Context(), fixture.binding.ID, gitprojection.ObservationPoll); !errors.Is(err, gitprojection.ErrMissingRef) {
		t.Fatalf("missing ref error=%v", err)
	}
	binding, err := store.Binding(t.Context(), fixture.binding.ID)
	if err != nil || binding.State != gitprojection.BindingMissingRef || binding.IndexedRevision != "" {
		t.Fatalf("missing ref state=%#v err=%v", binding, err)
	}
}

func TestMirrorRejectsExpandedTreeBeforeCheckout(t *testing.T) {
	fixture := seedRepository(t, false)
	large := bytes.Repeat([]byte("a"), 2<<20)
	if err := os.WriteFile(filepath.Join(fixture.seed, "compressed-large.txt"), large, 0o640); err != nil {
		t.Fatal(err)
	}
	runGit(t, fixture.seed, "add", "compressed-large.txt")
	runGit(t, fixture.seed, "commit", "-m", "large")
	fixture.head = runGit(t, fixture.seed, "rev-parse", "HEAD")
	runGit(t, fixture.seed, "push", "origin", "HEAD:refs/heads/main")
	fixture.binding.TargetHeadRevision = fixture.head
	fixture.binding.TargetHeadObservedAt = time.Now().UTC()
	fixture.binding.UpdatedAt = fixture.binding.TargetHeadObservedAt
	manager := &gitprojection.MirrorManager{Root: filepath.Join(t.TempDir(), "cache"), AllowLocalTests: true, LocalRemote: fixture.remote, MaxBytes: 512 << 10}
	_, err := manager.Prepare(t.Context(), fixture.binding, verified(fixture.binding, fixture.head, "provider-large", time.Now()), operationID)
	if err == nil || !strings.Contains(err.Error(), "expanded Git worktree") {
		t.Fatalf("expanded tree budget not enforced: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(manager.Root, "worktrees", operationID)); !os.IsNotExist(statErr) {
		t.Fatalf("oversized tree reached checkout: %v", statErr)
	}
}

func TestCoordinatorRunsVerifiedHeadThroughBoundedShadowProjection(t *testing.T) {
	fixture := seedRepository(t, false)
	fixture.binding.TargetHeadRevision = ""
	fixture.binding.TargetHeadObservedAt = time.Time{}
	fixture.binding.IndexedRevision = ""
	fixture.binding.IndexedAt = time.Time{}
	fixture.binding.ProjectionGeneration = 0
	fixture.binding.State = gitprojection.BindingWaiting
	fixture.binding.UpdatedAt = time.Now().UTC()
	store := gitprojection.NewMemoryStore()
	if err := store.PutBinding(t.Context(), fixture.binding); err != nil {
		t.Fatal(err)
	}
	provider := verifierFunc(func(_ context.Context, binding gitprojection.Binding, source gitprojection.ObservationSource) (gitprojection.VerifiedHead, error) {
		return verified(binding, fixture.head, "coordinator-provider-read", time.Now().UTC()), nil
	})
	manager := &gitprojection.MirrorManager{
		Root: filepath.Join(t.TempDir(), "cache"), AllowLocalTests: true, LocalRemote: fixture.remote,
		Timeout: 10 * time.Second, MaxBytes: 64 << 20, MaxFiles: 10_000,
	}
	coordinator := &gitprojection.Coordinator{
		Store: store, Provider: provider,
		Projector: gitprojection.ShadowProjector{
			Manager: manager, Indexer: gitprojection.Indexer{Store: store, Policy: gitprojection.SchemaOnlyAppConfigPolicyValidator{}, MaxDocuments: 1_000, MaxBytes: 8 << 20},
			OperationID: func() (string, error) { return operationID, nil },
		},
		Owner: "projection-repository-test", LeaseDuration: time.Minute, HeartbeatInterval: 10 * time.Second,
		WorkTimeout: time.Minute, PollInterval: time.Minute, MinimumBackoff: time.Second, MaximumBackoff: time.Minute,
		IdleDelay: 10 * time.Millisecond, JitterFraction: 0.2, Random: func() float64 { return 0.5 },
	}
	worked, err := coordinator.ReconcileNext(t.Context())
	if err != nil || !worked {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	binding, err := store.Binding(t.Context(), fixture.binding.ID)
	if err != nil || binding.State != gitprojection.BindingReady || binding.TargetHeadRevision != fixture.head || binding.IndexedRevision != fixture.head || binding.ProjectionGeneration != 1 {
		t.Fatalf("binding=%#v err=%v", binding, err)
	}
	cursor, err := store.PollCursor(t.Context(), fixture.binding.ID)
	if err != nil || cursor.LastCommit != fixture.head || cursor.ConsecutiveFail != 0 {
		t.Fatalf("cursor=%#v err=%v", cursor, err)
	}
	if entries, readErr := os.ReadDir(filepath.Join(manager.Root, "worktrees")); readErr != nil || len(entries) != 0 {
		t.Fatalf("worktree cleanup entries=%v err=%v", entries, readErr)
	}
}

func TestMemoryProjectionAtomicityFreshnessTombstonesAndReservations(t *testing.T) {
	fixture := seedRepository(t, false)
	store := gitprojection.NewMemoryStore()
	fixture.binding.TargetHeadRevision = ""
	fixture.binding.TargetHeadObservedAt = time.Time{}
	fixture.binding.State = gitprojection.BindingWaiting
	fixture.binding.UpdatedAt = time.Now().UTC()
	if err := store.PutBinding(t.Context(), fixture.binding); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	projectionLease := claimProjectionLease(t, store, now)
	binding, _, err := store.RecordVerifiedHead(t.Context(), verified(fixture.binding, fixture.head, "request", now))
	if err != nil {
		t.Fatal(err)
	}
	generation, err := store.BeginGeneration(t.Context(), projectionLease, fixture.head, binding.ParserVersion, now)
	if err != nil {
		t.Fatal(err)
	}
	document, err := gitprojection.NewDocument(binding, generation.Number, applicationID, fixture.head, fixture.head, strings.Repeat("a", 40), fixture.config, map[string]any{"nested": map[string]any{"value": "original"}}, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	invalidDocument := document
	invalidDocument.Path = "../escape"
	if err = store.PutDocuments(t.Context(), generation, []gitprojection.Document{document, invalidDocument}); !errors.Is(err, gitprojection.ErrInvalid) {
		t.Fatalf("invalid projection batch accepted: %v", err)
	}
	if err = store.PutDocuments(t.Context(), generation, []gitprojection.Document{document}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Document(t.Context(), binding.ID, document.Path); !errors.Is(err, gitprojection.ErrNotFound) {
		t.Fatalf("staging generation leaked: %v", err)
	}
	binding, err = store.ActivateGeneration(t.Context(), projectionLease, generation, gitprojection.SchemaOnlyAppConfigPolicyValidator{}, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := store.Bundle(t.Context(), binding.ID, document.Path, nil, "sha256:"+strings.Repeat("c", 64), "policy-v1")
	if err != nil || bundle.Stale {
		t.Fatalf("fresh bundle: %#v %v", bundle, err)
	}
	firstRead, err := store.Document(t.Context(), binding.ID, document.Path)
	if err != nil {
		t.Fatal(err)
	}
	firstRead.Parsed["nested"].(map[string]any)["value"] = "mutated"
	secondRead, err := store.Document(t.Context(), binding.ID, document.Path)
	if err != nil || secondRead.Parsed["nested"].(map[string]any)["value"] != "original" {
		t.Fatalf("projected parsed state leaked through memory copy: %#v %v", secondRead.Parsed, err)
	}
	newHead := strings.Repeat("d", 40)
	if _, _, err = store.RecordVerifiedHead(t.Context(), verified(binding, newHead, "request-2", now.Add(2*time.Second))); err != nil {
		t.Fatal(err)
	}
	bundle, err = store.Bundle(t.Context(), binding.ID, document.Path, nil, "sha256:"+strings.Repeat("c", 64), "policy-v1")
	if err != nil || !bundle.Stale {
		t.Fatalf("head/index lag hidden: %#v %v", bundle, err)
	}
	if _, _, err = store.RecordVerifiedHead(t.Context(), verified(binding, strings.Repeat("e", 40), "stale-provider-response", now.Add(time.Second))); !errors.Is(err, gitprojection.ErrConflict) {
		t.Fatalf("out-of-order provider observation rewound target head: %v", err)
	}

	tombstone := gitprojection.WebhookTombstone{Provider: "github", RepositoryID: binding.Repository.RepositoryID, TargetRef: binding.TargetRef, AfterCommit: newHead, DeliveryHash: "sha256:" + strings.Repeat("e", 64), ReceivedAt: now}
	var claims atomic.Int64
	var wait sync.WaitGroup
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			won, claimErr := store.ClaimWebhook(t.Context(), tombstone)
			if claimErr != nil {
				t.Errorf("claim: %v", claimErr)
			}
			if won {
				claims.Add(1)
			}
		}()
	}
	wait.Wait()
	if claims.Load() != 1 {
		t.Fatalf("webhook claims=%d", claims.Load())
	}

	// Restore an indexed/fresh head before reserving a path.
	repair, err := store.BeginGeneration(t.Context(), projectionLease, newHead, binding.ParserVersion, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	document, err = gitprojection.NewDocument(binding, repair.Number, applicationID, newHead, newHead, strings.Repeat("f", 40), fixture.config, nil, nil, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.PutDocuments(t.Context(), repair, []gitprojection.Document{document}); err != nil {
		t.Fatal(err)
	}
	binding, err = store.ActivateGeneration(t.Context(), projectionLease, repair, gitprojection.SchemaOnlyAppConfigPolicyValidator{}, now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	leaseStart := now.Add(5 * time.Second)
	lease := 30 * time.Second
	until := leaseStart.Add(lease)
	reservation := gitprojection.PathReservation{BindingID: binding.ID, TargetRef: binding.TargetRef, Path: document.Path, OperationID: operationID, Owner: "worker-a", BaseRevision: newHead, State: gitprojection.ReservationCandidate, LeaseUntil: &until, CreatedAt: leaseStart, UpdatedAt: leaseStart}
	if _, _, err = store.AcquirePath(t.Context(), reservation, leaseStart, lease); err != nil {
		t.Fatal(err)
	}
	other := reservation
	other.OperationID = "66666666-6666-4666-8666-666666666666"
	other.Owner = "worker-b"
	other.CreatedAt = until.Add(time.Second)
	other.UpdatedAt = other.CreatedAt
	otherUntil := other.UpdatedAt.Add(lease)
	other.LeaseUntil = &otherUntil
	if _, _, err = store.AcquirePath(t.Context(), other, other.UpdatedAt, lease); !errors.Is(err, gitprojection.ErrLeaseHeld) {
		t.Fatalf("expired reservation was stolen: %v", err)
	}
	if err = store.RepairExpiredPath(t.Context(), binding.ID, binding.TargetRef, document.Path, false, "", until.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.AcquirePath(t.Context(), other, other.UpdatedAt, lease); err != nil {
		t.Fatalf("reservation not released after authoritative repair: %v", err)
	}
}

func TestIndexerUsesExactHeadPreservesPathETagAndRepairsDivergence(t *testing.T) {
	fixture := seedRepository(t, false)
	store := gitprojection.NewMemoryStore()
	fixture.binding.TargetHeadRevision = ""
	fixture.binding.TargetHeadObservedAt = time.Time{}
	fixture.binding.State = gitprojection.BindingWaiting
	fixture.binding.UpdatedAt = time.Now().UTC()
	if err := store.PutBinding(t.Context(), fixture.binding); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	projectionLease := claimProjectionLease(t, store, now)
	binding, _, err := store.RecordVerifiedHead(t.Context(), verified(fixture.binding, fixture.head, "provider-1", now))
	if err != nil {
		t.Fatal(err)
	}
	manager := &gitprojection.MirrorManager{Root: filepath.Join(t.TempDir(), "cache"), AllowLocalTests: true, LocalRemote: fixture.remote}
	prepared, err := manager.Prepare(t.Context(), binding, verified(binding, fixture.head, "provider-1", now), operationID)
	if err != nil {
		t.Fatal(err)
	}
	indexer := gitprojection.Indexer{Store: store, Policy: gitprojection.SchemaOnlyAppConfigPolicyValidator{}}
	binding, err = indexer.Index(t.Context(), projectionLease, prepared, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err = prepared.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	applicationPath, _ := gitprojection.ApplicationPath(binding, applicationID)
	dependencies, _ := gitprojection.DependencyPaths(binding)
	bundle, err := store.Bundle(t.Context(), binding.ID, applicationPath, dependencies, "sha256:"+strings.Repeat("c", 64), "policy-v1")
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Stale || len(bundle.Documents) != 3 || !bundle.Documents[0].Valid {
		t.Fatalf("unexpected indexed bundle: %#v", bundle)
	}
	for _, dependency := range bundle.Documents[1:] {
		values, ok := dependency.Parsed["values"].(map[string]any)
		if !dependency.Valid || !ok || len(values) != 0 {
			t.Fatalf("VariableSet dependency was not strictly parsed: %#v", dependency)
		}
	}
	firstETag := bundle.ETag

	if err = os.WriteFile(filepath.Join(fixture.seed, "README.md"), []byte("unrelated\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	invalidID := "99999999-9999-4999-8999-999999999999"
	invalidPath, _ := gitprojection.ApplicationPath(binding, invalidID)
	invalidFull := filepath.Join(fixture.seed, filepath.FromSlash(invalidPath))
	if err = os.MkdirAll(filepath.Dir(invalidFull), 0o750); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(invalidFull, []byte("apiVersion: [invalid\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	runGit(t, fixture.seed, "add", "--all")
	runGit(t, fixture.seed, "commit", "-m", "unrelated")
	secondHead := runGit(t, fixture.seed, "rev-parse", "HEAD")
	runGit(t, fixture.seed, "push", "origin", "HEAD:refs/heads/main")
	binding, _, err = store.RecordVerifiedHead(t.Context(), verified(binding, secondHead, "provider-2", now.Add(2*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err = manager.Prepare(t.Context(), binding, verified(binding, secondHead, "provider-2", now.Add(2*time.Second)), "66666666-6666-4666-8666-666666666666")
	if err != nil {
		t.Fatal(err)
	}
	binding, err = indexer.Index(t.Context(), projectionLease, prepared, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	prepared.Close(t.Context()) //nolint:errcheck
	bundle, err = store.Bundle(t.Context(), binding.ID, applicationPath, dependencies, "sha256:"+strings.Repeat("c", 64), "policy-v1")
	if err != nil {
		t.Fatal(err)
	}
	if bundle.ETag != firstETag {
		t.Fatalf("unrelated commit changed path-scoped ETag: %s != %s", bundle.ETag, firstETag)
	}
	invalidDocument, err := store.Document(t.Context(), binding.ID, invalidPath)
	if err != nil || invalidDocument.Valid || len(invalidDocument.Diagnostics) == 0 {
		t.Fatalf("invalid manual AppConfig was not indexed diagnostically: %#v %v", invalidDocument, err)
	}

	invalidDependencyPath := dependencies[0]
	invalidDependencyFull := filepath.Join(fixture.seed, filepath.FromSlash(invalidDependencyPath))
	if err = os.WriteFile(invalidDependencyFull, []byte("apiVersion: variables.kuberploy.io/v1alpha1\nkind: VariableSet\nvalues:\n  AMBIGUOUS: true\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	runGit(t, fixture.seed, "add", invalidDependencyPath)
	runGit(t, fixture.seed, "commit", "-m", "invalid inherited variables")
	thirdHead := runGit(t, fixture.seed, "rev-parse", "HEAD")
	runGit(t, fixture.seed, "push", "origin", "HEAD:refs/heads/main")
	binding, _, err = store.RecordVerifiedHead(t.Context(), verified(binding, thirdHead, "provider-3", now.Add(4*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err = manager.Prepare(t.Context(), binding, verified(binding, thirdHead, "provider-3", now.Add(4*time.Second)), "77777777-7777-4777-8777-777777777777")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = indexer.Index(t.Context(), projectionLease, prepared, now.Add(5*time.Second)); !errors.Is(err, gitprojection.ErrInvalid) {
		t.Fatalf("invalid inherited variables activated: %v", err)
	}
	prepared.Close(t.Context()) //nolint:errcheck
	stillActive, err := store.Document(t.Context(), binding.ID, invalidDependencyPath)
	if err != nil || !stillActive.Valid || stillActive.SourceRevision != secondHead {
		t.Fatalf("invalid VariableSet replaced the prior active dependency: %#v %v", stillActive, err)
	}

	if err = os.WriteFile(invalidDependencyFull, []byte("apiVersion: variables.kuberploy.io/v1alpha1\nkind: VariableSet\nvalues: {}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	runGit(t, fixture.seed, "add", invalidDependencyPath)
	runGit(t, fixture.seed, "commit", "-m", "repair inherited variables")
	repairedHead := runGit(t, fixture.seed, "rev-parse", "HEAD")
	runGit(t, fixture.seed, "push", "origin", "HEAD:refs/heads/main")
	binding, _, err = store.RecordVerifiedHead(t.Context(), verified(binding, repairedHead, "provider-repair", now.Add(6*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err = manager.Prepare(t.Context(), binding, verified(binding, repairedHead, "provider-repair", now.Add(6*time.Second)), "78777777-7777-4777-8777-777777777777")
	if err != nil {
		t.Fatal(err)
	}
	if binding, err = indexer.Index(t.Context(), projectionLease, prepared, now.Add(7*time.Second)); err != nil {
		t.Fatal(err)
	}
	prepared.Close(t.Context()) //nolint:errcheck

	tree := runGit(t, fixture.seed, "rev-parse", "HEAD^{tree}")
	rewrite := runGit(t, fixture.seed, "commit-tree", tree, "-m", "rewritten root")
	runGit(t, fixture.seed, "push", "--force", "origin", rewrite+":refs/heads/main")
	binding, _, err = store.RecordVerifiedHead(t.Context(), verified(binding, rewrite, "provider-4", now.Add(8*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err = manager.Prepare(t.Context(), binding, verified(binding, rewrite, "provider-4", now.Add(8*time.Second)), "88888888-8888-4888-8888-888888888888")
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close(context.Background()) //nolint:errcheck
	if _, err = indexer.Index(t.Context(), projectionLease, prepared, now.Add(9*time.Second)); !errors.Is(err, gitprojection.ErrDiverged) {
		t.Fatalf("force push not detected: %v", err)
	}
	if binding, err = indexer.FullReindex(t.Context(), projectionLease, prepared, now.Add(10*time.Second)); err != nil || binding.IndexedRevision != rewrite {
		t.Fatalf("full shadow repair failed: %#v %v", binding, err)
	}
}

func TestIndexerActivatesPlatformBindingWithoutParsingTenantDocuments(t *testing.T) {
	fixture := seedRepository(t, false)
	const platformClusterID = "88888888-8888-4888-8888-888888888888"
	fixture.binding.Kind = gitprojection.BindingPlatform
	fixture.binding.ScopeID = platformClusterID
	fixture.binding.ProjectID = ""
	fixture.binding.EnvironmentID = ""
	fixture.binding.ClusterID = platformClusterID
	fixture.binding.Prefix = gitprojection.PlatformPrefix(platformClusterID)
	fixture.binding.TargetHeadRevision = ""
	fixture.binding.TargetHeadObservedAt = time.Time{}
	fixture.binding.State = gitprojection.BindingWaiting
	fixture.binding.UpdatedAt = time.Now().UTC()

	store := gitprojection.NewMemoryStore()
	if err := store.PutBinding(t.Context(), fixture.binding); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	lease := claimProjectionLease(t, store, now)
	binding, _, err := store.RecordVerifiedHead(t.Context(), verified(fixture.binding, fixture.head, "platform-provider", now))
	if err != nil {
		t.Fatal(err)
	}
	manager := &gitprojection.MirrorManager{Root: filepath.Join(t.TempDir(), "cache"), AllowLocalTests: true, LocalRemote: fixture.remote}
	prepared, err := manager.Prepare(t.Context(), binding, verified(binding, fixture.head, "platform-provider", now), operationID)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close(t.Context()) //nolint:errcheck
	binding, err = (gitprojection.Indexer{Store: store, Policy: gitprojection.SchemaOnlyAppConfigPolicyValidator{}}).Index(t.Context(), lease, prepared, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if binding.State != gitprojection.BindingReady || binding.IndexedRevision != fixture.head || binding.ProjectionGeneration != 1 {
		t.Fatalf("platform head did not activate: %#v", binding)
	}
}
