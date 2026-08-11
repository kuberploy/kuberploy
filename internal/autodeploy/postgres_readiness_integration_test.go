package autodeploy_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/testdb"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/autodeploy"
)

func TestPostgreSQLAutoDeployRuntimeReadinessIsExactAndLeaseFenced(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL")
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
	store, err := autodeploy.NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	leaseDuration := 15 * time.Second
	identity := autodeploy.RuntimeIdentity{ContractVersion: autodeploy.RuntimeContractVersion, OperatorConfigDigest: "sha256:" + strings.Repeat("a", 64)}
	observation := autodeploy.RuntimeObservation{WorkerID: "worker-auto-deploy-readiness", RuntimeIdentity: identity, StartedAt: now, ObservedAt: now}
	lease, err := store.AcquireRuntimeReadiness(ctx, observation, leaseDuration)
	if err != nil || lease.Epoch != 1 {
		t.Fatalf("acquire lease=%#v err=%v", lease, err)
	}
	if err = store.RuntimeReady(ctx, identity, now.Add(time.Second), autodeploy.RuntimeReadinessMaxAge); err != nil {
		t.Fatalf("exact runtime not ready: %v", err)
	}
	wrong := identity
	wrong.OperatorConfigDigest = "sha256:" + strings.Repeat("b", 64)
	if err = store.RuntimeReady(ctx, wrong, now.Add(time.Second), autodeploy.RuntimeReadinessMaxAge); !errors.Is(err, autodeploy.ErrRuntimeNotReady) {
		t.Fatalf("substituted config became ready: %v", err)
	}

	heartbeatAt := now.Add(time.Second)
	lease, err = store.HeartbeatRuntimeReadiness(ctx, lease, heartbeatAt, leaseDuration)
	if err != nil || lease.Epoch != 1 || !lease.ObservedAt.Equal(heartbeatAt) {
		t.Fatalf("heartbeat lease=%#v err=%v", lease, err)
	}
	stale := lease
	stale.Epoch++
	if _, err = store.HeartbeatRuntimeReadiness(ctx, stale, heartbeatAt.Add(time.Second), leaseDuration); !errors.Is(err, autodeploy.ErrLeaseLost) {
		t.Fatalf("stale epoch heartbeat accepted: %v", err)
	}

	expiredAt := lease.Until.Add(time.Second)
	observation.ObservedAt = expiredAt
	reclaimed, err := store.AcquireRuntimeReadiness(ctx, observation, leaseDuration)
	if err != nil || reclaimed.Epoch != 2 {
		t.Fatalf("expired reacquire lease=%#v err=%v", reclaimed, err)
	}
	if _, err = store.HeartbeatRuntimeReadiness(ctx, lease, expiredAt.Add(time.Second), leaseDuration); !errors.Is(err, autodeploy.ErrLeaseLost) {
		t.Fatalf("old lease survived reacquire: %v", err)
	}

	_, err = pool.Exec(ctx, `UPDATE runtime_readiness SET config_digest=$2
		WHERE runtime_kind='auto-deploy' AND scope_key='global' AND worker_id=$1`, observation.WorkerID, wrong.OperatorConfigDigest)
	assertPGCode(t, err, "23514")
	_, err = pool.Exec(ctx, `UPDATE runtime_readiness SET observed_at=$2,lease_until=$3
		WHERE runtime_kind='auto-deploy' AND scope_key='global' AND worker_id=$1`, observation.WorkerID, time.Now().UTC().Add(time.Minute), time.Now().UTC().Add(2*time.Minute))
	assertPGCode(t, err, "23514")
}
