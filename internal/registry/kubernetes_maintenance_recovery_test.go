package registry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/store"
)

type preparedMaintenanceStore struct {
	store.RegistryRuntimeStore
}

type replayedSweepStore struct {
	store.RegistryRuntimeStore
	receipt store.RegistryGCSweepReceipt
}

func (s *replayedSweepStore) RegistryGCSweepReceipt(context.Context, store.RegistryMaintenanceLease, time.Time) (store.RegistryGCSweepReceipt, bool, error) {
	return store.RegistryGCSweepReceipt{}, false, nil
}

func (s *replayedSweepStore) BeginRegistryGCSweep(_ context.Context, _ store.RegistryMaintenanceLease, _ string, _ time.Time) (store.RegistryGCSweepReceipt, bool, error) {
	return store.RegistryGCSweepReceipt{}, true, nil
}

func (s *replayedSweepStore) CompleteRegistryGCSweep(_ context.Context, _ store.RegistryMaintenanceLease, receipt store.RegistryGCSweepReceipt, _ time.Time) error {
	s.receipt = receipt
	return nil
}

func (preparedMaintenanceStore) PrepareRegistryMaintenanceStop(_ context.Context, lease store.RegistryMaintenanceLease, uid string, replicas int32, _ time.Time) (store.RegistryMaintenanceLease, error) {
	lease.DeploymentUID = uid
	lease.OriginalReplicas = replicas
	return lease, nil
}

type stopResponseLostWorkloads struct {
	RegistryMaintenanceWorkloads
	now           time.Time
	restoredLease store.RegistryMaintenanceLease
}

func (w *stopResponseLostWorkloads) Inspect(context.Context, RuntimeConfig) (ManagedRegistryStopProof, error) {
	return ManagedRegistryStopProof{Namespace: "registry-system", Deployment: "registry", DeploymentUID: "registry-uid",
		OriginalReplicas: 1, PersistentVolumeClaim: "registry-data", RegistryConfigMap: "registry-config",
		ObservedAt: w.now}, nil
}

func (*stopResponseLostWorkloads) Stop(context.Context, RuntimeConfig, store.RegistryMaintenanceLease) (ManagedRegistryStopProof, error) {
	return ManagedRegistryStopProof{}, errors.New("response lost after scale")
}

func (w *stopResponseLostWorkloads) Restore(_ context.Context, _ RuntimeConfig, lease store.RegistryMaintenanceLease) (ManagedRegistryRestoreProof, error) {
	w.restoredLease = lease
	return ManagedRegistryRestoreProof{Namespace: "registry-system", Deployment: "registry", DeploymentUID: lease.DeploymentUID,
		DesiredReplicas: lease.OriginalReplicas, AvailableReplicas: lease.OriginalReplicas, Ready: true, ObservedAt: w.now}, nil
}

func TestEnterPublishesPreparedIdentityBeforeStopMutation(t *testing.T) {
	now := time.Now().UTC()
	runtime := RuntimeConfig{Enabled: true, Namespace: "registry-system", Deployment: "registry",
		PersistentVolumeClaim: "registry-data", RegistryConfigMap: "registry-config"}
	workloads := &stopResponseLostWorkloads{now: now}
	adapter := &KubernetesMaintenanceAdapter{store: preparedMaintenanceStore{}, workloads: workloads, runtime: runtime, now: func() time.Time { return now }}
	session := &kubernetesMaintenanceSession{adapter: adapter, lease: store.RegistryMaintenanceLease{
		TargetID: "11111111-1111-4111-8111-111111111111", PlanID: "22222222-2222-4222-8222-222222222222",
		ExecutionKey: "sha256:" + repeatHex("a", 64), CandidateSetDigest: "sha256:" + repeatHex("b", 64),
		Owner: "worker", Epoch: 1, Until: now.Add(time.Minute), State: "acquired"}}

	if _, err := session.Enter(context.Background()); err == nil || err.Error() != "response lost after scale" {
		t.Fatalf("unexpected enter result: %v", err)
	}
	if err := session.Restore(context.Background()); err != nil {
		t.Fatalf("prepared identity could not restore after stop response loss: %v", err)
	}
	if workloads.restoredLease.DeploymentUID != "registry-uid" || workloads.restoredLease.OriginalReplicas != 1 {
		t.Fatalf("restore lost prepared deployment identity: %+v", workloads.restoredLease)
	}
}

