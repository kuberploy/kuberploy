package memory

import (
	"context"
	"slices"
	"sort"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/externaldns"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func (s *Store) ListExternalDNSIntegrationsForActor(_ context.Context, actor string) ([]domain.ExternalDNSIntegration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorizeLocked(actor, domain.PermissionExternalDNSManage, domain.AccessTarget{Type: "platform", ID: "platform"}); err != nil {
		return nil, err
	}
	return s.externalDNSIntegrationsLocked(nil), nil
}

func (s *Store) ListExternalDNSIntegrationsForRuntime(_ context.Context, limit int) ([]domain.ExternalDNSIntegration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit < 1 || limit > 100 {
		return nil, base.ErrConflict
	}
	items := s.externalDNSIntegrationsLocked(func(item domain.ExternalDNSIntegration) bool {
		return item.Lifecycle == "active" || item.ProtectedGitState != "dematerialized"
	})
	if len(items) > limit {
		return nil, base.ErrConflict
	}
	return items, nil
}

func (s *Store) CreateExternalDNSIntegrationForActor(_ context.Context, actor, key, fingerprint, _ string, integration domain.ExternalDNSIntegration) (base.Result[domain.ExternalDNSIntegration], error) {
	if err := externaldns.Validate(integration); err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorizeLocked(actor, domain.PermissionExternalDNSManage, domain.AccessTarget{Type: "platform", ID: "platform"}); err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, err
	}
	idemIdentity := ik(actor, "external-dns-integrations.create", key)
	old, replay := s.idempotency[idemIdentity]
	if err := check(old, replay, fingerprint); err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, err
	}
	if replay {
		current, ok := s.externalDNSIntegrations[old.resourceID]
		if !ok {
			return base.Result[domain.ExternalDNSIntegration]{}, base.ErrNotFound
		}
		return base.Result[domain.ExternalDNSIntegration]{Value: cloneExternalDNSIntegration(current), Replay: true}, nil
	}
	if _, exists := s.externalDNSIntegrations[integration.ID]; exists || s.externalDNSIdentityConflictLocked(integration, "") {
		return base.Result[domain.ExternalDNSIntegration]{}, base.ErrConflict
	}
	if err := s.externalDNSEnvironmentsExistLocked(integration.EnvironmentIDs); err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, err
	}
	now := time.Now().UTC()
	integration.CreatedBy = actor
	integration.CreatedAt = now
	integration.UpdatedAt = now
	integration.RuntimeRevision, integration.Lifecycle = 1, "active"
	integration.ProtectedGitState = "pending"
	s.externalDNSIntegrations[integration.ID] = cloneExternalDNSIntegration(integration)
	s.invalidateExternalDNSProjectionBindingsLocked(integration.EnvironmentIDs, now)
	s.idempotency[idemIdentity] = idemRecord{fingerprint: fingerprint, typ: "external-dns-integration", resourceID: integration.ID}
	s.audits++
	return base.Result[domain.ExternalDNSIntegration]{Value: cloneExternalDNSIntegration(integration)}, nil
}

