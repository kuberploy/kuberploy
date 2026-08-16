package memory

import (
	"context"
	"sort"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/registry"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func (s *Store) ListRegistryPullTargetsForActor(_ context.Context, actor, projectID string) ([]domain.RegistryTarget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorizeLocked(actor, domain.PermissionRegistryRead, domain.AccessTarget{Type: "project", ID: projectID}); err != nil {
		return nil, err
	}
	items := make([]domain.RegistryTarget, 0)
	for _, target := range s.registryTargets {
		if target.PullCredentialRef != "" {
			items = append(items, target)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items, nil
}

func (s *Store) ListProjectRegistryPullCredentialsForActor(_ context.Context, actor, projectID string) ([]domain.ProjectRegistryPullCredential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorizeLocked(actor, domain.PermissionRegistryRead, domain.AccessTarget{Type: "project", ID: projectID}); err != nil {
		return nil, err
	}
	items := make([]domain.ProjectRegistryPullCredential, 0)
	for _, item := range s.projectRegistryPullCredentials {
		if item.ProjectID == projectID {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items, nil
}

func (s *Store) CreateProjectRegistryPullCredentialForActor(_ context.Context, actor, key, fingerprint, _ string, item domain.ProjectRegistryPullCredential) (base.Result[domain.ProjectRegistryPullCredential], error) {
	if err := registry.ValidateProjectPullCredential(item); err != nil {
		return base.Result[domain.ProjectRegistryPullCredential]{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorizeLocked(actor, domain.PermissionRegistryPolicyWrite, domain.AccessTarget{Type: "project", ID: item.ProjectID}); err != nil {
		return base.Result[domain.ProjectRegistryPullCredential]{}, err
	}
	identity := ik(actor, "project-registry-pull-credentials.create:"+item.ProjectID, key)
	old, replay := s.idempotency[identity]
	if err := check(old, replay, fingerprint); err != nil {
		return base.Result[domain.ProjectRegistryPullCredential]{}, err
	}
	if replay {
		current, ok := s.projectRegistryPullCredentials[old.resourceID]
		if !ok {
			return base.Result[domain.ProjectRegistryPullCredential]{}, base.ErrNotFound
		}
		return base.Result[domain.ProjectRegistryPullCredential]{Value: current, Replay: true}, nil
	}
	target, ok := s.registryTargets[item.RegistryTargetID]
	if !ok {
		return base.Result[domain.ProjectRegistryPullCredential]{}, base.ErrNotFound
	}
	if target.PullCredentialRef == "" {
		return base.Result[domain.ProjectRegistryPullCredential]{}, base.ErrRegistryPolicyInvalid
	}
	if s.projectRegistryPullCredentials == nil {
		s.projectRegistryPullCredentials = map[string]domain.ProjectRegistryPullCredential{}
	}
	for _, current := range s.projectRegistryPullCredentials {
		if current.ProjectID == item.ProjectID && (current.Name == item.Name || current.RegistryTargetID == item.RegistryTargetID) {
			return base.Result[domain.ProjectRegistryPullCredential]{}, base.ErrConflict
		}
	}
	now := time.Now().UTC()
	item.RegistryName, item.RegistryServer, item.RepositoryPrefix = target.Name, target.Endpoint, target.RepositoryPrefix
	item.CreatedAt, item.UpdatedAt = now, now
	s.projectRegistryPullCredentials[item.ID] = item
	s.idempotency[identity] = idemRecord{fingerprint: fingerprint, typ: "project-registry-pull-credential", resourceID: item.ID}
	s.audits++
	return base.Result[domain.ProjectRegistryPullCredential]{Value: item}, nil
}

func (s *Store) DeleteProjectRegistryPullCredentialForActor(_ context.Context, actor, projectID, credentialID, key, fp, _ string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorizeLocked(actor, domain.PermissionRegistryPolicyWrite, domain.AccessTarget{Type: "project", ID: projectID}); err != nil {
		return false, err
	}
	identity := ik(actor, "project-registry-pull-credentials.delete:"+projectID+":"+credentialID, key)
	old, replay := s.idempotency[identity]
	if err := check(old, replay, fp); err != nil {
		return false, err
	}
	if replay {
		return true, nil
	}
	item, ok := s.projectRegistryPullCredentials[credentialID]
	if !ok {
		return false, base.ErrNotFound
	}
	if item.ProjectID != projectID {
		return false, base.ErrNotFound
	}
	for _, selection := range s.applicationRegistryPullSelections {
		if selection.ProjectCredentialID == credentialID {
			return false, base.ErrConflict
		}
	}
	delete(s.projectRegistryPullCredentials, credentialID)
	s.idempotency[identity] = idemRecord{fingerprint: fp, typ: "project-registry-pull-credential", resourceID: credentialID}
	s.audits++
	return false, nil
}

func (s *Store) ApplicationRegistryPullSelectionForActor(_ context.Context, actor, applicationID string) (domain.ApplicationRegistryPullSelection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorizeLocked(actor, domain.PermissionRegistryRead, domain.AccessTarget{Type: "application", ID: applicationID}); err != nil {
		return domain.ApplicationRegistryPullSelection{}, err
	}
	selection, ok := s.applicationRegistryPullSelections[applicationID]
	if !ok {
		return domain.ApplicationRegistryPullSelection{ApplicationID: applicationID, Mode: domain.ApplicationRegistryPullPublic}, nil
	}
	return selection, nil
}

func (s *Store) PutApplicationRegistryPullSelectionForActor(_ context.Context, actor, key, fingerprint, _ string, selection domain.ApplicationRegistryPullSelection) (base.Result[domain.ApplicationRegistryPullSelection], error) {
	if err := registry.ValidateApplicationPullSelection(selection); err != nil {
		return base.Result[domain.ApplicationRegistryPullSelection]{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorizeLocked(actor, domain.PermissionRegistryPolicyWrite, domain.AccessTarget{Type: "application", ID: selection.ApplicationID}); err != nil {
		return base.Result[domain.ApplicationRegistryPullSelection]{}, err
	}
	identity := ik(actor, "application-registry-pull-selection.put:"+selection.ApplicationID, key)
	old, replay := s.idempotency[identity]
	if err := check(old, replay, fingerprint); err != nil {
		return base.Result[domain.ApplicationRegistryPullSelection]{}, err
	}
	if replay {
		current, ok := s.applicationRegistryPullSelections[selection.ApplicationID]
		if !ok {
			return base.Result[domain.ApplicationRegistryPullSelection]{}, base.ErrNotFound
		}
		return base.Result[domain.ApplicationRegistryPullSelection]{Value: current, Replay: true}, nil
	}
	app, ok := s.applications[selection.ApplicationID]
	if !ok {
		return base.Result[domain.ApplicationRegistryPullSelection]{}, base.ErrNotFound
	}
	if selection.Mode == domain.ApplicationRegistryPullCredential {
		credential, ok := s.projectRegistryPullCredentials[selection.ProjectCredentialID]
		if !ok || credential.ProjectID != app.ProjectID {
			return base.Result[domain.ApplicationRegistryPullSelection]{}, base.ErrNotFound
		}
	}
	if s.applicationRegistryPullSelections == nil {
		s.applicationRegistryPullSelections = map[string]domain.ApplicationRegistryPullSelection{}
	}
	selection.UpdatedAt = time.Now().UTC()
	s.applicationRegistryPullSelections[selection.ApplicationID] = selection
	s.idempotency[identity] = idemRecord{fingerprint: fingerprint, typ: "application-registry-pull-selection", resourceID: selection.ApplicationID}
	s.audits++
	return base.Result[domain.ApplicationRegistryPullSelection]{Value: selection}, nil
}
