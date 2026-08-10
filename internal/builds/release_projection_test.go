package builds

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/builder"
	"github.com/kuberploy/kuberploy/internal/buildpromotion"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/registry"
	storememory "github.com/kuberploy/kuberploy/internal/store/memory"
)

func TestReleaseProjectorRegistersBuildAndCacheExactlyOnce(t *testing.T) {
	ctx := context.Background()
	buildStore, definition, attempt, completed := completedProjectionAttempt(t, RegistryManaged, true)
	lifecycle := projectionRegistry(t, definition, attempt, domain.RegistryTargetManaged, completed)
	projector := &ReleaseProjector{Store: buildStore, Registry: lifecycle, Owner: "release-projector", LeaseDuration: time.Minute, Now: func() time.Time { return completed.Add(time.Second) }}

	result, err := projector.ReconcileNext(ctx)
	if err != nil || result.State != ReleaseProjectionSucceeded || result.AttemptID != attempt.ID {
		t.Fatalf("projection result=%#v err=%v", result, err)
	}
	release, err := lifecycle.RegistryRelease(ctx, attempt.ID)
	if err != nil || release.RootDigest != attempt.Result.Image.Digest || release.Repository != targetLocalRepository(t, attempt.PlanRequest.Build.Destination.Repository, definition.Spec.Registry.Server) || release.SucceededAt == nil || !release.SucceededAt.Equal(completed) {
		t.Fatalf("release=%#v err=%v", release, err)
	}
	snapshot, err := lifecycle.RegistryLifecycleSnapshot(ctx, definition.Spec.Registry.TargetID, attempt.ServiceID, completed)
	if err != nil || len(snapshot.CacheGenerations) != 1 {
		t.Fatalf("cache snapshot=%#v err=%v", snapshot.CacheGenerations, err)
	}
	cache := snapshot.CacheGenerations[0]
	if cache.ID != attempt.ID || cache.RootDigest != attempt.Result.Cache.Digest || cache.Generation != attempt.Generation || cache.PlatformSet != "linux/amd64,linux/arm64" || cache.SizeBytes != 0 {
		t.Fatalf("cache=%#v", cache)
	}
	if _, err = projector.ReconcileNext(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("completed projection was reclaimed: %v", err)
	}
}

