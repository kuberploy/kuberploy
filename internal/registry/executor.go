package registry

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/store"
)

var (
	ErrRegistryExecutionInvalid = errors.New("managed registry cleanup execution is invalid")
)

type CleanupCoordinator interface {
	Claim(context.Context, string, string, time.Duration) (domain.RegistryCleanupPlan, bool, error)
	Renew(context.Context, string, string, time.Duration) error
	AuthorizeItem(context.Context, string, int, string) (domain.RegistryCleanupItem, error)
	RecordItemResult(context.Context, string, int, string, domain.RegistryCleanupItemResult) error
	Finish(context.Context, string, string, bool, string) error
}

var _ CleanupCoordinator = (*Service)(nil)

type CleanupExecutorConfig struct {
	LeaseDuration             time.Duration
	LeaseRenewInterval        time.Duration
	MaintenanceCleanupTimeout time.Duration
}

func DefaultCleanupExecutorConfig() CleanupExecutorConfig {
	return CleanupExecutorConfig{
		LeaseDuration:             30 * time.Minute,
		LeaseRenewInterval:        5 * time.Minute,
		MaintenanceCleanupTimeout: 30 * time.Second,
	}
}

func (c CleanupExecutorConfig) validate() error {
	if c.LeaseDuration < time.Minute || c.LeaseDuration > 24*time.Hour ||
		c.LeaseRenewInterval < time.Second || c.LeaseRenewInterval >= c.LeaseDuration/2 ||
		c.MaintenanceCleanupTimeout < time.Second || c.MaintenanceCleanupTimeout > 2*time.Minute {
		return ErrRegistryExecutionInvalid
	}
	return nil
}

// CleanupExecutor translates a claimed lifecycle plan into exact Distribution
// manifest deletions followed, when required, by one offline registry-wide GC
// sweep. It has no online blob-delete operation.
type CleanupExecutor struct {
	coordinator CleanupCoordinator
	deleter     ManifestDeleter
	maintenance RegistryMaintenanceAdapter
	checkpoints RegistryCheckpointProvider
	config      CleanupExecutorConfig
	target      domain.RegistryTarget
	now         func() time.Time
}

func NewCleanupExecutor(
	coordinator CleanupCoordinator,
	deleter ManifestDeleter,
	maintenance RegistryMaintenanceAdapter,
	checkpoints RegistryCheckpointProvider,
	config CleanupExecutorConfig,
) (*CleanupExecutor, error) {
	if coordinator == nil || deleter == nil || maintenance == nil || checkpoints == nil || config.validate() != nil {
		return nil, ErrRegistryExecutionInvalid
	}
	target := deleter.ManagedTarget()
	if target.Mode == domain.RegistryTargetExternal {
		return nil, store.ErrRegistryExternalLifecycle
	}
	if target.Mode != domain.RegistryTargetManaged || ValidateTarget(target) != nil || !validSafeIdentity(target.ID) || !validRepository(target.RepositoryPrefix) {
		return nil, ErrRegistryExecutionInvalid
	}
	return &CleanupExecutor{
		coordinator: coordinator,
		deleter:     deleter,
		maintenance: maintenance,
		checkpoints: checkpoints,
		config:      config,
		target:      target,
		now:         func() time.Time { return time.Now().UTC() },
	}, nil
}

