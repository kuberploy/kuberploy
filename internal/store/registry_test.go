package store

import (
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

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

	cases := map[string]func(*domain.RegistryCleanupPlan){
		"non-terminal":        func(plan *domain.RegistryCleanupPlan) { plan.State = "executing" },
		"missing failure":     func(plan *domain.RegistryCleanupPlan) { plan.Failure = "" },
		"planned candidate":   func(plan *domain.RegistryCleanupPlan) { plan.Items[1].State = "planned" },
		"failed candidate":    func(plan *domain.RegistryCleanupPlan) { plan.Items[1].State = "failed" },
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
