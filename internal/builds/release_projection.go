package builds

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	registrycore "github.com/kuberploy/kuberploy/internal/registry"
	platformstore "github.com/kuberploy/kuberploy/internal/store"
)

type ReleaseProjectionState string

const (
	ReleaseProjectionPending    ReleaseProjectionState = "pending"
	ReleaseProjectionProcessing ReleaseProjectionState = "processing"
	ReleaseProjectionSucceeded  ReleaseProjectionState = "succeeded"
	ReleaseProjectionFailed     ReleaseProjectionState = "failed"
)

type ReleaseProjectionLease struct {
	AttemptID string
	Owner     string
	Epoch     int64
	Until     time.Time
}

type ReleaseProjectionWork struct {
	Attempt    BuildAttempt
	Definition BuildDefinition
	Lease      ReleaseProjectionLease
	Attempts   int
}

type ReleaseProjectionStore interface {
	ClaimNextReleaseProjection(context.Context, string, time.Time, time.Duration) (ReleaseProjectionWork, error)
	HeartbeatReleaseProjection(context.Context, ReleaseProjectionLease, time.Time, time.Duration) (ReleaseProjectionLease, error)
	RetryReleaseProjection(context.Context, ReleaseProjectionLease, string, time.Time, time.Time) (bool, error)
	FailReleaseProjection(context.Context, ReleaseProjectionLease, string, time.Time) error
	CompleteReleaseProjection(context.Context, ReleaseProjectionLease, string, string, time.Time) error
}

type ReleaseProjectionRegistry interface {
	RegistryTarget(context.Context, string) (domain.RegistryTarget, error)
	ServiceRegistryPolicy(context.Context, string, string) (domain.ServiceRegistryPolicy, error)
	PutRegistryRelease(context.Context, domain.RegistryRelease) (domain.RegistryRelease, bool, error)
	PutRegistryCacheGeneration(context.Context, domain.RegistryCacheGeneration) (domain.RegistryCacheGeneration, bool, error)
}

// ReleaseProjector turns an already verified immutable build result into the
// registry lifecycle records used by rollback and managed retention. Every
// identity is derived from the persisted attempt/definition; no webhook or
// tenant input is consulted again at this boundary.
type ReleaseProjector struct {
	Store         ReleaseProjectionStore
	Registry      ReleaseProjectionRegistry
	Owner         string
	LeaseDuration time.Duration
	Now           func() time.Time
}

type ReleaseProjectionResult struct {
	AttemptID string
	State     ReleaseProjectionState
	RetryAt   time.Time
}

func (p *ReleaseProjector) ReconcileNext(ctx context.Context) (ReleaseProjectionResult, error) {
	if p == nil || p.Store == nil || p.Registry == nil || !validOwnerLease(p.Owner, p.LeaseDuration) {
		return ReleaseProjectionResult{}, ErrInvalid
	}
	now := p.now()
	work, err := p.Store.ClaimNextReleaseProjection(ctx, p.Owner, now, p.LeaseDuration)
	if err != nil {
		return ReleaseProjectionResult{}, err
	}
	result := ReleaseProjectionResult{AttemptID: work.Attempt.ID, State: ReleaseProjectionProcessing}
	if err = validateReleaseProjectionWork(work); err != nil {
		return p.fail(ctx, work, "build-release-input-invalid", err)
	}

	target, err := p.Registry.RegistryTarget(ctx, work.Definition.Spec.Registry.TargetID)
	if err != nil {
		return p.handle(ctx, work, "registry-target-unavailable", err)
	}
	policy, err := p.Registry.ServiceRegistryPolicy(ctx, target.ID, work.Attempt.ServiceID)
	if err != nil {
		return p.handle(ctx, work, "registry-policy-unavailable", err)
	}
	if err = validateProjectionBinding(work, target, policy); err != nil {
		return p.fail(ctx, work, "build-release-binding-invalid", err)
	}

	lease, err := p.Store.HeartbeatReleaseProjection(ctx, work.Lease, p.now(), p.LeaseDuration)
	if err != nil {
		return result, err
	}
	work.Lease = lease
	completed := work.Attempt.CompletedAt.UTC()
	imageRepository, err := targetRepository(work.Attempt.PlanRequest.Build.Destination.Repository, work.Definition.Spec.Registry.Server)
	if err != nil || imageRepository != policy.Repository {
		return p.fail(ctx, work, "build-release-binding-invalid", ErrInvalid)
	}
	release := domain.RegistryRelease{
		ID: work.Attempt.ID, RegistryTargetID: target.ID, ServiceID: work.Attempt.ServiceID,
		Repository: imageRepository, RootDigest: work.Attempt.Result.Image.Digest,
		CreatedAt: work.Attempt.CreatedAt.UTC(), SucceededAt: &completed,
		Availability: domain.RegistryArtifactPresent,
	}
	if _, _, err = p.Registry.PutRegistryRelease(ctx, release); err != nil {
		return p.handle(ctx, work, "registry-release-write-failed", err)
	}

	cacheID := ""
	if work.Attempt.Result.Cache != nil {
		cache, cacheErr := cacheGenerationForProjection(work, target, completed)
		if cacheErr != nil {
			return p.fail(ctx, work, "build-cache-input-invalid", cacheErr)
		}
		lease, err = p.Store.HeartbeatReleaseProjection(ctx, work.Lease, p.now(), p.LeaseDuration)
		if err != nil {
			return result, err
		}
		work.Lease = lease
		if _, _, err = p.Registry.PutRegistryCacheGeneration(ctx, cache); err != nil {
			return p.handle(ctx, work, "registry-cache-write-failed", err)
		}
		cacheID = cache.ID
	}

	lease, err = p.Store.HeartbeatReleaseProjection(ctx, work.Lease, p.now(), p.LeaseDuration)
	if err != nil {
		return result, err
	}
	if err = p.Store.CompleteReleaseProjection(ctx, lease, release.ID, cacheID, p.now()); err != nil {
		return result, err
	}
	result.State = ReleaseProjectionSucceeded
	return result, nil
}

