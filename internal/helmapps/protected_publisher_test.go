package helmapps

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
	"github.com/kuberploy/kuberploy/internal/id"
)

type protectedPublisherStoreStub struct {
	ProtectedPublicationStore
	payload     ProtectedPayloadIntent
	application ProtectedApplicationIntent
}

func (s *protectedPublisherStoreStub) ClaimPayload(_ context.Context, owner string, publisher ProtectedPublisherIdentity,
	now time.Time, duration time.Duration) (ProtectedPayloadIntent, ProtectedIntentLease, error) {
	if s.payload.ID == "" || s.payload.State == ProtectedVerified {
		return ProtectedPayloadIntent{}, ProtectedIntentLease{}, ErrNotFound
	}
	s.payload.State, s.payload.LeaseOwner = ProtectedClaimed, owner
	if s.payload.CommittedRevision != "" {
		s.payload.State = ProtectedGitCommitted
	}
	s.payload.Attempts++
	s.payload.LeaseEpoch++
	until := now.Add(duration)
	s.payload.LeaseUntil, s.payload.UpdatedAt = &until, now
	lease := payloadLease(s.payload)
	if publisher != s.payload.Publisher || s.payload.Validate() != nil || lease.Validate() != nil {
		return ProtectedPayloadIntent{}, ProtectedIntentLease{}, ErrInvalid
	}
	return s.payload, lease, nil
}

func (s *protectedPublisherStoreStub) ClaimApplication(_ context.Context, owner string, publisher ProtectedPublisherIdentity,
	now time.Time, duration time.Duration) (ProtectedApplicationIntent, ProtectedIntentLease, error) {
	if s.application.ID == "" || s.application.State == ProtectedVerified {
		return ProtectedApplicationIntent{}, ProtectedIntentLease{}, ErrNotFound
	}
	s.application.State, s.application.LeaseOwner = ProtectedClaimed, owner
	if s.application.CommittedRevision != "" {
		s.application.State = ProtectedGitCommitted
	}
	s.application.Attempts++
	s.application.LeaseEpoch++
	until := now.Add(duration)
	s.application.LeaseUntil, s.application.UpdatedAt = &until, now
	lease := applicationLease(s.application)
	if publisher != s.application.Publisher || s.application.Validate() != nil || lease.Validate() != nil {
		return ProtectedApplicationIntent{}, ProtectedIntentLease{}, ErrInvalid
	}
	return s.application, lease, nil
}

func (s *protectedPublisherStoreStub) HeartbeatPayload(_ context.Context, lease ProtectedIntentLease,
	now time.Time, duration time.Duration) (ProtectedIntentLease, error) {
	if !activePayloadLease(s.payload, lease, now) {
		return ProtectedIntentLease{}, ErrLeaseLost
	}
	until := now.Add(duration)
	s.payload.LeaseUntil, s.payload.UpdatedAt = &until, now
	return payloadLease(s.payload), nil
}

func (s *protectedPublisherStoreStub) HeartbeatApplication(_ context.Context, lease ProtectedIntentLease,
	now time.Time, duration time.Duration) (ProtectedIntentLease, error) {
	if !activeApplicationLease(s.application, lease, now) {
		return ProtectedIntentLease{}, ErrLeaseLost
	}
	until := now.Add(duration)
	s.application.LeaseUntil, s.application.UpdatedAt = &until, now
	return applicationLease(s.application), nil
}

func (s *protectedPublisherStoreStub) BindPayloadWriteBase(_ context.Context, lease ProtectedIntentLease,
	revision string, observedAt, now time.Time) (ProtectedPayloadIntent, error) {
	if !activePayloadLease(s.payload, lease, now) || s.payload.WriteBaseRevision != "" {
		return ProtectedPayloadIntent{}, ErrConflict
	}
	s.payload.WriteBaseRevision, s.payload.WriteBaseObservedAt, s.payload.UpdatedAt = revision, &observedAt, now
	return s.payload, s.payload.Validate()
}

func (s *protectedPublisherStoreStub) BindApplicationWriteBase(_ context.Context, lease ProtectedIntentLease,
	revision string, observedAt, now time.Time) (ProtectedApplicationIntent, error) {
	if !activeApplicationLease(s.application, lease, now) || s.application.WriteBaseRevision != "" {
		return ProtectedApplicationIntent{}, ErrConflict
	}
	s.application.WriteBaseRevision, s.application.WriteBaseObservedAt, s.application.UpdatedAt = revision, &observedAt, now
	return s.application, s.application.Validate()
}