func (e *CleanupExecutor) Execute(ctx context.Context, planID, owner string) error {
	if e == nil || !validSafeIdentity(planID) || !validSafeIdentity(owner) {
		return ErrRegistryExecutionInvalid
	}
	plan, _, err := e.coordinator.Claim(ctx, planID, owner, e.config.LeaseDuration)
	if err != nil {
		return err
	}
	if plan.ID != planID {
		_ = e.coordinator.Finish(ctx, planID, owner, false, "managed registry cleanup plan validation failed")
		return ErrRegistryExecutionInvalid
	}
	if err = e.validatePlan(plan); err != nil {
		_ = e.coordinator.Finish(ctx, planID, owner, false, "managed registry cleanup plan validation failed")
		return err
	}

	runContext, heartbeat := e.startHeartbeat(ctx, plan.ID, owner)
	fail := func(cause error, items ...domain.RegistryCleanupItem) error {
		heartbeatErr := heartbeat.stop()
		if heartbeatErr != nil {
			return heartbeatErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		message := executionFailureMessage(cause)
		for _, item := range items {
			if item.State != "deleting" {
				continue
			}
			if recordErr := e.coordinator.RecordItemResult(ctx, plan.ID, item.Ordinal, owner, domain.RegistryCleanupItemResult{
				State:           "failed",
				ProviderMessage: message,
				ObservedAt:      e.now().UTC(),
			}); recordErr != nil {
				return recordErr
			}
		}
		if finishErr := e.coordinator.Finish(ctx, plan.ID, owner, false, "managed registry cleanup execution failed"); finishErr != nil {
			return finishErr
		}
		return cause
	}

	for i := range plan.Items {
		item := &plan.Items[i]
		if item.Disposition != domain.RegistryCleanupDelete || item.ResourceKind == "blob" || item.State == "deleted" {
			continue
		}
		if err = heartbeat.err(); err != nil {
			return err
		}
		if item.State == "planned" {
			authorized, authorizeErr := e.coordinator.AuthorizeItem(runContext, plan.ID, item.Ordinal, owner)
			if authorizeErr != nil {
				return fail(authorizeErr)
			}
			if !sameCleanupItemIdentity(*item, authorized) || authorized.State != "deleting" {
				return fail(ErrRegistryExecutionInvalid)
			}
			*item = authorized
		}
		if item.State != "deleting" {
			return fail(ErrRegistryExecutionInvalid)
		}
		deleteResult, deleteErr := e.deleter.DeleteManifest(runContext, plan.RegistryTargetID, item.Repository, item.Digest)
		if deleteErr != nil {
			return fail(deleteErr, *item)
		}
		if deleteResult.TargetID != plan.RegistryTargetID || deleteResult.Repository != item.Repository || deleteResult.Digest != item.Digest ||
			(deleteResult.Outcome != ManifestDeleted && deleteResult.Outcome != ManifestAlreadyMissing) {
			return fail(ErrRegistryExecutionInvalid, *item)
		}
		providerMessage := "manifest digest delete confirmed"
		if deleteResult.Outcome == ManifestAlreadyMissing {
			providerMessage = "manifest digest already absent"
		}
		if err = e.coordinator.RecordItemResult(runContext, plan.ID, item.Ordinal, owner, domain.RegistryCleanupItemResult{
			State:           "deleted",
			ProviderMessage: providerMessage,
			ObservedAt:      e.now().UTC(),
		}); err != nil {
			return fail(err)
		}
		item.State = "deleted"
		item.ProviderMessage = providerMessage
		item.UpdatedAt = e.now().UTC()
	}

	manifestDeletesCompletedAt := e.now().UTC()
	blobItems := cleanupBlobItems(plan.Items)
	pendingBlobs := pendingCleanupItems(blobItems)
	if len(pendingBlobs) > 0 {
		for i := range plan.Items {
			item := &plan.Items[i]
			if item.Disposition != domain.RegistryCleanupDelete || item.ResourceKind != "blob" || item.State == "deleted" {
				continue
			}
			if item.State == "planned" {
				authorized, authorizeErr := e.coordinator.AuthorizeItem(runContext, plan.ID, item.Ordinal, owner)
				if authorizeErr != nil {
					return fail(authorizeErr)
				}
				if !sameCleanupItemIdentity(*item, authorized) || authorized.State != "deleting" {
					return fail(ErrRegistryExecutionInvalid)
				}
				*item = authorized
			}
			if item.State != "deleting" {
				return fail(ErrRegistryExecutionInvalid)
			}
		}
		blobItems = cleanupBlobItems(plan.Items)
		if err = e.runOfflineGC(runContext, plan, owner, manifestDeletesCompletedAt, blobItems); err != nil {
			if errors.Is(err, store.ErrRegistrySnapshotStale) {
				// This immutable plan can never become authoritative again. Mark
				// every unfinished blob terminal so the worker does not retry the
				// same stale sweep forever. No registry mutation occurred because
				// the maintenance adapter checks this before taking its lease.
				return fail(err, blobItems...)
			}
			// A sweep may have completed before a receipt or one of the durable
			// item records failed. Do not guess per-blob outcomes here; a fresh
			// plan/checkpoint (or an idempotent receipt replay) reconciles them.
			return fail(err)
		}
	}

	if err = heartbeat.stop(); err != nil {
		return err
	}
	if err = e.coordinator.Finish(ctx, plan.ID, owner, true, ""); err != nil {
		return err
	}
	return nil
}

func (e *CleanupExecutor) runOfflineGC(ctx context.Context, plan domain.RegistryCleanupPlan, owner string, notBefore time.Time, blobItems []domain.RegistryCleanupItem) (returnErr error) {
	digests := make([]string, 0, len(blobItems))
	for _, item := range blobItems {
		digests = append(digests, item.Digest)
	}
	candidateSetDigest, orderedDigests, err := cleanupCandidateSetDigest(digests)
	if err != nil {
		return err
	}
	executionKey, err := cleanupExecutionKey(plan.ID, plan.PlanDigest, candidateSetDigest)
	if err != nil {
		return err
	}
	acquire := MaintenanceAcquireRequest{TargetID: plan.RegistryTargetID, PlanID: plan.ID, ExecutionKey: executionKey, Owner: owner}
	session, err := e.maintenance.Acquire(ctx, acquire)
	if err != nil {
		if session != nil {
			cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.config.MaintenanceCleanupTimeout)
			_ = session.Release(cleanupContext)
			cancel()
		}
		return maintenanceError("acquire", err)
	}
	if session == nil {
		return ErrRegistryMaintenanceInvalid
	}
	entered := false
	restored := false
	released := false
	defer func() {
		if restored && released {
			return
		}
		cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.config.MaintenanceCleanupTimeout)
		defer cancel()
		if entered && !restored {
			_ = session.Restore(cleanupContext)
		}
		if !released {
			_ = session.Release(cleanupContext)
		}
	}()

	// Enter may fail after partially applying a stop/read-only transition, so
	// restoration is attempted even when it cannot return a valid proof.
	entered = true
	ready, err := session.Enter(ctx)
	if err != nil {
		return maintenanceError("enter", err)
	}
	if err = validateMaintenanceReady(ready, acquire, notBefore, e.now().UTC()); err != nil {
		return err
	}
	checkpointRequest := ReachabilityCheckpointRequest{
		TargetID:           plan.RegistryTargetID,
		PlanID:             plan.ID,
		PlanDigest:         plan.PlanDigest,
		ExecutionKey:       executionKey,
		CandidateSetDigest: candidateSetDigest,
		CandidateDigests:   orderedDigests,
		NotBefore:          ready.EnteredAt.UTC(),
	}
	providerCheckpointRequest := checkpointRequest
	providerCheckpointRequest.CandidateDigests = append([]string(nil), checkpointRequest.CandidateDigests...)
	checkpoint, err := e.checkpoints.Capture(ctx, providerCheckpointRequest)
	if err != nil {
		return fmt.Errorf("%w: checkpoint capture failed: %v", ErrRegistryCheckpointIncomplete, err)
	}
	if err = validateReachabilityCheckpoint(checkpoint, checkpointRequest, e.now().UTC()); err != nil {
		return err
	}
	sweepRequest := GCSweepRequest{
		TargetID:           plan.RegistryTargetID,
		PlanID:             plan.ID,
		ExecutionKey:       executionKey,
		CandidateSetDigest: candidateSetDigest,
		CandidateDigests:   append([]string(nil), orderedDigests...),
		Checkpoint:         checkpoint,
	}
	providerSweepRequest := sweepRequest
	providerSweepRequest.CandidateDigests = append([]string(nil), sweepRequest.CandidateDigests...)
	providerSweepRequest.Checkpoint.Blobs = append([]RegistryBlobReachability(nil), sweepRequest.Checkpoint.Blobs...)
	sweep, err := session.GarbageCollect(ctx, providerSweepRequest)
	if err != nil {
		return maintenanceError("garbage collection", err)
	}
	if err = validateGCSweepResult(sweep, sweepRequest, checkpoint.ObservedAt, e.now().UTC()); err != nil {
		return err
	}
	if err = session.Restore(ctx); err != nil {
		return maintenanceError("restore", err)
	}
	restored = true
	if err = session.Release(ctx); err != nil {
		return maintenanceError("release", err)
	}
	released = true
	for _, item := range blobItems {
		if item.State == "deleted" {
			continue
		}
		if err = e.coordinator.RecordItemResult(ctx, plan.ID, item.Ordinal, owner, domain.RegistryCleanupItemResult{
			State:           "deleted",
			ProviderMessage: "offline Distribution garbage-collection sweep confirmed",
			ObservedAt:      e.now().UTC(),
		}); err != nil {
			return err
		}
	}
	return nil
}

