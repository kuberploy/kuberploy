package environmentfoundation

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

type localHeadVerifier struct {
	remote string
	now    time.Time
	calls  int
	failAt int
}

func (v *localHeadVerifier) VerifyTargetHead(ctx context.Context, binding gitprojection.Binding, source gitprojection.ObservationSource) (gitprojection.VerifiedHead, error) {
	v.calls++
	if source != gitprojection.ObservationWrite {
		return gitprojection.VerifiedHead{}, errors.New("wrong observation authority")
	}
	if v.failAt == v.calls {
		return gitprojection.VerifiedHead{}, errors.New("simulated provider outage")
	}
	command := exec.CommandContext(ctx, "git", "--git-dir", v.remote, "rev-parse", binding.TargetRef)
	output, err := command.Output()
	if err != nil {
		return gitprojection.VerifiedHead{}, err
	}
	return gitprojection.VerifiedHead{BindingID: binding.ID, Repository: binding.Repository,
		TargetRef: binding.TargetRef, Commit: strings.TrimSpace(string(output)), Source: source,
		ProviderRequest: "github:foundation-test", ObservedAt: v.now}, nil
}

type crashAfterBindStore struct {
	Store
	crash bool
}

func (s *crashAfterBindStore) BindWriteBase(ctx context.Context, lease Lease, revision string, observedAt, now time.Time) (Intent, error) {
	intent, err := s.Store.BindWriteBase(ctx, lease, revision, observedAt, now)
	if err == nil && s.crash {
		s.crash = false
		return Intent{}, ErrUnavailable
	}
	return intent, err
}

type publisherFixture struct {
	store     *MemoryStore
	bindings  *gitprojection.MemoryStore
	verifier  *localHeadVerifier
	publisher *ProtectedGitPublisher
	lease     Lease
	request   PublicationRequest
	base      string
	now       time.Time
}

func secondPublisherIdentity() EnvironmentIdentity {
	return EnvironmentIdentity{"77777777-7777-4777-8777-777777777777", "88888888-8888-4888-8888-888888888888", "kp-demo-two", "kp-demo-two"}
}

func newPublisherFixture(t *testing.T) publisherFixture {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	remote, work := filepath.Join(t.TempDir(), "remote.git"), filepath.Join(t.TempDir(), "seed")
	runFoundationGit(t, "", "init", "--bare", remote)
	runFoundationGit(t, "", "init", work)
	runFoundationGit(t, work, "config", "user.name", "Kuberploy Test")
	runFoundationGit(t, work, "config", "user.email", "test@kuberploy.invalid")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("platform\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runFoundationGit(t, work, "add", "README.md")
	runFoundationGit(t, work, "commit", "-m", "seed")
	runFoundationGit(t, work, "branch", "-M", "main")
	runFoundationGit(t, work, "remote", "add", "origin", remote)
	runFoundationGit(t, work, "push", "origin", "main")
	base := strings.TrimSpace(runFoundationGit(t, work, "rev-parse", "HEAD"))

	repository := gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 1, RepositoryID: 2, Owner: "kuberploy", Name: "platform"}
	binding, err := gitprojection.NewGitHubPlatformBinding(testBindingID, repository, "refs/heads/main", now)
	if err != nil {
		t.Fatal(err)
	}
	binding.ProjectionGeneration = 7
	bindings := gitprojection.NewMemoryStore()
	if err = bindings.PutBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}
	authority := testAuthority()
	authority.PlannedHead = base
	store, err := NewMemoryStore([]AuthorityRecord{{testIdentity(), authority}, {secondPublisherIdentity(), authority}})
	if err != nil {
		t.Fatal(err)
	}
	profile := testProfile()
	intent, err := store.EnsureIntent(ctx, EnsureRequest{testIntentID, testEnvironmentID, profile, now})
	if err != nil {
		t.Fatal(err)
	}
	profileDigest, _ := profile.Digest()
	lease, found, err := store.ClaimIntent(ctx, testWorker1, profileDigest, profile.PublisherConfigDigest, now.Add(time.Second), time.Minute)
	if err != nil || !found {
		t.Fatalf("claim: found=%v err=%v", found, err)
	}
	verifier := &localHeadVerifier{remote: remote, now: now.Add(2 * time.Second)}
	cacheRoot := filepath.Join(t.TempDir(), "git-cache")
	if err = os.Mkdir(cacheRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	publisher := &ProtectedGitPublisher{Store: store, Bindings: bindings, Provider: verifier,
		Manager:   &gitprojection.MirrorManager{Root: cacheRoot, AllowLocalTests: true, LocalRemote: remote},
		Publisher: PublisherIdentity{PublisherContract, ProtectedGitPolicy, profile.PublisherConfigDigest},
		Now:       func() time.Time { return now.Add(5 * time.Second) }}
	return publisherFixture{store, bindings, verifier, publisher, lease, publicationFor(intent), base, now}
}

