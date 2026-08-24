package memory

import (
	"context"
	"encoding/hex"
	"sort"
	"strings"
	"time"

	accesspolicy "github.com/kuberploy/kuberploy/internal/access"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/id"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func (s *Store) isAdminLocked(actor string) bool {
	if user, ok := s.users[actor]; !ok || user.Issuer == "kuberploy:deleted" {
		return false
	}
	return accesspolicy.HasPermission(s.bindingsLocked(actor), domain.AccessTarget{Type: "platform", ID: "platform"}, domain.PermissionPlatformAdmin)
}

func (s *Store) ownsTeamLocked(actor, teamID string) bool {
	member, ok := s.memberships[teamID][actor]
	return ok && member.Role == "owner"
}

func (s *Store) canManageTeamLocked(actor, teamID string) bool {
	target := domain.AccessTarget{Type: "team", ID: teamID, TeamID: teamID}
	return accesspolicy.HasPermission(s.bindingsLocked(actor), target, domain.PermissionGrantsManage)
}

func (s *Store) canAccessTeamLocked(actor, teamID string) bool {
	if teamID == "" {
		return s.isAdminLocked(actor)
	}
	if _, exists := s.teams[teamID]; !exists {
		return false
	}
	return accesspolicy.HasPermission(s.bindingsLocked(actor), domain.AccessTarget{Type: "team", ID: teamID, TeamID: teamID}, domain.PermissionResourcesRead)
}

func (s *Store) canAccessProjectLocked(actor string, project domain.Project) bool {
	if project.ID == "" {
		return false
	}
	bindings := s.bindingsLocked(actor)
	if accesspolicy.HasPermission(bindings, domain.AccessTarget{Type: "project", ID: project.ID, TeamID: project.TeamID, ProjectID: project.ID}, domain.PermissionResourcesRead) {
		return true
	}
	for _, binding := range bindings {
		target, ok := s.scopeTargetForProjectLocked(project, binding.ScopeType, binding.ScopeID)
		if ok && accesspolicy.HasPermission([]domain.AccessBinding{binding}, target, domain.PermissionResourcesRead) {
			return true
		}
	}
	return false
}

func (s *Store) bindingsLocked(actor string) []domain.AccessBinding {
	bindings := make([]domain.AccessBinding, 0)
	for _, grant := range s.accessGrants {
		_, teamSubject := s.memberships[grant.SubjectTeamID][actor]
		if grant.SubjectUserID == actor || grant.SubjectTeamID != "" && teamSubject {
			bindings = append(bindings, domain.AccessBinding{Role: grant.Role, ScopeType: grant.ScopeType, ScopeID: grant.ScopeID, Permissions: append([]domain.Permission(nil), grant.Permissions...), Source: grant.Source})
		}
	}
	for teamID, members := range s.memberships {
		member, ok := members[actor]
		if !ok {
			continue
		}
		role := domain.RoleDeveloper
		if member.Role == "owner" {
			role = domain.RoleOrganizationAdmin
		}
		bindings = append(bindings, domain.AccessBinding{Role: role, ScopeType: domain.ScopeTeam, ScopeID: teamID, Source: "team-" + member.Role})
	}
	return bindings
}

func (s *Store) resolveTargetLocked(target domain.AccessTarget) (domain.AccessTarget, bool) {
	switch target.Type {
	case "platform":
		return domain.AccessTarget{Type: "platform", ID: "platform"}, true
	case "team":
		_, ok := s.teams[target.ID]
		return domain.AccessTarget{Type: "team", ID: target.ID, TeamID: target.ID}, ok
	case "project":
		project, ok := s.projects[target.ID]
		return domain.AccessTarget{Type: "project", ID: project.ID, TeamID: project.TeamID, ProjectID: project.ID}, ok
	case "environment":
		environment, ok := s.environments[target.ID]
		project := s.projects[environment.ProjectID]
		return domain.AccessTarget{Type: "environment", ID: environment.ID, TeamID: project.TeamID, ProjectID: project.ID, EnvironmentID: environment.ID, Namespace: environment.Namespace}, ok && project.ID != ""
	case "namespace":
		for _, environment := range s.environments {
			if environment.Namespace == target.ID {
				project := s.projects[environment.ProjectID]
				return domain.AccessTarget{Type: "namespace", ID: environment.Namespace, TeamID: project.TeamID, ProjectID: project.ID, EnvironmentID: environment.ID, Namespace: environment.Namespace}, project.ID != ""
			}
		}
		return domain.AccessTarget{}, false
	case "application":
		application, ok := s.applications[target.ID]
		project := s.projects[application.ProjectID]
		return domain.AccessTarget{Type: "application", ID: application.ID, TeamID: project.TeamID, ProjectID: project.ID, ApplicationID: application.ID}, ok && project.ID != ""
	case "secret-binding":
		application, applicationOK := s.applications[target.ApplicationID]
		environment, environmentOK := s.environments[target.EnvironmentID]
		project := s.projects[application.ProjectID]
		valid := target.ID != "" && applicationOK && environmentOK && project.ID != "" &&
			environment.ProjectID == project.ID && target.ProjectID == project.ID && target.TeamID == project.TeamID && target.Namespace == environment.Namespace
		return domain.AccessTarget{Type: "secret-binding", ID: target.ID, TeamID: project.TeamID, ProjectID: project.ID,
			EnvironmentID: environment.ID, Namespace: environment.Namespace, ApplicationID: application.ID}, valid
	case "deployment":
		deployment, ok := s.deployments[target.ID]
		environment := s.environments[deployment.EnvironmentID]
		project := s.projects[environment.ProjectID]
		return domain.AccessTarget{Type: "deployment", ID: deployment.ID, TeamID: project.TeamID, ProjectID: project.ID, EnvironmentID: environment.ID, Namespace: environment.Namespace, ApplicationID: deployment.ApplicationID}, ok && project.ID != ""
	case "operation":
		operation, ok := s.operations[target.ID]
		if !ok {
			return domain.AccessTarget{}, false
		}
		switch operation.TargetType {
		case "deployment", "environment", "project":
			return s.resolveTargetLocked(domain.AccessTarget{Type: operation.TargetType, ID: operation.TargetID})
		default:
			return domain.AccessTarget{}, false
		}
	default:
		return domain.AccessTarget{}, false
	}
}

