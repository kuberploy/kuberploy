package memory

import (
	"context"
	"encoding/hex"
	"sort"
	"strings"
	"time"

	accesspolicy "github.com/kuberploy/kuberploy/internal/access"
	"github.com/kuberploy/kuberploy/internal/automation"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/id"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func (s *Store) CreateServiceAccount(_ context.Context, actor, key, fp, requestID string, in domain.CreateServiceAccount) (base.Result[domain.ServiceAccount], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !automation.ValidName(in.Name) {
		return base.Result[domain.ServiceAccount]{}, base.ErrConflict
	}
	project, ok := s.projects[in.ProjectID]
	if !ok {
		return base.Result[domain.ServiceAccount]{}, base.ErrNotFound
	}
	target := domain.AccessTarget{Type: "project", ID: project.ID, ProjectID: project.ID, TeamID: project.TeamID}
	bindings := s.bindingsLocked(actor)
	if !accesspolicy.HasPermission(bindings, target, domain.PermissionResourcesRead) {
		return base.Result[domain.ServiceAccount]{}, base.ErrNotFound
	}
	if !automation.ValidRole(in.Role) || !accesspolicy.CanManageGrant(bindings, target, in.Role) {
		return base.Result[domain.ServiceAccount]{}, base.ErrForbidden
	}
	idemScope := "projects.service-accounts.create:" + project.ID
	idemKey := ik(actor, idemScope, key)
	old, replay := s.idempotency[idemKey]
	if err := check(old, replay, fp); err != nil {
		return base.Result[domain.ServiceAccount]{}, err
	}
	if replay {
		account, exists := s.serviceAccounts[old.resourceID]
		if !exists {
			return base.Result[domain.ServiceAccount]{}, base.ErrConflict
		}
		return base.Result[domain.ServiceAccount]{Value: account, Replay: true}, nil
	}
	for _, account := range s.serviceAccounts {
		if account.ProjectID == project.ID && strings.EqualFold(account.Name, in.Name) {
			return base.Result[domain.ServiceAccount]{}, base.ErrConflict
		}
	}
	now := time.Now().UTC()
	accountID := id.New()
	user := domain.User{ID: accountID, Login: in.Name, Role: "developer", Issuer: "kuberploy:service-account", Subject: accountID, GrantRevision: 1, CreatedAt: now}
	account := domain.ServiceAccount{ID: accountID, ProjectID: project.ID, Name: in.Name, Role: in.Role, CreatedBy: actor, CreatedAt: now}
	grant := domain.AccessGrant{ID: id.New(), SubjectUserID: accountID, Role: in.Role, ScopeType: domain.ScopeProject, ScopeID: project.ID, Source: "service-account", CreatedBy: actor, CreatedAt: now}
	s.users[user.ID] = user
	s.serviceAccounts[account.ID] = account
	s.accessGrants[grant.ID] = grant
	s.idempotency[idemKey] = idemRecord{fingerprint: fp, typ: "service-account", resourceID: account.ID}
	s.audits++
	_ = requestID
	return base.Result[domain.ServiceAccount]{Value: account}, nil
}

func (s *Store) ListServiceAccounts(_ context.Context, actor, projectID string) ([]domain.ServiceAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	project, ok := s.projects[projectID]
	if !ok {
		return nil, base.ErrNotFound
	}
	target := domain.AccessTarget{Type: "project", ID: project.ID, ProjectID: project.ID, TeamID: project.TeamID}
	bindings := s.bindingsLocked(actor)
	if !accesspolicy.HasPermission(bindings, target, domain.PermissionResourcesRead) {
		return nil, base.ErrNotFound
	}
	if !accesspolicy.HasPermission(bindings, target, domain.PermissionGrantsRead) {
		return nil, base.ErrForbidden
	}
	result := make([]domain.ServiceAccount, 0)
	for _, account := range s.serviceAccounts {
		if account.ProjectID == project.ID {
			result = append(result, account)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if !result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].CreatedAt.Before(result[j].CreatedAt)
		}
		return result[i].ID < result[j].ID
	})
	return result, nil
}

