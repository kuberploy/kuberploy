package registry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/store"
)

type staleSweepRegistryStore struct {
	store.RegistryStore
	plan     domain.RegistryCleanupPlan
	snapshot domain.RegistryLifecycleSnapshot
}

func (s staleSweepRegistryStore) RegistryCleanupPlan(context.Context, string) (domain.RegistryCleanupPlan, error) {
	return s.plan, nil
}

func (s staleSweepRegistryStore) RegistryLifecycleSnapshot(context.Context, string, string, time.Time) (domain.RegistryLifecycleSnapshot, error) {
	return s.snapshot, nil
}

func TestMaintenanceAcquireRejectsAuthorityDriftBeforeRuntimeLease(t *testing.T) {
	now := time.Now().UTC()
	targetID := "f0b03740-c7a9-4ca0-a852-e807c97d2fc8"
	plan := domain.RegistryCleanupPlan{
		ID: "026cd564-3f87-46ca-a692-3ecb51864b34", RegistryTargetID: targetID,
		ServiceID: "245b3ca8-8b4d-4306-adb9-35fc8270bde3", State: "executing",
		PlanDigest: "sha256:" + repeatHex("a", 64), AuthorityToken: "sha256:" + repeatHex("b", 64),
		Items: []domain.RegistryCleanupItem{{ResourceKind: "blob", Digest: "sha256:" + repeatHex("c", 64),
			Disposition: domain.RegistryCleanupDelete, Action: "garbage-collect-blob", State: "deleting"}},
	}
	snapshot := domain.RegistryLifecycleSnapshot{
		Target: domain.RegistryTarget{ID: targetID},
		Policy: domain.ServiceRegistryPolicy{RegistryTargetID: targetID, ServiceID: plan.ServiceID},
		AuthorityObservations: []domain.RegistryAuthorityObservation{{RegistryTargetID: targetID,
			ServiceID: plan.ServiceID, Authority: domain.RegistryAuthorityRuntime, Revision: "changed",
			Complete: true, SnapshotDigest: "sha256:" + repeatHex("d", 64), ObservedAt: now}},
	}
	adapter := &KubernetesMaintenanceAdapter{
		registry: staleSweepRegistryStore{plan: plan, snapshot: snapshot},
		runtime:  RuntimeConfig{TargetID: targetID}, now: func() time.Time { return now },
	}
	session, err := adapter.Acquire(t.Context(), MaintenanceAcquireRequest{
		TargetID: targetID, PlanID: plan.ID, ExecutionKey: "sha256:" + repeatHex("e", 64), Owner: "worker-registry-12345678",
	})
	if session != nil || !errors.Is(err, store.ErrRegistrySnapshotStale) {
		t.Fatalf("session=%v err=%v", session, err)
	}
}

type maintenanceVersionProbe struct {
	store.RegistryRuntimeStore
	calls int
	err   error
}

func (s *maintenanceVersionProbe) AcquireRegistryMaintenance(context.Context, string, string, string, string, string, time.Time, time.Duration) (store.RegistryMaintenanceLease, error) {
	s.calls++
	return store.RegistryMaintenanceLease{}, s.err
}

func TestMaintenancePreservesLegacyAuthorityBeforeAnyRuntimeChange(t *testing.T) {
	now := time.Now().UTC()
	for _, semantic := range []bool{false, true} {
		for _, refreshed := range []bool{false, true} {
			snapshot := domain.RegistryLifecycleSnapshot{Target: domain.RegistryTarget{ID: "11111111-1111-4111-8111-111111111111"}, Releases: []domain.RegistryRelease{{ID: "release", Availability: domain.RegistryArtifactExpired, AvailabilityObservedAt: &now}}}
			plan := domain.RegistryCleanupPlan{ID: "22222222-2222-4222-8222-222222222222", RegistryTargetID: snapshot.Target.ID, ServiceID: "app", State: "executing", SnapshotToken: "sha256:" + repeatHex("a", 64), PlanDigest: "sha256:" + repeatHex("b", 64), Items: []domain.RegistryCleanupItem{{ResourceKind: "blob", Digest: "sha256:" + repeatHex("c", 64), Disposition: domain.RegistryCleanupDelete, Action: "garbage-collect-blob", State: "deleting"}}}
			if semantic {
				plan.SnapshotToken = store.RegistrySnapshotToken(snapshot)
			}
			plan.AuthorityToken = store.RegistryAuthorityTokenForPlan(snapshot, plan)
			if refreshed {
				next := now.Add(time.Minute)
				snapshot.Releases[0].AvailabilityObservedAt = &next
			}
			stop := errors.New("stop before runtime mutation")
			probe := &maintenanceVersionProbe{err: stop}
			adapter := &KubernetesMaintenanceAdapter{store: probe, registry: staleSweepRegistryStore{plan: plan, snapshot: snapshot}, runtime: RuntimeConfig{TargetID: snapshot.Target.ID}, now: func() time.Time { return now }}
			session, err := adapter.Acquire(t.Context(), MaintenanceAcquireRequest{TargetID: snapshot.Target.ID, PlanID: plan.ID, ExecutionKey: "sha256:" + repeatHex("d", 64), Owner: "worker-registry-12345678"})
			if session != nil {
				t.Fatal("diagnostic unexpectedly opened a maintenance session")
			}
			if !semantic && refreshed {
				if !errors.Is(err, store.ErrRegistrySnapshotStale) || probe.calls != 0 {
					t.Fatalf("legacy approval normalized before maintenance: err=%v calls=%d", err, probe.calls)
				}
			} else if !errors.Is(err, stop) || probe.calls != 1 {
				t.Fatalf("unchanged approval blocked before sentinel: semantic=%v refreshed=%v err=%v calls=%d", semantic, refreshed, err, probe.calls)
			}
		}
	}
}
