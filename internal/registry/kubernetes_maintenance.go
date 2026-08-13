package registry

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/store"
)

type ManagedRegistryStopProof struct {
	Namespace             string
	Deployment            string
	DeploymentUID         string
	OriginalReplicas      int32
	PersistentVolumeClaim string
	RegistryConfigMap     string
	Stopped               bool
	NoSelectedPods        bool
	ObservedAt            time.Time
}

type ManagedRegistryRestoreProof struct {
	Namespace         string
	Deployment        string
	DeploymentUID     string
	DesiredReplicas   int32
	AvailableReplicas int32
	Ready             bool
	ObservedAt        time.Time
}

type RegistryMaintenanceJobEvidence struct {
	Name        string
	UID         string
	InputDigest string
	StartedAt   time.Time
	CompletedAt time.Time
}

type RegistryMaintenanceWorkloads interface {
	Inspect(context.Context, RuntimeConfig) (ManagedRegistryStopProof, error)
	Stop(context.Context, RuntimeConfig, store.RegistryMaintenanceLease) (ManagedRegistryStopProof, error)
	VerifyStopped(context.Context, RuntimeConfig, store.RegistryMaintenanceLease) (ManagedRegistryStopProof, error)
	Restore(context.Context, RuntimeConfig, store.RegistryMaintenanceLease) (ManagedRegistryRestoreProof, error)
	Checkpoint(context.Context, RuntimeConfig, maintenanceHelperRequest) (physicalReachabilityCheckpoint, RegistryMaintenanceJobEvidence, error)
	Sweep(context.Context, RuntimeConfig, maintenanceHelperRequest) (GCSweepResult, RegistryMaintenanceJobEvidence, error)
	RecoverSweep(context.Context, RuntimeConfig, string, string, string, string) (maintenanceHelperRequest, GCSweepResult, RegistryMaintenanceJobEvidence, bool, error)
	DeleteJob(context.Context, RuntimeConfig, RegistryMaintenanceJobEvidence) error
}

type KubernetesMaintenanceConfig struct {
	LeaseDuration     time.Duration
	HeartbeatInterval time.Duration
	OperationTimeout  time.Duration
}

func DefaultKubernetesMaintenanceConfig() KubernetesMaintenanceConfig {
	return KubernetesMaintenanceConfig{LeaseDuration: 5 * time.Minute, HeartbeatInterval: time.Minute, OperationTimeout: 30 * time.Minute}
}

func (c KubernetesMaintenanceConfig) validate() error {
	if c.LeaseDuration < time.Minute || c.LeaseDuration > time.Hour || c.HeartbeatInterval < 5*time.Second ||
		c.HeartbeatInterval >= c.LeaseDuration/2 || c.OperationTimeout < time.Minute || c.OperationTimeout > 2*time.Hour {
		return ErrRegistryMaintenanceInvalid
	}
	return nil
}

// KubernetesMaintenanceAdapter is bound to one operator-owned managed
// registry profile. It implements both the maintenance and checkpoint seams so
// Capture can use the exact active, fenced session created by Acquire.
type KubernetesMaintenanceAdapter struct {
	store     store.RegistryRuntimeStore
	registry  store.RegistryStore
	workloads RegistryMaintenanceWorkloads
	runtime   RuntimeConfig
	config    KubernetesMaintenanceConfig
	now       func() time.Time
	mu        sync.Mutex
	sessions  map[string]*kubernetesMaintenanceSession
}

func NewKubernetesMaintenanceAdapter(runtimeStore store.RegistryRuntimeStore, registryStore store.RegistryStore, workloads RegistryMaintenanceWorkloads, runtime RuntimeConfig, config KubernetesMaintenanceConfig) (*KubernetesMaintenanceAdapter, error) {
	if runtimeStore == nil || registryStore == nil || workloads == nil || runtime.Validate() != nil || !runtime.Enabled || config.validate() != nil {
		return nil, ErrRegistryMaintenanceInvalid
	}
	return &KubernetesMaintenanceAdapter{store: runtimeStore, registry: registryStore, workloads: workloads,
		runtime: runtime, config: config, now: func() time.Time { return time.Now().UTC() }, sessions: make(map[string]*kubernetesMaintenanceSession)}, nil
}

