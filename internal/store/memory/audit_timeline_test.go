package memory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func TestAuditTimelineRequiresExactAuthorizedScope(t *testing.T) {
	s := New()
	admin := domain.User{ID: "11111111-1111-4111-8111-111111111111", Email: "admin@audit.test", Role: "platform-admin", GrantRevision: 1, CreatedAt: time.Now().UTC()}
	if err := s.BootstrapAdmin(context.Background(), admin, "hash", make([]byte, 32), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	projectID := "22222222-2222-4222-8222-222222222222"
	s.projects[projectID] = domain.Project{ID: projectID, Name: "Scoped", Slug: "scoped", CreatedAt: time.Now().UTC()}
	viewer := "33333333-3333-4333-8333-333333333333"
	s.users[viewer] = domain.User{ID: viewer, Role: "viewer", GrantRevision: 1, CreatedAt: time.Now().UTC()}
	s.accessGrants["44444444-4444-4444-8444-444444444444"] = domain.AccessGrant{ID: "44444444-4444-4444-8444-444444444444", SubjectUserID: viewer, Role: domain.RoleViewer, ScopeType: domain.ScopeProject, ScopeID: projectID, Source: "test", CreatedBy: admin.ID, CreatedAt: time.Now().UTC()}
	s.AddAuditEvent(domain.AuditEvent{ID: "55555555-5555-4555-8555-555555555555", ActorID: admin.ID, Action: "project.create", TargetType: "project", TargetID: projectID, Outcome: "recorded", CreatedAt: time.Now().UTC()})
	items, err := s.ListAuditEventsForActor(context.Background(), viewer, domain.AuditEventQuery{TargetType: "project", TargetID: projectID, Limit: 10})
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%#v err=%v", items, err)
	}
	_, err = s.ListAuditEventsForActor(context.Background(), viewer, domain.AuditEventQuery{Limit: 10})
	if !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("global scoped err=%v", err)
	}
}