func (s *protectedPublisherStoreStub) MarkPayloadCommitted(_ context.Context, lease ProtectedIntentLease,
	revision, parent string, now time.Time) (ProtectedPayloadIntent, error) {
	if !activePayloadLease(s.payload, lease, now) || s.payload.WriteBaseRevision != parent {
		return ProtectedPayloadIntent{}, ErrLeaseLost
	}
	s.payload.State, s.payload.CommittedRevision, s.payload.CommittedParentRevision = ProtectedGitCommitted, revision, parent
	s.payload.CommittedAt, s.payload.UpdatedAt = &now, now
	return s.payload, s.payload.Validate()
}

func (s *protectedPublisherStoreStub) MarkApplicationCommitted(_ context.Context, lease ProtectedIntentLease,
	revision, parent string, now time.Time) (ProtectedApplicationIntent, error) {
	if !activeApplicationLease(s.application, lease, now) || s.application.WriteBaseRevision != parent {
		return ProtectedApplicationIntent{}, ErrLeaseLost
	}
	s.application.State, s.application.CommittedRevision, s.application.CommittedParentRevision = ProtectedGitCommitted, revision, parent
	s.application.CommittedAt, s.application.UpdatedAt = &now, now
	return s.application, s.application.Validate()
}

func (s *protectedPublisherStoreStub) VerifyPayload(_ context.Context, lease ProtectedIntentLease,
	revision, digest, request string, now time.Time) (ProtectedPayloadIntent, error) {
	if !activePayloadLease(s.payload, lease, now) || s.payload.CommittedRevision != revision || digest != s.payload.ContentDigest {
		return ProtectedPayloadIntent{}, ErrLeaseLost
	}
	s.payload.State, s.payload.VerifiedPathDigest, s.payload.ProviderRequest = ProtectedVerified, digest, request
	s.payload.VerifiedAt, s.payload.CompletedAt, s.payload.UpdatedAt = &now, &now, now
	s.payload.LeaseOwner, s.payload.LeaseUntil = "", nil
	return s.payload, s.payload.Validate()
}

func (s *protectedPublisherStoreStub) VerifyApplication(_ context.Context, lease ProtectedIntentLease,
	revision, digest, request string, now time.Time) (ProtectedApplicationIntent, error) {
	if !activeApplicationLease(s.application, lease, now) || s.application.CommittedRevision != revision || digest != s.application.ContentDigest {
		return ProtectedApplicationIntent{}, ErrLeaseLost
	}
	s.application.State, s.application.VerifiedPathDigest, s.application.ProviderRequest = ProtectedVerified, digest, request
	s.application.VerifiedAt, s.application.CompletedAt, s.application.UpdatedAt = &now, &now, now
	s.application.LeaseOwner, s.application.LeaseUntil = "", nil
	return s.application, s.application.Validate()
}

type protectedPublisherFixture struct {
	now       time.Time
	remote    string
	seed      string
	base      string
	binding   gitprojection.Binding
	bindings  *gitprojection.MemoryStore
	manager   *gitprojection.MirrorManager
	store     *protectedPublisherStoreStub
	publisher ProtectedPublisherIdentity
	target    ReleaseTarget
}

func runProtectedPublisherGit(t *testing.T, directory string, args ...string) (string, error) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	command.Env = append(os.Environ(), "LC_ALL=C", "GIT_AUTHOR_NAME=Helm Test", "GIT_AUTHOR_EMAIL=helm@example.invalid",
		"GIT_COMMITTER_NAME=Helm Test", "GIT_COMMITTER_EMAIL=helm@example.invalid")
	output, err := command.CombinedOutput()
	if err != nil {
		return string(output), err
	}
	return strings.TrimSpace(string(output)), nil
}

