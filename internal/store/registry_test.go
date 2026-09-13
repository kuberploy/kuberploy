package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

func registryTokenFixture(t *testing.T) domain.RegistryLifecycleSnapshot {
	t.Helper()
	now := time.Date(2026, 9, 13, 7, 0, 0, 0, time.UTC)
	return domain.RegistryLifecycleSnapshot{
		Target:                domain.RegistryTarget{ID: "target", Name: "managed", Mode: domain.RegistryTargetManaged, Endpoint: "https://registry.example.test", RepositoryPrefix: "tenant", CreatedAt: now, UpdatedAt: now},
		Policy:                domain.ServiceRegistryPolicy{RegistryTargetID: "target", ServiceID: "app", Repository: "tenant/app", KeepLastSuccessful: 1, MinimumSafetyAge: time.Hour, CacheUnusedExpiry: time.Hour, CacheByteQuota: 100, CreatedAt: now, UpdatedAt: now},
		Inventory:             domain.RegistryInventoryObservation{RegistryTargetID: "target", Revision: "inventory-1", Complete: true, Repositories: []string{"tenant/app"}, ObservedAt: now},
		CatalogObservations:   []domain.RegistryCatalogObservation{{ID: "catalog-1", RegistryTargetID: "target", Repository: "tenant/app", Revision: 1, Complete: true, SnapshotDigest: "catalog-digest", ObservedAt: now, ManifestCount: 1, BlobCount: 1}},
		AuthorityObservations: []domain.RegistryAuthorityObservation{{RegistryTargetID: "target", ServiceID: "app", Authority: domain.RegistryAuthorityRuntime, Revision: "authority-1", Complete: true, SnapshotDigest: "authority-digest", ObservedAt: now}},
		References:            []domain.RegistryArtifactReference{{RegistryTargetID: "target", ServiceID: "app", Repository: "tenant/app", Digest: "root", Kind: domain.RegistryReferenceObservedRunning, ReferenceKey: "deployment", SourceRevision: "git-1", CreatedAt: now, ObservedAt: now}},
		Releases:              []domain.RegistryRelease{{ID: "release", RegistryTargetID: "target", ServiceID: "app", Repository: "tenant/app", RootDigest: "expired-root", CreatedAt: now, SucceededAt: &now, Availability: domain.RegistryArtifactExpired, AvailabilityObservedAt: &now}},
		CacheGenerations:      []domain.RegistryCacheGeneration{{ID: "cache", RegistryTargetID: "target", ServiceID: "app", Repository: "tenant/app", RootDigest: "cache-root", State: "succeeded", CreatedAt: now, LastUsedAt: now}},
		Manifests:             []domain.RegistryManifest{{RegistryTargetID: "target", Repository: "tenant/app", Digest: "root", Kind: domain.RegistryManifestImage, MediaType: "manifest", SizeBytes: 20, Present: true, FirstObservedAt: now, LastObservedAt: now, LastObservationRevision: 1}},
		Blobs:                 []domain.RegistryBlob{{RegistryTargetID: "target", Repository: "tenant/app", Digest: "blob", MediaType: "layer", SizeBytes: 10, Present: true, FirstObservedAt: now, LastObservedAt: now, LastObservationRevision: 1}},
		Children:              []domain.RegistryManifestLink{{Repository: "tenant/app", ParentDigest: "index", ChildDigest: "root"}},
		BlobLinks:             []domain.RegistryManifestBlobLink{{Repository: "tenant/app", ManifestDigest: "root", BlobDigest: "blob"}},
	}
}

