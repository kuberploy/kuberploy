package certissuers

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/testdb"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/id"
)

func TestPostgresObserverReadinessFencesIdentityTargetsAndLeaseEpoch(t *testing.T) {
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
	store, err := NewPostgresStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	config := ObserverConfig{Enabled: true, BindingID: id.New(), ClusterID: id.New(), Namespace: "kuberploy-system",
		ServiceAccount: "kuberploy-worker", PollInterval: 5 * time.Second, RequestTimeout: time.Second,
		MaximumAge: 30 * time.Second, ReadinessLease: 30 * time.Second}
	identity, err := ObserverIdentityForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	targetA, targetB := digestText("observer-target-a"), digestText("observer-target-b")
	first := ObserverWorkerObservation{WorkerID: "issuer-observer-a-" + config.BindingID, Identity: identity, TargetDigest: targetA,
		TargetCount: 1, StartedAt: now, ObservedAt: now}
	lease, err := store.AcquireObserverReadiness(ctx, first, config.ReadinessLease)
	if err != nil || lease.Epoch != 1 {
		t.Fatalf("first lease=%#v err=%v", lease, err)
	}
	if err = store.ObserverRuntimeReady(ctx, identity, targetA, 1, now.Add(time.Second), config.MaximumAge); err != nil {
		t.Fatalf("exact observer readiness: %v", err)
	}
	takeover := first
	takeover.WorkerID = "issuer-observer-b-" + config.BindingID
	takeover.StartedAt = now.Add(time.Second)
	takeover.ObservedAt = now.Add(time.Second)
	if _, err = store.AcquireObserverReadiness(ctx, takeover, config.ReadinessLease); !errors.Is(err, ErrObserverLeaseLost) {
		t.Fatalf("active observer lease takeover err=%v", err)
	}
	heartbeat := first
	heartbeat.TargetDigest, heartbeat.TargetCount, heartbeat.ObservedAt = targetB, 2, now.Add(2*time.Second)
	updated, err := store.HeartbeatObserverReadiness(ctx, lease, heartbeat, config.ReadinessLease)
	if err != nil || updated.Epoch != lease.Epoch || updated.TargetDigest != targetB {
		t.Fatalf("heartbeat=%#v err=%v", updated, err)
	}
	if err = store.ObserverRuntimeReady(ctx, identity, targetA, 1, now.Add(3*time.Second), config.MaximumAge); !errors.Is(err, ErrObservationUnavailable) {
		t.Fatalf("old target identity remained ready: %v", err)
	}
	if err = store.ObserverRuntimeReady(ctx, identity, targetB, 2, now.Add(3*time.Second), config.MaximumAge); err != nil {
		t.Fatalf("new target identity not ready: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE cert_manager_issuer_observer_readiness SET config_digest=$2 WHERE config_digest=$1`, identity.ConfigDigest, digestText("substituted-config")); !isPGCode(err, "23514") {
		t.Fatalf("active config substitution err=%v", err)
	}
	if _, err = pool.Exec(ctx, `DELETE FROM cert_manager_issuer_observer_readiness WHERE config_digest=$1`, identity.ConfigDigest); !isPGCode(err, "23514") {
		t.Fatalf("readiness deletion err=%v", err)
	}

	// A separately keyed, already-expired lease proves the sole legal reclaim
	// transition without waiting in the test.
	expiredConfig := config
	expiredConfig.BindingID = id.New()
	expiredIdentity, err := ObserverIdentityForConfig(expiredConfig)
	if err != nil {
		t.Fatal(err)
	}
	past := now.Add(-2 * time.Minute)
	expiredObservation := ObserverWorkerObservation{WorkerID: "issuer-observer-expired-a-" + expiredConfig.BindingID,
		Identity: expiredIdentity, TargetDigest: targetA, TargetCount: 1, StartedAt: past, ObservedAt: past}
	expiredLease, err := store.AcquireObserverReadiness(ctx, expiredObservation, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	replacement := expiredObservation
	replacement.WorkerID = "issuer-observer-expired-b-" + expiredConfig.BindingID
	replacement.StartedAt, replacement.ObservedAt = now.Add(-time.Second), now
	reclaimed, err := store.AcquireObserverReadiness(ctx, replacement, 30*time.Second)
	if err != nil || reclaimed.Epoch != expiredLease.Epoch+1 {
		t.Fatalf("reclaimed=%#v err=%v", reclaimed, err)
	}
	stale := expiredObservation
	stale.ObservedAt = now
	if _, err = store.HeartbeatObserverReadiness(ctx, expiredLease, stale, 30*time.Second); !errors.Is(err, ErrObserverLeaseLost) {
		t.Fatalf("stale epoch heartbeat err=%v", err)
	}
}
