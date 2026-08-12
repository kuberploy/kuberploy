package secrets

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/testdb"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/domain"
)

func TestPostgreSQLRuntimeSecretContract(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = migrateSecretTestDatabase(ctx, pool); err != nil {
		t.Fatal(err)
	}
	cleanupPostgresSecretFixture(ctx, pool)
	if _, err = pool.Exec(ctx, `INSERT INTO users(id,login,role,issuer,subject,created_at) VALUES($1,'secret-test','platform-admin','test','secret-test',$2)`, testActor, testTime); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO teams(id,name,slug,created_by,created_at) VALUES($1,'Secret team','secret-team',$2,$3)`, testOrganization, testActor, testTime); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO projects(id,name,slug,team_id,created_at) VALUES($1,'Secret project','secret-project',$2,$3)`, testProject, testOrganization, testTime); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at) VALUES($1,$2,'Runtime','runtime','runtime-test','secret-runtime',$3)`, testEnvironment, testProject, testTime); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO applications(id,project_id,name,slug,created_at) VALUES($1,$2,'Database','database',$3)`, testApplication, testProject, testTime); err != nil {
		t.Fatal(err)
	}
	store, err := NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	provider := &fakeProviders{}
	service := testService(store, provider)
	const personalProject = "10000000-0000-4000-8000-000000000013"
	const personalEnvironment = "10000000-0000-4000-8000-000000000014"
	const personalApplication = "10000000-0000-4000-8000-000000000015"
	if _, err = pool.Exec(ctx, `INSERT INTO projects(id,name,slug,created_at) VALUES($1,'Personal project','personal-project',$2)`, personalProject, testTime); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at) VALUES($1,$2,'Runtime','runtime','personal-runtime','personal-runtime',$3)`, personalEnvironment, personalProject, testTime); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO applications(id,project_id,name,slug,created_at) VALUES($1,$2,'Personal API','personal-api',$3)`, personalApplication, personalProject, testTime); err != nil {
		t.Fatal(err)
	}
	personalRequest := createRequest(t, ProviderSealedSecrets, "personal-project-value", "postgres-personal-01")
	personalRequest.Scope = Scope{ProjectID: personalProject, EnvironmentID: personalEnvironment, ApplicationID: personalApplication, Namespace: "personal-runtime"}
	personalRequest.Name, personalRequest.RequestID = "personal", "postgres-personal-create"
	personalCreated, err := service.Create(ctx, personalRequest)
	if err != nil || personalCreated.Binding.Scope.OrganizationID != "" {
		t.Fatalf("personal binding=%#v err=%v", personalCreated.Binding, err)
	}
	personalBindings, err := store.ListBindings(ctx, personalApplication, personalEnvironment)
	if err != nil || len(personalBindings) != 1 || personalBindings[0].ID != personalCreated.Binding.ID || personalBindings[0].Scope.OrganizationID != "" {
		t.Fatalf("personal bindings=%#v err=%v", personalBindings, err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO secret_bindings(
		id,organization_id,project_id,environment_id,application_id,target_namespace,name,provider,purpose,state,active_version,created_by,created_at,updated_at)
		SELECT '10000000-0000-4000-8000-000000000016',$2,project_id,environment_id,application_id,target_namespace,'forged-team',provider,purpose,state,active_version,created_by,created_at,updated_at
		FROM secret_bindings WHERE id=$1`, personalCreated.Binding.ID, testOrganization); err == nil {
		t.Fatal("personal project accepted a forged team organization")
	}
	created, err := service.Create(ctx, createRequest(t, ProviderSealedSecrets, "pg-plaintext-value", "postgres-create-01"))
	if err != nil || created.Version.State != VersionAwaitingReadiness {
		t.Fatalf("created=%#v err=%v", created, err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO secret_bindings(
		id,organization_id,project_id,environment_id,application_id,target_namespace,name,provider,purpose,state,active_version,created_by,created_at,updated_at)
		SELECT '10000000-0000-4000-8000-000000000017',NULL,project_id,environment_id,application_id,target_namespace,'forged-personal',provider,purpose,state,active_version,created_by,created_at,updated_at
		FROM secret_bindings WHERE id=$1`, created.Binding.ID); err == nil {
		t.Fatal("team-owned project accepted an empty organization")
	}
	replayed, err := service.Create(ctx, createRequest(t, ProviderSealedSecrets, "pg-plaintext-value", "postgres-create-01"))
	if err != nil || !replayed.Replay || replayed.Version.ID != created.Version.ID {
		t.Fatalf("replayed=%#v err=%v", replayed, err)
	}
	active, err := service.ReconcileVersion(ctx, created.Version.ID, "postgres-ready-1")
	if err != nil || active.Version.State != VersionActive || active.Binding.ActiveVersion != 1 {
		t.Fatalf("active=%#v err=%v", active, err)
	}
	const workers = 8
	var wait sync.WaitGroup
	concurrentIDs, concurrentErrors := make(chan string, workers), make(chan error, workers)
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			request := createRequest(t, ProviderExternalSecrets, "same-cache-secret", "postgres-concurrent-1")
			request.Name, request.RequestID = "cache", "postgres-concurrent"
			result, createErr := service.Create(context.Background(), request)
			if createErr == nil {
				concurrentIDs <- result.Version.ID
			}
			concurrentErrors <- createErr
		}()
	}
	wait.Wait()
	close(concurrentIDs)
	close(concurrentErrors)
	for createErr := range concurrentErrors {
		if createErr != nil {
			t.Errorf("concurrent create: %v", createErr)
		}
	}
	ids := map[string]struct{}{}
	for versionID := range concurrentIDs {
		ids[versionID] = struct{}{}
	}
	if len(ids) != 1 {
		t.Fatalf("concurrent version IDs=%v", ids)
	}
	bindings, err := store.ListBindings(ctx, testApplication, testEnvironment)
	if err != nil || len(bindings) != 2 {
		t.Fatalf("listed bindings=%#v err=%v", bindings, err)
	}
	rotated, err := service.Rotate(ctx, RotateRequest{ActorID: testActor, BindingID: active.Binding.ID, ExpectedActiveVersion: 1,
		Deliveries: testDeliveries(), IdempotencyKey: "postgres-rotate-01", RequestID: "postgres-rotate", Material: testMaterial(t, "pg-next-value")})
	if err != nil {
		t.Fatal(err)
	}
	rotated, err = service.ReconcileVersion(ctx, rotated.Version.ID, "postgres-ready-2")
	if err != nil || rotated.Binding.ActiveVersion != 2 {
		t.Fatalf("rotated=%#v err=%v", rotated, err)
	}
	versions, err := store.Versions(ctx, active.Binding.ID)
	if err != nil || len(versions) != 2 || versions[0].State != VersionRetained || versions[1].State != VersionActive {
		t.Fatalf("versions=%#v err=%v", versions, err)
	}
	reference := Reference{BindingID: active.Binding.ID, VersionID: versions[1].ID, Kind: ReferenceGitCurrent,
		Reference: "tenants/project/environment/apps/database/app.yaml", Revision: strings.Repeat("d", 40)}
	if err = service.AddReference(ctx, testActor, "postgres-ref-add", reference); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE secret_binding_references SET revision=$2 WHERE binding_id=$1 AND kind='git-current'`, active.Binding.ID, strings.Repeat("e", 40)); err == nil {
		t.Fatal("reference was rebound in place")
	}
	if _, err = service.Delete(ctx, testActor, active.Binding.ID, "postgres-delete-blocked"); !errors.Is(err, ErrReferenced) {
		t.Fatalf("delete with reference: %v", err)
	}
	if err = service.RemoveReference(ctx, testActor, active.Binding.ID, ReferenceGitCurrent, reference.Reference, "postgres-ref-remove"); err != nil {
		t.Fatal(err)
	}
	deleted, err := service.Delete(ctx, testActor, active.Binding.ID, "postgres-delete")
	if err != nil || deleted.State != BindingDeleted {
		t.Fatalf("deleted=%#v err=%v", deleted, err)
	}
	events, err := store.PendingEvents(ctx, 100)
	if err != nil || len(events) < 8 {
		t.Fatalf("events=%#v err=%v", events, err)
	}
	if err = store.MarkEventPublished(ctx, events[0].ID, events[0].OccurredAt); err != nil {
		t.Fatal(err)
	}

	// Production reconciliation is a durable, multi-worker strict
	// SealedSecrets state machine. Simulate a worker crash, lease expiry and a
	// second worker takeover; the first epoch must never commit afterward.
	runtimeRequest := createRequest(t, ProviderSealedSecrets, "pg-runtime-only-value", "postgres-runtime-01")
	runtimeRequest.Name, runtimeRequest.RequestID = "runtimequeue", "postgres-runtime-create"
	runtimeCreated, err := service.Create(ctx, runtimeRequest)
	if err != nil {
		t.Fatal(err)
	}
	runtimeConfig := testRuntimeConfig()
	runtimeIdentity := testRuntimeIdentity(t, runtimeConfig)
	claimAt := testTime.Add(10 * time.Minute)
	firstLease, err := store.ClaimRuntimeSecret(ctx, runtimeIdentity, "runtime-pg-worker-one", runtimeConfig.Namespaces, claimAt, runtimeConfig.WorkLease)
	if err != nil || firstLease.Version.ID != runtimeCreated.Version.ID || firstLease.Lease.Epoch != 1 {
		t.Fatalf("first runtime claim=%#v err=%v", firstLease, err)
	}
	if _, err = store.ClaimRuntimeSecret(ctx, runtimeIdentity, "runtime-pg-worker-two", runtimeConfig.Namespaces, claimAt.Add(time.Second), runtimeConfig.WorkLease); !errors.Is(err, ErrNotFound) {
		t.Fatalf("concurrent runtime claim: %v", err)
	}
	takeoverAt := firstLease.Lease.Until.Add(time.Second)
	secondLease, err := store.ClaimRuntimeSecret(ctx, runtimeIdentity, "runtime-pg-worker-two", runtimeConfig.Namespaces, takeoverAt, runtimeConfig.WorkLease)
	if err != nil || secondLease.Lease.Epoch != 2 {
		t.Fatalf("runtime takeover=%#v err=%v", secondLease, err)
	}
	staleAt := takeoverAt.Add(time.Second)
	staleEvent := Event{ID: "20000000-0000-4000-8000-000000000001", BindingID: firstLease.Binding.ID,
		VersionID: firstLease.Version.ID, Kind: EventVersionActive, RequestID: "postgres-runtime-stale", OccurredAt: staleAt}
	if _, _, err = store.ApplyRuntimeSecretReady(ctx, firstLease.Lease, staleEvent, staleAt); !errors.Is(err, ErrRuntimeLeaseLost) {
		t.Fatalf("stale runtime apply: %v", err)
	}
	readyAt := takeoverAt.Add(2 * time.Second)
	readyEvent := Event{ID: "20000000-0000-4000-8000-000000000002", BindingID: secondLease.Binding.ID,
		VersionID: secondLease.Version.ID, Kind: EventVersionActive, RequestID: "postgres-runtime-ready", OccurredAt: readyAt}
	runtimeBinding, runtimeVersion, err := store.ApplyRuntimeSecretReady(ctx, secondLease.Lease, readyEvent, readyAt)
	if err != nil || runtimeBinding.State != BindingReady || runtimeVersion.State != VersionActive {
		t.Fatalf("runtime ready binding=%#v version=%#v err=%v", runtimeBinding, runtimeVersion, err)
	}
	var safeEventCount int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM secret_binding_events WHERE id=$1`, readyEvent.ID).Scan(&safeEventCount); err != nil || safeEventCount != 1 {
		t.Fatalf("atomic runtime event count=%d err=%v", safeEventCount, err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM secret_binding_events WHERE id=$1`, staleEvent.ID).Scan(&safeEventCount); err != nil || safeEventCount != 0 {
		t.Fatalf("stale runtime event count=%d err=%v", safeEventCount, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE secret_binding_runtime_reconciliations SET lease_epoch=lease_epoch+2 WHERE version_id=$1`, runtimeVersion.ID); err == nil {
		t.Fatal("runtime epoch skipped its fence")
	}

	backoffRequest := createRequest(t, ProviderSealedSecrets, "pg-runtime-backoff-value", "postgres-runtime-02")
	backoffRequest.Name, backoffRequest.RequestID = "runtimebackoff", "postgres-runtime-backoff"
	backoffCreated, err := service.Create(ctx, backoffRequest)
	if err != nil {
		t.Fatal(err)
	}
	backoffAt := readyAt.Add(time.Minute)
	backoffWork, err := store.ClaimRuntimeSecret(ctx, runtimeIdentity, "runtime-pg-worker-one", runtimeConfig.Namespaces, backoffAt, runtimeConfig.WorkLease)
	if err != nil || backoffWork.Version.ID != backoffCreated.Version.ID {
		t.Fatalf("backoff claim=%#v err=%v", backoffWork, err)
	}
	heartbeatLease, err := store.HeartbeatRuntimeSecret(ctx, backoffWork.Lease, backoffAt.Add(time.Second), runtimeConfig.WorkLease)
	if err != nil || !heartbeatLease.Until.After(backoffWork.Lease.Until) {
		t.Fatalf("heartbeat lease=%#v err=%v", heartbeatLease, err)
	}
	if _, err = store.HeartbeatRuntimeSecret(ctx, backoffWork.Lease, backoffAt.Add(2*time.Second), runtimeConfig.WorkLease); !errors.Is(err, ErrRuntimeLeaseLost) {
		t.Fatalf("superseded work heartbeat: %v", err)
	}
	nextAttempt := backoffAt.Add(10 * time.Second)
	if err = store.ApplyRuntimeSecretPending(ctx, heartbeatLease,
		RuntimePendingOutcome{FailureCode: "provider-observe-failed", NextAt: nextAttempt}, backoffAt.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ClaimRuntimeSecret(ctx, runtimeIdentity, "runtime-pg-worker-two", runtimeConfig.Namespaces, nextAttempt.Add(-time.Millisecond), runtimeConfig.WorkLease); !errors.Is(err, ErrNotFound) {
		t.Fatalf("backoff claimed early: %v", err)
	}
	backoffRetry, err := store.ClaimRuntimeSecret(ctx, runtimeIdentity, "runtime-pg-worker-two", runtimeConfig.Namespaces, nextAttempt, runtimeConfig.WorkLease)
	if err != nil || backoffRetry.Lease.Epoch != 2 || backoffRetry.ConsecutiveFailures != 1 {
		t.Fatalf("backoff retry=%#v err=%v", backoffRetry, err)
	}
	failedAt := nextAttempt.Add(time.Second)
	failedEvent := Event{ID: "20000000-0000-4000-8000-000000000003", BindingID: backoffRetry.Binding.ID,
		VersionID: backoffRetry.Version.ID, Kind: EventVersionFailed, RequestID: "postgres-runtime-failed", OccurredAt: failedAt}
	failedVersion, err := store.ApplyRuntimeSecretFailed(ctx, backoffRetry.Lease, "sealed-secret-sync-failed", failedEvent, failedAt)
	if err != nil || failedVersion.State != VersionFailed || failedVersion.FailureCode != "sealed-secret-sync-failed" {
		t.Fatalf("failed runtime version=%#v err=%v", failedVersion, err)
	}
	var runtimeState string
	if err = pool.QueryRow(ctx, `SELECT runtime_state FROM secret_binding_runtime_reconciliations WHERE version_id=$1`, failedVersion.ID).Scan(&runtimeState); err != nil || runtimeState != "failed" {
		t.Fatalf("runtime state=%q err=%v", runtimeState, err)
	}

	readinessObservation := RuntimeWorkerObservation{WorkerID: "runtime-pg-worker-one", Identity: runtimeIdentity,
		StartedAt: readyAt, ObservedAt: readyAt}
	firstReadiness, err := store.AcquireRuntimeSecretReadiness(ctx, readinessObservation, RuntimeSecretReadinessLease)
	if err != nil {
		t.Fatal(err)
	}
	secondReadiness, err := store.AcquireRuntimeSecretReadiness(ctx, readinessObservation, RuntimeSecretReadinessLease)
	if err != nil || secondReadiness.Epoch != firstReadiness.Epoch+1 {
		t.Fatalf("readiness takeover=%#v err=%v", secondReadiness, err)
	}
	if _, err = store.HeartbeatRuntimeSecretReadiness(ctx, firstReadiness, readyAt.Add(time.Second), RuntimeSecretReadinessLease); !errors.Is(err, ErrRuntimeLeaseLost) {
		t.Fatalf("stale readiness heartbeat: %v", err)
	}
	secondReadiness, err = store.HeartbeatRuntimeSecretReadiness(ctx, secondReadiness, readyAt.Add(2*time.Second), RuntimeSecretReadinessLease)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.RuntimeSecretReady(ctx, runtimeIdentity, readyAt.Add(3*time.Second), RuntimeSecretHeartbeatMaxAge); err != nil {
		t.Fatalf("fresh exact readiness: %v", err)
	}
	mismatchedIdentity := runtimeIdentity
	mismatchedIdentity.ConfigDigest = "sha256:" + strings.Repeat("f", 64)
	if err = store.RuntimeSecretReady(ctx, mismatchedIdentity, readyAt.Add(3*time.Second), RuntimeSecretHeartbeatMaxAge); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("mismatched readiness: %v", err)
	}
	if err = store.RuntimeSecretReady(ctx, runtimeIdentity, secondReadiness.ObservedAt.Add(RuntimeSecretHeartbeatMaxAge+time.Second), RuntimeSecretHeartbeatMaxAge); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("stale readiness: %v", err)
	}

	// Git-current reference replacement runs inside its caller's transaction.
	// It re-resolves exact active metadata, rolls back as one unit, and never
	// mutates retained/current release guards.
	referenceProvider := &exactReferenceProvider{}
	referenceService := referenceService(store, referenceProvider)
	referenceRequest := createRequest(t, ProviderSealedSecrets, "pg-reference-plan-value", "postgres-reference-plan-01")
	referenceRequest.Name, referenceRequest.RequestID = "gitreference", "postgres-reference-plan"
	referenceCreated, err := referenceService.Create(ctx, referenceRequest)
	if err != nil {
		t.Fatal(err)
	}
	referenceActive, err := referenceService.ReconcileVersion(ctx, referenceCreated.Version.ID, "postgres-reference-ready")
	if err != nil {
		t.Fatal(err)
	}
	ref := domain.SecretBindingRef{BindingID: referenceActive.Binding.ID, Name: referenceActive.Binding.Name, Key: "password", Version: 1}
	referencePlan, err := ResolveWorkloadBindingReferences(ctx, store, referenceActive.Binding.Scope, referenceRuntime(ref, "DATABASE_PASSWORD"))
	if err != nil {
		t.Fatal(err)
	}
	const referencePath = "tenants/runtime-test/apps/gitreference/app.yaml"
	revisionOne := "sha256:" + strings.Repeat("7", 64)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = ReplaceGitCurrentReferencesTx(ctx, tx, referencePlan, testActor, referencePath, revisionOne, "postgres-git-current-1", readyAt); err != nil {
		tx.Rollback(ctx) //nolint:errcheck
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = referenceService.Delete(ctx, testActor, referenceActive.Binding.ID, "postgres-reference-delete-blocked"); !errors.Is(err, ErrReferenced) {
		t.Fatalf("Git-current delete guard missing: %v", err)
	}
	var gitReferenceCount int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM secret_binding_references WHERE binding_id=$1 AND kind='git-current' AND reference_id=$2`, referenceActive.Binding.ID, referencePath).Scan(&gitReferenceCount); err != nil || gitReferenceCount != 1 {
		t.Fatalf("Git-current count=%d err=%v", gitReferenceCount, err)
	}
	invalidPlan := referencePlan
	invalidPlan.Uses = append([]ResolvedBindingReference(nil), referencePlan.Uses...)
	invalidPlan.Uses[0].Delivery.EnvironmentName = "OTHER_VARIABLE"
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = ReplaceGitCurrentReferencesTx(ctx, tx, invalidPlan, testActor, referencePath, "sha256:"+strings.Repeat("8", 64), "postgres-git-current-invalid", readyAt.Add(time.Second)); !errors.Is(err, ErrNotReady) {
		tx.Rollback(ctx) //nolint:errcheck
		t.Fatalf("delivery drift error=%v", err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var storedRevision string
	if err = pool.QueryRow(ctx, `SELECT revision FROM secret_binding_references WHERE binding_id=$1 AND kind='git-current' AND reference_id=$2`, referenceActive.Binding.ID, referencePath).Scan(&storedRevision); err != nil || storedRevision != revisionOne {
		t.Fatalf("rolled-back revision=%q err=%v", storedRevision, err)
	}

	referenceRotated, err := referenceService.Rotate(ctx, RotateRequest{ActorID: testActor, BindingID: referenceActive.Binding.ID, ExpectedActiveVersion: 1,
		Deliveries: testDeliveries(), IdempotencyKey: "postgres-reference-plan-02", RequestID: "postgres-reference-rotate", Material: testMaterial(t, "pg-reference-plan-next")})
	if err != nil {
		t.Fatal(err)
	}
	referenceRotated, err = referenceService.ReconcileVersion(ctx, referenceRotated.Version.ID, "postgres-reference-ready-2")
	if err != nil {
		t.Fatal(err)
	}
	retainedReference := Reference{BindingID: referenceActive.Binding.ID, VersionID: referenceActive.Version.ID, Kind: ReferenceRetainedRelease,
		Reference: "release-gitreference-v1", Revision: strings.Repeat("9", 40), CreatedAt: readyAt.Add(2 * time.Second)}
	retainedEvent := Event{ID: "30000000-0000-4000-8000-000000000001", BindingID: retainedReference.BindingID, VersionID: retainedReference.VersionID,
		ActorID: testActor, Kind: EventReferenceAdded, RequestID: "postgres-retained-release", OccurredAt: retainedReference.CreatedAt}
	if err = store.AddReference(ctx, retainedReference, retainedEvent); err != nil {
		t.Fatal(err)
	}
	ref.Version = 2
	updatedPlan, err := ResolveWorkloadBindingReferences(ctx, store, referenceRotated.Binding.Scope, referenceRuntime(ref, "DATABASE_PASSWORD"))
	if err != nil {
		t.Fatal(err)
	}
	revisionTwo := "sha256:" + strings.Repeat("a", 64)
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = ReplaceGitCurrentReferencesTx(ctx, tx, updatedPlan, "", referencePath, revisionTwo, "postgres-git-current-2", readyAt.Add(3*time.Second)); err != nil {
		tx.Rollback(ctx) //nolint:errcheck
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var storedVersionID string
	if err = pool.QueryRow(ctx, `SELECT version_id::text,revision FROM secret_binding_references WHERE binding_id=$1 AND kind='git-current' AND reference_id=$2`, referenceActive.Binding.ID, referencePath).Scan(&storedVersionID, &storedRevision); err != nil || storedVersionID != referenceRotated.Version.ID || storedRevision != revisionTwo {
		t.Fatalf("updated Git-current version=%q revision=%q err=%v", storedVersionID, storedRevision, err)
	}
	emptyPlan := BindingReferencePlan{Scope: referenceActive.Binding.Scope, Uses: []ResolvedBindingReference{}}
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = ReplaceGitCurrentReferencesTx(ctx, tx, emptyPlan, testActor, referencePath, "sha256:"+strings.Repeat("b", 64), "postgres-git-current-remove", readyAt.Add(4*time.Second)); err != nil {
		tx.Rollback(ctx) //nolint:errcheck
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var retainedCount int
	if err = pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE kind='git-current'),count(*) FILTER (WHERE kind='retained-release') FROM secret_binding_references WHERE binding_id=$1`, referenceActive.Binding.ID).Scan(&gitReferenceCount, &retainedCount); err != nil || gitReferenceCount != 0 || retainedCount != 1 {
		t.Fatalf("references Git=%d retained=%d err=%v", gitReferenceCount, retainedCount, err)
	}

	var durable string
	if err = pool.QueryRow(ctx, `SELECT string_agg(row_to_json(row_value)::text,' ') FROM (
		SELECT b.*,v.*,d.* FROM secret_bindings b JOIN secret_binding_versions v ON v.binding_id=b.id
		JOIN secret_binding_deliveries d ON d.version_id=v.id WHERE b.application_id=$1) row_value`, testApplication).Scan(&durable); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"pg-plaintext-value", "pg-next-value", "pg-runtime-only-value", "pg-runtime-backoff-value", "pg-reference-plan-value", "pg-reference-plan-next",
		base64.StdEncoding.EncodeToString([]byte("pg-plaintext-value")), base64.StdEncoding.EncodeToString([]byte("pg-runtime-only-value")),
		base64.StdEncoding.EncodeToString([]byte("pg-runtime-backoff-value"))} {
		if strings.Contains(durable, forbidden) {
			t.Fatalf("plaintext/base64 persisted: %s", durable)
		}
	}
	if _, err = pool.Exec(ctx, `UPDATE secret_binding_versions SET content_fingerprint=decode($2,'hex') WHERE id=$1`, versions[0].ID, strings.Repeat("0", 64)); err == nil {
		t.Fatal("immutable fingerprint was updated")
	}
	if _, err = pool.Exec(ctx, `UPDATE secret_binding_deliveries SET source_key='changed' WHERE version_id=$1 AND ordinal=0`, versions[0].ID); err == nil {
		t.Fatal("immutable delivery was updated")
	}
	if _, err = pool.Exec(ctx, `UPDATE secret_binding_versions SET provider_revision='different-provider-version' WHERE id=$1`, versions[0].ID); err == nil {
		t.Fatal("immutable provider artifact was updated")
	}
	if _, err = pool.Exec(ctx, `UPDATE secret_binding_events SET request_id='rewritten' WHERE binding_id=$1 AND kind='version-staging'`, active.Binding.ID); err == nil {
		t.Fatal("immutable event was updated")
	}
	if _, err = pool.Exec(ctx, `DELETE FROM mutation_receipts WHERE secret_binding_id=$1`, active.Binding.ID); err == nil {
		t.Fatal("idempotency tombstone was deleted")
	}
}