func cloneRegistryTokenFixture(t *testing.T, snapshot domain.RegistryLifecycleSnapshot) domain.RegistryLifecycleSnapshot {
	t.Helper()
	body, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var result domain.RegistryLifecycleSnapshot
	if err = json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestRegistryTokensIgnoreUnchangedObserverPublication(t *testing.T) {
	before := registryTokenFixture(t)
	after := cloneRegistryTokenFixture(t, before)
	now := before.Inventory.ObservedAt.Add(5 * time.Minute)
	after.Inventory.Revision, after.Inventory.ObservedAt = "inventory-2", now
	after.CatalogObservations[0].ID, after.CatalogObservations[0].Revision = "catalog-2", 2
	after.CatalogObservations[0].SnapshotDigest, after.CatalogObservations[0].ObservedAt = "new-observation-digest", now
	after.AuthorityObservations[0].Revision, after.AuthorityObservations[0].SnapshotDigest, after.AuthorityObservations[0].ObservedAt = "authority-2", "new-authority-observation-digest", now
	after.References[0].ObservedAt = now
	after.Manifests[0].LastObservedAt, after.Manifests[0].LastObservationRevision = now, 2
	after.Blobs[0].LastObservedAt, after.Blobs[0].LastObservationRevision = now, 2
	after.Releases[0].AvailabilityObservedAt = &now
	for name, token := range map[string]func(domain.RegistryLifecycleSnapshot) string{"snapshot": RegistrySnapshotToken, "authority": RegistryAuthorityToken} {
		t.Run(name, func(t *testing.T) {
			if token(before) != token(after) {
				t.Fatal("unchanged observer publication invalidated the cleanup token")
			}
		})
	}
}

func TestRegistrySnapshotTokenPreservesCleanupAuthorityAndGraph(t *testing.T) {
	before := registryTokenFixture(t)
	baseline := RegistrySnapshotToken(before)
	cases := map[string]func(*domain.RegistryLifecycleSnapshot){
		"target origin": func(s *domain.RegistryLifecycleSnapshot) { s.Target.Endpoint += "/changed" },
		"repository membership": func(s *domain.RegistryLifecycleSnapshot) {
			s.Inventory.Repositories = append(s.Inventory.Repositories, "tenant/other")
		},
		"inventory completeness": func(s *domain.RegistryLifecycleSnapshot) { s.Inventory.Complete = false },
		"catalog completeness":   func(s *domain.RegistryLifecycleSnapshot) { s.CatalogObservations[0].Complete = false },
		"catalog count":          func(s *domain.RegistryLifecycleSnapshot) { s.CatalogObservations[0].ManifestCount++ },
		"authority completeness": func(s *domain.RegistryLifecycleSnapshot) { s.AuthorityObservations[0].Complete = false },
		"root identity":          func(s *domain.RegistryLifecycleSnapshot) { s.References[0].Digest = "other" },
		"root source revision":   func(s *domain.RegistryLifecycleSnapshot) { s.References[0].SourceRevision = "git-2" },
		"root creation time": func(s *domain.RegistryLifecycleSnapshot) {
			s.References[0].CreatedAt = s.References[0].CreatedAt.Add(time.Second)
		},
		"release availability": func(s *domain.RegistryLifecycleSnapshot) { s.Releases[0].Availability = domain.RegistryArtifactPresent },
		"release ordering": func(s *domain.RegistryLifecycleSnapshot) {
			s.Releases[0].CreatedAt = s.Releases[0].CreatedAt.Add(time.Second)
		},
		"cache usage": func(s *domain.RegistryLifecycleSnapshot) {
			s.CacheGenerations[0].LastUsedAt = s.CacheGenerations[0].LastUsedAt.Add(time.Second)
		},
		"active cache":      func(s *domain.RegistryLifecycleSnapshot) { s.CacheGenerations[0].ActiveImports++ },
		"policy":            func(s *domain.RegistryLifecycleSnapshot) { s.Policy.KeepLastSuccessful++ },
		"manifest bytes":    func(s *domain.RegistryLifecycleSnapshot) { s.Manifests[0].SizeBytes++ },
		"manifest identity": func(s *domain.RegistryLifecycleSnapshot) { s.Manifests[0].Digest = "other" },
		"manifest first observation": func(s *domain.RegistryLifecycleSnapshot) {
			s.Manifests[0].FirstObservedAt = s.Manifests[0].FirstObservedAt.Add(time.Second)
		},
		"blob bytes": func(s *domain.RegistryLifecycleSnapshot) { s.Blobs[0].SizeBytes++ },
		"blob first observation": func(s *domain.RegistryLifecycleSnapshot) {
			s.Blobs[0].FirstObservedAt = s.Blobs[0].FirstObservedAt.Add(time.Second)
		},
		"manifest edge": func(s *domain.RegistryLifecycleSnapshot) { s.Children[0].ChildDigest = "other" },
		"blob edge":     func(s *domain.RegistryLifecycleSnapshot) { s.BlobLinks[0].BlobDigest = "other" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			changed := cloneRegistryTokenFixture(t, before)
			mutate(&changed)
			if RegistrySnapshotToken(changed) == baseline {
				t.Fatal("changed cleanup input retained its approval token")
			}
		})
	}
	if RegistrySnapshotToken(before) != baseline {
		t.Fatal("token calculation modified the caller's snapshot")
	}
}

