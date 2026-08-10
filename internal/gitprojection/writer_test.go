package gitprojection_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/gitpublication"
)

type pullRequestProviderStub struct {
	observation gitpublication.PullRequestObservation
	created     int
	gets        int
}

type protectionVerifierStub struct{ err error }

func (v protectionVerifierStub) VerifyRepositoryProtection(_ context.Context, binding gitprojection.Binding, head gitprojection.VerifiedHead, now time.Time) (gitprojection.RepositoryProtectionObservation, error) {
	if v.err != nil {
		return gitprojection.RepositoryProtectionObservation{}, v.err
	}
	return gitprojection.RepositoryProtectionObservation{BindingID: binding.ID, TargetRef: binding.TargetRef, Head: head.Commit,
		PolicyDigest: "sha256:" + strings.Repeat("9", 64), ObservedAt: now}, nil
}

func TestProjectionWriterRejectsPathMutatedDescendantBeforeCandidatePush(t *testing.T) {
	fixture := seedRepository(t, false)
	now := time.Now().UTC()
	binding := readyFixtureBinding(fixture, now)
	projectionStore := gitprojection.NewMemoryStore()
	if err := projectionStore.PutBinding(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	command := newCreateCommand(t, binding, operationID, "66666666-6666-4666-8666-666666666666", fixture.config, now)
	command.PublicationMode = gitprojection.PublicationPullRequest
	if err := projectionStore.PutWriteCommand(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	changedPath := filepath.Join(fixture.seed, filepath.FromSlash(command.Path))
	if err := os.MkdirAll(filepath.Dir(changedPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(changedPath, []byte("competing protected content\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	runGit(t, fixture.seed, "add", "--all")
	runGit(t, fixture.seed, "commit", "-m", "competing protected path")
	descendant := runGit(t, fixture.seed, "rev-parse", "HEAD")
	runGit(t, fixture.seed, "push", "origin", "HEAD:"+binding.TargetRef)

	publications := gitpublication.NewMemoryStore()
	repository := gitpublication.Repository{InstallationID: binding.Repository.InstallationID, ID: binding.Repository.RepositoryID,
		Owner: binding.Repository.Owner, Name: binding.Repository.Name}
	publication, err := gitpublication.NewPublication(operationID, binding.ID, repository, binding.TargetRef, command.Plan.BaseRevision, now)
	if err != nil || publications.CreatePublication(t.Context(), publication) != nil {
		t.Fatalf("publication=%#v err=%v", publication, err)
	}
	provider := &pullRequestProviderStub{}
	service := &gitpublication.Service{Store: publications, Provider: provider, Now: func() time.Time { return now.Add(5 * time.Second) }}
	observed := now
	writer := &gitprojection.ProjectionWriter{Store: projectionStore, Commands: projectionStore,
		Provider:     localHeadVerifier(t, fixture.remote, &observed),
		Manager:      &gitprojection.MirrorManager{Root: t.TempDir(), AllowLocalTests: true, LocalRemote: fixture.remote},
		Publications: publications, PullRequests: service, Protection: protectionVerifierStub{}, Owner: "writer-test-owner", Now: func() time.Time { return observed.Add(time.Second) }}

	if _, err = writer.PublishOperation(t.Context(), operationID); !errors.Is(err, gitprojection.ErrConflict) {
		t.Fatalf("path-mutated descendant accepted: %v", err)
	}
	stored, err := publications.Publication(t.Context(), operationID)
	if err != nil || stored.State != gitpublication.StatePendingCandidate || stored.WriteBaseRevision != "" {
		t.Fatalf("publication advanced before path proof: %#v err=%v", stored, err)
	}
	if refs := runGit(t, fixture.remote, "for-each-ref", "--format=%(refname)", "refs/heads/kuberploy/operations"); strings.Contains(refs, publication.CandidateRef) {
		t.Fatal("candidate ref was pushed despite changed protected path")
	}
	if target := runGit(t, fixture.remote, "rev-parse", binding.TargetRef); target != descendant {
		t.Fatalf("target changed unexpectedly: %s", target)
	}
}

func (p *pullRequestProviderStub) CreatePullRequest(_ context.Context, request gitpublication.CreatePullRequestRequest) (gitpublication.PullRequestObservation, error) {
	p.created++
	p.observation.HeadRevision = request.HeadSHA
	return p.observation, nil
}
func (p *pullRequestProviderStub) FindPullRequest(_ context.Context, _ gitpublication.FindPullRequestRequest) (gitpublication.PullRequestObservation, bool, error) {
	return gitpublication.PullRequestObservation{}, false, nil
}
func (p *pullRequestProviderStub) GetPullRequest(_ context.Context, _ gitpublication.GetPullRequestRequest) (gitpublication.PullRequestObservation, error) {
	p.gets++
	return p.observation, nil
}
func (p *pullRequestProviderStub) ResolveTargetHead(_ context.Context, repository gitpublication.Repository, targetRef string) (gitpublication.TargetHeadObservation, error) {
	return gitpublication.TargetHeadObservation{Repository: repository, TargetRef: targetRef, Revision: strings.Repeat("f", 40), ObservedAt: p.observation.ObservedAt}, nil
}
func (p *pullRequestProviderStub) IsAncestor(context.Context, gitpublication.Repository, string, string) (bool, error) {
	return true, nil
}

func readyFixtureBinding(fixture repositoryFixture, now time.Time) gitprojection.Binding {
	binding := fixture.binding
	binding.State, binding.IndexedRevision, binding.ProjectionGeneration = gitprojection.BindingReady, fixture.head, 1
	binding.IndexedAt, binding.TargetHeadObservedAt, binding.UpdatedAt = now, now, now
	return binding
}

func localHeadVerifier(t *testing.T, remote string, observed *time.Time) verifierFunc {
	t.Helper()
	return func(_ context.Context, binding gitprojection.Binding, source gitprojection.ObservationSource) (gitprojection.VerifiedHead, error) {
		*observed = observed.Add(time.Second)
		return gitprojection.VerifiedHead{BindingID: binding.ID, Repository: binding.Repository, TargetRef: binding.TargetRef,
			Commit: runGit(t, remote, "rev-parse", binding.TargetRef), Source: source, ProviderRequest: "writer-provider-read", ObservedAt: *observed}, nil
	}
}

func newCreateCommand(t *testing.T, binding gitprojection.Binding, operation, application string, raw []byte, now time.Time) gitprojection.WriteCommand {
	t.Helper()
	plan := gitprojection.WritePlan{BindingID: binding.ID, ProjectID: binding.ProjectID, EnvironmentID: binding.EnvironmentID,
		ApplicationID: application, BaseRevision: binding.IndexedRevision, Precondition: gitprojection.MutationCreateIfAbsent,
		ChartDigest: "sha256:" + strings.Repeat("e", 64), PolicyVersion: "runtime-policy-v1"}
	command, err := gitprojection.NewWriteCommand(operation, "99999999-9999-4999-8999-999999999999", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		plan, binding, raw, "create application config", now)
	if err != nil {
		t.Fatal(err)
	}
	return command
}

func newVariableCreateCommand(t *testing.T, binding gitprojection.Binding, operation, scope string, raw []byte, now time.Time) gitprojection.WriteCommand {
	t.Helper()
	paths, err := gitprojection.DependencyPaths(binding)
	if err != nil {
		t.Fatal(err)
	}
	variablePath := paths[0]
	if scope == "environment" {
		variablePath = paths[1]
	}
	plan := gitprojection.WritePlan{BindingID: binding.ID, ProjectID: binding.ProjectID, EnvironmentID: binding.EnvironmentID,
		BaseRevision: binding.IndexedRevision, Precondition: gitprojection.MutationCreateIfAbsent, PolicyVersion: binding.ParserVersion,
		VariableScope: scope, VariablePath: variablePath}
	command, err := gitprojection.NewVariableWriteCommand(operation, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", plan, binding, raw,
		"save "+scope+" variables", "sha256:"+strings.Repeat("a", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	return command
}

func TestVariableWriterDurableReservationRecoveryAndStaleConflict(t *testing.T) {
	fixture := seedRepository(t, false)
	dependencyPaths, err := gitprojection.DependencyPaths(fixture.binding)
	if err != nil {
		t.Fatal(err)
	}
	for _, dependencyPath := range dependencyPaths {
		if err = os.Remove(filepath.Join(fixture.seed, filepath.FromSlash(dependencyPath))); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, fixture.seed, "add", "--all")
	runGit(t, fixture.seed, "commit", "-m", "start without optional variables")
	fixture.head = runGit(t, fixture.seed, "rev-parse", "HEAD")
	runGit(t, fixture.seed, "push", "origin", "HEAD:"+fixture.binding.TargetRef)
	fixture.binding.TargetHeadRevision = fixture.head
	now := time.Now().UTC()
	binding := readyFixtureBinding(fixture, now)
	store := gitprojection.NewMemoryStore()
	if err := store.PutBinding(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	projectOperation := operationID
	projectCommand := newVariableCreateCommand(t, binding, projectOperation, "project", []byte("variables:\n  REGION: ap-southeast-1\n"), now)
	environmentOperation := "66666666-6666-4666-8666-666666666666"
	environmentCommand := newVariableCreateCommand(t, binding, environmentOperation, "environment", []byte("variables:\n  LOG_LEVEL: info\n"), now)
	if err := store.PutWriteCommand(t.Context(), projectCommand); err != nil {
		t.Fatal(err)
	}
	staleStore := gitprojection.NewMemoryStore()
	if err := staleStore.PutBinding(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	if err := staleStore.PutWriteCommand(t.Context(), environmentCommand); err != nil {
		t.Fatal(err)
	}
	expiredOperation := "77777777-7777-4777-8777-777777777777"
	expiredCommand := newVariableCreateCommand(t, binding, expiredOperation, "environment", []byte("variables:\n  LOG_LEVEL: debug\n"), now)
	expiredStore := gitprojection.NewMemoryStore()
	if err := expiredStore.PutBinding(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	if err := expiredStore.PutWriteCommand(t.Context(), expiredCommand); err != nil {
		t.Fatal(err)
	}

	// The reservation is the durable pre-push receipt. Simulate a process that
	// acquired it, pushed the exact operation commit, then crashed before either
	// the reservation or immutable command was finalized.
	leaseDuration := 30 * time.Second
	leaseUntil := now.Add(leaseDuration)
	reservation := gitprojection.PathReservation{BindingID: binding.ID, TargetRef: binding.TargetRef, Path: projectCommand.Path,
		OperationID: projectOperation, Owner: "writer-test-owner", BaseRevision: fixture.head,
		State: gitprojection.ReservationCandidate, LeaseUntil: &leaseUntil, CreatedAt: now, UpdatedAt: now}
	if _, replay, err := store.AcquirePath(t.Context(), reservation, now, leaseDuration); err != nil || replay {
		t.Fatalf("reservation replay=%v err=%v", replay, err)
	}
	expiredUntil := now.Add(leaseDuration)
	expiredReservation := gitprojection.PathReservation{BindingID: binding.ID, TargetRef: binding.TargetRef, Path: expiredCommand.Path,
		OperationID: expiredOperation, Owner: "writer-test-owner", BaseRevision: fixture.head, State: gitprojection.ReservationCandidate,
		LeaseUntil: &expiredUntil, CreatedAt: now, UpdatedAt: now}
	if _, _, err = expiredStore.AcquirePath(t.Context(), expiredReservation, now, leaseDuration); err != nil {
		t.Fatal(err)
	}
	manager := &gitprojection.MirrorManager{Root: t.TempDir(), AllowLocalTests: true, LocalRemote: fixture.remote}
	prepared, err := manager.Prepare(t.Context(), binding, verified(binding, fixture.head, "pre-crash-provider", now), projectOperation)
	if err != nil {
		t.Fatal(err)
	}
	pushed, err := prepared.Commit(t.Context(), projectCommand.Mutation())
	if err != nil {
		t.Fatal(err)
	}
	if err = prepared.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stored, readErr := store.WriteCommand(t.Context(), projectOperation); readErr != nil || stored.State != gitprojection.WriteCommandPending {
		t.Fatalf("command finalized before recovery: %#v err=%v", stored, readErr)
	}

	// A normal unrelated descendant after the operation commit must not hide
	// that receipt. Recovery is bounded by the operation trailer and exact blob.
	runGit(t, fixture.seed, "fetch", "origin", binding.TargetRef)
	runGit(t, fixture.seed, "reset", "--hard", "FETCH_HEAD")
	runGit(t, fixture.seed, "commit", "--allow-empty", "-m", "unrelated descendant")
	descendant := runGit(t, fixture.seed, "rev-parse", "HEAD")
	runGit(t, fixture.seed, "push", "origin", "HEAD:"+binding.TargetRef)
	observed := leaseUntil.Add(time.Second)
	writer := &gitprojection.ProjectionWriter{Store: store, Commands: store, Provider: localHeadVerifier(t, fixture.remote, &observed),
		Manager: manager, Owner: "writer-test-owner", LeaseDuration: leaseDuration, Now: func() time.Time { return observed.Add(time.Second) }}
	recovered, err := writer.CommitOperation(t.Context(), projectOperation)
	if err != nil || recovered != pushed {
		t.Fatalf("recovered=%q pushed=%q descendant=%q err=%v", recovered, pushed, descendant, err)
	}
	stored, err := store.WriteCommand(t.Context(), projectOperation)
	if err != nil || stored.State != gitprojection.WriteCommandGitCommitted || stored.CommittedRevision != pushed {
		t.Fatalf("recovered command=%#v err=%v", stored, err)
	}

	// A separately accepted old-head mutation has no reservation/commit receipt.
	// It must return an explicit conflict and leave the authoritative ref intact.
	staleWriter := &gitprojection.ProjectionWriter{Store: staleStore, Commands: staleStore, Provider: localHeadVerifier(t, fixture.remote, &observed),
		Manager: manager, Owner: "writer-test-owner", LeaseDuration: leaseDuration, Now: func() time.Time { return observed.Add(time.Second) }}
	if _, err = staleWriter.CommitOperation(t.Context(), environmentOperation); !errors.Is(err, gitprojection.ErrConflict) {
		t.Fatalf("stale unrelated variable mutation error=%v", err)
	}
	if target := runGit(t, fixture.remote, "rev-parse", binding.TargetRef); target != descendant {
		t.Fatalf("stale variable command mutated target: got %s want %s", target, descendant)
	}

	// An expired candidate reservation with no matching pushed operation is
	// repaired from authoritative history, never stolen implicitly.
	observed = expiredUntil.Add(time.Second)
	expiredWriter := &gitprojection.ProjectionWriter{Store: expiredStore, Commands: expiredStore, Provider: localHeadVerifier(t, fixture.remote, &observed),
		Manager: manager, Owner: "writer-test-owner", LeaseDuration: leaseDuration, Now: func() time.Time { return observed.Add(time.Second) }}
	if _, err = expiredWriter.CommitOperation(t.Context(), expiredOperation); !errors.Is(err, gitprojection.ErrConflict) {
		t.Fatalf("expired reservation without commit error=%v", err)
	}
	if _, err = expiredStore.PathReservation(t.Context(), binding.ID, binding.TargetRef, expiredCommand.Path); !errors.Is(err, gitprojection.ErrNotFound) {
		t.Fatalf("expired reservation was not repaired: %v", err)
	}
}

func TestProjectionWriterReservesPushesAndFinalizesVerifiedHead(t *testing.T) {
	fixture := seedRepository(t, false)
	now := time.Now().UTC()
	binding := readyFixtureBinding(fixture, now)
	store := gitprojection.NewMemoryStore()
	if err := store.PutBinding(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	command := newCreateCommand(t, binding, operationID, "66666666-6666-4666-8666-666666666666", fixture.config, now)
	if err := store.PutWriteCommand(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	observed := now
	manager := &gitprojection.MirrorManager{Root: t.TempDir(), AllowLocalTests: true, LocalRemote: fixture.remote}
	writer := &gitprojection.ProjectionWriter{Store: store, Commands: store, Provider: localHeadVerifier(t, fixture.remote, &observed),
		Manager: manager, Owner: "writer-test-owner", Now: func() time.Time { return observed.Add(time.Second) }}
	revision, err := writer.CommitOperation(t.Context(), operationID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.WriteCommand(t.Context(), operationID)
	if err != nil || stored.State != gitprojection.WriteCommandGitCommitted || stored.CommittedRevision != revision {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
	updated, err := store.Binding(t.Context(), binding.ID)
	if err != nil || updated.TargetHeadRevision != revision || updated.State != gitprojection.BindingIndexing {
		t.Fatalf("binding=%#v err=%v", updated, err)
	}
	reservation, err := store.PathReservation(t.Context(), binding.ID, binding.TargetRef, command.Path)
	if err != nil || reservation.State != gitprojection.ReservationCommittedPendingIndex || reservation.CommittedRevision != revision {
		t.Fatalf("reservation=%#v err=%v", reservation, err)
	}
	if replay, err := writer.CommitOperation(t.Context(), operationID); err != nil || replay != revision {
		t.Fatalf("replay=%q err=%v", replay, err)
	}
}

func TestProjectionWriterPublishesProtectedCandidateWithoutAdvancingTarget(t *testing.T) {
	fixture := seedRepository(t, false)
	now := time.Now().UTC()
	binding := readyFixtureBinding(fixture, now)
	projectionStore := gitprojection.NewMemoryStore()
	if err := projectionStore.PutBinding(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	command := newCreateCommand(t, binding, operationID, "66666666-6666-4666-8666-666666666666", fixture.config, now)
	command.PublicationMode = gitprojection.PublicationPullRequest
	if err := projectionStore.PutWriteCommand(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	publications := gitpublication.NewMemoryStore()
	repository := gitpublication.Repository{InstallationID: binding.Repository.InstallationID, ID: binding.Repository.RepositoryID,
		Owner: binding.Repository.Owner, Name: binding.Repository.Name}
	publication, err := gitpublication.NewPublication(operationID, binding.ID, repository, binding.TargetRef, command.Plan.BaseRevision, now)
	if err != nil || publications.CreatePublication(t.Context(), publication) != nil {
		t.Fatalf("publication=%#v err=%v", publication, err)
	}
	provider := &pullRequestProviderStub{}
	serviceNow := now.Add(5 * time.Second)
	service := &gitpublication.Service{Store: publications, Provider: provider, Now: func() time.Time { return serviceNow }}
	observed := now
	manager := &gitprojection.MirrorManager{Root: t.TempDir(), AllowLocalTests: true, LocalRemote: fixture.remote}
	writer := &gitprojection.ProjectionWriter{Store: projectionStore, Commands: projectionStore,
		Provider: localHeadVerifier(t, fixture.remote, &observed), Manager: manager, Publications: publications, PullRequests: service,
		Protection: protectionVerifierStub{}, Owner: "writer-test-owner", Now: func() time.Time { return observed.Add(time.Second) }}
	provider.observation = gitpublication.PullRequestObservation{Repository: repository, Number: 7,
		URL: "https://github.com/" + repository.Owner + "/" + repository.Name + "/pull/7", TargetRef: binding.TargetRef,
		HeadRef: publication.CandidateRef, State: gitpublication.PullRequestOpen, ObservedAt: now.Add(4 * time.Second)}
	writer.Protection = protectionVerifierStub{err: gitprojection.ErrProtectionUnavailable}
	if _, protectionErr := writer.PublishOperation(t.Context(), operationID); protectionErr == nil {
		t.Fatal("protected publication proceeded without a fresh branch policy attestation")
	}
	blocked, err := publications.Publication(t.Context(), operationID)
	if err != nil || blocked.State != gitpublication.StatePendingCandidate || blocked.WriteBaseRevision != "" || provider.created != 0 {
		t.Fatalf("unattested publication changed durable/provider state: %#v created=%d err=%v", blocked, provider.created, err)
	}
	writer.Protection = protectionVerifierStub{}
	// Another protected operation may have advanced an unrelated path after
	// this command was accepted from fixture.head. Publication must claim that
	// descendant as its durable write base instead of permanently conflicting.
	runGit(t, fixture.seed, "commit", "--allow-empty", "-m", "independent protected operation")
	descendant := runGit(t, fixture.seed, "rev-parse", "HEAD")
	runGit(t, fixture.seed, "push", "origin", "HEAD:"+binding.TargetRef)

	result, err := writer.PublishOperation(t.Context(), operationID)
	if err != nil || result.Validate() != nil || result.Mode != gitprojection.PublicationPullRequest || result.Publication.PullRequestNumber != 7 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	provider.observation.HeadRevision = result.Publication.CandidateRevision
	if result.Publication.WriteBaseRevision != descendant {
		t.Fatalf("write base=%s want descendant %s", result.Publication.WriteBaseRevision, descendant)
	}
	if parent := runGit(t, fixture.remote, "rev-parse", result.Publication.CandidateRevision+"^"); parent != descendant {
		t.Fatalf("candidate parent=%s want write base %s", parent, descendant)
	}
	if target := runGit(t, fixture.remote, "rev-parse", binding.TargetRef); target != descendant {
		t.Fatalf("protected publication advanced target to %s", target)
	}
	if candidate := runGit(t, fixture.remote, "rev-parse", publication.CandidateRef); candidate != result.Publication.CandidateRevision {
		t.Fatalf("candidate=%s publication=%s", candidate, result.Publication.CandidateRevision)
	}
	stored, err := projectionStore.WriteCommand(t.Context(), operationID)
	if err != nil || stored.State != gitprojection.WriteCommandPending || stored.CommittedRevision != "" {
		t.Fatalf("protected command became target-committed: %#v err=%v", stored, err)
	}
	if _, err = projectionStore.PathReservation(t.Context(), binding.ID, binding.TargetRef, command.Path); !errors.Is(err, gitprojection.ErrNotFound) {
		t.Fatalf("protected candidate fenced the target path: %v", err)
	}

	serviceNow = now.Add(6 * time.Second)
	replay, err := writer.PublishOperation(t.Context(), operationID)
	if err != nil || replay.Publication.CandidateRevision != result.Publication.CandidateRevision || provider.created != 1 || provider.gets != 1 {
		t.Fatalf("replay=%#v created=%d gets=%d err=%v", replay, provider.created, provider.gets, err)
	}
	if target := runGit(t, fixture.remote, "rev-parse", binding.TargetRef); target != descendant {
		t.Fatalf("protected replay advanced target to %s", target)
	}
}

func TestProjectionWriterRecoversPushBeforeDatabaseFinalization(t *testing.T) {
	fixture := seedRepository(t, false)
	now := time.Now().UTC()
	binding := readyFixtureBinding(fixture, now)
	store := gitprojection.NewMemoryStore()
	if err := store.PutBinding(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	command := newCreateCommand(t, binding, operationID, "66666666-6666-4666-8666-666666666666", fixture.config, now)
	if err := store.PutWriteCommand(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	leaseDuration := 90 * time.Second
	until := now.Add(leaseDuration)
	reservation := gitprojection.PathReservation{BindingID: binding.ID, TargetRef: binding.TargetRef, Path: command.Path, OperationID: operationID,
		Owner: "writer-test-owner", BaseRevision: fixture.head, State: gitprojection.ReservationCandidate, LeaseUntil: &until, CreatedAt: now, UpdatedAt: now}
	if _, _, err := store.AcquirePath(t.Context(), reservation, now, leaseDuration); err != nil {
		t.Fatal(err)
	}
	manager := &gitprojection.MirrorManager{Root: t.TempDir(), AllowLocalTests: true, LocalRemote: fixture.remote}
	prepared, err := manager.Prepare(t.Context(), binding, verified(binding, fixture.head, "pre-crash-provider", now), operationID)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := prepared.Commit(t.Context(), command.Mutation())
	if err != nil {
		t.Fatal(err)
	}
	if err = prepared.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	observed := now
	writer := &gitprojection.ProjectionWriter{Store: store, Commands: store, Provider: localHeadVerifier(t, fixture.remote, &observed),
		Manager: manager, Owner: "writer-test-owner", Now: func() time.Time { return observed.Add(time.Second) }}
	recovered, err := writer.CommitOperation(t.Context(), operationID)
	if err != nil || recovered != revision {
		t.Fatalf("recovered=%q want=%q err=%v", recovered, revision, err)
	}
	stored, err := store.WriteCommand(t.Context(), operationID)
	if err != nil || stored.State != gitprojection.WriteCommandGitCommitted || stored.CommittedRevision != revision {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
}

func TestProjectionWriterMarksPostPushProviderOutageReconcilePending(t *testing.T) {
	fixture := seedRepository(t, false)
	now := time.Now().UTC()
	binding := readyFixtureBinding(fixture, now)
	store := gitprojection.NewMemoryStore()
	if err := store.PutBinding(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	command := newCreateCommand(t, binding, operationID, "66666666-6666-4666-8666-666666666666", fixture.config, now)
	if err := store.PutWriteCommand(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	observed, calls := now, 0
	providerOutage := errors.New("provider temporarily unavailable")
	provider := verifierFunc(func(_ context.Context, candidate gitprojection.Binding, source gitprojection.ObservationSource) (gitprojection.VerifiedHead, error) {
		calls++
		if calls == 2 {
			// This is the provider verification immediately after the normal
			// fast-forward push has already updated the authoritative remote.
			return gitprojection.VerifiedHead{}, providerOutage
		}
		observed = observed.Add(time.Second)
		return gitprojection.VerifiedHead{BindingID: candidate.ID, Repository: candidate.Repository, TargetRef: candidate.TargetRef,
			Commit: runGit(t, fixture.remote, "rev-parse", candidate.TargetRef), Source: source,
			ProviderRequest: "writer-provider-read", ObservedAt: observed}, nil
	})
	manager := &gitprojection.MirrorManager{Root: t.TempDir(), AllowLocalTests: true, LocalRemote: fixture.remote}
	writer := &gitprojection.ProjectionWriter{Store: store, Commands: store, Provider: provider,
		Manager: manager, Owner: "writer-test-owner", Now: func() time.Time { return observed.Add(time.Second) }}
	if _, err := writer.CommitOperation(t.Context(), operationID); err == nil || !errors.Is(err, providerOutage) {
		t.Fatalf("post-push outage error=%v", err)
	} else {
		var pending interface {
			ReconcilePending() (string, string)
		}
		if !errors.As(err, &pending) {
			t.Fatalf("post-push outage was not reconcile-pending: %T %v", err, err)
		}
		code, detail := pending.ReconcilePending()
		if code != "GitPostPushVerificationPending" || detail == "" {
			t.Fatalf("pending code=%q detail=%q", code, detail)
		}
	}
	pushed := runGit(t, fixture.remote, "rev-parse", binding.TargetRef)
	if pushed == fixture.head {
		t.Fatal("provider outage fixture did not occur after a successful push")
	}
	stored, err := store.WriteCommand(t.Context(), operationID)
	if err != nil || stored.State != gitprojection.WriteCommandPending {
		t.Fatalf("command prematurely finalized: %#v err=%v", stored, err)
	}
	recovered, err := writer.CommitOperation(t.Context(), operationID)
	if err != nil || recovered != pushed {
		t.Fatalf("recovered=%q pushed=%q err=%v", recovered, pushed, err)
	}
	stored, err = store.WriteCommand(t.Context(), operationID)
	if err != nil || stored.State != gitprojection.WriteCommandGitCommitted || stored.CommittedRevision != pushed {
		t.Fatalf("recovered command=%#v err=%v", stored, err)
	}
}