func (e *CleanupExecutor) validatePlan(plan domain.RegistryCleanupPlan) error {
	target := e.target
	if target.Mode != domain.RegistryTargetManaged || plan.State != "executing" || plan.ID == "" || plan.RegistryTargetID != target.ID ||
		!validSafeIdentity(plan.ServiceID) || plan.Policy.RegistryTargetID != target.ID || plan.Policy.ServiceID != plan.ServiceID ||
		!repositoryInTarget(target, plan.Policy.Repository) || !validDigest(plan.PlanDigest) {
		return ErrRegistryExecutionInvalid
	}
	progressed := false
	seen := make(map[string]struct{}, len(plan.Items))
	seenBlob := false
	deleteBlobs := 0
	for index, item := range plan.Items {
		if item.Ordinal != index || !validDigest(item.Digest) || item.EstimatedBytes < 0 {
			return ErrRegistryExecutionInvalid
		}
		key := item.Repository + "\x00" + item.Digest
		if _, exists := seen[key]; exists {
			return ErrRegistryExecutionInvalid
		}
		seen[key] = struct{}{}
		if item.ResourceKind == "blob" {
			seenBlob = true
			if item.Disposition == domain.RegistryCleanupDelete {
				deleteBlobs++
				if deleteBlobs > maximumMaintenanceCandidates {
					return ErrRegistryExecutionInvalid
				}
			}
			if item.Repository != "*" {
				return ErrRegistryExecutionInvalid
			}
		} else {
			if seenBlob || !repositoryInTarget(target, item.Repository) || (item.ResourceKind != "release-manifest" && item.ResourceKind != "cache-manifest") {
				return ErrRegistryExecutionInvalid
			}
		}
		switch item.Disposition {
		case domain.RegistryCleanupProtect:
			if item.Action != "none" || item.State != "protected" {
				return ErrRegistryExecutionInvalid
			}
		case domain.RegistryCleanupDelete:
			expectedAction := "delete-manifest"
			if item.ResourceKind == "blob" {
				expectedAction = "garbage-collect-blob"
			}
			if item.Action != expectedAction || (item.State != "planned" && item.State != "deleting" && item.State != "deleted") {
				return ErrRegistryExecutionInvalid
			}
			if item.State != "planned" {
				progressed = true
			}
		default:
			return ErrRegistryExecutionInvalid
		}
	}
	if !progressed && store.RegistryCleanupPlanDigest(plan) != plan.PlanDigest {
		return ErrRegistryExecutionInvalid
	}
	return nil
}

