package postgres

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/id"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func TestPostgreSQLProjectRegistryPullCredentialScopeAndSelection(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	store, err := Open(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = Migrate(t.Context(), store.pool); err != nil {
		t.Fatal(err)
	}
	now, actorID := databaseTime(time.Now()), id.New()
	if _, err = store.pool.Exec(t.Context(), `INSERT INTO users(id,login,role,issuer,subject,grant_revision,created_at) VALUES($1,$2,'platform-admin','registry-pull-test',$4,1,$3)`, actorID, "pull-admin-"+actorID[:8], now, actorID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(t.Context(), `INSERT INTO access_grants(id,subject_user_id,role,scope_type,scope_id,source,created_by,created_at) VALUES($1,$2,'platform-admin','platform','platform','bootstrap',$2,$3)`, id.New(), actorID, now); err != nil {
		t.Fatal(err)
	}
	createProject := func(slug string) domain.Project {
		result, createErr := store.CreateProject(t.Context(), actorID, "project-"+slug, "fingerprint-"+slug, domain.CreateProject{Name: slug, Slug: slug})
		if createErr != nil {
			t.Fatal(createErr)
		}
		return result.Value
	}
	project, otherProject := createProject("pull-"+actorID[:8]), createProject("other-"+actorID[:8])
	application, err := store.CreateApplication(t.Context(), actorID, "app-main", "app-main", domain.CreateApplication{ProjectID: project.ID, Name: "API", Slug: "api"})
	if err != nil {
		t.Fatal(err)
	}
	otherApplication, err := store.CreateApplication(t.Context(), actorID, "app-other", "app-other", domain.CreateApplication{ProjectID: otherProject.ID, Name: "Other", Slug: "other"})
	if err != nil {
		t.Fatal(err)
	}
	targetID := id.New()
	if _, err = store.PutRegistryTarget(t.Context(), domain.RegistryTarget{ID: targetID, Name: "pull-" + actorID[:8], Mode: domain.RegistryTargetExternal, Endpoint: "registry.example.test", RepositoryPrefix: "team", PullCredentialRef: "runtime-pull/main"}); err != nil {
		t.Fatal(err)
	}
	credential := domain.ProjectRegistryPullCredential{ID: id.New(), ProjectID: project.ID, RegistryTargetID: targetID, Name: "Production"}
	created, err := store.CreateProjectRegistryPullCredentialForActor(t.Context(), actorID, "credential", "credential", "request", credential)
	if err != nil || created.Value.RegistryServer != "registry.example.test" {
		t.Fatalf("created=%#v err=%v", created, err)
	}
	selection := domain.ApplicationRegistryPullSelection{ApplicationID: application.Value.ID, Mode: domain.ApplicationRegistryPullCredential, ProjectCredentialID: credential.ID}
	if _, err = store.PutApplicationRegistryPullSelectionForActor(t.Context(), actorID, "selection", "selection", "request", selection); err != nil {
		t.Fatal(err)
	}
	if err = store.DeleteProjectRegistryPullCredentialForActor(t.Context(), actorID, project.ID, credential.ID); !errors.Is(err, base.ErrConflict) {
		t.Fatalf("selected delete err=%v", err)
	}
	if _, err = store.pool.Exec(t.Context(), `INSERT INTO application_registry_pull_selections(application_id,mode,project_credential_id,updated_by,updated_at) VALUES($1,'project-credential',$2,$3,$4)`, otherApplication.Value.ID, credential.ID, actorID, now); err == nil {
		t.Fatal("cross-project selection was accepted")
	}
	public := domain.ApplicationRegistryPullSelection{ApplicationID: application.Value.ID, Mode: domain.ApplicationRegistryPullPublic}
	if _, err = store.PutApplicationRegistryPullSelectionForActor(t.Context(), actorID, "public", "public", "request", public); err != nil {
		t.Fatal(err)
	}
	if err = store.DeleteProjectRegistryPullCredentialForActor(t.Context(), actorID, project.ID, credential.ID); err != nil {
		t.Fatal(err)
	}
}
