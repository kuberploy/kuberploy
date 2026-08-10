package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/kuberploy/kuberploy/internal/domain"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func TestProjectRegistryPullCredentialsAreMultipleScopedAndSelectable(t *testing.T) {
	ctx := context.Background()
	store := New()
	projectID := "11111111-1111-4111-8111-111111111111"
	otherProjectID := "22222222-2222-4222-8222-222222222222"
	applicationID := "33333333-3333-4333-8333-333333333333"
	otherApplicationID := "44444444-4444-4444-8444-444444444444"
	actorID := "55555555-5555-4555-8555-555555555555"
	store.projects[projectID] = domain.Project{ID: projectID, Name: "Main"}
	store.projects[otherProjectID] = domain.Project{ID: otherProjectID, Name: "Other"}
	store.applications[applicationID] = domain.Application{ID: applicationID, ProjectID: projectID, Name: "API"}
	store.applications[otherApplicationID] = domain.Application{ID: otherApplicationID, ProjectID: otherProjectID, Name: "Other"}
	grantRegistryActor(store, actorID, domain.RoleProjectAdmin, domain.ScopeProject, projectID)
	for index, target := range []domain.RegistryTarget{
		{ID: "66666666-6666-4666-8666-666666666666", Name: "primary", Mode: domain.RegistryTargetExternal, Endpoint: "registry.example.com", RepositoryPrefix: "team", PullCredentialRef: "pull-primary"},
		{ID: "77777777-7777-4777-8777-777777777777", Name: "backup", Mode: domain.RegistryTargetExternal, Endpoint: "backup.example.com", RepositoryPrefix: "team", PullCredentialRef: "pull-backup"},
	} {
		if _, err := store.PutRegistryTarget(ctx, target); err != nil {
			t.Fatalf("target %d: %v", index, err)
		}
	}
	first := domain.ProjectRegistryPullCredential{ID: "88888888-8888-4888-8888-888888888888", ProjectID: projectID, RegistryTargetID: "66666666-6666-4666-8666-666666666666", Name: "Production"}
	second := domain.ProjectRegistryPullCredential{ID: "99999999-9999-4999-8999-999999999999", ProjectID: projectID, RegistryTargetID: "77777777-7777-4777-8777-777777777777", Name: "Backup"}
	if _, err := store.CreateProjectRegistryPullCredentialForActor(ctx, actorID, "first", "first", "request", first); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateProjectRegistryPullCredentialForActor(ctx, actorID, "second", "second", "request", second); err != nil {
		t.Fatal(err)
	}
	items, err := store.ListProjectRegistryPullCredentialsForActor(ctx, actorID, projectID)
	if err != nil || len(items) != 2 || items[0].Name != "Backup" || items[1].Name != "Production" {
		t.Fatalf("items=%#v err=%v", items, err)
	}
	selection := domain.ApplicationRegistryPullSelection{ApplicationID: applicationID, Mode: domain.ApplicationRegistryPullCredential, ProjectCredentialID: second.ID}
	if _, err = store.PutApplicationRegistryPullSelectionForActor(ctx, actorID, "select", "select", "request", selection); err != nil {
		t.Fatal(err)
	}
	if err = store.DeleteProjectRegistryPullCredentialForActor(ctx, actorID, projectID, second.ID); !errors.Is(err, base.ErrConflict) {
		t.Fatalf("selected delete err=%v", err)
	}
	cross := domain.ApplicationRegistryPullSelection{ApplicationID: otherApplicationID, Mode: domain.ApplicationRegistryPullCredential, ProjectCredentialID: first.ID}
	if _, err = store.PutApplicationRegistryPullSelectionForActor(ctx, actorID, "cross", "cross", "request", cross); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("cross project err=%v", err)
	}
	public := domain.ApplicationRegistryPullSelection{ApplicationID: applicationID, Mode: domain.ApplicationRegistryPullPublic}
	if _, err = store.PutApplicationRegistryPullSelectionForActor(ctx, actorID, "public", "public", "request", public); err != nil {
		t.Fatal(err)
	}
	if err = store.DeleteProjectRegistryPullCredentialForActor(ctx, actorID, projectID, second.ID); err != nil {
		t.Fatal(err)
	}
}
