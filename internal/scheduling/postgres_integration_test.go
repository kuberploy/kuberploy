package scheduling_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/testdb"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/scheduling"
)

func TestPostgresSchedulingAuditUsesCanonicalImmutableTimeline(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = testdb.ApplyMigrations(ctx, pool); err != nil {
		t.Fatal(err)
	}
	actor, project := id.New(), id.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err = pool.Exec(ctx, `INSERT INTO users(id,login,role,issuer,subject) VALUES($1,$2,'platform-admin','test',$2)`, actor, "scheduling-admin-"+actor[:8]); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO projects(id,name,slug,created_at) VALUES($1,'Scheduling project',$2,$3)`, project, "scheduling-"+project[:8], now); err != nil {
		t.Fatal(err)
	}
	store, err := scheduling.NewPostgresStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	command := func(key string, at time.Time) scheduling.Command {
		return scheduling.Command{ActorID: actor, IdempotencyKey: key, RequestID: "request-" + key, Now: at}
	}
	created, err := store.Create(ctx, command("scheduling-create", now), "general", scheduling.Spec{
		Description: "General workloads",
		Pod: scheduling.PodScheduling{NodeSelector: map[string]string{
			"kubernetes.io/os": "linux",
		}},
	}, []scheduling.Assignment{{Scope: scheduling.ProjectScope, ID: project}})
	if err != nil {
		t.Fatal(err)
	}
	revised, err := store.Revise(ctx, command("scheduling-revise", now.Add(time.Second)), scheduling.Ref{
		ProfileID: created.Profile.ID,
		Revision:  created.Revision.Revision,
	}, scheduling.Spec{
		Description: "General Linux workloads",
		Pod: scheduling.PodScheduling{NodeSelector: map[string]string{
			"kubernetes.io/os": "linux",
		}},
	}, []scheduling.Assignment{{Scope: scheduling.ProjectScope, ID: project}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Deactivate(ctx, command("scheduling-deactivate", now.Add(2*time.Second)), scheduling.Ref{
		ProfileID: revised.Profile.ID,
		Revision:  revised.Revision.Revision,
	}); err != nil {
		t.Fatal(err)
	}
	var auditID string
	var count int
	if err = pool.QueryRow(ctx, `SELECT min(id::text),count(*) FROM audit_events WHERE target_type='scheduling-profile' AND target_id=$1`, created.Profile.ID).Scan(&auditID, &count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("canonical scheduling audit count=%d, want 3", count)
	}
	if _, err = pool.Exec(ctx, `UPDATE audit_events SET request_id='substituted' WHERE id=$1`, auditID); !hasSQLState(err, "23514") {
		t.Fatalf("managed audit update was not rejected: %v", err)
	}
	if _, err = pool.Exec(ctx, `DELETE FROM audit_events WHERE id=$1`, auditID); !hasSQLState(err, "23514") {
		t.Fatalf("managed audit delete was not rejected: %v", err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO audit_events(id,actor_id,action,target_type,target_id,request_id,detail,created_at)
		VALUES($1,$2,'scheduling-profile.revise','scheduling-profile',$3,'forged-audit',jsonb_build_object(
			'revision',$4::bigint,'idempotencyKey','forged-audit','specDigest',$5::text,'assignmentsDigest',$6::text),$7)`,
		id.New(), actor, created.Profile.ID, revised.Revision.Revision, "sha256:"+strings.Repeat("0", 64), revised.Revision.AssignmentsDigest, now); !hasSQLState(err, "23514") {
		t.Fatalf("managed audit digest substitution was not rejected: %v", err)
	}
}

func hasSQLState(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}
