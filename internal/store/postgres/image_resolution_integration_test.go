package postgres

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/testdb"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/registry"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func TestAuthorizedImageSourcesSQLIsDeploymentScoped(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	store, err := Open(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = testdb.ApplyMigrations(t.Context(), store.pool); err != nil {
		t.Fatal(err)
	}
	now := databaseTime(time.Now())
	actorID, viewerID := id.New(), id.New()
	for _, user := range []struct {
		id, login, role string
	}{{actorID, "image-resolution-admin-" + actorID[:8], "platform-admin"}, {viewerID, "image-resolution-viewer-" + viewerID[:8], "developer"}} {
		if _, err = store.pool.Exec(t.Context(), `INSERT INTO users(id,login,role,issuer,subject,grant_revision,created_at)
			VALUES($1,$2,$3,'image-resolution-integration',$4,1,$5)`, user.id, user.login, user.role, user.id, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = store.pool.Exec(t.Context(), `INSERT INTO access_grants(id,subject_user_id,role,scope_type,scope_id,source,created_by,created_at)
		VALUES($1,$2,'platform-admin','platform','platform','bootstrap',$2,$3)`, id.New(), actorID, now); err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(t.Context(), actorID, "image-resolution-project-"+actorID[:8], "project-fingerprint-"+actorID,
		domain.CreateProject{Name: "Image resolution " + actorID[:8], Slug: "image-resolution-" + actorID[:8]})
	if err != nil {
		t.Fatal(err)
	}
	environment, err := store.CreateEnvironment(t.Context(), actorID, "image-resolution-environment-"+actorID[:8], "environment-fingerprint-"+actorID,
		domain.CreateEnvironment{ProjectID: project.Value.ID, Name: "Development", Slug: "development"})
	if err != nil {
		t.Fatal(err)
	}
	application, err := store.CreateApplication(t.Context(), actorID, "image-resolution-application-"+actorID[:8], "application-fingerprint-"+actorID,
		domain.CreateApplication{ProjectID: project.Value.ID, Name: "API", Slug: "api"})
	if err != nil {
		t.Fatal(err)
	}
	targetID := id.New()
	target, err := store.PutRegistryTarget(t.Context(), domain.RegistryTarget{ID: targetID, Name: "image-resolution-" + actorID[:8], Mode: domain.RegistryTargetExternal,
		Endpoint: "https://registry.example.test", RepositoryPrefix: "tenant", PullCredentialRef: "runtime-pull/main", CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	policy := registry.DefaultPolicy(target.ID, application.Value.ID, "tenant/api", now)
	if _, err = store.PutServiceRegistryPolicy(t.Context(), policy); err != nil {
		t.Fatal(err)
	}

	sources, err := store.AuthorizedImageSourcesForActor(t.Context(), actorID, application.Value.ID, environment.Value.ID)
	if err != nil || len(sources) != 1 || sources[0].Target.ID != targetID || sources[0].Policy.Repository != "tenant/api" {
		t.Fatalf("sources=%+v err=%v", sources, err)
	}
	if _, err = store.AuthorizedImageSourcesForActor(t.Context(), viewerID, application.Value.ID, environment.Value.ID); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("unscoped viewer err=%v", err)
	}
	otherProject, err := store.CreateProject(t.Context(), actorID, "image-resolution-other-project-"+actorID[:8], "other-project-fingerprint-"+actorID,
		domain.CreateProject{Name: "Other " + actorID[:8], Slug: "image-resolution-other-" + actorID[:8]})
	if err != nil {
		t.Fatal(err)
	}
	otherEnvironment, err := store.CreateEnvironment(t.Context(), actorID, "image-resolution-other-environment-"+actorID[:8], "other-environment-fingerprint-"+actorID,
		domain.CreateEnvironment{ProjectID: otherProject.Value.ID, Name: "Other", Slug: "other"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.AuthorizedImageSourcesForActor(t.Context(), actorID, application.Value.ID, otherEnvironment.Value.ID); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("cross-project err=%v", err)
	}
}
