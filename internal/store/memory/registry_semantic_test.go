package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/registry"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func seedRegistryWithExpiredRelease(t *testing.T) registrySeed {
	t.Helper()
	seed := seedManagedRegistry(t)
	expired := seed.oldRelease
	expired.ID, expired.RootDigest = "77777777-7777-4777-8777-777777777777", registryDigest("e")
	if _, _, err := seed.store.PutRegistryRelease(t.Context(), expired); err != nil {
		t.Fatal(err)
	}
	refreshRegistrySeed(t, &seed)
	return seed
}

func refreshRegistrySeed(t *testing.T, seed *registrySeed) {
	t.Helper()
	seed.now = seed.now.Add(time.Minute)
	seed.catalog.Observation.Revision++
	seed.catalog.Observation.ID += "-next"
	seed.catalog.Observation.ObservedAt = seed.now
	if err := seed.store.ReplaceRegistryCatalog(t.Context(), seed.catalog); err != nil {
		t.Fatal(err)
	}
	if err := seed.store.RecordRegistryInventory(t.Context(), domain.RegistryInventoryObservation{RegistryTargetID: seed.targetID, Revision: seed.catalog.Observation.ID, Complete: true, Repositories: []string{seed.repository}, ObservedAt: seed.now}); err != nil {
		t.Fatal(err)
	}
}

func TestRegistrySemanticClaimSurvivesObserverRefresh(t *testing.T) {
	seed := seedRegistryWithExpiredRelease(t)
	lifecycle := registry.NewService(seed.store, registry.WithClock(func() time.Time { return seed.now }))
	plan, err := lifecycle.Preview(t.Context(), seed.targetID, seed.serviceID)
	if err != nil {
		t.Fatal(err)
	}
	refreshRegistrySeed(t, &seed)
	claimed, won, err := lifecycle.Claim(t.Context(), plan.ID, "worker", time.Hour)
	if err != nil || !won || claimed.SnapshotToken != plan.SnapshotToken || claimed.AuthorityToken != plan.AuthorityToken {
		t.Fatalf("unchanged refresh claim: won=%v err=%v", won, err)
	}
	for _, item := range claimed.Items {
		if item.ResourceKind != "release-manifest" {
			continue
		}
		_, authErr := lifecycle.AuthorizeItem(t.Context(), plan.ID, item.Ordinal, "worker")
		if item.Digest == seed.oldRelease.RootDigest {
			if authErr != nil {
				t.Fatalf("approved old release denied: %v", authErr)
			}
		} else if !errors.Is(authErr, base.ErrConflict) {
			t.Fatalf("protected release authorization: %v", authErr)
		}
	}
}

