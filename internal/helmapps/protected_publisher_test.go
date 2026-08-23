package helmapps

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/id"
	"go.yaml.in/yaml/v3"
)

type protectedPublisherStoreStub struct {
	ProtectedPublicationStore
	ProtectedCascadeStore
	payload          ProtectedPayloadIntent
	application      ProtectedApplicationIntent
	cascade          ProtectedApplicationCascadePreflight
	absenceProof     ProtectedCascadePathAbsenceProof
	absenceFailures  int
	prerequisite     ProtectedPublicationPrerequisiteReceipt
	prerequisiteErr  error
	payloadRebinds   int
	appRebinds       int
	payloadAdoptions int
	appAdoptions     int
	payloadHeartbeat chan time.Time
	appHeartbeat     chan time.Time
}

func (s *protectedPublisherStoreStub) ActivateCascadeObserver(context.Context, string, int64,
	ProtectedPublisherIdentity, time.Time) (int64, error) {
	return 1, nil
}

func (s *protectedPublisherStoreStub) PublicationPrerequisite(_ context.Context,
	releaseID string) (ProtectedPublicationPrerequisiteReceipt, error) {
	if s.prerequisiteErr != nil {
		return ProtectedPublicationPrerequisiteReceipt{}, s.prerequisiteErr
	}
	value := s.prerequisite
	var target ReleaseTarget
	var binding ProtectedBindingSnapshot
	if s.payload.ReleaseRevisionID == releaseID {
		target, binding = s.payload.Target, s.payload.Binding
	} else if s.application.ReleaseRevisionID == releaseID {
		target, binding = s.application.Target, s.application.Binding
	} else {
		return ProtectedPublicationPrerequisiteReceipt{}, ErrNotFound
	}
	value.ReleaseRevisionID, value.ProjectID = releaseID, target.ProjectID
	value.EnvironmentID, value.ApplicationID = target.EnvironmentID, target.ApplicationID
	value.PlatformBindingID, value.EnvironmentBindingID = binding.PlatformBindingID, binding.EnvironmentBindingID
	value.EnvironmentRevision = binding.EnvironmentRevision
	value.EnvironmentGeneration, value.PlannedBaseRevision = binding.EnvironmentGeneration, binding.PlannedBaseRevision
	return value, nil
}

