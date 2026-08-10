package registry

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/store"
)

func digest(character string) string { return "sha256:" + strings.Repeat(character, 64) }

func fixtureSnapshot(now time.Time) domain.RegistryLifecycleSnapshot {
	targetID, serviceID := "11111111-1111-4111-8111-111111111111", "service-main"
	releaseRepo, cacheRepo, otherRepo := "owned/service", "owned/cache/service/trusted", "owned/other"
	old := now.Add(-30 * 24 * time.Hour)
	young := now.Add(-time.Hour)
	manifest := func(repository, value string, kind domain.RegistryManifestKind, observed time.Time) domain.RegistryManifest {
		return domain.RegistryManifest{RegistryTargetID: targetID, Repository: repository, Digest: digest(value), Kind: kind, MediaType: "application/vnd.oci.image.manifest.v1+json", SizeBytes: 10, Present: true, FirstObservedAt: observed, LastObservedAt: now, LastObservationRevision: 1}
	}
	blob := func(repository, value string, observed time.Time) domain.RegistryBlob {
		return domain.RegistryBlob{RegistryTargetID: targetID, Repository: repository, Digest: digest(value), SizeBytes: 100, Present: true, FirstObservedAt: observed, LastObservedAt: now, LastObservationRevision: 1}
	}
	manifests := []domain.RegistryManifest{
		manifest(releaseRepo, "a", domain.RegistryManifestIndex, old),
		manifest(releaseRepo, "b", domain.RegistryManifestImage, old),
		manifest(releaseRepo, "c", domain.RegistryManifestImage, old),
		manifest(releaseRepo, "d", domain.RegistryManifestImage, old),
		manifest(releaseRepo, "e", domain.RegistryManifestImage, young),
		manifest(cacheRepo, "f", domain.RegistryManifestImage, old),
		manifest(cacheRepo, "0", domain.RegistryManifestImage, old),
		manifest(cacheRepo, "1", domain.RegistryManifestImage, old),
		manifest(otherRepo, "2", domain.RegistryManifestImage, old),
	}
	blobs := []domain.RegistryBlob{
		blob(releaseRepo, "3", old), blob(releaseRepo, "4", old),
		blob(releaseRepo, "5", old), blob(releaseRepo, "6", young),
		blob(cacheRepo, "7", old), blob(cacheRepo, "8", old),
		blob(cacheRepo, "9", old), blob(otherRepo, "5", old),
	}
	succeeded := func(days int) *time.Time { value := now.Add(-time.Duration(days) * 24 * time.Hour); return &value }
	completed := now.Add(-29 * 24 * time.Hour)
	snapshot := domain.RegistryLifecycleSnapshot{
		Target:    domain.RegistryTarget{ID: targetID, Name: "managed", Mode: domain.RegistryTargetManaged, Endpoint: "registry.test", RepositoryPrefix: "owned", CreatedAt: old, UpdatedAt: old},
		Policy:    domain.ServiceRegistryPolicy{RegistryTargetID: targetID, ServiceID: serviceID, Repository: releaseRepo, KeepLastSuccessful: 2, MinimumSafetyAge: 24 * time.Hour, CacheKeepGenerations: 2, CacheUnusedExpiry: 7 * 24 * time.Hour, CacheByteQuota: 250, CreatedAt: old, UpdatedAt: old},
		Inventory: domain.RegistryInventoryObservation{RegistryTargetID: targetID, Revision: "inventory-1", Complete: true, Repositories: []string{releaseRepo, cacheRepo, otherRepo}, ObservedAt: now},
		AuthorityObservations: []domain.RegistryAuthorityObservation{
			{RegistryTargetID: targetID, ServiceID: serviceID, Authority: domain.RegistryAuthorityGitIntent, Revision: "git-1", Complete: true, ObservedAt: now},
			{RegistryTargetID: targetID, ServiceID: serviceID, Authority: domain.RegistryAuthorityRuntime, Revision: "runtime-1", Complete: true, ObservedAt: now},
			{RegistryTargetID: targetID, ServiceID: serviceID, Authority: domain.RegistryAuthorityOperations, Revision: "operations-1", Complete: true, ObservedAt: now},
		},
		References: []domain.RegistryArtifactReference{
			{RegistryTargetID: targetID, ServiceID: serviceID, Repository: releaseRepo, Digest: digest("a"), Kind: domain.RegistryReferenceCurrentGitIntent, ReferenceKey: "env/dev", ObservedAt: now},
			{RegistryTargetID: targetID, ServiceID: serviceID, Repository: releaseRepo, Digest: digest("a"), Kind: domain.RegistryReferenceObservedRunning, ReferenceKey: "pod/api", ObservedAt: now},
			{RegistryTargetID: targetID, ServiceID: serviceID, Repository: releaseRepo, Digest: digest("a"), Kind: domain.RegistryReferenceActiveOperation, ReferenceKey: "operation/build", ObservedAt: now},
			{RegistryTargetID: targetID, ServiceID: serviceID, Repository: releaseRepo, Digest: digest("a"), Kind: domain.RegistryReferencePin, ReferenceKey: "pin/prod", ObservedAt: now},
		},
		Releases: []domain.RegistryRelease{
			{ID: "release-a", RegistryTargetID: targetID, ServiceID: serviceID, Repository: releaseRepo, RootDigest: digest("a"), CreatedAt: old, SucceededAt: succeeded(1), Availability: domain.RegistryArtifactPresent},
			{ID: "release-c", RegistryTargetID: targetID, ServiceID: serviceID, Repository: releaseRepo, RootDigest: digest("c"), CreatedAt: old, SucceededAt: succeeded(2), Availability: domain.RegistryArtifactPresent},
			{ID: "release-d", RegistryTargetID: targetID, ServiceID: serviceID, Repository: releaseRepo, RootDigest: digest("d"), CreatedAt: old, SucceededAt: succeeded(3), Availability: domain.RegistryArtifactPresent},
		},
		CacheGenerations: []domain.RegistryCacheGeneration{
			{ID: "cache-3", RegistryTargetID: targetID, ServiceID: serviceID, Repository: cacheRepo, PlatformSet: "linux/amd64", TrustLane: "protected", CacheSchema: "v1", BuildDefinitionHash: "definition", Generation: 3, RootDigest: digest("f"), SizeBytes: 100, State: "succeeded", CreatedAt: old, CompletedAt: &completed, LastUsedAt: now.Add(-24 * time.Hour)},
			{ID: "cache-2", RegistryTargetID: targetID, ServiceID: serviceID, Repository: cacheRepo, PlatformSet: "linux/amd64", TrustLane: "protected", CacheSchema: "v1", BuildDefinitionHash: "definition", Generation: 2, RootDigest: digest("0"), SizeBytes: 100, State: "succeeded", CreatedAt: old, CompletedAt: &completed, LastUsedAt: now.Add(-48 * time.Hour)},
			{ID: "cache-1", RegistryTargetID: targetID, ServiceID: serviceID, Repository: cacheRepo, PlatformSet: "linux/amd64", TrustLane: "protected", CacheSchema: "v1", BuildDefinitionHash: "definition", Generation: 1, RootDigest: digest("1"), SizeBytes: 100, State: "succeeded", CreatedAt: old, CompletedAt: &completed, LastUsedAt: now.Add(-10 * 24 * time.Hour)},
		},
		Manifests: manifests,
		Blobs:     blobs,
		Children:  []domain.RegistryManifestLink{{Repository: releaseRepo, ParentDigest: digest("a"), ChildDigest: digest("b")}},
		BlobLinks: []domain.RegistryManifestBlobLink{
			{Repository: releaseRepo, ManifestDigest: digest("b"), BlobDigest: digest("3")},
			{Repository: releaseRepo, ManifestDigest: digest("c"), BlobDigest: digest("4")},
			{Repository: releaseRepo, ManifestDigest: digest("d"), BlobDigest: digest("5")},
			{Repository: releaseRepo, ManifestDigest: digest("e"), BlobDigest: digest("6")},
			{Repository: cacheRepo, ManifestDigest: digest("f"), BlobDigest: digest("7")},
			{Repository: cacheRepo, ManifestDigest: digest("0"), BlobDigest: digest("8")},
			{Repository: cacheRepo, ManifestDigest: digest("1"), BlobDigest: digest("9")},
			{Repository: otherRepo, ManifestDigest: digest("2"), BlobDigest: digest("5")},
		},
		AsOf: now,
	}
	for _, repository := range snapshot.Inventory.Repositories {
		manifestCount, blobCount := 0, 0
		for _, item := range manifests {
			if item.Repository == repository {
				manifestCount++
			}
		}
		for _, item := range blobs {
			if item.Repository == repository {
				blobCount++
			}
		}
		snapshot.CatalogObservations = append(snapshot.CatalogObservations, domain.RegistryCatalogObservation{ID: repository, RegistryTargetID: targetID, Repository: repository, Revision: 1, Complete: true, ObservedAt: now, ManifestCount: manifestCount, BlobCount: blobCount})
	}
	return snapshot
}

