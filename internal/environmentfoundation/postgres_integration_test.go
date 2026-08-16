package environmentfoundation

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/testdb"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresFoundationFencingAndExactReadiness(t *testing.T) {
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
	if err = testdb.ApplyMigrations(ctx, pool); err != nil {
		t.Fatal(err)
	}
	// Prove the stable initial schema remains idempotent.
	if err = testdb.ApplyMigrations(ctx, pool); err != nil {
		t.Fatal(err)
	}
	cleanup := func(c context.Context) {
		_, _ = pool.Exec(c, `DELETE FROM runtime_readiness
			WHERE runtime_kind='environment-foundation' AND scope_key='global' AND worker_id IN ($1,$2)`, testWorker1, testWorker2)
		_, _ = pool.Exec(c, `DELETE FROM environment_foundation_intents WHERE environment_id=$1`, testEnvironmentID)
		_, _ = pool.Exec(c, `DELETE FROM git_repository_bindings WHERE id=$1`, testBindingID)
		_, _ = pool.Exec(c, `DELETE FROM environments WHERE id=$1`, testEnvironmentID)
		_, _ = pool.Exec(c, `DELETE FROM projects WHERE id=$1`, testProjectID)
	}
	cleanup(ctx)
	defer cleanup(context.Background())
	// Keep a sub-microsecond component so claims and heartbeats prove that the
	// store returns PostgreSQL's authoritative timestamptz precision.
	now := time.Now().UTC().Truncate(time.Microsecond).Add(789 * time.Nanosecond)
	if _, err = pool.Exec(ctx, `INSERT INTO projects(id,name,slug,created_at) VALUES($1,'Foundation test','foundation-test',$2)`, testProjectID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at) VALUES($1,$2,'Development','dev','kp-demo-dev','kp-demo-dev',$3)`, testEnvironmentID, testProjectID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO git_repository_bindings(
		id,kind,scope_id,cluster_id,provider,installation_id,repository_id,repository_owner,
		repository_name,target_ref,path_prefix,credential_secret_name,credential_mode,state,
		target_head_revision,indexed_revision,projection_generation,parser_version,
		target_head_observed_at,indexed_at,created_at,updated_at)
		VALUES($1,'platform',$2::uuid,$2::uuid,'github',1,2,'kuberploy','platform','refs/heads/main',
		'clusters/'||$2::text,'','github-app','ready',$3,$3,7,'appconfig.v1',$4,$4,$4,$4)`, testBindingID, testClusterID, strings.Repeat("a", 40), now); err != nil {
		t.Fatal(err)
	}
	store, err := NewPostgresStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	profile := testProfile()
	profileDigest, _ := profile.Digest()
	intent, err := store.EnsureIntent(ctx, EnsureRequest{testIntentID, testEnvironmentID, profile, now})
	if err != nil {
		t.Fatal(err)
	}
	if intent.ProjectID != testProjectID || intent.Namespace != "kp-demo-dev" || intent.Authority.BindingID != testBindingID {
		t.Fatalf("authority was not derived: project=%s namespace=%s binding=%s", intent.ProjectID, intent.Namespace, intent.Authority.BindingID)
	}
	// A retry of the same immutable command must return its frozen authority
	// even if the protected branch advances between attempts.
	if _, err = pool.Exec(ctx, `UPDATE git_repository_bindings SET target_head_revision=$2,
		indexed_revision=$2,projection_generation=8,target_head_observed_at=$3,indexed_at=$3,updated_at=$3
		WHERE id=$1`, testBindingID, strings.Repeat("d", 40), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	replayed, err := store.EnsureIntent(ctx, EnsureRequest{testIntentID, testEnvironmentID, profile, now.Add(2 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Authority.PlannedHead != strings.Repeat("a", 40) || replayed.IntentDigest != intent.IntentDigest {
		t.Fatalf("retry rebound immutable authority: %s", replayed.Authority.PlannedHead)
	}
	pendingRotation := profile
	pendingRotation.ObserverServiceAccount = "kuberploy-api-rotated"
	deferred, err := store.EnsureIntent(ctx, EnsureRequest{testIntentID2, testEnvironmentID, pendingRotation, now.Add(2 * time.Second)})
	if err != nil || deferred.ID != intent.ID {
		t.Fatalf("PostgreSQL superseded a nonterminal predecessor: %#v err=%v", deferred, err)
	}
	pendingDigest, _ := pendingRotation.Digest()
	legacyLease, legacyFound, err := store.ClaimIntent(ctx, testWorker1, pendingDigest, pendingRotation.PublisherConfigDigest, now.Add(time.Second), MinimumLease)
	if err != nil || !legacyFound || legacyLease.Intent.ID != intent.ID || legacyLease.Intent.ProfileDigest == pendingDigest {
		t.Fatalf("rotated runtime could not recover the PostgreSQL predecessor: %#v found=%v err=%v", legacyLease, legacyFound, err)
	}
	if _, err = store.RecordRetry(ctx, legacyLease, "release-for-main-test", false, now.Add(time.Second), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	first, found, err := store.ClaimIntent(ctx, testWorker1, profileDigest, profile.PublisherConfigDigest, now.Add(time.Second), MinimumLease)
	if err != nil || !found {
		t.Fatalf("first claim: found=%v err=%v", found, err)
	}
	first, err = store.HeartbeatIntent(ctx, first, now.Add(2*time.Second), MinimumLease)
	if err != nil || first.Validate(now.Add(2*time.Second)) != nil {
		t.Fatalf("fractional-time heartbeat returned an invalid lease: %#v %v", first, err)
	}
	second, found, err := store.ClaimIntent(ctx, testWorker2, profileDigest, profile.PublisherConfigDigest, first.Until, MinimumLease)
	if err != nil || !found || second.Epoch != first.Epoch+1 {
		t.Fatalf("recovery: epoch=%d found=%v err=%v", second.Epoch, found, err)
	}
	if _, err = store.RecordRetry(ctx, first, "stale-worker", false, first.Until.Add(time.Minute), first.Until.Add(time.Second)); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale finalizer accepted: %v", err)
	}
	bound, err := store.BindWriteBase(ctx, second, strings.Repeat("a", 40), first.Until.Add(time.Second), first.Until.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	second.Intent = bound
	receipt := PublicationReceipt{IntentID: intent.ID, BindingID: testBindingID, TargetRef: "refs/heads/main", Path: intent.Path, ContentDigest: intent.ManifestDigest, ParentRevision: strings.Repeat("a", 40), CommittedRevision: strings.Repeat("b", 40), ProviderRequest: "github:pg-1", ObservedAt: first.Until.Add(time.Second)}
	ready, err := store.RecordReady(ctx, second, receipt, first.Until.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if ready.State != StateReady {
		t.Fatalf("state=%s", ready.State)
	}
	r := Readiness{testWorker2, 1, Contract, profileDigest, profile.PublisherConfigDigest, 1, now, first.Until.Add(2 * time.Second), first.Until.Add(time.Minute)}
	if err = store.RecordReadiness(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err = store.ExactReady(ctx, profileDigest, profile.PublisherConfigDigest, 1, first.Until.Add(3*time.Second)); err != nil {
		t.Fatalf("exact readiness: %v", err)
	}
	regressed := r
	regressed.ObservedAt = now
	if err = store.RecordReadiness(ctx, regressed); !errors.Is(err, ErrConflict) {
		t.Fatalf("regressed heartbeat accepted: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE environment_foundation_intents SET namespace='attacker',updated_at=$2 WHERE id=$1`, intent.ID, first.Until.Add(3*time.Second)); err == nil {
		t.Fatal("database trigger accepted identity rewrite")
	}
	if err = store.ExactReady(ctx, profileDigest, profile.PublisherConfigDigest, 1, first.Until.Add(2*time.Minute)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expired readiness accepted: %v", err)
	}
	changed := profile
	changed.ObserverServiceAccount = "kuberploy-api-rotated"
	replacement, err := store.EnsureIntent(ctx, EnsureRequest{testIntentID2, testEnvironmentID, changed, first.Until.Add(3 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	expected, found, err := store.ExpectedPreimage(ctx, replacement.ID)
	if err != nil || !found || expected != ready.ManifestDigest {
		t.Fatalf("exact PostgreSQL foundation preimage was not retained: digest=%q found=%v err=%v", expected, found, err)
	}
}