func TestRecoveredSweepRequiresExactImmutableIdentity(t *testing.T) {
	now := time.Now().UTC()
	digests := []string{"sha256:" + repeatHex("1", 64), "sha256:" + repeatHex("2", 64)}
	candidateSetDigest, ordered, err := cleanupCandidateSetDigest(digests)
	if err != nil {
		t.Fatal(err)
	}
	plan := domain.RegistryCleanupPlan{ID: "22222222-2222-4222-8222-222222222222", RegistryTargetID: "11111111-1111-4111-8111-111111111111", PlanDigest: "sha256:" + repeatHex("3", 64)}
	request := GCSweepRequest{TargetID: plan.RegistryTargetID, PlanID: plan.ID, ExecutionKey: "sha256:" + repeatHex("4", 64),
		CandidateSetDigest: candidateSetDigest, CandidateDigests: ordered, Checkpoint: RegistryReachabilityCheckpoint{Revision: "physical-current"}}
	oldRequest := maintenanceHelperRequest{Version: 1, Mode: "gc", TargetID: request.TargetID, PlanID: request.PlanID,
		PlanDigest: plan.PlanDigest, ExecutionKey: request.ExecutionKey, CandidateSetDigest: candidateSetDigest,
		CandidateDigests: append([]string(nil), ordered...), CheckpointRevision: "physical-prior", NotBefore: now.Add(-time.Minute)}
	sweep := GCSweepResult{TargetID: request.TargetID, ExecutionKey: request.ExecutionKey, CandidateSetDigest: candidateSetDigest,
		CheckpointRevision: oldRequest.CheckpointRevision, ProviderSweepID: "gc-proof", Complete: true, StartedAt: now.Add(-time.Minute), CompletedAt: now.Add(-time.Minute + time.Second)}
	if !sameRecoveredSweepIdentity(oldRequest, sweep, plan, request) {
		t.Fatal("exact completed sweep was not recoverable")
	}
	mutations := []func(*maintenanceHelperRequest, *GCSweepResult, *domain.RegistryCleanupPlan, *GCSweepRequest){
		func(value *maintenanceHelperRequest, _ *GCSweepResult, _ *domain.RegistryCleanupPlan, _ *GCSweepRequest) {
			value.PlanDigest = "sha256:" + repeatHex("f", 64)
		},
		func(value *maintenanceHelperRequest, _ *GCSweepResult, _ *domain.RegistryCleanupPlan, _ *GCSweepRequest) {
			value.CandidateDigests[0] = "sha256:" + repeatHex("f", 64)
		},
		func(_ *maintenanceHelperRequest, value *GCSweepResult, _ *domain.RegistryCleanupPlan, _ *GCSweepRequest) {
			value.ExecutionKey = "sha256:" + repeatHex("f", 64)
		},
		func(_ *maintenanceHelperRequest, value *GCSweepResult, _ *domain.RegistryCleanupPlan, _ *GCSweepRequest) {
			value.CheckpointRevision = "physical-substituted"
		},
		func(value *maintenanceHelperRequest, _ *GCSweepResult, _ *domain.RegistryCleanupPlan, _ *GCSweepRequest) {
			value.NotBefore = now
		},
		func(_ *maintenanceHelperRequest, value *GCSweepResult, _ *domain.RegistryCleanupPlan, _ *GCSweepRequest) {
			value.Complete = false
		},
	}
	for index, mutate := range mutations {
		changedRequest := oldRequest
		changedRequest.CandidateDigests = append([]string(nil), oldRequest.CandidateDigests...)
		changedSweep, changedPlan, changedGCRequest := sweep, plan, request
		mutate(&changedRequest, &changedSweep, &changedPlan, &changedGCRequest)
		if sameRecoveredSweepIdentity(changedRequest, changedSweep, changedPlan, changedGCRequest) {
			t.Fatalf("recovered sweep mutation %d accepted", index)
		}
	}
}

