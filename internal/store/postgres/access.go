package postgres

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	accesspolicy "github.com/kuberploy/kuberploy/internal/access"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/id"
	base "github.com/kuberploy/kuberploy/internal/store"
)

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

type accessQuerier interface {
	rowQuerier
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func actorIsAdmin(ctx context.Context, q rowQuerier, actor string) (bool, error) {
	var admin bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM access_grants WHERE subject_user_id=$1 AND role='platform-admin' AND scope_type='platform' AND scope_id='platform')`, actor).Scan(&admin)
	return admin, err
}

func effectiveBindings(ctx context.Context, q accessQuerier, actor string) ([]domain.AccessBinding, error) {
	rows, err := q.Query(ctx, `SELECT role,scope_type,scope_id,permissions,source FROM access_grants WHERE subject_user_id=$1
		UNION ALL
		SELECT g.role,g.scope_type,g.scope_id,g.permissions,g.source||'-team'
		FROM access_grants g JOIN team_memberships m ON m.team_id=g.subject_team_id
		WHERE m.user_id=$1
		UNION ALL
		SELECT CASE WHEN role='owner' THEN 'organization-admin' ELSE 'developer' END,'team',team_id::text,ARRAY[]::text[],'team-'||role
		FROM team_memberships WHERE user_id=$1`, actor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	bindings := make([]domain.AccessBinding, 0)
	for rows.Next() {
		var binding domain.AccessBinding
		var permissions []string
		if err = rows.Scan(&binding.Role, &binding.ScopeType, &binding.ScopeID, &permissions, &binding.Source); err != nil {
			return nil, err
		}
		for _, permission := range permissions {
			binding.Permissions = append(binding.Permissions, domain.Permission(permission))
		}
		bindings = append(bindings, binding)
	}
	return bindings, rows.Err()
}

func teamIDsWithPermission(bindings []domain.AccessBinding, permission domain.Permission) []string {
	seen := map[string]struct{}{}
	for _, binding := range bindings {
		if binding.ScopeType != domain.ScopeTeam || binding.ScopeID == "" {
			continue
		}
		target := domain.AccessTarget{Type: "team", ID: binding.ScopeID, TeamID: binding.ScopeID}
		if accesspolicy.HasPermission([]domain.AccessBinding{binding}, target, permission) {
			seen[binding.ScopeID] = struct{}{}
		}
	}
	ids := make([]string, 0, len(seen))
	for teamID := range seen {
		ids = append(ids, teamID)
	}
	return ids
}

func actorCanManageTeam(ctx context.Context, q accessQuerier, actor, teamID string) (bool, error) {
	bindings, err := effectiveBindings(ctx, q, actor)
	if err != nil {
		return false, err
	}
	target := domain.AccessTarget{Type: "team", ID: teamID, TeamID: teamID}
	return accesspolicy.HasPermission(bindings, target, domain.PermissionGrantsManage), nil
}

func resolveAccessTarget(ctx context.Context, q rowQuerier, target domain.AccessTarget) (domain.AccessTarget, error) {
	var resolved domain.AccessTarget
	resolved.Type, resolved.ID = target.Type, target.ID
	switch target.Type {
	case "platform":
		resolved.ID = "platform"
		return resolved, nil
	case "team":
		err := q.QueryRow(ctx, `SELECT id::text FROM teams WHERE id=$1`, target.ID).Scan(&resolved.TeamID)
		return resolved, classify(err)
	case "project":
		err := q.QueryRow(ctx, `SELECT id::text,COALESCE(team_id::text,'') FROM projects WHERE id=$1`, target.ID).Scan(&resolved.ProjectID, &resolved.TeamID)
		return resolved, classify(err)
	case "environment":
		err := q.QueryRow(ctx, `SELECT e.id::text,e.namespace,p.id::text,COALESCE(p.team_id::text,'') FROM environments e JOIN projects p ON p.id=e.project_id WHERE e.id=$1`, target.ID).Scan(&resolved.EnvironmentID, &resolved.Namespace, &resolved.ProjectID, &resolved.TeamID)
		return resolved, classify(err)
	case "namespace":
		err := q.QueryRow(ctx, `SELECT e.namespace,e.id::text,p.id::text,COALESCE(p.team_id::text,'') FROM environments e JOIN projects p ON p.id=e.project_id WHERE e.namespace=$1`, target.ID).Scan(&resolved.Namespace, &resolved.EnvironmentID, &resolved.ProjectID, &resolved.TeamID)
		return resolved, classify(err)
	case "application":
		err := q.QueryRow(ctx, `SELECT a.id::text,p.id::text,COALESCE(p.team_id::text,'') FROM applications a JOIN projects p ON p.id=a.project_id WHERE a.id=$1`, target.ID).Scan(&resolved.ApplicationID, &resolved.ProjectID, &resolved.TeamID)
		return resolved, classify(err)
	case "secret-binding":
		err := q.QueryRow(ctx, `SELECT a.id::text,e.id::text,e.namespace,p.id::text,COALESCE(p.team_id::text,'')
			FROM applications a JOIN projects p ON p.id=a.project_id JOIN environments e ON e.project_id=p.id
			WHERE a.id=$1 AND e.id=$2`, target.ApplicationID, target.EnvironmentID).
			Scan(&resolved.ApplicationID, &resolved.EnvironmentID, &resolved.Namespace, &resolved.ProjectID, &resolved.TeamID)
		if err != nil {
			return resolved, classify(err)
		}
		if target.ID == "" || target.ProjectID != resolved.ProjectID || target.TeamID != resolved.TeamID || target.Namespace != resolved.Namespace {
			return resolved, base.ErrNotFound
		}
		return resolved, nil
	case "deployment":
		err := q.QueryRow(ctx, `SELECT d.id::text,e.id::text,e.namespace,a.id::text,p.id::text,COALESCE(p.team_id::text,'') FROM deployments d JOIN environments e ON e.id=d.environment_id JOIN applications a ON a.id=d.application_id JOIN projects p ON p.id=e.project_id WHERE d.id=$1`, target.ID).Scan(&resolved.ID, &resolved.EnvironmentID, &resolved.Namespace, &resolved.ApplicationID, &resolved.ProjectID, &resolved.TeamID)
		return resolved, classify(err)
	case "operation":
		var targetType, targetID string
		if err := q.QueryRow(ctx, `SELECT target_type,target_id::text FROM operations WHERE id=$1`, target.ID).Scan(&targetType, &targetID); err != nil {
			return resolved, classify(err)
		}
		switch targetType {
		case "deployment", "environment", "project":
			return resolveAccessTarget(ctx, q, domain.AccessTarget{Type: targetType, ID: targetID})
		default:
			return resolved, base.ErrNotFound
		}
	default:
		return resolved, base.ErrNotFound
	}
}

func authorizeWith(ctx context.Context, q accessQuerier, actor string, permission domain.Permission, target domain.AccessTarget) error {
	resolved, err := resolveAccessTarget(ctx, q, target)
	if err != nil {
		return err
	}
	bindings, err := effectiveBindings(ctx, q, actor)
	if err != nil {
		return err
	}
	if accesspolicy.HasPermission(bindings, resolved, permission) {
		return nil
	}
	projectIsVisible := false
	if resolved.Type == "project" {
		project, getErr := getProject(ctx, q, resolved.ProjectID)
		if getErr != nil {
			return getErr
		}
		projectIsVisible = projectVisible(ctx, q, bindings, project)
	}
	if resolved.Type == "platform" || projectIsVisible || accesspolicy.HasPermission(bindings, resolved, domain.PermissionResourcesRead) {
		return base.ErrForbidden
	}
	return base.ErrNotFound
}

func (s *Store) Authorize(ctx context.Context, actor string, permission domain.Permission, target domain.AccessTarget) error {
	return authorizeWith(ctx, s.pool, actor, permission, target)
}

func (s *Store) AuthorizePromotion(ctx context.Context, actor, environmentID, applicationID string) error {
	var target domain.AccessTarget
	target.Type, target.ID, target.EnvironmentID, target.ApplicationID = "deployment", environmentID+":"+applicationID, environmentID, applicationID
	err := s.pool.QueryRow(ctx, `SELECT e.namespace,p.id::text,COALESCE(p.team_id::text,'')
		FROM environments e JOIN applications a ON a.project_id=e.project_id
		JOIN projects p ON p.id=e.project_id WHERE e.id=$1 AND a.id=$2`, environmentID, applicationID).
		Scan(&target.Namespace, &target.ProjectID, &target.TeamID)
	if err != nil {
		return classify(err)
	}
	bindings, err := effectiveBindings(ctx, s.pool, actor)
	if err != nil {
		return err
	}
	if accesspolicy.HasPermission(bindings, target, domain.PermissionResourcesWrite) {
		return nil
	}
	if accesspolicy.HasPermission(bindings, target, domain.PermissionResourcesRead) {
		return base.ErrForbidden
	}
	return base.ErrNotFound
}

func (s *Store) EffectiveCapabilities(ctx context.Context, actor string) ([]domain.AccessCapability, error) {
	bindings, err := effectiveBindings(ctx, s.pool, actor)
	if err != nil {
		return nil, err
	}
	return accesspolicy.Capabilities(bindings), nil
}

func canAccessTeam(ctx context.Context, q rowQuerier, actor, teamID string) (bool, error) {
	queryer, ok := q.(accessQuerier)
	if !ok {
		return false, errors.New("access queryer does not support Query")
	}
	target := domain.AccessTarget{Type: "platform", ID: "platform"}
	if teamID != "" {
		target = domain.AccessTarget{Type: "team", ID: teamID}
	}
	err := authorizeWith(ctx, queryer, actor, domain.PermissionResourcesRead, target)
	return err == nil, ignoreAccessDenial(err)
}

func canAccessProject(ctx context.Context, q rowQuerier, actor, projectID string) (bool, error) {
	queryer, ok := q.(accessQuerier)
	if !ok {
		return false, errors.New("access queryer does not support Query")
	}
	err := authorizeWith(ctx, queryer, actor, domain.PermissionResourcesRead, domain.AccessTarget{Type: "project", ID: projectID})
	return err == nil, ignoreAccessDenial(err)
}

func ignoreAccessDenial(err error) error {
	if errors.Is(err, base.ErrNotFound) || errors.Is(err, base.ErrForbidden) {
		return nil
	}
	return err
}

func invalidateUsers(ctx context.Context, tx pgx.Tx, userIDs map[string]struct{}) error {
	for userID := range userIDs {
		tag, err := tx.Exec(ctx, `UPDATE users SET grant_revision=grant_revision+1 WHERE id=$1 AND role<>'platform-admin'`, userID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() > 0 {
			if _, err = tx.Exec(ctx, `UPDATE service_account_tokens SET revoked_at=now() WHERE service_account_id=$1 AND revoked_at IS NULL`, userID); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `DELETE FROM sessions WHERE user_id=$1`, userID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) BootstrapRequired(ctx context.Context) (bool, error) {
	var required bool
	err := s.pool.QueryRow(ctx, `SELECT consumed_at IS NULL FROM bootstrap_state WHERE singleton=true`).Scan(&required)
	return required, err
}

func (s *Store) CreateUserInvitation(ctx context.Context, actor, email string, tokenHash []byte, expires time.Time, requestID string) (domain.UserInvitation, error) {
	now := time.Now().UTC()
	if len(tokenHash) != 32 || !expires.After(now) || expires.After(now.Add(24*time.Hour+time.Minute)) {
		return domain.UserInvitation{}, base.ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.UserInvitation{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	admin, err := actorIsAdmin(ctx, tx, actor)
	if err != nil {
		return domain.UserInvitation{}, err
	}
	if !admin {
		return domain.UserInvitation{}, base.ErrForbidden
	}
	invitation := domain.UserInvitation{ID: id.New(), Email: email, ExpiresAt: expires.UTC()}
	_, err = tx.Exec(ctx, `INSERT INTO user_invitations(id,token_hash,email,created_by,expires_at) VALUES($1,$2,$3,$4,$5)`, invitation.ID, tokenHash, email, actor, invitation.ExpiresAt)
	if err != nil {
		return domain.UserInvitation{}, classify(err)
	}
	if err = audit(ctx, tx, actor, "user.invitation.create", "user-invitation", invitation.ID, requestID, map[string]any{"email": email, "expiresAt": invitation.ExpiresAt}); err != nil {
		return domain.UserInvitation{}, err
	}
	return invitation, tx.Commit(ctx)
}

func (s *Store) AcceptUserInvitation(ctx context.Context, tokenHash []byte, displayName, passwordHash string, sessionHash []byte, sessionExpires time.Time) (domain.User, error) {
	if len(tokenHash) != 32 || len(sessionHash) != 32 {
		return domain.User{}, base.ErrInvitationInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return domain.User{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var invitationID, expectedEmail string
	var expires time.Time
	var acceptedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT id,email,expires_at,accepted_at FROM user_invitations WHERE token_hash=$1 FOR UPDATE`, tokenHash).Scan(&invitationID, &expectedEmail, &expires, &acceptedAt)
	now := time.Now().UTC()
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (acceptedAt != nil || !expires.After(now) || !sessionExpires.After(now)) {
		return domain.User{}, base.ErrInvitationInvalid
	}
	if err != nil {
		return domain.User{}, err
	}
	u := domain.User{ID: id.New(), Email: expectedEmail, DisplayName: displayName, Role: "developer", Issuer: "kuberploy:invitation", Subject: invitationID, GrantRevision: 1, CreatedAt: now}
	_, err = tx.Exec(ctx, `INSERT INTO users(id,email,display_name,role,issuer,subject,grant_revision,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, u.ID, u.Email, u.DisplayName, u.Role, u.Issuer, u.Subject, u.GrantRevision, u.CreatedAt)
	if err != nil {
		return domain.User{}, classify(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO user_password_credentials(user_id,email_normalized,password_hash) VALUES($1,$2,$3)`, u.ID, normalizeCredential(u.Email), passwordHash)
	if err != nil {
		return domain.User{}, base.ErrInvitationInvalid
	}
	_, err = tx.Exec(ctx, `INSERT INTO sessions(token_hash,user_id,grant_revision,expires_at) VALUES($1,$2,$3,$4)`, sessionHash, u.ID, u.GrantRevision, sessionExpires)
	if err != nil {
		return domain.User{}, classify(err)
	}
	tag, err := tx.Exec(ctx, `UPDATE user_invitations SET accepted_at=$2,accepted_user_id=$3 WHERE id=$1 AND accepted_at IS NULL`, invitationID, now, u.ID)
	if err != nil || tag.RowsAffected() != 1 {
		return domain.User{}, base.ErrInvitationInvalid
	}
	if err = audit(ctx, tx, u.ID, "user.invitation.accept", "user", u.ID, "", map[string]string{"invitationId": invitationID}); err != nil {
		return domain.User{}, err
	}
	return u, tx.Commit(ctx)
}