func (a *KubernetesMaintenanceAdapter) Acquire(ctx context.Context, request MaintenanceAcquireRequest) (RegistryMaintenanceSession, error) {
	if a == nil || request.TargetID != a.runtime.TargetID || !validSafeIdentity(request.PlanID) || !validDigest(request.ExecutionKey) || !validSafeIdentity(request.Owner) {
		return nil, ErrRegistryMaintenanceInvalid
	}
	plan, err := a.registry.RegistryCleanupPlan(ctx, request.PlanID)
	if err != nil {
		return nil, err
	}
	if plan.RegistryTargetID != request.TargetID || plan.State != "executing" || !validDigest(plan.PlanDigest) {
		return nil, ErrRegistryMaintenanceInvalid
	}
	// A failed offline sweep may be retried after its original repository
	// leases expired. Revalidate the mutable protection authorities before
	// acquiring the exclusive runtime lease or stopping the registry. The
	// physical checkpoint below remains a second fence, but it must not be the
	// first place a stale plan is discovered because that causes avoidable
	// registry downtime.
	snapshot, err := a.registry.RegistryLifecycleSnapshot(ctx, plan.RegistryTargetID, plan.ServiceID, a.now())
	if err != nil {
		return nil, err
	}
	if store.RegistryAuthorityToken(snapshot) != plan.AuthorityToken {
		return nil, store.ErrRegistrySnapshotStale
	}
	candidates := cleanupBlobItems(plan.Items)
	if len(candidates) < 1 || len(candidates) > maximumMaintenanceCandidates {
		return nil, ErrRegistryMaintenanceInvalid
	}
	digests := make([]string, 0, len(candidates))
	for _, item := range candidates {
		if item.State != "deleting" && item.State != "deleted" {
			return nil, ErrRegistryMaintenanceInvalid
		}
		digests = append(digests, item.Digest)
	}
	candidateDigest, ordered, err := cleanupCandidateSetDigest(digests)
	if err != nil {
		return nil, err
	}
	lease, err := a.store.AcquireRegistryMaintenance(ctx, request.TargetID, request.PlanID, request.ExecutionKey, candidateDigest, request.Owner, a.now(), a.config.LeaseDuration)
	if err != nil {
		return nil, err
	}
	session := &kubernetesMaintenanceSession{adapter: a, lease: lease, plan: plan, candidates: ordered, heartbeatDone: make(chan struct{})}
	session.heartbeatContext, session.cancelHeartbeat = context.WithCancel(context.WithoutCancel(ctx))
	go session.heartbeat()
	a.mu.Lock()
	if current := a.sessions[request.ExecutionKey]; current != nil {
		a.mu.Unlock()
		session.cancelHeartbeat()
		<-session.heartbeatDone
		_ = a.store.ReleaseRegistryMaintenance(context.WithoutCancel(ctx), lease, a.now())
		return nil, ErrRegistryMaintenanceInvalid
	}
	a.sessions[request.ExecutionKey] = session
	a.mu.Unlock()
	return session, nil
}

func (a *KubernetesMaintenanceAdapter) Capture(ctx context.Context, request ReachabilityCheckpointRequest) (RegistryReachabilityCheckpoint, error) {
	if a == nil || !validDigest(request.ExecutionKey) {
		return RegistryReachabilityCheckpoint{}, ErrRegistryCheckpointIncomplete
	}
	a.mu.Lock()
	session := a.sessions[request.ExecutionKey]
	a.mu.Unlock()
	if session == nil {
		return RegistryReachabilityCheckpoint{}, ErrRegistryCheckpointIncomplete
	}
	return session.capture(ctx, request)
}

