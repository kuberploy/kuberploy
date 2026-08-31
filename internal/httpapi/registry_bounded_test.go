package httpapi

import (
	"fmt"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

func TestSafeRegistryViewsBoundEveryRepeatedCollection(t *testing.T) {
	now := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	snapshot := domain.RegistryLifecycleSnapshot{
		Target: domain.RegistryTarget{ID: "target", Mode: domain.RegistryTargetManaged},
		Inventory: domain.RegistryInventoryObservation{
			RegistryTargetID: "target",
			Revision:         "inventory-1",
			Complete:         true,
			ObservedAt:       now,
		},
		AsOf: now,
	}
	for index := 104; index >= 0; index-- {
		repository := fmt.Sprintf("repository-%03d", index)
		snapshot.Inventory.Repositories = append(snapshot.Inventory.Repositories, repository)
		snapshot.CatalogObservations = append(snapshot.CatalogObservations, domain.RegistryCatalogObservation{Repository: repository})
		snapshot.Releases = append(snapshot.Releases, domain.RegistryRelease{ID: repository, Repository: repository, CreatedAt: now.Add(time.Duration(index) * time.Second)})
		snapshot.CacheGenerations = append(snapshot.CacheGenerations, domain.RegistryCacheGeneration{ID: repository, Repository: repository, Generation: int64(index), LastUsedAt: now.Add(time.Duration(index) * time.Second)})
	}

	view := safeRegistryApplicationTarget(snapshot, 7)
	if view.Inventory == nil || len(view.Inventory.Repositories) != 7 || !view.Inventory.RepositoriesTruncated {
		t.Fatalf("inventory repositories were not bounded: %#v", view.Inventory)
	}
	if view.Inventory.Repositories[0] != "repository-000" || view.Inventory.Repositories[6] != "repository-006" {
		t.Fatalf("inventory repositories were not deterministically sorted: %#v", view.Inventory.Repositories)
	}
	if len(view.CatalogObservations) != 7 || !view.CatalogTruncated || len(view.Releases) != 7 || !view.ReleasesTruncated || len(view.CacheGenerations) != 7 || !view.CacheGenerationsTruncated {
		t.Fatalf("nested registry collections were not bounded: catalogs=%d/%t releases=%d/%t caches=%d/%t", len(view.CatalogObservations), view.CatalogTruncated, len(view.Releases), view.ReleasesTruncated, len(view.CacheGenerations), view.CacheGenerationsTruncated)
	}

	plan := domain.RegistryCleanupPlan{ID: "plan", CreatedAt: now}
	for index := 104; index >= 0; index-- {
		plan.Items = append(plan.Items, domain.RegistryCleanupItem{Ordinal: index, UpdatedAt: now})
	}
	planView := safeRegistryCleanupPlan(plan)
	if len(planView.Items) != maximumRegistryCleanupItems || !planView.ItemsTruncated {
		t.Fatalf("cleanup items were not bounded: %d/%t", len(planView.Items), planView.ItemsTruncated)
	}
	if planView.Items[0].Ordinal != 0 || planView.Items[maximumRegistryCleanupItems-1].Ordinal != maximumRegistryCleanupItems-1 {
		t.Fatalf("cleanup items were not deterministically ordered: first=%d last=%d", planView.Items[0].Ordinal, planView.Items[maximumRegistryCleanupItems-1].Ordinal)
	}
}

func TestSafeRegistryApplicationTargetExcludesOtherApplicationRepositories(t *testing.T) {
	now := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	imageRepository := "managed/projects/current/apps/current/image"
	cacheRepository := "managed/projects/current/apps/current/cache/current"
	unrelatedRepository := "managed/projects/other/apps/other/image"
	snapshot := domain.RegistryLifecycleSnapshot{
		Target: domain.RegistryTarget{ID: "target", Mode: domain.RegistryTargetManaged},
		Policy: domain.ServiceRegistryPolicy{Repository: imageRepository},
		Inventory: domain.RegistryInventoryObservation{
			RegistryTargetID: "target", Complete: true, ObservedAt: now,
			Repositories: []string{unrelatedRepository, cacheRepository, imageRepository},
		},
		CatalogObservations: []domain.RegistryCatalogObservation{
			{Repository: unrelatedRepository, Complete: true},
			{Repository: cacheRepository, Complete: true},
			{Repository: imageRepository, Complete: true},
		},
		Releases:         []domain.RegistryRelease{{Repository: imageRepository}},
		CacheGenerations: []domain.RegistryCacheGeneration{{Repository: cacheRepository}},
		AsOf:             now,
	}

	view := safeRegistryApplicationTarget(snapshot, 50)
	if view.Inventory == nil || len(view.Inventory.Repositories) != 2 || view.Inventory.RepositoriesTruncated {
		t.Fatalf("application inventory was not exactly scoped: %#v", view.Inventory)
	}
	if view.Inventory.Repositories[0] != cacheRepository || view.Inventory.Repositories[1] != imageRepository {
		t.Fatalf("application repositories=%#v", view.Inventory.Repositories)
	}
	if len(view.CatalogObservations) != 2 || view.CatalogTruncated {
		t.Fatalf("application catalogs=%#v truncated=%t", view.CatalogObservations, view.CatalogTruncated)
	}
	for _, observation := range view.CatalogObservations {
		if observation.Repository == unrelatedRepository {
			t.Fatalf("unrelated repository leaked into application view: %#v", observation)
		}
	}
}