func (s *Store) UpdateExternalDNSIntegrationForActor(_ context.Context, actor, key, fingerprint, _ string, integration domain.ExternalDNSIntegration) (base.Result[domain.ExternalDNSIntegration], error) {
	if err := externaldns.Validate(integration); err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorizeLocked(actor, domain.PermissionExternalDNSManage, domain.AccessTarget{Type: "platform", ID: "platform"}); err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, err
	}
	idemIdentity := ik(actor, "external-dns-integrations.update:"+integration.ID, key)
	old, replay := s.idempotency[idemIdentity]
	if err := check(old, replay, fingerprint); err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, err
	}
	if replay {
		current, ok := s.externalDNSIntegrations[old.resourceID]
		if !ok {
			return base.Result[domain.ExternalDNSIntegration]{}, base.ErrNotFound
		}
		return base.Result[domain.ExternalDNSIntegration]{Value: cloneExternalDNSIntegration(current), Replay: true}, nil
	}
	current, exists := s.externalDNSIntegrations[integration.ID]
	if !exists {
		return base.Result[domain.ExternalDNSIntegration]{}, base.ErrNotFound
	}
	if current.Lifecycle == "deactivated" || current.Slug != integration.Slug || current.TXTOwnerID != integration.TXTOwnerID {
		return base.Result[domain.ExternalDNSIntegration]{}, base.ErrConflict
	}
	if s.externalDNSIdentityConflictLocked(integration, integration.ID) {
		return base.Result[domain.ExternalDNSIntegration]{}, base.ErrConflict
	}
	if err := s.externalDNSEnvironmentsExistLocked(integration.EnvironmentIDs); err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, err
	}
	integration.CreatedBy = current.CreatedBy
	integration.CreatedAt = current.CreatedAt
	integration.UpdatedAt = time.Now().UTC()
	changed := current.Name != integration.Name || current.Mode != integration.Mode || current.ProviderKind != integration.ProviderKind || !slices.Equal(current.AllowedDomainSuffixes, integration.AllowedDomainSuffixes) || current.SyncPolicy != integration.SyncPolicy || current.DestructiveSyncConfirmed != integration.DestructiveSyncConfirmed || current.CredentialSecretRef != integration.CredentialSecretRef || current.ProviderConfigRef != integration.ProviderConfigRef || current.EgressConfigRef != integration.EgressConfigRef || current.OperatorProfileRef != integration.OperatorProfileRef
	integration.RuntimeRevision, integration.Lifecycle = current.RuntimeRevision, "active"
	if changed {
		integration.RuntimeRevision++
		integration.ProtectedGitState = "pending"
	} else {
		integration.ProtectedGitState = current.ProtectedGitState
		integration.ProtectedGitRevision = current.ProtectedGitRevision
		integration.ProtectedGitContentDigest = current.ProtectedGitContentDigest
		integration.ProtectedGitCommit = current.ProtectedGitCommit
		integration.ProtectedGitObservedAt = current.ProtectedGitObservedAt
	}
	s.externalDNSIntegrations[integration.ID] = cloneExternalDNSIntegration(integration)
	affectedEnvironments := append(append([]string(nil), current.EnvironmentIDs...), integration.EnvironmentIDs...)
	s.invalidateExternalDNSProjectionBindingsLocked(affectedEnvironments, integration.UpdatedAt)
	s.idempotency[idemIdentity] = idemRecord{fingerprint: fingerprint, typ: "external-dns-integration", resourceID: integration.ID}
	s.audits++
	return base.Result[domain.ExternalDNSIntegration]{Value: cloneExternalDNSIntegration(integration)}, nil
}

func (s *Store) DeactivateExternalDNSIntegrationForActor(_ context.Context, actor, key, fingerprint, _ string, integrationID string) (base.Result[domain.ExternalDNSIntegration], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorizeLocked(actor, domain.PermissionExternalDNSManage, domain.AccessTarget{Type: "platform", ID: "platform"}); err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, err
	}
	identity := ik(actor, "external-dns-integrations.deactivate:"+integrationID, key)
	old, replay := s.idempotency[identity]
	if err := check(old, replay, fingerprint); err != nil {
		return base.Result[domain.ExternalDNSIntegration]{}, err
	}
	item, ok := s.externalDNSIntegrations[integrationID]
	if !ok {
		return base.Result[domain.ExternalDNSIntegration]{}, base.ErrNotFound
	}
	if replay {
		return base.Result[domain.ExternalDNSIntegration]{Value: cloneExternalDNSIntegration(item), Replay: true}, nil
	}
	if item.Lifecycle == "deactivated" {
		return base.Result[domain.ExternalDNSIntegration]{}, base.ErrConflict
	}
	now := time.Now().UTC()
	item.Lifecycle, item.DeactivatedBy, item.DeactivatedAt, item.UpdatedAt = "deactivated", actor, &now, now
	item.ProtectedGitState, item.ProtectedGitRevision, item.ProtectedGitContentDigest, item.ProtectedGitCommit, item.ProtectedGitObservedAt = "pending", 0, "", "", nil
	s.externalDNSIntegrations[integrationID] = cloneExternalDNSIntegration(item)
	s.invalidateExternalDNSProjectionBindingsLocked(item.EnvironmentIDs, now)
	s.idempotency[identity] = idemRecord{fingerprint: fingerprint, typ: "external-dns-integration", resourceID: integrationID}
	s.audits++
	return base.Result[domain.ExternalDNSIntegration]{Value: cloneExternalDNSIntegration(item)}, nil
}

func (s *Store) RecordExternalDNSPublication(_ context.Context, integrationID string, revision int64, deleted bool, contentDigest, commit string, observedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.externalDNSIntegrations[integrationID]
	if !ok || item.RuntimeRevision != revision || observedAt.IsZero() || deleted != (item.Lifecycle == "deactivated") {
		return base.ErrConflict
	}
	item.ProtectedGitState = "materialized"
	if deleted {
		item.ProtectedGitState = "dematerialized"
		contentDigest = ""
	}
	item.ProtectedGitRevision, item.ProtectedGitContentDigest, item.ProtectedGitCommit, item.ProtectedGitObservedAt = revision, contentDigest, commit, &observedAt
	s.externalDNSIntegrations[integrationID] = cloneExternalDNSIntegration(item)
	return nil
}

