package buildlogs

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/kuberploy/kuberploy/internal/builds"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/store"
)

type resolverAttemptCatalog struct {
	attempt builds.BuildAttempt
	err     error
}

func (c *resolverAttemptCatalog) Attempt(context.Context, string) (builds.BuildAttempt, error) {
	return c.attempt, c.err
}

type resolverResourceCatalog struct {
	application domain.Application
	project     domain.Project
	appErr      error
	projectErr  error
	authorize   map[domain.Permission]error
	calls       []domain.Permission
	targets     []domain.AccessTarget
}

func (c *resolverResourceCatalog) GetApplication(context.Context, string) (domain.Application, error) {
	return c.application, c.appErr
}

func (c *resolverResourceCatalog) GetProject(context.Context, string) (domain.Project, error) {
	return c.project, c.projectErr
}

func (c *resolverResourceCatalog) Authorize(_ context.Context, _ string, permission domain.Permission, target domain.AccessTarget) error {
	c.calls = append(c.calls, permission)
	c.targets = append(c.targets, target)
	return c.authorize[permission]
}

func TestRecordResolverRequiresExactRecordsAndBothPermissions(t *testing.T) {
	authorized, _, _ := buildLogFixture(t)
	attempts := &resolverAttemptCatalog{attempt: authorized.Attempt}
	resources := &resolverResourceCatalog{
		application: domain.Application{ID: testApplicationID, ProjectID: testProjectID},
		project:     domain.Project{ID: testProjectID, TeamID: "88888888-8888-4888-8888-888888888888"},
		authorize:   map[domain.Permission]error{},
	}
	resolver, err := NewRecordResolver(attempts, resources)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolver.Resolve(t.Context(), AccessRequest{ActorID: testActorID, AttemptID: testAttemptID})
	if err != nil || resolved.Attempt.ID != testAttemptID || resolved.ApplicationID != testApplicationID || resolved.ProjectID != testProjectID {
		t.Fatalf("resolved=%#v err=%v", resolved, err)
	}
	wantPermissions := []domain.Permission{domain.PermissionBuildsRead, domain.PermissionLogsRead}
	if !reflect.DeepEqual(resources.calls, wantPermissions) || len(resources.targets) != 2 {
		t.Fatalf("permissions=%v targets=%#v", resources.calls, resources.targets)
	}
	for _, target := range resources.targets {
		if target.Type != "application" || target.ID != testApplicationID || target.ApplicationID != testApplicationID || target.ProjectID != testProjectID || target.TeamID != resources.project.TeamID {
			t.Fatalf("incomplete authorization chain: %#v", target)
		}
	}
}

func TestRecordResolverCollapsesMissingForbiddenAndOwnershipMismatch(t *testing.T) {
	authorized, _, _ := buildLogFixture(t)
	tests := []struct {
		name   string
		change func(*resolverAttemptCatalog, *resolverResourceCatalog)
	}{
		{name: "attempt missing", change: func(a *resolverAttemptCatalog, _ *resolverResourceCatalog) { a.err = builds.ErrNotFound }},
		{name: "application missing", change: func(_ *resolverAttemptCatalog, r *resolverResourceCatalog) { r.appErr = store.ErrNotFound }},
		{name: "ownership mismatch", change: func(_ *resolverAttemptCatalog, r *resolverResourceCatalog) {
			r.application.ProjectID = "99999999-9999-4999-8999-999999999999"
		}},
		{name: "build hidden", change: func(_ *resolverAttemptCatalog, r *resolverResourceCatalog) {
			r.authorize[domain.PermissionBuildsRead] = store.ErrForbidden
		}},
		{name: "logs hidden", change: func(_ *resolverAttemptCatalog, r *resolverResourceCatalog) {
			r.authorize[domain.PermissionLogsRead] = store.ErrForbidden
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			attempts := &resolverAttemptCatalog{attempt: authorized.Attempt}
			resources := &resolverResourceCatalog{application: domain.Application{ID: testApplicationID, ProjectID: testProjectID}, project: domain.Project{ID: testProjectID}, authorize: map[domain.Permission]error{}}
			test.change(attempts, resources)
			resolver, _ := NewRecordResolver(attempts, resources)
			if _, err := resolver.Resolve(t.Context(), AccessRequest{ActorID: testActorID, AttemptID: testAttemptID}); !errors.Is(err, ErrNotFound) {
				t.Fatalf("missing/forbidden resource was distinguishable: %v", err)
			}
		})
	}
}

func TestRecordResolverPropagatesInfrastructureAndRevalidates(t *testing.T) {
	authorized, _, _ := buildLogFixture(t)
	infrastructure := errors.New("database unavailable")
	attempts := &resolverAttemptCatalog{attempt: authorized.Attempt}
	resources := &resolverResourceCatalog{application: domain.Application{ID: testApplicationID, ProjectID: testProjectID}, project: domain.Project{ID: testProjectID}, authorize: map[domain.Permission]error{}}
	resolver, _ := NewRecordResolver(attempts, resources)
	access := AccessRequest{ActorID: testActorID, AttemptID: testAttemptID}
	if err := resolver.Revalidate(t.Context(), access); err != nil {
		t.Fatal(err)
	}
	resources.authorize[domain.PermissionLogsRead] = infrastructure
	if err := resolver.Revalidate(t.Context(), access); !errors.Is(err, infrastructure) {
		t.Fatalf("infrastructure failure was collapsed: %v", err)
	}
}
