package gitprojection

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRuntimeReadinessIsExactFreshAndEpochFenced(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now().UTC()
	identity := RuntimeIdentity{ContractVersion: RuntimeContract, ConfigDigest: "sha256:" + strings.Repeat("a", 64), GitHubAppID: 123}
	observation := RuntimeWorkerObservation{WorkerID: "worker-git-runtime-1", RuntimeIdentity: identity, StartedAt: now, ObservedAt: now}
	first, err := store.AcquireRuntimeReadiness(context.Background(), observation, RuntimeReadinessLease)
	if err != nil || first.Epoch != 1 {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	second, err := store.AcquireRuntimeReadiness(context.Background(), observation, RuntimeReadinessLease)
	if err != nil || second.Epoch != 2 {
		t.Fatalf("second=%#v err=%v", second, err)
	}
	if _, err = store.HeartbeatRuntimeReadiness(context.Background(), first, now.Add(time.Second), RuntimeReadinessLease); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale epoch heartbeat=%v", err)
	}
	second, err = store.HeartbeatRuntimeReadiness(context.Background(), second, now.Add(time.Second), RuntimeReadinessLease)
	if err != nil {
		t.Fatal(err)
	}
	probe := &RuntimeReadinessProbe{Store: store, Identity: identity, Now: func() time.Time { return now.Add(2 * time.Second) }}
	if err = probe.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	probe.Identity.ConfigDigest = "sha256:" + strings.Repeat("b", 64)
	if err = probe.Probe(context.Background()); !errors.Is(err, ErrRuntimeNotReady) {
		t.Fatalf("mismatched runtime ready: %v", err)
	}
	probe.Identity = identity
	probe.Now = func() time.Time { return second.ObservedAt.Add(RuntimeHeartbeatMaxAge + time.Second) }
	if err = probe.Probe(context.Background()); !errors.Is(err, ErrRuntimeNotReady) {
		t.Fatalf("stale runtime ready: %v", err)
	}
}