func validateReleaseProjectionWork(work ReleaseProjectionWork) error {
	if work.Attempts < 1 || work.Attempts > 20 || work.Lease.AttemptID != work.Attempt.ID || work.Lease.Epoch < 1 ||
		work.Attempt.State != AttemptSucceeded || work.Attempt.Result == nil || work.Attempt.CompletedAt == nil ||
		validateResultForAttempt(*work.Attempt.Result, work.Attempt) != nil || work.Definition.validate() != nil ||
		work.Definition.ID != work.Attempt.DefinitionID || work.Definition.ProjectID != work.Attempt.ProjectID ||
		work.Definition.ServiceID != work.Attempt.ServiceID || work.Definition.DefinitionDigest != work.Attempt.DefinitionDigest {
		return ErrInvalid
	}
	return nil
}

func validateProjectionBinding(work ReleaseProjectionWork, target domain.RegistryTarget, policy domain.ServiceRegistryPolicy) error {
	definition := work.Definition.Spec.Registry
	server, serverErr := registryServer(target.Endpoint)
	expectedMode := domain.RegistryTargetManaged
	if definition.Mode == RegistryExternal {
		expectedMode = domain.RegistryTargetExternal
	}
	if serverErr != nil || registrycore.ValidateTarget(target) != nil || target.ID != definition.TargetID || target.Mode != expectedMode ||
		server != definition.Server || target.RepositoryPrefix != definition.RepositoryPrefix ||
		target.PushCredentialRef != definition.PushCredentialSecret ||
		target.CacheCredentialRef != definition.CacheCredentialSecret ||
		target.PushCredentialRef == target.CacheCredentialRef ||
		policy.RegistryTargetID != target.ID || policy.ServiceID != work.Attempt.ServiceID {
		return ErrInvalid
	}
	if policy.Repository == "" || !strings.HasPrefix(policy.Repository, target.RepositoryPrefix+"/") {
		return ErrInvalid
	}
	return nil
}

