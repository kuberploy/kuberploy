package postgres

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/registry"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func postgresRegistryDigest(character string) string {
	return "sha256:" + strings.Repeat(character, 64)
}

func TestRegistryLifecycleSQLPaths(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := context.Background()
	st, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := databaseTime(time.Now())
	old := now.Add(-30 * 24 * time.Hour)
	targetID, serviceID := id.New(), "integration-main-"+id.New()
	repository := "integration/" + targetID + "/main"
	target, err := st.PutRegistryTarget(ctx, domain.RegistryTarget{ID: targetID, Name: "integration-" + targetID, Mode: domain.RegistryTargetManaged, Endpoint: "registry.integration.test", RepositoryPrefix: "integration", CreatedAt: old, UpdatedAt: old})
	if err != nil {
		t.Fatal(err)
	}
	policy := registry.DefaultPolicy(target.ID, serviceID, repository, old)
	policy.KeepLastSuccessful = 1
	policy, err = st.PutServiceRegistryPolicy(ctx, policy)
	if err != nil {
		t.Fatal(err)
	}
	if policy.KeepLastSuccessful != 1 || policy.CacheUnusedExpiry != 7*24*time.Hour {
		t.Fatalf("policy round trip=%#v", policy)
	}
	catalog := domain.RegistryCatalogSnapshot{
		Observation: domain.RegistryCatalogObservation{ID: id.New(), RegistryTargetID: targetID, Repository: repository, Revision: 1, Complete: true, ObservedAt: now, ManifestCount: 2, BlobCount: 2},
		Manifests: []domain.RegistryManifest{
			{RegistryTargetID: targetID, Repository: repository, Digest: postgresRegistryDigest("a"), Kind: domain.RegistryManifestImage, MediaType: "application/vnd.oci.image.manifest.v1+json", SizeBytes: 10, FirstObservedAt: old},
			{RegistryTargetID: targetID, Repository: repository, Digest: postgresRegistryDigest("b"), Kind: domain.RegistryManifestImage, MediaType: "application/vnd.oci.image.manifest.v1+json", SizeBytes: 10, FirstObservedAt: old},
		},
		Blobs: []domain.RegistryBlob{
			{RegistryTargetID: targetID, Repository: repository, Digest: postgresRegistryDigest("c"), SizeBytes: 100, FirstObservedAt: old},
			{RegistryTargetID: targetID, Repository: repository, Digest: postgresRegistryDigest("d"), SizeBytes: 100, FirstObservedAt: old},
		},
		BlobLinks: []domain.RegistryManifestBlobLink{
			{Repository: repository, ManifestDigest: postgresRegistryDigest("a"), BlobDigest: postgresRegistryDigest("c")},
			{Repository: repository, ManifestDigest: postgresRegistryDigest("b"), BlobDigest: postgresRegistryDigest("d")},
		},
	}
	if err = st.ReplaceRegistryCatalog(ctx, catalog); err != nil {
		t.Fatal(err)
	}
	if err = st.ReplaceRegistryCatalog(ctx, catalog); err != nil {
		t.Fatalf("catalog replay: %v", err)
	}
	changed := catalog
	changed.Manifests = append([]domain.RegistryManifest(nil), catalog.Manifests...)
	changed.Manifests[0].SizeBytes++
	if err = st.ReplaceRegistryCatalog(ctx, changed); !errors.Is(err, base.ErrConflict) {
		t.Fatalf("same catalog revision mutation err=%v", err)
	}
	if err = st.RecordRegistryInventory(ctx, domain.RegistryInventoryObservation{RegistryTargetID: targetID, Revision: "inventory-1", Complete: true, Repositories: []string{repository}, ObservedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, authority := range []domain.RegistryAuthority{domain.RegistryAuthorityGitIntent, domain.RegistryAuthorityRuntime, domain.RegistryAuthorityOperations} {
		snapshot := domain.RegistryProtectionSnapshot{Observation: domain.RegistryAuthorityObservation{RegistryTargetID: targetID, ServiceID: serviceID, Authority: authority, Revision: string(authority) + "-1", Complete: true, ObservedAt: now}}
		if err = st.ReplaceRegistryProtectionSnapshot(ctx, snapshot); err != nil {
			t.Fatal(err)
		}
		if err = st.ReplaceRegistryProtectionSnapshot(ctx, snapshot); err != nil {
			t.Fatalf("authority replay: %v", err)
		}
	}
	latestAt, oldAt := now.Add(-24*time.Hour), now.Add(-48*time.Hour)
	latest := domain.RegistryRelease{ID: id.New(), RegistryTargetID: targetID, ServiceID: serviceID, Repository: repository, RootDigest: postgresRegistryDigest("a"), CreatedAt: old, SucceededAt: &latestAt, Availability: domain.RegistryArtifactPresent}
	oldRelease := domain.RegistryRelease{ID: id.New(), RegistryTargetID: targetID, ServiceID: serviceID, Repository: repository, RootDigest: postgresRegistryDigest("b"), CreatedAt: old, SucceededAt: &oldAt, Availability: domain.RegistryArtifactPresent}
	for _, release := range []domain.RegistryRelease{latest, oldRelease} {
		if _, replay, putErr := st.PutRegistryRelease(ctx, release); putErr != nil || replay {
			t.Fatalf("put release replay=%v err=%v", replay, putErr)
		}
		if _, replay, putErr := st.PutRegistryRelease(ctx, release); putErr != nil || !replay {
			t.Fatalf("replay release replay=%v err=%v", replay, putErr)
		}
	}
	concurrentRelease := domain.RegistryRelease{ID: id.New(), RegistryTargetID: targetID, ServiceID: serviceID, Repository: repository, RootDigest: postgresRegistryDigest("a"), CreatedAt: old, Availability: domain.RegistryArtifactPresent}
	const concurrentWriters = 8
	var inserted atomic.Int64
	var writers sync.WaitGroup
	writeErrors := make(chan error, concurrentWriters)
	for range concurrentWriters {
		writers.Add(1)
		go func() {
			defer writers.Done()
			_, replay, putErr := st.PutRegistryRelease(ctx, concurrentRelease)
			if putErr == nil && !replay {
				inserted.Add(1)
			}
			writeErrors <- putErr
		}()
	}
	writers.Wait()
	close(writeErrors)
	for putErr := range writeErrors {
		if putErr != nil {
			t.Errorf("concurrent release: %v", putErr)
		}
	}
	if inserted.Load() != 1 {
		t.Fatalf("concurrent release inserts=%d", inserted.Load())
	}
	lifecycle := registry.NewService(st, registry.WithClock(func() time.Time { return now }), registry.WithMaxObservationAge(time.Hour))
	plan, err := lifecycle.Preview(ctx, targetID, serviceID)
	if err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := lifecycle.Claim(ctx, plan.ID, "integration-worker", 10*time.Minute); err != nil || !claimed {
		t.Fatalf("claim claimed=%v err=%v", claimed, err)
	}
	if _, claimed, err := lifecycle.Claim(ctx, plan.ID, "integration-worker", 10*time.Minute); err != nil || claimed {
		t.Fatalf("claim replay claimed=%v err=%v", claimed, err)
	}
	cleanupOwner := "integration-replacement"
	if recovered, claimed, reclaimErr := st.ClaimRegistryCleanupPlan(ctx, plan.ID, cleanupOwner, now.Add(11*time.Minute), 10*time.Minute); reclaimErr != nil || !claimed || recovered.State != "executing" {
		t.Fatalf("expired cleanup recovery claimed=%v state=%q err=%v", claimed, recovered.State, reclaimErr)
	}
	if renewErr := st.RenewRegistryCleanupPlanLeases(ctx, plan.ID, "integration-worker", now.Add(11*time.Minute), 10*time.Minute); !errors.Is(renewErr, base.ErrRegistryLeaseLost) {
		t.Fatalf("stale cleanup owner renewed after recovery: %v", renewErr)
	}
	for _, expectedKind := range []string{"release-manifest", "blob"} {
		current, getErr := st.RegistryCleanupPlan(ctx, plan.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		ordinal := -1
		for _, item := range current.Items {
			if item.ResourceKind == expectedKind && item.Disposition == domain.RegistryCleanupDelete && item.State == "planned" {
				ordinal = item.Ordinal
				break
			}
		}
		if ordinal < 0 {
			t.Fatalf("no %s deletion", expectedKind)
		}
		if _, err = lifecycle.AuthorizeItem(ctx, plan.ID, ordinal, cleanupOwner); err != nil {
			t.Fatal(err)
		}
		result := domain.RegistryCleanupItemResult{State: "deleted", ObservedAt: now.Add(time.Second)}
		if err = lifecycle.RecordItemResult(ctx, plan.ID, ordinal, cleanupOwner, result); err != nil {
			t.Fatal(err)
		}
		if err = lifecycle.RecordItemResult(ctx, plan.ID, ordinal, cleanupOwner, result); err != nil {
			t.Fatalf("result replay: %v", err)
		}
	}
	if err = lifecycle.Finish(ctx, plan.ID, cleanupOwner, true, ""); err != nil {
		t.Fatal(err)
	}
	resolved, err := st.RegistryRelease(ctx, oldRelease.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Availability != domain.RegistryArtifactExpired {
		t.Fatalf("old release availability=%s", resolved.Availability)
	}

	externalID := id.New()
	if _, err = st.PutRegistryTarget(ctx, domain.RegistryTarget{ID: externalID, Name: "external-" + externalID, Mode: domain.RegistryTargetExternal, Endpoint: "external.integration.test", RepositoryPrefix: "integration"}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.pool.Exec(ctx, `INSERT INTO registry_cleanup_plans(
		id,registry_target_id,service_id,snapshot_token,authority_token,plan_digest,
		state,policy,observations,summary,created_at
	) VALUES($1,$2,'main','snapshot','authority',$3,'preview','{}','{}','{}',$4)`,
		id.New(), externalID, postgresRegistryDigest("e"), now); err == nil {
		t.Fatal("database accepted cleanup plan for external registry")
	}
	if _, err = st.pool.Exec(ctx, `UPDATE registry_targets SET mode='external' WHERE id=$1`, targetID); err == nil {
		t.Fatal("database accepted registry mode transition")
	}
}
