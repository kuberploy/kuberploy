package registry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/store"
)

type fakeCleanupCoordinator struct {
	mu                sync.Mutex
	plan              domain.RegistryCleanupPlan
	authorizeMutation func(domain.RegistryCleanupItem) domain.RegistryCleanupItem
	recordErrorAt     int
	records           []domain.RegistryCleanupItemResult
	recordOrdinals    []int
	finished          bool
	succeeded         bool
	failure           string
}

type substitutingCleanupCoordinator struct{ *fakeCleanupCoordinator }

func (s substitutingCleanupCoordinator) Claim(context.Context, string, string, time.Duration) (domain.RegistryCleanupPlan, bool, error) {
	return cloneExecutorPlan(s.plan), false, nil
}

func (f *fakeCleanupCoordinator) Claim(_ context.Context, planID, owner string, _ time.Duration) (domain.RegistryCleanupPlan, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if planID != f.plan.ID || owner == "" {
		return domain.RegistryCleanupPlan{}, false, ErrRegistryExecutionInvalid
	}
	return cloneExecutorPlan(f.plan), false, nil
}

func (f *fakeCleanupCoordinator) Renew(context.Context, string, string, time.Duration) error {
	return nil
}

func (f *fakeCleanupCoordinator) AuthorizeItem(_ context.Context, planID string, ordinal int, _ string) (domain.RegistryCleanupItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if planID != f.plan.ID || ordinal < 0 || ordinal >= len(f.plan.Items) || f.plan.Items[ordinal].State != "planned" {
		return domain.RegistryCleanupItem{}, ErrRegistryExecutionInvalid
	}
	item := f.plan.Items[ordinal]
	item.State = "deleting"
	f.plan.Items[ordinal] = item
	if f.authorizeMutation != nil {
		item = f.authorizeMutation(item)
	}
	return item, nil
}

func (f *fakeCleanupCoordinator) RecordItemResult(_ context.Context, planID string, ordinal int, _ string, result domain.RegistryCleanupItemResult) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if planID != f.plan.ID || ordinal < 0 || ordinal >= len(f.plan.Items) {
		return ErrRegistryExecutionInvalid
	}
	if f.recordErrorAt == ordinal {
		return errors.New("durable result write failed")
	}
	item := f.plan.Items[ordinal]
	if item.State != "deleting" {
		if item.State == result.State && item.ProviderMessage == result.ProviderMessage {
			return nil
		}
		return ErrRegistryExecutionInvalid
	}
	item.State = result.State
	item.ProviderMessage = result.ProviderMessage
	item.UpdatedAt = result.ObservedAt
	f.plan.Items[ordinal] = item
	f.records = append(f.records, result)
	f.recordOrdinals = append(f.recordOrdinals, ordinal)
	return nil
}

func (f *fakeCleanupCoordinator) Finish(_ context.Context, planID, _ string, succeeded bool, failure string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if planID != f.plan.ID {
		return ErrRegistryExecutionInvalid
	}
	if succeeded {
		for _, item := range f.plan.Items {
			if item.Disposition == domain.RegistryCleanupDelete && item.State != "deleted" {
				return fmt.Errorf("unfinished item %d", item.Ordinal)
			}
		}
	}
	f.finished = true
	f.succeeded = succeeded
	f.failure = failure
	return nil
}

func cloneExecutorPlan(plan domain.RegistryCleanupPlan) domain.RegistryCleanupPlan {
	plan.Items = append([]domain.RegistryCleanupItem(nil), plan.Items...)
	return plan
}

type fakeManifestDeleter struct {
	target domain.RegistryTarget
	result ManifestDeleteOutcome
	err    error
	calls  []string
}

func (f *fakeManifestDeleter) ManagedTarget() domain.RegistryTarget { return f.target }

func (f *fakeManifestDeleter) DeleteManifest(_ context.Context, targetID, repository, digest string) (ManifestDeleteResult, error) {
	f.calls = append(f.calls, targetID+"/"+repository+"@"+digest)
	if f.err != nil {
		return ManifestDeleteResult{}, f.err
	}
	return ManifestDeleteResult{TargetID: targetID, Repository: repository, Digest: digest, Outcome: f.result}, nil
}

