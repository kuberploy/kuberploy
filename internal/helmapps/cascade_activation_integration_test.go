package helmapps

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/argo"
	"github.com/kuberploy/kuberploy/internal/testdb"
)

func TestPostgresCascadeActivationRotatesComponentwise(t *testing.T) {
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
	var activationSettings []string
	if err = pool.QueryRow(ctx, `SELECT proconfig FROM pg_catalog.pg_proc
		WHERE oid='public.validate_helm_application_cascade_observer_activation()'::pg_catalog.regprocedure`).
		Scan(&activationSettings); err != nil {
		t.Fatal(err)
	}
	if len(activationSettings) != 1 || activationSettings[0] != "search_path=pg_catalog, pg_temp" {
		t.Fatalf("activation validator search path=%v", activationSettings)
	}
	t.Cleanup(func() {
		// Publisher bootstrap is intentionally global. Do not leave this test's
		// still-live readiness rows able to outrank a later fixture that uses the
		// same shared disposable database.
		_, _ = pool.Exec(context.Background(), `DELETE FROM public.runtime_readiness
			WHERE worker_id LIKE 'helm-cascade-rotation-worker-%'
			   OR worker_id LIKE 'argo-cascade-rotation-worker-%'`)
	})

	fixture := newHelmReleasePGFixture()
	fixture.now = helmPGDatabaseNow(t, ctx, pool).Add(-10 * time.Minute).Truncate(time.Microsecond)
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if _, cleanupErr := pool.Exec(cleanupCtx, `ALTER TABLE public.helm_application_cascade_observer_activations DISABLE TRIGGER USER`); cleanupErr != nil {
			t.Errorf("disable cascade activation cleanup trigger: %v", cleanupErr)
			return
		}
		if _, cleanupErr := pool.Exec(cleanupCtx, `DELETE FROM public.helm_application_cascade_observer_activations WHERE platform_binding_id=$1`, fixture.platformBindingID); cleanupErr != nil {
			t.Errorf("clean up cascade activation: %v", cleanupErr)
		}
		if _, cleanupErr := pool.Exec(cleanupCtx, `ALTER TABLE public.helm_application_cascade_observer_activations ENABLE TRIGGER USER`); cleanupErr != nil {
			t.Errorf("restore cascade activation cleanup trigger: %v", cleanupErr)
		}
		if _, cleanupErr := pool.Exec(cleanupCtx, `ALTER TABLE public.helm_chart_approval_documents DISABLE TRIGGER USER`); cleanupErr != nil {
			t.Errorf("disable Helm approval cleanup trigger: %v", cleanupErr)
			return
		}
		if _, cleanupErr := pool.Exec(cleanupCtx, `DELETE FROM helm_chart_approval_documents WHERE approval_id=$1`, fixture.approvalID); cleanupErr != nil {
			t.Errorf("clean up cascade approval documents: %v", cleanupErr)
		}
		if _, cleanupErr := pool.Exec(cleanupCtx, `DELETE FROM helm_chart_approvals WHERE approval_id=$1`, fixture.approvalID); cleanupErr != nil {
			t.Errorf("clean up cascade approval: %v", cleanupErr)
		}
		if _, cleanupErr := pool.Exec(cleanupCtx, `ALTER TABLE public.helm_chart_approval_documents ENABLE TRIGGER USER`); cleanupErr != nil {
			t.Errorf("restore Helm approval cleanup trigger: %v", cleanupErr)
		}
	})
	setup, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	setupHelmReleasePGFixture(t, ctx, setup, fixture)
	if err = setup.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	authorityAt := helmPGDatabaseNow(t, ctx, pool)
	argoStarted := authorityAt.Add(-2 * time.Minute).Truncate(time.Microsecond)
	argoIdentity, argoObservation := helmPGArgoObservation(t, fixture,
		"argo-cascade-rotation-worker-0001", argoStarted)
	argoObservation.ObservedAt = authorityAt
	argoStore, err := argo.NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	argoLease, err := argoStore.AcquireDesiredStateReadiness(ctx, argoObservation, 5*time.Minute)
	if err != nil || argoLease.Epoch != 1 {
		t.Fatalf("initial Argo readiness=%+v err=%v", argoLease, err)
	}
	store, err := NewPostgresProtectedPublicationStoreWithCascade(pool, helmPGArgoAuthority(), argoObservation)
	if err != nil {
		t.Fatal(err)
	}
	publisher := ProtectedPublisherIdentity{Contract: ProtectedPublisherContract,
		PolicyVersion: ProtectedGitPolicy, ConfigDigest: fixture.publisherDigest}
	publisherWorker := "helm-cascade-rotation-worker-0001"
	publisherStarted := authorityAt.Add(-2 * time.Minute).Truncate(time.Microsecond)
	putPublisher := func(worker string, epoch int64, identity ProtectedPublisherIdentity,
		startedAt, observedAt time.Time) {
		t.Helper()
		if putErr := store.PutPublisherReadiness(ctx, ProtectedPublisherReadiness{
			WorkerID: worker, WorkerEpoch: epoch, Publisher: identity,
			StartedAt: startedAt, ObservedAt: observedAt, LeaseUntil: observedAt.Add(5 * time.Minute),
		}); putErr != nil {
			t.Fatal(putErr)
		}
	}
	putPublisher(publisherWorker, 1, publisher, publisherStarted, authorityAt)

	activate := func(worker string, epoch int64, identity ProtectedPublisherIdentity) int64 {
		t.Helper()
		activationEpoch, activateErr := store.ActivateCascadeObserver(ctx, worker, epoch,
			identity, helmPGDatabaseNow(t, ctx, pool))
		if activateErr != nil {
			t.Fatal(activateErr)
		}
		return activationEpoch
	}
	if got := activate(publisherWorker, 1, publisher); got != 1 {
		t.Fatalf("initial activation epoch=%d", got)
	}
	if got := activate(publisherWorker, 1, publisher); got != 1 {
		t.Fatalf("idempotent activation epoch=%d", got)
	}

	// Argo intentionally reacquires a new readiness epoch after a transient
	// production-prerequisite loss without restarting the process.
	argoObservation.ObservedAt = helmPGDatabaseNow(t, ctx, pool)
	argoLease, err = argoStore.AcquireDesiredStateReadiness(ctx, argoObservation, 5*time.Minute)
	if err != nil || argoLease.Epoch != 2 {
		t.Fatalf("Argo reacquire=%+v err=%v", argoLease, err)
	}
	if got := activate(publisherWorker, 1, publisher); got != 2 {
		t.Fatalf("Argo-only rotation activation epoch=%d", got)
	}

	// The symmetric publisher readiness epoch renewal is also monotonic.
	publisherAt := helmPGDatabaseNow(t, ctx, pool)
	putPublisher(publisherWorker, 2, publisher, publisherStarted, publisherAt)
	if got := activate(publisherWorker, 2, publisher); got != 3 {
		t.Fatalf("publisher-only rotation activation epoch=%d", got)
	}

	// Both components may advance before the next activation attempt.
	argoObservation.ObservedAt = helmPGDatabaseNow(t, ctx, pool)
	argoLease, err = argoStore.AcquireDesiredStateReadiness(ctx, argoObservation, 5*time.Minute)
	if err != nil || argoLease.Epoch != 3 {
		t.Fatalf("second Argo reacquire=%+v err=%v", argoLease, err)
	}
	publisherAt = helmPGDatabaseNow(t, ctx, pool)
	putPublisher(publisherWorker, 3, publisher, publisherStarted, publisherAt)
	if got := activate(publisherWorker, 3, publisher); got != 4 {
		t.Fatalf("combined rotation activation epoch=%d", got)
	}
	if got := activate(publisherWorker, 3, publisher); got != 4 {
		t.Fatalf("combined idempotent activation epoch=%d", got)
	}

	assertExact := func(publisherEpoch, argoEpoch int64, want bool) {
		t.Helper()
		var exact bool
		if queryErr := pool.QueryRow(ctx, `SELECT public.helm_application_cascade_active_observer_is_exact(
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,pg_catalog.clock_timestamp())`,
			fixture.platformBindingID, publisherWorker, publisherEpoch, publisher.Contract,
			publisher.PolicyVersion, publisher.ConfigDigest, argoObservation.WorkerID, argoEpoch,
			argoIdentity.ContractVersion, argoIdentity.ConfigDigest).Scan(&exact); queryErr != nil {
			t.Fatal(queryErr)
		}
		if exact != want {
			t.Fatalf("exact publisher epoch=%d Argo epoch=%d: got %v want %v", publisherEpoch, argoEpoch, exact, want)
		}
	}
	assertExact(3, 3, true)
	assertExact(2, 3, false)
	assertExact(3, 2, false)

	// Equal-start different-worker substitutions are neither the same process
	// nor a later process, on both sides.
	alternatePublisher := "helm-cascade-rotation-worker-0002"
	putPublisher(alternatePublisher, 1, publisher, publisherStarted,
		helmPGDatabaseNow(t, ctx, pool))
	if _, activationErr := store.ActivateCascadeObserver(ctx, alternatePublisher, 1,
		publisher, helmPGDatabaseNow(t, ctx, pool)); !errors.Is(activationErr, ErrConflict) {
		t.Fatalf("equal-start publisher substitution accepted: %v", activationErr)
	}
	alternateArgo := argoObservation
	alternateArgo.WorkerID = "argo-cascade-rotation-worker-0002"
	alternateArgo.ObservedAt = helmPGDatabaseNow(t, ctx, pool)
	alternateArgoLease, acquireErr := argoStore.AcquireDesiredStateReadiness(ctx, alternateArgo, 5*time.Minute)
	if acquireErr != nil {
		t.Fatal(acquireErr)
	}
	var ignoredEpoch int64
	directErr := pool.QueryRow(ctx, `SELECT public.activate_helm_application_cascade_observer(
		$1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, fixture.platformBindingID,
		publisher.Contract, publisher.PolicyVersion, publisher.ConfigDigest, publisherWorker, int64(3),
		argoIdentity.ContractVersion, argoIdentity.ConfigDigest, alternateArgo.WorkerID,
		alternateArgoLease.Epoch).Scan(&ignoredEpoch)
	if !postgresCheckViolation(directErr) {
		t.Fatalf("equal-start Argo substitution accepted: %v", directErr)
	}

	// Same-process config drift with a higher epoch is not a readiness renewal.
	driftTx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	driftDigest := helmPGDigest([]byte("same-process-config-drift"))
	_, err = driftTx.Exec(ctx, `UPDATE public.runtime_readiness SET worker_epoch=4,
		config_digest=$2,observed_at=stamp.at,
		lease_until=stamp.at+interval '1 minute',updated_at=stamp.at
		FROM (SELECT pg_catalog.clock_timestamp() AS at) AS stamp
		WHERE runtime_kind='helm-protected-publisher' AND scope_key='global' AND worker_id=$1`,
		publisherWorker, driftDigest)
	if err == nil {
		err = driftTx.QueryRow(ctx, `SELECT public.activate_helm_application_cascade_observer(
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, fixture.platformBindingID,
			publisher.Contract, publisher.PolicyVersion, driftDigest, publisherWorker, int64(4),
			argoIdentity.ContractVersion, argoIdentity.ConfigDigest, argoObservation.WorkerID,
			argoLease.Epoch).Scan(&ignoredEpoch)
	}
	_ = driftTx.Rollback(ctx)
	if !postgresCheckViolation(err) {
		t.Fatalf("same-process config drift accepted: %v", err)
	}
	argoDriftTx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	argoDriftDigest := helmPGDigest([]byte("same-process-argo-config-drift"))
	_, err = argoDriftTx.Exec(ctx, `UPDATE public.runtime_readiness SET worker_epoch=4,
		config_digest=$2,identity=jsonb_set(identity,'{chartVersion}','"drift"'::jsonb),
		observed_at=stamp.at,lease_until=stamp.at+interval '1 minute',updated_at=stamp.at
		FROM (SELECT pg_catalog.clock_timestamp() AS at) AS stamp
		WHERE runtime_kind='argo-desired-state' AND scope_key='global' AND worker_id=$1`,
		argoObservation.WorkerID, argoDriftDigest)
	if err == nil {
		err = argoDriftTx.QueryRow(ctx, `SELECT public.activate_helm_application_cascade_observer(
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, fixture.platformBindingID,
			publisher.Contract, publisher.PolicyVersion, publisher.ConfigDigest, publisherWorker, int64(3),
			argoIdentity.ContractVersion, argoDriftDigest, argoObservation.WorkerID, int64(4)).Scan(&ignoredEpoch)
	}
	_ = argoDriftTx.Rollback(ctx)
	if !postgresCheckViolation(err) {
		t.Fatalf("same-process Argo config drift accepted: %v", err)
	}

	// A genuinely later process may change config, and another still-later
	// process may deliberately roll it back. Neither prior process can return.
	next := publisher
	next.ConfigDigest = helmPGDigest([]byte("next-publisher-config"))
	nextWorker := "helm-cascade-rotation-worker-0003"
	nextStarted := publisherStarted.Add(time.Minute)
	putPublisher(nextWorker, 1, next, nextStarted, helmPGDatabaseNow(t, ctx, pool))
	if got := activate(nextWorker, 1, next); got != 5 {
		t.Fatalf("later process activation epoch=%d", got)
	}
	if _, activationErr := store.ActivateCascadeObserver(ctx, publisherWorker, 3,
		publisher, helmPGDatabaseNow(t, ctx, pool)); !errors.Is(activationErr, ErrConflict) {
		t.Fatalf("old process A returned after B: %v", activationErr)
	}
	rollbackWorker := "helm-cascade-rotation-worker-0004"
	rollbackStarted := nextStarted.Add(30 * time.Second)
	putPublisher(rollbackWorker, 1, publisher, rollbackStarted, helmPGDatabaseNow(t, ctx, pool))
	if got := activate(rollbackWorker, 1, publisher); got != 6 {
		t.Fatalf("later rollback process activation epoch=%d", got)
	}
	if _, activationErr := store.ActivateCascadeObserver(ctx, nextWorker, 1,
		next, helmPGDatabaseNow(t, ctx, pool)); !errors.Is(activationErr, ErrConflict) {
		t.Fatalf("old process B returned after later rollback: %v", activationErr)
	}
}
