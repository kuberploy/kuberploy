package memory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/registry"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func grantRegistryActor(st *Store, actor string, role domain.AccessRole, scopeType domain.AccessScopeType, scopeID string) {
	st.users[actor] = domain.User{ID: actor, Login: actor, Role: string(role), CreatedAt: time.Now().UTC()}
	st.accessGrants["grant-"+actor] = domain.AccessGrant{
		ID: "grant-" + actor, SubjectUserID: actor, Role: role, ScopeType: scopeType,
		ScopeID: scopeID, Source: "test", CreatedBy: actor, CreatedAt: time.Now().UTC(),
	}
}

func TestRegistryTargetAdminWrappersArePlatformBoundIdempotentAndAudited(t *testing.T) {
	ctx := context.Background()
	st := New()
	grantRegistryActor(st, "platform-admin", domain.RolePlatformAdmin, domain.ScopePlatform, "platform")
	grantRegistryActor(st, "project-admin", domain.RoleProjectAdmin, domain.ScopeProject, "project-a")
	target := domain.RegistryTarget{
		ID: "11111111-1111-4111-8111-111111111111", Name: "primary", Mode: domain.RegistryTargetManaged,
		Endpoint: "registry.internal", RepositoryPrefix: "kuberploy", PullCredentialRef: "pull-ref",
		PushCredentialRef: "push-ref", CacheCredentialRef: "cache-ref",
	}

	created, err := st.CreateRegistryTargetForActor(ctx, "platform-admin", "create-primary", "fingerprint-a", "request-a", target)
	if err != nil || created.Replay || created.Value.ID != target.ID || st.AuditCount() != 1 {
		t.Fatalf("created=%#v audits=%d err=%v", created, st.AuditCount(), err)
	}
	replay, err := st.CreateRegistryTargetForActor(ctx, "platform-admin", "create-primary", "fingerprint-a", "request-b", target)
	if err != nil || !replay.Replay || replay.Value != created.Value || st.AuditCount() != 1 {
		t.Fatalf("replay=%#v audits=%d err=%v", replay, st.AuditCount(), err)
	}
	if _, err = st.CreateRegistryTargetForActor(ctx, "platform-admin", "create-primary", "different", "request-c", target); !errors.Is(err, base.ErrIdempotencyConflict) {
		t.Fatalf("changed replay err=%v", err)
	}
	if _, err = st.ListRegistryTargetsForActor(ctx, "project-admin"); !errors.Is(err, base.ErrForbidden) {
		t.Fatalf("project target listing err=%v", err)
	}
	target.Name = "primary-updated"
	updated, err := st.UpdateRegistryTargetForActor(ctx, "platform-admin", "update-primary", "fingerprint-update", "request-d", target)
	if err != nil || updated.Value.CreatedAt != created.Value.CreatedAt || st.AuditCount() != 2 {
		t.Fatalf("updated=%#v audits=%d err=%v", updated, st.AuditCount(), err)
	}
	target.Mode = domain.RegistryTargetExternal
	if _, err = st.UpdateRegistryTargetForActor(ctx, "platform-admin", "change-mode", "fingerprint-mode", "request-e", target); !errors.Is(err, base.ErrConflict) {
		t.Fatalf("mode mutation err=%v", err)
	}
}

