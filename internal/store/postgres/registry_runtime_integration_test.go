package postgres

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/registry"
	base "github.com/kuberploy/kuberploy/internal/store"
	"github.com/kuberploy/kuberploy/internal/testdb"
)

func TestRegistryRuntimeObservationLeaseRecoveryAndFencing(t *testing.T) {
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
	now := databaseTime(time.Now().UTC())
	targetID := id.New()
	target, err := st.PutRegistryTarget(ctx, domain.RegistryTarget{ID: targetID, Name: "runtime-" + targetID,
		Mode: domain.RegistryTargetManaged, Endpoint: "https://registry.integration.test", RepositoryPrefix: "integration",
		PushCredentialRef: "operator/managed-registry", CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	first, err := st.ClaimRegistryObservation(ctx, target.ID, "registry-observer-owner-a", now, time.Minute)
	if err != nil || first.Reclaimed || first.Lease.Epoch != 1 || first.Lease.Revision != 1 {
		t.Fatalf("first work=%+v err=%v", first, err)
	}
	if _, err = st.ClaimRegistryObservation(ctx, target.ID, "registry-observer-owner-b", now.Add(30*time.Second), time.Minute); !errors.Is(err, base.ErrConflict) {
		t.Fatalf("live observation lease takeover err=%v", err)
	}
	recoveredAt := now.Add(61 * time.Second)
	recovered, err := st.ClaimRegistryObservation(ctx, target.ID, "registry-observer-owner-b", recoveredAt, time.Minute)
	if err != nil || !recovered.Reclaimed || recovered.Lease.Epoch != first.Lease.Epoch+1 || recovered.Lease.Revision != first.Lease.Revision {
		t.Fatalf("recovered work=%+v err=%v", recovered, err)
	}
	if _, err = st.HeartbeatRegistryObservation(ctx, first.Lease, recoveredAt, time.Minute); !errors.Is(err, base.ErrRegistryLeaseLost) {
		t.Fatalf("stale observation heartbeat err=%v", err)
	}
	publication := base.RegistryObservationPublication{
		Inventory: domain.RegistryInventoryObservation{RegistryTargetID: target.ID,
			Revision: registryObservationRevision(recovered.Lease.Revision), Complete: true, ObservedAt: recoveredAt},
		ObservedAt: recoveredAt, NextAt: recoveredAt.Add(5 * time.Minute),
	}
	if err = st.PublishRegistryObservation(ctx, first.Lease, publication); !errors.Is(err, base.ErrRegistryLeaseLost) {
		t.Fatalf("stale observation publish err=%v", err)
	}
	if err = st.PublishRegistryObservation(ctx, recovered.Lease, publication); err != nil {
		t.Fatalf("recovered observation publish: %v", err)
	}
	if _, err = st.ClaimRegistryObservation(ctx, target.ID, "registry-observer-owner-c", recoveredAt.Add(time.Minute), time.Minute); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("observation scheduled before next_at err=%v", err)
	}
}

func TestPutRegistryTargetAllowsOnlyPolicyPreservingPrefixRotation(t *testing.T) {
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
	now := databaseTime(time.Now().UTC())
	target := domain.RegistryTarget{ID: id.New(), Name: "prefix-rotation-" + id.New(), Mode: domain.RegistryTargetManaged,
		Endpoint: "https://registry-prefix.integration.test", RepositoryPrefix: "kuberploy/apps", CreatedAt: now, UpdatedAt: now}
	if target, err = st.PutRegistryTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	policy := registry.DefaultPolicy(target.ID, "service", "kuberploy/apps/service", now)
	if _, err = st.PutServiceRegistryPolicy(ctx, policy); err != nil {
		t.Fatal(err)
	}
	target.RepositoryPrefix = "kuberploy"
	if target, err = st.PutRegistryTarget(ctx, target); err != nil {
		t.Fatalf("safe operator prefix broadening was rejected: %v", err)
	}
	target.RepositoryPrefix = "other"
	if _, err = st.PutRegistryTarget(ctx, target); !errors.Is(err, base.ErrConflict) {
		t.Fatalf("policy-orphaning prefix rotation was accepted: %v", err)
	}
}

func TestNextAcceptedRegistryCleanupUsesUUIDIdempotencyIdentity(t *testing.T) {
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
	now := databaseTime(time.Now().UTC())
	actorID, targetID, planID := id.New(), id.New(), id.New()
	if _, err = st.pool.Exec(ctx, `INSERT INTO users(id,login,role,issuer,subject,created_at)
		VALUES($1,$2,'platform-admin','local',$2,$3)`, actorID, "registry-cleanup-"+actorID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = st.PutRegistryTarget(ctx, domain.RegistryTarget{
		ID: targetID, Name: "cleanup-" + targetID, Mode: domain.RegistryTargetManaged,
		Endpoint: "https://registry-cleanup.integration.test", RepositoryPrefix: "integration",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.pool.Exec(ctx, `INSERT INTO registry_cleanup_plans(
		id,registry_target_id,service_id,snapshot_token,authority_token,plan_digest,state,
		policy,observations,summary,created_at
	) VALUES($1,$2,'service','snapshot','authority',$3,'preview','{}','{}','{}',$4)`,
		planID, targetID, postgresRegistryDigest("f"), now); err != nil {
		t.Fatal(err)
	}
	if _, err = st.pool.Exec(ctx, `INSERT INTO mutation_receipts(
		actor_id,receipt_kind,namespace,scope_key,idempotency_key,request_digest,resource_type,resource_id,created_at
	) VALUES($1,'resource',$2,'global','registry-cleanup-key','request-fingerprint','registry-cleanup-plan',$3,$4)`,
		actorID, "registry-cleanup.execute:"+planID, planID, now); err != nil {
		t.Fatal(err)
	}
	accepted, err := st.NextAcceptedRegistryCleanup(ctx, targetID, now)
	if err != nil || accepted != planID {
		t.Fatalf("accepted cleanup=%q want=%q err=%v", accepted, planID, err)
	}
}

func TestAcquireRegistryMaintenanceAcceptsMatchingUUIDTarget(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if err = testdb.ApplyMigrations(ctx, pool); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	pool.Close()
	st, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := databaseTime(time.Now().UTC())
	targetID, planID := id.New(), id.New()
	owner := "registry-maintenance-integration"
	blobDigest := postgresRegistryDigest("b")
	candidateDigest, err := registryCandidateSetDigest([]string{blobDigest})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.PutRegistryTarget(ctx, domain.RegistryTarget{
		ID: targetID, Name: "maintenance-" + targetID, Mode: domain.RegistryTargetManaged,
		Endpoint: "https://registry-maintenance.integration.test", RepositoryPrefix: "integration",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.pool.Exec(ctx, `INSERT INTO registry_cleanup_plans(
		id,registry_target_id,service_id,snapshot_token,authority_token,plan_digest,state,
		policy,observations,summary,created_at,claimed_at
	) VALUES($1,$2,'service','snapshot','authority',$3,'executing','{}','{}','{}',$4,$4)`,
		planID, targetID, postgresRegistryDigest("f"), now); err != nil {
		t.Fatal(err)
	}
	if _, err = st.pool.Exec(ctx, `INSERT INTO registry_cleanup_items(
		plan_id,ordinal,repository,resource_kind,digest,disposition,action,estimated_bytes,reasons,state,updated_at
	) VALUES($1,0,'*','blob',$2,'delete','garbage-collect-blob',1,'["globally-unreachable"]','deleting',$3)`,
		planID, blobDigest, now); err != nil {
		t.Fatal(err)
	}
	if _, err = st.pool.Exec(ctx, `INSERT INTO registry_cleanup_leases(
		registry_target_id,repository,plan_id,owner,lease_until
	) VALUES($1,'*',$2,$3,$4)`, targetID, planID, owner, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	lease, err := st.AcquireRegistryMaintenance(ctx, targetID, planID, postgresRegistryDigest("e"), candidateDigest, owner, now, time.Minute)
	if err != nil {
		t.Fatalf("acquire maintenance for matching UUID target: %v", err)
	}
	if lease.TargetID != targetID || lease.PlanID != planID || lease.CandidateSetDigest != candidateDigest || lease.State != "acquired" || lease.Epoch != 1 {
		t.Fatalf("unexpected maintenance lease: %+v", lease)
	}
}

func TestFailedRegistryOfflineSweepMayResumeWithExactCandidates(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if err = testdb.ApplyMigrations(ctx, pool); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	pool.Close()
	st, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := databaseTime(time.Now().UTC())
	targetID, planID := id.New(), id.New()
	if _, err = st.PutRegistryTarget(ctx, domain.RegistryTarget{
		ID: targetID, Name: "sweep-recovery-" + targetID, Mode: domain.RegistryTargetManaged,
		Endpoint: "https://registry-sweep-recovery.integration.test", RepositoryPrefix: "integration",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.pool.Exec(ctx, `INSERT INTO registry_cleanup_plans(
		id,registry_target_id,service_id,snapshot_token,authority_token,plan_digest,state,
		policy,observations,summary,created_at,claimed_at,completed_at,failure
	) VALUES($1,$2,'service','snapshot','authority',$3,'failed','{}','{}','{}',$4,$4,$4,
		'managed registry cleanup execution failed')`, planID, targetID, postgresRegistryDigest("e"), now); err != nil {
		t.Fatal(err)
	}
	if _, err = st.pool.Exec(ctx, `INSERT INTO registry_cleanup_items(
		plan_id,ordinal,repository,resource_kind,digest,disposition,action,estimated_bytes,reasons,state,updated_at
	) VALUES
		($1,0,'integration/service','release-manifest',$2,'delete','delete-manifest',1,'["retention-eligible"]','deleted',$4),
		($1,1,'*','blob',$3,'delete','garbage-collect-blob',1,'["globally-unreachable"]','deleting',$4)`,
		planID, postgresRegistryDigest("a"), postgresRegistryDigest("b"), now); err != nil {
		t.Fatal(err)
	}

	recovered, claimed, err := st.ClaimRegistryCleanupPlan(ctx, planID, "sweep-recovery-worker", now.Add(time.Second), time.Minute)
	if err != nil || !claimed || recovered.State != "executing" || recovered.Failure != "" || recovered.CompletedAt != nil {
		t.Fatalf("recovered=%#v claimed=%v err=%v", recovered, claimed, err)
	}
	var state, failure string
	var completedAt *time.Time
	if err = st.pool.QueryRow(ctx, `SELECT state,failure,completed_at FROM registry_cleanup_plans WHERE id=$1`, planID).Scan(&state, &failure, &completedAt); err != nil {
		t.Fatal(err)
	}
	if state != "executing" || failure != "" || completedAt != nil {
		t.Fatalf("persisted state=%q failure=%q completedAt=%v", state, failure, completedAt)
	}
}

func TestManagedRegistryRuntimeReadinessSQLFencingAndExactMatch(t *testing.T) {
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
	now := databaseTime(time.Now().UTC())
	config := registry.RuntimeConfig{
		Enabled: true, TargetID: id.New(), Endpoint: "https://registry-readiness.integration.test", RepositoryPrefix: "integration",
		CredentialRef: "operator/managed-registry", Namespace: "kuberploy-registry", Deployment: "kuberploy-registry",
		PersistentVolumeClaim: "kuberploy-registry", RegistryConfigMap: "kuberploy-registry-config-abc123",
		HelperServiceAccount: "kuberploy-registry-maintenance",
		HelperImage:          "ghcr.io/kuberploy/kuberploy-worker@sha256:" + strings.Repeat("a", 64),
		ObservationInterval:  5 * time.Minute,
	}
	if _, err = st.PutRegistryTarget(ctx, domain.RegistryTarget{ID: config.TargetID, Name: "readiness-" + config.TargetID,
		Mode: domain.RegistryTargetManaged, Endpoint: config.Endpoint, RepositoryPrefix: config.RepositoryPrefix,
		PushCredentialRef: "builder-push-secret", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	identity, err := registry.RuntimeIdentityForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	observation := registry.RuntimeWorkerObservation{WorkerID: "worker-registry-ready-01234567", RuntimeIdentity: identity, StartedAt: now, ObservedAt: now}
	first, err := st.AcquireManagedRegistryReadiness(ctx, observation, registry.ManagedRegistryReadinessLease)
	if err != nil || first.Epoch != 1 {
		t.Fatalf("first lease=%+v err=%v", first, err)
	}
	second, err := st.AcquireManagedRegistryReadiness(ctx, observation, registry.ManagedRegistryReadinessLease)
	if err != nil || second.Epoch != 2 {
		t.Fatalf("second lease=%+v err=%v", second, err)
	}
	if _, err = st.HeartbeatManagedRegistryReadiness(ctx, first, now.Add(time.Second), registry.ManagedRegistryReadinessLease); !errors.Is(err, base.ErrRegistryLeaseLost) {
		t.Fatalf("stale heartbeat err=%v", err)
	}
	second, err = st.HeartbeatManagedRegistryReadiness(ctx, second, now.Add(10*time.Second), registry.ManagedRegistryReadinessLease)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.ManagedRegistryRuntimeReady(ctx, identity, now.Add(10*time.Second), registry.ManagedRegistryHeartbeatMaxAge); err != nil {
		t.Fatalf("fresh readiness: %v", err)
	}
	if _, err = st.pool.Exec(ctx, `UPDATE registry_targets SET push_credential_ref='rotated-builder-push-secret' WHERE id=$1`, config.TargetID); err != nil {
		t.Fatal(err)
	}
	if err = st.ManagedRegistryRuntimeReady(ctx, identity, now.Add(10*time.Second), registry.ManagedRegistryHeartbeatMaxAge); err != nil {
		t.Fatalf("lifecycle readiness was coupled to the separate build-push credential: %v", err)
	}
	if _, err = st.pool.Exec(ctx, `UPDATE registry_targets SET push_credential_ref=$2 WHERE id=$1`, config.TargetID, config.CredentialRef); err != nil {
		t.Fatal(err)
	}
	if err = st.ManagedRegistryRuntimeReady(ctx, identity, now.Add(10*time.Second), registry.ManagedRegistryHeartbeatMaxAge); !errors.Is(err, registry.ErrRegistryRuntimeNotReady) {
		t.Fatalf("lifecycle credential reuse as build-push credential was accepted: %v", err)
	}
	if _, err = st.pool.Exec(ctx, `UPDATE registry_targets SET push_credential_ref='builder-push-secret' WHERE id=$1`, config.TargetID); err != nil {
		t.Fatal(err)
	}
	if _, err = st.pool.Exec(ctx, `UPDATE registry_targets SET endpoint=$2 WHERE id=$1`, config.TargetID, "https://changed-registry.integration.test"); err != nil {
		t.Fatal(err)
	}
	if err = st.ManagedRegistryRuntimeReady(ctx, identity, now.Add(10*time.Second), registry.ManagedRegistryHeartbeatMaxAge); !errors.Is(err, registry.ErrRegistryRuntimeNotReady) {
		t.Fatalf("mutated target readiness err=%v", err)
	}
	if _, err = st.pool.Exec(ctx, `UPDATE registry_targets SET endpoint=$2 WHERE id=$1`, config.TargetID, config.Endpoint); err != nil {
		t.Fatal(err)
	}
	changed := config
	changed.ObservationInterval++
	mismatch, _ := registry.RuntimeIdentityForConfig(changed)
	if err = st.ManagedRegistryRuntimeReady(ctx, mismatch, now.Add(10*time.Second), registry.ManagedRegistryHeartbeatMaxAge); !errors.Is(err, registry.ErrRegistryRuntimeNotReady) {
		t.Fatalf("mismatched readiness err=%v", err)
	}
	if err = st.ManagedRegistryRuntimeReady(ctx, identity, second.ObservedAt.Add(registry.ManagedRegistryHeartbeatMaxAge+time.Second), registry.ManagedRegistryHeartbeatMaxAge); !errors.Is(err, registry.ErrRegistryRuntimeNotReady) {
		t.Fatalf("stale readiness err=%v", err)
	}
	externalID := id.New()
	if _, err = st.PutRegistryTarget(ctx, domain.RegistryTarget{ID: externalID, Name: "external-ready-" + externalID,
		Mode: domain.RegistryTargetExternal, Endpoint: "https://external-readiness.integration.test", RepositoryPrefix: "integration"}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.pool.Exec(ctx, `INSERT INTO runtime_readiness(runtime_kind,scope_key,registry_target_id,
		worker_id,worker_epoch,contract_version,config_digest,identity,observation,started_at,observed_at,lease_until,updated_at
	) VALUES('managed-registry',$1::text,$1,'worker-registry-ready-external',1,$2,$3,'{}'::jsonb,'{}'::jsonb,$4,$4,$5,$4)`, externalID,
		registry.ManagedRegistryRuntimeContract, identity.ConfigDigest, now, now.Add(time.Minute)); err == nil {
		t.Fatal("database accepted runtime readiness for external registry target")
	}
}