func cacheGenerationForProjection(work ReleaseProjectionWork, target domain.RegistryTarget, completed time.Time) (domain.RegistryCacheGeneration, error) {
	cache := work.Attempt.Result.Cache
	if cache == nil || cache.Reference != work.Attempt.CacheReference || cache.Digest == "" {
		return domain.RegistryCacheGeneration{}, ErrInvalid
	}
	fullRepository, err := taggedRepository(cache.Reference)
	if err != nil {
		return domain.RegistryCacheGeneration{}, ErrInvalid
	}
	repository, err := targetRepository(fullRepository, work.Definition.Spec.Registry.Server)
	if err != nil || !strings.HasPrefix(repository, target.RepositoryPrefix+"/") {
		return domain.RegistryCacheGeneration{}, ErrInvalid
	}
	platforms := slices.Clone(work.Attempt.Result.Image.Platforms)
	slices.Sort(platforms)
	if len(platforms) == 0 || work.Attempt.PlanRequest.Build.Cache.Schema == "" || work.Attempt.PlanRequest.Build.Cache.TrustLane == "" {
		return domain.RegistryCacheGeneration{}, ErrInvalid
	}
	return domain.RegistryCacheGeneration{
		ID: work.Attempt.ID, RegistryTargetID: target.ID, ServiceID: work.Attempt.ServiceID,
		Repository: repository, PlatformSet: strings.Join(platforms, ","),
		TrustLane: work.Attempt.PlanRequest.Build.Cache.TrustLane, CacheSchema: work.Attempt.PlanRequest.Build.Cache.Schema,
		BuildDefinitionHash: work.Attempt.DefinitionDigest, Generation: work.Attempt.Generation,
		RootDigest: cache.Digest, SizeBytes: 0, State: "succeeded",
		CreatedAt: work.Attempt.CreatedAt.UTC(), CompletedAt: &completed, LastUsedAt: completed,
	}, nil
}

func taggedRepository(reference string) (string, error) {
	lastSlash := strings.LastIndexByte(reference, '/')
	lastColon := strings.LastIndexByte(reference, ':')
	if lastSlash < 1 || lastColon <= lastSlash+1 || lastColon == len(reference)-1 || strings.Contains(reference, "@") {
		return "", ErrInvalid
	}
	repository := reference[:lastColon]
	if _, err := registryServer(repository[:strings.IndexByte(repository, '/')]); err != nil {
		return "", ErrInvalid
	}
	return repository, nil
}

func targetRepository(full, server string) (string, error) {
	prefix := server + "/"
	if server == "" || !strings.HasPrefix(full, prefix) || len(full) == len(prefix) {
		return "", ErrInvalid
	}
	repository := strings.TrimPrefix(full, prefix)
	if strings.HasPrefix(repository, "/") || strings.HasSuffix(repository, "/") || strings.Contains(repository, "//") {
		return "", ErrInvalid
	}
	return repository, nil
}

func (p *ReleaseProjector) handle(ctx context.Context, work ReleaseProjectionWork, code string, cause error) (ReleaseProjectionResult, error) {
	if permanentProjectionError(cause) {
		return p.fail(ctx, work, code, cause)
	}
	retryAt := p.now().Add(retryDelay(work.Attempts))
	retry, err := p.Store.RetryReleaseProjection(ctx, work.Lease, code, p.now(), retryAt)
	if err != nil {
		return ReleaseProjectionResult{AttemptID: work.Attempt.ID, State: ReleaseProjectionProcessing}, err
	}
	state := ReleaseProjectionFailed
	if retry {
		state = ReleaseProjectionPending
	}
	return ReleaseProjectionResult{AttemptID: work.Attempt.ID, State: state, RetryAt: retryAt}, cause
}

func (p *ReleaseProjector) fail(ctx context.Context, work ReleaseProjectionWork, code string, cause error) (ReleaseProjectionResult, error) {
	if err := p.Store.FailReleaseProjection(ctx, work.Lease, code, p.now()); err != nil {
		return ReleaseProjectionResult{AttemptID: work.Attempt.ID, State: ReleaseProjectionProcessing}, err
	}
	return ReleaseProjectionResult{AttemptID: work.Attempt.ID, State: ReleaseProjectionFailed}, cause
}

func permanentProjectionError(err error) bool {
	return errors.Is(err, ErrInvalid) || errors.Is(err, ErrConflict) || errors.Is(err, platformstore.ErrNotFound) ||
		errors.Is(err, platformstore.ErrConflict) || errors.Is(err, platformstore.ErrRegistryPolicyInvalid) ||
		errors.Is(err, platformstore.ErrRegistryExternalLifecycle)
}

func (p *ReleaseProjector) now() time.Time {
	if p.Now == nil {
		return time.Now().UTC()
	}
	return p.Now().UTC()
}

func projectionLeaseError(lease ReleaseProjectionLease) error {
	return fmt.Errorf("%w: release projection lease %d was lost", ErrLeaseLost, lease.Epoch)
}