func TestWriteCommandCommitAndExactIndexReceiptsAreMonotonic(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now().UTC()
	binding, err := NewGitHubEnvironmentBinding("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222",
		"33333333-3333-4333-8333-333333333333", RepositoryIdentity{Provider: "github", InstallationID: 7, RepositoryID: 8, Owner: "kuberploy", Name: "environment"}, "refs/heads/main", now)
	if err != nil {
		t.Fatal(err)
	}
	baseRevision := strings.Repeat("a", 40)
	binding.TargetHeadRevision, binding.IndexedRevision, binding.ProjectionGeneration = baseRevision, baseRevision, 1
	binding.TargetHeadObservedAt, binding.IndexedAt, binding.State, binding.UpdatedAt = now, now, BindingReady, now
	if err = store.PutBinding(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	plan := WritePlan{BindingID: binding.ID, ProjectID: binding.ProjectID, EnvironmentID: binding.EnvironmentID,
		ApplicationID: "44444444-4444-4444-8444-444444444444", BaseRevision: baseRevision, Precondition: MutationCreateIfAbsent,
		ChartDigest: "sha256:" + strings.Repeat("b", 64), PolicyVersion: "runtime-policy-v1"}
	command, err := NewWriteCommand("55555555-5555-4555-8555-555555555555", "66666666-6666-4666-8666-666666666666",
		"77777777-7777-4777-8777-777777777777", plan, binding, []byte("apiVersion: config.kuberploy.io/v1alpha1\n"), "create app config", now)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.PutWriteCommand(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("c", 40)
	command, err = store.MarkWriteCommandCommitted(context.Background(), command.OperationID, commit, now.Add(time.Second))
	if err != nil || command.State != WriteCommandGitCommitted {
		t.Fatalf("command=%#v err=%v", command, err)
	}
	if _, err = store.MarkWriteCommandCommitted(context.Background(), command.OperationID, strings.Repeat("d", 40), now.Add(2*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("commit result changed: %v", err)
	}

	observed := VerifiedHead{BindingID: binding.ID, Repository: binding.Repository, TargetRef: binding.TargetRef, Commit: commit,
		Source: ObservationWrite, ProviderRequest: "write-receipt", ObservedAt: now.Add(2 * time.Second)}
	if _, _, err = store.RecordVerifiedHead(context.Background(), observed); err != nil {
		t.Fatal(err)
	}
	work, err := store.ClaimReconciliation(context.Background(), "command-index-owner", now.Add(3*time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := store.BeginGeneration(context.Background(), work.Lease, commit, binding.ParserVersion, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.ActivateGeneration(context.Background(), work.Lease, generation, SchemaOnlyAppConfigPolicyValidator{}, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	command, err = store.WriteCommand(context.Background(), command.OperationID)
	if err != nil || command.State != WriteCommandIndexed || command.IndexedGeneration != generation.Number || command.IndexedAt == nil {
		t.Fatalf("indexed command=%#v err=%v", command, err)
	}
}

func TestFinalizeVerifiedPathConvergesWhenHeadWasIndexedBeforeReceipt(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now().UTC().Truncate(time.Microsecond)
	binding := coordinatorBinding(t, now)
	baseRevision := strings.Repeat("a", 40)
	binding.TargetHeadRevision, binding.IndexedRevision, binding.ProjectionGeneration = baseRevision, baseRevision, 1
	binding.TargetHeadObservedAt, binding.IndexedAt, binding.State, binding.UpdatedAt = now, now, BindingReady, now
	if err := store.PutBinding(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	const applicationID = "44444444-4444-4444-8444-444444444444"
	plan := WritePlan{BindingID: binding.ID, ProjectID: binding.ProjectID, EnvironmentID: binding.EnvironmentID,
		ApplicationID: applicationID, BaseRevision: baseRevision, Precondition: MutationCreateIfAbsent,
		ChartDigest: "sha256:" + strings.Repeat("b", 64), PolicyVersion: "runtime-policy-v1"}
	command, err := NewWriteCommand("55555555-5555-4555-8555-555555555555", "66666666-6666-4666-8666-666666666666",
		"77777777-7777-4777-8777-777777777777", plan, binding, []byte("kind: AppConfig\n"), "create app config", now)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.PutWriteCommand(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	leaseStart := now.Add(time.Second)
	leaseDuration := 30 * time.Second
	leaseUntil := leaseStart.Add(leaseDuration)
	reservation := PathReservation{BindingID: binding.ID, TargetRef: binding.TargetRef, Path: command.Path, OperationID: command.OperationID,
		Owner: "projection-writer", BaseRevision: baseRevision, State: ReservationCandidate, LeaseUntil: &leaseUntil,
		CreatedAt: leaseStart, UpdatedAt: leaseStart}
	if _, _, err = store.AcquirePath(t.Context(), reservation, leaseStart, leaseDuration); err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("c", 40)
	observed := VerifiedHead{BindingID: binding.ID, Repository: binding.Repository, TargetRef: binding.TargetRef, Commit: commit,
		Source: ObservationPoll, ProviderRequest: "coordinator-before-writer-finalize", ObservedAt: now.Add(2 * time.Second)}
	if _, _, err = store.RecordVerifiedHead(t.Context(), observed); err != nil {
		t.Fatal(err)
	}
	work, err := store.ClaimReconciliation(t.Context(), "projection-indexer", now.Add(3*time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := store.BeginGeneration(t.Context(), work.Lease, commit, binding.ParserVersion, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.ActivateGeneration(t.Context(), work.Lease, generation, SchemaOnlyAppConfigPolicyValidator{}, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	metadataChangedAt := now.Add(5 * time.Second)
	if err = store.SetBindingState(t.Context(), binding.ID, commit, BindingIndexing, metadataChangedAt); err != nil {
		t.Fatal(err)
	}
	writeHead := observed
	writeHead.Source, writeHead.ProviderRequest, writeHead.ObservedAt = ObservationWrite, "writer-finalize-after-index", now.Add(4500*time.Millisecond)
	if _, err = store.FinalizeVerifiedPath(t.Context(), binding.ID, binding.TargetRef, command.Path, command.OperationID, commit, writeHead, now.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = store.PathReservation(t.Context(), binding.ID, binding.TargetRef, command.Path); !errors.Is(err, ErrNotFound) {
		t.Fatalf("indexed-before-finalize reservation remains: %v", err)
	}
	stored, err := store.WriteCommand(t.Context(), command.OperationID)
	if err != nil || stored.State != WriteCommandIndexed || stored.CommittedRevision != commit || stored.IndexedGeneration != generation.Number {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
	current, err := store.Binding(t.Context(), binding.ID)
	if err != nil || current.State != BindingIndexing || !current.UpdatedAt.After(metadataChangedAt) {
		t.Fatalf("metadata revalidation wakeup regressed: %#v err=%v", current, err)
	}
}

func TestActivationConvergesCommittedReservationThroughLaterHead(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now().UTC().Truncate(time.Microsecond)
	binding := coordinatorBinding(t, now)
	baseRevision := strings.Repeat("a", 40)
	binding.TargetHeadRevision, binding.IndexedRevision, binding.ProjectionGeneration = baseRevision, baseRevision, 1
	binding.TargetHeadObservedAt, binding.IndexedAt, binding.State, binding.UpdatedAt = now, now, BindingReady, now
	if err := store.PutBinding(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	plan := WritePlan{BindingID: binding.ID, ProjectID: binding.ProjectID, EnvironmentID: binding.EnvironmentID,
		ApplicationID: "44444444-4444-4444-8444-444444444444", BaseRevision: baseRevision, Precondition: MutationCreateIfAbsent,
		ChartDigest: "sha256:" + strings.Repeat("b", 64), PolicyVersion: "runtime-policy-v1"}
	command, err := NewWriteCommand("55555555-5555-4555-8555-555555555555", "66666666-6666-4666-8666-666666666666",
		"77777777-7777-4777-8777-777777777777", plan, binding, []byte("kind: AppConfig\n"), "create app config", now)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.PutWriteCommand(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	leaseStart, leaseDuration := now.Add(time.Second), 30*time.Second
	leaseUntil := leaseStart.Add(leaseDuration)
	reservation := PathReservation{BindingID: binding.ID, TargetRef: binding.TargetRef, Path: command.Path, OperationID: command.OperationID,
		Owner: "projection-writer", BaseRevision: baseRevision, State: ReservationCandidate, LeaseUntil: &leaseUntil,
		CreatedAt: leaseStart, UpdatedAt: leaseStart}
	if _, _, err = store.AcquirePath(t.Context(), reservation, leaseStart, leaseDuration); err != nil {
		t.Fatal(err)
	}
	operationCommit := strings.Repeat("c", 40)
	operationHead := VerifiedHead{BindingID: binding.ID, Repository: binding.Repository, TargetRef: binding.TargetRef, Commit: operationCommit,
		Source: ObservationWrite, ProviderRequest: "writer-operation-head", ObservedAt: now.Add(2 * time.Second)}
	if _, err = store.FinalizeVerifiedPath(t.Context(), binding.ID, binding.TargetRef, command.Path, command.OperationID, operationCommit, operationHead, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	laterHead := strings.Repeat("d", 40)
	if _, _, err = store.RecordVerifiedHead(t.Context(), VerifiedHead{BindingID: binding.ID, Repository: binding.Repository, TargetRef: binding.TargetRef,
		Commit: laterHead, Source: ObservationPoll, ProviderRequest: "later-fast-forward", ObservedAt: now.Add(4 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	work, err := store.ClaimReconciliation(t.Context(), "projection-indexer", now.Add(5*time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := store.BeginGeneration(t.Context(), work.Lease, laterHead, binding.ParserVersion, now.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.ActivateGeneration(t.Context(), work.Lease, generation, SchemaOnlyAppConfigPolicyValidator{}, now.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = store.PathReservation(t.Context(), binding.ID, binding.TargetRef, command.Path); !errors.Is(err, ErrNotFound) {
		t.Fatalf("descendant activation left reservation: %v", err)
	}
	stored, err := store.WriteCommand(t.Context(), command.OperationID)
	if err != nil || stored.State != WriteCommandIndexed || stored.CommittedRevision != operationCommit || stored.IndexedGeneration != generation.Number {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
}