func TestRegistryAuthorityCalculationRemainsBoundToPlanVersion(t *testing.T) {
	before := registryTokenFixture(t)
	after := cloneRegistryTokenFixture(t, before)
	refreshedAt := before.Inventory.ObservedAt.Add(time.Minute)
	after.Releases[0].AvailabilityObservedAt = &refreshedAt
	legacy := domain.RegistryCleanupPlan{SnapshotToken: digestJSON(canonicalRegistrySnapshot(before))}
	semantic := domain.RegistryCleanupPlan{SnapshotToken: RegistrySnapshotToken(before)}
	if RegistryCleanupUsesSemanticSnapshot(legacy) || !RegistryCleanupUsesSemanticSnapshot(semantic) {
		t.Fatal("plan versions were not distinguished")
	}
	if RegistryAuthorityTokenForPlan(before, legacy) == RegistryAuthorityTokenForPlan(after, legacy) {
		t.Fatal("legacy execution silently acquired semantic authorization")
	}
	if RegistryAuthorityTokenForPlan(before, semantic) != RegistryAuthorityTokenForPlan(after, semantic) {
		t.Fatal("new execution rejected unchanged availability observation")
	}
	if RegistryAuthorityTokenForPlan(before, legacy) == RegistryAuthorityToken(before) {
		t.Fatal("legacy checkpoint was rewritten with the new calculation")
	}
	before.Releases = nil
	if RegistryAuthorityTokenForPlan(before, legacy) == RegistryAuthorityToken(before) {
		t.Fatal("older binary could authorize a new plan without expired releases")
	}
}

func TestRegistryAuthorityTokenIgnoresObservationRefreshButNotProtectionChange(t *testing.T) {
	now := time.Date(2026, 8, 13, 5, 0, 0, 0, time.UTC)
	targetID := "11111111-1111-4111-8111-111111111111"
	serviceID := "22222222-2222-4222-8222-222222222222"
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	snapshot := domain.RegistryLifecycleSnapshot{
		Target: domain.RegistryTarget{ID: targetID, Name: "managed", Mode: domain.RegistryTargetManaged,
			Endpoint: "https://registry.example.test", RepositoryPrefix: "tenant", CreatedAt: now, UpdatedAt: now},
		Policy: domain.ServiceRegistryPolicy{RegistryTargetID: targetID, ServiceID: serviceID,
			Repository: "tenant/service/image", KeepLastSuccessful: 1, CreatedAt: now, UpdatedAt: now},
		Inventory: domain.RegistryInventoryObservation{RegistryTargetID: targetID, Revision: "inventory-1",
			Complete: true, Repositories: []string{"tenant/service/image"}, ObservedAt: now},
		CatalogObservations: []domain.RegistryCatalogObservation{{RegistryTargetID: targetID,
			Repository: "tenant/service/image", Revision: 1, Complete: true, SnapshotDigest: digest, ObservedAt: now}},
		AuthorityObservations: []domain.RegistryAuthorityObservation{{RegistryTargetID: targetID, ServiceID: serviceID,
			Authority: domain.RegistryAuthorityRuntime, Revision: "registry-protection-v1:content:1", Complete: true,
			SnapshotDigest: digest, ObservedAt: now}},
		References: []domain.RegistryArtifactReference{{RegistryTargetID: targetID, ServiceID: serviceID,
			Repository: "tenant/service/image", Digest: digest, Kind: domain.RegistryReferenceObservedRunning,
			ReferenceKey: "deployment/one", SourceRevision: "revision-one", CreatedAt: now.Add(-time.Minute), ObservedAt: now}},
	}
	baseline := RegistryAuthorityToken(snapshot)

	refreshed := snapshot
	refreshed.Inventory.Revision = "inventory-2"
	refreshed.Inventory.ObservedAt = now.Add(time.Minute)
	refreshed.CatalogObservations = append([]domain.RegistryCatalogObservation(nil), snapshot.CatalogObservations...)
	refreshed.CatalogObservations[0].Revision = 2
	refreshed.CatalogObservations[0].ObservedAt = now.Add(time.Minute)
	refreshed.AuthorityObservations = append([]domain.RegistryAuthorityObservation(nil), snapshot.AuthorityObservations...)
	refreshed.AuthorityObservations[0].Revision = "registry-protection-v1:content:2"
	refreshed.AuthorityObservations[0].ObservedAt = now.Add(time.Minute)
	refreshed.References = append([]domain.RegistryArtifactReference(nil), snapshot.References...)
	refreshed.References[0].ObservedAt = now.Add(time.Minute)
	if token := RegistryAuthorityToken(refreshed); token != baseline {
		t.Fatalf("unchanged observation refresh changed authority token: %s != %s", token, baseline)
	}

	changed := refreshed
	changed.References = append([]domain.RegistryArtifactReference(nil), refreshed.References...)
	changed.References[0].SourceRevision = "revision-two"
	if RegistryAuthorityToken(changed) == baseline {
		t.Fatal("source revision substitution did not change authority token")
	}
	changed = refreshed
	changed.AuthorityObservations = append([]domain.RegistryAuthorityObservation(nil), refreshed.AuthorityObservations...)
	changed.AuthorityObservations[0].Complete = false
	if RegistryAuthorityToken(changed) == baseline {
		t.Fatal("incomplete authority did not change authority token")
	}
}

