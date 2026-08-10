package main

import (
	"context"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/registry"
)

type registryReadinessRecorder struct{ acquisitions int }

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