func (s *Store) ListUsersForActor(ctx context.Context, actor string) ([]domain.User, error) {
	admin, err := actorIsAdmin(ctx, s.pool, actor)
	if err != nil {
		return nil, err
	}
	query := `SELECT u.id,COALESCE(u.email,''),u.display_name,u.role,u.issuer,u.subject,u.grant_revision,u.created_at FROM users u
		WHERE NOT EXISTS(SELECT 1 FROM service_accounts sa WHERE sa.id=u.id) ORDER BY u.created_at,u.id`
	args := []any{}
	if !admin {
		bindings, bindingErr := effectiveBindings(ctx, s.pool, actor)
		if bindingErr != nil {
			return nil, bindingErr
		}
		managedTeamIDs := teamIDsWithPermission(bindings, domain.PermissionGrantsManage)
		if len(managedTeamIDs) == 0 {
			return nil, base.ErrForbidden
		}
		query = `SELECT DISTINCT u.id,COALESCE(u.email,''),u.display_name,u.role,u.issuer,u.subject,u.grant_revision,u.created_at
			FROM team_memberships visible JOIN users u ON u.id=visible.user_id
			WHERE visible.team_id::text=ANY($1)
			AND NOT EXISTS(SELECT 1 FROM service_accounts sa WHERE sa.id=u.id) ORDER BY u.created_at,u.id`
		args = append(args, managedTeamIDs)
	}
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.User
	for rows.Next() {
		var u domain.User
		if err = rows.Scan(&u.ID, &u.Email, &u.DisplayName, &u.Role, &u.Issuer, &u.Subject, &u.GrantRevision, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) CreateTeam(ctx context.Context, actor, key, fingerprint, requestID string, in domain.CreateTeam) (base.Result[domain.Team], error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return base.Result[domain.Team]{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, advisoryIdentity(actor, "teams.create", key)); err != nil {
		return base.Result[domain.Team]{}, err
	}
	if old, ok, findErr := findIdem(ctx, tx, actor, "teams.create", key); findErr != nil {
		return base.Result[domain.Team]{}, findErr
	} else if ok {
		if old.fingerprint != fingerprint {
			return base.Result[domain.Team]{}, base.ErrIdempotencyConflict
		}
		team, getErr := getTeam(ctx, tx, old.resourceID)
		return base.Result[domain.Team]{Value: team, Replay: true}, getErr
	}
	var actorExists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)`, actor).Scan(&actorExists); err != nil {
		return base.Result[domain.Team]{}, err
	}
	if !actorExists {
		return base.Result[domain.Team]{}, base.ErrForbidden
	}
	now := time.Now().UTC()
	team := domain.Team{ID: id.New(), Name: in.Name, Slug: in.Slug, CreatedAt: now}
	if _, err = tx.Exec(ctx, `INSERT INTO teams(id,name,slug,created_by,created_at) VALUES($1,$2,$3,$4,$5)`, team.ID, team.Name, team.Slug, actor, now); err != nil {
		return base.Result[domain.Team]{}, classify(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO team_memberships(team_id,user_id,role,created_at,updated_at) VALUES($1,$2,'owner',$3,$3)`, team.ID, actor, now); err != nil {
		return base.Result[domain.Team]{}, classify(err)
	}
	if err = audit(ctx, tx, actor, "team.create", "team", team.ID, requestID, team); err != nil {
		return base.Result[domain.Team]{}, err
	}
	if err = putIdem(ctx, tx, actor, "teams.create", key, fingerprint, "team", team.ID, nil); err != nil {
		return base.Result[domain.Team]{}, classify(err)
	}
	if err = invalidateUsers(ctx, tx, map[string]struct{}{actor: {}}); err != nil {
		return base.Result[domain.Team]{}, err
	}
	return base.Result[domain.Team]{Value: team}, tx.Commit(ctx)
}