type fakeMaintenanceAdapter struct {
	session              *fakeMaintenanceSession
	err                  error
	returnSessionOnError bool
	request              MaintenanceAcquireRequest
	log                  *[]string
}

func (f *fakeMaintenanceAdapter) Acquire(_ context.Context, request MaintenanceAcquireRequest) (RegistryMaintenanceSession, error) {
	f.request = request
	if f.log != nil {
		*f.log = append(*f.log, "acquire")
	}
	if f.err != nil {
		if f.returnSessionOnError {
			f.session.acquire = request
			return f.session, f.err
		}
		return nil, f.err
	}
	f.session.acquire = request
	return f.session, nil
}

type fakeMaintenanceSession struct {
	acquire            MaintenanceAcquireRequest
	enteredAt          time.Time
	checkpointRevision string
	enterErr           error
	gcErr              error
	restoreErr         error
	releaseErr         error
	replay             bool
	gcCalls            int
	restoreCalls       int
	releaseCalls       int
	log                *[]string
}

func (f *fakeMaintenanceSession) Enter(context.Context) (MaintenanceReady, error) {
	if f.log != nil {
		*f.log = append(*f.log, "enter")
	}
	if f.enterErr != nil {
		return MaintenanceReady{}, f.enterErr
	}
	return MaintenanceReady{
		TargetID: f.acquire.TargetID, LeaseID: "maintenance-lease-1", ExecutionKey: f.acquire.ExecutionKey,
		Mode: RegistryMaintenanceStopped, Exclusive: true, Tested: true, EnteredAt: f.enteredAt,
	}, nil
}

func (f *fakeMaintenanceSession) GarbageCollect(_ context.Context, request GCSweepRequest) (GCSweepResult, error) {
	if f.log != nil {
		*f.log = append(*f.log, "gc")
	}
	f.gcCalls++
	if f.gcErr != nil {
		return GCSweepResult{}, f.gcErr
	}
	revision := request.Checkpoint.Revision
	startedAt := request.Checkpoint.ObservedAt
	if f.replay {
		revision = f.checkpointRevision
		startedAt = request.Checkpoint.StartedAt.Add(-time.Minute)
	}
	return GCSweepResult{
		TargetID: request.TargetID, ExecutionKey: request.ExecutionKey, CandidateSetDigest: request.CandidateSetDigest,
		CheckpointRevision: revision, ProviderSweepID: "provider-sweep-1", Complete: true, Replay: f.replay,
		StartedAt: startedAt, CompletedAt: startedAt.Add(time.Second),
	}, nil
}

func (f *fakeMaintenanceSession) Restore(context.Context) error {
	if f.log != nil {
		*f.log = append(*f.log, "restore")
	}
	f.restoreCalls++
	return f.restoreErr
}

func (f *fakeMaintenanceSession) Release(context.Context) error {
	if f.log != nil {
		*f.log = append(*f.log, "release")
	}
	f.releaseCalls++
	return f.releaseErr
}

type fakeCheckpointProvider struct {
	coordinator *fakeCleanupCoordinator
	mutate      func(*RegistryReachabilityCheckpoint)
	requests    []ReachabilityCheckpointRequest
	log         *[]string
}

func (f *fakeCheckpointProvider) Capture(_ context.Context, request ReachabilityCheckpointRequest) (RegistryReachabilityCheckpoint, error) {
	if f.log != nil {
		*f.log = append(*f.log, "checkpoint")
	}
	f.requests = append(f.requests, request)
	if f.coordinator != nil {
		f.coordinator.mu.Lock()
		defer f.coordinator.mu.Unlock()
		for _, item := range f.coordinator.plan.Items {
			if item.ResourceKind != "blob" && item.Disposition == domain.RegistryCleanupDelete && item.State != "deleted" {
				return RegistryReachabilityCheckpoint{}, errors.New("manifest was not durably recorded before checkpoint")
			}
		}
	}
	blobs := make([]RegistryBlobReachability, 0, len(request.CandidateDigests))
	for _, digest := range request.CandidateDigests {
		blobs = append(blobs, RegistryBlobReachability{Digest: digest, Present: true, Reachable: false})
	}
	checkpoint := RegistryReachabilityCheckpoint{
		TargetID: request.TargetID, PlanID: request.PlanID, PlanDigest: request.PlanDigest,
		ExecutionKey: request.ExecutionKey, CandidateSetDigest: request.CandidateSetDigest,
		Revision: "checkpoint-1", InventoryRevision: "inventory-1", AuthorityRevision: "authority-1",
		RegistryWide: true, InventoryComplete: true, AuthorityComplete: true, ReachabilityComplete: true,
		StartedAt: request.NotBefore, ObservedAt: request.NotBefore.Add(time.Second), Blobs: blobs,
	}
	if f.mutate != nil {
		f.mutate(&checkpoint)
	}
	checkpoint.GraphDigest = ReachabilityCheckpointDigest(checkpoint)
	return checkpoint, nil
}