func TestRegistryCleanupPlanCanResumeOnlyUnfinishedOfflineSweep(t *testing.T) {
	plan := domain.RegistryCleanupPlan{
		State:   "failed",
		Failure: "managed registry cleanup execution failed",
		Items: []domain.RegistryCleanupItem{
			{ResourceKind: "release-manifest", Disposition: domain.RegistryCleanupDelete, Action: "delete-manifest", State: "deleted"},
			{ResourceKind: "blob", Disposition: domain.RegistryCleanupDelete, Action: "garbage-collect-blob", State: "deleting"},
			{ResourceKind: "blob", Disposition: domain.RegistryCleanupProtect, Action: "none", State: "protected"},
		},
	}
	if !RegistryCleanupPlanCanResumeOfflineSweep(plan) {
		t.Fatal("exact unfinished offline sweep was not recoverable")
	}
	failedBlob := plan
	failedBlob.Items = append([]domain.RegistryCleanupItem(nil), plan.Items...)
	failedBlob.Items[1].State = "failed"
	if !RegistryCleanupPlanCanResumeOfflineSweep(failedBlob) {
		t.Fatal("terminal failed offline sweep was not recoverable")
	}

	cases := map[string]func(*domain.RegistryCleanupPlan){
		"non-terminal":        func(plan *domain.RegistryCleanupPlan) { plan.State = "executing" },
		"missing failure":     func(plan *domain.RegistryCleanupPlan) { plan.Failure = "" },
		"planned candidate":   func(plan *domain.RegistryCleanupPlan) { plan.Items[1].State = "planned" },
		"skipped candidate":   func(plan *domain.RegistryCleanupPlan) { plan.Items[1].State = "skipped" },
		"manifest unfinished": func(plan *domain.RegistryCleanupPlan) { plan.Items[0].State = "deleting" },
		"wrong blob action":   func(plan *domain.RegistryCleanupPlan) { plan.Items[1].Action = "delete-manifest" },
		"nothing unfinished":  func(plan *domain.RegistryCleanupPlan) { plan.Items[1].State = "deleted" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			candidate := plan
			candidate.Items = append([]domain.RegistryCleanupItem(nil), plan.Items...)
			mutate(&candidate)
			if RegistryCleanupPlanCanResumeOfflineSweep(candidate) {
				t.Fatal("unsafe failed plan was recoverable")
			}
		})
	}
}
