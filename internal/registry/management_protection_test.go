package registry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/store"
)

type managementProtectionStore struct {
	ManagementStore
	snapshots     []domain.RegistryLifecycleSnapshot
	snapshotsErr  error
	snapshotCalls int
	saved         domain.RegistryCleanupPlan
}

func (s *managementProtectionStore) RegistryLifecycleSnapshotsForActor(context.Context, string, string, time.Time) ([]domain.RegistryLifecycleSnapshot, error) {
	s.snapshotCalls++
	return append([]domain.RegistryLifecycleSnapshot(nil), s.snapshots...), s.snapshotsErr
}

func (s *managementProtectionStore) SaveRegistryCleanupPreviewForActor(_ context.Context, _, _, _, _, _ string, plan domain.RegistryCleanupPlan) (store.Result[domain.RegistryCleanupPlan], error) {
	s.saved = plan
	return store.Result[domain.RegistryCleanupPlan]{Value: plan}, nil
}

func TestManagementAuthorizesBeforeRefreshingAndReloadsAfterward(t *testing.T) {
	now := time.Date(2026, 8, 13, 2, 30, 0, 0, time.UTC)
	t.Run("unauthorized", func(t *testing.T) {
		repository := &managementProtectionStore{snapshotsErr: store.ErrNotFound}
		refresher := &protectionRefresherStub{}
		management := NewManagement(repository, nil, WithManagementClock(func() time.Time { return now }), WithManagementProtectionRefresher(refresher))
		_, err := management.PreviewCleanup(t.Context(), "attacker", "key", "fingerprint", "request", "service", "target")
		if !errors.Is(err, store.ErrNotFound) || len(refresher.calls) != 0 {
			t.Fatalf("err=%v refresh calls=%#v", err, refresher.calls)
		}
	})

	t.Run("authorized", func(t *testing.T) {
		snapshot := fixtureSnapshot(now)
		repository := &managementProtectionStore{snapshots: []domain.RegistryLifecycleSnapshot{snapshot}}
		refresher := &protectionRefresherStub{}
		management := NewManagement(repository, nil,
			WithManagementClock(func() time.Time { return now }),
			WithManagementIDGenerator(func() string { return "33333333-3333-4333-8333-333333333333" }),
			WithManagementProtectionRefresher(refresher))
		result, err := management.PreviewCleanup(t.Context(), "admin", "key", "fingerprint", "request", snapshot.Policy.ServiceID, snapshot.Target.ID)
		if err != nil || result.Value.ID == "" || repository.saved.ID != result.Value.ID {
			t.Fatalf("result=%#v saved=%#v err=%v", result, repository.saved, err)
		}
		if repository.snapshotCalls != 2 || len(refresher.calls) != 1 || !refresher.calls[0].forceFresh {
			t.Fatalf("snapshot calls=%d refresh calls=%#v", repository.snapshotCalls, refresher.calls)
		}
	})
}
