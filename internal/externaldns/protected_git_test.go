package externaldns

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

func TestProtectedOperationIdentityIsStableForRecoveryAndRevisionBound(t *testing.T) {
	item := runtimeIntegration()
	config := ProtectedGitConfig{BindingID: "33333333-3333-4333-8333-333333333333", Owner: "edge-worker:recovery-test", Template: runtimeTemplate()}
	content, _, err := RenderManagedBundle(item, config.Template)
	if err != nil {
		t.Fatal(err)
	}
	contentDigest := digest(content)
	first := externalDNSOperationID(config, item, gitprojection.MutationUpsert, contentDigest)
	second := externalDNSOperationID(config, item, gitprojection.MutationUpsert, contentDigest)
	if first != second {
		t.Fatalf("recovery operation identity drifted: %s %s", first, second)
	}
	revised := item
	revised.RuntimeRevision++
	revisedID := externalDNSOperationID(config, revised, gitprojection.MutationUpsert, contentDigest)
	deletedID := externalDNSOperationID(config, item, gitprojection.MutationDelete, contentDigest)
	if revisedID == first || deletedID == first || deletedID == revisedID {
		t.Fatal("operation identity did not bind revision and action")
	}
}

type publicationFixture struct {
	remote, seed string
	now          time.Time
	binding      gitprojection.Binding
	store        *gitprojection.MemoryStore
	manager      *gitprojection.MirrorManager
	config       ProtectedGitConfig
}

func publicationGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func newPublicationFixture(t *testing.T) *publicationFixture {
	t.Helper()
	remote, seed := filepath.Join(t.TempDir(), "remote.git"), filepath.Join(t.TempDir(), "seed")
	publicationGit(t, "", "init", "--bare", remote)
	publicationGit(t, "", "init", seed)
	publicationGit(t, seed, "config", "user.name", "Kuberploy Test")
	publicationGit(t, seed, "config", "user.email", "test@kuberploy.invalid")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("platform\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	publicationGit(t, seed, "add", ".")
	publicationGit(t, seed, "commit", "-m", "seed")
	publicationGit(t, seed, "branch", "-M", "main")
	publicationGit(t, seed, "remote", "add", "origin", remote)
	publicationGit(t, seed, "push", "origin", "main")
	now := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	binding, err := gitprojection.NewGitHubPlatformBinding("33333333-3333-4333-8333-333333333333", gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 1, RepositoryID: 2, Owner: "kuberploy", Name: "platform"}, "refs/heads/main", now)
	if err != nil {
		t.Fatal(err)
	}
	base := publicationGit(t, seed, "rev-parse", "HEAD")
	binding.State, binding.TargetHeadRevision, binding.IndexedRevision, binding.ProjectionGeneration = gitprojection.BindingReady, base, base, 1
	binding.TargetHeadObservedAt, binding.IndexedAt, binding.UpdatedAt = now, now, now
	store := gitprojection.NewMemoryStore()
	if err := store.PutBinding(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(t.TempDir(), "cache")
	if err := os.Mkdir(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	return &publicationFixture{remote: remote, seed: seed, now: now, binding: binding, store: store, manager: &gitprojection.MirrorManager{Root: cache, AllowLocalTests: true, LocalRemote: remote}, config: ProtectedGitConfig{BindingID: binding.ID, Owner: "externaldns-worker:test", Template: runtimeTemplate()}}
}

func (f *publicationFixture) VerifyTargetHead(ctx context.Context, binding gitprojection.Binding, source gitprojection.ObservationSource) (gitprojection.VerifiedHead, error) {
	command := exec.CommandContext(ctx, "git", "--git-dir", f.remote, "rev-parse", binding.TargetRef)
	output, err := command.Output()
	return gitprojection.VerifiedHead{BindingID: binding.ID, Repository: binding.Repository, TargetRef: binding.TargetRef, Commit: strings.TrimSpace(string(output)), Source: source, ProviderRequest: "github:externaldns-test", ObservedAt: f.now}, err
}

func (f *publicationFixture) publisher(t *testing.T, store gitprojection.Store) *ProtectedPublisher {
	t.Helper()
	p, err := NewProtectedPublisher(store, f, f.manager, f.config, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *publicationFixture) advance(t *testing.T, documentPath string, content []byte, elapsed time.Duration) {
	t.Helper()
	full := filepath.Join(f.seed, filepath.FromSlash(documentPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, content, 0o600); err != nil {
		t.Fatal(err)
	}
	publicationGit(t, f.seed, "add", ".")
	publicationGit(t, f.seed, "commit", "-m", "independent change")
	publicationGit(t, f.seed, "push", "origin", "main")
	f.now = f.now.Add(elapsed)
	observation, err := f.VerifyTargetHead(t.Context(), f.binding, gitprojection.ObservationWrite)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.store.RecordVerifiedHead(t.Context(), observation); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Second)
	work, err := f.store.ClaimReconciliation(t.Context(), "externaldns-test-projection", f.now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := f.store.BeginGeneration(t.Context(), work.Lease, observation.Commit, f.binding.ParserVersion, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.PutDocuments(t.Context(), generation, []gitprojection.Document{}); err != nil {
		t.Fatal(err)
	}
	f.binding, err = f.store.ActivateGeneration(t.Context(), work.Lease, generation, gitprojection.SchemaOnlyAppConfigPolicyValidator{}, f.now)
	if err != nil {
		t.Fatal(err)
	}

}

func TestProtectedPublisherRecoversExpiredAbsentReservationAfterHeadAdvances(t *testing.T) {
	for _, scenario := range []string{"unrelated", "same-path", "active-lease", "foreign-owner-active", "foreign-owner-expired", "foreign-operation"} {
		t.Run(scenario, func(t *testing.T) {
			f := newPublicationFixture(t)
			item := runtimeIntegration()
			content, _, err := RenderManagedBundle(item, f.config.Template)
			if err != nil {
				t.Fatal(err)
			}
			documentPath := path.Join(gitprojection.PlatformPrefix(), "argocd", "platform", "external-dns", item.ID+".yaml")
			operationID := externalDNSOperationID(f.config, item, gitprojection.MutationUpsert, digest(content))
			lease := f.now.Add(90 * time.Second)
			owner := f.config.Owner
			if strings.HasPrefix(scenario, "foreign-owner") {
				owner = "other-worker:reserved"
			}
			if scenario == "foreign-operation" {
				operationID = "44444444-4444-4444-8444-444444444444"
			}
			reservation := gitprojection.PathReservation{BindingID: f.binding.ID, TargetRef: f.binding.TargetRef, Path: documentPath, OperationID: operationID, Owner: owner, BaseRevision: f.binding.IndexedRevision, State: gitprojection.ReservationCandidate, LeaseUntil: &lease, CreatedAt: f.now, UpdatedAt: f.now}
			if _, _, err := f.store.AcquirePath(t.Context(), reservation, f.now, 90*time.Second); err != nil {
				t.Fatal(err)
			}
			elapsed := 2 * time.Minute
			if scenario == "active-lease" || scenario == "foreign-owner-active" {
				elapsed = time.Minute
			}
			if scenario == "same-path" {
				f.advance(t, documentPath, content, elapsed)
			} else {
				f.advance(t, "unrelated.md", []byte("independent\n"), elapsed)
			}
			publisher := f.publisher(t, f.store)
			if _, err := publisher.Reconcile(t.Context(), item); err == nil {
				t.Fatal("first attempt must retry or reject")
			}
			kept, err := f.store.PathReservation(t.Context(), f.binding.ID, f.binding.TargetRef, documentPath)
			if scenario != "unrelated" && scenario != "foreign-owner-expired" {
				if err != nil || kept.OperationID != reservation.OperationID || kept.BaseRevision != reservation.BaseRevision {
					t.Fatalf("reservation changed without authority: %#v %v", kept, err)
				}
				return
			}
			if !errors.Is(err, gitprojection.ErrNotFound) {
				t.Fatalf("expired absent reservation still fences the path: %#v %v", kept, err)
			}
			receipt, err := publisher.Reconcile(t.Context(), item)
			if err != nil || !receipt.Changed || receipt.CommittedRevision == f.binding.IndexedRevision {
				t.Fatalf("fresh CAS retry failed: %#v %v", receipt, err)
			}
			if got := publicationGit(t, "", "--git-dir", f.remote, "show", "refs/heads/main:unrelated.md"); got != "independent" {
				t.Fatal("unrelated path changed")
			}
		})
	}
}

type publicationLostReceiptStore struct {
	gitprojection.Store
	fail bool
}

func (s *publicationLostReceiptStore) FinalizePath(ctx context.Context, bindingID, ref, documentPath, operationID, revision string, now time.Time) (gitprojection.PathReservation, error) {
	if s.fail {
		s.fail = false
		return gitprojection.PathReservation{}, errors.New("simulated lost receipt")
	}
	return s.Store.FinalizePath(ctx, bindingID, ref, documentPath, operationID, revision, now)
}

func TestProtectedPublisherRecoversAcceptedPushWithoutDuplicateCommit(t *testing.T) {
	f := newPublicationFixture(t)
	publisher := f.publisher(t, &publicationLostReceiptStore{Store: f.store, fail: true})
	if _, err := publisher.Reconcile(t.Context(), runtimeIntegration()); err == nil {
		t.Fatal("expected lost receipt")
	}
	first := publicationGit(t, "", "--git-dir", f.remote, "rev-parse", "refs/heads/main")
	receipt, err := publisher.Reconcile(t.Context(), runtimeIntegration())
	if err != nil || receipt.CommittedRevision != first {
		t.Fatalf("accepted push recovery failed: %#v %v", receipt, err)
	}
	if next := publicationGit(t, "", "--git-dir", f.remote, "rev-parse", "refs/heads/main"); next != first {
		t.Fatal("recovery created another commit")
	}
}

func TestProtectedPublisherDeletesOnlyExactLegacyBundle(t *testing.T) {
	for _, substituted := range []bool{false, true} {
		t.Run(fmt.Sprint(substituted), func(t *testing.T) {
			f := newPublicationFixture(t)
			item := runtimeIntegration()
			legacy, _, err := renderManagedBundle(item, f.config.Template, true)
			if err != nil {
				t.Fatal(err)
			}
			if substituted {
				legacy = []byte(strings.ReplaceAll(string(legacy), "cloudflare-credentials", "another-provider-credential"))
			}
			documentPath := path.Join(gitprojection.PlatformPrefix(), "argocd", "platform", "external-dns", item.ID+".yaml")
			f.advance(t, documentPath, legacy, time.Second)
			base := f.binding.IndexedRevision
			item.Lifecycle = "deactivated"
			receipt, err := f.publisher(t, f.store).Reconcile(t.Context(), item)
			if substituted {
				if !errors.Is(err, gitprojection.ErrConflict) || publicationGit(t, "", "--git-dir", f.remote, "rev-parse", "refs/heads/main") != base {
					t.Fatalf("changed legacy content was not fenced: %#v %v", receipt, err)
				}
				return
			}
			if err != nil || !receipt.Deleted || !receipt.Changed {
				t.Fatalf("legacy deactivation failed: %#v %v", receipt, err)
			}
			if publicationGit(t, "", "--git-dir", f.remote, "ls-tree", "-r", "--name-only", receipt.CommittedRevision, "--", documentPath) != "" || publicationGit(t, "", "--git-dir", f.remote, "show", receipt.CommittedRevision+":README.md") != "platform" {
				t.Fatal("deactivation did not delete only the legacy path")
			}
		})
	}
}

func TestProtectedPublisherRecoversOnlyKnownExpiredLegacyOperation(t *testing.T) {
	for _, scenario := range []string{"expired", "foreign-active", "foreign-operation", "changed-path"} {
		t.Run(scenario, func(t *testing.T) {
			f := newPublicationFixture(t)
			item := runtimeIntegration()
			legacy, _, err := renderManagedBundle(item, f.config.Template, true)
			if err != nil {
				t.Fatal(err)
			}
			documentPath := path.Join(gitprojection.PlatformPrefix(), "argocd", "platform", "external-dns", item.ID+".yaml")
			operation := externalDNSOperationID(f.config, item, gitprojection.MutationUpsert, digest(legacy))
			if scenario == "foreign-operation" {
				operation = "44444444-4444-4444-8444-444444444444"
			}
			lease := f.now.Add(90 * time.Second)
			reservation := gitprojection.PathReservation{BindingID: f.binding.ID, TargetRef: f.binding.TargetRef, Path: documentPath, OperationID: operation, Owner: "previous-worker:legacy", BaseRevision: f.binding.IndexedRevision, State: gitprojection.ReservationCandidate, LeaseUntil: &lease, CreatedAt: f.now, UpdatedAt: f.now}
			if _, _, err = f.store.AcquirePath(t.Context(), reservation, f.now, 90*time.Second); err != nil {
				t.Fatal(err)
			}
			elapsed := 2 * time.Minute
			if scenario == "foreign-active" {
				elapsed = time.Minute
			}
			if scenario == "changed-path" {
				f.advance(t, documentPath, legacy, elapsed)
			} else {
				f.advance(t, "unrelated.md", []byte("preserved\n"), elapsed)
			}
			publisher := f.publisher(t, f.store)
			if _, err = publisher.Reconcile(t.Context(), item); err == nil {
				t.Fatal("first legacy recovery must retry or reject")
			}
			kept, reservationErr := f.store.PathReservation(t.Context(), f.binding.ID, f.binding.TargetRef, documentPath)
			if scenario != "expired" {
				if reservationErr != nil || kept.OperationID != reservation.OperationID || kept.BaseRevision != reservation.BaseRevision {
					t.Fatalf("unproven reservation was changed: %#v %v", kept, reservationErr)
				}
				return
			}
			if !errors.Is(reservationErr, gitprojection.ErrNotFound) {
				t.Fatal("known expired legacy candidate remains reserved")
			}
			receipt, err := publisher.Reconcile(t.Context(), item)
			if err != nil || !receipt.Changed {
				t.Fatalf("fresh publication after legacy recovery failed: %#v %v", receipt, err)
			}
			expected, _, _ := RenderManagedBundle(item, f.config.Template)
			if publicationGit(t, "", "--git-dir", f.remote, "show", receipt.CommittedRevision+":"+documentPath) != strings.TrimSpace(string(expected)) {
				t.Fatal("legacy recovery did not publish current account ownership")
			}
		})
	}
}

func TestProtectedPublisherRecoversAcceptedLegacyPushBeforeUpgrade(t *testing.T) {
	f := newPublicationFixture(t)
	item := runtimeIntegration()
	legacy, _, err := renderManagedBundle(item, f.config.Template, true)
	if err != nil {
		t.Fatal(err)
	}
	publisher := f.publisher(t, &publicationLostReceiptStore{Store: f.store, fail: true})
	if _, err = publisher.publish(t.Context(), item, legacy, gitprojection.MutationUpsert); err == nil {
		t.Fatal("expected lost legacy publication receipt")
	}
	first := publicationGit(t, "", "--git-dir", f.remote, "rev-parse", "refs/heads/main")
	if _, err = publisher.Reconcile(t.Context(), item); !errors.Is(err, gitprojection.ErrStale) {
		t.Fatalf("accepted legacy publication did not wait for projection: %v", err)
	}
	if next := publicationGit(t, "", "--git-dir", f.remote, "rev-parse", "refs/heads/main"); next != first {
		t.Fatal("legacy recovery created a duplicate commit")
	}
}
