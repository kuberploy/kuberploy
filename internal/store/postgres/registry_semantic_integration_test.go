package postgres

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/registry"
	base "github.com/kuberploy/kuberploy/internal/store"
)

type semanticRegistrySQLFixture struct {
	store                           *Store
	now                             time.Time
	targetID, serviceID, repository string
	catalog                         domain.RegistryCatalogSnapshot
}

func seedSemanticRegistrySQL(t *testing.T) *semanticRegistrySQLFixture {
	t.Helper()
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	st, err := Open(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	f := &semanticRegistrySQLFixture{store: st, now: databaseTime(time.Now()), targetID: id.New(), serviceID: "semantic-" + id.New()}
	f.repository = "integration/" + f.targetID + "/app"
	old := f.now.Add(-30 * 24 * time.Hour)
	if _, err = st.PutRegistryTarget(t.Context(), domain.RegistryTarget{ID: f.targetID, Name: f.targetID, Mode: domain.RegistryTargetManaged, Endpoint: "https://registry.integration.test", RepositoryPrefix: "integration", CreatedAt: old, UpdatedAt: old}); err != nil {
		t.Fatal(err)
	}
	policy := registry.DefaultPolicy(f.targetID, f.serviceID, f.repository, old)
	policy.KeepLastSuccessful = 1
	if _, err = st.PutServiceRegistryPolicy(t.Context(), policy); err != nil {
		t.Fatal(err)
	}
	f.catalog = domain.RegistryCatalogSnapshot{Observation: domain.RegistryCatalogObservation{ID: id.New(), RegistryTargetID: f.targetID, Repository: f.repository, Revision: 1, Complete: true, ObservedAt: f.now, ManifestCount: 2, BlobCount: 2},
		Manifests: []domain.RegistryManifest{
			{RegistryTargetID: f.targetID, Repository: f.repository, Digest: postgresRegistryDigest("a"), Kind: domain.RegistryManifestImage, MediaType: "application/vnd.oci.image.manifest.v1+json", SizeBytes: 10, FirstObservedAt: old},
			{RegistryTargetID: f.targetID, Repository: f.repository, Digest: postgresRegistryDigest("b"), Kind: domain.RegistryManifestImage, MediaType: "application/vnd.oci.image.manifest.v1+json", SizeBytes: 10, FirstObservedAt: old},
		},
		Blobs: []domain.RegistryBlob{
			{RegistryTargetID: f.targetID, Repository: f.repository, Digest: postgresRegistryDigest("c"), SizeBytes: 100, FirstObservedAt: old},
			{RegistryTargetID: f.targetID, Repository: f.repository, Digest: postgresRegistryDigest("d"), SizeBytes: 100, FirstObservedAt: old},
		},
		BlobLinks: []domain.RegistryManifestBlobLink{{Repository: f.repository, ManifestDigest: postgresRegistryDigest("a"), BlobDigest: postgresRegistryDigest("c")}, {Repository: f.repository, ManifestDigest: postgresRegistryDigest("b"), BlobDigest: postgresRegistryDigest("d")}},
	}
	if err = st.ReplaceRegistryCatalog(t.Context(), f.catalog); err != nil {
		t.Fatal(err)
	}
	for _, authority := range []domain.RegistryAuthority{domain.RegistryAuthorityGitIntent, domain.RegistryAuthorityRuntime, domain.RegistryAuthorityOperations} {
		if err = st.ReplaceRegistryProtectionSnapshot(t.Context(), domain.RegistryProtectionSnapshot{Observation: domain.RegistryAuthorityObservation{RegistryTargetID: f.targetID, ServiceID: f.serviceID, Authority: authority, Revision: string(authority) + "-1", Complete: true, ObservedAt: f.now}}); err != nil {
			t.Fatal(err)
		}
	}
	for index, value := range []string{"a", "b", "e"} {
		created := old.Add(-time.Duration(index) * time.Hour)
		if _, _, err = st.PutRegistryRelease(t.Context(), domain.RegistryRelease{ID: id.New(), RegistryTargetID: f.targetID, ServiceID: f.serviceID, Repository: f.repository, RootDigest: postgresRegistryDigest(value), CreatedAt: created, SucceededAt: &created, Availability: domain.RegistryArtifactPresent}); err != nil {
			t.Fatal(err)
		}
	}
	f.refresh(t)
	return f
}

func (f *semanticRegistrySQLFixture) refresh(t *testing.T) {
	t.Helper()
	f.now = f.now.Add(time.Minute)
	f.catalog.Observation.ID, f.catalog.Observation.Revision, f.catalog.Observation.ObservedAt = id.New(), f.catalog.Observation.Revision+1, f.now
	if err := f.store.ReplaceRegistryCatalog(t.Context(), f.catalog); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RecordRegistryInventory(t.Context(), domain.RegistryInventoryObservation{RegistryTargetID: f.targetID, Revision: f.catalog.Observation.ID, Complete: true, Repositories: []string{f.repository}, ObservedAt: f.now}); err != nil {
		t.Fatal(err)
	}
}

func (f *semanticRegistrySQLFixture) lifecycle() *registry.Service {
	return registry.NewService(f.store, registry.WithClock(func() time.Time { return f.now }))
}

func TestRegistrySemanticSQLClaimAfterObserverRefresh(t *testing.T) {
	f := seedSemanticRegistrySQL(t)
	lifecycle := f.lifecycle()
	plan, err := lifecycle.Preview(t.Context(), f.targetID, f.serviceID)
	if err != nil {
		t.Fatal(err)
	}
	f.refresh(t)
	claimed, won, err := lifecycle.Claim(t.Context(), plan.ID, "worker", time.Hour)
	if err != nil || !won || claimed.SnapshotToken != plan.SnapshotToken || claimed.AuthorityToken != plan.AuthorityToken {
		t.Fatalf("observer refresh claim: won=%v err=%v", won, err)
	}
	for _, item := range plan.Items {
		if item.ResourceKind != "release-manifest" {
			continue
		}
		_, authErr := lifecycle.AuthorizeItem(t.Context(), plan.ID, item.Ordinal, "worker")
		if item.Digest == postgresRegistryDigest("b") {
			if authErr != nil {
				t.Fatalf("approved old release denied: %v", authErr)
			}
		} else if !errors.Is(authErr, base.ErrConflict) {
			t.Fatalf("protected release authorized: %v", authErr)
		}
	}
}

func TestRegistrySemanticSQLChangedOrStaleInputsRejectClaim(t *testing.T) {
	for _, mode := range []string{"graph-bytes", "root-pin", "first-observation", "stale", "stale-inventory", "stale-catalog", "stale-authority"} {
		t.Run(mode, func(t *testing.T) {
			f := seedSemanticRegistrySQL(t)
			lifecycle := f.lifecycle()
			plan, err := lifecycle.Preview(t.Context(), f.targetID, f.serviceID)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "graph-bytes":
				f.catalog.Manifests[0].SizeBytes++
				f.refresh(t)
			case "root-pin":
				err = f.store.PutRegistryPin(t.Context(), domain.RegistryArtifactReference{RegistryTargetID: f.targetID, ServiceID: f.serviceID, Repository: f.repository, Digest: postgresRegistryDigest("b"), Kind: domain.RegistryReferencePin, ReferenceKey: "concurrent-pin", CreatedAt: f.now, ObservedAt: f.now})
			case "first-observation":
				_, err = f.store.pool.Exec(t.Context(), `UPDATE registry_manifests SET first_observed_at=first_observed_at+interval '1 second' WHERE registry_target_id=$1 AND repository=$2 AND digest=$3`, f.targetID, f.repository, postgresRegistryDigest("b"))
			case "stale":
				f.now = f.now.Add(time.Hour)
			case "stale-inventory":
				_, err = f.store.pool.Exec(t.Context(), `UPDATE registry_inventory_observations SET observed_at=$2 WHERE registry_target_id=$1`, f.targetID, f.now.Add(-time.Hour))
			case "stale-catalog":
				_, err = f.store.pool.Exec(t.Context(), `UPDATE registry_catalog_observations SET observed_at=$2 WHERE registry_target_id=$1`, f.targetID, f.now.Add(-time.Hour))
			case "stale-authority":
				_, err = f.store.pool.Exec(t.Context(), `UPDATE registry_authority_observations SET observed_at=$2 WHERE registry_target_id=$1`, f.targetID, f.now.Add(-time.Hour))
			}
			if err != nil {
				t.Fatal(err)
			}
			_, won, claimErr := f.store.ClaimRegistryCleanupPlan(t.Context(), plan.ID, "worker", f.now, time.Hour, 15*time.Minute)
			if claimErr == nil || won {
				t.Fatalf("unsafe claim accepted: won=%v err=%v", won, claimErr)
			}
			current, err := f.store.RegistryCleanupPlan(t.Context(), plan.ID)
			if err != nil || current.ClaimedAt != nil {
				t.Fatalf("failed claim acquired execution: err=%v", err)
			}
			var leases int
			if err = f.store.pool.QueryRow(t.Context(), `SELECT count(*) FROM registry_cleanup_leases WHERE plan_id=$1`, plan.ID).Scan(&leases); err != nil || leases != 0 {
				t.Fatalf("failed claim acquired leases: count=%d err=%v", leases, err)
			}
		})
	}
}