func TestBuildCleanupPlanProtectsEveryAuthorityAndOCIReachability(t *testing.T) {
	now := time.Date(2026, 8, 9, 4, 0, 0, 0, time.UTC)
	plan, err := BuildCleanupPlan(fixtureSnapshot(now), now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	a := findItem(t, plan, "owned/service", digest("a"))
	for _, reason := range []string{ReasonCurrentGitIntent, ReasonObservedRunning, ReasonActiveOperation, ReasonPin, ReasonRetainedSuccessWindow} {
		if !slices.Contains(a.Reasons, reason) {
			t.Errorf("index reasons %v missing %s", a.Reasons, reason)
		}
	}
	b := findItem(t, plan, "owned/service", digest("b"))
	if b.Disposition != domain.RegistryCleanupProtect || !slices.Contains(b.Reasons, ReasonReachableManifest) {
		t.Fatalf("child=%#v", b)
	}
	if item := findItem(t, plan, "owned/service", digest("c")); item.Disposition != domain.RegistryCleanupProtect || !slices.Contains(item.Reasons, ReasonRetainedSuccessWindow) {
		t.Fatalf("retained=%#v", item)
	}
	if item := findItem(t, plan, "owned/service", digest("d")); item.Disposition != domain.RegistryCleanupDelete {
		t.Fatalf("old release=%#v", item)
	}
	if item := findItem(t, plan, "owned/service", digest("e")); item.Disposition != domain.RegistryCleanupProtect || !slices.Contains(item.Reasons, ReasonMinimumSafetyAge) {
		t.Fatalf("young=%#v", item)
	}
	if item := findItem(t, plan, "owned/cache/service/trusted", digest("1")); item.Disposition != domain.RegistryCleanupDelete || !slices.Contains(item.Reasons, ReasonCacheExpired) {
		t.Fatalf("cache=%#v", item)
	}
	if item := findItem(t, plan, "*", digest("5")); item.Disposition != domain.RegistryCleanupProtect || !slices.Contains(item.Reasons, ReasonReachableBlob) {
		t.Fatalf("cross-repository shared blob=%#v", item)
	}
	if item := findItem(t, plan, "*", digest("9")); item.Disposition != domain.RegistryCleanupDelete || !slices.Contains(item.Reasons, ReasonUnreachableBlob) {
		t.Fatalf("unreachable cache blob=%#v", item)
	}
	if !plan.Summary.CacheQuotaSatisfied || plan.Summary.CacheBytesBefore != 300 || plan.Summary.CacheBytesAfter != 200 {
		t.Fatalf("summary=%#v", plan.Summary)
	}
}

func TestBuildCleanupPlanIsDeterministicAcrossInputOrdering(t *testing.T) {
	now := time.Date(2026, 8, 9, 4, 0, 0, 0, time.UTC)
	first := fixtureSnapshot(now)
	second := fixtureSnapshot(now)
	slices.Reverse(second.Inventory.Repositories)
	slices.Reverse(second.CatalogObservations)
	slices.Reverse(second.AuthorityObservations)
	slices.Reverse(second.References)
	slices.Reverse(second.Releases)
	slices.Reverse(second.CacheGenerations)
	slices.Reverse(second.Manifests)
	slices.Reverse(second.Blobs)
	slices.Reverse(second.BlobLinks)
	planA, err := BuildCleanupPlan(first, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	planB, err := BuildCleanupPlan(second, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if planA.PlanDigest != planB.PlanDigest || !reflect.DeepEqual(planA.Items, planB.Items) {
		t.Fatalf("non-deterministic plans %s %s", planA.PlanDigest, planB.PlanDigest)
	}
}

func TestCacheQuotaCountsDuplicateRootOnceAndNeverDeletesRetainedRoot(t *testing.T) {
	now := time.Date(2026, 8, 9, 4, 0, 0, 0, time.UTC)
	snapshot := fixtureSnapshot(now)
	// An older generation record aliases the newest retained digest. Physical
	// registry usage and a deletion decision are both root-digest based.
	snapshot.CacheGenerations[2].RootDigest = digest("f")
	plan, err := BuildCleanupPlan(snapshot, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Summary.CacheBytesBefore != 200 || plan.Summary.CacheBytesAfter != 200 {
		t.Fatalf("duplicate root counted more than once: %#v", plan.Summary)
	}
	item := findItem(t, plan, "owned/cache/service/trusted", digest("f"))
	if item.Disposition != domain.RegistryCleanupProtect || !slices.Contains(item.Reasons, ReasonCacheRetained) {
		t.Fatalf("aliased retained root became deletable: %#v", item)
	}
}

func TestBuildCleanupPlanFailsClosedAndNeverPlansExternalLifecycle(t *testing.T) {
	now := time.Date(2026, 8, 9, 4, 0, 0, 0, time.UTC)
	tests := map[string]func(*domain.RegistryLifecycleSnapshot){
		"incomplete inventory": func(snapshot *domain.RegistryLifecycleSnapshot) { snapshot.Inventory.Complete = false },
		"missing runtime": func(snapshot *domain.RegistryLifecycleSnapshot) {
			snapshot.AuthorityObservations = snapshot.AuthorityObservations[:2]
		},
		"incomplete catalog": func(snapshot *domain.RegistryLifecycleSnapshot) { snapshot.CatalogObservations[0].Complete = false },
		"stale authority": func(snapshot *domain.RegistryLifecycleSnapshot) {
			snapshot.AuthorityObservations[0].ObservedAt = now.Add(-2 * time.Hour)
		},
		"missing protected digest": func(snapshot *domain.RegistryLifecycleSnapshot) { snapshot.References[0].Digest = digest("f") },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			snapshot := fixtureSnapshot(now)
			mutate(&snapshot)
			if _, err := BuildCleanupPlan(snapshot, now, time.Hour); !errors.Is(err, store.ErrRegistryObservationIncomplete) {
				t.Fatalf("err=%v", err)
			}
		})
	}
	external := fixtureSnapshot(now)
	external.Target.Mode = domain.RegistryTargetExternal
	if _, err := BuildCleanupPlan(external, now, time.Hour); !errors.Is(err, store.ErrRegistryExternalLifecycle) {
		t.Fatalf("external err=%v", err)
	}
}

func TestArtifactProblemCodeDistinguishesOwnership(t *testing.T) {
	release := domain.RegistryRelease{Availability: domain.RegistryArtifactExpired}
	managed := domain.RegistryTarget{Mode: domain.RegistryTargetManaged}
	external := domain.RegistryTarget{Mode: domain.RegistryTargetExternal}
	if got := ArtifactProblemCode(managed, release, false); got != domain.ProblemArtifactExpired {
		t.Fatal(got)
	}
	if got := ArtifactProblemCode(external, release, false); got != domain.ProblemArtifactMissing {
		t.Fatal(got)
	}
	release.Availability = domain.RegistryArtifactPresent
	if got := ArtifactProblemCode(managed, release, true); got != "" {
		t.Fatal(got)
	}
}

func TestRegistryTargetAndPolicyValidationCloseCredentialAndRepositoryScope(t *testing.T) {
	target := domain.RegistryTarget{ID: "registry-1", Name: "primary", Mode: domain.RegistryTargetManaged,
		Endpoint: "registry.example.test", RepositoryPrefix: "owned/team", PullCredentialRef: "runtime/pull",
		PushCredentialRef: "builder/push", CacheCredentialRef: "builder/cache"}
	policy := DefaultPolicy(target.ID, "service-1", "owned/team/service", time.Now().UTC())
	if err := ValidateTarget(target); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePolicyForTarget(target, policy); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*domain.RegistryTarget){
		"unsupported scheme":   func(value *domain.RegistryTarget) { value.Endpoint = "ftp://registry.example.test" },
		"endpoint credentials": func(value *domain.RegistryTarget) { value.Endpoint = "https://user:pass@registry.example.test" },
		"unsafe prefix":        func(value *domain.RegistryTarget) { value.RepositoryPrefix = "../other" },
		"raw credential":       func(value *domain.RegistryTarget) { value.PullCredentialRef = "raw token" },
		"shared pull push":     func(value *domain.RegistryTarget) { value.PullCredentialRef = value.PushCredentialRef },
		"shared push cache":    func(value *domain.RegistryTarget) { value.CacheCredentialRef = value.PushCredentialRef },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := target
			mutate(&candidate)
			if err := ValidateTarget(candidate); err == nil {
				t.Fatalf("unsafe target accepted: %#v", candidate)
			}
		})
	}
	outside := policy
	outside.Repository = "other/service"
	if err := ValidatePolicyForTarget(target, outside); err == nil {
		t.Fatal("repository outside target prefix was accepted")
	}
}

