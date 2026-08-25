package postgres

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/id"
	base "github.com/kuberploy/kuberploy/internal/store"
	"github.com/kuberploy/kuberploy/internal/testdb"
)

func TestPostgreSQLApplicationAndEnvironmentDeletion(t *testing.T) {
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
	if err = testdb.ApplyMigrations(ctx, store.pool); err != nil {
		t.Fatal(err)
	}

	suffix := strings.ReplaceAll(id.New(), "-", "")[:12]
	actorID := id.New()
	now := time.Now().UTC()
	if _, err = store.pool.Exec(ctx, `INSERT INTO users(id,display_name,role,issuer,subject,grant_revision,created_at) VALUES($1,$2,'platform-admin','resource-delete-test',$3,1,$4)`, actorID, "delete-admin-"+suffix, actorID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO access_grants(id,subject_user_id,role,scope_type,scope_id,source,created_by,created_at) VALUES($1,$2,'platform-admin','platform','platform','bootstrap',$2,$3)`, id.New(), actorID, now); err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(ctx, actorID, "delete-project-"+suffix, "delete-project-"+suffix, domain.CreateProject{Name: "Delete project", Slug: "delete-" + suffix})
	if err != nil {
		t.Fatal(err)
	}
	environment, err := store.CreateEnvironment(ctx, actorID, "delete-environment-"+suffix, "delete-environment-"+suffix, domain.CreateEnvironment{ProjectID: project.Value.ID, Name: "Disposable environment", Slug: "disposable"})
	if err != nil {
		t.Fatal(err)
	}
	application, err := store.CreateApplication(ctx, actorID, "delete-application-"+suffix, "delete-application-"+suffix, domain.CreateApplication{ProjectID: project.Value.ID, EnvironmentID: environment.Value.ID, Name: "Disposable App", Slug: "disposable"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.DeleteApplication(ctx, actorID, application.Value.ID, "Wrong", "delete-app-wrong-"+suffix, "wrong", "request-wrong-"+suffix); !errors.Is(err, base.ErrDeletionConfirmation) {
		t.Fatalf("wrong confirmation err=%v", err)
	}
	replay, err := store.DeleteApplication(ctx, actorID, application.Value.ID, application.Value.Name, "delete-app-"+suffix, "delete-app", "request-app-"+suffix)
	if err != nil || replay {
		t.Fatalf("delete App replay=%t err=%v", replay, err)
	}
	replay, err = store.DeleteApplication(ctx, actorID, application.Value.ID, application.Value.Name, "delete-app-"+suffix, "delete-app", "request-app-replay-"+suffix)
	if err != nil || !replay {
		t.Fatalf("delete App replay replay=%t err=%v", replay, err)
	}
	if _, err = store.GetApplication(ctx, application.Value.ID); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("deleted App err=%v", err)
	}

	replay, err = store.DeleteEnvironment(ctx, actorID, environment.Value.ID, environment.Value.Name, "delete-env-"+suffix, "delete-env", "request-env-"+suffix)
	if err != nil || replay {
		t.Fatalf("delete Environment replay=%t err=%v", replay, err)
	}
	if _, err = store.GetEnvironment(ctx, environment.Value.ID); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("deleted Environment err=%v", err)
	}
}

func TestPostgreSQLResourceDeletionRejectsDeploymentHistory(t *testing.T) {
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
	if err = testdb.ApplyMigrations(ctx, store.pool); err != nil {
		t.Fatal(err)
	}

	suffix := strings.ReplaceAll(id.New(), "-", "")[:12]
	actorID := id.New()
	now := time.Now().UTC()
	if _, err = store.pool.Exec(ctx, `INSERT INTO users(id,display_name,role,issuer,subject,grant_revision,created_at) VALUES($1,$2,'platform-admin','resource-delete-block-test',$3,1,$4)`, actorID, "delete-block-admin-"+suffix, actorID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO access_grants(id,subject_user_id,role,scope_type,scope_id,source,created_by,created_at) VALUES($1,$2,'platform-admin','platform','platform','bootstrap',$2,$3)`, id.New(), actorID, now); err != nil {
		t.Fatal(err)
	}
	project, _ := store.CreateProject(ctx, actorID, "blocked-project-"+suffix, "blocked-project-"+suffix, domain.CreateProject{Name: "Blocked project", Slug: "blocked-" + suffix})
	environment, _ := store.CreateEnvironment(ctx, actorID, "blocked-environment-"+suffix, "blocked-environment-"+suffix, domain.CreateEnvironment{ProjectID: project.Value.ID, Name: "Production", Slug: "production"})
	application, _ := store.CreateApplication(ctx, actorID, "blocked-application-"+suffix, "blocked-application-"+suffix, domain.CreateApplication{ProjectID: project.Value.ID, Name: "API", Slug: "api"})
	_, _, err = store.CreateDeployment(ctx, actorID, "blocked-deployment-"+suffix, "blocked-deployment-"+suffix, "request-"+suffix, domain.CreateDeployment{
		EnvironmentID: environment.Value.ID, ApplicationID: application.Value.ID,
		Image: "registry.example.test/api@sha256:" + strings.Repeat("a", 64), Replicas: 1, Port: 8080,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.DeleteApplication(ctx, actorID, application.Value.ID, application.Value.Name, "blocked-app-"+suffix, "blocked-app", "request-blocked-app-"+suffix); !errors.Is(err, base.ErrApplicationDeletionBlocked) {
		t.Fatalf("active App deletion err=%v", err)
	}
	if _, err = store.DeleteEnvironment(ctx, actorID, environment.Value.ID, environment.Value.Name, "blocked-env-"+suffix, "blocked-env", "request-blocked-env-"+suffix); !errors.Is(err, base.ErrEnvironmentDeletionBlocked) {
		t.Fatalf("active Environment deletion err=%v", err)
	}
}
