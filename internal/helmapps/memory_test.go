package helmapps

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func preparedMemoryStore(t *testing.T) (*MemoryStore, Approval, DesiredRender) {
	t.Helper()
	files := testChartFiles()
	approval := testApproval(t, packageChart(t, files), files)
	desired := testDesired(t, approval, []byte("replicas: 2\n"))
	store := NewMemoryStore()
	if _, _, err := store.PutApproval(context.Background(), approval); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Submit(context.Background(), desired, testTime); err != nil {
		t.Fatal(err)
	}
	return store, approval, desired
}

func TestMemoryStoreExactIdempotency(t *testing.T) {
	ctx := context.Background()
	files := testChartFiles()
	approval := testApproval(t, packageChart(t, files), files)
	store := NewMemoryStore()
	stored, replay, err := store.PutApproval(ctx, approval)
	if err != nil || replay || !stored.replayEqual(approval) {
		t.Fatalf("initial approval: %+v %v %v", stored, replay, err)
	}
	if _, replay, err = store.PutApproval(ctx, approval); err != nil || !replay {
		t.Fatalf("approval replay: %v %v", replay, err)
	}
	conflictingApproval := approval
	conflictingApproval.ChartVersion = "1.2.4"
	if _, _, err = store.PutApproval(ctx, conflictingApproval); !errors.Is(err, ErrConflict) {
		t.Fatalf("approval idempotency conflict not detected: %v", err)
	}

	desired := testDesired(t, approval, []byte("replicas: 2\n"))
	command, replay, err := store.Submit(ctx, desired, testTime)
	if err != nil || replay || command.State != StateQueued {
		t.Fatalf("initial command: %+v %v %v", command, replay, err)
	}
	if _, replay, err = store.Submit(ctx, desired, testTime.Add(time.Hour)); err != nil || !replay {
		t.Fatalf("command replay: %v %v", replay, err)
	}
	conflictingDesired, err := NewDesiredRender(testCommandID, testScopeID, "render-create-0001", approval, testDestination(), []byte("replicas: 3\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.Submit(ctx, conflictingDesired, testTime); !errors.Is(err, ErrConflict) {
		t.Fatalf("command idempotency conflict not detected: %v", err)
	}
}

func TestMemoryStoreClaimsExclusivelyAndFencesExpiredLease(t *testing.T) {
	store, _, _ := preparedMemoryStore(t)
	ctx := context.Background()
	runtime := ExpectedRenderWorkerIdentity(store.operatorConfigDigest)
	owners := []string{"helm-renderer-0001", "helm-renderer-0002"}
	type claimResult struct {
		lease RenderLease
		err   error
	}
	results := make(chan claimResult, len(owners))
	var group sync.WaitGroup
	for _, owner := range owners {
		group.Add(1)
		go func(owner string) {
			defer group.Done()
			lease, err := store.Claim(ctx, owner, runtime, testTime, time.Minute)
			results <- claimResult{lease, err}
		}(owner)
	}
	group.Wait()
	close(results)
	var first RenderLease
	winners, empty := 0, 0
	for result := range results {
		if result.err == nil {
			first, winners = result.lease, winners+1
		} else if errors.Is(result.err, ErrNotFound) {
			empty++
		} else {
			t.Fatalf("claim error: %v", result.err)
		}
	}
	if winners != 1 || empty != 1 || first.Epoch != 1 || first.Command.Attempts != 1 {
		t.Fatalf("exclusive claim failed: winners=%d empty=%d lease=%+v", winners, empty, first)
	}
	recoveredAt := first.Until
	second, err := store.Claim(ctx, "helm-renderer-0003", runtime, recoveredAt, time.Minute)
	if err != nil || second.Epoch != 2 || second.Command.Attempts != 2 || second.Command.LastFailureCode != "renderer-lease-expired" {
		t.Fatalf("reclaim: %+v %v", second, err)
	}
	if _, err = store.Heartbeat(ctx, first, recoveredAt, time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale heartbeat was not fenced: %v", err)
	}
	failed, err := store.Fail(ctx, second, "renderer-temporary", true, recoveredAt.Add(time.Second))
	if err != nil || failed.State != StateQueued || failed.Attempts != 2 || failed.AvailableAt != recoveredAt.Add(time.Second).Add(RetryDelay(2)) {
		t.Fatalf("retry transition: %+v %v", failed, err)
	}
	if _, err = store.Claim(ctx, "helm-renderer-0004", runtime, recoveredAt.Add(time.Second), time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("command was claimed before backoff: %v", err)
	}
}

func TestMemoryStoreCompletesOnlyValidatedLeaseOutput(t *testing.T) {
	store, _, _ := preparedMemoryStore(t)
	ctx := context.Background()
	lease, err := store.Claim(ctx, "helm-renderer-0001", ExpectedRenderWorkerIdentity(store.operatorConfigDigest), testTime, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := ValidateRenderedManifests(validConfigMapManifest(lease.Command.Descriptor), lease.Command.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.Complete(ctx, lease, validated, testTime.Add(time.Second))
	if err != nil || result.Validate(lease.Command) != nil {
		t.Fatalf("complete: %+v %v", result, err)
	}
	command, err := store.Command(ctx, lease.Command.ID)
	if err != nil || command.State != StateSucceeded || command.LeaseOwner != "" || command.CompletedAt == nil {
		t.Fatalf("terminal command: %+v %v", command, err)
	}
	if _, err = store.Complete(ctx, lease, validated, testTime.Add(2*time.Second)); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("duplicate stale completion accepted: %v", err)
	}
}

func TestMemoryStoreReadinessRequiresExactFreshRuntime(t *testing.T) {
	store := NewMemoryStore()
	worker := Worker{Store: store, Packages: stubPackages{}, Renderer: &stubRenderer{}, LeaseDuration: time.Minute,
		Now: func() time.Time { return testTime }, OperatorConfigDigest: store.operatorConfigDigest}
	readiness, err := worker.Readiness("helm-renderer-0001", 1, testTime, testTime, time.Minute)
	if err != nil || store.PutReadiness(context.Background(), readiness) != nil {
		t.Fatalf("readiness: %+v %v", readiness, err)
	}
	ready, err := store.RuntimeReady(context.Background(), testTime.Add(30*time.Second))
	if err != nil || !ready {
		t.Fatalf("fresh exact runtime not ready: %v %v", ready, err)
	}
	ready, err = store.RuntimeReady(context.Background(), readiness.LeaseUntil)
	if err != nil || ready {
		t.Fatalf("expired runtime ready: %v %v", ready, err)
	}
	gap := readiness
	gap.WorkerEpoch = 3
	gap.ObservedAt, gap.LeaseUntil = testTime.Add(time.Second), testTime.Add(time.Minute)
	if err = store.PutReadiness(context.Background(), gap); !errors.Is(err, ErrConflict) {
		t.Fatalf("readiness epoch gap accepted: %v", err)
	}
}

func TestMemoryStoreFencesRollingOperatorConfigDrift(t *testing.T) {
	oldDigest := digestBytes([]byte("helm-operator-config-old.v1"))
	newDigest := digestBytes([]byte("helm-operator-config-new.v1"))
	store := NewMemoryStore(oldDigest)
	files := testChartFiles()
	approval := testApproval(t, packageChart(t, files), files)
	desired := testDesired(t, approval, []byte("replicas: 2\n"))
	if _, _, err := store.PutApproval(context.Background(), approval); err != nil {
		t.Fatal(err)
	}
	command, _, err := store.Submit(context.Background(), desired, testTime)
	if err != nil || command.OperatorConfigDigest != oldDigest {
		t.Fatalf("old command digest=%q err=%v", command.OperatorConfigDigest, err)
	}
	oldReadiness := Readiness{WorkerID: "helm-renderer-config-old", WorkerEpoch: 1,
		RenderWorkerIdentity: ExpectedRenderWorkerIdentity(oldDigest), StartedAt: testTime,
		ObservedAt: testTime, LeaseUntil: testTime.Add(time.Minute)}
	if err = store.PutReadiness(context.Background(), oldReadiness); err != nil {
		t.Fatal(err)
	}

	// Simulate a new API/worker ReplicaSet using the same durable store while an
	// old command and readiness observation survive the rollout.
	store.mu.Lock()
	store.operatorConfigDigest = newDigest
	store.mu.Unlock()
	if _, _, err = store.Submit(context.Background(), desired, testTime.Add(time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("old-config command replayed under new config: %v", err)
	}
	if err = store.PutReadiness(context.Background(), oldReadiness); !errors.Is(err, ErrInvalid) {
		t.Fatalf("new store accepted old-config readiness: %v", err)
	}
	if ready, readyErr := store.RuntimeReady(context.Background(), testTime.Add(time.Second)); readyErr != nil || ready {
		t.Fatalf("old readiness advertised new config: ready=%v err=%v", ready, readyErr)
	}
	if _, err = store.Claim(context.Background(), "helm-renderer-config-new", ExpectedRenderWorkerIdentity(newDigest),
		testTime, time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("new worker adopted old-config command: %v", err)
	}
	if _, err = store.Claim(context.Background(), "helm-renderer-config-old", ExpectedRenderWorkerIdentity(oldDigest),
		testTime, time.Minute); !errors.Is(err, ErrInvalid) {
		t.Fatalf("old worker claimed through new store: %v", err)
	}

	newReadiness := oldReadiness
	newReadiness.WorkerEpoch = 2
	newReadiness.RenderWorkerIdentity = ExpectedRenderWorkerIdentity(newDigest)
	newReadiness.ObservedAt = testTime.Add(time.Second)
	newReadiness.LeaseUntil = testTime.Add(time.Minute)
	if err = store.PutReadiness(context.Background(), newReadiness); err != nil {
		t.Fatal(err)
	}
	if ready, readyErr := store.RuntimeReady(context.Background(), testTime.Add(2*time.Second)); readyErr != nil || !ready {
		t.Fatalf("new exact readiness unavailable: ready=%v err=%v", ready, readyErr)
	}
}