func (s *Store) CreateServiceAccountToken(_ context.Context, actor, key, fp, requestID string, in domain.CreateServiceAccountToken) (base.Result[domain.ServiceAccountToken], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !automation.ValidName(in.Name) {
		return base.Result[domain.ServiceAccountToken]{}, base.ErrConflict
	}
	account, project, bindings, err := s.manageableServiceAccountLocked(actor, in.ServiceAccountID)
	if err != nil {
		return base.Result[domain.ServiceAccountToken]{}, err
	}
	idemScope := "service-accounts.tokens.create:" + account.ID
	idemKey := ik(actor, idemScope, key)
	old, replay := s.idempotency[idemKey]
	if err = check(old, replay, fp); err != nil {
		return base.Result[domain.ServiceAccountToken]{}, err
	}
	if replay {
		token, exists := s.serviceAccountTokens[old.resourceID]
		if !exists {
			return base.Result[domain.ServiceAccountToken]{}, base.ErrConflict
		}
		token.Scopes = append([]domain.AutomationScope(nil), token.Scopes...)
		return base.Result[domain.ServiceAccountToken]{Value: token, Replay: true}, nil
	}
	if account.DisabledAt != nil || len(in.TokenHash) != 32 || !automation.ValidTokenPrefix(in.Prefix) {
		return base.Result[domain.ServiceAccountToken]{}, base.ErrConflict
	}
	now := time.Now().UTC()
	if !automation.ValidExpiry(now, in.ExpiresAt) {
		return base.Result[domain.ServiceAccountToken]{}, base.ErrConflict
	}
	scopes, valid := automation.NormalizeScopes(in.Scopes)
	if !valid {
		return base.Result[domain.ServiceAccountToken]{}, base.ErrConflict
	}
	hashKey := hex.EncodeToString(in.TokenHash)
	if _, duplicate := s.serviceAccountTokenHashes[hashKey]; duplicate {
		return base.Result[domain.ServiceAccountToken]{}, base.ErrConflict
	}
	token := domain.ServiceAccountToken{ID: id.New(), ServiceAccountID: account.ID, Name: in.Name, Prefix: in.Prefix, Scopes: scopes, ExpiresAt: in.ExpiresAt.UTC(), CreatedBy: actor, CreatedAt: now}
	s.serviceAccountTokens[token.ID] = token
	s.serviceAccountTokenHashes[hashKey] = token.ID
	s.idempotency[idemKey] = idemRecord{fingerprint: fp, typ: "service-account-token", resourceID: token.ID}
	s.audits++
	_ = project
	_ = bindings
	_ = requestID
	result := token
	result.Scopes = append([]domain.AutomationScope(nil), token.Scopes...)
	return base.Result[domain.ServiceAccountToken]{Value: result}, nil
}

func (s *Store) ServiceAccountTokenReplay(_ context.Context, actor, serviceAccountID, key, fp string) (base.Result[domain.ServiceAccountToken], bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	account, _, _, err := s.manageableServiceAccountLocked(actor, serviceAccountID)
	if err != nil {
		return base.Result[domain.ServiceAccountToken]{}, false, err
	}
	old, replay := s.idempotency[ik(actor, "service-accounts.tokens.create:"+account.ID, key)]
	if !replay {
		return base.Result[domain.ServiceAccountToken]{}, false, nil
	}
	if err = check(old, true, fp); err != nil {
		return base.Result[domain.ServiceAccountToken]{}, false, err
	}
	token, exists := s.serviceAccountTokens[old.resourceID]
	if !exists {
		return base.Result[domain.ServiceAccountToken]{}, false, base.ErrConflict
	}
	token.Scopes = append([]domain.AutomationScope(nil), token.Scopes...)
	return base.Result[domain.ServiceAccountToken]{Value: token, Replay: true}, true, nil
}

func (s *Store) ListServiceAccountTokens(_ context.Context, actor, serviceAccountID string) ([]domain.ServiceAccountToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	account, ok := s.serviceAccounts[serviceAccountID]
	if !ok {
		return nil, base.ErrNotFound
	}
	project, ok := s.projects[account.ProjectID]
	if !ok {
		return nil, base.ErrNotFound
	}
	target := domain.AccessTarget{Type: "project", ID: project.ID, ProjectID: project.ID, TeamID: project.TeamID}
	bindings := s.bindingsLocked(actor)
	if !accesspolicy.HasPermission(bindings, target, domain.PermissionResourcesRead) {
		return nil, base.ErrNotFound
	}
	if !accesspolicy.HasPermission(bindings, target, domain.PermissionGrantsRead) {
		return nil, base.ErrForbidden
	}
	result := make([]domain.ServiceAccountToken, 0)
	for _, token := range s.serviceAccountTokens {
		if token.ServiceAccountID != account.ID {
			continue
		}
		copyToken := token
		copyToken.Scopes = append([]domain.AutomationScope(nil), token.Scopes...)
		result = append(result, copyToken)
	}
	sort.Slice(result, func(i, j int) bool {
		if !result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].CreatedAt.Before(result[j].CreatedAt)
		}
		return result[i].ID < result[j].ID
	})
	return result, nil
}

