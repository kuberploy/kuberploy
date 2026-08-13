package registry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/store"
)

type preparedMaintenanceStore struct {
	store.RegistryRuntimeStore
}

func (preparedMaintenanceStore) PrepareRegistryMaintenanceStop(_ context.Context, lease store.RegistryMaintenanceLease, uid string, replicas int32, _ time.Time) (store.RegistryMaintenanceLease, error) {
	lease.DeploymentUID = uid
	lease.OriginalReplicas = replicas
	return lease, nil
}

type stopResponseLostWorkloads struct {
	RegistryMaintenanceWorkloads
	now           time.Time
	restoredLease store.RegistryMaintenanceLease
}

func (w *stopResponseLostWorkloads) Inspect(context.Context, RuntimeConfig) (ManagedRegistryStopProof, error) {
	return ManagedRegistryStopProof{Namespace: "registry-system", Deployment: "registry", DeploymentUID: "registry-uid",
		OriginalReplicas: 1, PersistentVolumeClaim: "registry-data", RegistryConfigMap: "registry-config",
		ObservedAt: w.now}, nil
}

func (*stopResponseLostWorkloads) Stop(context.Context, RuntimeConfig, store.RegistryMaintenanceLease) (ManagedRegistryStopProof, error) {
	return ManagedRegistryStopProof{}, errors.New("response lost after scale")
}

func (w *stopResponseLostWorkloads) Restore(_ context.Context, _ RuntimeConfig, lease store.RegistryMaintenanceLease) (ManagedRegistryRestoreProof, error) {
	w.restoredLease = lease
	return ManagedRegistryRestoreProof{Namespace: "registry-system", Deployment: "registry", DeploymentUID: lease.DeploymentUID,
		DesiredReplicas: lease.OriginalReplicas, AvailableReplicas: lease.OriginalReplicas, Ready: true, ObservedAt: w.now}, nil
}

func TestEnterPublishesPreparedIdentityBeforeStopMutation(t *testing.T) {
	now := time.Now().UTC()
	runtime := RuntimeConfig{Enabled: true, Namespace: "registry-system", Deployment: "registry",
		PersistentVolumeClaim: "registry-data", RegistryConfigMap: "registry-config"}
	workloads := &stopResponseLostWorkloads{now: now}
	adapter := &KubernetesMaintenanceAdapter{store: preparedMaintenanceStore{}, workloads: workloads, runtime: runtime, now: func() time.Time { return now }}
	session := &kubernetesMaintenanceSession{adapter: adapter, lease: store.RegistryMaintenanceLease{
		TargetID: "11111111-1111-4111-8111-111111111111", PlanID: "22222222-2222-4222-8222-222222222222",
		ExecutionKey: "sha256:" + repeatHex("a", 64), CandidateSetDigest: "sha256:" + repeatHex("b", 64),
		Owner: "worker", Epoch: 1, Until: now.Add(time.Minute), State: "acquired"}}

	if _, err := session.Enter(context.Background()); err == nil || err.Error() != "response lost after scale" {
		t.Fatalf("unexpected enter result: %v", err)
	}
	if err := session.Restore(context.Background()); err != nil {
		t.Fatalf("prepared identity could not restore after stop response loss: %v", err)
	}
	if workloads.restoredLease.DeploymentUID != "registry-uid" || workloads.restoredLease.OriginalReplicas != 1 {
		t.Fatalf("restore lost prepared deployment identity: %+v", workloads.restoredLease)
	}
}