func executorTarget() domain.RegistryTarget {
	return domain.RegistryTarget{ID: "registry-1", Name: "managed", Mode: domain.RegistryTargetManaged, Endpoint: "https://registry.internal", RepositoryPrefix: "kuberploy/team-a"}
}

func executorPlan(states ...string) domain.RegistryCleanupPlan {
	digests := []string{
		"sha256:" + strings.Repeat("a", 64),
		"sha256:" + strings.Repeat("b", 64),
		"sha256:" + strings.Repeat("c", 64),
	}
	items := []domain.RegistryCleanupItem{
		{Ordinal: 0, Repository: "kuberploy/team-a/service", ResourceKind: "release-manifest", Digest: digests[0], Disposition: domain.RegistryCleanupDelete, Action: "delete-manifest", EstimatedBytes: 100, State: "planned"},
		{Ordinal: 1, Repository: "*", ResourceKind: "blob", Digest: digests[1], Disposition: domain.RegistryCleanupDelete, Action: "garbage-collect-blob", EstimatedBytes: 200, State: "planned"},
		{Ordinal: 2, Repository: "*", ResourceKind: "blob", Digest: digests[2], Disposition: domain.RegistryCleanupDelete, Action: "garbage-collect-blob", EstimatedBytes: 300, State: "planned"},
	}
	if len(states) > 0 {
		items = items[:len(states)]
		for i, state := range states {
			items[i].State = state
		}
	}
	plan := domain.RegistryCleanupPlan{
		ID: "plan-1", RegistryTargetID: "registry-1", ServiceID: "service-1", State: "executing",
		Policy: domain.ServiceRegistryPolicy{RegistryTargetID: "registry-1", ServiceID: "service-1", Repository: "kuberploy/team-a/service"},
		Items:  items,
	}
	plan.PlanDigest = store.RegistryCleanupPlanDigest(plan)
	return plan
}

func newTestExecutor(t *testing.T, coordinator *fakeCleanupCoordinator, deleter *fakeManifestDeleter, maintenance RegistryMaintenanceAdapter, checkpoints RegistryCheckpointProvider, now time.Time) *CleanupExecutor {
	t.Helper()
	config := DefaultCleanupExecutorConfig()
	config.LeaseDuration = time.Minute
	config.LeaseRenewInterval = time.Second
	executor, err := NewCleanupExecutor(coordinator, deleter, maintenance, checkpoints, config)
	if err != nil {
		t.Fatalf("new cleanup executor: %v", err)
	}
	executor.now = func() time.Time { return now }
	return executor
}

func TestCleanupExecutorRunsManifestDeletesBeforeOneOfflineSweep(t *testing.T) {
	now := time.Date(2026, 8, 9, 3, 0, 0, 0, time.UTC)
	plan := executorPlan("planned", "planned", "planned")
	coordinator := &fakeCleanupCoordinator{plan: plan, recordErrorAt: -1}
	deleter := &fakeManifestDeleter{target: executorTarget(), result: ManifestDeleted}
	log := []string{}
	session := &fakeMaintenanceSession{enteredAt: now.Add(time.Second), log: &log}
	maintenance := &fakeMaintenanceAdapter{session: session, log: &log}
	checkpoints := &fakeCheckpointProvider{coordinator: coordinator, log: &log}
	executor := newTestExecutor(t, coordinator, deleter, maintenance, checkpoints, now)

	if err := executor.Execute(context.Background(), plan.ID, "worker-1"); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(deleter.calls) != 1 {
		t.Fatalf("manifest calls = %#v", deleter.calls)
	}
	if session.gcCalls != 1 || session.restoreCalls != 1 || session.releaseCalls != 1 {
		t.Fatalf("gc=%d restore=%d release=%d", session.gcCalls, session.restoreCalls, session.releaseCalls)
	}
	if got, want := strings.Join(log, ","), "acquire,enter,checkpoint,gc,restore,release"; got != want {
		t.Fatalf("maintenance order = %q, want %q", got, want)
	}
	if !coordinator.finished || !coordinator.succeeded || len(coordinator.recordOrdinals) != 3 {
		t.Fatalf("finish=%t success=%t records=%#v", coordinator.finished, coordinator.succeeded, coordinator.recordOrdinals)
	}
	if len(checkpoints.requests) != 1 || len(checkpoints.requests[0].CandidateDigests) != 2 {
		t.Fatalf("checkpoint requests = %#v", checkpoints.requests)
	}
}