func newProtectedPublisherFixture(t *testing.T) *protectedPublisherFixture {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	root, remote, seed := t.TempDir(), "", ""
	remote, seed = filepath.Join(root, "platform.git"), filepath.Join(root, "seed")
	if err := os.Mkdir(seed, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, command := range [][]string{{"init", "--bare", remote}, {"init", "--initial-branch=main"}} {
		directory := root
		if len(command) == 2 {
			directory = seed
		}
		if output, err := runProtectedPublisherGit(t, directory, command...); err != nil {
			t.Fatalf("git %v: %v: %s", command, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("protected platform repository\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	for _, command := range [][]string{{"add", "README.md"}, {"commit", "-m", "seed"}, {"remote", "add", "origin", remote}, {"push", "origin", "HEAD:refs/heads/platform"}} {
		if output, err := runProtectedPublisherGit(t, seed, command...); err != nil {
			t.Fatalf("git %v: %v: %s", command, err, output)
		}
	}
	base, err := runProtectedPublisherGit(t, remote, "rev-parse", "refs/heads/platform")
	if err != nil {
		t.Fatal(err)
	}
	clusterID, bindingID := id.New(), id.New()
	binding, err := gitprojection.NewGitHubPlatformBinding(bindingID, clusterID,
		gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 7, RepositoryID: 8, Owner: "kuberploy", Name: "platform"},
		"refs/heads/platform", now)
	if err != nil {
		t.Fatal(err)
	}
	binding.State, binding.TargetHeadRevision, binding.TargetHeadObservedAt, binding.UpdatedAt = gitprojection.BindingIndexing, base, now, now
	bindings := gitprojection.NewMemoryStore()
	if err = bindings.PutBinding(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	publisher := ProtectedPublisherIdentity{Contract: ProtectedPublisherContract, PolicyVersion: ProtectedGitPolicy,
		ConfigDigest: digestBytes([]byte("protected-publisher"))}
	target := ReleaseTarget{ProjectID: id.New(), EnvironmentID: id.New(), ApplicationID: id.New()}
	fixture := &protectedPublisherFixture{now: now, remote: remote, seed: seed, base: base, binding: binding, bindings: bindings,
		manager: &gitprojection.MirrorManager{Root: filepath.Join(root, "cache"), AllowLocalTests: true, LocalRemote: remote},
		store:   &protectedPublisherStoreStub{}, publisher: publisher, target: target}
	fixture.store.payload = fixture.pendingPayload(id.New(), []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: approved\n"), false, base)
	return fixture
}

func (f *protectedPublisherFixture) pendingPayload(releaseID string, content []byte, disabled bool, plannedBase string) ProtectedPayloadIntent {
	action := ProtectedPayloadPublish
	if disabled {
		action = ProtectedPayloadDisable
	}
	value := ProtectedPayloadIntent{ID: id.New(), ReleaseRevisionID: releaseID, ReleaseGeneration: 1,
		Target: f.target, Action: action, Binding: ProtectedBindingSnapshot{PlatformBindingID: f.binding.ID,
			EnvironmentBindingID: id.New(), ClusterID: f.binding.ClusterID, PlatformTargetRef: f.binding.TargetRef,
			EnvironmentTargetRef: "refs/heads/main", EnvironmentRevision: strings.Repeat("a", 40), EnvironmentGeneration: 1,
			CatalogDigest: digestBytes([]byte("catalog")), PlannedBaseRevision: plannedBase},
		Content: content, ContentDigest: digestBytes(content), IntentDigest: digestBytes([]byte("payload-intent-" + releaseID)),
		Publisher: f.publisher, Message: "publish protected payload", State: ProtectedPending,
		NextAttemptAt: f.now, CreatedAt: f.now, UpdatedAt: f.now}
	if !disabled {
		value.InventoryDigest, value.ResourceCount = digestBytes([]byte("inventory")), 1
	}
	value.Path = protectedPayloadPath(f.binding.ClusterID, f.target.EnvironmentID, f.target.ApplicationID, releaseID, disabled)
	value.CommitTrailer = "Kuberploy-Helm-Payload-Intent: " + value.ID
	return value
}

func (f *protectedPublisherFixture) pendingApplication(payload ProtectedPayloadIntent, content []byte,
	action ProtectedApplicationAction, precondition, etag string) ProtectedApplicationIntent {
	operation := "create"
	if precondition == "match-etag" {
		operation = "update"
	}
	if action == ProtectedApplicationDelete {
		operation, content = "delete", nil
	}
	value := ProtectedApplicationIntent{ID: id.New(), ReleaseRevisionID: payload.ReleaseRevisionID,
		PayloadIntentID: payload.ID, ReleaseGeneration: 1, Target: f.target, Action: action, Binding: payload.Binding,
		PayloadRevision: payload.CommittedRevision, PayloadPath: payload.Path,
		ApplicationPath: protectedApplicationPath(f.binding.ClusterID, f.target.EnvironmentID, f.target.ApplicationID),
		Operation:       operation, Precondition: precondition, ExpectedETag: etag, Content: content,
		IntentDigest: digestBytes([]byte("application-intent-" + payload.ReleaseRevisionID)), Publisher: f.publisher,
		Message: "publish protected application", State: ProtectedPending, NextAttemptAt: f.now, CreatedAt: f.now, UpdatedAt: f.now}
	if action == ProtectedApplicationPublish {
		value.SourceDirectory = protectedSourceDirectory(f.binding.ClusterID, f.target.EnvironmentID, f.target.ApplicationID, payload.ReleaseRevisionID)
		value.ContentDigest = digestBytes(content)
	}
	value.CommitTrailer = "Kuberploy-Helm-Application-Intent: " + value.ID
	return value
}

func (f *protectedPublisherFixture) headVerifier(t *testing.T) gitprojection.HeadVerifier {
	t.Helper()
	return protectedHeadVerifierFunc(func(_ context.Context, binding gitprojection.Binding, source gitprojection.ObservationSource) (gitprojection.VerifiedHead, error) {
		head, err := runProtectedPublisherGit(t, f.remote, "rev-parse", binding.TargetRef)
		if err != nil {
			return gitprojection.VerifiedHead{}, err
		}
		return gitprojection.VerifiedHead{BindingID: binding.ID, Repository: binding.Repository, TargetRef: binding.TargetRef,
			Commit: head, Source: source, ProviderRequest: "helm-provider-read", ObservedAt: f.now}, nil
	})
}

type protectedHeadVerifierFunc func(context.Context, gitprojection.Binding, gitprojection.ObservationSource) (gitprojection.VerifiedHead, error)

func (f protectedHeadVerifierFunc) VerifyTargetHead(ctx context.Context, binding gitprojection.Binding,
	source gitprojection.ObservationSource) (gitprojection.VerifiedHead, error) {
	return f(ctx, binding, source)
}

func (f *protectedPublisherFixture) worker(t *testing.T) *ProtectedGitPublisher {
	return &ProtectedGitPublisher{Store: f.store, Bindings: f.bindings, Provider: f.headVerifier(t), Manager: f.manager,
		Publisher: f.publisher, WorkerID: "helm-protected-worker-0001", Now: func() time.Time { return f.now }}
}

func TestProtectedGitPublisherCommitsTwoPhasesAndMatchDeletesStableApplication(t *testing.T) {
	fixture := newProtectedPublisherFixture(t)
	payload, err := fixture.worker(t).ProcessPayloadOne(t.Context())
	if err != nil || payload.State != ProtectedVerified || payload.CommittedParentRevision != fixture.base {
		t.Fatalf("payload=%#v err=%v", payload, err)
	}
	manifest := []byte("apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata:\n  name: exact\n")
	fixture.store.application = fixture.pendingApplication(payload, manifest, ProtectedApplicationPublish, "create-if-absent", "")
	application, err := fixture.worker(t).ProcessApplicationOne(t.Context())
	if err != nil || application.State != ProtectedVerified || application.CommittedParentRevision != payload.CommittedRevision {
		t.Fatalf("application=%#v err=%v", application, err)
	}
	commitObject, err := runProtectedPublisherGit(t, fixture.remote, "cat-file", "commit", application.CommittedRevision)
	if err != nil || !strings.Contains(commitObject, "Kuberploy-Helm-Application-Intent: "+application.ID) ||
		!strings.Contains(commitObject, "Kuberploy-Operation: "+application.ID) {
		t.Fatalf("missing exact recovery trailers: %v\n%s", err, commitObject)
	}
	disableReceipt := []byte(`{"apiVersion":"kuberploy.io/v1alpha1","kind":"HelmReleaseDisabledReceipt"}`)
	disable := fixture.pendingPayload(id.New(), disableReceipt, true, fixture.base)
	disable.Binding.PlannedBaseRevision = fixture.base
	fixture.store.payload = disable
	disable, err = fixture.worker(t).ProcessPayloadOne(t.Context())
	if err != nil || disable.State != ProtectedVerified {
		t.Fatalf("disable payload=%#v err=%v", disable, err)
	}
	fixture.store.application = fixture.pendingApplication(disable, nil, ProtectedApplicationDelete,
		"match-etag", `"`+application.ContentDigest+`"`)
	deleted, err := fixture.worker(t).ProcessApplicationOne(t.Context())
	if err != nil || deleted.State != ProtectedVerified || deleted.VerifiedPathDigest != "" {
		t.Fatalf("deleted=%#v err=%v", deleted, err)
	}
	if _, showErr := runProtectedPublisherGit(t, fixture.remote, "show", deleted.CommittedRevision+":"+deleted.ApplicationPath); showErr == nil {
		t.Fatal("match-delete receipt was verified while the stable Application still existed")
	}
}

func TestProtectedGitPublisherRecoversExactCommitAndRejectsPostimageSubstitution(t *testing.T) {
	fixture := newProtectedPublisherFixture(t)
	intent := &fixture.store.payload
	intent.State, intent.LeaseOwner, intent.Attempts, intent.LeaseEpoch = ProtectedClaimed, "helm-protected-worker-0001", 1, 1
	until := fixture.now.Add(time.Minute)
	intent.LeaseUntil = &until
	intent.WriteBaseRevision, intent.WriteBaseObservedAt = fixture.base, &fixture.now
	mutation, err := intent.Mutation()
	if err != nil {
		t.Fatal(err)
	}
	gitMutation, err := mutation.gitMutation()
	if err != nil {
		t.Fatal(err)
	}
	head, err := fixture.headVerifier(t).VerifyTargetHead(t.Context(), fixture.binding, gitprojection.ObservationWrite)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := fixture.manager.Prepare(t.Context(), fixture.binding, head, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	exactCommit, err := prepared.Commit(t.Context(), gitMutation)
	if err != nil {
		t.Fatal(err)
	}
	if err = prepared.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Simulate lease expiry after the push but before the database receipt.
	intent.LeaseUntil = &fixture.now
	intent.LeaseOwner = ""
	recovered, err := fixture.worker(t).ProcessPayloadOne(t.Context())
	if err != nil || recovered.State != ProtectedVerified || recovered.CommittedRevision != exactCommit {
		t.Fatalf("recovered=%#v err=%v", recovered, err)
	}
	if recovered.CommittedRevision == recovered.CommittedParentRevision {
		t.Fatal("recovery substituted the write base for the exact operation commit")
	}

	// A fresh fixture with the same crash shape must reject a descendant that
	// substitutes different bytes at the protected path.
	fixture = newProtectedPublisherFixture(t)
	intent = &fixture.store.payload
	intent.State, intent.LeaseOwner, intent.Attempts, intent.LeaseEpoch = ProtectedClaimed, "helm-protected-worker-0001", 1, 1
	until = fixture.now.Add(time.Minute)
	intent.LeaseUntil, intent.WriteBaseRevision, intent.WriteBaseObservedAt = &until, fixture.base, &fixture.now
	mutation, _ = intent.Mutation()
	gitMutation, _ = mutation.gitMutation()
	head, _ = fixture.headVerifier(t).VerifyTargetHead(t.Context(), fixture.binding, gitprojection.ObservationWrite)
	prepared, err = fixture.manager.Prepare(t.Context(), fixture.binding, head, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = prepared.Commit(t.Context(), gitMutation); err != nil {
		t.Fatal(err)
	}
	if err = prepared.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = runProtectedPublisherGit(t, fixture.seed, "fetch", "origin", "refs/heads/platform"); err != nil {
		t.Fatal(err)
	}
	if _, err = runProtectedPublisherGit(t, fixture.seed, "reset", "--hard", "FETCH_HEAD"); err != nil {
		t.Fatal(err)
	}
	fullPath := filepath.Join(fixture.seed, filepath.FromSlash(intent.Path))
	if err = os.WriteFile(fullPath, []byte("substituted bytes\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	for _, command := range [][]string{{"add", "--all"}, {"commit", "-m", "external substitution"}, {"push", "origin", "HEAD:refs/heads/platform"}} {
		if output, gitErr := runProtectedPublisherGit(t, fixture.seed, command...); gitErr != nil {
			t.Fatalf("git %v: %v: %s", command, gitErr, output)
		}
	}
	intent.LeaseUntil, intent.LeaseOwner = &fixture.now, ""
	if _, err = fixture.worker(t).ProcessPayloadOne(t.Context()); !errors.Is(err, gitprojection.ErrConflict) {
		t.Fatalf("substituted descendant was not rejected: %v", err)
	}
}

func TestProtectedGitPublisherRejectsDigestCASAncestryAndTrailerSubstitution(t *testing.T) {
	t.Run("digest", func(t *testing.T) {
		fixture := newProtectedPublisherFixture(t)
		mutation, err := fixture.store.payload.Mutation()
		if err != nil {
			t.Fatal(err)
		}
		gitMutation, err := mutation.gitMutation()
		if err != nil {
			t.Fatal(err)
		}
		gitMutation.ContentSHA256 = digestBytes([]byte("substituted digest"))
		if gitMutation.Validate(fixture.binding) == nil {
			t.Fatal("shared Git transport accepted bytes under a substituted digest")
		}
		gitMutation = gitprojection.Mutation{BindingID: fixture.binding.ID, OperationID: mutation.IntentID,
			Path: mutation.Path, BaseRevision: fixture.base, Precondition: gitprojection.MutationCreateIfAbsent,
			Content: mutation.Content, Message: mutation.Message}
		if gitMutation.Validate(fixture.binding) == nil {
			t.Fatal("ordinary writer authority bypassed the protected Helm path/trailer policy")
		}
	})

	t.Run("cas", func(t *testing.T) {
		fixture := newProtectedPublisherFixture(t)
		payload, err := fixture.worker(t).ProcessPayloadOne(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		manifest := []byte("apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata:\n  name: exact\n")
		fixture.store.application = fixture.pendingApplication(payload, manifest, ProtectedApplicationPublish, "create-if-absent", "")
		application, err := fixture.worker(t).ProcessApplicationOne(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		disable := fixture.pendingPayload(id.New(), []byte("disabled receipt\n"), true, fixture.base)
		fixture.store.payload = disable
		disable, err = fixture.worker(t).ProcessPayloadOne(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		fixture.store.application = fixture.pendingApplication(disable, nil, ProtectedApplicationDelete,
			"match-etag", `"`+digestBytes([]byte("wrong before image"))+`"`)
		if _, err = fixture.worker(t).ProcessApplicationOne(t.Context()); !errors.Is(err, gitprojection.ErrConflict) {
			t.Fatalf("match-delete accepted a substituted before-image instead of %s: %v", application.ContentDigest, err)
		}
	})

	t.Run("ancestry", func(t *testing.T) {
		fixture := newProtectedPublisherFixture(t)
		payload, err := fixture.worker(t).ProcessPayloadOne(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		fixture.store.application = fixture.pendingApplication(payload, []byte("application\n"),
			ProtectedApplicationPublish, "create-if-absent", "")
		fixture.store.application.PayloadRevision = strings.Repeat("f", 40)
		if fixture.store.application.Validate() != nil {
			t.Fatal("negative ancestry fixture is structurally invalid")
		}
		if _, err = fixture.worker(t).ProcessApplicationOne(t.Context()); !errors.Is(err, gitprojection.ErrConflict) {
			t.Fatalf("phase two accepted a payload revision outside provider ancestry: %v", err)
		}
	})

	t.Run("trailer", func(t *testing.T) {
		fixture := newProtectedPublisherFixture(t)
		intent := &fixture.store.payload
		intent.State, intent.LeaseOwner, intent.Attempts, intent.LeaseEpoch = ProtectedClaimed, "", 1, 1
		intent.WriteBaseRevision, intent.WriteBaseObservedAt = fixture.base, &fixture.now
		fullPath := filepath.Join(fixture.seed, filepath.FromSlash(intent.Path))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, intent.Content, 0o640); err != nil {
			t.Fatal(err)
		}
		for _, command := range [][]string{{"add", "--all"}, {"commit", "-m", "push without protected trailer\n\nKuberploy-Operation: " + intent.ID}, {"push", "origin", "HEAD:refs/heads/platform"}} {
			if output, gitErr := runProtectedPublisherGit(t, fixture.seed, command...); gitErr != nil {
				t.Fatalf("git %v: %v: %s", command, gitErr, output)
			}
		}
		if _, err := fixture.worker(t).ProcessPayloadOne(t.Context()); !errors.Is(err, ErrConflict) {
			t.Fatalf("recovery accepted a commit missing its exact Helm intent trailer: %v", err)
		}
	})
}