func TestRegistrySemanticSQLLegacyPlanCompatibility(t *testing.T) {
	for _, mode := range []string{"unclaimed", "refreshed-execution", "checkpoint", "offline-recovery"} {
		t.Run(mode, func(t *testing.T) {
			f := seedSemanticRegistrySQL(t)
			lifecycle := f.lifecycle()
			plan, err := lifecycle.Preview(t.Context(), f.targetID, f.serviceID)
			if err != nil {
				t.Fatal(err)
			}
			if mode != "unclaimed" {
				plan, _, err = lifecycle.Claim(t.Context(), plan.ID, "worker", time.Hour)
				if err != nil {
					t.Fatal(err)
				}
			}
			snapshot, err := f.store.RegistryLifecycleSnapshot(t.Context(), f.targetID, f.serviceID, f.now)
			if err != nil {
				t.Fatal(err)
			}
			plan.SnapshotToken = "sha256:" + strings.Repeat("a", 64)
			plan.AuthorityToken = base.RegistryAuthorityTokenForPlan(snapshot, plan)
			plan.PlanDigest = base.RegistryCleanupPlanDigest(plan)
			if _, err = f.store.pool.Exec(t.Context(), `UPDATE registry_cleanup_plans SET snapshot_token=$2,authority_token=$3,plan_digest=$4 WHERE id=$1`, plan.ID, plan.SnapshotToken, plan.AuthorityToken, plan.PlanDigest); err != nil {
				t.Fatal(err)
			}
			if mode == "unclaimed" {
				_, won, claimErr := lifecycle.Claim(t.Context(), plan.ID, "worker", time.Hour)
				current, _ := f.store.RegistryCleanupPlan(t.Context(), plan.ID)
				if !errors.Is(claimErr, base.ErrRegistrySnapshotStale) || won || current.State != "superseded" || current.SnapshotToken != plan.SnapshotToken {
					t.Fatalf("legacy preview reauthorized: won=%v state=%s err=%v", won, current.State, claimErr)
				}
				return
			}
			if mode == "offline-recovery" {
				if _, err = f.store.pool.Exec(t.Context(), `UPDATE registry_cleanup_plans SET state='failed',failure='offline sweep failed' WHERE id=$1`, plan.ID); err != nil {
					t.Fatal(err)
				}
				if _, err = f.store.pool.Exec(t.Context(), `UPDATE registry_cleanup_items SET state=CASE WHEN resource_kind='blob' THEN 'failed' ELSE 'deleted' END WHERE plan_id=$1 AND disposition='delete'`, plan.ID); err != nil {
					t.Fatal(err)
				}
				recovered, won, claimErr := lifecycle.Claim(t.Context(), plan.ID, "worker", time.Hour)
				if claimErr != nil || !won || recovered.State != "executing" || recovered.SnapshotToken != plan.SnapshotToken || recovered.AuthorityToken != plan.AuthorityToken {
					t.Fatalf("legacy recovery checkpoint changed: won=%v err=%v", won, claimErr)
				}
				return
			}
			if mode == "refreshed-execution" {
				f.refresh(t)
			}
			for _, item := range plan.Items {
				if item.ResourceKind != "release-manifest" || item.Digest != postgresRegistryDigest("b") {
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
				if err = lifecycle.RecordItemResult(context.Background(), plan.ID, item.Ordinal, "worker", domain.RegistryCleanupItemResult{State: "deleted", ObservedAt: f.now}); err != nil {
					t.Fatal(err)
				}
				current, _ := f.store.RegistryCleanupPlan(t.Context(), plan.ID)
				snapshot, _ = f.store.RegistryLifecycleSnapshot(t.Context(), f.targetID, f.serviceID, f.now)
				if current.SnapshotToken != plan.SnapshotToken || current.AuthorityToken != base.RegistryAuthorityTokenForPlan(snapshot, plan) || current.AuthorityToken == base.RegistryAuthorityToken(snapshot) {
					t.Fatal("legacy item checkpoint changed token semantics")
				}
				return
			}
			t.Fatal("fixture did not contain expected deletion item")
		})
	}
}
