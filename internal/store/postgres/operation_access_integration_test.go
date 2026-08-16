package postgres

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/id"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func TestPostgreSQLOperationAccessUsesStoredVariableSetScope(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := t.Context()
	store, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	suffix := strings.ReplaceAll(id.New(), "-", "")
	now := databaseTime(time.Now().UTC())
	adminID, ownerID := id.New(), id.New()
	projectID, otherProjectID, environmentID := id.New(), id.New(), id.New()
	projectOperationID, environmentOperationID, otherOperationID := id.New(), id.New(), id.New()
	if _, err = store.pool.Exec(ctx, `INSERT INTO users(id,login,role,issuer,subject,grant_revision,created_at)
		VALUES($1,$2,'platform-admin','operation-access-test',$2,1,$3),
		($4,$5,'developer','operation-access-test',$5,1,$3)`, adminID, "operation-access-admin-"+suffix, now, ownerID, "operation-access-owner-"+suffix); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO projects(id,name,slug,created_at) VALUES
		($1,'Variable operations',$2,$3),($4,'Other operations',$5,$3)`, projectID, "variable-operations-"+suffix, now, otherProjectID, "other-operations-"+suffix); err != nil {
		t.Fatal(err)
	}
	namespace := "variable-operations-" + suffix
	if _, err = store.pool.Exec(ctx, `INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at,protection_policy)
		VALUES($1,$2,'Development','development',$3,$3,$4,'development')`, environmentID, projectID, namespace, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO access_grants(id,subject_user_id,role,scope_type,scope_id,source,created_by,created_at)
		VALUES($1,$2,'project-admin','project',$3,'explicit',$4,$5)`, id.New(), ownerID, projectID, adminID, now); err != nil {
		t.Fatal(err)
	}
	insertOperation := func(operationID, targetType, targetID string) {
		t.Helper()
		if _, execErr := store.pool.Exec(ctx, `INSERT INTO operations(id,kind,status,target_type,target_id,request_id,generation,progress,created_at,updated_at)
			VALUES($1,'variable-set.git-write','queued',$2,$3,$4,1,'[]'::jsonb,$5,$5)`, operationID, targetType, targetID, operationID, now); execErr != nil {
			t.Fatal(execErr)
		}
	}
	insertOperation(projectOperationID, "project", projectID)
	insertOperation(environmentOperationID, "environment", environmentID)
	insertOperation(otherOperationID, "project", otherProjectID)
	unknownOperationID := id.New()
	insertOperation(unknownOperationID, "future-scope", projectID)

	for _, operationID := range []string{projectOperationID, environmentOperationID} {
		operation, getErr := store.GetOperationForActor(ctx, ownerID, operationID)
		if getErr != nil || operation.ID != operationID {
			t.Fatalf("tenant owner could not read scoped operation=%#v err=%v", operation, getErr)
		}
	}
	operations, err := store.ListOperationsForActor(ctx, ownerID)
	if err != nil || len(operations) != 2 {
		t.Fatalf("scoped operation list=%#v err=%v", operations, err)
	}
	if _, err = store.GetOperationForActor(ctx, ownerID, unknownOperationID); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("unknown-scope operation was visible: %v", err)
	}
	if _, err = store.GetOperationForActor(ctx, ownerID, otherOperationID); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("cross-tenant operation was visible: %v", err)
	}
}
