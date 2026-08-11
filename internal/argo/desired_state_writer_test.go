package argo_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/argo"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

type desiredStateVerifierFunc func(context.Context, gitprojection.Binding, gitprojection.ObservationSource) (gitprojection.VerifiedHead, error)

func (f desiredStateVerifierFunc) VerifyTargetHead(ctx context.Context, binding gitprojection.Binding, source gitprojection.ObservationSource) (gitprojection.VerifiedHead, error) {
	return f(ctx, binding, source)
}

type failingDesiredStateHeartbeatStore struct {
	argo.DesiredStateStore
	heartbeat chan struct{}
}

func (s *failingDesiredStateHeartbeatStore) HeartbeatDesiredState(context.Context, argo.DesiredStateLease, time.Time, time.Duration) (argo.DesiredStateLease, error) {
	select {
	case s.heartbeat <- struct{}{}:
	default:
	}
	return argo.DesiredStateLease{}, argo.ErrLeaseLost
}

type observingDesiredStateStore struct {
	*argo.MemoryDesiredStateStore
	heartbeat chan struct{}
}

func (s *observingDesiredStateStore) HeartbeatDesiredState(ctx context.Context, lease argo.DesiredStateLease, now time.Time, duration time.Duration) (argo.DesiredStateLease, error) {
	updated, err := s.MemoryDesiredStateStore.HeartbeatDesiredState(ctx, lease, now, duration)
	if err == nil {
		select {
		case s.heartbeat <- struct{}{}:
		default:
		}
	}
	return updated, err
}

type failingDesiredStateClaimStore struct {
	*argo.MemoryDesiredStateStore
	err error
}

func (s *failingDesiredStateClaimStore) ClaimDesiredState(context.Context, string, argo.DesiredStateWorkerIdentity, time.Time, time.Duration) (argo.DesiredStateWork, error) {
	return argo.DesiredStateWork{}, s.err
}

func runDesiredStateGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	command.Env = append(os.Environ(), "LC_ALL=C", "GIT_AUTHOR_NAME=Argo Test", "GIT_AUTHOR_EMAIL=argo@example.invalid", "GIT_COMMITTER_NAME=Argo Test", "GIT_COMMITTER_EMAIL=argo@example.invalid")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

type desiredStateWriterFixture struct {
	target      argo.DesiredStateTarget
	command     argo.DesiredStateCommand
	identity    argo.DesiredStateRuntimeIdentity
	bindings    *gitprojection.MemoryStore
	commands    *argo.MemoryDesiredStateStore
	manager     *gitprojection.MirrorManager
	claimGate   argo.DesiredStateClaimGate
	remote      string
	seed        string
	baseHead    string
	claim       argo.DesiredStateWork
	now         time.Time
	providerNow time.Time
}