type kubernetesMaintenanceSession struct {
	adapter          *KubernetesMaintenanceAdapter
	mu               sync.Mutex
	lease            store.RegistryMaintenanceLease
	plan             domain.RegistryCleanupPlan
	candidates       []string
	enteredAt        time.Time
	restored         bool
	released         bool
	heartbeatContext context.Context
	cancelHeartbeat  context.CancelFunc
	heartbeatDone    chan struct{}
	heartbeatErr     error
}

func (s *kubernetesMaintenanceSession) heartbeat() {
	defer close(s.heartbeatDone)
	ticker := time.NewTicker(s.adapter.config.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.heartbeatContext.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			lease := s.lease
			s.mu.Unlock()
			updated, err := s.adapter.store.HeartbeatRegistryMaintenance(s.heartbeatContext, lease, s.adapter.now(), s.adapter.config.LeaseDuration)
			if err != nil {
				s.mu.Lock()
				s.heartbeatErr = err
				s.mu.Unlock()
				s.cancelHeartbeat()
				return
			}
			s.mu.Lock()
			s.lease = updated
			s.mu.Unlock()
		}
	}
}

func (s *kubernetesMaintenanceSession) checkHeartbeat() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.heartbeatErr
}

func (s *kubernetesMaintenanceSession) Enter(ctx context.Context) (MaintenanceReady, error) {
	if s == nil || s.adapter == nil || s.checkHeartbeat() != nil {
		return MaintenanceReady{}, ErrRegistryMaintenanceInvalid
	}
	s.mu.Lock()
	lease := s.lease
	s.mu.Unlock()
	var err error
	if lease.DeploymentUID == "" {
		inspection, inspectErr := s.adapter.workloads.Inspect(ctx, s.adapter.runtime)
		if inspectErr != nil {
			return MaintenanceReady{}, inspectErr
		}
		if err = validateDeploymentIdentity(inspection, s.adapter.runtime, s.adapter.now()); err != nil {
			return MaintenanceReady{}, err
		}
		lease, err = s.adapter.store.PrepareRegistryMaintenanceStop(ctx, lease, inspection.DeploymentUID, inspection.OriginalReplicas, s.adapter.now())
		if err != nil {
			return MaintenanceReady{}, err
		}
		// Stop may successfully scale the Deployment to zero and then lose its
		// response or fail while proving pod termination. Publish the durable
		// prepared identity to the session before that mutation so deferred
		// recovery can always restore the exact Deployment and replica count.
		s.mu.Lock()
		s.lease = lease
		s.mu.Unlock()
	}
	proof, err := s.adapter.workloads.Stop(ctx, s.adapter.runtime, lease)
	if err != nil {
		return MaintenanceReady{}, err
	}
	if err = validateStopProof(proof, s.adapter.runtime, lease.DeploymentUID, lease.OriginalReplicas, s.adapter.now()); err != nil {
		return MaintenanceReady{}, err
	}
	lease, err = s.adapter.store.EnterRegistryMaintenance(ctx, lease, proof.DeploymentUID, proof.OriginalReplicas, string(RegistryMaintenanceStopped), s.adapter.now())
	if err != nil {
		return MaintenanceReady{}, err
	}
	s.mu.Lock()
	s.lease, s.enteredAt = lease, proof.ObservedAt
	s.mu.Unlock()
	return MaintenanceReady{TargetID: lease.TargetID, LeaseID: maintenanceLeaseID(lease), ExecutionKey: lease.ExecutionKey,
		Mode: RegistryMaintenanceStopped, Exclusive: true, Tested: true, EnteredAt: proof.ObservedAt}, nil
}

func validateStopProof(proof ManagedRegistryStopProof, runtime RuntimeConfig, expectedUID string, expectedReplicas int32, now time.Time) error {
	if validateDeploymentIdentity(proof, runtime, now) != nil || !proof.Stopped || !proof.NoSelectedPods ||
		expectedUID != "" && (proof.DeploymentUID != expectedUID || proof.OriginalReplicas != expectedReplicas) {
		return ErrRegistryMaintenanceInvalid
	}
	return nil
}

