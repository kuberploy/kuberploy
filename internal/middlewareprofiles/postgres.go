package middlewareprofiles

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
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
	e := tx.QueryRow(ctx, `SELECT request_digest,profile_id::text,result_revision FROM mutation_receipts WHERE actor_id=$1 AND receipt_kind='configuration-profile' AND namespace='middleware' AND scope_key='global' AND idempotency_key=$2`, c.ActorID, c.IdempotencyKey).Scan(&old, &pid, &rev)
	if errors.Is(e, pgx.ErrNoRows) {
		return MutationResult{}, false, nil
	}
	if e != nil {
		return MutationResult{}, true, e
	}
	if old != d {
		return MutationResult{}, true, ErrConflict
	}
	p, v, e := loadRevision(ctx, tx, Ref{ProfileID: pid, Revision: rev})
	return MutationResult{p, v, true}, true, e
}

type qrow interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func loadRevision(ctx context.Context, q qrow, ref Ref) (Profile, Revision, error) {
	var p Profile
	var v Revision
	var cloned Ref
	var raw []byte
	e := q.QueryRow(ctx, `SELECT p.id::text,p.name,p.lifecycle,p.current_revision,p.created_by::text,p.created_at,
		COALESCE(p.deactivated_by::text,''),p.deactivated_at,r.spec,r.spec_digest,r.assignments_digest,r.created_by::text,r.created_at,
		COALESCE(r.cloned_from_profile_id::text,''),COALESCE(r.cloned_from_revision,0),COALESCE(source.spec_digest,''),COALESCE(source.assignments_digest,'')
		FROM configuration_profiles p
		JOIN configuration_profile_revisions r ON r.profile_id=p.id AND r.revision=$2
		LEFT JOIN configuration_profile_revisions source ON source.profile_id=r.cloned_from_profile_id AND source.revision=r.cloned_from_revision AND source.profile_kind='middleware'
		WHERE p.id=$1 AND p.kind='middleware' FOR SHARE OF p,r`, ref.ProfileID, ref.Revision).Scan(
		&p.ID, &p.Name, &p.Lifecycle, &p.CurrentRevision, &p.CreatedBy, &p.CreatedAt, &p.DeactivatedBy, &p.DeactivatedAt,
		&raw, &v.SpecDigest, &v.AssignmentsDigest, &v.CreatedBy, &v.CreatedAt,
		&cloned.ProfileID, &cloned.Revision, &cloned.SpecDigest, &cloned.AssignmentsDigest,
	)
	if e != nil {
		return p, v, pgerr(e)
	}
	v.ProfileID = p.ID
	v.Revision = ref.Revision
	if cloned.ProfileID != "" {
		if validateRef(cloned) != nil || cloned.SpecDigest == "" || cloned.AssignmentsDigest == "" {
			return Profile{}, Revision{}, ErrConflict
		}
		v.ClonedFrom = &cloned
	} else if cloned.Revision != 0 || cloned.SpecDigest != "" || cloned.AssignmentsDigest != "" {
		return Profile{}, Revision{}, ErrConflict
	}
	if json.Unmarshal(raw, &v.Spec) != nil {
		return p, v, ErrConflict
	}
	rows, e := q.(interface {
		Query(context.Context, string, ...any) (pgx.Rows, error)
	}).Query(ctx, `SELECT scope_type,scope_id::text FROM configuration_profile_assignments WHERE profile_id=$1 AND revision=$2 AND profile_kind='middleware' ORDER BY ordinal`, ref.ProfileID, ref.Revision)
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
func (s *PostgresStore) mutate(ctx context.Context, c Command, action, name string, ref Ref, spec Spec, a []Assignment, clonedFrom *Ref) (MutationResult, error) {
	if validateCommand(c) != nil || (action == "create" || action == "clone") && !dnsLabelRE.MatchString(name) {
		return MutationResult{}, ErrInvalid
	}
	rev := ref.Revision
	if action == "create" {
		rev = 1
	}
	d, e := commandDigest(action, ref.ProfileID, name, rev, spec, a, clonedFrom)
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
	if action == "create" || action == "clone" {
		pid = id.New()
		_, e = tx.Exec(ctx, `INSERT INTO configuration_profiles(id,kind,name,lifecycle,current_revision,created_by,created_at) VALUES($1,'middleware',$2,'active',1,$3,$4)`, pid, name, c.ActorID, c.Now.UTC())
	} else {
		var lifecycle Lifecycle
		var current int64
		e = tx.QueryRow(ctx, `SELECT lifecycle,current_revision FROM configuration_profiles WHERE id=$1 AND kind='middleware' FOR UPDATE`, pid).Scan(&lifecycle, &current)
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
	var clonedProfile any
	var clonedRevision any
	if clonedFrom != nil {
		clonedProfile, clonedRevision = clonedFrom.ProfileID, clonedFrom.Revision
	}
	_, e = tx.Exec(ctx, `INSERT INTO configuration_profile_revisions(profile_id,revision,profile_kind,spec,spec_digest,assignments_digest,created_by,created_at,cloned_from_profile_id,cloned_from_revision) VALUES($1,$2,'middleware',$3,$4,$5,$6,$7,$8,$9)`, pid, resultRev, raw, sd, ad, c.ActorID, c.Now.UTC(), clonedProfile, clonedRevision)
	if e != nil {
		return MutationResult{}, pgerr(e)
	}
	for i, x := range a {
		_, e = tx.Exec(ctx, `INSERT INTO configuration_profile_assignments(profile_id,revision,profile_kind,ordinal,scope_type,scope_id) VALUES($1,$2,'middleware',$3,$4,$5)`, pid, resultRev, i, x.Scope, x.ID)
		if e != nil {
			return MutationResult{}, pgerr(e)
		}
	}
	if action == "revise" {
		_, e = tx.Exec(ctx, `UPDATE configuration_profiles SET current_revision=$2 WHERE id=$1 AND kind='middleware'`, pid, resultRev)
		if e != nil {
			return MutationResult{}, pgerr(e)
		}
	}
	_, e = tx.Exec(ctx, `INSERT INTO mutation_receipts(actor_id,receipt_kind,namespace,scope_key,idempotency_key,action,request_digest,profile_id,result_revision,request_id,created_at) VALUES($1,'configuration-profile','middleware','global',$2,$3,$4,$5,$6,$7,$8)`, c.ActorID, c.IdempotencyKey, action, d, pid, resultRev, c.RequestID, c.Now.UTC())
	if e != nil {
		return MutationResult{}, pgerr(e)
	}
	_, e = tx.Exec(ctx, `INSERT INTO audit_events(id,actor_id,action,target_type,target_id,request_id,detail,created_at)
		VALUES($1,$2,$3,'middleware-profile',$4,$5,jsonb_build_object(
			'revision',$6::bigint,'idempotencyKey',$7::text,'specDigest',$8::text,'assignmentsDigest',$9::text),$10)`,
		id.New(), c.ActorID, "middleware-profile."+action, pid, c.RequestID, resultRev, c.IdempotencyKey, sd, ad, c.Now.UTC())
	if e != nil {
		return MutationResult{}, pgerr(e)
	}
	if e = tx.Commit(ctx); e != nil {
		return MutationResult{}, pgerr(e)
	}
	p, v, e := s.Revision(ctx, Ref{ProfileID: pid, Revision: resultRev})
	return MutationResult{p, v, false}, e
}
func (s *PostgresStore) Create(ctx context.Context, c Command, n string, sp Spec, a []Assignment) (MutationResult, error) {
	return s.mutate(ctx, c, "create", n, Ref{}, sp, a, nil)
}
func (s *PostgresStore) Revise(ctx context.Context, c Command, r Ref, sp Spec, a []Assignment) (MutationResult, error) {
	return s.mutate(ctx, c, "revise", "", r, sp, a, nil)
}
func (s *PostgresStore) Clone(ctx context.Context, c Command, source Ref, name string, assignments []Assignment) (MutationResult, error) {
	if validateRef(source) != nil {
		return MutationResult{}, ErrInvalid
	}
	_, revision, err := s.Revision(ctx, source)
	if err != nil {
		return MutationResult{}, err
	}
	exact := Ref{ProfileID: source.ProfileID, Revision: source.Revision, SpecDigest: revision.SpecDigest, AssignmentsDigest: revision.AssignmentsDigest}
	return s.mutate(ctx, c, "clone", name, Ref{}, revision.Spec, assignments, &exact)
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
	var referenced bool
	if e = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM middleware_profile_references WHERE profile_id=$1)`, p.ID).Scan(&referenced); e != nil {
		return MutationResult{}, e
	}
	if referenced {
		return MutationResult{}, ErrReferenced
	}
	_, e = tx.Exec(ctx, `UPDATE configuration_profiles SET lifecycle='deactivated',deactivated_by=$2,deactivated_at=$3 WHERE id=$1 AND kind='middleware'`, p.ID, c.ActorID, c.Now.UTC())
	if e != nil {
		return MutationResult{}, pgerr(e)
	}
	_, e = tx.Exec(ctx, `INSERT INTO mutation_receipts(actor_id,receipt_kind,namespace,scope_key,idempotency_key,action,request_digest,profile_id,result_revision,request_id,created_at) VALUES($1,'configuration-profile','middleware','global',$2,'deactivate',$3,$4,$5,$6,$7)`, c.ActorID, c.IdempotencyKey, d, p.ID, v.Revision, c.RequestID, c.Now.UTC())
	if e != nil {
		return MutationResult{}, pgerr(e)
	}
	_, e = tx.Exec(ctx, `INSERT INTO audit_events(id,actor_id,action,target_type,target_id,request_id,detail,created_at)
		VALUES($1,$2,'middleware-profile.deactivate','middleware-profile',$3,$4,jsonb_build_object(
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
	e := s.pool.QueryRow(ctx, `SELECT current_revision FROM configuration_profiles WHERE id=$1 AND kind='middleware'`, id).Scan(&r)
	if e != nil {
		return Profile{}, Revision{}, pgerr(e)
	}
	return s.Revision(ctx, Ref{ProfileID: id, Revision: r})
}

func (s *PostgresStore) Catalog(ctx context.Context, limit int) ([]Entry, error) {
	if limit < 1 || limit > 500 {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text,current_revision FROM configuration_profiles WHERE kind='middleware' ORDER BY name,id LIMIT $1`, limit)
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
		FROM configuration_profiles p
		WHERE p.kind='middleware' AND p.lifecycle='active' AND EXISTS (
			SELECT 1 FROM configuration_profile_assignments a
			WHERE a.profile_id=p.id AND a.revision=p.current_revision AND (
				(a.scope_type='project' AND a.scope_id::text=$1) OR
				(a.scope_type='environment' AND a.scope_id::text=$2) OR
				(a.scope_type='application' AND a.scope_id::text=$3)))
		ORDER BY p.name,p.id LIMIT $4`, target.ProjectID, target.EnvironmentID, target.ApplicationID, limit)
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

func (s *PostgresStore) References(ctx context.Context, profileID string, limit int) ([]Reference, error) {
	if !uuidRE.MatchString(profileID) || limit < 1 || limit > 500 {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT profile_id::text,revision,application_id::text,environment_id::text,git_path,logical_name FROM middleware_profile_references WHERE profile_id=$1 ORDER BY git_path,logical_name LIMIT $2`, profileID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Reference{}
	for rows.Next() {
		var item Reference
		if err = rows.Scan(&item.ProfileID, &item.Revision, &item.ApplicationID, &item.EnvironmentID, &item.GitPath, &item.LogicalName); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *PostgresStore) ReplaceReferences(ctx context.Context, applicationID, environmentID, gitPath string, refs []Reference) error {
	if !uuidRE.MatchString(applicationID) || !uuidRE.MatchString(environmentID) || gitPath == "" || len(gitPath) > 1024 || len(refs) > 32 {
		return ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `DELETE FROM middleware_profile_references WHERE git_path=$1`, gitPath); err != nil {
		return err
	}
	seen := map[string]struct{}{}
	for _, ref := range refs {
		if !uuidRE.MatchString(ref.ProfileID) || ref.Revision < 1 || ref.ApplicationID != applicationID || ref.EnvironmentID != environmentID || ref.GitPath != gitPath || !dnsLabelRE.MatchString(ref.LogicalName) {
			return ErrInvalid
		}
		key := ref.ProfileID + "\x00" + ref.LogicalName
		if _, duplicate := seen[key]; duplicate {
			return ErrInvalid
		}
		seen[key] = struct{}{}
		var lifecycle Lifecycle
		var current int64
		if err = tx.QueryRow(ctx, `SELECT lifecycle,current_revision FROM configuration_profiles WHERE id=$1 AND kind='middleware' FOR SHARE`, ref.ProfileID).Scan(&lifecycle, &current); err != nil {
			return pgerr(err)
		}
		if lifecycle != Active || current != ref.Revision {
			return ErrConflict
		}
		if _, err = tx.Exec(ctx, `INSERT INTO middleware_profile_references(profile_id,revision,application_id,environment_id,git_path,logical_name,updated_at) VALUES($1,$2,$3,$4,$5,$6,now())`, ref.ProfileID, ref.Revision, applicationID, environmentID, gitPath, ref.LogicalName); err != nil {
			return pgerr(err)
		}
	}
	return tx.Commit(ctx)
}

var _ Store = (*PostgresStore)(nil)
var _ ReferenceWriter = (*PostgresStore)(nil)

// ValidateMaterializedTx re-resolves every reusable profile and atomically
// records the active Git document references. Inline definitions are closed by
// ValidateSpec but create no reusable-profile authority.
func (s *PostgresStore) ValidateMaterializedTx(ctx context.Context, tx pgx.Tx, definitions []MaterializedDefinition, target Target, applicationID, environmentID, gitPath string, now time.Time) (bool, error) {
	if s == nil || tx == nil || validateTarget(target) != nil || target.ApplicationID != applicationID || target.EnvironmentID != environmentID || gitPath == "" || len(gitPath) > 1024 || len(definitions) > 32 || now.IsZero() {
		return false, ErrInvalid
	}
	var destinationMatches bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM applications a
		JOIN environments e ON e.id=$2 AND e.project_id=a.project_id
		JOIN git_repository_bindings b ON b.project_id=e.project_id AND b.environment_id=e.id AND b.kind='environment'
		WHERE a.id=$1 AND a.project_id=$3 AND $4=b.path_prefix || '/apps/' || a.id::text || '/app.yaml')`, applicationID, environmentID, target.ProjectID, gitPath).Scan(&destinationMatches); err != nil {
		return false, err
	}
	if !destinationMatches {
		return false, ErrInvalid
	}
	refs := []Reference{}
	for _, definition := range definitions {
		if ValidateDefinition(definition.Name, definition.Spec) != nil {
			return false, nil
		}
		if definition.ProfileRef == nil {
			continue
		}
		ref := *definition.ProfileRef
		if validateRef(ref) != nil {
			return false, nil
		}
		profile, revision, err := loadRevision(ctx, tx, ref)
		if err != nil {
			if errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) {
				return false, nil
			}
			return false, err
		}
		if profile.Lifecycle != Active || profile.CurrentRevision != ref.Revision || revision.SpecDigest != ref.SpecDigest || revision.AssignmentsDigest != ref.AssignmentsDigest {
			return false, nil
		}
		allowed := false
		for _, assignment := range revision.Assignments {
			allowed = allowed || assigned(assignment, target)
		}
		left, _ := json.Marshal(cloneSpec(definition.Spec))
		right, _ := json.Marshal(cloneSpec(revision.Spec))
		if !allowed || !bytes.Equal(left, right) {
			return false, nil
		}
		refs = append(refs, Reference{ProfileID: ref.ProfileID, Revision: ref.Revision, ApplicationID: applicationID, EnvironmentID: environmentID, GitPath: gitPath, LogicalName: definition.Name})
	}
	if _, err := tx.Exec(ctx, `DELETE FROM middleware_profile_references WHERE git_path=$1`, gitPath); err != nil {
		return false, err
	}
	for _, ref := range refs {
		if _, err := tx.Exec(ctx, `INSERT INTO middleware_profile_references(profile_id,revision,application_id,environment_id,git_path,logical_name,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, ref.ProfileID, ref.Revision, ref.ApplicationID, ref.EnvironmentID, ref.GitPath, ref.LogicalName, now.UTC()); err != nil {
			return false, pgerr(err)
		}
	}
	return true, nil
}

func (s *PostgresStore) ReconcileDeletedTx(ctx context.Context, tx pgx.Tx, gitPath string) error {
	if s == nil || tx == nil || gitPath == "" {
		return ErrInvalid
	}
	_, err := tx.Exec(ctx, `DELETE FROM middleware_profile_references WHERE git_path=$1`, gitPath)
	return err
}