func newDesiredStateWriterFixture(t *testing.T) *desiredStateWriterFixture {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	base := t.TempDir()
	remote := filepath.Join(base, "platform.git")
	seed := filepath.Join(base, "seed")
	if err := os.Mkdir(seed, 0o750); err != nil {
		t.Fatal(err)
	}
	runDesiredStateGit(t, base, "init", "--bare", remote)
	runDesiredStateGit(t, seed, "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("protected platform repository\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	runDesiredStateGit(t, seed, "add", "README.md")
	runDesiredStateGit(t, seed, "commit", "-m", "seed")
	baseHead := runDesiredStateGit(t, seed, "rev-parse", "HEAD")
	runDesiredStateGit(t, seed, "remote", "add", "origin", remote)
	runDesiredStateGit(t, seed, "push", "origin", "HEAD:refs/heads/platform")

	target, applications, deployments := desiredStateTargetFixture(t, now)
	target.PlatformBinding.TargetHeadRevision = baseHead
	target.PlatformBinding.TargetHeadObservedAt, target.PlatformBinding.UpdatedAt = now, now
	command, err := planDesiredStateCommand(t, desiredStateCommandID, target, applications, deployments, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	bindings := gitprojection.NewMemoryStore()
	if err = bindings.PutBinding(t.Context(), target.PlatformBinding); err != nil {
		t.Fatal(err)
	}
	if err = bindings.PutBinding(t.Context(), target.Environment.Binding); err != nil {
		t.Fatal(err)
	}
	commands := argo.NewMemoryDesiredStateStore()
	if _, err = commands.CreateDesiredState(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	identity := desiredStateIdentity(t, target)
	claimGate := staticDesiredStateProjectionGate{approval: desiredStateApproval(t, target, applications, deployments)}
	claim, err := commands.ClaimDesiredState(t.Context(), "argo-writer-worker-a", identity.DesiredStateWorkerIdentity, now.Add(time.Second), 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return &desiredStateWriterFixture{target: target, command: command, identity: identity, bindings: bindings, commands: commands,
		manager:   &gitprojection.MirrorManager{Root: filepath.Join(base, "cache"), AllowLocalTests: true, LocalRemote: remote},
		claimGate: claimGate, remote: remote, seed: seed, baseHead: baseHead, claim: claim,
		now: now.Add(2 * time.Second), providerNow: now.Add(2 * time.Second)}
}

func (f *desiredStateWriterFixture) provider(t *testing.T, override func(int, string) string) gitprojection.HeadVerifier {
	t.Helper()
	calls := 0
	return desiredStateVerifierFunc(func(_ context.Context, binding gitprojection.Binding, source gitprojection.ObservationSource) (gitprojection.VerifiedHead, error) {
		calls++
		actual := runDesiredStateGit(t, f.remote, "rev-parse", binding.TargetRef)
		if override != nil {
			actual = override(calls, actual)
		}
		f.providerNow = f.providerNow.Add(time.Millisecond)
		return gitprojection.VerifiedHead{BindingID: binding.ID, Repository: binding.Repository, TargetRef: binding.TargetRef,
			Commit: actual, Source: source, ProviderRequest: "argo-desired-state-provider-read", ObservedAt: f.providerNow}, nil
	})
}

func (f *desiredStateWriterFixture) writer(provider gitprojection.HeadVerifier) *argo.DesiredStateWriter {
	return &argo.DesiredStateWriter{Store: f.commands, Bindings: f.bindings, ClaimGate: f.claimGate, Provider: provider, Manager: f.manager,
		Identity: f.identity, Now: func() time.Time { return f.now }}
}

func (f *desiredStateWriterFixture) advancePlatform(t *testing.T, mutate func(string) error) string {
	t.Helper()
	runDesiredStateGit(t, f.seed, "fetch", "origin", "refs/heads/platform")
	runDesiredStateGit(t, f.seed, "reset", "--hard", "FETCH_HEAD")
	if err := mutate(f.seed); err != nil {
		t.Fatal(err)
	}
	runDesiredStateGit(t, f.seed, "add", "--all")
	runDesiredStateGit(t, f.seed, "commit", "-m", "advance protected platform ref")
	revision := runDesiredStateGit(t, f.seed, "rev-parse", "HEAD")
	runDesiredStateGit(t, f.seed, "push", "origin", "HEAD:refs/heads/platform")
	return revision
}

func (f *desiredStateWriterFixture) advanceUnrelated(t *testing.T) string {
	t.Helper()
	return f.advancePlatform(t, func(seed string) error {
		return os.WriteFile(filepath.Join(seed, "unrelated.txt"), []byte("unrelated protected change\n"), 0o640)
	})
}

func (f *desiredStateWriterFixture) mutateProtectedPath(t *testing.T) string {
	t.Helper()
	return f.advancePlatform(t, func(seed string) error {
		document := filepath.Join(seed, filepath.FromSlash(f.command.Path))
		if err := os.MkdirAll(filepath.Dir(document), 0o750); err != nil {
			return err
		}
		return os.WriteFile(document, []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: bypass\n"), 0o640)
	})
}

func (f *desiredStateWriterFixture) advanceEnvironmentHead(t *testing.T) {
	t.Helper()
	f.providerNow = f.providerNow.Add(time.Second)
	binding := f.target.Environment.Binding
	_, _, err := f.bindings.RecordVerifiedHead(t.Context(), gitprojection.VerifiedHead{
		BindingID: binding.ID, Repository: binding.Repository, TargetRef: binding.TargetRef,
		Commit: strings.Repeat("9", 40), Source: gitprojection.ObservationPoll,
		ProviderRequest: "environment-advanced-after-approval", ObservedAt: f.providerNow,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDesiredStateWriterRecoversPushBeforeDatabaseAcknowledgement(t *testing.T) {
	fixture := newDesiredStateWriterFixture(t)
	head := gitprojection.VerifiedHead{BindingID: fixture.target.PlatformBinding.ID, Repository: fixture.target.PlatformBinding.Repository,
		TargetRef: fixture.target.PlatformBinding.TargetRef, Commit: fixture.baseHead, Source: gitprojection.ObservationWrite,
		ProviderRequest: "pre-crash-provider-read", ObservedAt: fixture.providerNow}
	prepared, err := fixture.manager.Prepare(t.Context(), fixture.target.PlatformBinding, head, fixture.command.ID)
	if err != nil {
		t.Fatal(err)
	}
	receipted, err := fixture.commands.BindDesiredStateWriteBase(t.Context(), fixture.claim.Lease, fixture.baseHead, head.ObservedAt, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := prepared.Commit(t.Context(), receipted.Mutation())
	if err != nil {
		t.Fatal(err)
	}
	if err = prepared.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	descendant := fixture.advanceUnrelated(t)
	fixture.advanceEnvironmentHead(t)
	before, err := fixture.commands.DesiredStateCommand(t.Context(), fixture.command.ID)
	if err != nil || before.State != argo.DesiredStateClaimed || before.CommittedRevision != "" {
		t.Fatalf("pre-recovery command=%#v err=%v", before, err)
	}
	recovered, err := fixture.writer(fixture.provider(t, nil)).CommitClaim(t.Context(), fixture.claim.Lease)
	if err != nil || recovered.State != argo.DesiredStateVerified || recovered.CommittedRevision != revision {
		t.Fatalf("recovered=%#v err=%v", recovered, err)
	}
	if recovered.CommittedRevision == descendant {
		t.Fatal("recovery recorded a descendant instead of the exact operation commit")
	}
	if got := runDesiredStateGit(t, fixture.remote, "show", revision+":"+fixture.command.Path); got != strings.TrimSpace(string(fixture.command.Content)) {
		t.Fatal("recovered commit does not contain exact durable manifest bytes")
	}
}

func TestDesiredStateWriterRecoversCrashAfterWriteBaseReceiptBeforePush(t *testing.T) {
	fixture := newDesiredStateWriterFixture(t)
	receipted, err := fixture.commands.BindDesiredStateWriteBase(t.Context(), fixture.claim.Lease, fixture.baseHead, fixture.providerNow, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	if receipted.WriteBaseRevision != fixture.baseHead || receipted.WriteBaseObservedAt == nil {
		t.Fatalf("durable write-base receipt is incomplete: %#v", receipted)
	}
	verified, err := fixture.writer(fixture.provider(t, nil)).CommitClaim(t.Context(), fixture.claim.Lease)
	if err != nil || verified.State != argo.DesiredStateVerified || verified.BaseRevision != fixture.baseHead || verified.WriteBaseRevision != fixture.baseHead {
		t.Fatalf("receipt-before-push recovery=%#v err=%v", verified, err)
	}
}

func TestDesiredStateWriterSerializesTwoPlannedCommandsAndBindsDescendantWriteBase(t *testing.T) {
	fixture := newDesiredStateWriterFixture(t)
	const (
		secondEnvironmentID = "3a111111-1111-4111-8111-111111111111"
		secondBindingID     = "1b111111-1111-4111-8111-111111111111"
		secondApplicationID = "4c111111-1111-4111-8111-111111111111"
		secondDeploymentID  = "9d111111-1111-4111-8111-111111111111"
		secondCommandID     = "1e111111-1111-4111-8111-111111111111"
	)
	createdAt := fixture.command.CreatedAt
	secondEnvironment := domain.Environment{ID: secondEnvironmentID, ProjectID: fixture.target.Environment.Project.ID,
		Name: "Staging", Slug: "staging", CreatedAt: createdAt}
	secondEnvironment.Namespace, secondEnvironment.ArgoProject = domain.DeriveEnvironmentDestination(fixture.target.Environment.Project, secondEnvironment.Slug)
	secondBinding, err := gitprojection.NewGitHubEnvironmentBinding(secondBindingID, fixture.target.Environment.Project.ID,
		secondEnvironment.ID, fixture.target.Environment.Binding.Repository, fixture.target.Environment.Binding.TargetRef, createdAt)
	if err != nil {
		t.Fatal(err)
	}
	secondBinding.TargetHeadRevision = fixture.target.Environment.Binding.IndexedRevision
	secondBinding.IndexedRevision = fixture.target.Environment.Binding.IndexedRevision
	secondBinding.TargetHeadObservedAt, secondBinding.IndexedAt = createdAt, createdAt
	secondBinding.ProjectionGeneration, secondBinding.State, secondBinding.UpdatedAt = 1, gitprojection.BindingReady, createdAt
	secondTarget := argo.DesiredStateTarget{
		Environment: argo.EnvironmentTarget{Project: fixture.target.Environment.Project, Environment: secondEnvironment,
			Binding: secondBinding, ArgoNamespace: fixture.target.Environment.ArgoNamespace, Runtime: fixture.target.Environment.Runtime},
		PlatformBinding: fixture.target.PlatformBinding,
	}
	secondApplications := []domain.Application{{ID: secondApplicationID, ProjectID: secondTarget.Environment.Project.ID}}
	secondDeployments := []domain.Deployment{{ID: secondDeploymentID, ApplicationID: secondApplicationID, EnvironmentID: secondEnvironmentID}}
	secondApproval := desiredStateApproval(t, secondTarget, secondApplications, secondDeployments)
	secondCommand, err := (argo.DesiredStatePlanner{Projection: staticDesiredStateProjectionGate{approval: secondApproval},
		RegistryEligibility: &staticDesiredStateRegistryEligibility{resolved: true}}).Plan(
		t.Context(), secondCommandID, secondTarget, nil, createdAt)
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.bindings.PutBinding(t.Context(), secondBinding); err != nil {
		t.Fatal(err)
	}
	if created, createErr := fixture.commands.CreateDesiredState(t.Context(), secondCommand); createErr != nil || !created {
		t.Fatalf("second command created=%v err=%v", created, createErr)
	}
	if _, claimErr := fixture.commands.ClaimDesiredState(t.Context(), "argo-writer-worker-b", fixture.identity.DesiredStateWorkerIdentity, fixture.now, 2*time.Minute); !errors.Is(claimErr, argo.ErrNotFound) {
		t.Fatalf("second command bypassed the platform-binding lease lane: %v", claimErr)
	}
	first, err := fixture.writer(fixture.provider(t, nil)).CommitClaim(t.Context(), fixture.claim.Lease)
	if err != nil || first.State != argo.DesiredStateVerified {
		t.Fatalf("first command=%#v err=%v", first, err)
	}
	fixture.now = fixture.now.Add(time.Second)
	secondWork, err := fixture.commands.ClaimDesiredState(t.Context(), "argo-writer-worker-b", fixture.identity.DesiredStateWorkerIdentity, fixture.now, 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	secondWriter := fixture.writer(fixture.provider(t, nil))
	secondWriter.ClaimGate = staticDesiredStateProjectionGate{approval: secondApproval}
	second, err := secondWriter.CommitClaim(t.Context(), secondWork.Lease)
	if err != nil || second.State != argo.DesiredStateVerified {
		t.Fatalf("second command=%#v err=%v", second, err)
	}
	if second.BaseRevision != fixture.baseHead || second.WriteBaseRevision != first.CommittedRevision ||
		second.CommittedRevision == first.CommittedRevision {
		t.Fatalf("second command did not retain planned authority and bind the descendant write base: first=%#v second=%#v", first, second)
	}
}

func TestDesiredStateWriterRejectsNewMutationAfterProjectionGenerationChanges(t *testing.T) {
	fixture := newDesiredStateWriterFixture(t)
	fixture.advanceEnvironmentHead(t)
	_, err := fixture.writer(fixture.provider(t, nil)).CommitClaim(t.Context(), fixture.claim.Lease)
	if !errors.Is(err, argo.ErrInvalid) {
		t.Fatalf("stale projection generation reached a new Git mutation: %v", err)
	}
	if head := runDesiredStateGit(t, fixture.remote, "rev-parse", fixture.target.PlatformBinding.TargetRef); head != fixture.baseHead {
		t.Fatalf("stale projection moved protected ref to %s", head)
	}
}

func TestDesiredStateWriterRequiresPostPushProviderHeadBeforeTerminalSuccess(t *testing.T) {
	fixture := newDesiredStateWriterFixture(t)
	provider := fixture.provider(t, func(call int, actual string) string {
		if call == 2 {
			return strings.Repeat("f", 40)
		}
		return actual
	})
	_, err := fixture.writer(provider).CommitClaim(t.Context(), fixture.claim.Lease)
	if !errors.Is(err, gitprojection.ErrProviderMismatch) {
		t.Fatalf("unverified post-push head error=%v", err)
	}
	committed, readErr := fixture.commands.DesiredStateCommand(t.Context(), fixture.command.ID)
	if readErr != nil || committed.State != argo.DesiredStateGitCommitted || committed.CommittedRevision == "" || committed.CompletedAt != nil {
		t.Fatalf("command became terminal without provider proof: %#v err=%v", committed, readErr)
	}
	descendant := fixture.advanceUnrelated(t)
	fixture.now = fixture.now.Add(time.Second)
	verified, err := fixture.writer(fixture.provider(t, nil)).CommitClaim(t.Context(), fixture.claim.Lease)
	if err != nil || verified.State != argo.DesiredStateVerified || verified.CommittedRevision != committed.CommittedRevision {
		t.Fatalf("acknowledged crash recovery=%#v err=%v", verified, err)
	}
	if verified.CommittedRevision == descendant {
		t.Fatal("acknowledged recovery recorded a descendant instead of the operation commit")
	}
}

func TestDesiredStateWriterFinalizesCommittedReceiptAfterRuntimeRotation(t *testing.T) {
	fixture := newDesiredStateWriterFixture(t)
	provider := fixture.provider(t, func(call int, actual string) string {
		if call == 2 {
			return strings.Repeat("f", 40)
		}
		return actual
	})
	_, err := fixture.writer(provider).CommitClaim(t.Context(), fixture.claim.Lease)
	if !errors.Is(err, gitprojection.ErrProviderMismatch) {
		t.Fatalf("expected durable command pending provider proof: %v", err)
	}
	fixture.now = fixture.now.Add(time.Second)
	if _, err = fixture.commands.RetryDesiredState(t.Context(), fixture.claim.Lease,
		argo.DesiredStateRetry{FailureCode: "provider-mismatch", NextAttemptAt: fixture.now}, fixture.now); err != nil {
		t.Fatal(err)
	}
	rotatedTarget := fixture.target
	rotatedTarget.Environment.Runtime.ChartVersion = "1.2.4"
	rotatedTarget.Environment.Runtime.ChartDigest = "sha256:" + strings.Repeat("e", 64)
	rotatedIdentity := desiredStateIdentity(t, rotatedTarget)
	work, err := fixture.commands.ClaimDesiredState(t.Context(), "argo-writer-worker-rotated", rotatedIdentity.DesiredStateWorkerIdentity, fixture.now, 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	writer := fixture.writer(fixture.provider(t, nil))
	writer.Identity = rotatedIdentity
	verified, err := writer.CommitClaim(t.Context(), work.Lease)
	if err != nil || verified.State != argo.DesiredStateVerified || verified.Runtime != fixture.command.Runtime {
		t.Fatalf("runtime rotation stranded immutable committed receipt: command=%#v err=%v", verified, err)
	}
}

func TestDesiredStateWriterRejectsRecoveryWhenProtectedPathChanged(t *testing.T) {
	fixture := newDesiredStateWriterFixture(t)
	provider := fixture.provider(t, func(call int, actual string) string {
		if call == 2 {
			return strings.Repeat("f", 40)
		}
		return actual
	})
	_, err := fixture.writer(provider).CommitClaim(t.Context(), fixture.claim.Lease)
	if !errors.Is(err, gitprojection.ErrProviderMismatch) {
		t.Fatalf("expected an acknowledged command pending provider proof: %v", err)
	}
	fixture.mutateProtectedPath(t)
	fixture.now = fixture.now.Add(time.Second)
	_, err = fixture.writer(fixture.provider(t, nil)).CommitClaim(t.Context(), fixture.claim.Lease)
	if !errors.Is(err, gitprojection.ErrConflict) {
		t.Fatalf("mutated protected path was accepted during recovery: %v", err)
	}
	current, readErr := fixture.commands.DesiredStateCommand(t.Context(), fixture.command.ID)
	if readErr != nil || current.State != argo.DesiredStateGitCommitted || current.CompletedAt != nil {
		t.Fatalf("mutated recovery became terminal: %#v err=%v", current, readErr)
	}
}

func TestDesiredStateWriterCancelsProviderIOWhenLeaseHeartbeatIsLost(t *testing.T) {
	fixture := newDesiredStateWriterFixture(t)
	store := &failingDesiredStateHeartbeatStore{DesiredStateStore: fixture.commands, heartbeat: make(chan struct{}, 1)}
	provider := desiredStateVerifierFunc(func(ctx context.Context, _ gitprojection.Binding, _ gitprojection.ObservationSource) (gitprojection.VerifiedHead, error) {
		<-ctx.Done()
		return gitprojection.VerifiedHead{}, ctx.Err()
	})
	writer := fixture.writer(provider)
	writer.Store = store
	writer.LeaseDuration = 30 * time.Second
	writer.HeartbeatInterval = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err := writer.CommitClaim(ctx, fixture.claim.Lease)
	if !errors.Is(err, argo.ErrLeaseLost) {
		t.Fatalf("provider I/O continued after lease loss: %v", err)
	}
	select {
	case <-store.heartbeat:
	default:
		t.Fatal("command lease was not heartbeated during provider I/O")
	}
	current, readErr := fixture.commands.DesiredStateCommand(t.Context(), fixture.command.ID)
	if readErr != nil || current.State != argo.DesiredStateClaimed || current.CompletedAt != nil {
		t.Fatalf("lease loss mutated terminal state: %#v err=%v", current, readErr)
	}
}

func TestDesiredStateWriterStopsHeartbeatBeforeTerminalLeaseClear(t *testing.T) {
	fixture := newDesiredStateWriterFixture(t)
	store := &observingDesiredStateStore{MemoryDesiredStateStore: fixture.commands, heartbeat: make(chan struct{}, 1)}
	var clockTick atomic.Int64
	clock := func() time.Time { return fixture.now.Add(time.Duration(clockTick.Add(1)) * time.Millisecond) }
	var providerCalls atomic.Int64
	provider := desiredStateVerifierFunc(func(ctx context.Context, binding gitprojection.Binding, source gitprojection.ObservationSource) (gitprojection.VerifiedHead, error) {
		if providerCalls.Add(1) == 1 {
			select {
			case <-store.heartbeat:
			case <-ctx.Done():
				return gitprojection.VerifiedHead{}, ctx.Err()
			}
		}
		actual := runDesiredStateGit(t, fixture.remote, "rev-parse", binding.TargetRef)
		return gitprojection.VerifiedHead{BindingID: binding.ID, Repository: binding.Repository, TargetRef: binding.TargetRef,
			Commit: actual, Source: source, ProviderRequest: "terminal-heartbeat-provider-read", ObservedAt: clock()}, nil
	})
	writer := fixture.writer(provider)
	writer.Store, writer.Now = store, clock
	writer.LeaseDuration, writer.HeartbeatInterval = 30*time.Second, 5*time.Millisecond
	verified, err := writer.CommitClaim(t.Context(), fixture.claim.Lease)
	if err != nil || verified.State != argo.DesiredStateVerified || verified.Lease != nil {
		t.Fatalf("terminal lease clear raced its heartbeat: %#v err=%v", verified, err)
	}
}

func TestDesiredStateWriterRevalidatesProjectionApprovalBeforeGitMutation(t *testing.T) {
	fixture := newDesiredStateWriterFixture(t)
	gate := fixture.claimGate.(staticDesiredStateProjectionGate)
	gate.approval.SecretReferencesResolved = false
	fixture.claimGate = gate
	providerCalled := false
	provider := desiredStateVerifierFunc(func(_ context.Context, binding gitprojection.Binding, source gitprojection.ObservationSource) (gitprojection.VerifiedHead, error) {
		providerCalled = true
		fixture.providerNow = fixture.providerNow.Add(time.Millisecond)
		return gitprojection.VerifiedHead{BindingID: binding.ID, Repository: binding.Repository, TargetRef: binding.TargetRef,
			Commit: fixture.baseHead, Source: source, ProviderRequest: "approval-gate-provider-read", ObservedAt: fixture.providerNow}, nil
	})
	_, err := fixture.writer(provider).CommitClaim(t.Context(), fixture.claim.Lease)
	if !errors.Is(err, argo.ErrInvalid) || !providerCalled || runDesiredStateGit(t, fixture.remote, "rev-parse", fixture.target.PlatformBinding.TargetRef) != fixture.baseHead {
		t.Fatalf("unapproved projection reached a Git mutation: providerRead=%v err=%v", providerCalled, err)
	}
}

func TestDesiredStateRuntimeUsesHeartbeatedLeaseForDurableRetry(t *testing.T) {
	fixture := newDesiredStateWriterFixture(t)
	if _, err := fixture.commands.RetryDesiredState(t.Context(), fixture.claim.Lease,
		argo.DesiredStateRetry{FailureCode: "test-reset", NextAttemptAt: fixture.now}, fixture.now); err != nil {
		t.Fatal(err)
	}
	store := &observingDesiredStateStore{MemoryDesiredStateStore: fixture.commands, heartbeat: make(chan struct{}, 1)}
	providerFailure := errors.New("provider temporarily unavailable")
	provider := desiredStateVerifierFunc(func(ctx context.Context, _ gitprojection.Binding, _ gitprojection.ObservationSource) (gitprojection.VerifiedHead, error) {
		select {
		case <-store.heartbeat:
			return gitprojection.VerifiedHead{}, providerFailure
		case <-ctx.Done():
			return gitprojection.VerifiedHead{}, ctx.Err()
		}
	})
	writer := fixture.writer(provider)
	writer.Store = store
	var clockTick atomic.Int64
	clock := func() time.Time { return fixture.now.Add(time.Duration(clockTick.Add(1)) * time.Millisecond) }
	writer.Now = clock
	writer.LeaseDuration = 30 * time.Second
	writer.HeartbeatInterval = 5 * time.Millisecond
	now := fixture.now
	worker := &argo.DesiredStateRuntimeWorker{
		Store: store, Writer: writer, LeaseDuration: 30 * time.Second, PollInterval: 250 * time.Millisecond,
		Observation: argo.DesiredStateRuntimeWorkerObservation{WorkerID: "argo-runtime-worker-a", DesiredStateRuntimeIdentity: fixture.identity, StartedAt: now, ObservedAt: now},
		Now:         clock,
	}
	processed, err := worker.ProcessOne(t.Context())
	if err != nil || !processed {
		t.Fatalf("durably requeued provider error escaped worker: processed=%v err=%v", processed, err)
	}
	current, readErr := fixture.commands.DesiredStateCommand(t.Context(), fixture.command.ID)
	if readErr != nil || current.State != argo.DesiredStatePending || current.Lease != nil || current.ConsecutiveFailures != 2 || current.LastFailureCode != "git-write-transient" {
		t.Fatalf("provider error was not durably requeued: %#v err=%v", current, readErr)
	}
}

func TestDesiredStateRuntimeTerminatesOnClaimInfrastructureFailure(t *testing.T) {
	fixture := newDesiredStateWriterFixture(t)
	infrastructureFailure := errors.New("database unavailable")
	store := &failingDesiredStateClaimStore{MemoryDesiredStateStore: fixture.commands, err: infrastructureFailure}
	writer := fixture.writer(fixture.provider(t, nil))
	writer.Store = store
	now := fixture.now
	worker := &argo.DesiredStateRuntimeWorker{
		Store: store, Writer: writer, PollInterval: 250 * time.Millisecond, Now: func() time.Time { return now },
		Observation: argo.DesiredStateRuntimeWorkerObservation{WorkerID: "argo-runtime-worker-b", DesiredStateRuntimeIdentity: fixture.identity, StartedAt: now, ObservedAt: now},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := worker.Run(ctx); !errors.Is(err, infrastructureFailure) {
		t.Fatalf("claim-loop failure left a falsely healthy worker running: %v", err)
	}
}