func TestValidateCatalogRejectsCycle(t *testing.T) {
	now := time.Now().UTC()
	target, repository := "target", "repo"
	snapshot := domain.RegistryCatalogSnapshot{
		Observation: domain.RegistryCatalogObservation{RegistryTargetID: target, Repository: repository, Revision: 1, Complete: true, ObservedAt: now, ManifestCount: 2},
		Manifests: []domain.RegistryManifest{
			{RegistryTargetID: target, Repository: repository, Digest: digest("a"), Kind: domain.RegistryManifestIndex},
			{RegistryTargetID: target, Repository: repository, Digest: digest("b"), Kind: domain.RegistryManifestIndex},
		},
		Children: []domain.RegistryManifestLink{{Repository: repository, ParentDigest: digest("a"), ChildDigest: digest("b")}, {Repository: repository, ParentDigest: digest("b"), ChildDigest: digest("a")}},
	}
	if err := ValidateCatalog(snapshot); !errors.Is(err, store.ErrRegistryGraphInvalid) {
		t.Fatalf("err=%v", err)
	}
}

func findItem(t *testing.T, plan domain.RegistryCleanupPlan, repository, value string) domain.RegistryCleanupItem {
	t.Helper()
	for _, item := range plan.Items {
		if item.Repository == repository && item.Digest == value {
			return item
		}
	}
	t.Fatalf("item %s %s not found", repository, value)
	return domain.RegistryCleanupItem{}
}
