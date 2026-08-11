package certissuers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

func NewPostgresStore(pool *pgxpool.Pool) (*PostgresStore, error) {
	if pool == nil {
		return nil, ErrInvalid
	}
	return &PostgresStore{pool: pool}, nil
}
func OpenPostgresStore(ctx context.Context, databaseURL string) (*PostgresStore, error) {
	if databaseURL == "" {
		return nil, ErrInvalid
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, ErrInvalid
	}
	config.ConnConfig.RuntimeParams["application_name"] = "kuberploy-certissuers"
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
func mapError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var p *pgconn.PgError
	if errors.As(err, &p) {
		switch p.Code {
		case "23505", "23514", "40001", "42501":
			return ErrConflict
		case "23503":
			return ErrReferenced
		}
	}
	return err
}
func (s *PostgresStore) replay(ctx context.Context, tx pgx.Tx, c Command, d string) (MutationResult, bool, error) {
	var old, pid string
	var rev int64
	err := tx.QueryRow(ctx, `SELECT request_digest,profile_id::text,result_revision FROM cert_manager_issuer_commands WHERE actor_id=$1 AND idempotency_key=$2`, c.ActorID, c.IdempotencyKey).Scan(&old, &pid, &rev)
	if errors.Is(err, pgx.ErrNoRows) {
		return MutationResult{}, false, nil
	}
	if err != nil {
		return MutationResult{}, true, err
	}
	if old != d {
		return MutationResult{}, true, ErrConflict
	}
	e, err := loadEntry(ctx, tx, pid, rev)
	return MutationResult{e.Profile, e.Revision, true}, true, mapError(err)
}

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func loadEntry(ctx context.Context, q rowQuerier, id string, rev int64) (Entry, error) {
	var e Entry
	var raw []byte
	err := q.QueryRow(ctx, `SELECT p.id::text,p.name,p.lifecycle,p.current_revision,p.created_by::text,p.created_at,COALESCE(p.deactivated_by::text,''),p.deactivated_at,r.revision,r.solver_type,r.spec,r.spec_digest,r.created_by::text,r.created_at FROM cert_manager_issuer_profiles p JOIN cert_manager_issuer_profile_revisions r ON r.profile_id=p.id AND r.revision=$2 WHERE p.id=$1`, id, rev).Scan(&e.Profile.ID, &e.Profile.Name, &e.Profile.Lifecycle, &e.Profile.CurrentRevision, &e.Profile.CreatedBy, &e.Profile.CreatedAt, &e.Profile.DeactivatedBy, &e.Profile.DeactivatedAt, &e.Revision.Revision, &e.Revision.Solver, &raw, &e.Revision.SpecDigest, &e.Revision.CreatedBy, &e.Revision.CreatedAt)
	if err != nil {
		return Entry{}, mapError(err)
	}
	e.Revision.ProfileID = e.Profile.ID
	if json.Unmarshal(raw, &e.Revision.Spec) != nil {
		return Entry{}, ErrConflict
	}
	clean, solver, digest, err := normalizeSpec(e.Revision.Spec)
	if err != nil || solver != e.Revision.Solver || digest != e.Revision.SpecDigest {
		return Entry{}, ErrConflict
	}
	e.Revision.Spec = clean
	return e, nil
}
func (s *PostgresStore) Create(ctx context.Context, c Command, name string, spec Spec) (MutationResult, error) {
	return s.mutate(ctx, c, "create", name, Ref{}, spec)
}
func (s *PostgresStore) Revise(ctx context.Context, c Command, ref Ref, spec Spec) (MutationResult, error) {
	return s.mutate(ctx, c, "revise", "", ref, spec)
}
func (s *PostgresStore) mutate(ctx context.Context, c Command, action, name string, ref Ref, spec Spec) (MutationResult, error) {
	if !validateCommand(c) || action == "create" && !dnsLabelRE.MatchString(name) || action == "revise" && (!uuidRE.MatchString(ref.ProfileID) || ref.Revision < 1) {
		return MutationResult{}, ErrInvalid
	}
	d, err := commandDigest(action, ref.ProfileID, name, ref.Revision, spec)
	if err != nil {
		return MutationResult{}, err
	}
	clean, solver, sd, _ := normalizeSpec(spec)
	raw, _ := json.Marshal(clean)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return MutationResult{}, err
	}
	defer tx.Rollback(context.Background())
	if out, ok, e := s.replay(ctx, tx, c, d); ok {
		return out, e
	}
	pid := ref.ProfileID
	rev := int64(1)
	if action == "create" {
		pid = id.New()
		_, err = tx.Exec(ctx, `INSERT INTO cert_manager_issuer_profiles(id,name,lifecycle,current_revision,created_by,created_at) VALUES($1,$2,'active',1,$3,$4)`, pid, name, c.ActorID, c.Now.UTC())
	} else {
		var lifecycle Lifecycle
		var current int64
		err = tx.QueryRow(ctx, `SELECT lifecycle,current_revision FROM cert_manager_issuer_profiles WHERE id=$1 FOR UPDATE`, pid).Scan(&lifecycle, &current)
		if err == nil && lifecycle != Active {
			return MutationResult{}, ErrInactive
		}
		if err == nil && current != ref.Revision {
			return MutationResult{}, ErrConflict
		}
		rev = current + 1
	}
	if err != nil {
		return MutationResult{}, mapError(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO cert_manager_issuer_profile_revisions(profile_id,revision,solver_type,spec,spec_digest,created_by,created_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, pid, rev, solver, raw, sd, c.ActorID, c.Now.UTC())
	if err != nil {
		return MutationResult{}, mapError(err)
	}
	if action == "revise" {
		if _, err = tx.Exec(ctx, `UPDATE cert_manager_issuer_profiles SET current_revision=$2 WHERE id=$1`, pid, rev); err != nil {
			return MutationResult{}, mapError(err)
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO cert_manager_issuer_observations(profile_id,revision,state,updated_at) VALUES($1,$2,'pending',$3)`, pid, rev, c.Now.UTC())
	if err != nil {
		return MutationResult{}, mapError(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO cert_manager_issuer_commands(actor_id,idempotency_key,action,request_digest,profile_id,result_revision,request_id,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, c.ActorID, c.IdempotencyKey, action, d, pid, rev, c.RequestID, c.Now.UTC())
	if err != nil {
		return MutationResult{}, mapError(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_events(id,actor_id,action,target_type,target_id,request_id,detail,created_at)
		VALUES($1,$2,$3,'certificate-issuer-profile',$4,$5,jsonb_build_object(
			'revision',$6::bigint,'idempotencyKey',$7::text,'specDigest',$8::text),$9)`,
		id.New(), c.ActorID, "certificate-issuer-profile."+action, pid, c.RequestID, rev, c.IdempotencyKey, sd, c.Now.UTC())
	if err != nil {
		return MutationResult{}, mapError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return MutationResult{}, mapError(err)
	}
	e, err := loadEntry(ctx, s.pool, pid, rev)
	return MutationResult{e.Profile, e.Revision, false}, err
}
func (s *PostgresStore) Deactivate(ctx context.Context, c Command, ref Ref) (MutationResult, error) {
	if !validateCommand(c) || !uuidRE.MatchString(ref.ProfileID) || ref.Revision < 1 {
		return MutationResult{}, ErrInvalid
	}
	d := digestText(fmt.Sprintf("%s\x00deactivate\x00%s\x00%d", Contract, ref.ProfileID, ref.Revision))
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return MutationResult{}, err
	}
	defer tx.Rollback(context.Background())
	if out, ok, e := s.replay(ctx, tx, c, d); ok {
		return out, e
	}
	e, err := loadEntry(ctx, tx, ref.ProfileID, ref.Revision)
	if err != nil {
		return MutationResult{}, err
	}
	if e.Profile.Lifecycle != Active {
		return MutationResult{}, ErrInactive
	}
	if e.Profile.CurrentRevision != ref.Revision {
		return MutationResult{}, ErrConflict
	}
	_, err = tx.Exec(ctx, `UPDATE cert_manager_issuer_profiles SET lifecycle='deactivated',deactivated_by=$2,deactivated_at=$3 WHERE id=$1`, ref.ProfileID, c.ActorID, c.Now.UTC())
	if err != nil {
		return MutationResult{}, mapError(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO cert_manager_issuer_commands(actor_id,idempotency_key,action,request_digest,profile_id,result_revision,request_id,created_at) VALUES($1,$2,'deactivate',$3,$4,$5,$6,$7)`, c.ActorID, c.IdempotencyKey, d, ref.ProfileID, ref.Revision, c.RequestID, c.Now.UTC())
	if err != nil {
		return MutationResult{}, mapError(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_events(id,actor_id,action,target_type,target_id,request_id,detail,created_at)
		VALUES($1,$2,'certificate-issuer-profile.deactivate','certificate-issuer-profile',$3,$4,jsonb_build_object(
			'revision',$5::bigint,'idempotencyKey',$6::text,'specDigest',$7::text),$8)`,
		id.New(), c.ActorID, ref.ProfileID, c.RequestID, ref.Revision, c.IdempotencyKey, e.Revision.SpecDigest, c.Now.UTC())
	if err != nil {
		return MutationResult{}, mapError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return MutationResult{}, mapError(err)
	}
	e.Profile.Lifecycle = Deactivated
	e.Profile.DeactivatedBy = c.ActorID
	t := c.Now.UTC()
	e.Profile.DeactivatedAt = &t
	return MutationResult{e.Profile, e.Revision, false}, nil
}
func (s *PostgresStore) Current(ctx context.Context, id string) (Entry, error) {
	var rev int64
	if err := s.pool.QueryRow(ctx, `SELECT current_revision FROM cert_manager_issuer_profiles WHERE id=$1`, id).Scan(&rev); err != nil {
		return Entry{}, mapError(err)
	}
	return loadEntry(ctx, s.pool, id, rev)
}
func (s *PostgresStore) List(ctx context.Context, limit int) ([]Entry, error) {
	if limit < 1 || limit > 500 {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text,current_revision FROM cert_manager_issuer_profiles ORDER BY name,id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var id string
		var rev int64
		if err = rows.Scan(&id, &rev); err != nil {
			return nil, err
		}
		e, e2 := loadEntry(ctx, s.pool, id, rev)
		if e2 != nil {
			return nil, e2
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
func (s *PostgresStore) PendingMaterialization(ctx context.Context, limit int) ([]Desired, error) {
	if limit < 1 || limit > 500 {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT p.id::text,p.name,p.current_revision FROM cert_manager_issuer_profiles p JOIN cert_manager_issuer_observations o ON o.profile_id=p.id AND o.revision=p.current_revision JOIN cert_manager_issuer_profile_revisions r ON r.profile_id=p.id AND r.revision=p.current_revision WHERE p.lifecycle='active' AND (o.state<>'ready' OR o.observed_spec_digest IS DISTINCT FROM r.spec_digest) ORDER BY p.name LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type x struct {
		id, name string
		rev      int64
	}
	xs := []x{}
	for rows.Next() {
		var v x
		if err = rows.Scan(&v.id, &v.name, &v.rev); err != nil {
			return nil, err
		}
		xs = append(xs, v)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	out := []Desired{}
	for _, v := range xs {
		e, e2 := loadEntry(ctx, s.pool, v.id, v.rev)
		if e2 != nil {
			return nil, e2
		}
		out = append(out, Desired{e.Profile.ID, e.Profile.Name, e.Revision.SpecDigest, e.Revision.Revision, e.Revision.Solver, e.Revision.Spec})
	}
	return out, nil
}
func (s *PostgresStore) RecordObservation(ctx context.Context, o Observation) error {
	e, err := loadEntry(ctx, s.pool, o.ProfileID, o.Revision)
	if err != nil {
		return err
	}
	if !validObservation(o, e.Revision.SpecDigest) {
		return ErrInvalid
	}
	tag, err := s.pool.Exec(ctx, `UPDATE cert_manager_issuer_observations SET state=$3,observed_spec_digest=NULLIF($4,''),observed_generation=NULLIF($5,0),reason=$6,observed_at=$7,updated_at=$8 WHERE profile_id=$1 AND revision=$2`, o.ProfileID, o.Revision, o.State, o.ObservedSpecDigest, o.ObservedGeneration, o.Reason, o.ObservedAt, o.UpdatedAt.UTC())
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}
func (s *PostgresStore) Observation(ctx context.Context, profileID string, revision int64) (Observation, error) {
	if !uuidRE.MatchString(profileID) || revision < 1 {
		return Observation{}, ErrInvalid
	}
	var o Observation
	o.ProfileID = profileID
	o.Revision = revision
	err := s.pool.QueryRow(ctx, `SELECT state,COALESCE(observed_spec_digest,''),COALESCE(observed_generation,0),reason,observed_at,updated_at FROM cert_manager_issuer_observations WHERE profile_id=$1 AND revision=$2`, profileID, revision).Scan(&o.State, &o.ObservedSpecDigest, &o.ObservedGeneration, &o.Reason, &o.ObservedAt, &o.UpdatedAt)
	return o, mapError(err)
}
func (s *PostgresStore) ReadyForHostname(ctx context.Context, host string, now time.Time, maxAge time.Duration, limit int) ([]TenantIdentity, error) {
	if !validHostname(host, true) || !validFreshness(now, maxAge) || limit < 1 || limit > 500 {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT p.id::text,p.name,p.current_revision FROM cert_manager_issuer_profiles p JOIN cert_manager_issuer_profile_revisions r ON r.profile_id=p.id AND r.revision=p.current_revision JOIN cert_manager_issuer_observations o ON o.profile_id=p.id AND o.revision=p.current_revision WHERE p.lifecycle='active' AND o.state='ready' AND o.observed_spec_digest=r.spec_digest AND o.observed_at >= $1 AND o.observed_at <= $2 ORDER BY p.name LIMIT 500`, now.Add(-maxAge), now.Add(30*time.Second))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type x struct {
		id, name string
		rev      int64
	}
	xs := []x{}
	for rows.Next() {
		var v x
		if err = rows.Scan(&v.id, &v.name, &v.rev); err != nil {
			return nil, err
		}
		xs = append(xs, v)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	out := []TenantIdentity{}
	for _, v := range xs {
		e, e2 := loadEntry(ctx, s.pool, v.id, v.rev)
		if e2 != nil {
			return nil, e2
		}
		if coversHostname(e.Revision.Spec, e.Revision.Solver, host) {
			out = append(out, TenantIdentity{ProfileID: e.Profile.ID, Name: e.Profile.Name, Revision: e.Revision.Revision, Solver: e.Revision.Solver, Environment: environmentForServer(e.Revision.Spec.ACME.Server)})
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

func (s *PostgresStore) PendingDematerialization(ctx context.Context, limit int) ([]Desired, error) {
	if limit < 1 || limit > 500 {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text,current_revision FROM cert_manager_issuer_profiles WHERE lifecycle='deactivated' ORDER BY name LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type ref struct {
		id  string
		rev int64
	}
	refs := []ref{}
	for rows.Next() {
		var r ref
		if err = rows.Scan(&r.id, &r.rev); err != nil {
			return nil, err
		}
		refs = append(refs, r)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	out := []Desired{}
	for _, r := range refs {
		e, e2 := loadEntry(ctx, s.pool, r.id, r.rev)
		if e2 != nil {
			return nil, e2
		}
		out = append(out, Desired{e.Profile.ID, e.Profile.Name, e.Revision.SpecDigest, e.Revision.Revision, e.Revision.Solver, e.Revision.Spec})
	}
	return out, nil
}

var _ Store = (*PostgresStore)(nil)

// ReconcileReferencesTx is the direct-Git admission seam. It resolves names
// against the exact active, fresh, ready revision and atomically replaces one
// AppConfig document's references. A false result is a policy rejection, not a
// transient database failure. Empty selections clear references on deletion.
func (s *PostgresStore) ReconcileReferencesTx(ctx context.Context, tx pgx.Tx, applicationID, environmentID, gitPath string, selections []Selection, now time.Time, maxAge time.Duration) (bool, error) {
	if s == nil || tx == nil || !uuidRE.MatchString(applicationID) || !uuidRE.MatchString(environmentID) || gitPath == "" || len(gitPath) > 1024 || len(selections) > 64 || !validFreshness(now, maxAge) {
		return false, ErrInvalid
	}
	type resolved struct {
		entry    Entry
		hostname string
	}
	items := make([]resolved, 0, len(selections))
	seen := map[string]struct{}{}
	for _, selection := range selections {
		host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(selection.Hostname)), ".")
		if !validHostname(host, true) || !dnsLabelRE.MatchString(selection.IssuerName) {
			return false, nil
		}
		if _, ok := seen[host]; ok {
			return false, nil
		}
		seen[host] = struct{}{}
		var pid string
		var rev int64
		err := tx.QueryRow(ctx, `SELECT p.id::text,p.current_revision FROM cert_manager_issuer_profiles p JOIN cert_manager_issuer_profile_revisions r ON r.profile_id=p.id AND r.revision=p.current_revision JOIN cert_manager_issuer_observations o ON o.profile_id=p.id AND o.revision=p.current_revision WHERE p.name=$1 AND p.lifecycle='active' AND o.state='ready' AND o.observed_spec_digest=r.spec_digest AND o.observed_at >= $2 AND o.observed_at <= $3 FOR SHARE OF p,r,o`, selection.IssuerName, now.Add(-maxAge), now.Add(30*time.Second)).Scan(&pid, &rev)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		entry, err := loadEntry(ctx, tx, pid, rev)
		if err != nil {
			return false, err
		}
		if !coversHostname(entry.Revision.Spec, entry.Revision.Solver, host) {
			return false, nil
		}
		items = append(items, resolved{entry, host})
	}
	if _, err := tx.Exec(ctx, `DELETE FROM cert_manager_issuer_references WHERE git_path=$1`, gitPath); err != nil {
		return false, err
	}
	for _, item := range items {
		if _, err := tx.Exec(ctx, `INSERT INTO cert_manager_issuer_references(profile_id,revision,application_id,environment_id,git_path,hostname,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, item.entry.Profile.ID, item.entry.Revision.Revision, applicationID, environmentID, gitPath, item.hostname, now); err != nil {
			return false, mapError(err)
		}
	}
	return true, nil
}

func (s *PostgresStore) ReconcileDeletedTx(ctx context.Context, tx pgx.Tx, gitPath string) error {
	if s == nil || tx == nil || gitPath == "" || len(gitPath) > 1024 {
		return ErrInvalid
	}
	_, err := tx.Exec(ctx, `DELETE FROM cert_manager_issuer_references WHERE git_path=$1`, gitPath)
	return err
}
