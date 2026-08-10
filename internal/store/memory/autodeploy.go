package memory

import (
	"context"
	"sort"

	accesspolicy "github.com/kuberploy/kuberploy/internal/access"
	"github.com/kuberploy/kuberploy/internal/autodeploy"
	"github.com/kuberploy/kuberploy/internal/domain"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func autoDeployCommandKey(actorID, key string) string { return actorID + "\x00" + key }

func (s *Store) PolicyCommandReplay(_ context.Context, actorID, key, action, digest string) (autodeploy.Policy, autodeploy.Revision, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.autoDeployPolicyReplayLocked(actorID, key, action, digest)
}

func (s *Store) autoDeployPolicyReplayLocked(actorID, key, action, digest string) (autodeploy.Policy, autodeploy.Revision, bool, error) {
	command, ok := s.autoDeployCommands[autoDeployCommandKey(actorID, key)]
	if !ok {
		return autodeploy.Policy{}, autodeploy.Revision{}, false, nil
	}
	if command.action != action || command.digest != digest {
		return autodeploy.Policy{}, autodeploy.Revision{}, false, base.ErrIdempotencyConflict
	}
	policy, ok := s.autoDeployPolicies[command.policyID]
	if !ok {
		return autodeploy.Policy{}, autodeploy.Revision{}, false, base.ErrConflict
	}
	revision, ok := s.autoDeployRevisions[policy.ID][command.revision]
	if !ok {
		return autodeploy.Policy{}, autodeploy.Revision{}, false, base.ErrConflict
	}
	return policy, revision, true, nil
}

func (s *Store) CreatePolicy(_ context.Context, policy autodeploy.Policy, revision autodeploy.Revision, key, digest, requestID string) (autodeploy.Policy, autodeploy.Revision, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if replayPolicy, replayRevision, found, err := s.autoDeployPolicyReplayLocked(policy.CreatedBy, key, "create", digest); err != nil || found {
		return replayPolicy, replayRevision, found, err
	}
	if policy.Validate() != nil || revision.ValidateFor(policy) != nil || policy.CurrentRevision != 1 || revision.Revision != 1 || key == "" || digest == "" || requestID == "" {
		return autodeploy.Policy{}, autodeploy.Revision{}, false, autodeploy.ErrInvalid
	}
	if err := s.authorizeAutoDeployPolicyMutationLocked(policy.CreatedBy, policy.ProjectID, policy.EnvironmentID, policy.ApplicationID, revision.ServiceActorID); err != nil {
		return autodeploy.Policy{}, autodeploy.Revision{}, false, err
	}
	if _, exists := s.autoDeployPolicies[policy.ID]; exists {
		return autodeploy.Policy{}, autodeploy.Revision{}, false, base.ErrConflict
	}
	for _, current := range s.autoDeployPolicies {
		if current.BuildDefinitionID == policy.BuildDefinitionID && current.EnvironmentID == policy.EnvironmentID {
			return autodeploy.Policy{}, autodeploy.Revision{}, false, base.ErrConflict
		}
	}
	s.autoDeployPolicies[policy.ID] = policy
	s.autoDeployRevisions[policy.ID] = map[int64]autodeploy.Revision{revision.Revision: revision}
	s.autoDeployCommands[autoDeployCommandKey(policy.CreatedBy, key)] = autoDeployCommandRecord{action: "create", digest: digest, policyID: policy.ID, revision: revision.Revision}
	s.audits++
	return policy, revision, false, nil
}

func (s *Store) RevisePolicy(_ context.Context, prior autodeploy.Policy, revision autodeploy.Revision, key, digest, requestID string) (autodeploy.Policy, autodeploy.Revision, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if replayPolicy, replayRevision, found, err := s.autoDeployPolicyReplayLocked(revision.CreatedBy, key, "revise", digest); err != nil || found {
		return replayPolicy, replayRevision, found, err
	}
	current, exists := s.autoDeployPolicies[prior.ID]
	if !exists {
		return autodeploy.Policy{}, autodeploy.Revision{}, false, base.ErrNotFound
	}
	next := prior
	next.CurrentRevision = revision.Revision
	if current != prior || revision.Revision != prior.CurrentRevision+1 || revision.ValidateFor(next) != nil || key == "" || digest == "" || requestID == "" {
		return autodeploy.Policy{}, autodeploy.Revision{}, false, base.ErrConflict
	}
	if err := s.authorizeAutoDeployPolicyMutationLocked(revision.CreatedBy, prior.ProjectID, prior.EnvironmentID, prior.ApplicationID, revision.ServiceActorID); err != nil {
		return autodeploy.Policy{}, autodeploy.Revision{}, false, err
	}
	s.autoDeployPolicies[prior.ID] = next
	s.autoDeployRevisions[prior.ID][revision.Revision] = revision
	s.autoDeployCommands[autoDeployCommandKey(revision.CreatedBy, key)] = autoDeployCommandRecord{action: "revise", digest: digest, policyID: prior.ID, revision: revision.Revision}
	s.audits++
	return next, revision, false, nil
}

func (s *Store) authorizeAutoDeployPolicyMutationLocked(actorID, projectID, environmentID, applicationID, serviceActorID string) error {
	if err := s.authorizeLocked(actorID, domain.PermissionBuildsManage, domain.AccessTarget{Type: "application", ID: applicationID}); err != nil {
		return err
	}
	environment, environmentOK := s.environments[environmentID]
	application, applicationOK := s.applications[applicationID]
	project, projectOK := s.projects[projectID]
	if !environmentOK || !applicationOK || !projectOK || environment.ProjectID != projectID || application.ProjectID != projectID {
		return base.ErrNotFound
	}
	target := domain.AccessTarget{Type: "deployment", ID: environmentID + ":" + applicationID, TeamID: project.TeamID,
		ProjectID: projectID, EnvironmentID: environmentID, Namespace: environment.Namespace, ApplicationID: applicationID}
	bindings := s.bindingsLocked(actorID)
	if !accesspolicy.HasPermission(bindings, target, domain.PermissionResourcesWrite) {
		return base.ErrForbidden
	}
	account, ok := s.serviceAccounts[serviceActorID]
	if !ok || account.ProjectID != projectID || account.DisabledAt != nil {
		return base.ErrNotFound
	}
	projectTarget := domain.AccessTarget{Type: "project", ID: projectID, ProjectID: projectID, TeamID: project.TeamID}
	if !accesspolicy.CanManageGrant(bindings, projectTarget, account.Role) {
		return base.ErrForbidden
	}
	return nil
}

func (s *Store) PolicyForActor(_ context.Context, actorID, policyID string) (autodeploy.PolicyStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	policy, ok := s.autoDeployPolicies[policyID]
	if !ok {
		return autodeploy.PolicyStatus{}, base.ErrNotFound
	}
	if err := s.authorizeLocked(actorID, domain.PermissionBuildsRead, domain.AccessTarget{Type: "application", ID: policy.ApplicationID}); err != nil {
		return autodeploy.PolicyStatus{}, err
	}
	return autodeploy.PolicyStatus{Policy: policy, CurrentRevision: s.autoDeployRevisions[policyID][policy.CurrentRevision]}, nil
}

func (s *Store) PoliciesForApplication(_ context.Context, actorID, applicationID string) ([]autodeploy.PolicyStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorizeLocked(actorID, domain.PermissionBuildsRead, domain.AccessTarget{Type: "application", ID: applicationID}); err != nil {
		return nil, err
	}
	items := make([]autodeploy.PolicyStatus, 0)
	for _, policy := range s.autoDeployPolicies {
		if policy.ApplicationID == applicationID {
			items = append(items, autodeploy.PolicyStatus{Policy: policy, CurrentRevision: s.autoDeployRevisions[policy.ID][policy.CurrentRevision]})
		}
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].Policy.CreatedAt.Before(items[j].Policy.CreatedAt) || items[i].Policy.CreatedAt.Equal(items[j].Policy.CreatedAt) && items[i].Policy.ID < items[j].Policy.ID
	})
	return items, nil
}

func (s *Store) PolicyRevisionsForActor(ctx context.Context, actorID, policyID string, limit int) ([]autodeploy.Revision, error) {
	if limit < 1 || limit > 100 {
		return nil, autodeploy.ErrInvalid
	}
	if _, err := s.PolicyForActor(ctx, actorID, policyID); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]autodeploy.Revision, 0, len(s.autoDeployRevisions[policyID]))
	for _, revision := range s.autoDeployRevisions[policyID] {
		items = append(items, revision)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Revision > items[j].Revision })
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (s *Store) PolicyRunsForActor(ctx context.Context, actorID, policyID string, limit int) ([]autodeploy.Run, error) {
	if limit < 1 || limit > 100 {
		return nil, autodeploy.ErrInvalid
	}
	if _, err := s.PolicyForActor(ctx, actorID, policyID); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	items := append([]autodeploy.Run(nil), s.autoDeployRuns[policyID]...)
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

var _ autodeploy.PolicyStore = (*Store)(nil)
var _ autodeploy.PolicyReader = (*Store)(nil)