func repositoryInTarget(target domain.RegistryTarget, repository string) bool {
	if !validRepository(repository) {
		return false
	}
	prefix := strings.TrimSuffix(target.RepositoryPrefix, "/")
	return repository == prefix || strings.HasPrefix(repository, prefix+"/")
}

func sameCleanupItemIdentity(expected, actual domain.RegistryCleanupItem) bool {
	return expected.Ordinal == actual.Ordinal && expected.Repository == actual.Repository && expected.ResourceKind == actual.ResourceKind &&
		expected.Digest == actual.Digest && expected.Disposition == actual.Disposition && expected.Action == actual.Action && expected.EstimatedBytes == actual.EstimatedBytes
}

func cleanupBlobItems(items []domain.RegistryCleanupItem) []domain.RegistryCleanupItem {
	out := make([]domain.RegistryCleanupItem, 0)
	for _, item := range items {
		if item.ResourceKind == "blob" && item.Disposition == domain.RegistryCleanupDelete {
			out = append(out, item)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Digest < out[j].Digest })
	return out
}

func pendingCleanupItems(items []domain.RegistryCleanupItem) []domain.RegistryCleanupItem {
	out := make([]domain.RegistryCleanupItem, 0, len(items))
	for _, item := range items {
		if item.State != "deleted" {
			out = append(out, item)
		}
	}
	return out
}

func executionFailureMessage(err error) string {
	var provider *DistributionError
	if errors.As(err, &provider) {
		return "Distribution request failed: " + string(provider.Class)
	}
	switch {
	case errors.Is(err, ErrDistributionCredentialUnavailable):
		return "Distribution credential unavailable"
	case errors.Is(err, ErrDistributionManifestUnconfirmed):
		return "Distribution manifest deletion unconfirmed"
	case errors.Is(err, ErrRegistryCheckpointIncomplete):
		return "registry-wide checkpoint incomplete"
	case errors.Is(err, ErrRegistryGCSweepUnconfirmed):
		return "offline garbage-collection sweep unconfirmed"
	case errors.Is(err, ErrRegistryMaintenanceUnavailable):
		return "offline maintenance adapter unavailable"
	default:
		return "managed registry cleanup failed"
	}
}

type cleanupHeartbeat struct {
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	errVal error
}

func (e *CleanupExecutor) startHeartbeat(parent context.Context, planID, owner string) (context.Context, *cleanupHeartbeat) {
	ctx, cancel := context.WithCancel(parent)
	heartbeat := &cleanupHeartbeat{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(heartbeat.done)
		ticker := time.NewTicker(e.config.LeaseRenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				renewContext, renewCancel := context.WithTimeout(ctx, min(e.config.LeaseRenewInterval, 30*time.Second))
				err := e.coordinator.Renew(renewContext, planID, owner, e.config.LeaseDuration)
				renewCancel()
				if err != nil {
					heartbeat.mu.Lock()
					heartbeat.errVal = err
					heartbeat.mu.Unlock()
					cancel()
					return
				}
			}
		}
	}()
	return ctx, heartbeat
}

func (h *cleanupHeartbeat) err() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.errVal
}

func (h *cleanupHeartbeat) stop() error {
	h.cancel()
	<-h.done
	return h.err()
}