func validateDeploymentIdentity(proof ManagedRegistryStopProof, runtime RuntimeConfig, now time.Time) error {
	if proof.Namespace != runtime.Namespace || proof.Deployment != runtime.Deployment || proof.PersistentVolumeClaim != runtime.PersistentVolumeClaim ||
		proof.RegistryConfigMap != runtime.RegistryConfigMap || proof.DeploymentUID == "" || proof.OriginalReplicas < 1 ||
		proof.ObservedAt.IsZero() || proof.ObservedAt.After(now.Add(time.Minute)) {
		return ErrRegistryMaintenanceInvalid
	}
	return nil
}

func maintenanceLeaseID(lease store.RegistryMaintenanceLease) string {
	return "lease-" + strings.TrimPrefix(lease.ExecutionKey, "sha256:")[:16] + fmt.Sprintf("-%d", lease.Epoch)
}

func (s *kubernetesMaintenanceSession) capture(ctx context.Context, request ReachabilityCheckpointRequest) (RegistryReachabilityCheckpoint, error) {
	if s == nil {
		return RegistryReachabilityCheckpoint{}, ErrRegistryCheckpointIncomplete
	}
	s.mu.Lock()
	initialLease := s.lease
	s.mu.Unlock()
	if request.TargetID != initialLease.TargetID || request.PlanID != s.plan.ID || request.PlanDigest != s.plan.PlanDigest ||
		request.ExecutionKey != initialLease.ExecutionKey || request.CandidateSetDigest != initialLease.CandidateSetDigest ||
		len(request.CandidateDigests) != len(s.candidates) || request.NotBefore.IsZero() {
		return RegistryReachabilityCheckpoint{}, ErrRegistryCheckpointIncomplete
	}
	for index := range s.candidates {
		if request.CandidateDigests[index] != s.candidates[index] {
			return RegistryReachabilityCheckpoint{}, ErrRegistryCheckpointIncomplete
		}
	}
	if err := s.checkHeartbeat(); err != nil {
		return RegistryReachabilityCheckpoint{}, err
	}
	s.mu.Lock()
	lease := s.lease
	s.mu.Unlock()
	proof, err := s.adapter.workloads.VerifyStopped(ctx, s.adapter.runtime, lease)
	if err != nil || validateStopProof(proof, s.adapter.runtime, lease.DeploymentUID, lease.OriginalReplicas, s.adapter.now()) != nil {
		return RegistryReachabilityCheckpoint{}, ErrRegistryCheckpointIncomplete
	}
	// Recover a sweep whose deterministic Job completed before its immutable DB
	// receipt was committed. The registry is still stopped here.
	if lease.State == "sweeping" && lease.SweepJobUID != "" {
		oldRequest, oldSweep, evidence, found, recoverErr := s.adapter.workloads.RecoverSweep(ctx, s.adapter.runtime,
			lease.TargetID, lease.PlanID, lease.ExecutionKey, lease.CandidateSetDigest)
		if recoverErr != nil {
			return RegistryReachabilityCheckpoint{}, recoverErr
		}
		if found {
			if oldRequest.CheckpointRevision != lease.CheckpointRevision || oldSweep.CheckpointRevision != lease.CheckpointRevision ||
				oldSweep.ExecutionKey != lease.ExecutionKey || oldSweep.CandidateSetDigest != lease.CandidateSetDigest || !oldSweep.Complete {
				return RegistryReachabilityCheckpoint{}, ErrRegistryGCSweepUnconfirmed
			}
			receipt := sweepReceipt(lease, oldSweep, evidence)
			if err = s.adapter.store.CompleteRegistryGCSweep(ctx, lease, receipt, s.adapter.now()); err != nil {
				return RegistryReachabilityCheckpoint{}, err
			}
			_ = s.adapter.workloads.DeleteJob(context.WithoutCancel(ctx), s.adapter.runtime, evidence)
			lease.State = "swept"
		}
	}
	helperRequest := maintenanceHelperRequest{Version: 1, Mode: "checkpoint", TargetID: request.TargetID, PlanID: request.PlanID,
		PlanDigest: request.PlanDigest, ExecutionKey: request.ExecutionKey, CandidateSetDigest: request.CandidateSetDigest,
		CandidateDigests: append([]string(nil), request.CandidateDigests...), NotBefore: proof.ObservedAt}
	physical, evidence, err := s.adapter.workloads.Checkpoint(ctx, s.adapter.runtime, helperRequest)
	if err != nil || validatePhysicalCheckpoint(physical, helperRequest, s.adapter.now()) != nil {
		return RegistryReachabilityCheckpoint{}, ErrRegistryCheckpointIncomplete
	}
	plan, err := s.adapter.registry.RegistryCleanupPlan(ctx, s.plan.ID)
	if err != nil || plan.State != "executing" || plan.PlanDigest != s.plan.PlanDigest || plan.RegistryTargetID != s.lease.TargetID {
		return RegistryReachabilityCheckpoint{}, ErrRegistryCheckpointIncomplete
	}
	snapshot, err := s.adapter.registry.RegistryLifecycleSnapshot(ctx, plan.RegistryTargetID, plan.ServiceID, s.adapter.now())
	if err != nil || store.RegistryAuthorityToken(snapshot) != plan.AuthorityToken || !completeRegistryAuthorities(snapshot, s.adapter.now()) {
		return RegistryReachabilityCheckpoint{}, ErrRegistryCheckpointIncomplete
	}
	authorityRevision := "authority-" + strings.TrimPrefix(plan.AuthorityToken, "sha256:")[:24]
	checkpoint := RegistryReachabilityCheckpoint{
		TargetID: physical.TargetID, PlanID: physical.PlanID, PlanDigest: physical.PlanDigest,
		ExecutionKey: physical.ExecutionKey, CandidateSetDigest: physical.CandidateSetDigest,
		Revision: physical.Revision, InventoryRevision: physical.InventoryRevision, AuthorityRevision: authorityRevision,
		RegistryWide: physical.RegistryWide, InventoryComplete: physical.InventoryComplete, AuthorityComplete: true,
		ReachabilityComplete: physical.ReachabilityComplete, StartedAt: physical.StartedAt, ObservedAt: physical.ObservedAt,
		Blobs: append([]RegistryBlobReachability(nil), physical.Blobs...),
	}
	checkpoint.GraphDigest = ReachabilityCheckpointDigest(checkpoint)
	if err = validateReachabilityCheckpoint(checkpoint, request, s.adapter.now()); err != nil {
		return RegistryReachabilityCheckpoint{}, err
	}
	lease, err = s.adapter.store.RecordRegistryCheckpoint(ctx, lease, checkpoint.Revision, checkpoint.GraphDigest, checkpoint.ObservedAt, s.adapter.now())
	if err != nil {
		return RegistryReachabilityCheckpoint{}, err
	}
	s.mu.Lock()
	s.lease = lease
	s.mu.Unlock()
	_ = s.adapter.workloads.DeleteJob(context.WithoutCancel(ctx), s.adapter.runtime, evidence)
	return checkpoint, nil
}