func (s *Store) authorizeLocked(actor string, permission domain.Permission, target domain.AccessTarget) error {
	resolved, exists := s.resolveTargetLocked(target)
	if !exists {
		return base.ErrNotFound
	}
	bindings := s.bindingsLocked(actor)
	if accesspolicy.HasPermission(bindings, resolved, permission) {
		return nil
	}
	projectVisible := false
	if resolved.Type == "project" {
		projectVisible = s.canAccessProjectLocked(actor, s.projects[resolved.ProjectID])
	}
	if resolved.Type == "platform" || projectVisible || accesspolicy.HasPermission(bindings, resolved, domain.PermissionResourcesRead) {
		return base.ErrForbidden
	}
	return base.ErrNotFound
}

func (s *Store) Authorize(_ context.Context, actor string, permission domain.Permission, target domain.AccessTarget) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authorizeLocked(actor, permission, target)
}

func (s *Store) AuthorizePromotion(_ context.Context, actor, environmentID, applicationID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	environment, environmentOK := s.environments[environmentID]
	application, applicationOK := s.applications[applicationID]
	project, projectOK := s.projects[environment.ProjectID]
	if !environmentOK || !applicationOK || !projectOK || application.ProjectID != environment.ProjectID {
		return base.ErrNotFound
	}
	target := domain.AccessTarget{Type: "deployment", ID: environmentID + ":" + applicationID, TeamID: project.TeamID,
		ProjectID: project.ID, EnvironmentID: environment.ID, Namespace: environment.Namespace, ApplicationID: application.ID}
	bindings := s.bindingsLocked(actor)
	if accesspolicy.HasPermission(bindings, target, domain.PermissionResourcesWrite) {
		return nil
	}
	if accesspolicy.HasPermission(bindings, target, domain.PermissionResourcesRead) {
		return base.ErrForbidden
	}
	return base.ErrNotFound
}

func (s *Store) EffectiveCapabilities(_ context.Context, actor string) ([]domain.AccessCapability, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[actor]; !ok {
		return nil, base.ErrNotFound
	}
	return accesspolicy.Capabilities(s.bindingsLocked(actor)), nil
}

func (s *Store) bumpGrantsLocked(userIDs map[string]struct{}) {
	now := time.Now().UTC()
	for userID := range userIDs {
		u, ok := s.users[userID]
		if !ok || u.Role == "platform-admin" {
			continue
		}
		u.GrantRevision++
		s.users[userID] = u
		for tokenID, token := range s.serviceAccountTokens {
			if token.ServiceAccountID == userID && token.RevokedAt == nil {
				token.RevokedAt = &now
				s.serviceAccountTokens[tokenID] = token
			}
		}
		for token, session := range s.sessions {
			if session.userID == userID {
				delete(s.sessions, token)
			}
		}
	}
}

func (s *Store) CreateUserInvitation(_ context.Context, actor, email string, tokenHash []byte, expires time.Time, _ string) (domain.UserInvitation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.isAdminLocked(actor) {
		return domain.UserInvitation{}, base.ErrForbidden
	}
	now := time.Now().UTC()
	if len(tokenHash) != 32 || !expires.After(now) || expires.After(now.Add(24*time.Hour+time.Minute)) {
		return domain.UserInvitation{}, base.ErrConflict
	}
	key := hex.EncodeToString(tokenHash)
	if _, exists := s.invitations[key]; exists {
		return domain.UserInvitation{}, base.ErrConflict
	}
	invitation := domain.UserInvitation{ID: id.New(), Email: email, ExpiresAt: expires.UTC()}
	s.invitations[key] = invitationRecord{invitation: invitation, email: email}
	s.audits++
	return invitation, nil
}

