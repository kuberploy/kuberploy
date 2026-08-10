package memory

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/registry"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func registryDigest(character string) string { return "sha256:" + strings.Repeat(character, 64) }

type registrySeed struct {
	store      *Store
	now        time.Time
	targetID   string
	serviceID  string
	repository string
	catalog    domain.RegistryCatalogSnapshot
	oldRelease domain.RegistryRelease
}

func seedManagedRegistry(t *testing.T) registrySeed {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 9, 5, 0, 0, 0, time.UTC)
	old := now.Add(-30 * 24 * time.Hour)
	targetID := "11111111-1111-4111-8111-111111111111"
	serviceID, repository := "main", "owned/service"
	st := New()
	if _, err := st.PutRegistryTarget(ctx, domain.RegistryTarget{ID: targetID, Name: "managed", Mode: domain.RegistryTargetManaged, Endpoint: "registry.test", RepositoryPrefix: "owned", CreatedAt: old, UpdatedAt: old}); err != nil {
		t.Fatal(err)
	}
	policy := registry.DefaultPolicy(targetID, serviceID, repository, old)
	policy.KeepLastSuccessful = 1
	if _, err := st.PutServiceRegistryPolicy(ctx, policy); err != nil {
		t.Fatal(err)
	}
	catalog := domain.RegistryCatalogSnapshot{
		Observation: domain.RegistryCatalogObservation{ID: "22222222-2222-4222-8222-222222222222", RegistryTargetID: targetID, Repository: repository, Revision: 1, Complete: true, ObservedAt: now, ManifestCount: 2, BlobCount: 2},
		Manifests: []domain.RegistryManifest{
			{RegistryTargetID: targetID, Repository: repository, Digest: registryDigest("a"), Kind: domain.RegistryManifestImage, MediaType: "application/vnd.oci.image.manifest.v1+json", SizeBytes: 10, FirstObservedAt: old},
			{RegistryTargetID: targetID, Repository: repository, Digest: registryDigest("b"), Kind: domain.RegistryManifestImage, MediaType: "application/vnd.oci.image.manifest.v1+json", SizeBytes: 10, FirstObservedAt: old},
		},
		Blobs: []domain.RegistryBlob{
			{RegistryTargetID: targetID, Repository: repository, Digest: registryDigest("c"), SizeBytes: 100, FirstObservedAt: old},
			{RegistryTargetID: targetID, Repository: repository, Digest: registryDigest("d"), SizeBytes: 100, FirstObservedAt: old},
		},
		BlobLinks: []domain.RegistryManifestBlobLink{
			{Repository: repository, ManifestDigest: registryDigest("a"), BlobDigest: registryDigest("c")},
			{Repository: repository, ManifestDigest: registryDigest("b"), BlobDigest: registryDigest("d")},
		},
	}
	if err := st.ReplaceRegistryCatalog(ctx, catalog); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordRegistryInventory(ctx, domain.RegistryInventoryObservation{RegistryTargetID: targetID, Revision: "inventory-1", Complete: true, Repositories: []string{repository}, ObservedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, authority := range []domain.RegistryAuthority{domain.RegistryAuthorityGitIntent, domain.RegistryAuthorityRuntime, domain.RegistryAuthorityOperations} {
		if err := st.ReplaceRegistryProtectionSnapshot(ctx, domain.RegistryProtectionSnapshot{Observation: domain.RegistryAuthorityObservation{RegistryTargetID: targetID, ServiceID: serviceID, Authority: authority, Revision: string(authority) + "-1", Complete: true, ObservedAt: now}}); err != nil {
			t.Fatal(err)
		}
	}
	latestAt, oldAt := now.Add(-24*time.Hour), now.Add(-48*time.Hour)
	latest := domain.RegistryRelease{ID: "33333333-3333-4333-8333-333333333333", RegistryTargetID: targetID, ServiceID: serviceID, Repository: repository, RootDigest: registryDigest("a"), CreatedAt: old, SucceededAt: &latestAt, Availability: domain.RegistryArtifactPresent}
	oldRelease := domain.RegistryRelease{ID: "44444444-4444-4444-8444-444444444444", RegistryTargetID: targetID, ServiceID: serviceID, Repository: repository, RootDigest: registryDigest("b"), CreatedAt: old, SucceededAt: &oldAt, Availability: domain.RegistryArtifactPresent}
	for _, release := range []domain.RegistryRelease{latest, oldRelease} {
		if _, _, err := st.PutRegistryRelease(ctx, release); err != nil {
			t.Fatal(err)
		}
	}
	return registrySeed{store: st, now: now, targetID: targetID, serviceID: serviceID, repository: repository, catalog: catalog, oldRelease: oldRelease}
}

func TestRegistryLifecycleConcurrencyAndIdempotency(t *testing.T) {
	seed := seedManagedRegistry(t)
	ctx := context.Background()
	var ids atomic.Int64
	lifecycle := registry.NewService(seed.store,
		registry.WithClock(func() time.Time { return seed.now }),
		registry.WithMaxObservationAge(time.Hour),
		registry.WithIDGenerator(func() string {
			value := ids.Add(1)
			if value == 1 {
				return "55555555-5555-4555-8555-555555555555"
			}
			return "66666666-6666-4666-8666-666666666666"
		}),
	)
	plan, err := lifecycle.Preview(ctx, seed.targetID, seed.serviceID)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := lifecycle.Preview(ctx, seed.targetID, seed.serviceID)
	if err != nil {
		t.Fatal(err)
	}
	if replay.ID != plan.ID || replay.PlanDigest != plan.PlanDigest {
		t.Fatalf("preview was not idempotent: %#v %#v", plan, replay)
	}

	const contenders = 12
	var claimed atomic.Int64
	errorsSeen := make(chan error, contenders)
	var wait sync.WaitGroup
	for range contenders {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, won, claimErr := lifecycle.Claim(ctx, plan.ID, "worker-a", 10*time.Minute)
			if won {
				claimed.Add(1)
			}
			errorsSeen <- claimErr
		}()
	}
	wait.Wait()
	close(errorsSeen)
	for claimErr := range errorsSeen {
		if claimErr != nil {
			t.Errorf("claim: %v", claimErr)
		}
	}
	if claimed.Load() != 1 {
		t.Fatalf("claims=%d", claimed.Load())
	}

	manifestOrdinal := deletionOrdinal(t, plan, "release-manifest")
	var authorized atomic.Int64
	authorizeErrors := make(chan error, contenders)
	for range contenders {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, authorizeErr := lifecycle.AuthorizeItem(ctx, plan.ID, manifestOrdinal, "worker-a")
			if authorizeErr == nil {
				authorized.Add(1)
			}
			authorizeErrors <- authorizeErr
		}()
	}
	wait.Wait()
	close(authorizeErrors)
	for authorizeErr := range authorizeErrors {
		if authorizeErr != nil && !errors.Is(authorizeErr, base.ErrConflict) {
			t.Errorf("authorize: %v", authorizeErr)
		}
	}
	if authorized.Load() != 1 {
		t.Fatalf("authorizations=%d", authorized.Load())
	}
	result := domain.RegistryCleanupItemResult{State: "deleted", ProviderMessage: "registry returned 202", ObservedAt: seed.now.Add(time.Second)}
	if err = lifecycle.RecordItemResult(ctx, plan.ID, manifestOrdinal, "worker-a", result); err != nil {
		t.Fatal(err)
	}
	if err = lifecycle.RecordItemResult(ctx, plan.ID, manifestOrdinal, "worker-a", result); err != nil {
		t.Fatalf("result replay: %v", err)
	}
	release, err := seed.store.RegistryRelease(ctx, seed.oldRelease.ID)
	if err != nil {
		t.Fatal(err)
	}
	if release.Availability != domain.RegistryArtifactExpired {
		t.Fatalf("availability=%s", release.Availability)
	}

	current, err := seed.store.RegistryCleanupPlan(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range current.Items {
		if item.Disposition != domain.RegistryCleanupDelete || item.State == "deleted" {
			continue
		}
		if _, err = seed.store.AuthorizeRegistryCleanupItem(ctx, plan.ID, item.Ordinal, "worker-a", seed.now.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		if err = seed.store.RecordRegistryCleanupItemResult(ctx, plan.ID, item.Ordinal, "worker-a", domain.RegistryCleanupItemResult{State: "deleted", ObservedAt: seed.now.Add(3 * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	if err = seed.store.FinishRegistryCleanupPlan(ctx, plan.ID, "worker-a", true, "", seed.now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = seed.store.FinishRegistryCleanupPlan(ctx, plan.ID, "worker-a", true, "", seed.now.Add(4*time.Second)); err != nil {
		t.Fatalf("finish replay: %v", err)
	}
}

func TestRegistryCleanupPlanReclaimsOnlyAfterEveryLeaseExpires(t *testing.T) {
	seed := seedManagedRegistry(t)
	ctx := context.Background()
	now := seed.now
	lifecycle := registry.NewService(seed.store,
		registry.WithClock(func() time.Time { return now }),
		registry.WithMaxObservationAge(time.Hour),
		registry.WithIDGenerator(func() string { return "56565656-5656-4565-8565-565656565656" }),
	)
	plan, err := lifecycle.Preview(ctx, seed.targetID, seed.serviceID)
	if err != nil {
		t.Fatal(err)
	}
	if _, claimed, claimErr := lifecycle.Claim(ctx, plan.ID, "worker-a", time.Minute); claimErr != nil || !claimed {
		t.Fatalf("initial claim claimed=%v err=%v", claimed, claimErr)
	}
	now = now.Add(30 * time.Second)
	if _, _, claimErr := lifecycle.Claim(ctx, plan.ID, "worker-b", time.Minute); !errors.Is(claimErr, base.ErrConflict) {
		t.Fatalf("live lease takeover err=%v", claimErr)
	}
	now = now.Add(31 * time.Second)
	recovered, claimed, err := lifecycle.Claim(ctx, plan.ID, "worker-b", time.Minute)
	if err != nil || !claimed || recovered.State != "executing" {
		t.Fatalf("expired lease recovery claimed=%v state=%q err=%v", claimed, recovered.State, err)
	}
	if err = lifecycle.Renew(ctx, plan.ID, "worker-a", time.Minute); !errors.Is(err, base.ErrRegistryLeaseLost) {
		t.Fatalf("stale owner renew err=%v", err)
	}
	if err = lifecycle.Renew(ctx, plan.ID, "worker-b", time.Minute); err != nil {
		t.Fatalf("new owner renew: %v", err)
	}
}

func TestRegistryPlanClaimFailsAfterProtectionChange(t *testing.T) {
	seed := seedManagedRegistry(t)
	ctx := context.Background()
	lifecycle := registry.NewService(seed.store, registry.WithClock(func() time.Time { return seed.now }), registry.WithMaxObservationAge(time.Hour), registry.WithIDGenerator(func() string { return "77777777-7777-4777-8777-777777777777" }))
	plan, err := lifecycle.Preview(ctx, seed.targetID, seed.serviceID)
	if err != nil {
		t.Fatal(err)
	}
	if err = seed.store.PutRegistryPin(ctx, domain.RegistryArtifactReference{RegistryTargetID: seed.targetID, ServiceID: seed.serviceID, Repository: seed.repository, Digest: seed.oldRelease.RootDigest, Kind: domain.RegistryReferencePin, ReferenceKey: "pin/prod", ObservedAt: seed.now}); err != nil {
		t.Fatal(err)
	}
	if _, _, err = lifecycle.Claim(ctx, plan.ID, "worker", time.Minute); !errors.Is(err, base.ErrRegistrySnapshotStale) {
		t.Fatalf("claim err=%v", err)
	}
}

func TestRegistryExternalModeCannotPersistLifecyclePlan(t *testing.T) {
	ctx := context.Background()
	st := New()
	targetID := "88888888-8888-4888-8888-888888888888"
	if _, err := st.PutRegistryTarget(ctx, domain.RegistryTarget{ID: targetID, Name: "external", Mode: domain.RegistryTargetExternal, Endpoint: "external.test", RepositoryPrefix: "tenant"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutServiceRegistryPolicy(ctx, registry.DefaultPolicy(targetID, "main", "tenant/main", time.Now())); err != nil {
		t.Fatal(err)
	}
	lifecycle := registry.NewService(st, registry.WithMaxObservationAge(0))
	if _, err := lifecycle.Preview(ctx, targetID, "main"); !errors.Is(err, base.ErrRegistryExternalLifecycle) {
		t.Fatalf("preview err=%v", err)
	}
	plan := domain.RegistryCleanupPlan{ID: "99999999-9999-4999-8999-999999999999", RegistryTargetID: targetID, ServiceID: "main", SnapshotToken: "snapshot", AuthorityToken: "authority", State: "preview"}
	plan.PlanDigest = base.RegistryCleanupPlanDigest(plan)
	if _, _, err := st.SaveRegistryCleanupPlan(ctx, plan); !errors.Is(err, base.ErrRegistryExternalLifecycle) {
		t.Fatalf("save err=%v", err)
	}
}

func TestRegistryCatalogAndReleaseWritesAreConcurrentIdempotent(t *testing.T) {
	seed := seedManagedRegistry(t)
	ctx := context.Background()
	const writers = 16
	var wait sync.WaitGroup
	errs := make(chan error, writers)
	for range writers {
		wait.Add(1)
		go func() { defer wait.Done(); errs <- seed.store.ReplaceRegistryCatalog(ctx, seed.catalog) }()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("catalog replay: %v", err)
		}
	}
	changed := seed.catalog
	changed.Manifests = append([]domain.RegistryManifest(nil), seed.catalog.Manifests...)
	changed.Manifests[0].SizeBytes++
	if err := seed.store.ReplaceRegistryCatalog(ctx, changed); !errors.Is(err, base.ErrConflict) {
		t.Fatalf("same revision mutation err=%v", err)
	}

	release := domain.RegistryRelease{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", RegistryTargetID: seed.targetID, ServiceID: seed.serviceID, Repository: seed.repository, RootDigest: registryDigest("a"), CreatedAt: seed.now, Availability: domain.RegistryArtifactPresent}
	errs = make(chan error, writers)
	var inserted atomic.Int64
	for range writers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, replay, err := seed.store.PutRegistryRelease(ctx, release)
			if err == nil && !replay {
				inserted.Add(1)
			}
			errs <- err
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("release replay: %v", err)
		}
	}
	if inserted.Load() != 1 {
		t.Fatalf("release inserts=%d", inserted.Load())
	}
}

func TestRegistryPolicyDefaultsAndBounds(t *testing.T) {
	ctx := context.Background()
	st := New()
	targetID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	if _, err := st.PutRegistryTarget(ctx, domain.RegistryTarget{ID: targetID, Name: "managed", Mode: domain.RegistryTargetManaged, Endpoint: "registry.test", RepositoryPrefix: "owned"}); err != nil {
		t.Fatal(err)
	}
	policy, err := st.PutServiceRegistryPolicy(ctx, domain.ServiceRegistryPolicy{RegistryTargetID: targetID, ServiceID: "main", Repository: "owned/main"})
	if err != nil {
		t.Fatal(err)
	}
	if policy.KeepLastSuccessful != 10 || policy.CacheKeepGenerations != 2 || policy.CacheUnusedExpiry != 7*24*time.Hour || policy.CacheByteQuota != 10<<30 {
		t.Fatalf("defaults=%#v", policy)
	}
	policy.KeepLastSuccessful = 101
	if _, err = st.PutServiceRegistryPolicy(ctx, policy); !errors.Is(err, base.ErrRegistryPolicyInvalid) {
		t.Fatalf("bound err=%v", err)
	}
}

func deletionOrdinal(t *testing.T, plan domain.RegistryCleanupPlan, kind string) int {
	t.Helper()
	for _, item := range plan.Items {
		if item.ResourceKind == kind && item.Disposition == domain.RegistryCleanupDelete {
			return item.Ordinal
		}
	}
	t.Fatalf("no deletion item of kind %s", kind)
	return -1
}