func validatePhysicalCheckpoint(checkpoint physicalReachabilityCheckpoint, request maintenanceHelperRequest, now time.Time) error {
	if checkpoint.TargetID != request.TargetID || checkpoint.PlanID != request.PlanID || checkpoint.PlanDigest != request.PlanDigest ||
		checkpoint.ExecutionKey != request.ExecutionKey || checkpoint.CandidateSetDigest != request.CandidateSetDigest ||
		!validSafeIdentity(checkpoint.Revision) || !validSafeIdentity(checkpoint.InventoryRevision) || !checkpoint.RegistryWide ||
		!checkpoint.InventoryComplete || !checkpoint.ReachabilityComplete || checkpoint.StartedAt.Before(request.NotBefore) ||
		checkpoint.ObservedAt.Before(checkpoint.StartedAt) || checkpoint.ObservedAt.After(now.Add(time.Minute)) || len(checkpoint.Blobs) != len(request.CandidateDigests) {
		return ErrRegistryCheckpointIncomplete
	}
	for index, row := range checkpoint.Blobs {
		if row.Digest != request.CandidateDigests[index] || row.Reachable && !row.Present {
			return ErrRegistryCheckpointIncomplete
		}
	}
	return nil
}

func completeRegistryAuthorities(snapshot domain.RegistryLifecycleSnapshot, now time.Time) bool {
	found := map[domain.RegistryAuthority]bool{}
	for _, observation := range snapshot.AuthorityObservations {
		if observation.Authority != domain.RegistryAuthorityGitIntent && observation.Authority != domain.RegistryAuthorityRuntime && observation.Authority != domain.RegistryAuthorityOperations {
			return false
		}
		if observation.RegistryTargetID != snapshot.Target.ID || !observation.Complete || observation.Revision == "" ||
			observation.ObservedAt.IsZero() || observation.ObservedAt.After(now.Add(time.Minute)) || found[observation.Authority] {
			return false
		}
		found[observation.Authority] = true
	}
	return found[domain.RegistryAuthorityGitIntent] && found[domain.RegistryAuthorityRuntime] && found[domain.RegistryAuthorityOperations]
}