func (s *Store) RevokeServiceAccountToken(_ context.Context, actor, serviceAccountID, tokenID, key, fp, requestID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	account, _, _, err := s.manageableServiceAccountLocked(actor, serviceAccountID)
	if err != nil {
		return false, err
	}
	idemScope := "service-accounts.tokens.revoke:" + account.ID + ":" + tokenID
	idemKey := ik(actor, idemScope, key)
	old, replay := s.idempotency[idemKey]
	if err = check(old, replay, fp); err != nil {
		return false, err
	}
	if replay {
		return true, nil
	}
	token, ok := s.serviceAccountTokens[tokenID]
	if !ok || token.ServiceAccountID != account.ID {
		return false, base.ErrNotFound
	}
	if token.RevokedAt == nil {
		now := time.Now().UTC()
		token.RevokedAt = &now
		s.serviceAccountTokens[token.ID] = token
		s.audits++
	}
	s.idempotency[idemKey] = idemRecord{fingerprint: fp, typ: "service-account-token", resourceID: token.ID}
	_ = requestID
	return false, nil
}

func (s *Store) DisableServiceAccount(_ context.Context, actor, serviceAccountID, key, fp, requestID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	account, _, _, err := s.manageableServiceAccountLocked(actor, serviceAccountID)
	if err != nil {
		return false, err
	}
	idemScope := "service-accounts.disable:" + account.ID
	idemKey := ik(actor, idemScope, key)
	old, replay := s.idempotency[idemKey]
	if err = check(old, replay, fp); err != nil {
		return false, err
	}
	if replay {
		return true, nil
	}
	if account.DisabledAt == nil {
		now := time.Now().UTC()
		account.DisabledAt = &now
		s.serviceAccounts[account.ID] = account
		for tokenID, token := range s.serviceAccountTokens {
			if token.ServiceAccountID == account.ID && token.RevokedAt == nil {
				token.RevokedAt = &now
				s.serviceAccountTokens[tokenID] = token
			}
		}
		s.audits++
	}
	s.idempotency[idemKey] = idemRecord{fingerprint: fp, typ: "service-account", resourceID: account.ID}
	_ = requestID
	return false, nil
}

func (s *Store) ServiceAccountByToken(_ context.Context, tokenHash []byte, now time.Time) (domain.AutomationPrincipal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(tokenHash) != 32 {
		return domain.AutomationPrincipal{}, base.ErrNotFound
	}
	tokenID, ok := s.serviceAccountTokenHashes[hex.EncodeToString(tokenHash)]
	if !ok {
		return domain.AutomationPrincipal{}, base.ErrNotFound
	}
	token, ok := s.serviceAccountTokens[tokenID]
	if !ok || token.RevokedAt != nil || !token.ExpiresAt.After(now) {
		return domain.AutomationPrincipal{}, base.ErrNotFound
	}
	account, ok := s.serviceAccounts[token.ServiceAccountID]
	if !ok || account.DisabledAt != nil {
		return domain.AutomationPrincipal{}, base.ErrNotFound
	}
	user, ok := s.users[account.ID]
	if !ok || user.Issuer != "kuberploy:service-account" || user.Subject != account.ID {
		return domain.AutomationPrincipal{}, base.ErrNotFound
	}
	if token.LastUsedAt == nil || token.LastUsedAt.Before(now.Add(-15*time.Minute)) {
		used := now.UTC()
		token.LastUsedAt = &used
		s.serviceAccountTokens[token.ID] = token
	}
	return domain.AutomationPrincipal{User: user, ServiceAccountID: account.ID, TokenID: token.ID, Scopes: append([]domain.AutomationScope(nil), token.Scopes...), ExpiresAt: token.ExpiresAt}, nil
}

func (s *Store) manageableServiceAccountLocked(actor, accountID string) (domain.ServiceAccount, domain.Project, []domain.AccessBinding, error) {
	account, ok := s.serviceAccounts[accountID]
	if !ok {
		return domain.ServiceAccount{}, domain.Project{}, nil, base.ErrNotFound
	}
	project, ok := s.projects[account.ProjectID]
	if !ok {
		return domain.ServiceAccount{}, domain.Project{}, nil, base.ErrNotFound
	}
	target := domain.AccessTarget{Type: "project", ID: project.ID, ProjectID: project.ID, TeamID: project.TeamID}
	bindings := s.bindingsLocked(actor)
	if !accesspolicy.HasPermission(bindings, target, domain.PermissionResourcesRead) {
		return domain.ServiceAccount{}, domain.Project{}, nil, base.ErrNotFound
	}
	if !accesspolicy.CanManageGrant(bindings, target, account.Role) {
		return domain.ServiceAccount{}, domain.Project{}, nil, base.ErrForbidden
	}
	return account, project, bindings, nil
}