func TestSuccessfulReleaseProjectionOpensOnlyAfterExactProjection(t *testing.T) {
	ctx := context.Background()
	buildStore, definition, attempt, completed := completedProjectionAttempt(t, RegistryManaged, false)
	if _, err := buildStore.SuccessfulReleaseProjection(ctx, attempt.ID); !errors.Is(err, buildpromotion.ErrNotReady) {
		t.Fatalf("unprojected successful attempt was promotable: %v", err)
	}
	lifecycle := projectionRegistry(t, definition, attempt, domain.RegistryTargetManaged, completed)
	projector := &ReleaseProjector{Store: buildStore, Registry: lifecycle, Owner: "release-projector", LeaseDuration: time.Minute, Now: func() time.Time { return completed.Add(time.Second) }}
	if _, err := projector.ReconcileNext(ctx); err != nil {
		t.Fatal(err)
	}
	projected, err := buildStore.SuccessfulReleaseProjection(ctx, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantRepository := targetLocalRepository(t, attempt.PlanRequest.Build.Destination.Repository, definition.Spec.Registry.Server)
	if projected.AttemptID != attempt.ID || projected.ReleaseID != attempt.ID || projected.ProjectID != attempt.ProjectID ||
		projected.ApplicationID != attempt.ServiceID || projected.RegistryTargetID != definition.Spec.Registry.TargetID ||
		projected.Repository != wantRepository || projected.ImageReference != attempt.Result.Image.Reference ||
		projected.ImageDigest != attempt.Result.Image.Digest || !projected.CompletedAt.Equal(completed) ||
		!projected.ProjectionCompletedAt.Equal(completed.Add(time.Second)) {
		t.Fatalf("projection was not exact: %#v", projected)
	}
}

func TestReleaseProjectorRecordsExternalReleaseWithoutOwningLifecycle(t *testing.T) {
	ctx := context.Background()
	buildStore, definition, attempt, completed := completedProjectionAttempt(t, RegistryExternal, false)
	lifecycle := projectionRegistry(t, definition, attempt, domain.RegistryTargetExternal, completed)
	projector := &ReleaseProjector{Store: buildStore, Registry: lifecycle, Owner: "release-projector", LeaseDuration: time.Minute, Now: func() time.Time { return completed.Add(time.Second) }}
	if result, err := projector.ReconcileNext(ctx); err != nil || result.State != ReleaseProjectionSucceeded {
		t.Fatalf("external projection=%#v err=%v", result, err)
	}
	if _, err := lifecycle.RegistryRelease(ctx, attempt.ID); err != nil {
		t.Fatal(err)
	}
	snapshot, err := lifecycle.RegistryLifecycleSnapshot(ctx, definition.Spec.Registry.TargetID, attempt.ServiceID, completed)
	if err != nil || len(snapshot.CacheGenerations) != 0 {
		t.Fatalf("unexpected external cache generations=%#v err=%v", snapshot.CacheGenerations, err)
	}
}

func TestReleaseProjectorFailsClosedOnPolicyRebinding(t *testing.T) {
	ctx := context.Background()
	buildStore, definition, attempt, completed := completedProjectionAttempt(t, RegistryManaged, false)
	lifecycle := storememory.New()
	target := projectionTarget(definition, domain.RegistryTargetManaged, completed)
	if _, err := lifecycle.PutRegistryTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	policy := registry.DefaultPolicy(target.ID, attempt.ServiceID, target.RepositoryPrefix+"/attacker", completed)
	if _, err := lifecycle.PutServiceRegistryPolicy(ctx, policy); err != nil {
		t.Fatal(err)
	}
	projector := &ReleaseProjector{Store: buildStore, Registry: lifecycle, Owner: "release-projector", LeaseDuration: time.Minute, Now: func() time.Time { return completed.Add(time.Second) }}
	result, err := projector.ReconcileNext(ctx)
	if !errors.Is(err, ErrInvalid) || result.State != ReleaseProjectionFailed {
		t.Fatalf("rebound projection=%#v err=%v", result, err)
	}
	projection := buildStore.releaseProjections[attempt.ID]
	if projection.state != ReleaseProjectionFailed || projection.releaseID != "" {
		t.Fatalf("projection did not fail closed: %#v", projection)
	}
}

func TestReleaseProjectionRejectsCredentialAuthoritySubstitution(t *testing.T) {
	_, definition, attempt, completed := completedProjectionAttempt(t, RegistryManaged, false)
	target := projectionTarget(definition, domain.RegistryTargetManaged, completed)
	policy := registry.DefaultPolicy(target.ID, attempt.ServiceID,
		targetLocalRepository(t, attempt.PlanRequest.Build.Destination.Repository, definition.Spec.Registry.Server), completed)
	work := ReleaseProjectionWork{Attempt: attempt, Definition: definition}

	mutations := []struct {
		name   string
		mutate func(*domain.RegistryTarget)
	}{
		{name: "push revision changed", mutate: func(value *domain.RegistryTarget) { value.PushCredentialRef = "rotated-push" }},
		{name: "cache revision changed", mutate: func(value *domain.RegistryTarget) { value.CacheCredentialRef = "rotated-cache" }},
		{name: "cache aliases push", mutate: func(value *domain.RegistryTarget) { value.CacheCredentialRef = value.PushCredentialRef }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			candidate := target
			mutation.mutate(&candidate)
			if err := validateProjectionBinding(work, candidate, policy); !errors.Is(err, ErrInvalid) {
				t.Fatalf("credential substitution accepted: %v", err)
			}
		})
	}
}

