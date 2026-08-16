package imagepull

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/testdb"
)

func TestPostgreSQLRuntimeRegistryPullLifecycle(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("KUBERPLOY_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = testdb.ApplyMigrations(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store, err := NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	projectID := "71111111-1111-4111-8111-111111111111"
	environmentID := "72222222-2222-4222-8222-222222222222"
	targetID := "73333333-3333-4333-8333-333333333333"
	namespace := "pull-integration-a"
	pullRef := "runtime-pull/integration"
	// Keep the fixed-identity integration test rerunnable after an interrupted
	// process or a failed assertion whose best-effort cleanup could not finish.
	_, _ = pool.Exec(ctx, `DELETE FROM runtime_registry_pull_artifacts WHERE environment_id=$1`, environmentID)
	_, _ = pool.Exec(ctx, `DELETE FROM runtime_readiness
		WHERE runtime_kind='runtime-registry-pull' AND scope_key='global' AND worker_id LIKE 'registry-pull-integration:%'`)
	_, err = pool.Exec(ctx, `INSERT INTO projects(id,name,slug) VALUES($1,'pull integration','pull-integration')
		ON CONFLICT(id) DO NOTHING`, projectID)
	if err == nil {
		_, err = pool.Exec(ctx, `INSERT INTO environments(id,project_id,name,slug,namespace,argo_project)
			VALUES($1,$2,'pull integration','pull-integration',$3,$3) ON CONFLICT(id) DO NOTHING`, environmentID, projectID, namespace)
	}
	if err == nil {
		_, err = pool.Exec(ctx, `INSERT INTO registry_targets(id,name,mode,endpoint,repository_prefix,pull_credential_ref)
			VALUES($1,'pull-integration-target','external','https://registry.integration.test','pull-integration',$2)
			ON CONFLICT(id) DO NOTHING`, targetID, pullRef)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM runtime_registry_pull_artifacts WHERE environment_id=$1`, environmentID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM runtime_readiness
			WHERE runtime_kind='runtime-registry-pull' AND scope_key='global' AND worker_id LIKE 'registry-pull-integration:%'`)
		_, _ = pool.Exec(context.Background(), `DELETE FROM registry_targets WHERE id=$1`, targetID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM environments WHERE id=$1`, environmentID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id=$1`, projectID)
	})

	now := time.Date(2026, 8, 9, 8, 0, 0, 0, time.UTC)
	desired := DesiredArtifact{ArtifactKey: ArtifactKey{EnvironmentID: environmentID, RegistryTargetID: targetID, ProfileRevision: 1},
		Namespace: namespace, PullCredentialRef: pullRef, ProfileName: "integration", SecretName: SecretName(namespace, targetID, 1)}
	artifact, err := store.EnsureArtifact(ctx, desired, now)
	if err != nil || !artifact.Active || artifact.State != StateAwaiting {
		t.Fatalf("artifact=%#v err=%v", artifact, err)
	}
	if replay, replayErr := store.EnsureArtifact(ctx, desired, now.Add(time.Second)); replayErr != nil || replay != artifact {
		t.Fatalf("replay=%#v err=%v", replay, replayErr)
	}

	digest := "sha256:" + stringsOf('b', 64)
	var group sync.WaitGroup
	winners := make(chan Lease, 8)
	for index := 0; index < 8; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			owner := "registry-pull-integration:" + string(rune('a'+index))
			lease, found, claimErr := store.ClaimArtifact(ctx, owner, RuntimeContract, digest, now, time.Minute)
			if claimErr == nil && found {
				winners <- lease
			}
		}(index)
	}
	group.Wait()
	close(winners)
	if len(winners) != 1 {
		t.Fatalf("claim winners=%d", len(winners))
	}
	lease := <-winners
	ready, err := store.RecordArtifactReady(ctx, lease, "74444444-4444-4444-8444-444444444444", "101", now.Add(time.Second), now.Add(time.Minute))
	if err != nil || ready.State != StateReady || ready.LastObservedAt == nil {
		t.Fatalf("ready=%#v err=%v", ready, err)
	}
	healthy, err := store.ActiveArtifactsHealthy(ctx, now)
	if err != nil || !healthy {
		t.Fatalf("healthy=%t err=%v", healthy, err)
	}

	rotated := desired
	rotated.ProfileRevision = 2
	rotated.SecretName = SecretName(namespace, targetID, 2)
	rotatedArtifact, err := store.EnsureArtifact(ctx, rotated, now.Add(2*time.Minute))
	if err != nil || !rotatedArtifact.Active {
		t.Fatalf("rotated=%#v err=%v", rotatedArtifact, err)
	}
	old, err := store.Artifact(ctx, desired.ArtifactKey)
	if err != nil || old.Active || old.State != StateReady {
		t.Fatalf("retained old=%#v err=%v", old, err)
	}
	recoveryDigest := "sha256:" + stringsOf('c', 64)
	recoveryLease, found, err := store.ClaimArtifact(ctx, "registry-pull-integration:recovery", RuntimeContract,
		recoveryDigest, now.Add(2*time.Minute), time.Minute)
	if err != nil || !found {
		t.Fatalf("recovery claim=%#v found=%t err=%v", recoveryLease, found, err)
	}
	failed, err := store.RecordArtifactRetry(ctx, recoveryLease, profileMismatchFailureCode, true,
		now.Add(4*time.Minute), now.Add(2*time.Minute+time.Second))
	if err != nil || failed.State != StateFailed {
		t.Fatalf("profile mismatch=%#v err=%v", failed, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE runtime_registry_pull_artifacts
		SET runtime_state='ready',last_failure_code='',consecutive_failures=0,
		    last_observed_at=$4,observed_uid='76666666-6666-4666-8666-666666666666',
		    observed_resource_version='forged',updated_at=$4
		WHERE environment_id=$1 AND registry_target_id=$2 AND profile_revision=$3`,
		environmentID, targetID, int64(2), now.Add(3*time.Minute)); err == nil {
		t.Fatal("database accepted unleased profile-mismatch recovery")
	}
	reclaimed, found, err := store.ClaimArtifact(ctx, "registry-pull-integration:recovered", RuntimeContract,
		recoveryDigest, now.Add(4*time.Minute), time.Minute)
	if err != nil || !found || reclaimed.Artifact.LastFailureCode != profileMismatchFailureCode {
		t.Fatalf("reclaimed=%#v found=%t err=%v", reclaimed, found, err)
	}
	if _, err = store.RecordArtifactReady(ctx, reclaimed, "75555555-5555-4555-8555-555555555555", "102",
		now.Add(4*time.Minute+time.Second), now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	terminalLease, found, err := store.ClaimArtifact(ctx, "registry-pull-integration:terminal", RuntimeContract,
		recoveryDigest, now.Add(5*time.Minute), time.Minute)
	if err != nil || !found {
		t.Fatalf("terminal claim=%#v found=%t err=%v", terminalLease, found, err)
	}
	terminal, err := store.RecordArtifactRetry(ctx, terminalLease, "secret-mutation", true,
		now.Add(7*time.Minute), now.Add(5*time.Minute+time.Second))
	if err != nil || terminal.State != StateFailed {
		t.Fatalf("terminal artifact=%#v err=%v", terminal, err)
	}
	if _, found, err = store.ClaimArtifact(ctx, "registry-pull-integration:forged", RuntimeContract,
		recoveryDigest, now.Add(7*time.Minute), time.Minute); err != nil || found {
		t.Fatalf("other permanent failure became claimable: found=%t err=%v", found, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE runtime_registry_pull_artifacts
		SET runtime_state='ready',updated_at=$4
		WHERE environment_id=$1 AND registry_target_id=$2 AND profile_revision=$3`,
		environmentID, targetID, int64(2), now.Add(7*time.Minute)); err == nil {
		t.Fatal("database accepted recovery of a non-profile permanent failure")
	}
	if _, err = pool.Exec(ctx, `UPDATE runtime_registry_pull_artifacts SET namespace='default'
		WHERE environment_id=$1 AND registry_target_id=$2 AND profile_revision=2`, environmentID, targetID); err == nil {
		t.Fatal("database accepted runtime pull scope mutation")
	}

	readiness := Readiness{WorkerID: "registry-pull-integration:worker", WorkerEpoch: 1, Contract: RuntimeContract,
		ConfigDigest: digest, ProfileCount: 1, StartedAt: now, ObservedAt: now, LeaseUntil: now.Add(time.Minute)}
	if err = store.RecordReadiness(ctx, readiness); err != nil {
		t.Fatal(err)
	}
	if err = store.RuntimeReady(ctx, RuntimeContract, digest, 1, now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = store.RuntimeReady(ctx, RuntimeContract, digest, 1, now.Add(time.Minute)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expired readiness=%v", err)
	}
	readiness.WorkerEpoch = 3
	if err = store.RecordReadiness(ctx, readiness); !errors.Is(err, ErrConflict) {
		t.Fatalf("skipped readiness epoch=%v", err)
	}
}