func (s *protectedPublisherStoreStub) ClaimPayload(_ context.Context, owner string, publisher ProtectedPublisherIdentity,
	now time.Time, duration time.Duration) (ProtectedPayloadIntent, ProtectedIntentLease, error) {
	if s.payload.ID == "" || s.payload.State == ProtectedVerified || publisher != s.payload.Publisher {
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
	if s.payload.Validate() != nil || lease.Validate() != nil {
		return ProtectedPayloadIntent{}, ProtectedIntentLease{}, ErrInvalid
	}
	return s.payload, lease, nil
}

func (s *protectedPublisherStoreStub) ClaimApplication(_ context.Context, owner string, publisher ProtectedPublisherIdentity,
	now time.Time, duration time.Duration) (ProtectedApplicationIntent, ProtectedIntentLease, error) {
	if s.application.ID == "" || s.application.State == ProtectedVerified || publisher != s.application.Publisher {
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
	if s.application.Validate() != nil || lease.Validate() != nil {
		return ProtectedApplicationIntent{}, ProtectedIntentLease{}, ErrInvalid
	}
	return s.application, lease, nil
}

func (s *protectedPublisherStoreStub) ClaimCascadePreflight(_ context.Context, owner string,
	publisher ProtectedPublisherIdentity, now time.Time,
	duration time.Duration) (ProtectedApplicationCascadePreflight, ProtectedIntentLease, error) {
	if s.cascade.ID == "" || s.cascade.State == ProtectedVerified || s.cascade.State == ProtectedFailed ||
		publisher != s.cascade.Publisher {
		return ProtectedApplicationCascadePreflight{}, ProtectedIntentLease{}, ErrNotFound
	}
	s.cascade.State, s.cascade.LeaseOwner = ProtectedClaimed, owner
	if s.cascade.CommittedRevision != "" {
		s.cascade.State = ProtectedGitCommitted
	}
	s.cascade.Attempts++
	s.cascade.LeaseEpoch++
	until := now.Add(duration)
	s.cascade.LeaseUntil, s.cascade.UpdatedAt = &until, now
	lease := cascadePreflightLease(s.cascade)
	if s.cascade.Validate() != nil || lease.Validate() != nil {
		return ProtectedApplicationCascadePreflight{}, ProtectedIntentLease{}, ErrInvalid
	}
	return s.cascade, lease, nil
}

func (s *protectedPublisherStoreStub) AdoptCascadePreflight(_ context.Context, owner string, workerEpoch int64,
	publisher ProtectedPublisherIdentity, duration time.Duration) (ProtectedApplicationCascadePreflight, ProtectedIntentLease, error) {
	if s.cascade.ID == "" || s.cascade.State != ProtectedPending || workerEpoch < 1 ||
		publisher.Contract != s.cascade.Publisher.Contract ||
		publisher.PolicyVersion != s.cascade.Publisher.PolicyVersion ||
		publisher.ConfigDigest == s.cascade.Publisher.ConfigDigest {
		return ProtectedApplicationCascadePreflight{}, ProtectedIntentLease{}, ErrNotFound
	}
	now := time.Now().UTC()
	s.cascade.Publisher = publisher
	s.cascade.PublisherAdoptionEpoch++
	s.cascade.State, s.cascade.LeaseOwner = ProtectedClaimed, owner
	s.cascade.Attempts++
	s.cascade.LeaseEpoch++
	until := now.Add(duration)
	s.cascade.LeaseUntil, s.cascade.UpdatedAt = &until, now
	lease := cascadePreflightLease(s.cascade)
	if s.cascade.Validate() != nil || lease.Validate() != nil {
		return ProtectedApplicationCascadePreflight{}, ProtectedIntentLease{}, ErrInvalid
	}
	return s.cascade, lease, nil
}

func (s *protectedPublisherStoreStub) AdoptPayload(_ context.Context, owner string, workerEpoch int64,
	publisher ProtectedPublisherIdentity, duration time.Duration) (ProtectedPayloadIntent, ProtectedIntentLease, error) {
	if s.payload.ID == "" || s.payload.State == ProtectedVerified || workerEpoch < 1 ||
		publisher == s.payload.Publisher {
		return ProtectedPayloadIntent{}, ProtectedIntentLease{}, ErrNotFound
	}
	now := s.payload.UpdatedAt
	s.payload.Publisher, s.payload.PublisherAdoptionEpoch = publisher, s.payload.PublisherAdoptionEpoch+1
	s.payload.State, s.payload.LeaseOwner = ProtectedClaimed, owner
	if s.payload.CommittedRevision != "" {
		s.payload.State = ProtectedGitCommitted
	}
	s.payload.Attempts++
	s.payload.LeaseEpoch++
	until := now.Add(duration)
	s.payload.LeaseUntil = &until
	s.payloadAdoptions++
	lease := payloadLease(s.payload)
	return s.payload, lease, lease.Validate()
}

func (s *protectedPublisherStoreStub) AdoptApplication(_ context.Context, owner string, workerEpoch int64,
	publisher ProtectedPublisherIdentity, duration time.Duration) (ProtectedApplicationIntent, ProtectedIntentLease, error) {
	if s.application.ID == "" || s.application.State == ProtectedVerified || workerEpoch < 1 ||
		publisher == s.application.Publisher {
		return ProtectedApplicationIntent{}, ProtectedIntentLease{}, ErrNotFound
	}
	now := s.application.UpdatedAt
	s.application.Publisher, s.application.PublisherAdoptionEpoch = publisher, s.application.PublisherAdoptionEpoch+1
	s.application.State, s.application.LeaseOwner = ProtectedClaimed, owner
	if s.application.CommittedRevision != "" {
		s.application.State = ProtectedGitCommitted
	}
	s.application.Attempts++
	s.application.LeaseEpoch++
	until := now.Add(duration)
	s.application.LeaseUntil = &until
	s.appAdoptions++
	lease := applicationLease(s.application)
	return s.application, lease, lease.Validate()
}

func (s *protectedPublisherStoreStub) HeartbeatPayload(_ context.Context, lease ProtectedIntentLease,
	now time.Time, duration time.Duration) (ProtectedIntentLease, error) {
	if !activePayloadLease(s.payload, lease, now) {
		return ProtectedIntentLease{}, ErrLeaseLost
	}
	until := now.Add(duration)
	s.payload.LeaseUntil, s.payload.UpdatedAt = &until, now
	if s.payloadHeartbeat != nil {
		select {
		case s.payloadHeartbeat <- now:
		default:
		}
	}
	return payloadLease(s.payload), nil
}

func (s *protectedPublisherStoreStub) HeartbeatApplication(_ context.Context, lease ProtectedIntentLease,
	now time.Time, duration time.Duration) (ProtectedIntentLease, error) {
	if !activeApplicationLease(s.application, lease, now) {
		return ProtectedIntentLease{}, ErrLeaseLost
	}
	until := now.Add(duration)
	s.application.LeaseUntil, s.application.UpdatedAt = &until, now
	if s.appHeartbeat != nil {
		select {
		case s.appHeartbeat <- now:
		default:
		}
	}
	return applicationLease(s.application), nil
}

func (s *protectedPublisherStoreStub) HeartbeatCascadePreflight(_ context.Context,
	lease ProtectedIntentLease, now time.Time, duration time.Duration) (ProtectedIntentLease, error) {
	if s.cascade.ID != lease.IntentID || s.cascade.LeaseOwner != lease.Owner ||
		s.cascade.LeaseEpoch != lease.Epoch || s.cascade.LeaseUntil == nil ||
		!s.cascade.LeaseUntil.After(now) {
		return ProtectedIntentLease{}, ErrLeaseLost
	}
	until := now.Add(duration)
	s.cascade.LeaseUntil, s.cascade.UpdatedAt = &until, now
	return cascadePreflightLease(s.cascade), nil
}

func (s *protectedPublisherStoreStub) FailCascadePreflightPathAbsent(_ context.Context,
	lease ProtectedIntentLease, proof ProtectedCascadePathAbsenceProof,
	now time.Time) (ProtectedApplicationCascadePreflight, error) {
	if proof.Validate() != nil || s.cascade.ID != lease.IntentID ||
		s.cascade.State != ProtectedClaimed || s.cascade.Operation != "update" ||
		s.cascade.LeaseOwner != lease.Owner || s.cascade.LeaseEpoch != lease.Epoch ||
		s.cascade.LeaseUntil == nil || !s.cascade.LeaseUntil.After(now) ||
		s.cascade.CommittedRevision != "" {
		return ProtectedApplicationCascadePreflight{}, ErrConflict
	}
	s.absenceProof, s.absenceFailures = proof, s.absenceFailures+1
	s.cascade.State, s.cascade.ConsecutiveFailures = ProtectedFailed, s.cascade.ConsecutiveFailures+1
	s.cascade.LastFailureCode = "cascade-path-absent-recovery-required"
	s.cascade.LeaseOwner, s.cascade.LeaseUntil = "", nil
	s.cascade.CompletedAt, s.cascade.UpdatedAt = &now, now
	return s.cascade, s.cascade.Validate()
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

func (s *protectedPublisherStoreStub) RebindPayloadWriteBase(_ context.Context, lease ProtectedIntentLease,
	previous, revision string, observedAt, now time.Time) (ProtectedPayloadIntent, error) {
	if !activePayloadLease(s.payload, lease, now) || s.payload.State != ProtectedClaimed ||
		s.payload.WriteBaseRevision != previous || s.payload.CommittedRevision != "" || previous == revision {
		return ProtectedPayloadIntent{}, ErrConflict
	}
	s.payload.WriteBaseRevision, s.payload.WriteBaseObservedAt, s.payload.UpdatedAt = revision, &observedAt, now
	s.payloadRebinds++
	return s.payload, s.payload.Validate()
}

func (s *protectedPublisherStoreStub) RebindApplicationWriteBase(_ context.Context, lease ProtectedIntentLease,
	previous, revision string, observedAt, now time.Time) (ProtectedApplicationIntent, error) {
	if !activeApplicationLease(s.application, lease, now) || s.application.State != ProtectedClaimed ||
		s.application.WriteBaseRevision != previous || s.application.CommittedRevision != "" || previous == revision {
		return ProtectedApplicationIntent{}, ErrConflict
	}
	s.application.WriteBaseRevision, s.application.WriteBaseObservedAt, s.application.UpdatedAt = revision, &observedAt, now
	s.appRebinds++
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
	if !activeApplicationLease(s.application, lease, now) || now.Before(s.application.UpdatedAt) ||
		s.application.CommittedRevision != revision || digest != s.application.ContentDigest {
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
	refresher *protectedRootRefresherStub
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
	bindingID := id.New()
	binding, err := gitprojection.NewGitHubPlatformBinding(bindingID, gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 7, RepositoryID: 8, Owner: "kuberploy", Name: "platform"},
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
		store:   &protectedPublisherStoreStub{}, refresher: &protectedRootRefresherStub{},
		publisher: publisher, target: target}
	fixture.store.payload = fixture.pendingPayload(id.New(), []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: approved\n"), false, base)
	fixture.store.prerequisite = ProtectedPublicationPrerequisiteReceipt{
		ReleaseRevisionID: fixture.store.payload.ReleaseRevisionID,
		ProjectID:         target.ProjectID, EnvironmentID: target.EnvironmentID, ApplicationID: target.ApplicationID,
		PlatformBindingID: binding.ID, EnvironmentBindingID: fixture.store.payload.Binding.EnvironmentBindingID,
		EnvironmentRevision:   fixture.store.payload.Binding.EnvironmentRevision,
		EnvironmentGeneration: fixture.store.payload.Binding.EnvironmentGeneration,
		FoundationIntentID:    id.New(), FoundationRevision: base,
		DesiredStateCommandID: id.New(), DesiredStateRevision: base,
		PlannedBaseRevision: base, CreatedAt: now,
	}
	return fixture
}

func (f *protectedPublisherFixture) pendingPayload(releaseID string, content []byte, disabled bool, plannedBase string) ProtectedPayloadIntent {
	action := ProtectedPayloadPublish
	if disabled {
		action = ProtectedPayloadDisable
	}
	value := ProtectedPayloadIntent{ID: id.New(), ReleaseRevisionID: releaseID, ReleaseGeneration: 1,
		Target: f.target, Action: action, Binding: ProtectedBindingSnapshot{PlatformBindingID: f.binding.ID,
			EnvironmentBindingID: id.New(), PlatformTargetRef: f.binding.TargetRef,
			EnvironmentTargetRef: "refs/heads/main", EnvironmentRevision: strings.Repeat("a", 40), EnvironmentGeneration: 1,
			CatalogDigest: digestBytes([]byte("catalog")), PlannedBaseRevision: plannedBase},
		Content: content, ContentDigest: digestBytes(content), IntentDigest: digestBytes([]byte("payload-intent-" + releaseID)),
		Publisher: f.publisher, OriginalPublisherConfigDigest: f.publisher.ConfigDigest,
		Message: "publish protected payload", State: ProtectedPending,
		NextAttemptAt: f.now, CreatedAt: f.now, UpdatedAt: f.now}
	if !disabled {
		value.InventoryDigest, value.ResourceCount = digestBytes([]byte("inventory")), 1
	}
	value.Path = protectedPayloadPath(f.target.EnvironmentID, f.target.ApplicationID, releaseID, disabled)
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
		ApplicationPath: protectedApplicationPath(f.target.EnvironmentID, f.target.ApplicationID),
		Operation:       operation, Precondition: precondition, ExpectedETag: etag, Content: content,
		IntentDigest: digestBytes([]byte("application-intent-" + payload.ReleaseRevisionID)), Publisher: f.publisher,
		OriginalPublisherConfigDigest: f.publisher.ConfigDigest,
		Message:                       "publish protected application", State: ProtectedPending, NextAttemptAt: f.now, CreatedAt: f.now, UpdatedAt: f.now}
	if action == ProtectedApplicationDelete {
		value.CascadeRequired, value.CascadeReceiptID = true, value.ID
		value.CascadeContract = protectedCascadeContract
	}
	if action == ProtectedApplicationPublish {
		value.SourceDirectory = protectedSourceDirectory(f.target.EnvironmentID, f.target.ApplicationID, payload.ReleaseRevisionID)
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

type protectedRootRefresherStub struct {
	calls        int
	err          error
	beforeReturn func()
}

func (s *protectedRootRefresherStub) Validate() error {
	if s == nil {
		return ErrInvalid
	}
	return nil
}

func (s *protectedRootRefresherStub) RefreshProtectedRoot(_ context.Context,
	binding gitprojection.Binding, head gitprojection.VerifiedHead, now time.Time) error {
	if s.Validate() != nil || binding.Validate() != nil || head.ValidateFor(binding) != nil || now.Before(head.ObservedAt) {
		return ErrInvalid
	}
	s.calls++
	if s.beforeReturn != nil {
		s.beforeReturn()
	}
	return s.err
}

func (f *protectedPublisherFixture) worker(t *testing.T) *ProtectedGitPublisher {
	return &ProtectedGitPublisher{Store: f.store, Cascade: f.store, Activations: f.store,
		Bindings: f.bindings, Provider: f.headVerifier(t), Manager: f.manager,
		RootRefresher: f.refresher,
		Publisher:     f.publisher, WorkerID: "helm-protected-worker-0001", WorkerEpoch: 1,
		Now: func() time.Time { return f.now }}
}

func TestProtectedGitPublisherCommitsTwoPhasesAndMatchDeletesStableApplication(t *testing.T) {
	fixture := newProtectedPublisherFixture(t)
	payload, err := fixture.worker(t).ProcessPayloadOne(t.Context())
	if err != nil || payload.State != ProtectedVerified || payload.CommittedParentRevision != fixture.base {
		t.Fatalf("payload=%#v err=%v", payload, err)
	}
	manifest := []byte("apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata:\n  name: exact\n  finalizers:\n    - resources-finalizer.argocd.argoproj.io\n")
	fixture.store.application = fixture.pendingApplication(payload, manifest, ProtectedApplicationPublish, "create-if-absent", "")
	application, err := fixture.worker(t).ProcessApplicationOne(t.Context())
	if err != nil || application.State != ProtectedVerified || application.CommittedParentRevision != payload.CommittedRevision ||
		fixture.refresher.calls != 1 {
		t.Fatalf("application=%#v err=%v", application, err)
	}
	commitObject, err := runProtectedPublisherGit(t, fixture.remote, "cat-file", "commit", application.CommittedRevision)
	if err != nil || !strings.Contains(commitObject, "Kuberploy-Helm-Application-Intent: "+application.ID) ||
		!strings.Contains(commitObject, "Kuberploy-Operation: "+application.ID) {
		t.Fatalf("missing exact recovery trailers: %v\n%s", err, commitObject)
	}
	publishedManifest, err := runProtectedPublisherGit(t, fixture.remote, "show",
		application.CommittedRevision+":"+application.ApplicationPath)
	if err != nil {
		t.Fatal(err)
	}
	requireProtectedForegroundResourcesFinalizer(t, []byte(publishedManifest))
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
	if err != nil || deleted.State != ProtectedVerified || deleted.VerifiedPathDigest != "" || fixture.refresher.calls != 2 {
		t.Fatalf("deleted=%#v err=%v", deleted, err)
	}
	if _, showErr := runProtectedPublisherGit(t, fixture.remote, "show", deleted.CommittedRevision+":"+deleted.ApplicationPath); showErr == nil {
		t.Fatal("match-delete receipt was verified while the stable Application still existed")
	}
}

func TestProtectedGitPublisherRequiresRootRefreshBeforeApplicationVerification(t *testing.T) {
	fixture := newProtectedPublisherFixture(t)
	payload, err := fixture.worker(t).ProcessPayloadOne(t.Context())
	if err != nil || payload.State != ProtectedVerified || fixture.refresher.calls != 0 {
		t.Fatalf("payload=%#v refreshes=%d err=%v", payload, fixture.refresher.calls, err)
	}
	manifest := []byte("apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata:\n  name: exact\n  finalizers:\n    - resources-finalizer.argocd.argoproj.io\n")
	fixture.store.application = fixture.pendingApplication(payload, manifest,
		ProtectedApplicationPublish, "create-if-absent", "")
	fixture.refresher.err = ErrUnavailable
	first, err := fixture.worker(t).ProcessApplicationOne(t.Context())
	if !errors.Is(err, ErrUnavailable) || first.State != ProtectedGitCommitted ||
		first.CommittedRevision == "" || fixture.refresher.calls != 1 {
		t.Fatalf("first=%#v refreshes=%d err=%v", first, fixture.refresher.calls, err)
	}
	committed := first.CommittedRevision
	fixture.refresher.err = nil
	verified, err := fixture.worker(t).ProcessApplicationOne(t.Context())
	if err != nil || verified.State != ProtectedVerified || verified.CommittedRevision != committed ||
		fixture.refresher.calls != 2 {
		t.Fatalf("verified=%#v refreshes=%d err=%v", verified, fixture.refresher.calls, err)
	}
}

func TestProtectedGitPublisherVerifiesAfterRefreshHeartbeat(t *testing.T) {
	fixture := newProtectedPublisherFixture(t)
	payload, err := fixture.worker(t).ProcessPayloadOne(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	fixture.store.application = fixture.pendingApplication(payload,
		[]byte("apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata:\n  name: exact\n"),
		ProtectedApplicationPublish, "create-if-absent", "")

	var clockMu sync.Mutex
	clockNow := fixture.now
	now := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return clockNow
	}
	fixture.store.appHeartbeat = make(chan time.Time, 1)
	fixture.refresher.beforeReturn = func() {
		clockMu.Lock()
		target := fixture.now.Add(30 * time.Second)
		clockNow = target
		clockMu.Unlock()
		timeout := time.NewTimer(time.Second)
		defer timeout.Stop()
		for {
			select {
			case heartbeatAt := <-fixture.store.appHeartbeat:
				if !heartbeatAt.Before(target) {
					return
				}
			case <-timeout.C:
				t.Fatal("application refresh did not outlive a heartbeat")
			}
		}
	}
	worker := fixture.worker(t)
	worker.Now = now
	worker.HeartbeatInterval = 5 * time.Millisecond
	application, err := worker.ProcessApplicationOne(t.Context())
	if err != nil || application.State != ProtectedVerified || application.VerifiedAt == nil ||
		application.VerifiedAt.Before(clockNow) {
		t.Fatalf("application=%#v clock=%s err=%v", application, clockNow, err)
	}
}

func pathAbsentCascadeFixture(t *testing.T, fixture *protectedPublisherFixture) ProtectedApplicationCascadePreflight {
	t.Helper()
	release, payload, runtime := protectedApplicationFixture(t)
	release.Target = fixture.target
	payload.Target, payload.ReleaseRevisionID = fixture.target, release.ID
	payload.Binding.PlatformBindingID = fixture.binding.ID
	payload.Binding.PlatformTargetRef = fixture.binding.TargetRef
	payload.Binding.PlannedBaseRevision = fixture.base
	payload.Path = protectedPayloadPath(fixture.target.EnvironmentID,
		fixture.target.ApplicationID, release.ID, false)
	payload.WriteBaseRevision, payload.CommittedParentRevision = fixture.base, fixture.base
	payload.CommittedRevision = fixture.base
	adopted, err := renderProtectedArgoApplication(id.New(), release, payload, runtime,
		fixture.binding.Repository.Owner, fixture.binding.Repository.Name, "application-ns", "project-runtime")
	if err != nil {
		t.Fatal(err)
	}
	var legacy protectedArgoApplication
	if err = yaml.Unmarshal(adopted, &legacy); err != nil {
		t.Fatal(err)
	}
	legacy.Metadata.Finalizers = nil
	source, err := yaml.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	now := fixture.now
	preflight := ProtectedApplicationCascadePreflight{ID: id.New(), DeleteIntentID: id.New(),
		ReleaseRevisionID: id.New(), PayloadIntentID: id.New(), BaseApplicationIntentID: id.New(),
		PayloadRevision: fixture.base, ArgoNamespace: runtime.ArgoNamespace, ReleaseGeneration: 2,
		Target: fixture.target, Binding: payload.Binding,
		ApplicationPath: protectedApplicationPath(fixture.target.EnvironmentID, fixture.target.ApplicationID),
		SourceContent:   source, SourceContentDigest: digestBytes(source), AdoptedContent: adopted,
		AdoptedContentDigest: digestBytes(adopted), Operation: "update", Precondition: "match-etag",
		Contract: protectedCascadeContract, Publisher: fixture.publisher,
		OriginalPublisherConfigDigest: fixture.publisher.ConfigDigest,
		State:                         ProtectedPending, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now}
	preflight.ExpectedETag = `"` + preflight.SourceContentDigest + `"`
	preflight.CommitTrailer = "Kuberploy-Helm-Cascade-Preflight: " + preflight.ID
	preflight.IntentDigest, err = cascadePreflightIntentDigest(preflight)
	if err != nil || preflight.Validate() != nil {
		t.Fatalf("invalid cascade fixture: %+v err=%v", preflight, err)
	}
	return preflight
}

func commitProtectedPublisherFixturePath(t *testing.T, fixture *protectedPublisherFixture,
	path string, content []byte, message string) string {
	t.Helper()
	fullPath := filepath.Join(fixture.seed, filepath.FromSlash(path))
	if content == nil {
		if err := os.Remove(fullPath); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, content, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if output, err := runProtectedPublisherGit(t, fixture.seed, "add", "--all", "--", path); err != nil {
		t.Fatalf("stage protected fixture path: %v: %s", err, output)
	}
	if output, err := runProtectedPublisherGit(t, fixture.seed, "commit", "-m", message); err != nil {
		t.Fatalf("commit protected fixture path: %v: %s", err, output)
	}
	head, err := runProtectedPublisherGit(t, fixture.seed, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if output, pushErr := runProtectedPublisherGit(t, fixture.seed, "push", "--force-with-lease", "origin",
		"HEAD:"+fixture.binding.TargetRef); pushErr != nil {
		t.Fatalf("push protected fixture path: %v: %s", pushErr, output)
	}
	fixture.binding.TargetHeadRevision = head
	fixture.binding.TargetHeadObservedAt, fixture.binding.UpdatedAt = fixture.now, fixture.now
	if err = fixture.bindings.PutBinding(t.Context(), fixture.binding); err != nil {
		t.Fatal(err)
	}
	return head
}

func TestProtectedGitPublisherFailsClosedWhenLegacyCascadePathIsAbsent(t *testing.T) {
	fixture := newProtectedPublisherFixture(t)
	preflight := pathAbsentCascadeFixture(t, fixture)
	now := fixture.now
	protectedMutation, err := preflight.Mutation()
	if err != nil {
		t.Fatal(err)
	}
	gitMutation, err := protectedMutation.gitMutation()
	if err != nil || gitMutation.Validate(fixture.binding) != nil {
		t.Fatalf("invalid cascade Git mutation: %+v err=%v validate=%v binding=%+v",
			gitMutation, err, gitMutation.Validate(fixture.binding), fixture.binding)
	}
	fixture.store.cascade = preflight
	currentPublisher := fixture.publisher
	currentPublisher.ConfigDigest = digestBytes([]byte("path-absence-current-publisher"))
	fixture.publisher = currentPublisher
	headBefore, err := runProtectedPublisherGit(t, fixture.remote, "rev-parse", fixture.binding.TargetRef)
	if err != nil {
		t.Fatal(err)
	}
	result, err := fixture.worker(t).ProcessCascadePreflightOne(t.Context())
	if err != nil || result.State != ProtectedFailed ||
		result.LastFailureCode != "cascade-path-absent-recovery-required" ||
		fixture.store.absenceFailures != 1 || !fixture.store.absenceProof.OperationCommitAbsent ||
		fixture.store.absenceProof.ProviderHead != headBefore ||
		result.Publisher.ConfigDigest != currentPublisher.ConfigDigest || result.PublisherAdoptionEpoch != 1 {
		t.Fatalf("result=%+v proof=%+v failures=%d err=%v", result,
			fixture.store.absenceProof, fixture.store.absenceFailures, err)
	}
	headAfter, err := runProtectedPublisherGit(t, fixture.remote, "rev-parse", fixture.binding.TargetRef)
	if err != nil || headAfter != headBefore {
		t.Fatalf("path-absence recovery wrote Git: before=%s after=%s err=%v", headBefore, headAfter, err)
	}
	if _, err = fixture.worker(t).ProcessCascadePreflightOne(t.Context()); !errors.Is(err, ErrNotFound) ||
		fixture.store.absenceFailures != 1 {
		t.Fatalf("terminal path-absence preflight was reclaimed: failures=%d err=%v",
			fixture.store.absenceFailures, err)
	}
	committed := result
	committed.State, committed.LastFailureCode, committed.ConsecutiveFailures = ProtectedGitCommitted, "", 0
	committed.CompletedAt = nil
	committed.WriteBaseRevision = headBefore
	committed.WriteBaseObservedAt = &now
	committed.CommittedRevision = strings.Repeat("f", 40)
	committed.CommittedParentRevision = headBefore
	committed.CommittedAt = &now
	committed.LeaseOwner = "helm-publisher-git-committed-0001"
	committed.LeaseEpoch++
	committed.Attempts++
	leaseUntil := now.Add(time.Minute)
	committed.LeaseUntil = &leaseUntil
	if committed.Validate() != nil {
		t.Fatalf("invalid committed recovery fixture: %+v", committed)
	}
	fixture.store.cascade = committed
	if _, err = fixture.worker(t).ProcessCascadePreflightOne(t.Context()); !errors.Is(err, ErrConflict) ||
		fixture.store.absenceFailures != 1 {
		t.Fatalf("git-committed path absence was terminalized as no-effect: failures=%d err=%v",
			fixture.store.absenceFailures, err)
	}
}

func TestProtectedGitPublisherPathAbsenceProofRejectsAdjacentFailures(t *testing.T) {
	t.Run("wrong-etag", func(t *testing.T) {
		fixture := newProtectedPublisherFixture(t)
		preflight := pathAbsentCascadeFixture(t, fixture)
		driftedSource := append(append([]byte(nil), preflight.SourceContent...),
			[]byte("\n# provider-side path drift\n")...)
		base := commitProtectedPublisherFixturePath(t, fixture, preflight.ApplicationPath,
			driftedSource, "drift legacy protected Application")
		preflight.Binding.PlannedBaseRevision = base
		var err error
		preflight.IntentDigest, err = cascadePreflightIntentDigest(preflight)
		if err != nil || preflight.Validate() != nil {
			t.Fatalf("wrong-ETag durable preflight is invalid: %+v err=%v", preflight, err)
		}
		fixture.store.cascade = preflight
		if _, err = fixture.worker(t).ProcessCascadePreflightOne(t.Context()); !errors.Is(err, gitprojection.ErrConflict) ||
			errors.Is(err, gitprojection.ErrProtectedPathAbsent) || fixture.store.absenceFailures != 0 {
			t.Fatalf("wrong ETag entered path-absence recovery: failures=%d err=%v",
				fixture.store.absenceFailures, err)
		}
	})

	t.Run("provider-error", func(t *testing.T) {
		fixture := newProtectedPublisherFixture(t)
		fixture.store.cascade = pathAbsentCascadeFixture(t, fixture)
		worker := fixture.worker(t)
		providerErr := errors.New("provider unavailable")
		worker.Provider = protectedHeadVerifierFunc(func(context.Context, gitprojection.Binding,
			gitprojection.ObservationSource) (gitprojection.VerifiedHead, error) {
			return gitprojection.VerifiedHead{}, providerErr
		})
		if _, err := worker.ProcessCascadePreflightOne(t.Context()); !errors.Is(err, providerErr) ||
			fixture.store.absenceFailures != 0 {
			t.Fatalf("provider error entered path-absence recovery: failures=%d err=%v",
				fixture.store.absenceFailures, err)
		}
	})

	t.Run("operation-trailer-present", func(t *testing.T) {
		fixture := newProtectedPublisherFixture(t)
		preflight := pathAbsentCascadeFixture(t, fixture)
		base := commitProtectedPublisherFixturePath(t, fixture, preflight.ApplicationPath,
			preflight.SourceContent, "publish legacy protected Application")
		preflight.Binding.PlannedBaseRevision = base
		var err error
		preflight.IntentDigest, err = cascadePreflightIntentDigest(preflight)
		if err != nil || preflight.Validate() != nil {
			t.Fatalf("invalid operation recovery fixture: %+v err=%v", preflight, err)
		}
		protectedMutation, err := preflight.Mutation()
		if err != nil {
			t.Fatal(err)
		}
		mutation, err := protectedMutation.gitMutation()
		if err != nil {
			t.Fatal(err)
		}
		head, err := fixture.headVerifier(t).VerifyTargetHead(t.Context(), fixture.binding,
			gitprojection.ObservationWrite)
		if err != nil {
			t.Fatal(err)
		}
		prepared, err := fixture.manager.Prepare(t.Context(), fixture.binding, head, preflight.ID)
		if err != nil {
			t.Fatal(err)
		}
		operationCommit, err := prepared.Commit(t.Context(), mutation)
		if closeErr := prepared.Close(t.Context()); err == nil && closeErr != nil {
			err = closeErr
		}
		if err != nil {
			t.Fatal(err)
		}
		if output, fetchErr := runProtectedPublisherGit(t, fixture.seed, "fetch", "origin",
			fixture.binding.TargetRef); fetchErr != nil {
			t.Fatalf("fetch recovered operation: %v: %s", fetchErr, output)
		}
		if output, resetErr := runProtectedPublisherGit(t, fixture.seed, "reset", "--hard",
			operationCommit); resetErr != nil {
			t.Fatalf("reset to recovered operation: %v: %s", resetErr, output)
		}
		commitProtectedPublisherFixturePath(t, fixture, preflight.ApplicationPath, nil,
			"remove legacy protected Application after interrupted adoption")
		fixture.store.cascade = preflight
		if _, err = fixture.worker(t).ProcessCascadePreflightOne(t.Context()); !errors.Is(err, ErrConflict) || fixture.store.absenceFailures != 0 {
			t.Fatalf("existing operation trailer was treated as no-effect: failures=%d err=%v",
				fixture.store.absenceFailures, err)
		}
	})
}

func TestProtectedGitPublisherAdoptsExactCrossReleaseIntentsOnce(t *testing.T) {
	t.Run("payload", func(t *testing.T) {
		fixture := newProtectedPublisherFixture(t)
		originalDigest := fixture.publisher.ConfigDigest
		current := fixture.publisher
		current.ConfigDigest = digestBytes([]byte("protected-publisher-next-release"))
		worker := fixture.worker(t)
		worker.Publisher = current

		payload, err := worker.ProcessPayloadOne(t.Context())
		if err != nil || payload.State != ProtectedVerified ||
			payload.Publisher.ConfigDigest != current.ConfigDigest ||
			payload.OriginalPublisherConfigDigest != originalDigest ||
			payload.PublisherAdoptionEpoch != 1 || fixture.store.payloadAdoptions != 1 {
			t.Fatalf("payload=%+v adoptions=%d err=%v", payload, fixture.store.payloadAdoptions, err)
		}
		assertProtectedTrailerCount(t, fixture.remote, payload.CommitTrailer, 1)
		if _, err = worker.ProcessPayloadOne(t.Context()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("completed adopted payload replayed: %v", err)
		}
		assertProtectedTrailerCount(t, fixture.remote, payload.CommitTrailer, 1)
	})

	t.Run("application", func(t *testing.T) {
		fixture := newProtectedPublisherFixture(t)
		payload, err := fixture.worker(t).ProcessPayloadOne(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		fixture.store.application = fixture.pendingApplication(payload,
			[]byte("apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata:\n  name: adopted\n"),
			ProtectedApplicationPublish, "create-if-absent", "")
		originalDigest := fixture.publisher.ConfigDigest
		current := fixture.publisher
		current.ConfigDigest = digestBytes([]byte("protected-publisher-next-release"))
		worker := fixture.worker(t)
		worker.Publisher = current

		application, err := worker.ProcessApplicationOne(t.Context())
		if err != nil || application.State != ProtectedVerified ||
			application.Publisher.ConfigDigest != current.ConfigDigest ||
			application.OriginalPublisherConfigDigest != originalDigest ||
			application.PublisherAdoptionEpoch != 1 || fixture.store.appAdoptions != 1 {
			t.Fatalf("application=%+v adoptions=%d err=%v", application, fixture.store.appAdoptions, err)
		}
		assertProtectedTrailerCount(t, fixture.remote, application.CommitTrailer, 1)
		if _, err = worker.ProcessApplicationOne(t.Context()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("completed adopted Application replayed: %v", err)
		}
		assertProtectedTrailerCount(t, fixture.remote, application.CommitTrailer, 1)
	})
}

func TestProtectedGitPublisherFloorsAdoptedIntentTimestampsAcrossNegativeClockSkew(t *testing.T) {
	fixture := newProtectedPublisherFixture(t)
	adoptedAt := fixture.now.Add(2 * time.Minute)
	fixture.store.payload.UpdatedAt = adoptedAt
	fixture.store.payload.NextAttemptAt = adoptedAt
	current := fixture.publisher
	current.ConfigDigest = digestBytes([]byte("protected-publisher-clock-skew-release"))
	worker := fixture.worker(t)
	worker.Publisher = current

	payload, err := worker.ProcessPayloadOne(t.Context())
	if err != nil || payload.State != ProtectedVerified || payload.WriteBaseObservedAt == nil ||
		payload.CommittedAt == nil || payload.VerifiedAt == nil {
		t.Fatalf("clock-skew payload=%+v err=%v", payload, err)
	}
	for name, value := range map[string]time.Time{
		"write base": *payload.WriteBaseObservedAt,
		"committed":  *payload.CommittedAt,
		"verified":   *payload.VerifiedAt,
		"updated":    payload.UpdatedAt,
	} {
		if value.Before(adoptedAt) {
			t.Fatalf("%s timestamp=%s precedes adoption=%s", name, value, adoptedAt)
		}
	}
}

func TestProtectedPublisherReadinessRejectsOversizedLease(t *testing.T) {
	now := time.Now().UTC()
	readiness := ProtectedPublisherReadiness{WorkerID: "helm-publisher-readiness-0001", WorkerEpoch: 1,
		Publisher: ProtectedPublisherIdentity{Contract: ProtectedPublisherContract,
			PolicyVersion: ProtectedGitPolicy, ConfigDigest: digestBytes([]byte("publisher-readiness"))},
		StartedAt: now.Add(-time.Minute), ObservedAt: now,
		LeaseUntil: now.Add(maximumPublisherReadinessLease + time.Nanosecond)}
	if !errors.Is(readiness.Validate(), ErrInvalid) {
		t.Fatal("oversized protected publisher readiness lease was accepted")
	}
	readiness.LeaseUntil = now.Add(maximumPublisherReadinessLease)
	if err := readiness.Validate(); err != nil {
		t.Fatalf("exact maximum protected publisher readiness lease: %v", err)
	}
}

func TestProtectedPublicationLeaseGuardRetainsForwardHeartbeatFloorAcrossClockRegression(t *testing.T) {
	fixture := newProtectedPublisherFixture(t)
	adoptedAt := fixture.now
	forwardAt := adoptedAt.Add(30 * time.Second)
	backwardAt := adoptedAt.Add(-time.Minute)
	payload, lease, err := fixture.store.ClaimPayload(t.Context(), "helm-protected-worker-0001",
		fixture.publisher, adoptedAt, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	fixture.store.payloadHeartbeat = make(chan time.Time, 1)
	var clockMu sync.Mutex
	clockNow := forwardAt
	now := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return clockNow
	}
	guard := newProtectedPublicationLeaseGuard(t.Context(), lease, time.Minute,
		5*time.Millisecond, now, payload.UpdatedAt,
		func(ctx context.Context, current ProtectedIntentLease, heartbeatAt time.Time,
			duration time.Duration) (ProtectedIntentLease, error) {
			return fixture.store.HeartbeatPayload(ctx, current, heartbeatAt, duration)
		})
	defer guard.Close()
	select {
	case heartbeatAt := <-fixture.store.payloadHeartbeat:
		if !heartbeatAt.Equal(forwardAt) {
			t.Fatalf("heartbeat=%s want=%s", heartbeatAt, forwardAt)
		}
	case <-time.After(time.Second):
		t.Fatal("forward heartbeat was not observed")
	}
	clockMu.Lock()
	clockNow = backwardAt
	clockMu.Unlock()
	operationAt := guard.NotBefore(now())
	if operationAt.Before(forwardAt) {
		t.Fatalf("operation floor=%s regressed behind heartbeat=%s", operationAt, forwardAt)
	}
	if err = guard.Do(func(current ProtectedIntentLease) error {
		var bindErr error
		payload, bindErr = fixture.store.BindPayloadWriteBase(t.Context(), current,
			fixture.base, operationAt, operationAt)
		return bindErr
	}); err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("7", 40)
	if err = guard.Do(func(current ProtectedIntentLease) error {
		var markErr error
		payload, markErr = fixture.store.MarkPayloadCommitted(t.Context(), current,
			commit, fixture.base, operationAt)
		return markErr
	}); err != nil {
		t.Fatal(err)
	}
	if err = guard.Finish(func(current ProtectedIntentLease) error {
		var verifyErr error
		payload, verifyErr = fixture.store.VerifyPayload(t.Context(), current, commit,
			payload.ContentDigest, "clock-regression", operationAt)
		return verifyErr
	}); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]time.Time{
		"write base": *payload.WriteBaseObservedAt,
		"committed":  *payload.CommittedAt,
		"verified":   *payload.VerifiedAt,
		"updated":    payload.UpdatedAt,
	} {
		if value.Before(forwardAt) {
			t.Fatalf("%s timestamp=%s regressed behind heartbeat=%s", name, value, forwardAt)
		}
	}
}

func assertProtectedTrailerCount(t *testing.T, remote, trailer string, want int) {
	t.Helper()
	output, err := runProtectedPublisherGit(t, remote, "log", "--format=%B", "refs/heads/platform")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(output, trailer); got != want {
		t.Fatalf("trailer %q count=%d want=%d", trailer, got, want)
	}
}

func TestProtectedGitPublisherOrdersBothPrerequisiteRevisionsBeforeApplication(t *testing.T) {
	fixture := newProtectedPublisherFixture(t)
	if _, err := runProtectedPublisherGit(t, fixture.seed, "fetch", "origin", "refs/heads/platform"); err != nil {
		t.Fatal(err)
	}
	if _, err := runProtectedPublisherGit(t, fixture.seed, "reset", "--hard", "FETCH_HEAD"); err != nil {
		t.Fatal(err)
	}
	commit := func(name, body string) string {
		t.Helper()
		path := filepath.Join(fixture.seed, name)
		if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
		for _, command := range [][]string{{"add", name}, {"commit", "-m", name}} {
			if output, err := runProtectedPublisherGit(t, fixture.seed, command...); err != nil {
				t.Fatalf("git %v: %v: %s", command, err, output)
			}
		}
		revision, err := runProtectedPublisherGit(t, fixture.seed, "rev-parse", "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		return revision
	}
	foundationRevision := commit("foundation.yaml", "kind: Namespace\n")
	desiredStateRevision := commit("app-project.yaml", "kind: AppProject\n")
	if output, err := runProtectedPublisherGit(t, fixture.seed, "push", "origin", "HEAD:refs/heads/platform"); err != nil {
		t.Fatalf("push prerequisites: %v: %s", err, output)
	}
	fixture.binding.TargetHeadRevision = desiredStateRevision
	fixture.binding.TargetHeadObservedAt, fixture.binding.UpdatedAt = fixture.now, fixture.now
	if err := fixture.bindings.PutBinding(t.Context(), fixture.binding); err != nil {
		t.Fatal(err)
	}
	fixture.store.payload.Binding.PlannedBaseRevision = desiredStateRevision
	fixture.store.prerequisite.FoundationRevision = foundationRevision
	fixture.store.prerequisite.DesiredStateRevision = desiredStateRevision
	fixture.store.prerequisite.PlannedBaseRevision = desiredStateRevision

	payload, err := fixture.worker(t).ProcessPayloadOne(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	fixture.store.application = fixture.pendingApplication(payload,
		[]byte("apiVersion: argoproj.io/v1alpha1\nkind: Application\n"),
		ProtectedApplicationPublish, "create-if-absent", "")
	application, err := fixture.worker(t).ProcessApplicationOne(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for name, revision := range map[string]string{
		"foundation": foundationRevision, "Argo project": desiredStateRevision,
	} {
		if _, err = runProtectedPublisherGit(t, fixture.remote, "merge-base", "--is-ancestor", revision, application.CommittedRevision); err != nil {
			t.Fatalf("%s revision %s is not an ancestor of application %s: %v", name, revision, application.CommittedRevision, err)
		}
	}
}

func TestProtectedGitPublisherRebindsUncommittedIntentAfterUnrelatedHeadAdvance(t *testing.T) {
	advanceHead := func(t *testing.T, fixture *protectedPublisherFixture, name string) string {
		t.Helper()
		if _, err := runProtectedPublisherGit(t, fixture.seed, "fetch", "origin", "refs/heads/platform"); err != nil {
			t.Fatal(err)
		}
		if _, err := runProtectedPublisherGit(t, fixture.seed, "reset", "--hard", "FETCH_HEAD"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fixture.seed, name), []byte("unrelated protected-lane progress\n"), 0o640); err != nil {
			t.Fatal(err)
		}
		for _, command := range [][]string{{"add", name}, {"commit", "-m", "unrelated protected-lane progress"},
			{"push", "origin", "HEAD:refs/heads/platform"}} {
			if output, err := runProtectedPublisherGit(t, fixture.seed, command...); err != nil {
				t.Fatalf("git %v: %v: %s", command, err, output)
			}
		}
		revision, err := runProtectedPublisherGit(t, fixture.remote, "rev-parse", "refs/heads/platform")
		if err != nil {
			t.Fatal(err)
		}
		return revision
	}
	expireClaim := func(state *ProtectedIntentState, owner *string, attempts *int, epoch *int64,
		until **time.Time, writeBase *string, observedAt **time.Time, base string, now time.Time) {
		*state, *owner, *attempts, *epoch = ProtectedClaimed, "expired-worker-0001", 1, 1
		*until, *writeBase, *observedAt = &now, base, &now
	}

	t.Run("payload", func(t *testing.T) {
		fixture := newProtectedPublisherFixture(t)
		intent := &fixture.store.payload
		expireClaim(&intent.State, &intent.LeaseOwner, &intent.Attempts, &intent.LeaseEpoch,
			&intent.LeaseUntil, &intent.WriteBaseRevision, &intent.WriteBaseObservedAt, fixture.base, fixture.now)
		unrelated := advanceHead(t, fixture, "unrelated-payload.yaml")

		published, err := fixture.worker(t).ProcessPayloadOne(t.Context())
		if err != nil || published.State != ProtectedVerified || published.CommittedParentRevision != unrelated ||
			fixture.store.payloadRebinds != 1 {
			t.Fatalf("published=%#v rebinds=%d err=%v", published, fixture.store.payloadRebinds, err)
		}
		log, err := runProtectedPublisherGit(t, fixture.remote, "log", "--format=%B", "refs/heads/platform")
		if err != nil || strings.Count(log, "Kuberploy-Helm-Payload-Intent: "+published.ID) != 1 {
			t.Fatalf("operation was not published exactly once: %v\n%s", err, log)
		}
		if _, err = fixture.worker(t).ProcessPayloadOne(t.Context()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("verified operation was processed twice: %v", err)
		}
	})

	t.Run("application", func(t *testing.T) {
		fixture := newProtectedPublisherFixture(t)
		payload, err := fixture.worker(t).ProcessPayloadOne(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		fixture.store.application = fixture.pendingApplication(payload,
			[]byte("apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata:\n  name: exact\n"),
			ProtectedApplicationPublish, "create-if-absent", "")
		intent := &fixture.store.application
		expireClaim(&intent.State, &intent.LeaseOwner, &intent.Attempts, &intent.LeaseEpoch,
			&intent.LeaseUntil, &intent.WriteBaseRevision, &intent.WriteBaseObservedAt,
			payload.CommittedRevision, fixture.now)
		unrelated := advanceHead(t, fixture, "unrelated-application.yaml")

		published, err := fixture.worker(t).ProcessApplicationOne(t.Context())
		if err != nil || published.State != ProtectedVerified || published.CommittedParentRevision != unrelated ||
			fixture.store.appRebinds != 1 {
			t.Fatalf("published=%#v rebinds=%d err=%v", published, fixture.store.appRebinds, err)
		}
		log, err := runProtectedPublisherGit(t, fixture.remote, "log", "--format=%B", "refs/heads/platform")
		if err != nil || strings.Count(log, "Kuberploy-Helm-Application-Intent: "+published.ID) != 1 {
			t.Fatalf("operation was not published exactly once: %v\n%s", err, log)
		}
		if _, err = fixture.worker(t).ProcessApplicationOne(t.Context()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("verified operation was processed twice: %v", err)
		}
	})
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

func TestProtectedGitPublisherRecoversLegacyApplicationPushAfterV004Adoption(t *testing.T) {
	for _, state := range []ProtectedIntentState{ProtectedClaimed, ProtectedGitCommitted} {
		t.Run(string(state), func(t *testing.T) {
			fixture := newProtectedPublisherFixture(t)
			payload, err := fixture.worker(t).ProcessPayloadOne(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			fixture.store.application = fixture.pendingApplication(payload,
				[]byte("apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata:\n  name: legacy\n"),
				ProtectedApplicationPublish, "create-if-absent", "")
			intent := &fixture.store.application
			intent.State, intent.LeaseOwner, intent.Attempts, intent.LeaseEpoch =
				ProtectedClaimed, "helm-protected-worker-0001", 1, 1
			until := fixture.now.Add(time.Minute)
			intent.LeaseUntil = &until
			head, err := fixture.headVerifier(t).VerifyTargetHead(t.Context(), fixture.binding,
				gitprojection.ObservationWrite)
			if err != nil {
				t.Fatal(err)
			}
			intent.WriteBaseRevision, intent.WriteBaseObservedAt = head.Commit, &fixture.now
			mutation, err := intent.Mutation()
			if err != nil {
				t.Fatal(err)
			}
			gitMutation, err := mutation.gitMutation()
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := fixture.manager.Prepare(t.Context(), fixture.binding, head, intent.ID)
			if err != nil {
				t.Fatal(err)
			}
			pushed, err := prepared.Commit(t.Context(), gitMutation)
			if err != nil {
				t.Fatal(err)
			}
			if err = prepared.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			if state == ProtectedGitCommitted {
				intent.State = ProtectedGitCommitted
				intent.CommittedRevision, intent.CommittedParentRevision = pushed, head.Commit
				intent.CommittedAt = &fixture.now
			}
			// 004 adopts the exact legacy row only after its old lease expires.
			// The new publisher must find the old trailer, prove prerequisite and
			// payload ancestry, verify the exact stable path, and finish in place.
			intent.LeaseUntil, intent.LeaseOwner = &fixture.now, ""
			recovered, err := fixture.worker(t).ProcessApplicationOne(t.Context())
			if err != nil || recovered.State != ProtectedVerified || recovered.CommittedRevision != pushed ||
				recovered.VerifiedPathDigest != recovered.ContentDigest {
				t.Fatalf("recovered=%+v pushed=%s err=%v", recovered, pushed, err)
			}
			for name, revision := range map[string]string{
				"foundation": fixture.store.prerequisite.FoundationRevision,
				"AppProject": fixture.store.prerequisite.DesiredStateRevision,
				"payload":    payload.CommittedRevision,
			} {
				if _, err = runProtectedPublisherGit(t, fixture.remote, "merge-base", "--is-ancestor",
					revision, recovered.CommittedRevision); err != nil {
					t.Fatalf("%s revision %s not in recovered application ancestry: %v", name, revision, err)
				}
			}
		})
	}
}

func TestProtectedGitPublisherRejectsDigestCASAncestryAndTrailerSubstitution(t *testing.T) {
	t.Run("publication prerequisite", func(t *testing.T) {
		for _, test := range []struct {
			name   string
			mutate func(*protectedPublisherFixture)
			err    error
		}{
			{name: "missing foundation", mutate: func(f *protectedPublisherFixture) {
				f.store.prerequisiteErr = ErrFoundationNotReady
			}, err: ErrFoundationNotReady},
			{name: "missing Argo project", mutate: func(f *protectedPublisherFixture) {
				f.store.prerequisiteErr = ErrArgoProjectNotReady
			}, err: ErrArgoProjectNotReady},
			{name: "foundation outside ancestry", mutate: func(f *protectedPublisherFixture) {
				f.store.prerequisite.FoundationRevision = strings.Repeat("f", 40)
			}, err: gitprojection.ErrConflict},
			{name: "Argo project outside ancestry", mutate: func(f *protectedPublisherFixture) {
				f.store.prerequisite.DesiredStateRevision = strings.Repeat("e", 40)
			}, err: gitprojection.ErrConflict},
		} {
			t.Run(test.name, func(t *testing.T) {
				fixture := newProtectedPublisherFixture(t)
				test.mutate(fixture)
				before, gitErr := runProtectedPublisherGit(t, fixture.remote, "rev-parse", "refs/heads/platform")
				if gitErr != nil {
					t.Fatal(gitErr)
				}
				if _, err := fixture.worker(t).ProcessPayloadOne(t.Context()); !errors.Is(err, test.err) {
					t.Fatalf("publication prerequisite error=%v, want %v", err, test.err)
				}
				after, gitErr := runProtectedPublisherGit(t, fixture.remote, "rev-parse", "refs/heads/platform")
				if gitErr != nil || after != before {
					t.Fatalf("denied prerequisite mutated Git: before=%s after=%s err=%v", before, after, gitErr)
				}
			})
		}
	})

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