func (s *Store) AcceptUserInvitation(_ context.Context, tokenHash []byte, displayName, passwordHash string, sessionHash, previousSessionHash []byte, sessionExpires time.Time) (domain.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := hex.EncodeToString(tokenHash)
	record, ok := s.invitations[key]
	now := time.Now().UTC()
	if !ok || record.accepted || !record.invitation.ExpiresAt.After(now) || len(sessionHash) != 32 || !sessionExpires.After(now) {
		return domain.User{}, base.ErrInvitationInvalid
	}
	u := domain.User{ID: id.New(), Email: record.email, DisplayName: displayName, Role: "developer", Issuer: "kuberploy:invitation", Subject: record.invitation.ID, GrantRevision: 1, CreatedAt: now}
	if s.passwordCredentials == nil {
		s.passwordCredentials = map[string]struct{ userID, hash string }{}
	}
	email := strings.ToLower(strings.TrimSpace(record.email))
	if _, exists := s.passwordCredentials[email]; exists || passwordHash == "" {
		return domain.User{}, base.ErrInvitationInvalid
	}
	if len(previousSessionHash) != 0 && len(previousSessionHash) != 32 {
		return domain.User{}, base.ErrInvitationInvalid
	}
	if len(previousSessionHash) == 32 {
		delete(s.sessions, hex.EncodeToString(previousSessionHash))
	}
	s.users[u.ID] = u
	s.passwordCredentials[email] = struct{ userID, hash string }{u.ID, passwordHash}
	s.sessions[hex.EncodeToString(sessionHash)] = struct {
		userID   string
		revision int64
		expires  time.Time
	}{u.ID, u.GrantRevision, sessionExpires}
	record.accepted = true
	s.invitations[key] = record
	s.audits++
	return u, nil
}

