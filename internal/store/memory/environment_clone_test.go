package memory

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kuberploy/kuberploy/internal/domain"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func TestCloneEnvironmentCreatesOnlyDraftStoppedSharedAppPlacements(t *testing.T) {
	ctx := context.Background()
	store := New()
	admin := bootstrapAccessAdmin(t, store)
	project, err := store.CreateProject(ctx, admin.ID, "clone-project", "clone-project", domain.CreateProject{Name: "Clone", Slug: "clone"})
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.CreateEnvironment(ctx, admin.ID, "clone-source", "clone-source", domain.CreateEnvironment{
		ProjectID: project.Value.ID, Name: "Development", Slug: "development", ProtectionPolicy: domain.EnvironmentDevelopment,
	})
	if err != nil {
		t.Fatal(err)
	}
	applications := make([]domain.Application, 0, 2)
	for index, name := range []string{"API", "Worker"} {
		slug := strings.ToLower(name)
		application, createErr := store.CreateApplication(ctx, admin.ID, "clone-app-"+slug, "clone-app-"+slug, domain.CreateApplication{
			ProjectID: project.Value.ID, Name: name, Slug: slug,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		applications = append(applications, application.Value)
		_, _, createErr = store.CreateDeployment(ctx, admin.ID, "clone-deployment-"+slug, "clone-deployment-"+slug, "request", domain.CreateDeployment{
			EnvironmentID: source.Value.ID, ApplicationID: application.Value.ID,
			Image: "registry.example.test/" + slug + "@sha256:" + strings.Repeat(string(rune('a'+index)), 64), Replicas: 1, Port: 8080,
		}, nil)
		if createErr != nil {
			t.Fatal(createErr)
		}
	}

	// Simulate deployments created before explicit placement rows existed.
	delete(store.environmentAppPlacements, source.Value.ID)
	deploymentsBefore, operationsBefore := len(store.deployments), len(store.operations)
	outboxBefore, gitCommandsBefore := len(store.outbox), len(store.gitWriteCommands)
	autoDeployBefore, releasesBefore := len(store.autoDeployRuns), len(store.registryReleases)
	auditsBefore := store.AuditCount()

	input := domain.CloneEnvironment{Name: "Production", Slug: "production"}
	cloned, err := store.CloneEnvironment(ctx, admin.ID, source.Value.ID, "clone-environment", "clone-environment-fingerprint", input)
	if err != nil {
		t.Fatal(err)
	}
	if cloned.Replay || cloned.Value.Environment.ID == source.Value.ID || cloned.Value.Environment.ProjectID != project.Value.ID ||
		cloned.Value.Environment.ProtectionPolicy != domain.EnvironmentDevelopment {
		t.Fatalf("clone=%#v", cloned)
	}
	wantNamespace, wantArgoProject := domain.DeriveEnvironmentDestination(project.Value, "production")
	if cloned.Value.Environment.Namespace != wantNamespace || cloned.Value.Environment.ArgoProject != wantArgoProject {
		t.Fatalf("derived destination=%#v", cloned.Value.Environment)
	}
	if len(cloned.Value.AppPlacements) != len(applications) {
		t.Fatalf("placements=%#v", cloned.Value.AppPlacements)
	}
	seen := map[string]bool{}
	for _, placement := range cloned.Value.AppPlacements {
		seen[placement.ApplicationID] = true
		if placement.EnvironmentID != cloned.Value.Environment.ID || placement.ProjectID != project.Value.ID ||
			placement.State != domain.EnvironmentAppPlacementDraft || placement.DesiredState != domain.EnvironmentAppPlacementStopped {
			t.Fatalf("unsafe clone placement=%#v", placement)
		}
	}
	for _, application := range applications {
		if !seen[application.ID] {
			t.Fatalf("shared application identity %s missing", application.ID)
		}
	}
	if len(store.deployments) != deploymentsBefore || len(store.operations) != operationsBefore || len(store.outbox) != outboxBefore ||
		len(store.gitWriteCommands) != gitCommandsBefore || len(store.autoDeployRuns) != autoDeployBefore || len(store.registryReleases) != releasesBefore {
		t.Fatal("clone created deployment, Git, build, release, provider, or worker side effects")
	}
	if store.AuditCount() != auditsBefore+1 {
		t.Fatalf("audit count=%d", store.AuditCount())
	}

	replay, err := store.CloneEnvironment(ctx, admin.ID, source.Value.ID, "clone-environment", "clone-environment-fingerprint", input)
	if err != nil || !replay.Replay || replay.Value.Environment.ID != cloned.Value.Environment.ID || len(replay.Value.AppPlacements) != len(applications) {
		t.Fatalf("replay=%#v err=%v", replay, err)
	}
	if store.AuditCount() != auditsBefore+1 || len(store.environments) != 2 {
		t.Fatal("idempotent replay duplicated durable state")
	}
	if _, err = store.CloneEnvironment(ctx, admin.ID, source.Value.ID, "clone-environment", "different-fingerprint", domain.CloneEnvironment{Name: "Other", Slug: "other"}); !errors.Is(err, base.ErrIdempotencyConflict) {
		t.Fatalf("idempotency conflict err=%v", err)
	}

	// Cloning an already cloned draft proves explicit placements are durable and
	// source-compatible without any deployment.
	second, err := store.CloneEnvironment(ctx, admin.ID, cloned.Value.Environment.ID, "clone-environment-second", "clone-environment-second-fingerprint", domain.CloneEnvironment{Name: "Staging", Slug: "staging"})
	if err != nil || len(second.Value.AppPlacements) != len(applications) || len(store.deployments) != deploymentsBefore {
		t.Fatalf("draft clone=%#v deployments=%d err=%v", second, len(store.deployments), err)
	}
}

func TestCloneEnvironmentRequiresCreatePermissionAtSourceProject(t *testing.T) {
	ctx := context.Background()
	store := New()
	admin := bootstrapAccessAdmin(t, store)
	member, _ := invitedUser(t, store, admin, "Environment developer", "environment-clone-scope")
	project, err := store.CreateProject(ctx, admin.ID, "clone-auth-project", "clone-auth-project", domain.CreateProject{Name: "Auth", Slug: "clone-auth"})
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.CreateEnvironment(ctx, admin.ID, "clone-auth-source", "clone-auth-source", domain.CreateEnvironment{ProjectID: project.Value.ID, Name: "Dev", Slug: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.CreateProjectAccessGrant(ctx, admin.ID, "clone-auth-grant", "clone-auth-grant", "request", domain.CreateAccessGrant{
		ProjectID: project.Value.ID, SubjectUserID: member.ID, Role: domain.RoleDeveloper,
		ScopeType: domain.ScopeEnvironment, ScopeID: source.Value.ID,
	}); err != nil {
		t.Fatal(err)
	}
	environmentsBefore := len(store.environments)
	_, err = store.CloneEnvironment(ctx, member.ID, source.Value.ID, "forbidden-clone", "forbidden-clone", domain.CloneEnvironment{Name: "Prod", Slug: "prod"})
	if !errors.Is(err, base.ErrForbidden) || len(store.environments) != environmentsBefore {
		t.Fatalf("environment-scoped clone err=%v environments=%d", err, len(store.environments))
	}
}
