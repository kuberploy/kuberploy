package builds

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type runnerDeliveryStore struct {
	mu      sync.Mutex
	claims  []string
	claimed bool
}

func (s *runnerDeliveryStore) PendingDeliveries(context.Context, time.Time, int) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimed {
		return nil, nil
	}
	s.claimed = true
	return append([]string(nil), s.claims...), nil
}

type runnerResumer struct {
	seen chan string
	err  map[string]error
}

func (r *runnerResumer) ResumeDelivery(_ context.Context, claim string) (WebhookOutcome, error) {
	select {
	case r.seen <- claim:
	default:
	}
	return WebhookOutcome{}, r.err[claim]
}

type runnerReconciler struct {
	seen chan struct{}
	err  error
}

type runnerReleaseReconciler struct {
	seen chan struct{}
	err  error
}

func (r *runnerReleaseReconciler) ReconcileNext(context.Context) (ReleaseProjectionResult, error) {
	select {
	case r.seen <- struct{}{}:
	default:
	}
	return ReleaseProjectionResult{}, r.err
}

func (r *runnerReconciler) ReconcileNext(context.Context) (ReconcileResult, error) {
	select {
	case r.seen <- struct{}{}:
	default:
	}
	return ReconcileResult{}, r.err
}

func TestWorkerRunnerLoopsProgressAndStopIndependently(t *testing.T) {
	store := &runnerDeliveryStore{claims: []string{"one"}}
	resumer := &runnerResumer{seen: make(chan string, 1), err: map[string]error{}}
	reconciler := &runnerReconciler{seen: make(chan struct{}, 1), err: ErrInfrastructure}
	releases := &runnerReleaseReconciler{seen: make(chan struct{}, 1), err: ErrInfrastructure}
	runner := &WorkerRunner{
		Store: store, Deliveries: resumer, Builds: reconciler, Releases: releases,
		DeliveryOwner: "receipt-owner", BuildOwner: "build-owner", ReleaseOwner: "release-owner", DeliveryBatch: 10,
		IdleDelay: 10 * time.Millisecond, MinimumErrorDelay: 10 * time.Millisecond, MaximumErrorDelay: 20 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	select {
	case claim := <-resumer.seen:
		if claim != "one" {
			t.Fatalf("claim=%q", claim)
		}
	case <-time.After(time.Second):
		t.Fatal("delivery loop did not progress while build loop failed")
	}
	select {
	case <-reconciler.seen:
	case <-time.After(time.Second):
		t.Fatal("build loop did not run")
	}
	select {
	case <-releases.seen:
	case <-time.After(time.Second):
		t.Fatal("release projection loop did not run")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("runner did not stop with its parent context")
	}
}

func TestWorkerRunnerDeliveryBatchContinuesAfterOneFailure(t *testing.T) {
	store := &runnerDeliveryStore{claims: []string{"bad", "good"}}
	resumer := &runnerResumer{seen: make(chan string, 2), err: map[string]error{"bad": ErrProviderRetry}}
	runner := &WorkerRunner{Store: store, Deliveries: resumer, DeliveryBatch: 10, Now: func() time.Time { return testNow }}
	worked, err := runner.resumePendingOnce(context.Background())
	if !worked || !errors.Is(err, ErrProviderRetry) {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	seen := map[string]bool{<-resumer.seen: true, <-resumer.seen: true}
	if !seen["bad"] || !seen["good"] {
		t.Fatalf("claims=%v", seen)
	}
}

func TestWorkerRunnerRejectsSharedLeaseOwnerAndBoundsBackoff(t *testing.T) {
	runner := &WorkerRunner{
		Store: &runnerDeliveryStore{}, Deliveries: &runnerResumer{}, Builds: &runnerReconciler{}, Releases: &runnerReleaseReconciler{},
		DeliveryOwner: "same", BuildOwner: "same", ReleaseOwner: "release-owner", DeliveryBatch: 1,
		IdleDelay: 10 * time.Millisecond, MinimumErrorDelay: 10 * time.Millisecond, MaximumErrorDelay: time.Second,
	}
	if err := runner.validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("shared owner accepted: %v", err)
	}
	if got := nextErrorBackoff(750*time.Millisecond, time.Second); got != time.Second {
		t.Fatalf("backoff=%s", got)
	}
}