func TestCleanupExecutorRecoversManifestDeletingAfterProviderSuccess(t *testing.T) {
	now := time.Date(2026, 8, 9, 3, 0, 0, 0, time.UTC)
	plan := executorPlan("deleting")
	coordinator := &fakeCleanupCoordinator{plan: plan, recordErrorAt: -1}
	deleter := &fakeManifestDeleter{target: executorTarget(), result: ManifestAlreadyMissing}
	executor := newTestExecutor(t, coordinator, deleter, UnavailableMaintenanceAdapter{}, &fakeCheckpointProvider{}, now)

	if err := executor.Execute(context.Background(), plan.ID, "worker-1"); err != nil {
		t.Fatalf("execute recovery: %v", err)
	}
	if len(deleter.calls) != 1 || len(coordinator.records) != 1 || coordinator.records[0].ProviderMessage != "manifest digest already absent" || !coordinator.succeeded {
		t.Fatalf("calls=%#v records=%#v succeeded=%t", deleter.calls, coordinator.records, coordinator.succeeded)
	}
}

func TestCleanupExecutorReplaysDurableSweepReceiptAfterPartialRecording(t *testing.T) {
	now := time.Date(2026, 8, 9, 3, 0, 0, 0, time.UTC)
	plan := executorPlan("deleted", "deleted", "deleting")
	coordinator := &fakeCleanupCoordinator{plan: plan, recordErrorAt: -1}
	deleter := &fakeManifestDeleter{target: executorTarget(), result: ManifestAlreadyMissing}
	session := &fakeMaintenanceSession{enteredAt: now.Add(time.Second), replay: true, checkpointRevision: "checkpoint-prior"}
	maintenance := &fakeMaintenanceAdapter{session: session}
	checkpoints := &fakeCheckpointProvider{coordinator: coordinator, mutate: func(checkpoint *RegistryReachabilityCheckpoint) {
		for i := range checkpoint.Blobs {
			checkpoint.Blobs[i].Present = false
		}
	}}
	executor := newTestExecutor(t, coordinator, deleter, maintenance, checkpoints, now)

	if err := executor.Execute(context.Background(), plan.ID, "worker-1"); err != nil {
		t.Fatalf("execute replay: %v", err)
	}
	if len(deleter.calls) != 0 || session.gcCalls != 1 || !coordinator.succeeded {
		t.Fatalf("manifest calls=%d GC receipt calls=%d succeeded=%t", len(deleter.calls), session.gcCalls, coordinator.succeeded)
	}
	if len(coordinator.recordOrdinals) != 1 || coordinator.recordOrdinals[0] != 2 {
		t.Fatalf("record ordinals = %#v", coordinator.recordOrdinals)
	}
	if len(checkpoints.requests) != 1 || len(checkpoints.requests[0].CandidateDigests) != 2 {
		t.Fatalf("stable candidate set lost deleted alias: %#v", checkpoints.requests)
	}
}

