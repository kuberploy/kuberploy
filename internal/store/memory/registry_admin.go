package memory

import (
	"context"
	"sort"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/registry"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func (s *Store) ListRegistryTargetsForActor(_ context.Context, actor string) ([]domain.RegistryTarget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorizeLocked(actor, domain.PermissionRegistryTargetsManage, domain.AccessTarget{Type: "platform", ID: "platform"}); err != nil {
		return nil, err
	}
	targets := make([]domain.RegistryTarget, 0, len(s.registryTargets))
	for _, target := range s.registryTargets {
		targets = append(targets, target)
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Name != targets[j].Name {
			return targets[i].Name < targets[j].Name
		}
		return targets[i].ID < targets[j].ID
	})
	return targets, nil
}

func (s *Store) CreateRegistryTargetForActor(_ context.Context, actor, key, fingerprint, _ string, target domain.RegistryTarget) (base.Result[domain.RegistryTarget], error) {
	if err := registry.ValidateTarget(target); err != nil {
		return base.Result[domain.RegistryTarget]{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorizeLocked(actor, domain.PermissionRegistryTargetsManage, domain.AccessTarget{Type: "platform", ID: "platform"}); err != nil {
		return base.Result[domain.RegistryTarget]{}, err
	}
	idemIdentity := ik(actor, "registry-targets.create", key)
	old, replay := s.idempotency[idemIdentity]
	if err := check(old, replay, fingerprint); err != nil {
		return base.Result[domain.RegistryTarget]{}, err
	}
	if replay {
		current, ok := s.registryTargets[old.resourceID]
		if !ok {
			return base.Result[domain.RegistryTarget]{}, base.ErrNotFound
		}
		return base.Result[domain.RegistryTarget]{Value: current, Replay: true}, nil
	}
	if _, exists := s.registryTargets[target.ID]; exists {
		return base.Result[domain.RegistryTarget]{}, base.ErrConflict
	}
	for _, current := range s.registryTargets {
		if current.Name == target.Name {
			return base.Result[domain.RegistryTarget]{}, base.ErrConflict
		}
	}
	now := time.Now().UTC()
	target.CreatedAt = now
	target.UpdatedAt = now
	s.registryTargets[target.ID] = target
	s.idempotency[idemIdentity] = idemRecord{fingerprint: fingerprint, typ: "registry-target", resourceID: target.ID}
	s.audits++
	return base.Result[domain.RegistryTarget]{Value: target}, nil
}

func (s *Store) UpdateRegistryTargetForActor(_ context.Context, actor, key, fingerprint, _ string, target domain.RegistryTarget) (base.Result[domain.RegistryTarget], error) {
	if err := registry.ValidateTarget(target); err != nil {
		return base.Result[domain.RegistryTarget]{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorizeLocked(actor, domain.PermissionRegistryTargetsManage, domain.AccessTarget{Type: "platform", ID: "platform"}); err != nil {
		return base.Result[domain.RegistryTarget]{}, err
	}
	idemIdentity := ik(actor, "registry-targets.update:"+target.ID, key)
	old, replay := s.idempotency[idemIdentity]
	if err := check(old, replay, fingerprint); err != nil {
		return base.Result[domain.RegistryTarget]{}, err
	}
	if replay {
		current, ok := s.registryTargets[old.resourceID]
		if !ok {
			return base.Result[domain.RegistryTarget]{}, base.ErrNotFound
		}
		return base.Result[domain.RegistryTarget]{Value: current, Replay: true}, nil
	}
	current, exists := s.registryTargets[target.ID]
	if !exists {
		return base.Result[domain.RegistryTarget]{}, base.ErrNotFound
	}
	if current.Mode != target.Mode {
		return base.Result[domain.RegistryTarget]{}, base.ErrConflict
	}
	if current.RepositoryPrefix != target.RepositoryPrefix {
		for _, policy := range s.registryPolicies {
			if policy.RegistryTargetID == target.ID {
				return base.Result[domain.RegistryTarget]{}, base.ErrConflict
			}
		}
	}
	for targetID, other := range s.registryTargets {
		if targetID != target.ID && other.Name == target.Name {
			return base.Result[domain.RegistryTarget]{}, base.ErrConflict
		}
	}
	target.CreatedAt = current.CreatedAt
	target.UpdatedAt = time.Now().UTC()
	s.registryTargets[target.ID] = target
	s.idempotency[idemIdentity] = idemRecord{fingerprint: fingerprint, typ: "registry-target", resourceID: target.ID}
	s.audits++
	return base.Result[domain.RegistryTarget]{Value: target}, nil
}

func (s *Store) RegistryLifecycleSnapshotsForActor(_ context.Context, actor, applicationID string, now time.Time) ([]domain.RegistryLifecycleSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorizeLocked(actor, domain.PermissionRegistryRead, domain.AccessTarget{Type: "application", ID: applicationID}); err != nil {
		return nil, err
	}
	snapshots := make([]domain.RegistryLifecycleSnapshot, 0)
	for _, policy := range s.registryPolicies {
		if policy.ServiceID != applicationID {
			continue
		}
		snapshot, err := s.registryLifecycleSnapshotLocked(policy.RegistryTargetID, applicationID, now)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snapshot)
	}
	sort.Slice(snapshots, func(i, j int) bool {
		if snapshots[i].Target.Name != snapshots[j].Target.Name {
			return snapshots[i].Target.Name < snapshots[j].Target.Name
		}
		return snapshots[i].Target.ID < snapshots[j].Target.ID
	})
	return snapshots, nil
}

func (s *Store) PutServiceRegistryPolicyForActor(_ context.Context, actor, key, fingerprint, _, applicationID string, policy domain.ServiceRegistryPolicy) (base.Result[domain.ServiceRegistryPolicy], error) {
	if policy.ServiceID != applicationID {
		return base.Result[domain.ServiceRegistryPolicy]{}, base.ErrRegistryPolicyInvalid
	}
	now := time.Now().UTC()
	policy = registry.NormalizePolicy(policy, now)
	if err := registry.ValidatePolicy(policy); err != nil {
		return base.Result[domain.ServiceRegistryPolicy]{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorizeLocked(actor, domain.PermissionRegistryPolicyWrite, domain.AccessTarget{Type: "application", ID: applicationID}); err != nil {
		return base.Result[domain.ServiceRegistryPolicy]{}, err
	}
	idemIdentity := ik(actor, "registry-policies.put:"+applicationID, key)
	old, replay := s.idempotency[idemIdentity]
	if err := check(old, replay, fingerprint); err != nil {
		return base.Result[domain.ServiceRegistryPolicy]{}, err
	}
	policyKey := registryScopeKey(policy.RegistryTargetID, applicationID)
	if replay {
		current, ok := s.registryPolicies[policyKey]
		if !ok || old.resourceID != applicationID {
			return base.Result[domain.ServiceRegistryPolicy]{}, base.ErrNotFound
		}
		return base.Result[domain.ServiceRegistryPolicy]{Value: current, Replay: true}, nil
	}
	target, ok := s.registryTargets[policy.RegistryTargetID]
	if !ok {
		return base.Result[domain.ServiceRegistryPolicy]{}, base.ErrNotFound
	}
	if err := registry.ValidatePolicyForTarget(target, policy); err != nil {
		return base.Result[domain.ServiceRegistryPolicy]{}, err
	}
	if current, ok := s.registryPolicies[policyKey]; ok {
		policy.CreatedAt = current.CreatedAt
	}
	for otherKey, current := range s.registryPolicies {
		if otherKey != policyKey && current.RegistryTargetID == policy.RegistryTargetID && current.Repository == policy.Repository {
			return base.Result[domain.ServiceRegistryPolicy]{}, base.ErrConflict
		}
	}
	s.registryPolicies[policyKey] = policy
	s.idempotency[idemIdentity] = idemRecord{fingerprint: fingerprint, typ: "registry-policy", resourceID: applicationID}
	s.audits++
	return base.Result[domain.ServiceRegistryPolicy]{Value: policy}, nil
}

func (s *Store) SaveRegistryCleanupPreviewForActor(_ context.Context, actor, key, fingerprint, _, applicationID string, plan domain.RegistryCleanupPlan) (base.Result[domain.RegistryCleanupPlan], error) {
	if plan.ServiceID != applicationID {
		return base.Result[domain.RegistryCleanupPlan]{}, base.ErrRegistryPolicyInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorizeLocked(actor, domain.PermissionRegistryCleanupPreview, domain.AccessTarget{Type: "application", ID: applicationID}); err != nil {
		return base.Result[domain.RegistryCleanupPlan]{}, err
	}
	idemIdentity := ik(actor, "registry-cleanup.preview:"+applicationID, key)
	old, replay := s.idempotency[idemIdentity]
	if err := check(old, replay, fingerprint); err != nil {
		return base.Result[domain.RegistryCleanupPlan]{}, err
	}
	if replay {
		current, ok := s.registryPlans[old.resourceID]
		if !ok || current.ServiceID != applicationID {
			return base.Result[domain.RegistryCleanupPlan]{}, base.ErrNotFound
		}
		return base.Result[domain.RegistryCleanupPlan]{Value: clonePlan(current), Replay: true}, nil
	}
	saved, _, err := s.saveRegistryCleanupPlanLocked(plan)
	if err != nil {
		return base.Result[domain.RegistryCleanupPlan]{}, err
	}
	s.idempotency[idemIdentity] = idemRecord{fingerprint: fingerprint, typ: "registry-cleanup-plan", resourceID: saved.ID}
	s.audits++
	return base.Result[domain.RegistryCleanupPlan]{Value: saved}, nil
}

func (s *Store) RegistryCleanupPlanForActor(_ context.Context, actor, planID string) (domain.RegistryCleanupPlan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	plan, ok := s.registryPlans[planID]
	if !ok {
		return domain.RegistryCleanupPlan{}, base.ErrNotFound
	}
	if err := s.authorizeLocked(actor, domain.PermissionRegistryRead, domain.AccessTarget{Type: "application", ID: plan.ServiceID}); err != nil {
		return domain.RegistryCleanupPlan{}, err
	}
	return clonePlan(plan), nil
}

func (s *Store) PrepareRegistryCleanupExecutionForActor(_ context.Context, actor, key, fingerprint, _, applicationID, confirmation string) (base.Result[domain.RegistryCleanupPlan], error) {
	planID := confirmation
	s.mu.Lock()
	defer s.mu.Unlock()
	plan, ok := s.registryPlans[planID]
	if !ok || plan.ServiceID != applicationID {
		return base.Result[domain.RegistryCleanupPlan]{}, base.ErrNotFound
	}
	if err := s.authorizeLocked(actor, domain.PermissionRegistryCleanupExecute, domain.AccessTarget{Type: "application", ID: applicationID}); err != nil {
		return base.Result[domain.RegistryCleanupPlan]{}, err
	}
	target, ok := s.registryTargets[plan.RegistryTargetID]
	if !ok {
		return base.Result[domain.RegistryCleanupPlan]{}, base.ErrNotFound
	}
	if target.Mode != domain.RegistryTargetManaged {
		return base.Result[domain.RegistryCleanupPlan]{}, base.ErrRegistryExternalLifecycle
	}
	idemIdentity := ik(actor, "registry-cleanup.execute:"+planID, key)
	old, replay := s.idempotency[idemIdentity]
	if err := check(old, replay, fingerprint); err != nil {
		return base.Result[domain.RegistryCleanupPlan]{}, err
	}
	if replay {
		current, ok := s.registryPlans[old.resourceID]
		if !ok || current.ServiceID != applicationID {
			return base.Result[domain.RegistryCleanupPlan]{}, base.ErrNotFound
		}
		return base.Result[domain.RegistryCleanupPlan]{Value: clonePlan(current), Replay: true}, nil
	}
	if plan.State != "preview" && !base.RegistryCleanupPlanCanResumeOfflineSweep(plan) {
		return base.Result[domain.RegistryCleanupPlan]{}, base.ErrConflict
	}
	s.idempotency[idemIdentity] = idemRecord{fingerprint: fingerprint, typ: "registry-cleanup-plan", resourceID: plan.ID}
	s.audits++
	return base.Result[domain.RegistryCleanupPlan]{Value: clonePlan(plan)}, nil
}