func (s *Store) ListUsersForActor(_ context.Context, actor string) ([]domain.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	visible := map[string]struct{}{}
	if s.isAdminLocked(actor) {
		for userID := range s.users {
			visible[userID] = struct{}{}
		}
	} else {
		owned := false
		for teamID := range s.teams {
			if !s.canManageTeamLocked(actor, teamID) {
				continue
			}
			owned = true
			for userID := range s.memberships[teamID] {
				visible[userID] = struct{}{}
			}
		}
		if !owned {
			return nil, base.ErrForbidden
		}
	}
	out := make([]domain.User, 0, len(visible))
	for userID := range visible {
		if _, serviceAccount := s.serviceAccounts[userID]; serviceAccount {
			continue
		}
		user := s.users[userID]
		if user.Issuer == "kuberploy:deleted" {
			continue
		}
		out = append(out, user)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (s *Store) DeleteUser(_ context.Context, actor, userID, confirmationEmail, key, fp, _ string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idemKey := ik(actor, "users.delete:"+userID, key)
	old, replay := s.idempotency[idemKey]
	if err := check(old, replay, fp); err != nil {
		return false, err
	}
	if replay {
		return true, nil
	}
	if !s.isAdminLocked(actor) {
		return false, base.ErrForbidden
	}
	if actor == userID {
		return false, base.ErrSelfDeletion
	}
	user, exists := s.users[userID]
	if !exists || user.Issuer == "kuberploy:deleted" {
		return false, nil
	}
	if _, serviceAccount := s.serviceAccounts[userID]; serviceAccount {
		return false, base.ErrNotFound
	}
	if !strings.EqualFold(strings.TrimSpace(confirmationEmail), strings.TrimSpace(user.Email)) {
		return false, base.ErrDeletionConfirmation
	}
	for _, installation := range s.installations {
		if installation.OwnerUserID == userID {
			return false, base.ErrUserDeletionBlocked
		}
	}
	for teamID, members := range s.memberships {
		if member, ok := members[userID]; ok && member.Role == "owner" && s.ownerCountLocked(teamID) == 1 {
			return false, base.ErrUserDeletionBlocked
		}
	}
	for teamID := range s.memberships {
		delete(s.memberships[teamID], userID)
	}
	for grantID, grant := range s.accessGrants {
		if grant.SubjectUserID == userID {
			delete(s.accessGrants, grantID)
		}
	}
	for token, session := range s.sessions {
		if session.userID == userID {
			delete(s.sessions, token)
		}
	}
	delete(s.passwordCredentials, strings.ToLower(strings.TrimSpace(user.Email)))
	user.Email = ""
	user.DisplayName = "Deleted user"
	user.Issuer = "kuberploy:deleted"
	user.GrantRevision++
	s.users[userID] = user
	s.idempotency[idemKey] = idemRecord{fp, "user", userID, ""}
	s.audits++
	return false, nil
}

func (s *Store) CreateTeam(_ context.Context, actor, key, fp, _ string, in domain.CreateTeam) (base.Result[domain.Team], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[actor]; !ok {
		return base.Result[domain.Team]{}, base.ErrForbidden
	}
	idemKey := ik(actor, "teams.create", key)
	old, replay := s.idempotency[idemKey]
	if err := check(old, replay, fp); err != nil {
		return base.Result[domain.Team]{}, err
	}
	if replay {
		return base.Result[domain.Team]{Value: s.teams[old.resourceID], Replay: true}, nil
	}
	for _, team := range s.teams {
		if team.Slug == in.Slug {
			return base.Result[domain.Team]{}, base.ErrConflict
		}
	}
	now := time.Now().UTC()
	team := domain.Team{ID: id.New(), Name: in.Name, Slug: in.Slug, CreatedAt: now}
	s.teams[team.ID] = team
	s.memberships[team.ID] = map[string]domain.TeamMember{actor: {TeamID: team.ID, UserID: actor, Role: "owner", CreatedAt: now}}
	s.idempotency[idemKey] = idemRecord{fp, "team", team.ID, ""}
	s.audits++
	s.bumpGrantsLocked(map[string]struct{}{actor: {}})
	return base.Result[domain.Team]{Value: team}, nil
}

func (s *Store) ListTeamsForActor(_ context.Context, actor string) ([]domain.Team, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.Team
	for teamID, team := range s.teams {
		if s.canAccessTeamLocked(actor, teamID) {
			out = append(out, team)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (s *Store) DeleteTeam(_ context.Context, actor, teamID, confirmationName, key, fp, _ string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idemKey := ik(actor, "teams.delete:"+teamID, key)
	old, replay := s.idempotency[idemKey]
	if err := check(old, replay, fp); err != nil {
		return false, err
	}
	if replay {
		return true, nil
	}
	if _, exists := s.teams[teamID]; !exists {
		return false, base.ErrNotFound
	}
	if !s.canManageTeamLocked(actor, teamID) {
		return false, base.ErrForbidden
	}
	if strings.TrimSpace(confirmationName) != s.teams[teamID].Name {
		return false, base.ErrDeletionConfirmation
	}
	for _, project := range s.projects {
		if project.TeamID == teamID {
			return false, base.ErrTeamDeletionBlocked
		}
	}
	for _, installation := range s.installations {
		if installation.TeamID == teamID {
			return false, base.ErrTeamDeletionBlocked
		}
	}
	affected := map[string]struct{}{}
	for userID := range s.memberships[teamID] {
		affected[userID] = struct{}{}
	}
	for grantID, grant := range s.accessGrants {
		if grant.SubjectTeamID == teamID || grant.ScopeType == domain.ScopeTeam && grant.ScopeID == teamID {
			delete(s.accessGrants, grantID)
		}
	}
	delete(s.memberships, teamID)
	delete(s.teams, teamID)
	s.idempotency[idemKey] = idemRecord{fp, "team", teamID, ""}
	s.audits++
	s.bumpGrantsLocked(affected)
	return false, nil
}

func (s *Store) ListTeamMembersForActor(_ context.Context, actor, teamID string) ([]domain.TeamMember, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.teams[teamID]; !exists || !s.canAccessTeamLocked(actor, teamID) {
		return nil, base.ErrNotFound
	}
	var out []domain.TeamMember
	for _, member := range s.memberships[teamID] {
		u := s.users[member.UserID]
		member.User = &u
		out = append(out, member)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (s *Store) AddTeamMember(_ context.Context, actor, teamID, key, fp, _ string, in domain.AddTeamMember) (base.Result[domain.TeamMember], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.teams[teamID]; !exists {
		return base.Result[domain.TeamMember]{}, base.ErrNotFound
	}
	if !s.canManageTeamLocked(actor, teamID) {
		return base.Result[domain.TeamMember]{}, base.ErrForbidden
	}
	if _, exists := s.users[in.UserID]; !exists {
		return base.Result[domain.TeamMember]{}, base.ErrNotFound
	}
	if _, serviceAccount := s.serviceAccounts[in.UserID]; serviceAccount {
		return base.Result[domain.TeamMember]{}, base.ErrNotFound
	}
	scope := "teams.members.add:" + teamID
	idemKey := ik(actor, scope, key)
	old, replay := s.idempotency[idemKey]
	if err := check(old, replay, fp); err != nil {
		return base.Result[domain.TeamMember]{}, err
	}
	if replay {
		member := s.memberships[teamID][old.resourceID]
		return base.Result[domain.TeamMember]{Value: member, Replay: true}, nil
	}
	now := time.Now().UTC()
	member, existed := s.memberships[teamID][in.UserID]
	if existed && member.Role == "owner" && in.Role != "owner" && s.ownerCountLocked(teamID) == 1 {
		return base.Result[domain.TeamMember]{}, base.ErrConflict
	}
	changed := !existed || member.Role != in.Role
	if !existed {
		member = domain.TeamMember{TeamID: teamID, UserID: in.UserID, CreatedAt: now}
	}
	member.Role = in.Role
	s.memberships[teamID][in.UserID] = member
	s.idempotency[idemKey] = idemRecord{fp, "team-member", in.UserID, ""}
	s.audits++
	if changed {
		s.bumpGrantsLocked(map[string]struct{}{in.UserID: {}})
	}
	return base.Result[domain.TeamMember]{Value: member}, nil
}

func (s *Store) ownerCountLocked(teamID string) int {
	owners := 0
	for _, member := range s.memberships[teamID] {
		if member.Role == "owner" {
			owners++
		}
	}
	return owners
}

func (s *Store) RemoveTeamMember(_ context.Context, actor, teamID, userID, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.teams[teamID]; !exists {
		return base.ErrNotFound
	}
	if !s.canManageTeamLocked(actor, teamID) {
		return base.ErrForbidden
	}
	member, exists := s.memberships[teamID][userID]
	if !exists {
		return nil
	}
	if member.Role == "owner" && s.ownerCountLocked(teamID) == 1 {
		return base.ErrConflict
	}
	delete(s.memberships[teamID], userID)
	s.audits++
	s.bumpGrantsLocked(map[string]struct{}{userID: {}})
	return nil
}

func (s *Store) CreateGitHubInstallation(_ context.Context, actor, key, fp, _ string, in domain.CreateGitHubInstallation) (base.Result[domain.GitHubInstallation], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.isAdminLocked(actor) {
		return base.Result[domain.GitHubInstallation]{}, base.ErrForbidden
	}
	idemKey := ik(actor, "github-installations.create", key)
	old, replay := s.idempotency[idemKey]
	if err := check(old, replay, fp); err != nil {
		return base.Result[domain.GitHubInstallation]{}, err
	}
	if replay {
		return base.Result[domain.GitHubInstallation]{Value: s.installations[old.resourceID], Replay: true}, nil
	}
	for _, installation := range s.installations {
		if installation.GitHubInstallationID == in.GitHubInstallationID {
			return base.Result[domain.GitHubInstallation]{}, base.ErrConflict
		}
	}
	now := time.Now().UTC()
	installation := domain.GitHubInstallation{ID: id.New(), GitHubInstallationID: in.GitHubInstallationID, AccountLogin: in.AccountLogin, AccountType: in.AccountType, OwnerUserID: actor, Visibility: "private", RepositorySelection: in.RepositorySelection, RepositoryCount: in.RepositoryCount, CreatedAt: now, UpdatedAt: now}
	s.installations[installation.ID] = installation
	s.idempotency[idemKey] = idemRecord{fp, "github-installation", installation.ID, ""}
	s.audits++
	return base.Result[domain.GitHubInstallation]{Value: installation}, nil
}

func (s *Store) LinkVerifiedGitHubInstallation(_ context.Context, actor, key, fp, _ string, in domain.CreateGitHubInstallation) (domain.GitHubInstallation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.users[actor]; !exists {
		return domain.GitHubInstallation{}, false, base.ErrNotFound
	}
	idemKey := ik(actor, "github-installations.link", key)
	old, replay := s.idempotency[idemKey]
	if err := check(old, replay, fp); err != nil {
		return domain.GitHubInstallation{}, false, err
	}
	if replay {
		installation, exists := s.installations[old.resourceID]
		if !exists {
			return domain.GitHubInstallation{}, false, base.ErrNotFound
		}
		return installation, true, nil
	}
	for installationID, installation := range s.installations {
		if installation.GitHubInstallationID == in.GitHubInstallationID {
			if installation.OwnerUserID != actor || !strings.EqualFold(installation.AccountLogin, in.AccountLogin) ||
				installation.AccountType != in.AccountType || installation.RepositorySelection != in.RepositorySelection {
				return domain.GitHubInstallation{}, false, base.ErrConflict
			}
			installation.RepositoryCount = in.RepositoryCount
			installation.UpdatedAt = time.Now().UTC()
			s.installations[installationID] = installation
			s.idempotency[idemKey] = idemRecord{fp, "github-installation", installation.ID, ""}
			s.audits++
			return installation, false, nil
		}
	}
	now := time.Now().UTC()
	installation := domain.GitHubInstallation{ID: id.New(), GitHubInstallationID: in.GitHubInstallationID, AccountLogin: in.AccountLogin,
		AccountType: in.AccountType, OwnerUserID: actor, Visibility: "private", RepositorySelection: in.RepositorySelection,
		RepositoryCount: in.RepositoryCount, CreatedAt: now, UpdatedAt: now}
	s.installations[installation.ID] = installation
	s.idempotency[idemKey] = idemRecord{fp, "github-installation", installation.ID, ""}
	s.audits++
	return installation, false, nil
}

func (s *Store) ListGitHubInstallationsForActor(_ context.Context, actor string) ([]domain.GitHubInstallation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.GitHubInstallation
	for _, installation := range s.installations {
		if s.isAdminLocked(actor) || installation.OwnerUserID == actor {
			out = append(out, installation)
			continue
		}
		if installation.Visibility == "team" {
			if _, ok := s.memberships[installation.TeamID][actor]; ok {
				out = append(out, installation)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (s *Store) AuthorizeGitHubInstallationForProject(_ context.Context, actor, installationID, projectID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	installation, installationExists := s.installations[installationID]
	project, projectExists := s.projects[projectID]
	if !installationExists || !projectExists {
		return base.ErrNotFound
	}
	if err := s.authorizeLocked(actor, domain.PermissionBuildsManage, domain.AccessTarget{Type: "project", ID: project.ID}); err != nil {
		return err
	}
	if s.isAdminLocked(actor) || installation.OwnerUserID == actor ||
		installation.Visibility == "team" && installation.TeamID != "" && installation.TeamID == project.TeamID {
		return nil
	}
	return base.ErrNotFound
}

func (s *Store) UpdateGitHubInstallationSharing(_ context.Context, actor, installationID, key, fp, _ string, in domain.UpdateGitHubInstallationSharing) (base.Result[domain.GitHubInstallation], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	installation, exists := s.installations[installationID]
	if !exists {
		return base.Result[domain.GitHubInstallation]{}, base.ErrNotFound
	}
	idemKey := ik(actor, "github-installations.sharing:"+installationID, key)
	old, replay := s.idempotency[idemKey]
	if err := check(old, replay, fp); err != nil {
		return base.Result[domain.GitHubInstallation]{}, err
	}
	if replay {
		stored, exists := s.installations[old.resourceID]
		if !exists {
			return base.Result[domain.GitHubInstallation]{}, base.ErrNotFound
		}
		return base.Result[domain.GitHubInstallation]{Value: stored, Replay: true}, nil
	}
	admin := s.isAdminLocked(actor)
	currentTeamOwner := installation.TeamID != "" && s.canManageTeamLocked(actor, installation.TeamID)
	if !admin && actor != installation.OwnerUserID && !currentTeamOwner {
		return base.Result[domain.GitHubInstallation]{}, base.ErrForbidden
	}
	if in.Visibility == "team" {
		if _, exists = s.teams[in.TeamID]; !exists {
			return base.Result[domain.GitHubInstallation]{}, base.ErrNotFound
		}
		if !admin && !s.canManageTeamLocked(actor, in.TeamID) {
			return base.Result[domain.GitHubInstallation]{}, base.ErrForbidden
		}
	}
	if installation.Visibility == in.Visibility && installation.TeamID == in.TeamID {
		s.idempotency[idemKey] = idemRecord{fp, "github-installation", installation.ID, ""}
		return base.Result[domain.GitHubInstallation]{Value: installation}, nil
	}
	affected := map[string]struct{}{installation.OwnerUserID: {}}
	if installation.TeamID != "" {
		for userID := range s.memberships[installation.TeamID] {
			affected[userID] = struct{}{}
		}
	}
	if in.TeamID != "" {
		for userID := range s.memberships[in.TeamID] {
			affected[userID] = struct{}{}
		}
	}
	installation.Visibility = in.Visibility
	installation.TeamID = in.TeamID
	installation.UpdatedAt = time.Now().UTC()
	s.installations[installation.ID] = installation
	s.idempotency[idemKey] = idemRecord{fp, "github-installation", installation.ID, ""}
	s.audits++
	s.bumpGrantsLocked(affected)
	return base.Result[domain.GitHubInstallation]{Value: installation}, nil
}

func (s *Store) ListProjectsForActor(_ context.Context, actor string) ([]domain.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.Project
	for _, project := range s.projects {
		if s.canAccessProjectLocked(actor, project) {
			out = append(out, project)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (s *Store) GetProjectForActor(_ context.Context, actor, projectID string) (domain.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	project, exists := s.projects[projectID]
	if !exists || !s.canAccessProjectLocked(actor, project) {
		return domain.Project{}, base.ErrNotFound
	}
	return project, nil
}

func (s *Store) ListEnvironmentsForActor(_ context.Context, actor string) ([]domain.Environment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.Environment
	for _, environment := range s.environments {
		target, ok := s.resolveTargetLocked(domain.AccessTarget{Type: "environment", ID: environment.ID})
		if ok && accesspolicy.HasPermission(s.bindingsLocked(actor), target, domain.PermissionResourcesRead) {
			out = append(out, environment)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (s *Store) GetEnvironmentForActor(_ context.Context, actor, environmentID string) (domain.Environment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	environment, exists := s.environments[environmentID]
	target, allowedTarget := s.resolveTargetLocked(domain.AccessTarget{Type: "environment", ID: environmentID})
	if !exists || !allowedTarget || !accesspolicy.HasPermission(s.bindingsLocked(actor), target, domain.PermissionResourcesRead) {
		return domain.Environment{}, base.ErrNotFound
	}
	return environment, nil
}

func (s *Store) ListEnvironmentAppPlacementsForActor(_ context.Context, actor, environmentID string) ([]domain.EnvironmentAppPlacement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	environment, exists := s.environments[environmentID]
	target, allowedTarget := s.resolveTargetLocked(domain.AccessTarget{Type: "environment", ID: environmentID})
	if !exists || !allowedTarget || !accesspolicy.HasPermission(s.bindingsLocked(actor), target, domain.PermissionResourcesRead) {
		return nil, base.ErrNotFound
	}
	byApplication := make(map[string]domain.EnvironmentAppPlacement)
	for applicationID, placement := range s.environmentAppPlacements[environmentID] {
		byApplication[applicationID] = placement
	}
	for _, deployment := range s.deployments {
		if deployment.EnvironmentID != environmentID {
			continue
		}
		if _, exists = byApplication[deployment.ApplicationID]; exists {
			continue
		}
		application, ok := s.applications[deployment.ApplicationID]
		if !ok || application.ProjectID != environment.ProjectID {
			continue
		}
		byApplication[application.ID] = domain.EnvironmentAppPlacement{
			ProjectID: environment.ProjectID, EnvironmentID: environmentID, ApplicationID: application.ID,
			ApplicationName: application.Name, ApplicationSlug: application.Slug,
			State: domain.EnvironmentAppPlacementActive, DesiredState: domain.EnvironmentAppPlacementRunning,
			CreatedAt: deployment.CreatedAt, UpdatedAt: deployment.UpdatedAt,
		}
	}
	items := make([]domain.EnvironmentAppPlacement, 0, len(byApplication))
	for _, placement := range byApplication {
		items = append(items, placement)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].ApplicationSlug == items[j].ApplicationSlug {
			return items[i].ApplicationID < items[j].ApplicationID
		}
		return items[i].ApplicationSlug < items[j].ApplicationSlug
	})
	return items, nil
}

func (s *Store) ListApplicationsForActor(_ context.Context, actor string) ([]domain.Application, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.Application
	for _, application := range s.applications {
		target, ok := s.resolveTargetLocked(domain.AccessTarget{Type: "application", ID: application.ID})
		if ok && accesspolicy.HasPermission(s.bindingsLocked(actor), target, domain.PermissionResourcesRead) {
			out = append(out, application)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (s *Store) GetApplicationForActor(_ context.Context, actor, applicationID string) (domain.Application, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	application, exists := s.applications[applicationID]
	target, allowedTarget := s.resolveTargetLocked(domain.AccessTarget{Type: "application", ID: applicationID})
	if !exists || !allowedTarget || !accesspolicy.HasPermission(s.bindingsLocked(actor), target, domain.PermissionResourcesRead) {
		return domain.Application{}, base.ErrNotFound
	}
	return application, nil
}

func (s *Store) canAccessDeploymentLocked(actor string, deployment domain.Deployment) bool {
	target, ok := s.resolveTargetLocked(domain.AccessTarget{Type: "deployment", ID: deployment.ID})
	return ok && accesspolicy.HasPermission(s.bindingsLocked(actor), target, domain.PermissionResourcesRead)
}

func (s *Store) ListDeploymentsForActor(_ context.Context, actor string) ([]domain.Deployment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.Deployment
	for _, deployment := range s.deployments {
		if s.canAccessDeploymentLocked(actor, deployment) {
			deployment.Environment = clone(deployment.Environment)
			deployment.Route = cloneRoute(deployment.Route)
			deployment.Runtime = cloneRuntime(deployment.Runtime)
			deployment.ConfigRaw = append([]byte(nil), deployment.ConfigRaw...)
			out = append(out, deployment)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (s *Store) GetDeploymentForActor(_ context.Context, actor, deploymentID string) (domain.Deployment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	deployment, exists := s.deployments[deploymentID]
	if !exists || !s.canAccessDeploymentLocked(actor, deployment) {
		return domain.Deployment{}, base.ErrNotFound
	}
	deployment.Environment = clone(deployment.Environment)
	deployment.Route = cloneRoute(deployment.Route)
	deployment.Runtime = cloneRuntime(deployment.Runtime)
	deployment.ConfigRaw = append([]byte(nil), deployment.ConfigRaw...)
	return deployment, nil
}

func (s *Store) DeploymentStatusForActor(_ context.Context, actor, deploymentID string) (domain.DeploymentStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	deployment, exists := s.deployments[deploymentID]
	if !exists || !s.canAccessDeploymentLocked(actor, deployment) {
		return domain.DeploymentStatus{}, base.ErrNotFound
	}
	op := s.operations[deployment.OperationID]
	return s.deploymentStatusLocked(deployment, op, time.Now().UTC()), nil
}

func (s *Store) canAccessOperationLocked(actor string, operation domain.Operation) bool {
	target, ok := s.resolveTargetLocked(domain.AccessTarget{Type: "operation", ID: operation.ID})
	return ok && accesspolicy.HasPermission(s.bindingsLocked(actor), target, domain.PermissionOperationsRead)
}

func (s *Store) AuditRuntimeAccess(_ context.Context, actor, deploymentID, action, _ string) error {
	if action != "runtime.logs.snapshot" && action != "runtime.logs.follow" && action != "runtime.events.snapshot" {
		return base.ErrConflict
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorizeLocked(actor, domain.PermissionLogsRead, domain.AccessTarget{Type: "deployment", ID: deploymentID}); err != nil {
		return err
	}
	s.audits++
	return nil
}

func (s *Store) ListOperationsForActor(_ context.Context, actor string) ([]domain.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.Operation
	for _, operation := range s.operations {
		if s.canAccessOperationLocked(actor, operation) {
			out = append(out, s.operationWithPublicationLocked(operation))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (s *Store) GetOperationForActor(_ context.Context, actor, operationID string) (domain.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	operation, exists := s.operations[operationID]
	if !exists || !s.canAccessOperationLocked(actor, operation) {
		return domain.Operation{}, base.ErrNotFound
	}
	return s.operationWithPublicationLocked(operation), nil
}

func (s *Store) scopeTargetForProjectLocked(project domain.Project, scopeType domain.AccessScopeType, scopeID string) (domain.AccessTarget, bool) {
	var target domain.AccessTarget
	switch scopeType {
	case domain.ScopeTeam:
		if project.TeamID == "" || scopeID != project.TeamID {
			return target, false
		}
		target, _ = s.resolveTargetLocked(domain.AccessTarget{Type: "team", ID: scopeID})
	case domain.ScopeProject:
		if scopeID != project.ID {
			return target, false
		}
		target, _ = s.resolveTargetLocked(domain.AccessTarget{Type: "project", ID: scopeID})
	case domain.ScopeEnvironment:
		target, _ = s.resolveTargetLocked(domain.AccessTarget{Type: "environment", ID: scopeID})
	case domain.ScopeNamespace:
		target, _ = s.resolveTargetLocked(domain.AccessTarget{Type: "namespace", ID: scopeID})
	case domain.ScopeApplication:
		target, _ = s.resolveTargetLocked(domain.AccessTarget{Type: "application", ID: scopeID})
	default:
		return target, false
	}
	return target, target.ProjectID == project.ID || scopeType == domain.ScopeTeam && target.TeamID == project.TeamID
}

func (s *Store) ListProjectAccessGrants(_ context.Context, actor, projectID string) ([]domain.AccessGrant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	project, exists := s.projects[projectID]
	if !exists {
		return nil, base.ErrNotFound
	}
	if err := s.authorizeLocked(actor, domain.PermissionGrantsRead, domain.AccessTarget{Type: "project", ID: projectID}); err != nil {
		return nil, err
	}
	out := make([]domain.AccessGrant, 0)
	for _, grant := range s.accessGrants {
		if _, ok := s.scopeTargetForProjectLocked(project, grant.ScopeType, grant.ScopeID); ok {
			grant.Permissions = append([]domain.Permission{}, grant.Permissions...)
			out = append(out, grant)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (s *Store) CreateProjectAccessGrant(_ context.Context, actor, key, fp, requestID string, in domain.CreateAccessGrant) (base.Result[domain.AccessGrant], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	project, exists := s.projects[in.ProjectID]
	if !exists {
		return base.Result[domain.AccessGrant]{}, base.ErrNotFound
	}
	if (in.SubjectUserID == "") == (in.SubjectTeamID == "") || !accesspolicy.ValidRole(in.Role) || !accesspolicy.ValidScope(in.ScopeType) || !accesspolicy.ValidExtraPermissions(in.Permissions) || in.Role == domain.RolePlatformAdmin || in.ScopeType == domain.ScopePlatform || in.Role == domain.RoleOrganizationAdmin && in.ScopeType != domain.ScopeTeam || in.Role == domain.RoleProjectAdmin && in.ScopeType != domain.ScopeProject {
		return base.Result[domain.AccessGrant]{}, base.ErrConflict
	}
	target, ok := s.scopeTargetForProjectLocked(project, in.ScopeType, in.ScopeID)
	if !ok {
		return base.Result[domain.AccessGrant]{}, base.ErrNotFound
	}
	bindings := s.bindingsLocked(actor)
	if !accesspolicy.HasPermission(bindings, domain.AccessTarget{Type: "project", ID: project.ID, TeamID: project.TeamID, ProjectID: project.ID}, domain.PermissionResourcesRead) {
		return base.Result[domain.AccessGrant]{}, base.ErrNotFound
	}
	if !accesspolicy.CanManageGrant(bindings, target, in.Role) {
		return base.Result[domain.AccessGrant]{}, base.ErrForbidden
	}
	if in.SubjectUserID != "" {
		_, exists = s.users[in.SubjectUserID]
	} else {
		_, exists = s.teams[in.SubjectTeamID]
	}
	if !exists {
		return base.Result[domain.AccessGrant]{}, base.ErrNotFound
	}
	idemScope := "projects.grants.create:" + in.ProjectID
	idemKey := ik(actor, idemScope, key)
	old, replay := s.idempotency[idemKey]
	if err := check(old, replay, fp); err != nil {
		return base.Result[domain.AccessGrant]{}, err
	}
	if replay {
		grant, ok := s.accessGrants[old.resourceID]
		if !ok {
			return base.Result[domain.AccessGrant]{}, base.ErrConflict
		}
		return base.Result[domain.AccessGrant]{Value: grant, Replay: true}, nil
	}
	for _, grant := range s.accessGrants {
		if grant.SubjectUserID == in.SubjectUserID && grant.SubjectTeamID == in.SubjectTeamID && grant.Role == in.Role && grant.ScopeType == in.ScopeType && grant.ScopeID == in.ScopeID {
			return base.Result[domain.AccessGrant]{}, base.ErrConflict
		}
	}
	now := time.Now().UTC()
	grant := domain.AccessGrant{ID: id.New(), SubjectUserID: in.SubjectUserID, SubjectTeamID: in.SubjectTeamID, Role: in.Role, ScopeType: in.ScopeType, ScopeID: in.ScopeID, Permissions: append([]domain.Permission{}, in.Permissions...), Source: "explicit", CreatedBy: actor, CreatedAt: now}
	s.accessGrants[grant.ID] = grant
	s.idempotency[idemKey] = idemRecord{fp, "access-grant", grant.ID, ""}
	s.audits++
	s.bumpGrantSubjectsLocked(grant)
	_ = requestID
	return base.Result[domain.AccessGrant]{Value: grant}, nil
}

func (s *Store) DeleteProjectAccessGrant(_ context.Context, actor, projectID, grantID, key, fp, requestID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	project, exists := s.projects[projectID]
	if !exists {
		return false, base.ErrNotFound
	}
	projectTarget := domain.AccessTarget{Type: "project", ID: project.ID, TeamID: project.TeamID, ProjectID: project.ID}
	bindings := s.bindingsLocked(actor)
	if !accesspolicy.HasPermission(bindings, projectTarget, domain.PermissionResourcesRead) {
		return false, base.ErrNotFound
	}
	idemScope := "projects.grants.delete:" + projectID + ":" + grantID
	idemKey := ik(actor, idemScope, key)
	old, replay := s.idempotency[idemKey]
	if err := check(old, replay, fp); err != nil {
		return false, err
	}
	if replay {
		return true, nil
	}
	grant, exists := s.accessGrants[grantID]
	if !exists {
		return false, base.ErrNotFound
	}
	target, ok := s.scopeTargetForProjectLocked(project, grant.ScopeType, grant.ScopeID)
	if !ok {
		return false, base.ErrNotFound
	}
	if grant.Source != "explicit" || !accesspolicy.CanManageGrant(bindings, target, grant.Role) {
		return false, base.ErrForbidden
	}
	delete(s.accessGrants, grantID)
	s.idempotency[idemKey] = idemRecord{fp, "access-grant", grantID, ""}
	s.audits++
	s.bumpGrantSubjectsLocked(grant)
	_ = requestID
	return false, nil
}

func (s *Store) bumpGrantSubjectsLocked(grant domain.AccessGrant) {
	users := map[string]struct{}{}
	if grant.SubjectUserID != "" {
		users[grant.SubjectUserID] = struct{}{}
	} else {
		for userID := range s.memberships[grant.SubjectTeamID] {
			users[userID] = struct{}{}
		}
	}
	s.bumpGrantsLocked(users)
}
