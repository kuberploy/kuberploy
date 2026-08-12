package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/registry"
)

type registryReadinessRecorder struct{ acquisitions int }

type registryInstallProbe struct {
	checks atomic.Int32
	ready  chan struct{}
}

func (p *registryInstallProbe) Inspect(ctx context.Context, _ registry.RuntimeConfig) (registry.ManagedRegistryStopProof, error) {
	p.checks.Add(1)
	select {
	case <-p.ready:
		return registry.ManagedRegistryStopProof{}, nil
	default:
		return registry.ManagedRegistryStopProof{}, errors.New("registry deployment is not installed")
	}
}

func (r *registryReadinessRecorder) AcquireManagedRegistryReadiness(context.Context, registry.RuntimeWorkerObservation, time.Duration) (registry.RuntimeReadinessLease, error) {
	r.acquisitions++
	return registry.RuntimeReadinessLease{}, nil
}
func (*registryReadinessRecorder) HeartbeatManagedRegistryReadiness(context.Context, registry.RuntimeReadinessLease, time.Time, time.Duration) (registry.RuntimeReadinessLease, error) {
	return registry.RuntimeReadinessLease{}, nil
}
func (*registryReadinessRecorder) ManagedRegistryRuntimeReady(context.Context, registry.RuntimeIdentity, time.Time, time.Duration) error {
	return nil
}

func TestManagedRegistryWorkerNeverHeartbeatsBeforeFullRuntimeValidation(t *testing.T) {
	readiness := &registryReadinessRecorder{}
	runtime := &managedRegistryRuntime{controller: &registry.RuntimeController{}, readiness: readiness,
		workerID: "worker-registry-ready-01234567", startedAt: time.Now().UTC(), prerequisitesValidated: true}
	if err := runtime.Run(context.Background()); err == nil {
		t.Fatal("invalid managed registry runtime started")
	}
	if readiness.acquisitions != 0 {
		t.Fatalf("invalid runtime published %d readiness observations", readiness.acquisitions)
	}
}

func TestManagedRegistryWaitsForInstallBeforePublishingReadiness(t *testing.T) {
	probe := &registryInstallProbe{ready: make(chan struct{})}
	runtime := &managedRegistryRuntime{workloads: probe, config: registry.RuntimeConfig{Enabled: true}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runtime.waitForManagedRegistry(ctx) }()

	time.Sleep(20 * time.Millisecond)
	if probe.checks.Load() == 0 {
		t.Fatal("managed registry installation was not probed")
	}
	select {
	case err := <-done:
		t.Fatalf("registry wait stopped before installation: %v", err)
	default:
	}
	close(probe.ready)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("registry wait failed after installation: %v", err)
		}
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("registry wait did not observe installation")
	}
	cancel()
}