func TestCleanupExecutorFailsClosedOnReachableCandidate(t *testing.T) {
	now := time.Date(2026, 8, 9, 3, 0, 0, 0, time.UTC)
	plan := executorPlan("deleted", "planned")
	coordinator := &fakeCleanupCoordinator{plan: plan, recordErrorAt: -1}
	deleter := &fakeManifestDeleter{target: executorTarget(), result: ManifestDeleted}
	session := &fakeMaintenanceSession{enteredAt: now.Add(time.Second)}
	maintenance := &fakeMaintenanceAdapter{session: session}
	checkpoints := &fakeCheckpointProvider{coordinator: coordinator, mutate: func(checkpoint *RegistryReachabilityCheckpoint) {
		checkpoint.Blobs[0].Reachable = true
	}}
	executor := newTestExecutor(t, coordinator, deleter, maintenance, checkpoints, now)

	err := executor.Execute(context.Background(), plan.ID, "worker-1")
	if !errors.Is(err, ErrRegistryCheckpointIncomplete) || session.gcCalls != 0 || session.restoreCalls != 1 || session.releaseCalls != 1 {
		t.Fatalf("err=%v gc=%d restore=%d release=%d", err, session.gcCalls, session.restoreCalls, session.releaseCalls)
	}
	if coordinator.succeeded || !coordinator.finished || coordinator.plan.Items[1].State == "deleted" {
		t.Fatalf("failure was not closed: finished=%t success=%t state=%q", coordinator.finished, coordinator.succeeded, coordinator.plan.Items[1].State)
	}
}

func TestCleanupExecutorReturnsExplicitUnavailableMaintenanceAdapter(t *testing.T) {
	now := time.Date(2026, 8, 9, 3, 0, 0, 0, time.UTC)
	plan := executorPlan("deleted", "planned")
	coordinator := &fakeCleanupCoordinator{plan: plan, recordErrorAt: -1}
	deleter := &fakeManifestDeleter{target: executorTarget(), result: ManifestDeleted}
	executor := newTestExecutor(t, coordinator, deleter, UnavailableMaintenanceAdapter{}, &fakeCheckpointProvider{}, now)

	err := executor.Execute(context.Background(), plan.ID, "worker-1")
	if !errors.Is(err, ErrRegistryMaintenanceUnavailable) || coordinator.succeeded || coordinator.plan.Items[1].State == "deleted" {
		t.Fatalf("err=%v success=%t state=%q", err, coordinator.succeeded, coordinator.plan.Items[1].State)
	}
}

func TestCleanupExecutorTerminatesStaleOfflineSweepBeforeMaintenance(t *testing.T) {
	now := time.Date(2026, 8, 9, 3, 0, 0, 0, time.UTC)
	plan := executorPlan("deleted", "deleting", "deleting")
	coordinator := &fakeCleanupCoordinator{plan: plan, recordErrorAt: -1}
	deleter := &fakeManifestDeleter{target: executorTarget(), result: ManifestAlreadyMissing}
	maintenance := &fakeMaintenanceAdapter{err: store.ErrRegistrySnapshotStale}
	executor := newTestExecutor(t, coordinator, deleter, maintenance, &fakeCheckpointProvider{}, now)

	err := executor.Execute(context.Background(), plan.ID, "worker-1")
	if !errors.Is(err, store.ErrRegistrySnapshotStale) {
		t.Fatalf("err = %v", err)
	}
	if !coordinator.finished || coordinator.succeeded || len(coordinator.records) != 2 {
		t.Fatalf("finished=%t succeeded=%t records=%#v", coordinator.finished, coordinator.succeeded, coordinator.records)
	}
	for _, item := range coordinator.plan.Items[1:] {
		if item.State != "failed" || item.ProviderMessage != "managed registry cleanup failed" {
			t.Fatalf("stale item = %#v", item)
		}
	}
	if store.RegistryCleanupPlanCanResumeOfflineSweep(coordinator.plan) {
		t.Fatal("terminal stale sweep remained resumable")
	}
}

func TestCleanupExecutorRestoresAfterPartialMaintenanceEntryFailure(t *testing.T) {
	now := time.Date(2026, 8, 9, 3, 0, 0, 0, time.UTC)
	plan := executorPlan("deleted", "planned")
	coordinator := &fakeCleanupCoordinator{plan: plan, recordErrorAt: -1}
	deleter := &fakeManifestDeleter{target: executorTarget(), result: ManifestDeleted}
	session := &fakeMaintenanceSession{enteredAt: now.Add(time.Second), enterErr: errors.New("controller-secret-marker")}
	maintenance := &fakeMaintenanceAdapter{session: session}
	executor := newTestExecutor(t, coordinator, deleter, maintenance, &fakeCheckpointProvider{}, now)

	err := executor.Execute(context.Background(), plan.ID, "worker-1")
	if err == nil || strings.Contains(err.Error(), "controller-secret-marker") || session.gcCalls != 0 || session.restoreCalls != 1 || session.releaseCalls != 1 {
		t.Fatalf("err=%v gc=%d restore=%d release=%d", err, session.gcCalls, session.restoreCalls, session.releaseCalls)
	}
}

