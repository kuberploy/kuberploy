package memory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	base "github.com/kuberploy/kuberploy/internal/store"
)

type buildLogAttemptCatalog struct {
	attempt base.BuildLogAttemptOwnership
	err     error
}

func (c *buildLogAttemptCatalog) BuildLogAttemptOwnership(context.Context, string) (base.BuildLogAttemptOwnership, error) {
	return c.attempt, c.err
}

func TestAuditBuildLogAccessUsesFreshExactAttemptChainAndBothPermissions(t *testing.T) {
	store := New()
	actorID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	projectID := "22222222-2222-4222-8222-222222222222"
	applicationID := "33333333-3333-4333-8333-333333333333"
	attemptID := "11111111-1111-4111-8111-111111111111"
	grantID := "44444444-4444-4444-8444-444444444444"
	store.users[actorID] = domain.User{ID: actorID, DisplayName: "viewer", Role: string(domain.RoleViewer), Issuer: "test", Subject: actorID, GrantRevision: 1, CreatedAt: time.Now().UTC()}
	store.projects[projectID] = domain.Project{ID: projectID, Name: "Project", Slug: "project", CreatedAt: time.Now().UTC()}
	store.applications[applicationID] = domain.Application{ID: applicationID, ProjectID: projectID, Name: "App", Slug: "app", CreatedAt: time.Now().UTC()}
	store.accessGrants[grantID] = domain.AccessGrant{ID: grantID, SubjectUserID: actorID, Role: domain.RoleViewer, ScopeType: domain.ScopeApplication, ScopeID: applicationID, Source: "test", CreatedBy: actorID, CreatedAt: time.Now().UTC()}
	catalog := &buildLogAttemptCatalog{attempt: base.BuildLogAttemptOwnership{AttemptID: attemptID, ProjectID: projectID, ApplicationID: applicationID}}
	store.BindBuildAttemptAuditCatalog(catalog)

	if err := store.AuditBuildLogAccess(t.Context(), actorID, attemptID, "build.logs.snapshot", "request"); !errors.Is(err, base.ErrForbidden) {
		t.Fatalf("viewer without logs.read was accepted: %v", err)
	}
	if store.AuditCount() != 0 {
		t.Fatal("failed authorization was audited")
	}
	grant := store.accessGrants[grantID]
	grant.Permissions = []domain.Permission{domain.PermissionLogsRead}
	store.accessGrants[grantID] = grant
	if err := store.AuditBuildLogAccess(t.Context(), actorID, attemptID, "build.logs.snapshot", "request"); err != nil {
		t.Fatal(err)
	}
	if store.AuditCount() != 1 {
		t.Fatalf("audit count=%d", store.AuditCount())
	}

	catalog.attempt.ProjectID = "55555555-5555-4555-8555-555555555555"
	if err := store.AuditBuildLogAccess(t.Context(), actorID, attemptID, "build.logs.follow", "request"); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("mismatched fresh attempt ownership accepted: %v", err)
	}
	catalog.err = base.ErrNotFound
	if err := store.AuditBuildLogAccess(t.Context(), actorID, attemptID, "build.logs.follow", "request"); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("missing attempt distinguishable: %v", err)
	}
	if err := store.AuditBuildLogAccess(t.Context(), actorID, attemptID, "attacker.action", "request"); !errors.Is(err, base.ErrConflict) {
		t.Fatalf("unbounded action accepted: %v", err)
	}
}