func getTeam(ctx context.Context, q rowQuerier, teamID string) (domain.Team, error) {
	var team domain.Team
	err := q.QueryRow(ctx, `SELECT id,name,slug,created_at FROM teams WHERE id=$1`, teamID).Scan(&team.ID, &team.Name, &team.Slug, &team.CreatedAt)
	return team, classify(err)
}

func (s *Store) ListTeamsForActor(ctx context.Context, actor string) ([]domain.Team, error) {
	admin, err := actorIsAdmin(ctx, s.pool, actor)
	if err != nil {
		return nil, err
	}
	query := `SELECT t.id,t.name,t.slug,t.created_at FROM teams t ORDER BY t.created_at,t.id`
	args := []any{}
	if !admin {
		bindings, bindingErr := effectiveBindings(ctx, s.pool, actor)
		if bindingErr != nil {
			return nil, bindingErr
		}
		visibleTeamIDs := teamIDsWithPermission(bindings, domain.PermissionResourcesRead)
		if len(visibleTeamIDs) == 0 {
			return []domain.Team{}, nil
		}
		query = `SELECT t.id,t.name,t.slug,t.created_at FROM teams t WHERE t.id::text=ANY($1) ORDER BY t.created_at,t.id`
		args = append(args, visibleTeamIDs)
	}
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Team
	for rows.Next() {
		var team domain.Team
		if err = rows.Scan(&team.ID, &team.Name, &team.Slug, &team.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, team)
	}
	return out, rows.Err()
}

