package memory

import (
	"context"

	"github.com/kuberploy/kuberploy/internal/domain"
	base "github.com/kuberploy/kuberploy/internal/store"
)

// BindBuildAttemptAuditCatalog wires the separate in-memory build store used
// by local and contract tests. Build-attempt ownership is immutable after
// creation; application/project/grant resolution and audit remain under the
// central store lock.
func (s *Store) BindBuildAttemptAuditCatalog(catalog base.BuildLogAttemptCatalog) {
	s.mu.Lock()
	s.buildAttemptAuditCatalog = catalog
	s.mu.Unlock()
}

func (s *Store) AuditBuildLogAccess(ctx context.Context, actor, attemptID, action, _ string) error {
	if action != "build.logs.snapshot" && action != "build.logs.follow" {
		return base.ErrConflict
	}
	s.mu.Lock()
	catalog := s.buildAttemptAuditCatalog
	s.mu.Unlock()
	if catalog == nil {
		return base.ErrNotFound
	}
	attempt, err := catalog.BuildLogAttemptOwnership(ctx, attemptID)
	if err != nil {
		return err
	}
	if attempt.AttemptID != attemptID || attempt.ApplicationID == "" || attempt.ProjectID == "" {
		return base.ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	application, exists := s.applications[attempt.ApplicationID]
	if !exists || application.ID != attempt.ApplicationID || application.ProjectID != attempt.ProjectID {
		return base.ErrNotFound
	}
	project, exists := s.projects[application.ProjectID]
	if !exists || project.ID != attempt.ProjectID {
		return base.ErrNotFound
	}
	target := domain.AccessTarget{Type: "application", ID: application.ID, TeamID: project.TeamID, ProjectID: project.ID, ApplicationID: application.ID}
	for _, permission := range []domain.Permission{domain.PermissionBuildsRead, domain.PermissionLogsRead} {
		if err = s.authorizeLocked(actor, permission, target); err != nil {
			return err
		}
	}
	s.audits++
	return nil
}