func TestCleanupExecutorReleasesPartialMaintenanceAcquire(t *testing.T) {
	now := time.Date(2026, 8, 9, 3, 0, 0, 0, time.UTC)
	plan := executorPlan("deleted", "planned")
	coordinator := &fakeCleanupCoordinator{plan: plan, recordErrorAt: -1}
	deleter := &fakeManifestDeleter{target: executorTarget(), result: ManifestDeleted}
	session := &fakeMaintenanceSession{}
	maintenance := &fakeMaintenanceAdapter{session: session, err: errors.New("acquire-secret-marker"), returnSessionOnError: true}
	executor := newTestExecutor(t, coordinator, deleter, maintenance, &fakeCheckpointProvider{}, now)

	err := executor.Execute(context.Background(), plan.ID, "worker-1")
	if err == nil || strings.Contains(err.Error(), "acquire-secret-marker") || session.restoreCalls != 0 || session.releaseCalls != 1 {
		t.Fatalf("err=%v restore=%d release=%d", err, session.restoreCalls, session.releaseCalls)
	}
}

func TestCleanupExecutorRejectsAuthorizedItemSubstitution(t *testing.T) {
	now := time.Date(2026, 8, 9, 3, 0, 0, 0, time.UTC)
	plan := executorPlan("planned")
	coordinator := &fakeCleanupCoordinator{plan: plan, recordErrorAt: -1, authorizeMutation: func(item domain.RegistryCleanupItem) domain.RegistryCleanupItem {
		item.Repository = "kuberploy/team-a/other"
		return item
	}}
	deleter := &fakeManifestDeleter{target: executorTarget(), result: ManifestDeleted}
	executor := newTestExecutor(t, coordinator, deleter, UnavailableMaintenanceAdapter{}, &fakeCheckpointProvider{}, now)

	if err := executor.Execute(context.Background(), plan.ID, "worker-1"); !errors.Is(err, ErrRegistryExecutionInvalid) {
		t.Fatalf("err = %v", err)
	}
	if len(deleter.calls) != 0 {
		t.Fatalf("substituted item reached provider: %#v", deleter.calls)
	}
}

func TestCleanupExecutorRejectsClaimedPlanSubstitution(t *testing.T) {
	now := time.Date(2026, 8, 9, 3, 0, 0, 0, time.UTC)
	plan := executorPlan("planned")
	coordinator := &fakeCleanupCoordinator{plan: plan, recordErrorAt: -1}
	deleter := &fakeManifestDeleter{target: executorTarget(), result: ManifestDeleted}
	config := DefaultCleanupExecutorConfig()
	config.LeaseDuration = time.Minute
	config.LeaseRenewInterval = time.Second
	executor, err := NewCleanupExecutor(substitutingCleanupCoordinator{coordinator}, deleter, UnavailableMaintenanceAdapter{}, &fakeCheckpointProvider{}, config)
	if err != nil {
		t.Fatal(err)
	}
	executor.now = func() time.Time { return now }
	if err = executor.Execute(context.Background(), "different-plan", "worker-1"); !errors.Is(err, ErrRegistryExecutionInvalid) {
		t.Fatalf("err = %v", err)
	}
	if len(deleter.calls) != 0 {
		t.Fatalf("substituted plan reached provider: %#v", deleter.calls)
	}
}

func TestCleanupExecutorRejectsExternalTarget(t *testing.T) {
	target := executorTarget()
	target.Mode = domain.RegistryTargetExternal
	deleter := &fakeManifestDeleter{target: target, result: ManifestDeleted}
	config := DefaultCleanupExecutorConfig()
	if _, err := NewCleanupExecutor(&fakeCleanupCoordinator{}, deleter, UnavailableMaintenanceAdapter{}, &fakeCheckpointProvider{}, config); !errors.Is(err, store.ErrRegistryExternalLifecycle) {
		t.Fatalf("err = %v", err)
	}
}