func TestRegistryApplicationWrappersAreScopedReplaySafeAndManagedOnly(t *testing.T) {
	ctx := context.Background()
	seed := seedManagedRegistry(t)
	st := seed.store
	st.projects["project-a"] = domain.Project{ID: "project-a", Name: "Project A"}
	st.applications[seed.serviceID] = domain.Application{ID: seed.serviceID, ProjectID: "project-a", Name: "Service"}
	grantRegistryActor(st, "project-admin", domain.RoleProjectAdmin, domain.ScopeProject, "project-a")
	grantRegistryActor(st, "viewer", domain.RoleViewer, domain.ScopeApplication, seed.serviceID)

	policy := registry.DefaultPolicy(seed.targetID, seed.serviceID, seed.repository, seed.now)
	policy.KeepLastSuccessful = 3
	auditsBefore := st.AuditCount()
	result, err := st.PutServiceRegistryPolicyForActor(ctx, "project-admin", "policy-1", "policy-fingerprint", "request-policy", seed.serviceID, policy)
	if err != nil || result.Replay || result.Value.KeepLastSuccessful != 3 || st.AuditCount() != auditsBefore+1 {
		t.Fatalf("policy=%#v audits=%d err=%v", result, st.AuditCount(), err)
	}
	target, err := st.RegistryTarget(ctx, seed.targetID)
	if err != nil {
		t.Fatal(err)
	}
	target.RepositoryPrefix = "other"
	if _, err = st.PutRegistryTarget(ctx, target); !errors.Is(err, base.ErrConflict) {
		t.Fatalf("target prefix changed beneath an existing policy: %v", err)
	}
	outside := policy
	outside.Repository = "other/service"
	if _, err = st.PutServiceRegistryPolicyForActor(ctx, "project-admin", "outside-policy", "outside-fingerprint", "request-outside", seed.serviceID, outside); !errors.Is(err, base.ErrRegistryPolicyInvalid) {
		t.Fatalf("outside repository policy err=%v", err)
	}
	replay, err := st.PutServiceRegistryPolicyForActor(ctx, "project-admin", "policy-1", "policy-fingerprint", "request-policy-replay", seed.serviceID, policy)
	if err != nil || !replay.Replay || st.AuditCount() != auditsBefore+1 {
		t.Fatalf("policy replay=%#v audits=%d err=%v", replay, st.AuditCount(), err)
	}
	if _, err = st.PutServiceRegistryPolicyForActor(ctx, "viewer", "viewer-policy", "viewer-fingerprint", "request-viewer", seed.serviceID, policy); !errors.Is(err, base.ErrForbidden) {
		t.Fatalf("viewer policy err=%v", err)
	}
	if snapshots, snapshotErr := st.RegistryLifecycleSnapshotsForActor(ctx, "viewer", seed.serviceID, seed.now); snapshotErr != nil || len(snapshots) != 1 || snapshots[0].Target.ID != seed.targetID {
		t.Fatalf("viewer snapshots=%#v err=%v", snapshots, snapshotErr)
	}

	snapshot, err := st.registryLifecycleSnapshotLockedForTest(seed.targetID, seed.serviceID, seed.now)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := registry.BuildCleanupPlan(snapshot, seed.now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	plan.ID = "55555555-5555-4555-8555-555555555555"
	preview, err := st.SaveRegistryCleanupPreviewForActor(ctx, "project-admin", "preview-1", "preview-fingerprint", "request-preview", seed.serviceID, plan)
	if err != nil || preview.Replay || st.AuditCount() != auditsBefore+2 {
		t.Fatalf("preview=%#v audits=%d err=%v", preview, st.AuditCount(), err)
	}
	previewReplay, err := st.SaveRegistryCleanupPreviewForActor(ctx, "project-admin", "preview-1", "preview-fingerprint", "request-preview-replay", seed.serviceID, plan)
	if err != nil || !previewReplay.Replay || previewReplay.Value.ID != preview.Value.ID || st.AuditCount() != auditsBefore+2 {
		t.Fatalf("preview replay=%#v audits=%d err=%v", previewReplay, st.AuditCount(), err)
	}
	prepared, err := st.PrepareRegistryCleanupExecutionForActor(ctx, "project-admin", "execute-1", "execute-fingerprint", "request-execute", seed.serviceID, plan.ID)
	if err != nil || prepared.Replay || prepared.Value.State != "preview" || st.AuditCount() != auditsBefore+3 {
		t.Fatalf("prepared=%#v audits=%d err=%v", prepared, st.AuditCount(), err)
	}
	preparedReplay, err := st.PrepareRegistryCleanupExecutionForActor(ctx, "project-admin", "execute-1", "execute-fingerprint", "request-execute-replay", seed.serviceID, plan.ID)
	if err != nil || !preparedReplay.Replay || st.AuditCount() != auditsBefore+3 {
		t.Fatalf("execute replay=%#v audits=%d err=%v", preparedReplay, st.AuditCount(), err)
	}
	st.mu.Lock()
	failed := st.registryPlans[plan.ID]
	failed.State = "failed"
	failed.Failure = "managed registry cleanup execution failed"
	pendingBlobs := 0
	for index := range failed.Items {
		if failed.Items[index].Disposition != domain.RegistryCleanupDelete {
			continue
		}
		failed.Items[index].State = "deleted"
		if failed.Items[index].ResourceKind == "blob" {
			failed.Items[index].State = "failed"
			failed.Items[index].ProviderMessage = "managed registry cleanup failed"
			pendingBlobs++
		}
	}
	if pendingBlobs == 0 {
		failed.Items = append(failed.Items, domain.RegistryCleanupItem{
			Ordinal: len(failed.Items), Repository: "*", ResourceKind: "blob", Digest: registryDigest("f"),
			Disposition: domain.RegistryCleanupDelete, Action: "garbage-collect-blob", State: "failed",
		})
	}
	st.registryPlans[plan.ID] = failed
	st.mu.Unlock()
	preparedRecovery, err := st.PrepareRegistryCleanupExecutionForActor(ctx, "project-admin", "execute-recovery", "execute-recovery-fingerprint", "request-execute-recovery", seed.serviceID, plan.ID)
	if err != nil || preparedRecovery.Replay || !base.RegistryCleanupPlanCanResumeOfflineSweep(preparedRecovery.Value) || st.AuditCount() != auditsBefore+4 {
		t.Fatalf("recovery=%#v audits=%d err=%v", preparedRecovery, st.AuditCount(), err)
	}
	recovered, claimed, err := st.ClaimRegistryCleanupPlan(ctx, plan.ID, "recovery-worker", seed.now.Add(time.Minute), time.Minute)
	if err != nil || !claimed || recovered.State != "executing" {
		t.Fatalf("claim recovery=%#v claimed=%v err=%v", recovered, claimed, err)
	}
	for _, item := range recovered.Items {
		if item.Disposition == domain.RegistryCleanupDelete && item.ResourceKind == "blob" && item.State != "deleting" {
			t.Fatalf("recovered blob state=%q", item.State)
		}
	}

	externalID := "66666666-6666-4666-8666-666666666666"
	if _, err = st.PutRegistryTarget(ctx, domain.RegistryTarget{ID: externalID, Name: "external", Mode: domain.RegistryTargetExternal, Endpoint: "external.test", RepositoryPrefix: "tenant"}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.PutServiceRegistryPolicy(ctx, registry.DefaultPolicy(externalID, seed.serviceID, "tenant/service", seed.now)); err != nil {
		t.Fatal(err)
	}
	externalPlan := domain.RegistryCleanupPlan{ID: "77777777-7777-4777-8777-777777777777", RegistryTargetID: externalID, ServiceID: seed.serviceID, SnapshotToken: "snapshot", AuthorityToken: "authority", State: "preview", CreatedAt: seed.now}
	externalPlan.PlanDigest = base.RegistryCleanupPlanDigest(externalPlan)
	if _, err = st.SaveRegistryCleanupPreviewForActor(ctx, "project-admin", "external-preview", "external-fingerprint", "request-external", seed.serviceID, externalPlan); !errors.Is(err, base.ErrRegistryExternalLifecycle) {
		t.Fatalf("external preview err=%v", err)
	}
}

func TestOperatorRegistryTargetMayBroadenWithoutOrphaningExistingPolicy(t *testing.T) {
	ctx := context.Background()
	st := New()
	target := domain.RegistryTarget{ID: "11111111-1111-4111-8111-111111111111", Name: "managed", Mode: domain.RegistryTargetManaged,
		Endpoint: "registry.test", RepositoryPrefix: "kuberploy/apps"}
	if _, err := st.PutRegistryTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	policy := registry.DefaultPolicy(target.ID, "service", "kuberploy/apps/service", time.Now().UTC())
	if _, err := st.PutServiceRegistryPolicy(ctx, policy); err != nil {
		t.Fatal(err)
	}
	target.RepositoryPrefix = "kuberploy"
	if _, err := st.PutRegistryTarget(ctx, target); err != nil {
		t.Fatalf("safe operator prefix broadening was rejected: %v", err)
	}
	target.RepositoryPrefix = "other"
	if _, err := st.PutRegistryTarget(ctx, target); !errors.Is(err, base.ErrConflict) {
		t.Fatalf("policy-orphaning prefix rotation was accepted: %v", err)
	}
}

func (s *Store) registryLifecycleSnapshotLockedForTest(targetID, serviceID string, now time.Time) (domain.RegistryLifecycleSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.registryLifecycleSnapshotLocked(targetID, serviceID, now)
}
