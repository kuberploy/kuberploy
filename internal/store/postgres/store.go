package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/certificates"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/secrets"
	base "github.com/kuberploy/kuberploy/internal/store"
)

type certificateReferenceResolver interface {
	ResolveCertificateReferencesTx(context.Context, pgx.Tx, secrets.Scope, []certificates.ReferenceSelection, time.Time) (certificates.ReferencePlan, error)
}

type Store struct {
	pool                  *pgxpool.Pool
	certificateReferences certificateReferenceResolver
}

func userDisplayName(user domain.User) string {
	return user.DisplayName
}

func normalizeCredential(value string) string { return strings.ToLower(strings.TrimSpace(value)) }

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (s *Store) ConfigureCertificateReferences(resolver certificateReferenceResolver) error {
	if s == nil || s.pool == nil || resolver == nil || s.certificateReferences != nil {
		return base.ErrPreconditionFailed
	}
	s.certificateReferences = resolver
	return nil
}

const (
	startupPingTimeout        = 30 * time.Second
	startupPingAttemptTimeout = 3 * time.Second
	startupPingBackoff        = 500 * time.Millisecond
)

func advisoryIdentity(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = hash.Write([]byte(part))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}
	config.ConnConfig.RuntimeParams["application_name"] = "kuberploy"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if err = pingPostgresAtStartup(ctx, startupPingTimeout, startupPingAttemptTimeout, startupPingBackoff, pool.Ping); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if err = VerifySchema(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

func pingPostgresAtStartup(ctx context.Context, timeout, attemptTimeout, backoff time.Duration, ping func(context.Context) error) error {
	retryCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := retryCtx.Err(); err != nil {
		return err
	}

	var lastErr error
	for {
		attemptCtx, cancelAttempt := context.WithTimeout(retryCtx, attemptTimeout)
		err := ping(attemptCtx)
		cancelAttempt()
		if err == nil {
			return nil
		}
		lastErr = err
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if !isRetryablePostgresStartupError(err) {
			return err
		}

		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-retryCtx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return fmt.Errorf("startup retry timed out after %s (last error: %v): %w", timeout, lastErr, retryCtx.Err())
		}
	}
}

func isRetryablePostgresStartupError(err error) bool {
	var postgresErr *pgconn.PgError
	if errors.As(err, &postgresErr) {
		return postgresErr.Code == "57P03"
	}
	if pgconn.Timeout(err) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ETIMEDOUT) || errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) {
		return true
	}
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && (dnsErr.IsTimeout || dnsErr.IsTemporary)
}

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }
func (s *Store) Close()                         { s.pool.Close() }

