package helmapps

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/testdb"
)

func TestPostgresHelmReadinessPrunesOnlyExpiredSamePodProcesses(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = testdb.ApplyMigrations(ctx, pool); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	suffix := id.New()
	oldWorker := "helm-pod-restart-old:" + suffix
	newWorker := "helm-pod-restart-new:" + suffix
	freshPeer := "helm-peer-fresh:" + suffix
	workers := []string{oldWorker, newWorker, freshPeer}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM runtime_readiness
			WHERE runtime_kind IN ('helm-renderer','helm-protected-publisher')
			  AND scope_key='global' AND worker_id=ANY($1::text[])`, workers)
	})

	operatorDigest := digestBytes([]byte("helm-readiness-retention-" + suffix))
	renderStore, err := NewPostgresStore(pool, operatorDigest)
	if err != nil {
		t.Fatal(err)
	}
	publisherStore, err := NewPostgresProtectedPublicationStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	publisher := ProtectedPublisherIdentity{Contract: ProtectedPublisherContract,
		PolicyVersion: ProtectedGitPolicy, ConfigDigest: digestBytes([]byte("helm-publisher-retention-" + suffix))}
	renderIdentity := ExpectedRenderWorkerIdentity(operatorDigest)

	putRenderer := func(worker string, startedAt, observedAt, leaseUntil time.Time) {
		t.Helper()
		if putErr := renderStore.PutReadiness(ctx, Readiness{WorkerID: worker, WorkerEpoch: 1,
			RenderWorkerIdentity: renderIdentity, StartedAt: startedAt, ObservedAt: observedAt, LeaseUntil: leaseUntil}); putErr != nil {
			t.Fatal(putErr)
		}
	}
	putPublisher := func(worker string, startedAt, observedAt, leaseUntil time.Time) {
		t.Helper()
		if putErr := publisherStore.PutPublisherReadiness(ctx, ProtectedPublisherReadiness{WorkerID: worker, WorkerEpoch: 1,
			Publisher: publisher, StartedAt: startedAt, ObservedAt: observedAt, LeaseUntil: leaseUntil}); putErr != nil {
			t.Fatal(putErr)
		}
	}

	oldLeaseUntil := now.Add(time.Minute)
	peerLeaseUntil := now.Add(10 * time.Minute)
	peerPublisherLeaseUntil := now.Add(4 * time.Minute)
	putRenderer(oldWorker, now, now, oldLeaseUntil)
	putPublisher(oldWorker, now, now, oldLeaseUntil)
	putRenderer(freshPeer, now, now, peerLeaseUntil)
	putPublisher(freshPeer, now, now, peerPublisherLeaseUntil)
	restartedAt := now
	putRenderer(newWorker, restartedAt, restartedAt, restartedAt.Add(time.Minute))
	putPublisher(newWorker, restartedAt, restartedAt, restartedAt.Add(time.Minute))
	assertHelmReadinessWorkers(t, ctx, pool, workers, 3, true)

	if _, err = pool.Exec(ctx, `DELETE FROM runtime_readiness
		WHERE runtime_kind IN ('helm-renderer','helm-protected-publisher')
		  AND scope_key='global' AND worker_id=$1`, oldWorker); err != nil {
		t.Fatal(err)
	}
	expiredObservedAt := now.Add(-2 * time.Minute)
	putRenderer(oldWorker, expiredObservedAt, expiredObservedAt, now.Add(-time.Minute))
	putPublisher(oldWorker, expiredObservedAt, expiredObservedAt, now.Add(-time.Minute))
	heartbeatAt := time.Now().UTC()
	putRenderer(newWorker, restartedAt, heartbeatAt, heartbeatAt.Add(time.Minute))
	putPublisher(newWorker, restartedAt, heartbeatAt, heartbeatAt.Add(time.Minute))
	assertHelmReadinessWorkers(t, ctx, pool, workers, 2, false)
}

func assertHelmReadinessWorkers(t testing.TB, ctx context.Context, pool *pgxpool.Pool, workers []string, wantCount int, oldPresent bool) {
	t.Helper()
	for _, runtimeKind := range []string{"helm-renderer", "helm-protected-publisher"} {
		var count int
		var foundOld bool
		err := pool.QueryRow(ctx, `SELECT count(*)::integer,
			COALESCE(bool_or(worker_id=$3),false)
			FROM runtime_readiness WHERE runtime_kind=$1 AND scope_key='global' AND worker_id=ANY($2::text[])`,
			runtimeKind, workers, workers[0]).Scan(&count, &foundOld)
		if err != nil || count != wantCount || foundOld != oldPresent {
			t.Fatalf("%s readiness count=%d oldPresent=%v err=%v", runtimeKind, count, foundOld, err)
		}
		var freshPeers int
		err = pool.QueryRow(ctx, `SELECT count(*)::integer FROM runtime_readiness
			WHERE runtime_kind=$1 AND scope_key='global' AND worker_id=ANY($2::text[])
			  AND worker_id IN ($3,$4)`, runtimeKind, workers, workers[1], workers[2]).Scan(&freshPeers)
		if err != nil || freshPeers != 2 {
			t.Fatalf("%s fresh process rows=%d err=%v", runtimeKind, freshPeers, err)
		}
	}
}
