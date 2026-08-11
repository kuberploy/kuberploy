package scheduling

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/id"
)

type PostgresStore struct {
	pool *pgxpool.Pool
	owns bool
}

func NewPostgresStore(p *pgxpool.Pool) (*PostgresStore, error) {
	if p == nil {
		return nil, ErrInvalid
	}
	return &PostgresStore{pool: p}, nil
}

func OpenPostgresStore(ctx context.Context, databaseURL, applicationName string) (*PostgresStore, error) {
	if databaseURL == "" || applicationName == "" {
		return nil, ErrInvalid
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, ErrInvalid
	}
	config.ConnConfig.RuntimeParams["application_name"] = applicationName
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &PostgresStore{pool: pool, owns: true}, nil
}

func (s *PostgresStore) Close() {
	if s != nil && s.owns && s.pool != nil {
		s.pool.Close()
	}
}
func pgerr(e error) error {
	if errors.Is(e, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var x *pgconn.PgError
	if errors.As(e, &x) && (x.Code == "23505" || x.Code == "40001" || x.Code == "23514") {
		return ErrConflict
	}
	return e
}
func (s *PostgresStore) replay(ctx context.Context, tx pgx.Tx, c Command, d string) (MutationResult, bool, error) {
	var old, pid string
	var rev int64
	e := tx.QueryRow(ctx, `SELECT request_digest,profile_id::text,result_revision FROM scheduling_profile_commands WHERE actor_id=$1 AND idempotency_key=$2`, c.ActorID, c.IdempotencyKey).Scan(&old, &pid, &rev)
	if errors.Is(e, pgx.ErrNoRows) {
		return MutationResult{}, false, nil
	}
	if e != nil {
		return MutationResult{}, true, e
	}
	if old != d {
		return MutationResult{}, true, ErrConflict
	}
	p, v, e := loadRevision(ctx, tx, Ref{pid, rev})
	return MutationResult{p, v, true}, true, e
}

type qrow interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func loadRevision(ctx context.Context, q qrow, ref Ref) (Profile, Revision, error) {
	var p Profile
	var v Revision
	var raw []byte
	e := q.QueryRow(ctx, `SELECT p.id::text,p.name,p.lifecycle,p.current_revision,p.created_by::text,p.created_at,COALESCE(p.deactivated_by::text,''),p.deactivated_at,r.spec,r.spec_digest,r.assignments_digest,r.created_by::text,r.created_at FROM scheduling_profiles p JOIN scheduling_profile_revisions r ON r.profile_id=p.id AND r.revision=$2 WHERE p.id=$1 FOR SHARE OF p,r`, ref.ProfileID, ref.Revision).Scan(&p.ID, &p.Name, &p.Lifecycle, &p.CurrentRevision, &p.CreatedBy, &p.CreatedAt, &p.DeactivatedBy, &p.DeactivatedAt, &raw, &v.SpecDigest, &v.AssignmentsDigest, &v.CreatedBy, &v.CreatedAt)
	if e != nil {
		return p, v, pgerr(e)
	}
	v.ProfileID = p.ID
	v.Revision = ref.Revision
	if json.Unmarshal(raw, &v.Spec) != nil {
		return p, v, ErrConflict
	}
	rows, e := q.(interface {
		Query(context.Context, string, ...any) (pgx.Rows, error)
	}).Query(ctx, `SELECT scope_type,scope_id::text FROM scheduling_profile_assignments WHERE profile_id=$1 AND revision=$2 ORDER BY ordinal`, ref.ProfileID, ref.Revision)
	if e != nil {
		return p, v, e
	}
	defer rows.Close()
	for rows.Next() {
		var a Assignment
		if e = rows.Scan(&a.Scope, &a.ID); e != nil {
			return p, v, e
		}
		v.Assignments = append(v.Assignments, a)
	}
	if e = rows.Err(); e != nil {
		return p, v, e
	}
	_, _, specDigest, assignmentsDigest, e := canonical(v.Spec, v.Assignments)
	if e != nil || specDigest != v.SpecDigest || assignmentsDigest != v.AssignmentsDigest {
		return Profile{}, Revision{}, ErrConflict
	}
	return p, v, nil
}
func (s *PostgresStore) mutate(ctx context.Context, c Command, action, name string, ref Ref, spec Spec, a []Assignment) (MutationResult, error) {
	if validateCommand(c) != nil || (action == "create" && !safeName(name)) {
		return MutationResult{}, ErrInvalid
	}
	rev := ref.Revision
	if action == "create" {
		rev = 1
	}
	d, e := requestDigest(action, ref.ProfileID, name, rev, spec, a)
	if e != nil {
		return MutationResult{}, e
	}
	tx, e := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if e != nil {
		return MutationResult{}, e
	}
	defer tx.Rollback(context.Background())
	if r, ok, e := s.replay(ctx, tx, c, d); ok {
		return r, e
	}
	spec, a, sd, ad, _ := canonical(spec, a)
	raw, _ := json.Marshal(spec)
	pid := ref.ProfileID
	resultRev := int64(1)
	if action == "create" {
		pid = id.New()
		_, e = tx.Exec(ctx, `INSERT INTO scheduling_profiles(id,name,lifecycle,current_revision,created_by,created_at) VALUES($1,$2,'active',1,$3,$4)`, pid, name, c.ActorID, c.Now.UTC())
	} else {
		var lifecycle Lifecycle
		var current int64
		e = tx.QueryRow(ctx, `SELECT lifecycle,current_revision FROM scheduling_profiles WHERE id=$1 FOR UPDATE`, pid).Scan(&lifecycle, &current)
		if errors.Is(e, pgx.ErrNoRows) {
			return MutationResult{}, ErrNotFound
		}
		if e == nil && (lifecycle != Active) {
			return MutationResult{}, ErrInactive
		}
		if e == nil && current != ref.Revision {
			return MutationResult{}, ErrConflict
		}
		resultRev = current + 1
	}
	if e != nil {
		return MutationResult{}, pgerr(e)
	}
	_, e = tx.Exec(ctx, `INSERT INTO scheduling_profile_revisions(profile_id,revision,spec,spec_digest,assignments_digest,created_by,created_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, pid, resultRev, raw, sd, ad, c.ActorID, c.Now.UTC())
	if e != nil {
		return MutationResult{}, pgerr(e)
	}
	for i, x := range a {
		_, e = tx.Exec(ctx, `INSERT INTO scheduling_profile_assignments(profile_id,revision,ordinal,scope_type,scope_id) VALUES($1,$2,$3,$4,$5)`, pid, resultRev, i, x.Scope, x.ID)
		if e != nil {
			return MutationResult{}, pgerr(e)
		}
	}
	if action == "revise" {
		_, e = tx.Exec(ctx, `UPDATE scheduling_profiles SET current_revision=$2 WHERE id=$1`, pid, resultRev)
		if e != nil {
			return MutationResult{}, pgerr(e)
		}
	}
	_, e = tx.Exec(ctx, `INSERT INTO scheduling_profile_commands(actor_id,idempotency_key,action,request_digest,profile_id,result_revision,request_id,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, c.ActorID, c.IdempotencyKey, action, d, pid, resultRev, c.RequestID, c.Now.UTC())
	if e != nil {
		return MutationResult{}, pgerr(e)
	}
	_, e = tx.Exec(ctx, `INSERT INTO audit_events(id,actor_id,action,target_type,target_id,request_id,detail,created_at)
		VALUES($1,$2,$3,'scheduling-profile',$4,$5,jsonb_build_object(
			'revision',$6::bigint,'idempotencyKey',$7::text,'specDigest',$8::text,'assignmentsDigest',$9::text),$10)`,
		id.New(), c.ActorID, "scheduling-profile."+action, pid, c.RequestID, resultRev, c.IdempotencyKey, sd, ad, c.Now.UTC())
	if e != nil {
		return MutationResult{}, pgerr(e)
	}
	if e = tx.Commit(ctx); e != nil {
		return MutationResult{}, pgerr(e)
	}
	p, v, e := s.Revision(ctx, Ref{pid, resultRev})
	return MutationResult{p, v, false}, e
}
func (s *PostgresStore) Create(ctx context.Context, c Command, n string, sp Spec, a []Assignment) (MutationResult, error) {
	return s.mutate(ctx, c, "create", n, Ref{}, sp, a)
}
func (s *PostgresStore) Revise(ctx context.Context, c Command, r Ref, sp Spec, a []Assignment) (MutationResult, error) {
	return s.mutate(ctx, c, "revise", "", r, sp, a)
}
func (s *PostgresStore) Deactivate(ctx context.Context, c Command, r Ref) (MutationResult, error) {
	if validateCommand(c) != nil {
		return MutationResult{}, ErrInvalid
	}
	tx, e := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if e != nil {
		return MutationResult{}, e
	}
	defer tx.Rollback(context.Background())
	p, v, e := loadRevision(ctx, tx, r)
	if e != nil {
		return MutationResult{}, e
	}
	d := digest([]byte(fmt.Sprintf("%s\x00deactivate\x00%s\x00%d", Contract, r.ProfileID, r.Revision)))
	if out, ok, e := s.replay(ctx, tx, c, d); ok {
		return out, e
	}
	if p.Lifecycle != Active {
		return MutationResult{}, ErrInactive
	}
	if p.CurrentRevision != r.Revision {
		return MutationResult{}, ErrConflict
	}
	_, e = tx.Exec(ctx, `UPDATE scheduling_profiles SET lifecycle='deactivated',deactivated_by=$2,deactivated_at=$3 WHERE id=$1`, p.ID, c.ActorID, c.Now.UTC())
	if e != nil {
		return MutationResult{}, pgerr(e)
	}
	_, e = tx.Exec(ctx, `INSERT INTO scheduling_profile_commands(actor_id,idempotency_key,action,request_digest,profile_id,result_revision,request_id,created_at) VALUES($1,$2,'deactivate',$3,$4,$5,$6,$7)`, c.ActorID, c.IdempotencyKey, d, p.ID, v.Revision, c.RequestID, c.Now.UTC())
	if e != nil {
		return MutationResult{}, pgerr(e)
	}
	_, e = tx.Exec(ctx, `INSERT INTO audit_events(id,actor_id,action,target_type,target_id,request_id,detail,created_at)
		VALUES($1,$2,'scheduling-profile.deactivate','scheduling-profile',$3,$4,jsonb_build_object(
			'revision',$5::bigint,'idempotencyKey',$6::text,'specDigest',$7::text,'assignmentsDigest',$8::text),$9)`,
		id.New(), c.ActorID, p.ID, c.RequestID, v.Revision, c.IdempotencyKey, v.SpecDigest, v.AssignmentsDigest, c.Now.UTC())
	if e != nil {
		return MutationResult{}, pgerr(e)
	}
	if e = tx.Commit(ctx); e != nil {
		return MutationResult{}, pgerr(e)
	}
	p.Lifecycle = Deactivated
	p.DeactivatedBy = c.ActorID
	t := c.Now.UTC()
	p.DeactivatedAt = &t
	return MutationResult{p, v, false}, nil
}
func (s *PostgresStore) Revision(ctx context.Context, r Ref) (Profile, Revision, error) {
	return loadRevision(ctx, s.pool, r)
}
func (s *PostgresStore) Current(ctx context.Context, id string) (Profile, Revision, error) {
	var r int64
	e := s.pool.QueryRow(ctx, `SELECT current_revision FROM scheduling_profiles WHERE id=$1`, id).Scan(&r)
	if e != nil {
		return Profile{}, Revision{}, pgerr(e)
	}
	return s.Revision(ctx, Ref{id, r})
}

func (s *PostgresStore) Catalog(ctx context.Context, limit int) ([]Entry, error) {
	if limit < 1 || limit > 500 {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text,current_revision FROM scheduling_profiles ORDER BY name,id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	refs := []Ref{}
	for rows.Next() {
		var ref Ref
		if err = rows.Scan(&ref.ProfileID, &ref.Revision); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return s.entries(ctx, refs)
}

func (s *PostgresStore) Assigned(ctx context.Context, target Target, limit int) ([]Entry, error) {
	if validateTarget(target) != nil || limit < 1 || limit > 500 {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT p.id::text,p.current_revision
		FROM scheduling_profiles p
		WHERE p.lifecycle='active' AND EXISTS (
			SELECT 1 FROM scheduling_profile_assignments a
			WHERE a.profile_id=p.id AND a.revision=p.current_revision AND (
				(a.scope_type='team' AND $1<>'' AND a.scope_id::text=$1) OR
				(a.scope_type='project' AND a.scope_id::text=$2) OR
				(a.scope_type='environment' AND a.scope_id::text=$3)))
		ORDER BY p.name,p.id LIMIT $4`, target.TeamID, target.ProjectID, target.EnvironmentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	refs := []Ref{}
	for rows.Next() {
		var ref Ref
		if err = rows.Scan(&ref.ProfileID, &ref.Revision); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return s.entries(ctx, refs)
}

func (s *PostgresStore) entries(ctx context.Context, refs []Ref) ([]Entry, error) {
	items := make([]Entry, 0, len(refs))
	for _, ref := range refs {
		profile, revision, err := s.Revision(ctx, ref)
		if err != nil {
			return nil, err
		}
		items = append(items, Entry{Profile: profile, Revision: revision})
	}
	return items, nil
}

// ResolveExactTx repeats assignment, lifecycle, current-revision, digest and
// material checks inside the caller's serializable projection transaction.
// This prevents a successful HTTP-time lookup from authorizing stale direct
// Git content after an administrator revises or deactivates the profile.
func (s *PostgresStore) ResolveExactTx(ctx context.Context, tx pgx.Tx, ref Ref, target Target) (Resolution, error) {
	if s == nil || tx == nil || !uuidRE.MatchString(ref.ProfileID) || ref.Revision < 1 || validateTarget(target) != nil {
		return Resolution{}, ErrInvalid
	}
	p, v, err := loadRevision(ctx, tx, ref)
	if err != nil {
		return Resolution{}, err
	}
	if p.Lifecycle != Active {
		return Resolution{}, ErrInactive
	}
	if p.CurrentRevision != ref.Revision {
		return Resolution{}, ErrConflict
	}
	for _, assignment := range v.Assignments {
		if match(assignment, target) {
			return Resolution{Ref: ref, SpecDigest: v.SpecDigest, AssignmentsDigest: v.AssignmentsDigest, Pod: clonePod(v.Spec.Pod)}, nil
		}
	}
	return Resolution{}, ErrUnassigned
}

func (s *PostgresStore) MatchesRuntimeTx(ctx context.Context, tx pgx.Tx, runtime domain.WorkloadRuntime, target Target, applicationID string) (bool, error) {
	return MatchesRuntimeTx(ctx, tx, runtime, target, applicationID)
}

func MatchesRuntimeTx(ctx context.Context, tx pgx.Tx, runtime domain.WorkloadRuntime, target Target, applicationID string) (bool, error) {
	if runtime.SchedulingProfile == nil {
		return !HasEffectiveMaterial(runtime), nil
	}
	ref := Ref{ProfileID: runtime.SchedulingProfile.ProfileID, Revision: runtime.SchedulingProfile.Revision}
	if tx == nil || !uuidRE.MatchString(ref.ProfileID) || ref.Revision < 1 || validateTarget(target) != nil {
		return false, nil
	}
	p, v, err := loadRevision(ctx, tx, ref)
	var resolved Resolution
	if err == nil {
		switch {
		case p.Lifecycle != Active:
			err = ErrInactive
		case p.CurrentRevision != ref.Revision:
			err = ErrConflict
		default:
			for _, assignment := range v.Assignments {
				if match(assignment, target) {
					resolved = Resolution{Ref: ref, SpecDigest: v.SpecDigest, AssignmentsDigest: v.AssignmentsDigest, Pod: clonePod(v.Spec.Pod)}
					break
				}
			}
			if resolved.Ref.ProfileID == "" {
				err = ErrUnassigned
			}
		}
	}
	if err != nil {
		if errors.Is(err, ErrInvalid) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) || errors.Is(err, ErrInactive) || errors.Is(err, ErrUnassigned) {
			return false, nil
		}
		return false, err
	}
	return Matches(runtime, resolved, applicationID), nil
}