func TestCheckpointProvesExactCandidatesAbsent(t *testing.T) {
	digestA := "sha256:" + repeatHex("a", 64)
	digestB := "sha256:" + repeatHex("b", 64)
	checkpoint := RegistryReachabilityCheckpoint{RegistryWide: true, InventoryComplete: true, ReachabilityComplete: true,
		Blobs: []RegistryBlobReachability{{Digest: digestA}, {Digest: digestB}}}
	if !checkpointProvesCandidatesAbsent(checkpoint, []string{digestA, digestB}) {
		t.Fatal("exact absent candidates were not proven")
	}
	mutations := []func(*RegistryReachabilityCheckpoint, *[]string){
		func(value *RegistryReachabilityCheckpoint, _ *[]string) { value.RegistryWide = false },
		func(value *RegistryReachabilityCheckpoint, _ *[]string) { value.InventoryComplete = false },
		func(value *RegistryReachabilityCheckpoint, _ *[]string) { value.ReachabilityComplete = false },
		func(value *RegistryReachabilityCheckpoint, _ *[]string) { value.Blobs[0].Present = true },
		func(value *RegistryReachabilityCheckpoint, _ *[]string) { value.Blobs[0].Reachable = true },
		func(value *RegistryReachabilityCheckpoint, _ *[]string) { value.Blobs[1].Digest = digestA },
		func(_ *RegistryReachabilityCheckpoint, candidates *[]string) {
			(*candidates)[1] = "sha256:" + repeatHex("c", 64)
		},
	}
	for index, mutate := range mutations {
		changed := checkpoint
		changed.Blobs = append([]RegistryBlobReachability(nil), checkpoint.Blobs...)
		candidates := []string{digestA, digestB}
		mutate(&changed, &candidates)
		if checkpointProvesCandidatesAbsent(changed, candidates) {
			t.Fatalf("checkpoint mutation %d accepted", index)
		}
	}
}

func TestGarbageCollectReplayUsesFreshAbsentCheckpointWithoutSecondSweep(t *testing.T) {
	now := time.Now().UTC()
	targetID := "11111111-1111-4111-8111-111111111111"
	planID := "22222222-2222-4222-8222-222222222222"
	executionKey := "sha256:" + repeatHex("3", 64)
	digest := "sha256:" + repeatHex("4", 64)
	candidateSetDigest, candidates, err := cleanupCandidateSetDigest([]string{digest})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := RegistryReachabilityCheckpoint{TargetID: targetID, PlanID: planID,
		ExecutionKey: executionKey, CandidateSetDigest: candidateSetDigest, Revision: "physical-current",
		RegistryWide: true, InventoryComplete: true, ReachabilityComplete: true, ObservedAt: now.Add(-time.Second),
		Blobs: []RegistryBlobReachability{{Digest: digest}}}
	lease := store.RegistryMaintenanceLease{TargetID: targetID, PlanID: planID, ExecutionKey: executionKey,
		CandidateSetDigest: candidateSetDigest, Owner: "worker", Epoch: 2, Until: now.Add(time.Minute),
		State: "sweeping", CheckpointRevision: checkpoint.Revision, SweepJobUID: "prior-gc-job"}
	repository := &replayedSweepStore{}
	session := &kubernetesMaintenanceSession{adapter: &KubernetesMaintenanceAdapter{store: repository, now: func() time.Time { return now }},
		lease: lease, plan: domain.RegistryCleanupPlan{ID: planID, RegistryTargetID: targetID}, candidates: candidates,
		checkpointJob: RegistryMaintenanceJobEvidence{Name: "checkpoint-job", UID: "checkpoint-job-uid",
			StartedAt: now.Add(-2 * time.Second), CompletedAt: now.Add(-time.Second)}}
	result, err := session.GarbageCollect(context.Background(), GCSweepRequest{TargetID: targetID, PlanID: planID,
		ExecutionKey: executionKey, CandidateSetDigest: candidateSetDigest, CandidateDigests: candidates, Checkpoint: checkpoint})
	if err != nil || !result.Complete || !result.Replay || result.ProviderSweepID == "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if repository.receipt.HelperJobUID != "checkpoint-job-uid" || repository.receipt.ProviderSweepID != result.ProviderSweepID ||
		repository.receipt.CheckpointRevision != checkpoint.Revision {
		t.Fatalf("receipt=%+v", repository.receipt)
	}
}