func (s *kubernetesMaintenanceSession) GarbageCollect(ctx context.Context, request GCSweepRequest) (GCSweepResult, error) {
	if s == nil {
		return GCSweepResult{}, ErrRegistryGCSweepUnconfirmed
	}
	s.mu.Lock()
	initialLease := s.lease
	s.mu.Unlock()
	if request.TargetID != initialLease.TargetID || request.PlanID != s.plan.ID || request.ExecutionKey != initialLease.ExecutionKey ||
		request.CandidateSetDigest != initialLease.CandidateSetDigest || request.Checkpoint.Revision == "" || len(request.CandidateDigests) != len(s.candidates) {
		return GCSweepResult{}, ErrRegistryGCSweepUnconfirmed
	}
	for index := range s.candidates {
		if request.CandidateDigests[index] != s.candidates[index] {
			return GCSweepResult{}, ErrRegistryGCSweepUnconfirmed
		}
	}
	if err := s.checkHeartbeat(); err != nil {
		return GCSweepResult{}, err
	}
	s.mu.Lock()
	lease := s.lease
	s.mu.Unlock()
	receipt, found, err := s.adapter.store.RegistryGCSweepReceipt(ctx, lease, s.adapter.now())
	if err != nil {
		return GCSweepResult{}, err
	}
	if found {
		return GCSweepResult{TargetID: receipt.TargetID, ExecutionKey: receipt.ExecutionKey,
			CandidateSetDigest: receipt.CandidateSetDigest, CheckpointRevision: receipt.CheckpointRevision,
			ProviderSweepID: receipt.ProviderSweepID, Complete: true, Replay: true,
			StartedAt: receipt.StartedAt, CompletedAt: receipt.CompletedAt}, nil
	}
	helperRequest := maintenanceHelperRequest{Version: 1, Mode: "gc", TargetID: request.TargetID, PlanID: request.PlanID,
		PlanDigest: s.plan.PlanDigest, ExecutionKey: request.ExecutionKey, CandidateSetDigest: request.CandidateSetDigest,
		CandidateDigests: append([]string(nil), request.CandidateDigests...), CheckpointRevision: request.Checkpoint.Revision,
		NotBefore: request.Checkpoint.ObservedAt}
	jobName := maintenanceJobName("gc", request.ExecutionKey)
	_, replay, err := s.adapter.store.BeginRegistryGCSweep(ctx, lease, jobName, s.adapter.now())
	if err != nil || replay {
		if err != nil {
			return GCSweepResult{}, err
		}
		return GCSweepResult{}, ErrRegistryGCSweepUnconfirmed
	}
	sweep, evidence, err := s.adapter.workloads.Sweep(ctx, s.adapter.runtime, helperRequest)
	if err != nil || validateGCSweepResult(sweep, request, request.Checkpoint.ObservedAt, s.adapter.now()) != nil {
		return GCSweepResult{}, ErrRegistryGCSweepUnconfirmed
	}
	receipt = sweepReceipt(lease, sweep, evidence)
	if err = s.adapter.store.CompleteRegistryGCSweep(ctx, lease, receipt, s.adapter.now()); err != nil {
		return GCSweepResult{}, err
	}
	s.mu.Lock()
	s.lease.State, s.lease.SweepJobUID = "swept", jobName
	s.mu.Unlock()
	_ = s.adapter.workloads.DeleteJob(context.WithoutCancel(ctx), s.adapter.runtime, evidence)
	return sweep, nil
}