func TestProtectedGitPublisherSerializesTwoIntentsFromOnePlannedHead(t *testing.T) {
	fixture := newPublisherFixture(t)
	ctx := context.Background()
	profile := testProfile()
	profileDigest, _ := profile.Digest()
	secondIdentity := secondPublisherIdentity()
	secondIntent, err := fixture.store.EnsureIntent(ctx, EnsureRequest{testIntentID2, secondIdentity.EnvironmentID, profile, fixture.now})
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := fixture.store.ClaimIntent(ctx, testWorker2, profileDigest, profile.PublisherConfigDigest, fixture.now.Add(2*time.Second), time.Minute); err != nil || found {
		t.Fatalf("second worker entered claimed binding: found=%v err=%v", found, err)
	}
	firstReceipt, err := fixture.publisher.Publish(ctx, fixture.lease, fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	firstCurrent, err := fixture.store.Intent(ctx, fixture.lease.Intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.lease.Intent = firstCurrent
	if _, err = fixture.store.RecordReady(ctx, fixture.lease, firstReceipt, fixture.now.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	fixture.publisher.Now = func() time.Time { return fixture.now.Add(20 * time.Second) }
	secondLease, found, err := fixture.store.ClaimIntent(ctx, testWorker2, profileDigest, profile.PublisherConfigDigest, fixture.now.Add(7*time.Second), time.Minute)
	if err != nil || !found || secondLease.Intent.ID != secondIntent.ID {
		t.Fatalf("second intent was not released: %#v found=%v err=%v", secondLease, found, err)
	}
	secondReceipt, err := fixture.publisher.Publish(ctx, secondLease, publicationFor(secondLease.Intent))
	if err != nil {
		t.Fatal(err)
	}
	if secondReceipt.ParentRevision != firstReceipt.CommittedRevision || secondReceipt.ParentRevision == fixture.base {
		t.Fatalf("second intent did not bind the verified descendant: first=%#v second=%#v", firstReceipt, secondReceipt)
	}
}

func TestProtectedGitPublisherDeletesExactPublishedFoundation(t *testing.T) {
	fixture := newPublisherFixture(t)
	ctx := context.Background()
	receipt, err := fixture.publisher.Publish(ctx, fixture.lease, fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	current, err := fixture.store.Intent(ctx, fixture.lease.Intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.lease.Intent = current
	if _, err = fixture.store.RecordReady(ctx, fixture.lease, receipt, fixture.now.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	updated := fixture.now.Add(7 * time.Second)
	until := updated.Add(time.Minute)
	deletion := Deletion{ID: testIntentID2, EnvironmentID: current.EnvironmentID, ProjectID: current.ProjectID,
		Namespace: current.Namespace, ArgoProject: current.ArgoProject, BindingID: current.Authority.BindingID,
		TargetRef: current.Authority.TargetRef, RequiredAncestor: receipt.CommittedRevision, Path: current.Path,
		ExpectedManifestDigest: current.ManifestDigest, State: DeletionClaimed, Attempts: 1,
		LeaseOwner: testWorker1, LeaseEpoch: 1, LeaseUntil: &until, NextAttemptAt: updated,
		CreatedAt: fixture.now, UpdatedAt: updated}
	fixture.publisher.Now = func() time.Time { return fixture.now.Add(10 * time.Second) }
	deleted, err := fixture.publisher.Delete(ctx, DeletionLease{Deletion: deletion, Owner: testWorker1, Epoch: 1, Until: until})
	if err != nil {
		t.Fatal(err)
	}
	if deleted.CommittedRevision == receipt.CommittedRevision || deleted.Path != current.Path {
		t.Fatalf("unexpected deletion receipt: %#v", deleted)
	}
	command := exec.Command("git", "--git-dir", fixture.verifier.remote, "show", deleted.CommittedRevision+":"+current.Path)
	if output, showErr := command.CombinedOutput(); showErr == nil {
		t.Fatalf("foundation path still exists: %s", output)
	}
	// Recovery observes the already absent exact path and returns a stable no-op receipt.
	fixture.verifier.now = fixture.now.Add(11 * time.Second)
	fixture.publisher.Now = func() time.Time { return fixture.now.Add(12 * time.Second) }
	replayed, err := fixture.publisher.Delete(ctx, DeletionLease{Deletion: deletion, Owner: testWorker1, Epoch: 1, Until: until})
	if err != nil || replayed.CommittedRevision != deleted.CommittedRevision {
		t.Fatalf("replay=%#v err=%v", replayed, err)
	}
}

func TestProtectedGitPublisherRotatesProfileOnlyFromExactPublishedPreimage(t *testing.T) {
	fixture := newPublisherFixture(t)
	ctx := context.Background()
	firstReceipt, err := fixture.publisher.Publish(ctx, fixture.lease, fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	firstCurrent, err := fixture.store.Intent(ctx, fixture.lease.Intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.lease.Intent = firstCurrent
	if _, err = fixture.store.RecordReady(ctx, fixture.lease, firstReceipt, fixture.now.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	fixture.publisher.Now = func() time.Time { return fixture.now.Add(20 * time.Second) }
	fixture.verifier.now = fixture.now.Add(9 * time.Second)

	changed := testProfile()
	changed.ObserverServiceAccount = "kuberploy-api-rotated"
	second, err := fixture.store.EnsureIntent(ctx, EnsureRequest{testIntentID2, testEnvironmentID, changed, fixture.now.Add(7 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	expected, found, err := fixture.store.ExpectedPreimage(ctx, second.ID)
	if err != nil || !found || expected != fixture.request.ContentDigest {
		t.Fatalf("exact predecessor was not retained: digest=%q found=%v err=%v", expected, found, err)
	}
	profileDigest, _ := changed.Digest()
	lease, found, err := fixture.store.ClaimIntent(ctx, testWorker2, profileDigest, changed.PublisherConfigDigest, fixture.now.Add(8*time.Second), time.Minute)
	if err != nil || !found || lease.Intent.ID != second.ID {
		t.Fatalf("rotation claim failed: %#v found=%v err=%v", lease, found, err)
	}
	receipt, err := fixture.publisher.Publish(ctx, lease, publicationFor(lease.Intent))
	if err != nil {
		t.Fatal(err)
	}
	if receipt.ParentRevision != firstReceipt.CommittedRevision {
		t.Fatalf("rotation did not CAS from the exact published predecessor: %#v", receipt)
	}
	clone := filepath.Join(t.TempDir(), "rotation")
	runFoundationGit(t, "", "clone", "--branch", "main", fixture.verifier.remote, clone)
	content, err := os.ReadFile(filepath.Join(clone, filepath.FromSlash(second.Path)))
	if err != nil || !strings.Contains(string(content), "name: kuberploy-api-rotated") {
		t.Fatalf("rotated manifest was not published: %v\n%s", err, content)
	}
}

func TestProtectedGitPublisherRejectsProfileRotationAfterPreimageSubstitution(t *testing.T) {
	fixture := newPublisherFixture(t)
	ctx := context.Background()
	firstReceipt, err := fixture.publisher.Publish(ctx, fixture.lease, fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	firstCurrent, err := fixture.store.Intent(ctx, fixture.lease.Intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.lease.Intent = firstCurrent
	if _, err = fixture.store.RecordReady(ctx, fixture.lease, firstReceipt, fixture.now.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	fixture.publisher.Now = func() time.Time { return fixture.now.Add(20 * time.Second) }
	fixture.verifier.now = fixture.now.Add(9 * time.Second)
	appendFoundationRemote(t, fixture.verifier.remote, fixture.request.Path, []byte("attacker substitution\n"))

	changed := testProfile()
	changed.ObserverServiceAccount = "kuberploy-api-rotated"
	second, err := fixture.store.EnsureIntent(ctx, EnsureRequest{testIntentID2, testEnvironmentID, changed, fixture.now.Add(7 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	profileDigest, _ := changed.Digest()
	lease, found, err := fixture.store.ClaimIntent(ctx, testWorker2, profileDigest, changed.PublisherConfigDigest, fixture.now.Add(8*time.Second), time.Minute)
	if err != nil || !found || lease.Intent.ID != second.ID {
		t.Fatalf("rotation claim failed: %#v found=%v err=%v", lease, found, err)
	}
	if _, err = fixture.publisher.Publish(ctx, lease, publicationFor(lease.Intent)); !errors.Is(err, ErrConflict) {
		t.Fatalf("substituted foundation preimage was accepted: %v", err)
	}
}

func TestProtectedGitPublisherPersistsBaseBeforePushAndRecovers(t *testing.T) {
	t.Run("crash after write base before push", func(t *testing.T) {
		fixture := newPublisherFixture(t)
		crashing := &crashAfterBindStore{Store: fixture.store, crash: true}
		fixture.publisher.Store = crashing
		if _, err := fixture.publisher.Publish(context.Background(), fixture.lease, fixture.request); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("expected injected crash, got %v", err)
		}
		current, err := fixture.store.Intent(context.Background(), fixture.request.IntentID)
		if err != nil || current.WriteBaseRevision != fixture.base || current.CommittedRevision != "" {
			t.Fatalf("write base was not durable before push: %#v %v", current, err)
		}
		fixture.lease.Intent = current
		receipt, err := fixture.publisher.Publish(context.Background(), fixture.lease, fixture.request)
		if err != nil || receipt.ParentRevision != fixture.base || receipt.Validate(current) != nil {
			t.Fatalf("crash-before-push recovery failed: %#v %v", receipt, err)
		}
	})

	t.Run("push before database receipt", func(t *testing.T) {
		fixture := newPublisherFixture(t)
		fixture.verifier.failAt = 2
		if _, err := fixture.publisher.Publish(context.Background(), fixture.lease, fixture.request); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("expected post-push provider outage, got %v", err)
		}
		current, err := fixture.store.Intent(context.Background(), fixture.request.IntentID)
		if err != nil || current.WriteBaseRevision != fixture.base || current.CommittedRevision != "" {
			t.Fatalf("push/database crash boundary changed: %#v %v", current, err)
		}
		fixture.lease.Intent = current
		fixture.verifier.failAt, fixture.verifier.calls = 0, 0
		descendant := appendFoundationRemote(t, fixture.verifier.remote, "operator-note.txt", []byte("safe descendant\n"))
		receipt, err := fixture.publisher.Publish(context.Background(), fixture.lease, fixture.request)
		if err != nil || receipt.ParentRevision != fixture.base || receipt.CommittedRevision == descendant || receipt.Validate(current) != nil {
			t.Fatalf("exact operation recovery failed: %#v %v", receipt, err)
		}
	})

	t.Run("descendant path substitution is rejected", func(t *testing.T) {
		fixture := newPublisherFixture(t)
		fixture.verifier.failAt = 2
		if _, err := fixture.publisher.Publish(context.Background(), fixture.lease, fixture.request); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("expected post-push outage, got %v", err)
		}
		current, err := fixture.store.Intent(context.Background(), fixture.request.IntentID)
		if err != nil {
			t.Fatal(err)
		}
		fixture.lease.Intent = current
		appendFoundationRemote(t, fixture.verifier.remote, fixture.request.Path, []byte("attacker substitution\n"))
		fixture.verifier.failAt, fixture.verifier.calls = 0, 0
		if _, err = fixture.publisher.Publish(context.Background(), fixture.lease, fixture.request); !errors.Is(err, ErrConflict) {
			t.Fatalf("substituted descendant receipt accepted: %v", err)
		}
	})
}

func TestProtectedGitPublisherRejectsRequestPathSubstitutionBeforeProvider(t *testing.T) {
	fixture := newPublisherFixture(t)
	fixture.request.Path = "platform/argocd/foundations/" + testIntentID2 + ".yaml"
	if _, err := fixture.publisher.Publish(context.Background(), fixture.lease, fixture.request); !errors.Is(err, ErrInvalid) {
		t.Fatalf("path substitution was not rejected: %v", err)
	}
	if fixture.verifier.calls != 0 {
		t.Fatalf("provider was consulted for invalid authority: %d", fixture.verifier.calls)
	}
}

func runFoundationGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	if directory != "" {
		command.Dir = directory
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}

func appendFoundationRemote(t *testing.T, remote, path string, content []byte) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "append")
	runFoundationGit(t, "", "clone", "--branch", "main", remote, work)
	runFoundationGit(t, work, "config", "user.name", "Kuberploy Test")
	runFoundationGit(t, work, "config", "user.email", "test@kuberploy.invalid")
	fullPath := filepath.Join(work, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	runFoundationGit(t, work, "add", "--", path)
	runFoundationGit(t, work, "commit", "-m", "append descendant")
	runFoundationGit(t, work, "push", "origin", "main")
	return strings.TrimSpace(runFoundationGit(t, work, "rev-parse", "HEAD"))
}