func migrateSecretTestDatabase(ctx context.Context, pool *pgxpool.Pool) error {
	return testdb.ApplyMigrations(ctx, pool)
}

func cleanupPostgresSecretFixture(ctx context.Context, pool *pgxpool.Pool) {
	_, _ = pool.Exec(ctx, `DELETE FROM secret_binding_events WHERE binding_id IN (SELECT id FROM secret_bindings WHERE application_id=$1)`, testApplication)
	_, _ = pool.Exec(ctx, `DELETE FROM secret_binding_references WHERE binding_id IN (SELECT id FROM secret_bindings WHERE application_id=$1)`, testApplication)
	_, _ = pool.Exec(ctx, `DELETE FROM mutation_receipts WHERE receipt_kind='secret-binding' AND scope_key=$1::text`, testApplication)
	// Delivery immutability intentionally prevents destructive cleanup of
	// version rows. Integration databases should be ephemeral; remove the whole
	// tenant only when no secret rows exist (the normal pre-test state).
	var secretsExist bool
	_ = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM secret_bindings WHERE application_id=$1)`, testApplication).Scan(&secretsExist)
	if secretsExist {
		return
	}
	_, _ = pool.Exec(ctx, `DELETE FROM applications WHERE id=$1`, testApplication)
	_, _ = pool.Exec(ctx, `DELETE FROM environments WHERE id=$1`, testEnvironment)
	_, _ = pool.Exec(ctx, `DELETE FROM projects WHERE id=$1`, testProject)
	_, _ = pool.Exec(ctx, `DELETE FROM teams WHERE id=$1`, testOrganization)
	_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, testActor)
}