func (s *Store) invalidateExternalDNSProjectionBindingsLocked(environmentIDs []string, changedAt time.Time) {
	seen := make(map[string]struct{}, len(environmentIDs))
	for _, environmentID := range environmentIDs {
		if _, duplicate := seen[environmentID]; duplicate {
			continue
		}
		seen[environmentID] = struct{}{}
		binding, exists := s.gitBindings[environmentID]
		if !exists {
			continue
		}
		if binding.TargetHeadRevision != "" {
			binding.State = gitprojection.BindingIndexing
		}
		updatedAt := changedAt.UTC()
		if !updatedAt.After(binding.UpdatedAt) {
			updatedAt = binding.UpdatedAt.Add(time.Microsecond)
		}
		binding.UpdatedAt = updatedAt
		s.gitBindings[environmentID] = binding
	}
}

func (s *Store) ExternalDNSIntegrationsForEnvironmentActor(_ context.Context, actor, environmentID string) ([]domain.ExternalDNSIntegration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorizeLocked(actor, domain.PermissionExternalDNSRead, domain.AccessTarget{Type: "environment", ID: environmentID}); err != nil {
		return nil, err
	}
	return s.externalDNSIntegrationsLocked(func(item domain.ExternalDNSIntegration) bool {
		return item.Lifecycle != "deactivated" && containsExternalDNSEnvironment(item, environmentID)
	}), nil
}

func (s *Store) ExternalDNSIntegrationsForApplicationActor(_ context.Context, actor, applicationID, environmentID string) ([]domain.ExternalDNSIntegration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorizeLocked(actor, domain.PermissionExternalDNSRead, domain.AccessTarget{Type: "application", ID: applicationID}); err != nil {
		return nil, err
	}
	application, applicationOK := s.applications[applicationID]
	environment, environmentOK := s.environments[environmentID]
	if !applicationOK || !environmentOK || application.ProjectID != environment.ProjectID {
		return nil, base.ErrNotFound
	}
	return s.externalDNSIntegrationsLocked(func(item domain.ExternalDNSIntegration) bool {
		return item.Lifecycle != "deactivated" && containsExternalDNSEnvironment(item, environmentID)
	}), nil
}

func (s *Store) externalDNSIntegrationsLocked(include func(domain.ExternalDNSIntegration) bool) []domain.ExternalDNSIntegration {
	const maximumRows = 101
	items := make([]domain.ExternalDNSIntegration, 0)
	for _, item := range s.externalDNSIntegrations {
		if include == nil || include(item) {
			items = append(items, cloneExternalDNSIntegration(item))
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Name != items[j].Name {
			return items[i].Name < items[j].Name
		}
		return items[i].ID < items[j].ID
	})
	if len(items) > maximumRows {
		items = items[:maximumRows]
	}
	return items
}

func (s *Store) externalDNSIdentityConflictLocked(candidate domain.ExternalDNSIntegration, exceptID string) bool {
	for id, current := range s.externalDNSIntegrations {
		if id != exceptID && (current.Slug == candidate.Slug || current.TXTOwnerID == candidate.TXTOwnerID) {
			return true
		}
	}
	return false
}

func (s *Store) externalDNSEnvironmentsExistLocked(environmentIDs []string) error {
	for _, environmentID := range environmentIDs {
		if _, exists := s.environments[environmentID]; !exists {
			return base.ErrNotFound
		}
	}
	return nil
}

func containsExternalDNSEnvironment(item domain.ExternalDNSIntegration, environmentID string) bool {
	for _, current := range item.EnvironmentIDs {
		if current == environmentID {
			return true
		}
	}
	return false
}

func cloneExternalDNSIntegration(item domain.ExternalDNSIntegration) domain.ExternalDNSIntegration {
	item.AllowedDomainSuffixes = append([]string(nil), item.AllowedDomainSuffixes...)
	item.EnvironmentIDs = append([]string(nil), item.EnvironmentIDs...)
	if item.DeactivatedAt != nil {
		value := *item.DeactivatedAt
		item.DeactivatedAt = &value
	}
	if item.ProtectedGitObservedAt != nil {
		value := *item.ProtectedGitObservedAt
		item.ProtectedGitObservedAt = &value
	}
	return item
}