func TestReleaseProjectionRepositoryParsingCannotCrossRegistry(t *testing.T) {
	for _, reference := range []string{
		"registry.test/kuberploy/cache@generation-1",
		"registry.test/kuberploy/cache",
		"registry.test:5000",
		"registry.test/kuberploy/cache:",
	} {
		if _, err := taggedRepository(reference); !errors.Is(err, ErrInvalid) {
			t.Fatalf("unsafe cache reference %q accepted: %v", reference, err)
		}
	}
	if _, err := targetRepository("attacker.test/kuberploy/service", "registry.test"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cross-registry repository accepted: %v", err)
	}
	if repository, err := targetRepository("registry.test:5000/kuberploy/service", "registry.test:5000"); err != nil || repository != "kuberploy/service" {
		t.Fatalf("registry port parsing repository=%q err=%v", repository, err)
	}
}

func completedProjectionAttempt(t *testing.T, mode RegistryMode, withCache bool) (*MemoryStore, BuildDefinition, BuildAttempt, time.Time) {
	t.Helper()
	ctx := context.Background()
	store, definition := seedMemory(t, mode)
	clock := testNow
	attempt := createAttempt(t, store, mode, &clock)
	claimed, err := store.ClaimNextAttempt(ctx, "build-worker", clock, time.Minute)
	if err != nil || claimed.ID != attempt.ID {
		t.Fatalf("claim=%#v err=%v", claimed, err)
	}
	if err = store.MarkAttemptRunning(ctx, claimed.ID, "build-worker", clock.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	completed := clock.Add(30 * time.Second)
	digest := "sha256:" + strings.Repeat("c", 64)
	result := builder.BuildResult{
		APIVersion: builder.ProtocolVersion, OperationID: claimed.ID, Generation: claimed.Generation, Status: "Succeeded",
		Image:    builder.Image{Reference: claimed.PlanRequest.Build.Destination.Repository + "@" + digest, Digest: digest, Platforms: claimed.PlanRequest.Build.Platforms},
		Warnings: []builder.Warning{}, StartedAt: clock.Add(time.Second), CompletedAt: completed,
	}
	cacheRef := ""
	if withCache {
		cacheRef = cacheReference(claimed)
		result.Cache = &builder.Cache{Reference: cacheRef, Digest: "sha256:" + strings.Repeat("d", 64)}
	}
	if err = store.CompleteAttempt(ctx, claimed.ID, "build-worker", BuildCompletion{Result: result, CacheReference: cacheRef, LogReference: "k8s://kuberploy-build-dind/pods/build-pod/containers/agent"}, completed); err != nil {
		t.Fatal(err)
	}
	attempt, err = store.Attempt(ctx, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	return store, definition, attempt, completed
}

func projectionRegistry(t *testing.T, definition BuildDefinition, attempt BuildAttempt, mode domain.RegistryTargetMode, now time.Time) *storememory.Store {
	t.Helper()
	ctx := context.Background()
	store := storememory.New()
	target := projectionTarget(definition, mode, now)
	if _, err := store.PutRegistryTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	repository := targetLocalRepository(t, attempt.PlanRequest.Build.Destination.Repository, definition.Spec.Registry.Server)
	if _, err := store.PutServiceRegistryPolicy(ctx, registry.DefaultPolicy(target.ID, attempt.ServiceID, repository, now)); err != nil {
		t.Fatal(err)
	}
	return store
}

func projectionTarget(definition BuildDefinition, mode domain.RegistryTargetMode, now time.Time) domain.RegistryTarget {
	return domain.RegistryTarget{
		ID: definition.Spec.Registry.TargetID, Name: "build-target", Mode: mode,
		Endpoint: definition.Spec.Registry.Server, RepositoryPrefix: definition.Spec.Registry.RepositoryPrefix,
		PushCredentialRef: definition.Spec.Registry.PushCredentialSecret, CacheCredentialRef: definition.Spec.Registry.CacheCredentialSecret,
		CreatedAt: now, UpdatedAt: now,
	}
}

func targetLocalRepository(t *testing.T, value, server string) string {
	t.Helper()
	repository, err := targetRepository(value, server)
	if err != nil {
		t.Fatal(err)
	}
	return repository
}