func sweepReceipt(lease store.RegistryMaintenanceLease, sweep GCSweepResult, evidence RegistryMaintenanceJobEvidence) store.RegistryGCSweepReceipt {
	return store.RegistryGCSweepReceipt{TargetID: lease.TargetID, ExecutionKey: lease.ExecutionKey, PlanID: lease.PlanID,
		CandidateSetDigest: lease.CandidateSetDigest, CheckpointRevision: sweep.CheckpointRevision,
		ProviderSweepID: sweep.ProviderSweepID, HelperJobUID: evidence.UID, StartedAt: sweep.StartedAt, CompletedAt: sweep.CompletedAt}
}

func maintenanceJobName(mode, executionKey string) string {
	return "kuberploy-registry-" + mode + "-" + strings.TrimPrefix(executionKey, "sha256:")[:20]
}

func (s *kubernetesMaintenanceSession) Restore(ctx context.Context) error {
	if s == nil || s.adapter == nil {
		return ErrRegistryMaintenanceInvalid
	}
	s.mu.Lock()
	if s.restored {
		s.mu.Unlock()
		return nil
	}
	lease := s.lease
	s.mu.Unlock()
	proof, err := s.adapter.workloads.Restore(ctx, s.adapter.runtime, lease)
	if err != nil {
		return err
	}
	if proof.Namespace != s.adapter.runtime.Namespace || proof.Deployment != s.adapter.runtime.Deployment || proof.DeploymentUID != lease.DeploymentUID ||
		proof.DesiredReplicas != lease.OriginalReplicas || proof.AvailableReplicas != lease.OriginalReplicas || !proof.Ready ||
		proof.ObservedAt.IsZero() || proof.ObservedAt.After(s.adapter.now().Add(time.Minute)) {
		return ErrRegistryMaintenanceInvalid
	}
	if lease.State != "acquired" {
		if err = s.adapter.store.MarkRegistryMaintenanceRestored(ctx, lease, s.adapter.now()); err != nil {
			return err
		}
		lease.State = "restored"
	}
	s.mu.Lock()
	s.lease, s.restored = lease, true
	s.mu.Unlock()
	return nil
}

func (s *kubernetesMaintenanceSession) Release(ctx context.Context) error {
	if s == nil || s.adapter == nil {
		return ErrRegistryMaintenanceInvalid
	}
	s.mu.Lock()
	if s.released {
		s.mu.Unlock()
		return nil
	}
	lease, restored := s.lease, s.restored
	s.mu.Unlock()
	if lease.DeploymentUID != "" && !restored {
		return ErrRegistryMaintenanceInvalid
	}
	if err := s.adapter.store.ReleaseRegistryMaintenance(ctx, lease, s.adapter.now()); err != nil {
		return err
	}
	s.cancelHeartbeat()
	<-s.heartbeatDone
	s.adapter.mu.Lock()
	delete(s.adapter.sessions, lease.ExecutionKey)
	s.adapter.mu.Unlock()
	s.mu.Lock()
	s.released = true
	s.mu.Unlock()
	return nil
}

var _ RegistryMaintenanceAdapter = (*KubernetesMaintenanceAdapter)(nil)
var _ RegistryCheckpointProvider = (*KubernetesMaintenanceAdapter)(nil)