func (s *Store) ListTeamMembersForActor(ctx context.Context, actor, teamID string) ([]domain.TeamMember, error) {
	allowed, err := canAccessTeam(ctx, s.pool, actor, teamID)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, base.ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT m.team_id,m.user_id,m.role,m.created_at,u.id,COALESCE(u.email,''),u.display_name,u.role,u.issuer,u.subject,u.grant_revision,u.created_at
        FROM team_memberships m JOIN users u ON u.id=m.user_id WHERE m.team_id=$1 ORDER BY m.created_at,m.user_id`, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.TeamMember
	for rows.Next() {
		var member domain.TeamMember
		var u domain.User
		if err = rows.Scan(&member.TeamID, &member.UserID, &member.Role, &member.CreatedAt, &u.ID, &u.Email, &u.DisplayName, &u.Role, &u.Issuer, &u.Subject, &u.GrantRevision, &u.CreatedAt); err != nil {
			return nil, err
		}
		member.User = &u
		out = append(out, member)
	}
	return out, rows.Err()
}

func (s *Store) AddTeamMember(ctx context.Context, actor, teamID, key, fingerprint, requestID string, in domain.AddTeamMember) (base.Result[domain.TeamMember], error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return base.Result[domain.TeamMember]{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	scope := "teams.members.add:" + teamID
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, advisoryIdentity(actor, scope, key)); err != nil {
		return base.Result[domain.TeamMember]{}, err
	}
	if old, ok, findErr := findIdem(ctx, tx, actor, scope, key); findErr != nil {
		return base.Result[domain.TeamMember]{}, findErr
	} else if ok {
		if old.fingerprint != fingerprint {
			return base.Result[domain.TeamMember]{}, base.ErrIdempotencyConflict
		}
		member, getErr := getTeamMember(ctx, tx, teamID, old.resourceID)
		return base.Result[domain.TeamMember]{Value: member, Replay: true}, getErr
	}
	var lockedTeamID string
	if err = tx.QueryRow(ctx, `SELECT id FROM teams WHERE id=$1 FOR UPDATE`, teamID).Scan(&lockedTeamID); err != nil {
		return base.Result[domain.TeamMember]{}, classify(err)
	}
	authorized, err := actorCanManageTeam(ctx, tx, actor, teamID)
	if err != nil {
		return base.Result[domain.TeamMember]{}, err
	}
	if !authorized {
		return base.Result[domain.TeamMember]{}, base.ErrForbidden
	}
	var targetExists bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND NOT EXISTS(SELECT 1 FROM service_accounts WHERE id=$1))`, in.UserID).Scan(&targetExists)
	if err != nil {
		return base.Result[domain.TeamMember]{}, err
	}
	if !targetExists {
		return base.Result[domain.TeamMember]{}, base.ErrNotFound
	}
	var oldRole string
	var createdAt time.Time
	err = tx.QueryRow(ctx, `SELECT role,created_at FROM team_memberships WHERE team_id=$1 AND user_id=$2 FOR UPDATE`, teamID, in.UserID).Scan(&oldRole, &createdAt)
	existed := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return base.Result[domain.TeamMember]{}, err
	}
	if existed && oldRole == "owner" && in.Role != "owner" {
		var ownerCount int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM team_memberships WHERE team_id=$1 AND role='owner'`, teamID).Scan(&ownerCount); err != nil {
			return base.Result[domain.TeamMember]{}, err
		}
		if ownerCount <= 1 {
			return base.Result[domain.TeamMember]{}, base.ErrConflict
		}
	}
	now := time.Now().UTC()
	if !existed {
		createdAt = now
	}
	_, err = tx.Exec(ctx, `INSERT INTO team_memberships(team_id,user_id,role,created_at,updated_at) VALUES($1,$2,$3,$4,$5)
        ON CONFLICT(team_id,user_id) DO UPDATE SET role=EXCLUDED.role,updated_at=EXCLUDED.updated_at`, teamID, in.UserID, in.Role, createdAt, now)
	if err != nil {
		return base.Result[domain.TeamMember]{}, classify(err)
	}
	member := domain.TeamMember{TeamID: teamID, UserID: in.UserID, Role: in.Role, CreatedAt: createdAt}
	if err = audit(ctx, tx, actor, "team.member.upsert", "team", teamID, requestID, map[string]string{"userId": in.UserID, "role": in.Role}); err != nil {
		return base.Result[domain.TeamMember]{}, err
	}
	if err = putIdem(ctx, tx, actor, scope, key, fingerprint, "team-member", in.UserID, nil); err != nil {
		return base.Result[domain.TeamMember]{}, classify(err)
	}
	if !existed || oldRole != in.Role {
		if err = invalidateUsers(ctx, tx, map[string]struct{}{in.UserID: {}}); err != nil {
			return base.Result[domain.TeamMember]{}, err
		}
	}
	return base.Result[domain.TeamMember]{Value: member}, tx.Commit(ctx)
}

func (s *Store) RemoveTeamMember(ctx context.Context, actor, teamID, userID, requestID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var lockedTeamID string
	if err = tx.QueryRow(ctx, `SELECT id FROM teams WHERE id=$1 FOR UPDATE`, teamID).Scan(&lockedTeamID); err != nil {
		return classify(err)
	}
	authorized, err := actorCanManageTeam(ctx, tx, actor, teamID)
	if err != nil {
		return err
	}
	if !authorized {
		return base.ErrForbidden
	}
	var role string
	err = tx.QueryRow(ctx, `SELECT role FROM team_memberships WHERE team_id=$1 AND user_id=$2 FOR UPDATE`, teamID, userID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if role == "owner" {
		var ownerCount int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM team_memberships WHERE team_id=$1 AND role='owner'`, teamID).Scan(&ownerCount); err != nil {
			return err
		}
		if ownerCount <= 1 {
			return base.ErrConflict
		}
	}
	tag, err := tx.Exec(ctx, `DELETE FROM team_memberships WHERE team_id=$1 AND user_id=$2`, teamID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return base.ErrConflict
	}
	if err = audit(ctx, tx, actor, "team.member.remove", "team", teamID, requestID, map[string]string{"userId": userID, "role": role}); err != nil {
		return err
	}
	if err = invalidateUsers(ctx, tx, map[string]struct{}{userID: {}}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func getTeamMember(ctx context.Context, q rowQuerier, teamID, userID string) (domain.TeamMember, error) {
	var member domain.TeamMember
	err := q.QueryRow(ctx, `SELECT team_id,user_id,role,created_at FROM team_memberships WHERE team_id=$1 AND user_id=$2`, teamID, userID).Scan(&member.TeamID, &member.UserID, &member.Role, &member.CreatedAt)
	return member, classify(err)
}

func (s *Store) CreateGitHubInstallation(ctx context.Context, actor, key, fingerprint, requestID string, in domain.CreateGitHubInstallation) (base.Result[domain.GitHubInstallation], error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return base.Result[domain.GitHubInstallation]{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	admin, err := actorIsAdmin(ctx, tx, actor)
	if err != nil {
		return base.Result[domain.GitHubInstallation]{}, err
	}
	if !admin {
		return base.Result[domain.GitHubInstallation]{}, base.ErrForbidden
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, advisoryIdentity(actor, "github-installations.create", key)); err != nil {
		return base.Result[domain.GitHubInstallation]{}, err
	}
	if old, ok, findErr := findIdem(ctx, tx, actor, "github-installations.create", key); findErr != nil {
		return base.Result[domain.GitHubInstallation]{}, findErr
	} else if ok {
		if old.fingerprint != fingerprint {
			return base.Result[domain.GitHubInstallation]{}, base.ErrIdempotencyConflict
		}
		installation, getErr := getGitHubInstallation(ctx, tx, old.resourceID, false)
		return base.Result[domain.GitHubInstallation]{Value: installation, Replay: true}, getErr
	}
	now := time.Now().UTC()
	installation := domain.GitHubInstallation{ID: id.New(), GitHubInstallationID: in.GitHubInstallationID, AccountLogin: in.AccountLogin, AccountType: in.AccountType, OwnerUserID: actor, Visibility: "private", RepositorySelection: in.RepositorySelection, RepositoryCount: in.RepositoryCount, CreatedAt: now, UpdatedAt: now}
	_, err = tx.Exec(ctx, `INSERT INTO github_installations(id,github_installation_id,account_login,account_type,owner_user_id,visibility,repository_selection,repository_count,created_at,updated_at) VALUES($1,$2,$3,$4,$5,'private',$6,$7,$8,$8)`, installation.ID, installation.GitHubInstallationID, installation.AccountLogin, installation.AccountType, actor, installation.RepositorySelection, installation.RepositoryCount, now)
	if err != nil {
		return base.Result[domain.GitHubInstallation]{}, classify(err)
	}
	if err = audit(ctx, tx, actor, "github.installation.register", "github-installation", installation.ID, requestID, map[string]any{"githubInstallationId": installation.GitHubInstallationID, "accountLogin": installation.AccountLogin}); err != nil {
		return base.Result[domain.GitHubInstallation]{}, err
	}
	if err = putIdem(ctx, tx, actor, "github-installations.create", key, fingerprint, "github-installation", installation.ID, nil); err != nil {
		return base.Result[domain.GitHubInstallation]{}, classify(err)
	}
	return base.Result[domain.GitHubInstallation]{Value: installation}, tx.Commit(ctx)
}

func (s *Store) LinkVerifiedGitHubInstallation(ctx context.Context, actor, key, fingerprint, requestID string, in domain.CreateGitHubInstallation) (domain.GitHubInstallation, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.GitHubInstallation{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, advisoryIdentity(actor, "github-installations.link", key)); err != nil {
		return domain.GitHubInstallation{}, false, err
	}
	if old, ok, findErr := findIdem(ctx, tx, actor, "github-installations.link", key); findErr != nil {
		return domain.GitHubInstallation{}, false, findErr
	} else if ok {
		if old.fingerprint != fingerprint {
			return domain.GitHubInstallation{}, false, base.ErrIdempotencyConflict
		}
		installation, getErr := getGitHubInstallation(ctx, tx, old.resourceID, false)
		if getErr != nil {
			return domain.GitHubInstallation{}, false, getErr
		}
		return installation, true, tx.Commit(ctx)
	}
	now := time.Now().UTC()
	var existing domain.GitHubInstallation
	err = tx.QueryRow(ctx, `SELECT id,github_installation_id,account_login,account_type,owner_user_id,visibility,COALESCE(team_id::text,''),repository_selection,repository_count,created_at,updated_at
		FROM github_installations WHERE github_installation_id=$1 FOR UPDATE`, in.GitHubInstallationID).Scan(&existing.ID, &existing.GitHubInstallationID,
		&existing.AccountLogin, &existing.AccountType, &existing.OwnerUserID, &existing.Visibility, &existing.TeamID,
		&existing.RepositorySelection, &existing.RepositoryCount, &existing.CreatedAt, &existing.UpdatedAt)
	if err == nil {
		if existing.OwnerUserID != actor || !strings.EqualFold(existing.AccountLogin, in.AccountLogin) || existing.AccountType != in.AccountType ||
			existing.RepositorySelection != in.RepositorySelection {
			return domain.GitHubInstallation{}, false, base.ErrConflict
		}
		existing.RepositoryCount = in.RepositoryCount
		existing.UpdatedAt = now
		if _, err = tx.Exec(ctx, `UPDATE github_installations SET repository_count=$2,updated_at=$3 WHERE id=$1`, existing.ID, existing.RepositoryCount, now); err != nil {
			return domain.GitHubInstallation{}, false, classify(err)
		}
		if err = audit(ctx, tx, actor, "github.installation.verify", "github-installation", existing.ID, requestID,
			map[string]any{"githubInstallationId": existing.GitHubInstallationID, "accountLogin": existing.AccountLogin}); err != nil {
			return domain.GitHubInstallation{}, false, err
		}
		if err = putIdem(ctx, tx, actor, "github-installations.link", key, fingerprint, "github-installation", existing.ID, nil); err != nil {
			return domain.GitHubInstallation{}, false, classify(err)
		}
		return existing, false, tx.Commit(ctx)
	}
	if !errors.Is(classify(err), base.ErrNotFound) {
		return domain.GitHubInstallation{}, false, classify(err)
	}
	installation := domain.GitHubInstallation{ID: id.New(), GitHubInstallationID: in.GitHubInstallationID, AccountLogin: in.AccountLogin,
		AccountType: in.AccountType, OwnerUserID: actor, Visibility: "private", RepositorySelection: in.RepositorySelection,
		RepositoryCount: in.RepositoryCount, CreatedAt: now, UpdatedAt: now}
	_, err = tx.Exec(ctx, `INSERT INTO github_installations(id,github_installation_id,account_login,account_type,owner_user_id,visibility,repository_selection,repository_count,created_at,updated_at) VALUES($1,$2,$3,$4,$5,'private',$6,$7,$8,$8)`,
		installation.ID, installation.GitHubInstallationID, installation.AccountLogin, installation.AccountType, actor,
		installation.RepositorySelection, installation.RepositoryCount, now)
	if err != nil {
		return domain.GitHubInstallation{}, false, classify(err)
	}
	if err = audit(ctx, tx, actor, "github.installation.link", "github-installation", installation.ID, requestID,
		map[string]any{"githubInstallationId": installation.GitHubInstallationID, "accountLogin": installation.AccountLogin}); err != nil {
		return domain.GitHubInstallation{}, false, err
	}
	if err = putIdem(ctx, tx, actor, "github-installations.link", key, fingerprint, "github-installation", installation.ID, nil); err != nil {
		return domain.GitHubInstallation{}, false, classify(err)
	}
	return installation, false, tx.Commit(ctx)
}

func getGitHubInstallation(ctx context.Context, q rowQuerier, installationID string, forUpdate bool) (domain.GitHubInstallation, error) {
	var installation domain.GitHubInstallation
	query := `SELECT id,github_installation_id,account_login,account_type,owner_user_id,visibility,COALESCE(team_id::text,''),repository_selection,repository_count,created_at,updated_at FROM github_installations WHERE id=$1`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	err := q.QueryRow(ctx, query, installationID).Scan(&installation.ID, &installation.GitHubInstallationID, &installation.AccountLogin, &installation.AccountType, &installation.OwnerUserID, &installation.Visibility, &installation.TeamID, &installation.RepositorySelection, &installation.RepositoryCount, &installation.CreatedAt, &installation.UpdatedAt)
	return installation, classify(err)
}

func (s *Store) ListGitHubInstallationsForActor(ctx context.Context, actor string) ([]domain.GitHubInstallation, error) {
	rows, err := s.pool.Query(ctx, `SELECT i.id,i.github_installation_id,i.account_login,i.account_type,i.owner_user_id,i.visibility,COALESCE(i.team_id::text,''),i.repository_selection,i.repository_count,i.created_at,i.updated_at
        FROM github_installations i WHERE
		EXISTS(SELECT 1 FROM access_grants WHERE subject_user_id=$1 AND role='platform-admin' AND scope_type='platform' AND scope_id='platform') OR
        i.owner_user_id=$1 OR
        (i.visibility='team' AND EXISTS(SELECT 1 FROM team_memberships WHERE team_id=i.team_id AND user_id=$1))
        ORDER BY i.created_at,i.id`, actor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.GitHubInstallation
	for rows.Next() {
		var installation domain.GitHubInstallation
		if err = rows.Scan(&installation.ID, &installation.GitHubInstallationID, &installation.AccountLogin, &installation.AccountType, &installation.OwnerUserID, &installation.Visibility, &installation.TeamID, &installation.RepositorySelection, &installation.RepositoryCount, &installation.CreatedAt, &installation.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, installation)
	}
	return out, rows.Err()
}

func (s *Store) AuthorizeGitHubInstallationForProject(ctx context.Context, actor, installationID, projectID string) error {
	if err := authorizeWith(ctx, s.pool, actor, domain.PermissionBuildsManage, domain.AccessTarget{Type: "project", ID: projectID}); err != nil {
		return err
	}
	var ownerID, visibility, installationTeamID, projectTeamID string
	err := s.pool.QueryRow(ctx, `SELECT i.owner_user_id::text,i.visibility,COALESCE(i.team_id::text,''),COALESCE(p.team_id::text,'')
		FROM github_installations i CROSS JOIN projects p WHERE i.id=$1 AND p.id=$2`, installationID, projectID).
		Scan(&ownerID, &visibility, &installationTeamID, &projectTeamID)
	if err != nil {
		return classify(err)
	}
	admin, err := actorIsAdmin(ctx, s.pool, actor)
	if err != nil {
		return err
	}
	if admin || ownerID == actor || visibility == "team" && installationTeamID != "" && installationTeamID == projectTeamID {
		return nil
	}
	return base.ErrNotFound
}

func (s *Store) UpdateGitHubInstallationSharing(ctx context.Context, actor, installationID, key, fingerprint, requestID string, in domain.UpdateGitHubInstallationSharing) (base.Result[domain.GitHubInstallation], error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return base.Result[domain.GitHubInstallation]{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	idemScope := "github-installations.sharing:" + installationID
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, advisoryIdentity(actor, idemScope, key)); err != nil {
		return base.Result[domain.GitHubInstallation]{}, err
	}
	if old, ok, findErr := findIdem(ctx, tx, actor, idemScope, key); findErr != nil {
		return base.Result[domain.GitHubInstallation]{}, findErr
	} else if ok {
		if old.fingerprint != fingerprint {
			return base.Result[domain.GitHubInstallation]{}, base.ErrIdempotencyConflict
		}
		stored, getErr := getGitHubInstallation(ctx, tx, old.resourceID, false)
		if getErr != nil {
			return base.Result[domain.GitHubInstallation]{}, getErr
		}
		return base.Result[domain.GitHubInstallation]{Value: stored, Replay: true}, tx.Commit(ctx)
	}
	installation, err := getGitHubInstallation(ctx, tx, installationID, true)
	if err != nil {
		return base.Result[domain.GitHubInstallation]{}, err
	}
	admin, err := actorIsAdmin(ctx, tx, actor)
	if err != nil {
		return base.Result[domain.GitHubInstallation]{}, err
	}
	currentOwner := false
	if installation.TeamID != "" {
		currentOwner, err = actorCanManageTeam(ctx, tx, actor, installation.TeamID)
		if err != nil {
			return base.Result[domain.GitHubInstallation]{}, err
		}
	}
	if !admin && actor != installation.OwnerUserID && !currentOwner {
		return base.Result[domain.GitHubInstallation]{}, base.ErrForbidden
	}
	if in.Visibility == "team" {
		targetOwner, manageErr := actorCanManageTeam(ctx, tx, actor, in.TeamID)
		if manageErr != nil {
			return base.Result[domain.GitHubInstallation]{}, manageErr
		}
		if !admin && !targetOwner {
			return base.Result[domain.GitHubInstallation]{}, base.ErrForbidden
		}
		var teamExists bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM teams WHERE id=$1)`, in.TeamID).Scan(&teamExists); err != nil {
			return base.Result[domain.GitHubInstallation]{}, err
		}
		if !teamExists {
			return base.Result[domain.GitHubInstallation]{}, base.ErrNotFound
		}
	}
	if installation.Visibility == in.Visibility && installation.TeamID == in.TeamID {
		if err = putIdem(ctx, tx, actor, idemScope, key, fingerprint, "github-installation", installation.ID, nil); err != nil {
			return base.Result[domain.GitHubInstallation]{}, classify(err)
		}
		return base.Result[domain.GitHubInstallation]{Value: installation}, tx.Commit(ctx)
	}
	affected := map[string]struct{}{installation.OwnerUserID: {}}
	for _, teamID := range []string{installation.TeamID, in.TeamID} {
		if teamID == "" {
			continue
		}
		rows, queryErr := tx.Query(ctx, `SELECT user_id FROM team_memberships WHERE team_id=$1`, teamID)
		if queryErr != nil {
			return base.Result[domain.GitHubInstallation]{}, queryErr
		}
		for rows.Next() {
			var userID string
			if queryErr = rows.Scan(&userID); queryErr != nil {
				rows.Close()
				return base.Result[domain.GitHubInstallation]{}, queryErr
			}
			affected[userID] = struct{}{}
		}
		rows.Close()
		if queryErr = rows.Err(); queryErr != nil {
			return base.Result[domain.GitHubInstallation]{}, queryErr
		}
	}
	now := time.Now().UTC()
	_, err = tx.Exec(ctx, `UPDATE github_installations SET visibility=$2,team_id=NULLIF($3,'')::uuid,updated_at=$4 WHERE id=$1`, installation.ID, in.Visibility, in.TeamID, now)
	if err != nil {
		return base.Result[domain.GitHubInstallation]{}, classify(err)
	}
	installation.Visibility, installation.TeamID, installation.UpdatedAt = in.Visibility, in.TeamID, now
	if err = audit(ctx, tx, actor, "github.installation.sharing.update", "github-installation", installation.ID, requestID, map[string]string{"visibility": in.Visibility, "teamId": in.TeamID}); err != nil {
		return base.Result[domain.GitHubInstallation]{}, err
	}
	if err = invalidateUsers(ctx, tx, affected); err != nil {
		return base.Result[domain.GitHubInstallation]{}, err
	}
	if err = putIdem(ctx, tx, actor, idemScope, key, fingerprint, "github-installation", installation.ID, nil); err != nil {
		return base.Result[domain.GitHubInstallation]{}, classify(err)
	}
	return base.Result[domain.GitHubInstallation]{Value: installation}, tx.Commit(ctx)
}

func projectVisible(ctx context.Context, q accessQuerier, bindings []domain.AccessBinding, project domain.Project) bool {
	target := domain.AccessTarget{Type: "project", ID: project.ID, TeamID: project.TeamID, ProjectID: project.ID}
	if accesspolicy.HasPermission(bindings, target, domain.PermissionResourcesRead) {
		return true
	}
	for _, binding := range bindings {
		scopeTarget, err := scopeTargetForProject(ctx, q, project, binding.ScopeType, binding.ScopeID)
		if err == nil && accesspolicy.HasPermission([]domain.AccessBinding{binding}, scopeTarget, domain.PermissionResourcesRead) {
			return true
		}
	}
	return false
}

func (s *Store) ListProjectsForActor(ctx context.Context, actor string) ([]domain.Project, error) {
	bindings, err := effectiveBindings(ctx, s.pool, actor)
	if err != nil {
		return nil, err
	}
	projects, err := s.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Project, 0, len(projects))
	for _, project := range projects {
		if projectVisible(ctx, s.pool, bindings, project) {
			out = append(out, project)
		}
	}
	return out, nil
}

func (s *Store) GetProjectForActor(ctx context.Context, actor, projectID string) (domain.Project, error) {
	project, err := getProject(ctx, s.pool, projectID)
	if err != nil {
		return domain.Project{}, err
	}
	bindings, err := effectiveBindings(ctx, s.pool, actor)
	if err != nil {
		return domain.Project{}, err
	}
	if !projectVisible(ctx, s.pool, bindings, project) {
		return domain.Project{}, base.ErrNotFound
	}
	return project, nil
}

func (s *Store) ListEnvironmentsForActor(ctx context.Context, actor string) ([]domain.Environment, error) {
	bindings, err := effectiveBindings(ctx, s.pool, actor)
	if err != nil {
		return nil, err
	}
	items, err := s.ListEnvironments(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Environment, 0, len(items))
	for _, item := range items {
		target, targetErr := resolveAccessTarget(ctx, s.pool, domain.AccessTarget{Type: "environment", ID: item.ID})
		if targetErr != nil {
			return nil, targetErr
		}
		if accesspolicy.HasPermission(bindings, target, domain.PermissionResourcesRead) {
			out = append(out, item)
		}
	}
	return out, nil
}

func (s *Store) GetEnvironmentForActor(ctx context.Context, actor, environmentID string) (domain.Environment, error) {
	if err := authorizeWith(ctx, s.pool, actor, domain.PermissionResourcesRead, domain.AccessTarget{Type: "environment", ID: environmentID}); err != nil {
		return domain.Environment{}, err
	}
	return getEnvironment(ctx, s.pool, environmentID)
}

func (s *Store) ListApplicationsForActor(ctx context.Context, actor string) ([]domain.Application, error) {
	bindings, err := effectiveBindings(ctx, s.pool, actor)
	if err != nil {
		return nil, err
	}
	items, err := s.ListApplications(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Application, 0, len(items))
	for _, item := range items {
		target, targetErr := resolveAccessTarget(ctx, s.pool, domain.AccessTarget{Type: "application", ID: item.ID})
		if targetErr != nil {
			return nil, targetErr
		}
		if accesspolicy.HasPermission(bindings, target, domain.PermissionResourcesRead) {
			out = append(out, item)
		}
	}
	return out, nil
}

func (s *Store) GetApplicationForActor(ctx context.Context, actor, applicationID string) (domain.Application, error) {
	if err := authorizeWith(ctx, s.pool, actor, domain.PermissionResourcesRead, domain.AccessTarget{Type: "application", ID: applicationID}); err != nil {
		return domain.Application{}, err
	}
	return getApplication(ctx, s.pool, applicationID)
}

func (s *Store) ListDeploymentsForActor(ctx context.Context, actor string) ([]domain.Deployment, error) {
	bindings, err := effectiveBindings(ctx, s.pool, actor)
	if err != nil {
		return nil, err
	}
	items, err := s.ListDeployments(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Deployment, 0, len(items))
	for _, item := range items {
		target, targetErr := resolveAccessTarget(ctx, s.pool, domain.AccessTarget{Type: "deployment", ID: item.ID})
		if targetErr != nil {
			return nil, targetErr
		}
		if accesspolicy.HasPermission(bindings, target, domain.PermissionResourcesRead) {
			out = append(out, item)
		}
	}
	return out, nil
}

func (s *Store) GetDeploymentForActor(ctx context.Context, actor, deploymentID string) (domain.Deployment, error) {
	if err := authorizeWith(ctx, s.pool, actor, domain.PermissionResourcesRead, domain.AccessTarget{Type: "deployment", ID: deploymentID}); err != nil {
		return domain.Deployment{}, err
	}
	return getDeployment(ctx, s.pool, deploymentID)
}

func (s *Store) DeploymentStatusForActor(ctx context.Context, actor, deploymentID string) (domain.DeploymentStatus, error) {
	if err := authorizeWith(ctx, s.pool, actor, domain.PermissionResourcesRead, domain.AccessTarget{Type: "deployment", ID: deploymentID}); err != nil {
		return domain.DeploymentStatus{}, err
	}
	return deploymentStatus(ctx, s.pool, deploymentID)
}

func (s *Store) ListOperationsForActor(ctx context.Context, actor string) ([]domain.Operation, error) {
	bindings, err := effectiveBindings(ctx, s.pool, actor)
	if err != nil {
		return nil, err
	}
	items, err := s.ListOperations(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Operation, 0, len(items))
	for _, item := range items {
		target, targetErr := resolveAccessTarget(ctx, s.pool, domain.AccessTarget{Type: "operation", ID: item.ID})
		if targetErr != nil {
			if errors.Is(targetErr, base.ErrNotFound) {
				continue
			}
			return nil, targetErr
		}
		if accesspolicy.HasPermission(bindings, target, domain.PermissionOperationsRead) {
			out = append(out, item)
		}
	}
	return out, nil
}

func (s *Store) GetOperationForActor(ctx context.Context, actor, operationID string) (domain.Operation, error) {
	if err := authorizeWith(ctx, s.pool, actor, domain.PermissionOperationsRead, domain.AccessTarget{Type: "operation", ID: operationID}); err != nil {
		return domain.Operation{}, err
	}
	return getOperation(ctx, s.pool, operationID)
}

func scopeTargetForProject(ctx context.Context, q rowQuerier, project domain.Project, scopeType domain.AccessScopeType, scopeID string) (domain.AccessTarget, error) {
	var target domain.AccessTarget
	var err error
	switch scopeType {
	case domain.ScopeTeam:
		if project.TeamID == "" || scopeID != project.TeamID {
			return target, base.ErrNotFound
		}
		target, err = resolveAccessTarget(ctx, q, domain.AccessTarget{Type: "team", ID: scopeID})
	case domain.ScopeProject:
		if scopeID != project.ID {
			return target, base.ErrNotFound
		}
		target, err = resolveAccessTarget(ctx, q, domain.AccessTarget{Type: "project", ID: scopeID})
	case domain.ScopeEnvironment:
		target, err = resolveAccessTarget(ctx, q, domain.AccessTarget{Type: "environment", ID: scopeID})
	case domain.ScopeNamespace:
		target, err = resolveAccessTarget(ctx, q, domain.AccessTarget{Type: "namespace", ID: scopeID})
	case domain.ScopeApplication:
		target, err = resolveAccessTarget(ctx, q, domain.AccessTarget{Type: "application", ID: scopeID})
	default:
		return target, base.ErrNotFound
	}
	if err != nil {
		return target, err
	}
	if scopeType == domain.ScopeTeam {
		if target.TeamID != project.TeamID {
			return target, base.ErrNotFound
		}
	} else if target.ProjectID != project.ID {
		return target, base.ErrNotFound
	}
	return target, nil
}

func scanAccessGrant(row pgx.Row) (domain.AccessGrant, error) {
	grant := domain.AccessGrant{Permissions: []domain.Permission{}}
	var permissions []string
	err := row.Scan(&grant.ID, &grant.SubjectUserID, &grant.SubjectTeamID, &grant.Role, &grant.ScopeType, &grant.ScopeID, &permissions, &grant.Source, &grant.CreatedBy, &grant.CreatedAt)
	for _, permission := range permissions {
		grant.Permissions = append(grant.Permissions, domain.Permission(permission))
	}
	return grant, classify(err)
}

func (s *Store) ListProjectAccessGrants(ctx context.Context, actor, projectID string) ([]domain.AccessGrant, error) {
	if err := authorizeWith(ctx, s.pool, actor, domain.PermissionGrantsRead, domain.AccessTarget{Type: "project", ID: projectID}); err != nil {
		return nil, err
	}
	project, err := getProject(ctx, s.pool, projectID)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT g.id,COALESCE(g.subject_user_id::text,''),COALESCE(g.subject_team_id::text,''),g.role,g.scope_type,g.scope_id,g.permissions,g.source,g.created_by,g.created_at
		FROM access_grants g WHERE
		(g.scope_type='team' AND g.scope_id=$2) OR
		(g.scope_type='project' AND g.scope_id=$1::text) OR
		(g.scope_type='environment' AND EXISTS(SELECT 1 FROM environments e WHERE e.id::text=g.scope_id AND e.project_id=$1::uuid)) OR
		(g.scope_type='namespace' AND EXISTS(SELECT 1 FROM environments e WHERE e.namespace=g.scope_id AND e.project_id=$1::uuid)) OR
		(g.scope_type='application' AND EXISTS(SELECT 1 FROM applications a WHERE a.id::text=g.scope_id AND a.project_id=$1::uuid))
		ORDER BY g.created_at,g.id`, project.ID, project.TeamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.AccessGrant, 0)
	for rows.Next() {
		grant := domain.AccessGrant{Permissions: []domain.Permission{}}
		var permissions []string
		if err = rows.Scan(&grant.ID, &grant.SubjectUserID, &grant.SubjectTeamID, &grant.Role, &grant.ScopeType, &grant.ScopeID, &permissions, &grant.Source, &grant.CreatedBy, &grant.CreatedAt); err != nil {
			return nil, err
		}
		for _, permission := range permissions {
			grant.Permissions = append(grant.Permissions, domain.Permission(permission))
		}
		out = append(out, grant)
	}
	return out, rows.Err()
}

func (s *Store) CreateProjectAccessGrant(ctx context.Context, actor, key, fingerprint, requestID string, in domain.CreateAccessGrant) (base.Result[domain.AccessGrant], error) {
	if (in.SubjectUserID == "") == (in.SubjectTeamID == "") || !accesspolicy.ValidRole(in.Role) || !accesspolicy.ValidScope(in.ScopeType) || !accesspolicy.ValidExtraPermissions(in.Permissions) || in.Role == domain.RolePlatformAdmin || in.ScopeType == domain.ScopePlatform || in.Role == domain.RoleOrganizationAdmin && in.ScopeType != domain.ScopeTeam || in.Role == domain.RoleProjectAdmin && in.ScopeType != domain.ScopeProject {
		return base.Result[domain.AccessGrant]{}, base.ErrConflict
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return base.Result[domain.AccessGrant]{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, advisoryIdentity(actor, "projects.grants.create:"+in.ProjectID, key)); err != nil {
		return base.Result[domain.AccessGrant]{}, err
	}
	project, err := getProject(ctx, tx, in.ProjectID)
	if err != nil {
		return base.Result[domain.AccessGrant]{}, err
	}
	target, err := scopeTargetForProject(ctx, tx, project, in.ScopeType, in.ScopeID)
	if err != nil {
		return base.Result[domain.AccessGrant]{}, err
	}
	bindings, err := effectiveBindings(ctx, tx, actor)
	if err != nil {
		return base.Result[domain.AccessGrant]{}, err
	}
	projectTarget, _ := resolveAccessTarget(ctx, tx, domain.AccessTarget{Type: "project", ID: project.ID})
	if !accesspolicy.HasPermission(bindings, projectTarget, domain.PermissionResourcesRead) {
		return base.Result[domain.AccessGrant]{}, base.ErrNotFound
	}
	if !accesspolicy.CanManageGrant(bindings, target, in.Role) {
		return base.Result[domain.AccessGrant]{}, base.ErrForbidden
	}
	var subjectExists bool
	if in.SubjectUserID != "" {
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)`, in.SubjectUserID).Scan(&subjectExists)
	} else {
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM teams WHERE id=$1)`, in.SubjectTeamID).Scan(&subjectExists)
	}
	if err != nil {
		return base.Result[domain.AccessGrant]{}, err
	}
	if !subjectExists {
		return base.Result[domain.AccessGrant]{}, base.ErrNotFound
	}
	idemScope := "projects.grants.create:" + in.ProjectID
	if old, ok, findErr := findIdem(ctx, tx, actor, idemScope, key); findErr != nil {
		return base.Result[domain.AccessGrant]{}, findErr
	} else if ok {
		if old.fingerprint != fingerprint {
			return base.Result[domain.AccessGrant]{}, base.ErrIdempotencyConflict
		}
		grant, getErr := scanAccessGrant(tx.QueryRow(ctx, `SELECT id,COALESCE(subject_user_id::text,''),COALESCE(subject_team_id::text,''),role,scope_type,scope_id,permissions,source,created_by,created_at FROM access_grants WHERE id=$1`, old.resourceID))
		return base.Result[domain.AccessGrant]{Value: grant, Replay: true}, getErr
	}
	permissions := make([]string, len(in.Permissions))
	for index, permission := range in.Permissions {
		permissions[index] = string(permission)
	}
	grant := domain.AccessGrant{ID: id.New(), SubjectUserID: in.SubjectUserID, SubjectTeamID: in.SubjectTeamID, Role: in.Role, ScopeType: in.ScopeType, ScopeID: in.ScopeID, Permissions: append([]domain.Permission{}, in.Permissions...), Source: "explicit", CreatedBy: actor, CreatedAt: time.Now().UTC()}
	_, err = tx.Exec(ctx, `INSERT INTO access_grants(id,subject_user_id,subject_team_id,role,scope_type,scope_id,permissions,source,created_by,created_at) VALUES($1,NULLIF($2,'')::uuid,NULLIF($3,'')::uuid,$4,$5,$6,$7,'explicit',$8,$9)`, grant.ID, grant.SubjectUserID, grant.SubjectTeamID, grant.Role, grant.ScopeType, grant.ScopeID, permissions, actor, grant.CreatedAt)
	if err != nil {
		return base.Result[domain.AccessGrant]{}, classify(err)
	}
	if err = audit(ctx, tx, actor, "access-grant.create", "access-grant", grant.ID, requestID, map[string]any{"projectId": project.ID, "subjectUserId": grant.SubjectUserID, "subjectTeamId": grant.SubjectTeamID, "role": grant.Role, "scopeType": grant.ScopeType, "scopeId": grant.ScopeID, "permissions": grant.Permissions}); err != nil {
		return base.Result[domain.AccessGrant]{}, err
	}
	if err = putIdem(ctx, tx, actor, idemScope, key, fingerprint, "access-grant", grant.ID, nil); err != nil {
		return base.Result[domain.AccessGrant]{}, classify(err)
	}
	if err = invalidateGrantSubjects(ctx, tx, grant); err != nil {
		return base.Result[domain.AccessGrant]{}, err
	}
	return base.Result[domain.AccessGrant]{Value: grant}, tx.Commit(ctx)
}

func (s *Store) DeleteProjectAccessGrant(ctx context.Context, actor, projectID, grantID, key, fingerprint, requestID string) (bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, advisoryIdentity(actor, "projects.grants.delete:"+projectID+":"+grantID, key)); err != nil {
		return false, err
	}
	project, err := getProject(ctx, tx, projectID)
	if err != nil {
		return false, err
	}
	bindings, err := effectiveBindings(ctx, tx, actor)
	if err != nil {
		return false, err
	}
	projectTarget, _ := resolveAccessTarget(ctx, tx, domain.AccessTarget{Type: "project", ID: project.ID})
	if !accesspolicy.HasPermission(bindings, projectTarget, domain.PermissionResourcesRead) {
		return false, base.ErrNotFound
	}
	idemScope := "projects.grants.delete:" + projectID + ":" + grantID
	if old, ok, findErr := findIdem(ctx, tx, actor, idemScope, key); findErr != nil {
		return false, findErr
	} else if ok {
		if old.fingerprint != fingerprint {
			return false, base.ErrIdempotencyConflict
		}
		return true, tx.Commit(ctx)
	}
	grant, err := scanAccessGrant(tx.QueryRow(ctx, `SELECT id,COALESCE(subject_user_id::text,''),COALESCE(subject_team_id::text,''),role,scope_type,scope_id,permissions,source,created_by,created_at FROM access_grants WHERE id=$1 FOR UPDATE`, grantID))
	if err != nil {
		return false, err
	}
	target, err := scopeTargetForProject(ctx, tx, project, grant.ScopeType, grant.ScopeID)
	if err != nil {
		return false, err
	}
	if grant.Source != "explicit" || !accesspolicy.CanManageGrant(bindings, target, grant.Role) {
		return false, base.ErrForbidden
	}
	if _, err = tx.Exec(ctx, `DELETE FROM access_grants WHERE id=$1`, grant.ID); err != nil {
		return false, err
	}
	if err = audit(ctx, tx, actor, "access-grant.delete", "access-grant", grant.ID, requestID, map[string]any{"projectId": project.ID, "subjectUserId": grant.SubjectUserID, "subjectTeamId": grant.SubjectTeamID, "role": grant.Role, "scopeType": grant.ScopeType, "scopeId": grant.ScopeID}); err != nil {
		return false, err
	}
	if err = putIdem(ctx, tx, actor, idemScope, key, fingerprint, "access-grant", grant.ID, nil); err != nil {
		return false, classify(err)
	}
	if err = invalidateGrantSubjects(ctx, tx, grant); err != nil {
		return false, err
	}
	return false, tx.Commit(ctx)
}

func invalidateGrantSubjects(ctx context.Context, tx pgx.Tx, grant domain.AccessGrant) error {
	users := map[string]struct{}{}
	if grant.SubjectUserID != "" {
		users[grant.SubjectUserID] = struct{}{}
	} else {
		rows, err := tx.Query(ctx, `SELECT user_id::text FROM team_memberships WHERE team_id=$1`, grant.SubjectTeamID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var userID string
			if err = rows.Scan(&userID); err != nil {
				return err
			}
			users[userID] = struct{}{}
		}
		if err = rows.Err(); err != nil {
			return err
		}
	}
	return invalidateUsers(ctx, tx, users)
}