func (s *Store) BootstrapAdmin(ctx context.Context, user domain.User, passwordHash string, sessionHash []byte, expires time.Time) error {
	if strings.TrimSpace(user.Email) == "" || passwordHash == "" {
		return base.ErrConflict
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var consumed *time.Time
	if err = tx.QueryRow(ctx, `SELECT consumed_at FROM bootstrap_state WHERE singleton=true FOR UPDATE`).Scan(&consumed); err != nil {
		return err
	}
	if consumed != nil {
		return base.ErrBootstrapConsumed
	}
	_, err = tx.Exec(ctx, `INSERT INTO users(id,email,display_name,role,issuer,subject,grant_revision,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, user.ID, nullableString(user.Email), userDisplayName(user), user.Role, user.Issuer, user.Subject, user.GrantRevision, user.CreatedAt)
	if err != nil {
		return classify(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO user_password_credentials(user_id,email_normalized,password_hash) VALUES($1,$2,$3)`, user.ID, normalizeCredential(user.Email), passwordHash)
	if err != nil {
		return classify(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO access_grants(id,subject_user_id,role,scope_type,scope_id,source,created_by,created_at) VALUES($1,$2,'platform-admin','platform','platform','bootstrap',$2,$3)`, id.New(), user.ID, user.CreatedAt)
	if err != nil {
		return classify(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO sessions(token_hash,user_id,grant_revision,expires_at) VALUES($1,$2,$3,$4)`, sessionHash, user.ID, user.GrantRevision, expires)
	if err != nil {
		return classify(err)
	}
	_, err = tx.Exec(ctx, `UPDATE bootstrap_state SET consumed_at=now() WHERE singleton=true AND consumed_at IS NULL`)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) LocalCredential(ctx context.Context, email string) (domain.User, string, error) {
	var u domain.User
	var hash string
	err := s.pool.QueryRow(ctx, `SELECT u.id,COALESCE(u.email,''),u.display_name,u.role,u.issuer,u.subject,u.grant_revision,u.created_at,c.password_hash
		FROM user_password_credentials c JOIN users u ON u.id=c.user_id WHERE c.email_normalized=$1`, normalizeCredential(email)).
		Scan(&u.ID, &u.Email, &u.DisplayName, &u.Role, &u.Issuer, &u.Subject, &u.GrantRevision, &u.CreatedAt, &hash)
	return u, hash, classify(err)
}

func (s *Store) CreateLoginSession(ctx context.Context, userID, expectedHash, upgradedHash string, sessionHash []byte, expires time.Time) (domain.User, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return domain.User{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var u domain.User
	var current string
	err = tx.QueryRow(ctx, `SELECT u.id,COALESCE(u.email,''),u.display_name,u.role,u.issuer,u.subject,u.grant_revision,u.created_at,c.password_hash
		FROM user_password_credentials c JOIN users u ON u.id=c.user_id WHERE u.id=$1 FOR UPDATE`, userID).
		Scan(&u.ID, &u.Email, &u.DisplayName, &u.Role, &u.Issuer, &u.Subject, &u.GrantRevision, &u.CreatedAt, &current)
	if err != nil || current != expectedHash || len(sessionHash) != 32 || !expires.After(time.Now()) {
		return domain.User{}, base.ErrNotFound
	}
	if upgradedHash != "" {
		if _, err = tx.Exec(ctx, `UPDATE user_password_credentials SET password_hash=$2,updated_at=now() WHERE user_id=$1 AND password_hash=$3`, userID, upgradedHash, expectedHash); err != nil {
			return domain.User{}, err
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO sessions(token_hash,user_id,grant_revision,expires_at) VALUES($1,$2,$3,$4)`, sessionHash, u.ID, u.GrantRevision, expires); err != nil {
		return domain.User{}, classify(err)
	}
	return u, tx.Commit(ctx)
}

func (s *Store) UserBySession(ctx context.Context, tokenHash []byte, now time.Time) (domain.User, error) {
	var u domain.User
	err := s.pool.QueryRow(ctx, `SELECT u.id,COALESCE(u.email,''),u.display_name,u.role,u.issuer,u.subject,u.grant_revision,u.created_at FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=$1 AND s.expires_at>$2 AND s.grant_revision=u.grant_revision`, tokenHash, now).
		Scan(&u.ID, &u.Email, &u.DisplayName, &u.Role, &u.Issuer, &u.Subject, &u.GrantRevision, &u.CreatedAt)
	return u, classify(err)
}
func (s *Store) RevokeSession(ctx context.Context, tokenHash []byte) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash=$1`, tokenHash)
	return err
}

type idem struct {
	fingerprint, resourceType, resourceID string
	operationID                           *string
}

func findIdem(ctx context.Context, tx pgx.Tx, actor, scope, key string) (idem, bool, error) {
	var x idem
	err := tx.QueryRow(ctx, `SELECT request_digest,resource_type,resource_id,operation_id FROM mutation_receipts WHERE actor_id=$1 AND receipt_kind='resource' AND namespace=$2 AND scope_key='global' AND idempotency_key=$3`, actor, scope, key).
		Scan(&x.fingerprint, &x.resourceType, &x.resourceID, &x.operationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return idem{}, false, nil
	}
	return x, err == nil, err
}

func putIdem(ctx context.Context, tx pgx.Tx, actor, scope, key, fingerprint, typ, resourceID string, operationID *string) error {
	_, err := tx.Exec(ctx, `INSERT INTO mutation_receipts(actor_id,receipt_kind,namespace,scope_key,idempotency_key,request_digest,resource_type,resource_id,operation_id) VALUES($1,'resource',$2,'global',$3,$4,$5,$6,$7)`, actor, scope, key, fingerprint, typ, resourceID, operationID)
	return err
}

func audit(ctx context.Context, tx pgx.Tx, actor, action, typ, target, requestID string, detail any) error {
	b, _ := json.Marshal(detail)
	_, err := tx.Exec(ctx, `INSERT INTO audit_events(id,actor_id,action,target_type,target_id,request_id,detail) VALUES($1,$2,$3,$4,$5,$6,$7)`, id.New(), actor, action, typ, target, requestID, b)
	return err
}

func (s *Store) CreateProject(ctx context.Context, actor, key, fingerprint string, in domain.CreateProject) (base.Result[domain.Project], error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return base.Result[domain.Project]{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	target := domain.AccessTarget{Type: "platform", ID: "platform"}
	if in.TeamID != "" {
		target = domain.AccessTarget{Type: "team", ID: in.TeamID}
	}
	if err = authorizeWith(ctx, tx, actor, domain.PermissionResourcesWrite, target); err != nil {
		return base.Result[domain.Project]{}, err
	}
	if old, ok, err := findIdem(ctx, tx, actor, "projects.create", key); err != nil {
		return base.Result[domain.Project]{}, err
	} else if ok {
		if old.fingerprint != fingerprint {
			return base.Result[domain.Project]{}, base.ErrIdempotencyConflict
		}
		p, err := getProject(ctx, tx, old.resourceID)
		return base.Result[domain.Project]{Value: p, Replay: true}, err
	}
	p := domain.Project{ID: id.New(), Name: in.Name, Slug: in.Slug, TeamID: in.TeamID, CreatedAt: time.Now().UTC()}
	_, err = tx.Exec(ctx, `INSERT INTO projects(id,name,slug,team_id,created_at) VALUES($1,$2,$3,NULLIF($4,'')::uuid,$5)`, p.ID, p.Name, p.Slug, p.TeamID, p.CreatedAt)
	if err != nil {
		return base.Result[domain.Project]{}, classify(err)
	}
	if err = audit(ctx, tx, actor, "project.create", "project", p.ID, "", p); err != nil {
		return base.Result[domain.Project]{}, err
	}
	if err = putIdem(ctx, tx, actor, "projects.create", key, fingerprint, "project", p.ID, nil); err != nil {
		return base.Result[domain.Project]{}, classify(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return base.Result[domain.Project]{}, err
	}
	return base.Result[domain.Project]{Value: p}, nil
}

func getProject(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id string) (domain.Project, error) {
	var p domain.Project
	err := q.QueryRow(ctx, `SELECT id,name,slug,COALESCE(team_id::text,''),created_at FROM projects WHERE id=$1`, id).Scan(&p.ID, &p.Name, &p.Slug, &p.TeamID, &p.CreatedAt)
	return p, classify(err)
}
func (s *Store) GetProject(ctx context.Context, id string) (domain.Project, error) {
	return getProject(ctx, s.pool, id)
}
func (s *Store) ListProjects(ctx context.Context) ([]domain.Project, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,name,slug,COALESCE(team_id::text,''),created_at FROM projects ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Project
	for rows.Next() {
		var p domain.Project
		if err = rows.Scan(&p.ID, &p.Name, &p.Slug, &p.TeamID, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) CreateEnvironment(ctx context.Context, actor, key, fingerprint string, in domain.CreateEnvironment) (base.Result[domain.Environment], error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return base.Result[domain.Environment]{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = authorizeWith(ctx, tx, actor, domain.PermissionResourcesWrite, domain.AccessTarget{Type: "project", ID: in.ProjectID}); err != nil {
		return base.Result[domain.Environment]{}, err
	}
	var project domain.Project
	if err = tx.QueryRow(ctx, `SELECT id,name,slug,COALESCE(team_id::text,''),created_at FROM projects WHERE id=$1`, in.ProjectID).Scan(&project.ID, &project.Name, &project.Slug, &project.TeamID, &project.CreatedAt); err != nil {
		return base.Result[domain.Environment]{}, classify(err)
	}
	in.Namespace, in.ArgoProject = domain.DeriveEnvironmentDestination(project, in.Slug)
	if in.ProtectionPolicy == "" {
		in.ProtectionPolicy = domain.EnvironmentProtected
	}
	if in.ProtectionPolicy != domain.EnvironmentDevelopment && in.ProtectionPolicy != domain.EnvironmentProtected {
		return base.Result[domain.Environment]{}, base.ErrConflict
	}
	if old, ok, err := findIdem(ctx, tx, actor, "environments.create", key); err != nil {
		return base.Result[domain.Environment]{}, err
	} else if ok {
		if old.fingerprint != fingerprint {
			return base.Result[domain.Environment]{}, base.ErrIdempotencyConflict
		}
		v, e := getEnvironment(ctx, tx, old.resourceID)
		return base.Result[domain.Environment]{Value: v, Replay: true}, e
	}
	e := domain.Environment{ID: id.New(), ProjectID: in.ProjectID, Name: in.Name, Slug: in.Slug, Namespace: in.Namespace, ArgoProject: in.ArgoProject, ProtectionPolicy: in.ProtectionPolicy, CreatedAt: time.Now().UTC()}
	_, err = tx.Exec(ctx, `INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,protection_policy,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, e.ID, e.ProjectID, e.Name, e.Slug, e.Namespace, e.ArgoProject, e.ProtectionPolicy, e.CreatedAt)
	if err != nil {
		return base.Result[domain.Environment]{}, classify(err)
	}
	if err = audit(ctx, tx, actor, "environment.create", "environment", e.ID, "", e); err != nil {
		return base.Result[domain.Environment]{}, err
	}
	if err = putIdem(ctx, tx, actor, "environments.create", key, fingerprint, "environment", e.ID, nil); err != nil {
		return base.Result[domain.Environment]{}, classify(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return base.Result[domain.Environment]{}, err
	}
	return base.Result[domain.Environment]{Value: e}, nil
}
func getEnvironment(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id string) (domain.Environment, error) {
	var e domain.Environment
	err := q.QueryRow(ctx, `SELECT id,project_id,name,slug,namespace,argo_project,protection_policy,created_at FROM environments WHERE id=$1`, id).Scan(&e.ID, &e.ProjectID, &e.Name, &e.Slug, &e.Namespace, &e.ArgoProject, &e.ProtectionPolicy, &e.CreatedAt)
	return e, classify(err)
}
func (s *Store) GetEnvironment(ctx context.Context, id string) (domain.Environment, error) {
	return getEnvironment(ctx, s.pool, id)
}
func (s *Store) ListEnvironments(ctx context.Context) ([]domain.Environment, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,project_id,name,slug,namespace,argo_project,protection_policy,created_at FROM environments ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Environment
	for rows.Next() {
		var e domain.Environment
		if err = rows.Scan(&e.ID, &e.ProjectID, &e.Name, &e.Slug, &e.Namespace, &e.ArgoProject, &e.ProtectionPolicy, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) CreateApplication(ctx context.Context, actor, key, fingerprint string, in domain.CreateApplication) (base.Result[domain.Application], error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return base.Result[domain.Application]{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = authorizeWith(ctx, tx, actor, domain.PermissionResourcesWrite, domain.AccessTarget{Type: "project", ID: in.ProjectID}); err != nil {
		return base.Result[domain.Application]{}, err
	}
	if old, ok, err := findIdem(ctx, tx, actor, "applications.create", key); err != nil {
		return base.Result[domain.Application]{}, err
	} else if ok {
		if old.fingerprint != fingerprint {
			return base.Result[domain.Application]{}, base.ErrIdempotencyConflict
		}
		v, e := getApplication(ctx, tx, old.resourceID)
		return base.Result[domain.Application]{Value: v, Replay: true}, e
	}
	a := domain.Application{ID: id.New(), ProjectID: in.ProjectID, Name: in.Name, Slug: in.Slug, CreatedAt: time.Now().UTC()}
	_, err = tx.Exec(ctx, `INSERT INTO applications(id,project_id,name,slug,created_at)VALUES($1,$2,$3,$4,$5)`, a.ID, a.ProjectID, a.Name, a.Slug, a.CreatedAt)
	if err != nil {
		return base.Result[domain.Application]{}, classify(err)
	}
	if err = audit(ctx, tx, actor, "application.create", "application", a.ID, "", a); err != nil {
		return base.Result[domain.Application]{}, err
	}
	if err = putIdem(ctx, tx, actor, "applications.create", key, fingerprint, "application", a.ID, nil); err != nil {
		return base.Result[domain.Application]{}, classify(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return base.Result[domain.Application]{}, err
	}
	return base.Result[domain.Application]{Value: a}, nil
}
func getApplication(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id string) (domain.Application, error) {
	var a domain.Application
	err := q.QueryRow(ctx, `SELECT id,project_id,name,slug,created_at FROM applications WHERE id=$1`, id).Scan(&a.ID, &a.ProjectID, &a.Name, &a.Slug, &a.CreatedAt)
	return a, classify(err)
}
func (s *Store) GetApplication(ctx context.Context, id string) (domain.Application, error) {
	return getApplication(ctx, s.pool, id)
}
func (s *Store) ListApplications(ctx context.Context) ([]domain.Application, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,project_id,name,slug,created_at FROM applications ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Application
	for rows.Next() {
		var a domain.Application
		if err = rows.Scan(&a.ID, &a.ProjectID, &a.Name, &a.Slug, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func classify(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return base.ErrNotFound
	}
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) {
		if pgerr.Code == "23505" || pgerr.Code == "23503" || pgerr.Code == "23514" || pgerr.Code == "23001" {
			return fmt.Errorf("%w: %s", base.ErrConflict, pgerr.ConstraintName)
		}
	}
	return err
}
