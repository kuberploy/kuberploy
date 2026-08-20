package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	accesspolicy "github.com/kuberploy/kuberploy/internal/access"
	"github.com/kuberploy/kuberploy/internal/automation"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/id"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func (s *Store) CreateServiceAccount(ctx context.Context, actor, key, fingerprint, requestID string, in domain.CreateServiceAccount) (base.Result[domain.ServiceAccount], error) {
	if !automation.ValidName(in.Name) || !automation.ValidRole(in.Role) {
		return base.Result[domain.ServiceAccount]{}, base.ErrForbidden
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return base.Result[domain.ServiceAccount]{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	idemScope := "projects.service-accounts.create:" + in.ProjectID
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, advisoryIdentity(actor, idemScope, key)); err != nil {
		return base.Result[domain.ServiceAccount]{}, err
	}
	project, err := getProject(ctx, tx, in.ProjectID)
	if err != nil {
		return base.Result[domain.ServiceAccount]{}, err
	}
	target, err := resolveAccessTarget(ctx, tx, domain.AccessTarget{Type: "project", ID: project.ID})
	if err != nil {
		return base.Result[domain.ServiceAccount]{}, err
	}
	bindings, err := effectiveBindings(ctx, tx, actor)
	if err != nil {
		return base.Result[domain.ServiceAccount]{}, err
	}
	if !accesspolicy.HasPermission(bindings, target, domain.PermissionResourcesRead) {
		return base.Result[domain.ServiceAccount]{}, base.ErrNotFound
	}
	if !accesspolicy.CanManageGrant(bindings, target, in.Role) {
		return base.Result[domain.ServiceAccount]{}, base.ErrForbidden
	}
	if old, ok, findErr := findIdem(ctx, tx, actor, idemScope, key); findErr != nil {
		return base.Result[domain.ServiceAccount]{}, findErr
	} else if ok {
		if old.fingerprint != fingerprint {
			return base.Result[domain.ServiceAccount]{}, base.ErrIdempotencyConflict
		}
		account, getErr := getServiceAccount(ctx, tx, old.resourceID, false)
		return base.Result[domain.ServiceAccount]{Value: account, Replay: true}, getErr
	}

	now := time.Now().UTC()
	accountID := id.New()
	account := domain.ServiceAccount{ID: accountID, ProjectID: project.ID, Name: in.Name, Role: in.Role, CreatedBy: actor, CreatedAt: now}
	if _, err = tx.Exec(ctx, `INSERT INTO users(id,email,display_name,role,issuer,subject,grant_revision,created_at) VALUES($1,NULL,$2,'developer','kuberploy:service-account',$3,1,$4)`, account.ID, account.Name, account.ID, account.CreatedAt); err != nil {
		return base.Result[domain.ServiceAccount]{}, classify(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO service_accounts(id,project_id,name,role,created_by,created_at) VALUES($1,$2,$3,$4,$5,$6)`, account.ID, account.ProjectID, account.Name, account.Role, account.CreatedBy, account.CreatedAt); err != nil {
		return base.Result[domain.ServiceAccount]{}, classify(err)
	}
	grantID := id.New()
	if _, err = tx.Exec(ctx, `INSERT INTO access_grants(id,subject_user_id,role,scope_type,scope_id,permissions,source,created_by,created_at) VALUES($1,$2,$3,'project',$4,ARRAY[]::text[],'service-account',$5,$6)`, grantID, account.ID, account.Role, account.ProjectID, actor, now); err != nil {
		return base.Result[domain.ServiceAccount]{}, classify(err)
	}
	if err = audit(ctx, tx, actor, "service-account.create", "service-account", account.ID, requestID, map[string]any{"projectId": account.ProjectID, "name": account.Name, "role": account.Role}); err != nil {
		return base.Result[domain.ServiceAccount]{}, err
	}
	if err = putIdem(ctx, tx, actor, idemScope, key, fingerprint, "service-account", account.ID, nil); err != nil {
		return base.Result[domain.ServiceAccount]{}, classify(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return base.Result[domain.ServiceAccount]{}, err
	}
	return base.Result[domain.ServiceAccount]{Value: account}, nil
}

func (s *Store) ListServiceAccounts(ctx context.Context, actor, projectID string) ([]domain.ServiceAccount, error) {
	if err := authorizeWith(ctx, s.pool, actor, domain.PermissionGrantsRead, domain.AccessTarget{Type: "project", ID: projectID}); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id,project_id,name,role,created_by,created_at,disabled_at FROM service_accounts WHERE project_id=$1 ORDER BY created_at,id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.ServiceAccount, 0)
	for rows.Next() {
		account, scanErr := scanServiceAccount(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, account)
	}
	return result, rows.Err()
}

func (s *Store) CreateServiceAccountToken(ctx context.Context, actor, key, fingerprint, requestID string, in domain.CreateServiceAccountToken) (base.Result[domain.ServiceAccountToken], error) {
	if !automation.ValidName(in.Name) {
		return base.Result[domain.ServiceAccountToken]{}, base.ErrConflict
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return base.Result[domain.ServiceAccountToken]{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	idemScope := "service-accounts.tokens.create:" + in.ServiceAccountID
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, advisoryIdentity(actor, idemScope, key)); err != nil {
		return base.Result[domain.ServiceAccountToken]{}, err
	}
	account, _, _, err := manageableServiceAccount(ctx, tx, actor, in.ServiceAccountID)
	if err != nil {
		return base.Result[domain.ServiceAccountToken]{}, err
	}
	if old, ok, findErr := findIdem(ctx, tx, actor, idemScope, key); findErr != nil {
		return base.Result[domain.ServiceAccountToken]{}, findErr
	} else if ok {
		if old.fingerprint != fingerprint {
			return base.Result[domain.ServiceAccountToken]{}, base.ErrIdempotencyConflict
		}
		token, getErr := getServiceAccountToken(ctx, tx, old.resourceID, false)
		return base.Result[domain.ServiceAccountToken]{Value: token, Replay: true}, getErr
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
	scopeStrings := make([]string, len(scopes))
	for index, scope := range scopes {
		scopeStrings[index] = string(scope)
	}
	token := domain.ServiceAccountToken{ID: id.New(), ServiceAccountID: account.ID, Name: in.Name, Prefix: in.Prefix, Scopes: scopes, ExpiresAt: in.ExpiresAt.UTC(), CreatedBy: actor, CreatedAt: now}
	if _, err = tx.Exec(ctx, `INSERT INTO service_account_tokens(id,service_account_id,name,token_prefix,token_hash,scopes,expires_at,created_by,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, token.ID, token.ServiceAccountID, token.Name, token.Prefix, in.TokenHash, scopeStrings, token.ExpiresAt, token.CreatedBy, token.CreatedAt); err != nil {
		return base.Result[domain.ServiceAccountToken]{}, classify(err)
	}
	if err = audit(ctx, tx, actor, "service-account-token.create", "service-account-token", token.ID, requestID, map[string]any{"serviceAccountId": account.ID, "name": token.Name, "prefix": token.Prefix, "scopes": token.Scopes, "expiresAt": token.ExpiresAt}); err != nil {
		return base.Result[domain.ServiceAccountToken]{}, err
	}
	if err = putIdem(ctx, tx, actor, idemScope, key, fingerprint, "service-account-token", token.ID, nil); err != nil {
		return base.Result[domain.ServiceAccountToken]{}, classify(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return base.Result[domain.ServiceAccountToken]{}, err
	}
	return base.Result[domain.ServiceAccountToken]{Value: token}, nil
}

func (s *Store) ServiceAccountTokenReplay(ctx context.Context, actor, serviceAccountID, key, fingerprint string) (base.Result[domain.ServiceAccountToken], bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return base.Result[domain.ServiceAccountToken]{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	account, _, _, err := manageableServiceAccount(ctx, tx, actor, serviceAccountID)
	if err != nil {
		return base.Result[domain.ServiceAccountToken]{}, false, err
	}
	old, found, err := findIdem(ctx, tx, actor, "service-accounts.tokens.create:"+account.ID, key)
	if err != nil {
		return base.Result[domain.ServiceAccountToken]{}, false, err
	}
	if !found {
		return base.Result[domain.ServiceAccountToken]{}, false, nil
	}
	if old.fingerprint != fingerprint {
		return base.Result[domain.ServiceAccountToken]{}, false, base.ErrIdempotencyConflict
	}
	token, err := getServiceAccountToken(ctx, tx, old.resourceID, false)
	if err != nil {
		return base.Result[domain.ServiceAccountToken]{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return base.Result[domain.ServiceAccountToken]{}, false, err
	}
	return base.Result[domain.ServiceAccountToken]{Value: token, Replay: true}, true, nil
}

func (s *Store) ListServiceAccountTokens(ctx context.Context, actor, serviceAccountID string) ([]domain.ServiceAccountToken, error) {
	account, err := getServiceAccount(ctx, s.pool, serviceAccountID, false)
	if err != nil {
		return nil, err
	}
	if err = authorizeWith(ctx, s.pool, actor, domain.PermissionGrantsRead, domain.AccessTarget{Type: "project", ID: account.ProjectID}); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id,service_account_id,name,token_prefix,scopes,expires_at,last_used_at,revoked_at,created_by,created_at FROM service_account_tokens WHERE service_account_id=$1 ORDER BY created_at,id`, account.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.ServiceAccountToken, 0)
	for rows.Next() {
		token, scanErr := scanServiceAccountToken(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, token)
	}
	return result, rows.Err()
}

func (s *Store) RevokeServiceAccountToken(ctx context.Context, actor, serviceAccountID, tokenID, key, fingerprint, requestID string) (bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	idemScope := "service-accounts.tokens.revoke:" + serviceAccountID + ":" + tokenID
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, advisoryIdentity(actor, idemScope, key)); err != nil {
		return false, err
	}
	account, _, _, err := manageableServiceAccount(ctx, tx, actor, serviceAccountID)
	if err != nil {
		return false, err
	}
	if old, ok, findErr := findIdem(ctx, tx, actor, idemScope, key); findErr != nil {
		return false, findErr
	} else if ok {
		if old.fingerprint != fingerprint {
			return false, base.ErrIdempotencyConflict
		}
		return true, tx.Commit(ctx)
	}
	token, err := getServiceAccountToken(ctx, tx, tokenID, true)
	if err != nil || token.ServiceAccountID != account.ID {
		if err == nil {
			err = base.ErrNotFound
		}
		return false, err
	}
	if token.RevokedAt == nil {
		now := time.Now().UTC()
		if _, err = tx.Exec(ctx, `UPDATE service_account_tokens SET revoked_at=GREATEST($2,created_at) WHERE id=$1 AND revoked_at IS NULL`, token.ID, now); err != nil {
			return false, err
		}
		if err = audit(ctx, tx, actor, "service-account-token.revoke", "service-account-token", token.ID, requestID, map[string]any{"serviceAccountId": account.ID, "prefix": token.Prefix}); err != nil {
			return false, err
		}
	}
	if err = putIdem(ctx, tx, actor, idemScope, key, fingerprint, "service-account-token", token.ID, nil); err != nil {
		return false, classify(err)
	}
	return false, tx.Commit(ctx)
}

func (s *Store) DisableServiceAccount(ctx context.Context, actor, serviceAccountID, key, fingerprint, requestID string) (bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	idemScope := "service-accounts.disable:" + serviceAccountID
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, advisoryIdentity(actor, idemScope, key)); err != nil {
		return false, err
	}
	account, _, _, err := manageableServiceAccount(ctx, tx, actor, serviceAccountID)
	if err != nil {
		return false, err
	}
	if old, ok, findErr := findIdem(ctx, tx, actor, idemScope, key); findErr != nil {
		return false, findErr
	} else if ok {
		if old.fingerprint != fingerprint {
			return false, base.ErrIdempotencyConflict
		}
		return true, tx.Commit(ctx)
	}
	if account.DisabledAt == nil {
		now := time.Now().UTC()
		if _, err = tx.Exec(ctx, `UPDATE service_accounts SET disabled_at=$2 WHERE id=$1 AND disabled_at IS NULL`, account.ID, now); err != nil {
			return false, err
		}
		if _, err = tx.Exec(ctx, `UPDATE service_account_tokens SET revoked_at=GREATEST($2,created_at) WHERE service_account_id=$1 AND revoked_at IS NULL`, account.ID, now); err != nil {
			return false, err
		}
		if err = audit(ctx, tx, actor, "service-account.disable", "service-account", account.ID, requestID, map[string]any{"projectId": account.ProjectID, "name": account.Name}); err != nil {
			return false, err
		}
	}
	if err = putIdem(ctx, tx, actor, idemScope, key, fingerprint, "service-account", account.ID, nil); err != nil {
		return false, classify(err)
	}
	return false, tx.Commit(ctx)
}

func (s *Store) ServiceAccountByToken(ctx context.Context, tokenHash []byte, now time.Time) (domain.AutomationPrincipal, error) {
	if len(tokenHash) != 32 {
		return domain.AutomationPrincipal{}, base.ErrNotFound
	}
	var principal domain.AutomationPrincipal
	var scopes []string
	err := s.pool.QueryRow(ctx, `SELECT u.id,COALESCE(u.email,''),u.display_name,u.role,u.issuer,u.subject,u.grant_revision,u.created_at,sa.id,t.id,t.scopes,t.expires_at
		FROM service_account_tokens t
		JOIN service_accounts sa ON sa.id=t.service_account_id
		JOIN users u ON u.id=sa.id
		WHERE t.token_hash=$1 AND t.revoked_at IS NULL AND t.expires_at>$2 AND sa.disabled_at IS NULL
		  AND u.issuer='kuberploy:service-account' AND u.subject=u.id::text`, tokenHash, now).
		Scan(&principal.User.ID, &principal.User.Email, &principal.User.DisplayName, &principal.User.Role, &principal.User.Issuer, &principal.User.Subject, &principal.User.GrantRevision, &principal.User.CreatedAt, &principal.ServiceAccountID, &principal.TokenID, &scopes, &principal.ExpiresAt)
	if err != nil {
		return domain.AutomationPrincipal{}, classify(err)
	}
	principal.Scopes = make([]domain.AutomationScope, len(scopes))
	for index, scope := range scopes {
		principal.Scopes[index] = domain.AutomationScope(scope)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE service_account_tokens SET last_used_at=$2 WHERE id=$1 AND (last_used_at IS NULL OR last_used_at < $2::timestamptz - interval '15 minutes')`, principal.TokenID, now.UTC()); err != nil {
		return domain.AutomationPrincipal{}, err
	}
	return principal, nil
}

func manageableServiceAccount(ctx context.Context, tx pgx.Tx, actor, accountID string) (domain.ServiceAccount, domain.Project, []domain.AccessBinding, error) {
	account, err := getServiceAccount(ctx, tx, accountID, true)
	if err != nil {
		return domain.ServiceAccount{}, domain.Project{}, nil, err
	}
	project, err := getProject(ctx, tx, account.ProjectID)
	if err != nil {
		return domain.ServiceAccount{}, domain.Project{}, nil, err
	}
	target, err := resolveAccessTarget(ctx, tx, domain.AccessTarget{Type: "project", ID: project.ID})
	if err != nil {
		return domain.ServiceAccount{}, domain.Project{}, nil, err
	}
	bindings, err := effectiveBindings(ctx, tx, actor)
	if err != nil {
		return domain.ServiceAccount{}, domain.Project{}, nil, err
	}
	if !accesspolicy.HasPermission(bindings, target, domain.PermissionResourcesRead) {
		return domain.ServiceAccount{}, domain.Project{}, nil, base.ErrNotFound
	}
	if !accesspolicy.CanManageGrant(bindings, target, account.Role) {
		return domain.ServiceAccount{}, domain.Project{}, nil, base.ErrForbidden
	}
	return account, project, bindings, nil
}

func getServiceAccount(ctx context.Context, q rowQuerier, accountID string, lock bool) (domain.ServiceAccount, error) {
	query := `SELECT id,project_id,name,role,created_by,created_at,disabled_at FROM service_accounts WHERE id=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanServiceAccount(q.QueryRow(ctx, query, accountID))
}

type serviceAccountScanner interface{ Scan(...any) error }

func scanServiceAccount(row serviceAccountScanner) (domain.ServiceAccount, error) {
	var account domain.ServiceAccount
	err := row.Scan(&account.ID, &account.ProjectID, &account.Name, &account.Role, &account.CreatedBy, &account.CreatedAt, &account.DisabledAt)
	return account, classify(err)
}

func getServiceAccountToken(ctx context.Context, q rowQuerier, tokenID string, lock bool) (domain.ServiceAccountToken, error) {
	query := `SELECT id,service_account_id,name,token_prefix,scopes,expires_at,last_used_at,revoked_at,created_by,created_at FROM service_account_tokens WHERE id=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanServiceAccountToken(q.QueryRow(ctx, query, tokenID))
}

func scanServiceAccountToken(row serviceAccountScanner) (domain.ServiceAccountToken, error) {
	var token domain.ServiceAccountToken
	var scopes []string
	err := row.Scan(&token.ID, &token.ServiceAccountID, &token.Name, &token.Prefix, &scopes, &token.ExpiresAt, &token.LastUsedAt, &token.RevokedAt, &token.CreatedBy, &token.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return token, base.ErrNotFound
		}
		return token, err
	}
	token.Scopes = make([]domain.AutomationScope, len(scopes))
	for index, scope := range scopes {
		token.Scopes[index] = domain.AutomationScope(scope)
	}
	return token, nil
}