func TestRegistryLegacyPlanVersionSurvivesRecoveryAndCheckpoints(t *testing.T) {
	for _, mode := range []string{"unclaimed", "refreshed-execution", "checkpoint", "offline-recovery"} {
		t.Run(mode, func(t *testing.T) {
			seed := seedRegistryWithExpiredRelease(t)
			lifecycle := registry.NewService(seed.store, registry.WithClock(func() time.Time { return seed.now }))
			plan, err := lifecycle.Preview(t.Context(), seed.targetID, seed.serviceID)
			if err != nil {
				t.Fatal(err)
			}
			if mode != "unclaimed" {
				plan, _, err = lifecycle.Claim(t.Context(), plan.ID, "worker", time.Hour)
				if err != nil {
					t.Fatal(err)
				}
			}
			snapshot, err := seed.store.RegistryLifecycleSnapshot(t.Context(), seed.targetID, seed.serviceID, seed.now)
			if err != nil {
				t.Fatal(err)
			}
			plan.SnapshotToken = "sha256:" + strings.Repeat("a", 64)
			plan.AuthorityToken = base.RegistryAuthorityTokenForPlan(snapshot, plan)
			plan.PlanDigest = base.RegistryCleanupPlanDigest(plan)
			seed.store.registryPlans[plan.ID] = clonePlan(plan)
			if mode == "unclaimed" {
				_, won, claimErr := lifecycle.Claim(t.Context(), plan.ID, "worker", time.Hour)
				current, _ := seed.store.RegistryCleanupPlan(t.Context(), plan.ID)
				if !errors.Is(claimErr, base.ErrRegistrySnapshotStale) || won || current.State != "superseded" || current.SnapshotToken != plan.SnapshotToken {
					t.Fatalf("legacy preview reauthorized: won=%v state=%s err=%v", won, current.State, claimErr)
				}
				return
			}
			if mode == "offline-recovery" {
				// Existing failed-sweep recovery reacquires the exact approved blob set;
				// no fresh snapshot or token conversion is permitted on this path.
				plan.State, plan.Failure = "failed", "offline sweep failed"
				for i := range plan.Items {
					item := &plan.Items[i]
					if item.Disposition != domain.RegistryCleanupDelete {
						continue
					}
					item.State = "deleted"
					if item.ResourceKind == "blob" {
						item.State = "failed"
					}
				}
				seed.store.registryPlans[plan.ID] = clonePlan(plan)
				recovered, won, claimErr := lifecycle.Claim(t.Context(), plan.ID, "worker", time.Hour)
				if claimErr != nil || !won || recovered.State != "executing" || recovered.SnapshotToken != plan.SnapshotToken || recovered.AuthorityToken != plan.AuthorityToken {
					t.Fatalf("legacy recovery checkpoint changed: won=%v err=%v", won, claimErr)
				}
				return
			}
			if mode == "refreshed-execution" {
				refreshRegistrySeed(t, &seed)
			}
			for _, item := range plan.Items {
				if item.ResourceKind != "release-manifest" || item.Digest != seed.oldRelease.RootDigest {
					continue
				}
				_, authErr := lifecycle.AuthorizeItem(t.Context(), plan.ID, item.Ordinal, "worker")
				if mode == "refreshed-execution" {
					if !errors.Is(authErr, base.ErrRegistrySnapshotStale) {
						t.Fatalf("legacy approval silently normalized: %v", authErr)
					}
					return
				}
				if authErr != nil {
					t.Fatal(authErr)
				}
				if err = lifecycle.RecordItemResult(context.Background(), plan.ID, item.Ordinal, "worker", domain.RegistryCleanupItemResult{State: "deleted", ObservedAt: seed.now}); err != nil {
					t.Fatal(err)
				}
				current, _ := seed.store.RegistryCleanupPlan(t.Context(), plan.ID)
				snapshot, _ = seed.store.RegistryLifecycleSnapshot(t.Context(), seed.targetID, seed.serviceID, seed.now)
				if current.SnapshotToken != plan.SnapshotToken || current.AuthorityToken != base.RegistryAuthorityTokenForPlan(snapshot, plan) || current.AuthorityToken == base.RegistryAuthorityToken(snapshot) {
					t.Fatal("legacy item checkpoint changed token semantics")
				}
				return
			}
			t.Fatal("fixture did not contain expected deletion item")
		})
	}
}

func TestRegistrySemanticClaimValidatesFreshnessInsideStore(t *testing.T) {
	for _, mode := range []string{"inventory", "catalog", "authority"} {
		t.Run(mode, func(t *testing.T) {
			seed := seedRegistryWithExpiredRelease(t)
			plan, err := registry.NewService(seed.store, registry.WithClock(func() time.Time { return seed.now })).Preview(t.Context(), seed.targetID, seed.serviceID)
			if err != nil {
				t.Fatal(err)
			}
			stale := seed.now.Add(-time.Hour)
			switch mode {
			case "inventory":
				err = seed.store.RecordRegistryInventory(t.Context(), domain.RegistryInventoryObservation{RegistryTargetID: seed.targetID, Revision: "backdated", Complete: true, Repositories: []string{seed.repository}, ObservedAt: stale})
			case "catalog":
				seed.catalog.Observation.ID += "-backdated"
				seed.catalog.Observation.Revision++
				seed.catalog.Observation.ObservedAt = stale
				err = seed.store.ReplaceRegistryCatalog(t.Context(), seed.catalog)
			case "authority":
				err = seed.store.ReplaceRegistryProtectionSnapshot(t.Context(), domain.RegistryProtectionSnapshot{Observation: domain.RegistryAuthorityObservation{RegistryTargetID: seed.targetID, ServiceID: seed.serviceID, Authority: domain.RegistryAuthorityRuntime, Revision: "backdated", Complete: true, ObservedAt: stale}})
			}
			if err != nil {
				t.Fatal(err)
			}
			// Bypass the service intentionally: the store lock is the final freshness fence.
			_, won, claimErr := seed.store.ClaimRegistryCleanupPlan(t.Context(), plan.ID, "worker", seed.now, time.Hour, 15*time.Minute)
			current, _ := seed.store.RegistryCleanupPlan(t.Context(), plan.ID)
			if !errors.Is(claimErr, base.ErrRegistryObservationIncomplete) || won || current.ClaimedAt != nil || len(seed.store.registryLeases) != 0 {
				t.Fatalf("stale snapshot reached execution: won=%v err=%v", won, claimErr)
			}
		})
	}
}
