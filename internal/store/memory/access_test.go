package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func bootstrapAccessAdmin(t *testing.T, store *Store) domain.User {
	t.Helper()
	admin := domain.User{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Login: "Admin", Role: "platform-admin", Issuer: "test", Subject: "admin", GrantRevision: 1, CreatedAt: time.Now().UTC()}
	hash := sha256.Sum256([]byte("admin-session"))
	if err := store.BootstrapAdmin(context.Background(), admin, strings.Repeat("h", 64), hash[:], time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return admin
}

func invitedUser(t *testing.T, store *Store, admin domain.User, name, seed string) (domain.User, [32]byte) {
	t.Helper()
	ctx := context.Background()
	token := sha256.Sum256([]byte("invite-" + seed))
	if _, err := store.CreateUserInvitation(ctx, admin.ID, name, token[:], time.Now().Add(time.Hour), "request"); err != nil {
		t.Fatal(err)
	}
	session := sha256.Sum256([]byte("session-" + seed))
	u, err := store.AcceptUserInvitation(ctx, token[:], name, strings.Repeat("h", 64), session[:], time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return u, session
}

func issueSession(t *testing.T, store *Store, userID, seed string) [32]byte {
	t.Helper()
	hash := sha256.Sum256([]byte("issued-session-" + seed))
	store.mu.Lock()
	defer store.mu.Unlock()
	u, ok := store.users[userID]
	if !ok {
		t.Fatalf("issue session for missing user %s", userID)
	}
	store.sessions[hex.EncodeToString(hash[:])] = struct {
		userID   string
		revision int64
		expires  time.Time
	}{userID: userID, revision: u.GrantRevision, expires: time.Now().Add(time.Hour)}
	return hash
}

func TestInvitationIsHashedSingleUseAndMembershipRevokesSession(t *testing.T) {
	ctx := context.Background()
	store := New()
	admin := bootstrapAccessAdmin(t, store)
	member, session := invitedUser(t, store, admin, "Developer", "one")
	if _, err := store.AcceptUserInvitation(ctx, sha256Bytes("invite-one"), "Developer", strings.Repeat("h", 64), sha256Bytes("another"), time.Now().Add(time.Hour)); !errors.Is(err, base.ErrInvitationInvalid) {
		t.Fatalf("single-use invitation replay err=%v", err)
	}
	team, err := store.CreateTeam(ctx, admin.ID, "team", "team", "request", domain.CreateTeam{Name: "Platform", Slug: "platform"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.AddTeamMember(ctx, admin.ID, team.Value.ID, "member", "member", "request", domain.AddTeamMember{UserID: member.ID, Role: "member"}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.UserBySession(ctx, session[:], time.Now()); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("membership change retained stale session: %v", err)
	}
}

func TestTeamMembershipKeepsAtLeastOneOwner(t *testing.T) {
	ctx := context.Background()
	store := New()
	admin := bootstrapAccessAdmin(t, store)
	team, err := store.CreateTeam(ctx, admin.ID, "team", "team", "request", domain.CreateTeam{Name: "Platform", Slug: "platform"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.AddTeamMember(ctx, admin.ID, team.Value.ID, "demote-sole-owner", "demote-sole-owner", "request", domain.AddTeamMember{UserID: admin.ID, Role: "member"}); !errors.Is(err, base.ErrConflict) {
		t.Fatalf("demoting the sole owner err=%v", err)
	}
	members, err := store.ListTeamMembersForActor(ctx, admin.ID, team.Value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].UserID != admin.ID || members[0].Role != "owner" {
		t.Fatalf("sole owner changed after rejected demotion: %#v", members)
	}
	secondOwner, _ := invitedUser(t, store, admin, "Second Owner", "second-owner")
	if _, err = store.AddTeamMember(ctx, admin.ID, team.Value.ID, "add-second-owner", "add-second-owner", "request", domain.AddTeamMember{UserID: secondOwner.ID, Role: "owner"}); err != nil {
		t.Fatal(err)
	}
	adminGrantBefore := store.users[admin.ID].GrantRevision
	if _, err = store.AddTeamMember(ctx, admin.ID, team.Value.ID, "demote-owner", "demote-owner", "request", domain.AddTeamMember{UserID: admin.ID, Role: "member"}); err != nil {
		t.Fatalf("demote with another owner: %v", err)
	}
	if store.users[admin.ID].GrantRevision != adminGrantBefore {
		t.Fatal("platform administrator grant revision changed for a team-only privilege change")
	}
	if err = store.RemoveTeamMember(ctx, admin.ID, team.Value.ID, secondOwner.ID, "request"); !errors.Is(err, base.ErrConflict) {
		t.Fatalf("removing the sole remaining owner err=%v", err)
	}
}

func TestRemoveTeamMemberRequiresOwnerAndRevokesCurrentGrant(t *testing.T) {
	ctx := context.Background()
	store := New()
	admin := bootstrapAccessAdmin(t, store)
	member, _ := invitedUser(t, store, admin, "Member", "remove-member")
	outsider, _ := invitedUser(t, store, admin, "Outsider", "remove-outsider")
	team, err := store.CreateTeam(ctx, admin.ID, "team", "team", "request", domain.CreateTeam{Name: "Platform", Slug: "platform"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.AddTeamMember(ctx, admin.ID, team.Value.ID, "member", "member", "request", domain.AddTeamMember{UserID: member.ID, Role: "member"}); err != nil {
		t.Fatal(err)
	}
	currentSession := issueSession(t, store, member.ID, "remove-member")
	if err = store.RemoveTeamMember(ctx, outsider.ID, team.Value.ID, member.ID, "request"); !errors.Is(err, base.ErrForbidden) {
		t.Fatalf("ordinary user removed a member: %v", err)
	}
	if _, err = store.UserBySession(ctx, currentSession[:], time.Now()); err != nil {
		t.Fatalf("forbidden removal changed the member's current session: %v", err)
	}
	if err = store.RemoveTeamMember(ctx, admin.ID, team.Value.ID, member.ID, "request"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.UserBySession(ctx, currentSession[:], time.Now()); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("removed member retained a current session: %v", err)
	}
	if err = store.RemoveTeamMember(ctx, admin.ID, team.Value.ID, member.ID, "request"); err != nil {
		t.Fatalf("repeated authorized removal should be idempotent: %v", err)
	}
}

func sha256Bytes(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}

func TestTeamProjectsAndGitHubInstallationsAreFilteredServerSide(t *testing.T) {
	ctx := context.Background()
	store := New()
	admin := bootstrapAccessAdmin(t, store)
	member, _ := invitedUser(t, store, admin, "Member", "member")
	outsider, _ := invitedUser(t, store, admin, "Outsider", "outsider")
	team, err := store.CreateTeam(ctx, admin.ID, "team", "team", "request", domain.CreateTeam{Name: "Product", Slug: "product"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.AddTeamMember(ctx, admin.ID, team.Value.ID, "member", "member", "request", domain.AddTeamMember{UserID: member.ID, Role: "member"}); err != nil {
		t.Fatal(err)
	}
	teamProject, err := store.CreateProject(ctx, admin.ID, "project-team", "project-team", domain.CreateProject{Name: "Team project", Slug: "team-project", TeamID: team.Value.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.CreateProject(ctx, admin.ID, "project-admin", "project-admin", domain.CreateProject{Name: "Admin only", Slug: "admin-only"}); err != nil {
		t.Fatal(err)
	}
	memberProjects, err := store.ListProjectsForActor(ctx, member.ID)
	if err != nil || len(memberProjects) != 1 || memberProjects[0].ID != teamProject.Value.ID {
		t.Fatalf("member project visibility=%#v err=%v", memberProjects, err)
	}
	if outsiderProjects, listErr := store.ListProjectsForActor(ctx, outsider.ID); listErr != nil || len(outsiderProjects) != 0 {
		t.Fatalf("outsider leaked projects=%#v err=%v", outsiderProjects, listErr)
	}
	environment, err := store.CreateEnvironment(ctx, member.ID, "environment", "environment", domain.CreateEnvironment{ProjectID: teamProject.Value.ID, Name: "Dev", Slug: "dev", Namespace: "caller-controlled", ArgoProject: "caller-controlled"})
	if err != nil {
		t.Fatal(err)
	}
	wantNamespace, wantArgoProject := domain.DeriveEnvironmentDestination(teamProject.Value, "dev")
	if environment.Value.Namespace != wantNamespace || environment.Value.ArgoProject != wantArgoProject {
		t.Fatalf("store accepted caller-owned destination: %#v", environment.Value)
	}
	application, err := store.CreateApplication(ctx, member.ID, "application", "application", domain.CreateApplication{ProjectID: teamProject.Value.ID, Name: "API", Slug: "api"})
	if err != nil {
		t.Fatal(err)
	}
	_, operation, err := store.CreateDeployment(ctx, member.ID, "deployment", "deployment", "request", domain.CreateDeployment{EnvironmentID: environment.Value.ID, ApplicationID: application.Value.ID, Image: "registry.test/api@sha256:" + strings.Repeat("a", 64), Replicas: 1, Port: 8080}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if visible, getErr := store.GetOperationForActor(ctx, member.ID, operation.ID); getErr != nil || visible.ID != operation.ID {
		t.Fatalf("member operation access=%#v err=%v", visible, getErr)
	}
	if _, getErr := store.GetOperationForActor(ctx, outsider.ID, operation.ID); !errors.Is(getErr, base.ErrNotFound) {
		t.Fatalf("outsider operation leaked: %v", getErr)
	}
	installation, err := store.CreateGitHubInstallation(ctx, admin.ID, "install", "install", "request", domain.CreateGitHubInstallation{GitHubInstallationID: 42, AccountLogin: "kuberploy", AccountType: "Organization", RepositorySelection: "selected", RepositoryCount: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.UpdateGitHubInstallationSharing(ctx, admin.ID, installation.Value.ID, "request", domain.UpdateGitHubInstallationSharing{Visibility: "team", TeamID: team.Value.ID}); err != nil {
		t.Fatal(err)
	}
	if accessible, listErr := store.ListGitHubInstallationsForActor(ctx, member.ID); listErr != nil || len(accessible) != 1 {
		t.Fatalf("member installation access=%#v err=%v", accessible, listErr)
	}
	if accessible, listErr := store.ListGitHubInstallationsForActor(ctx, outsider.ID); listErr != nil || len(accessible) != 0 {
		t.Fatalf("outsider installation leak=%#v err=%v", accessible, listErr)
	}
	if _, err = store.UpdateGitHubInstallationSharing(ctx, member.ID, installation.Value.ID, "request", domain.UpdateGitHubInstallationSharing{Visibility: "private"}); !errors.Is(err, base.ErrForbidden) {
		t.Fatalf("ordinary member reshared installation: %v", err)
	}
	serviceAccount, err := store.CreateServiceAccount(ctx, admin.ID, "build-account", "build-account", "request",
		domain.CreateServiceAccount{ProjectID: teamProject.Value.ID, Name: "Build agent", Role: domain.RoleProjectAdmin})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.AuthorizeGitHubInstallationForProject(ctx, serviceAccount.Value.ID, installation.Value.ID, teamProject.Value.ID); err != nil {
		t.Fatalf("project-admin service account could not use team-shared installation: %v", err)
	}
	if err = store.AuthorizeGitHubInstallationForProject(ctx, outsider.ID, installation.Value.ID, teamProject.Value.ID); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("outsider learned or used installation: %v", err)
	}
	if _, err = store.UpdateGitHubInstallationSharing(ctx, admin.ID, installation.Value.ID, "request", domain.UpdateGitHubInstallationSharing{Visibility: "private"}); err != nil {
		t.Fatal(err)
	}
	if err = store.AuthorizeGitHubInstallationForProject(ctx, serviceAccount.Value.ID, installation.Value.ID, teamProject.Value.ID); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("service account used private human installation: %v", err)
	}
}

func TestTeamOwnerUserDirectoryIsLimitedToManagedTeamMembers(t *testing.T) {
	ctx := context.Background()
	store := New()
	admin := bootstrapAccessAdmin(t, store)
	owner, _ := invitedUser(t, store, admin, "Owner", "owner")
	visible, _ := invitedUser(t, store, admin, "Visible", "visible")
	hidden, _ := invitedUser(t, store, admin, "Hidden", "hidden")
	team, err := store.CreateTeam(ctx, admin.ID, "team", "team", "request", domain.CreateTeam{Name: "Scoped", Slug: "scoped"})
	if err != nil {
		t.Fatal(err)
	}
	for key, input := range map[string]domain.AddTeamMember{"owner": {UserID: owner.ID, Role: "owner"}, "visible": {UserID: visible.ID, Role: "member"}} {
		if _, err = store.AddTeamMember(ctx, admin.ID, team.Value.ID, key, key, "request", input); err != nil {
			t.Fatal(err)
		}
	}
	users, err := store.ListUsersForActor(ctx, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, u := range users {
		ids[u.ID] = true
	}
	if !ids[owner.ID] || !ids[visible.ID] || ids[hidden.ID] {
		t.Fatalf("unsafe owner directory: %#v", ids)
	}
}

func TestExplicitScopedGrantsAreAdditiveFilteredAndSessionRevoking(t *testing.T) {
	ctx := context.Background()
	store := New()
	admin := bootstrapAccessAdmin(t, store)
	viewer, viewerSession := invitedUser(t, store, admin, "Scoped viewer", "scoped-viewer")
	team, err := store.CreateTeam(ctx, admin.ID, "team-scoped", "team-scoped", "request", domain.CreateTeam{Name: "Scoped", Slug: "scoped-access"})
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(ctx, admin.ID, "project-scoped", "project-scoped", domain.CreateProject{Name: "Payments", Slug: "payments-scoped", TeamID: team.Value.ID})
	if err != nil {
		t.Fatal(err)
	}
	environment, err := store.CreateEnvironment(ctx, admin.ID, "env-scoped", "env-scoped", domain.CreateEnvironment{ProjectID: project.Value.ID, Name: "Production", Slug: "production"})
	if err != nil {
		t.Fatal(err)
	}
	application, err := store.CreateApplication(ctx, admin.ID, "app-scoped", "app-scoped", domain.CreateApplication{ProjectID: project.Value.ID, Name: "API", Slug: "api-scoped"})
	if err != nil {
		t.Fatal(err)
	}
	siblingApplication, err := store.CreateApplication(ctx, admin.ID, "app-scoped-sibling", "app-scoped-sibling", domain.CreateApplication{ProjectID: project.Value.ID, Name: "Worker", Slug: "worker-scoped"})
	if err != nil {
		t.Fatal(err)
	}
	siblingProject, err := store.CreateProject(ctx, admin.ID, "project-scoped-sibling", "project-scoped-sibling", domain.CreateProject{Name: "Identity", Slug: "identity-scoped", TeamID: team.Value.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.GetProjectForActor(ctx, viewer.ID, project.Value.ID); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("ungranted project visible: %v", err)
	}
	auditsBeforeGrant := store.audits
	input := domain.CreateAccessGrant{ProjectID: project.Value.ID, SubjectUserID: viewer.ID, Role: domain.RoleViewer, ScopeType: domain.ScopeApplication, ScopeID: application.Value.ID, Permissions: []domain.Permission{domain.PermissionLogsRead}}
	grant, err := store.CreateProjectAccessGrant(ctx, admin.ID, "grant-viewer", "grant-viewer", "request", input)
	if err != nil || grant.Replay {
		t.Fatalf("create grant=%#v err=%v", grant, err)
	}
	if store.audits != auditsBeforeGrant+1 {
		t.Fatalf("grant creation was not audited exactly once: before=%d after=%d", auditsBeforeGrant, store.audits)
	}
	if _, err = store.UserBySession(ctx, viewerSession[:], time.Now()); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("grant change retained stale session: %v", err)
	}
	projects, err := store.ListProjectsForActor(ctx, viewer.ID)
	if err != nil || len(projects) != 1 || projects[0].ID != project.Value.ID {
		t.Fatalf("narrow grant did not expose safe project ancestry: %#v err=%v", projects, err)
	}
	applications, err := store.ListApplicationsForActor(ctx, viewer.ID)
	if err != nil || len(applications) != 1 || applications[0].ID != application.Value.ID {
		t.Fatalf("application grant visibility=%#v err=%v", applications, err)
	}
	if environments, listErr := store.ListEnvironmentsForActor(ctx, viewer.ID); listErr != nil || len(environments) != 0 {
		t.Fatalf("application grant leaked environments=%#v err=%v", environments, listErr)
	}
	if _, getErr := store.GetApplicationForActor(ctx, viewer.ID, siblingApplication.Value.ID); !errors.Is(getErr, base.ErrNotFound) {
		t.Fatalf("application viewer saw sibling application detail: %v", getErr)
	}
	if _, getErr := store.GetProjectForActor(ctx, viewer.ID, siblingProject.Value.ID); !errors.Is(getErr, base.ErrNotFound) {
		t.Fatalf("application viewer saw sibling project detail: %v", getErr)
	}
	if err = store.Authorize(ctx, viewer.ID, domain.PermissionLogsRead, domain.AccessTarget{Type: "application", ID: application.Value.ID}); err != nil {
		t.Fatalf("explicit logs.read ignored: %v", err)
	}
	secretTarget := domain.AccessTarget{Type: "secret-binding", ID: "binding", TeamID: team.Value.ID, ProjectID: project.Value.ID,
		EnvironmentID: environment.Value.ID, Namespace: environment.Value.Namespace, ApplicationID: application.Value.ID}
	if err = store.Authorize(ctx, viewer.ID, domain.PermissionSecretsRead, secretTarget); err != nil {
		t.Fatalf("application viewer could not read secret metadata at an exact application/environment intersection: %v", err)
	}
	if err = store.Authorize(ctx, viewer.ID, domain.PermissionSecretsBind, secretTarget); !errors.Is(err, base.ErrForbidden) {
		t.Fatalf("viewer bind permission err=%v, want forbidden", err)
	}
	tampered := secretTarget
	tampered.Namespace = "some-other-namespace"
	if err = store.Authorize(ctx, viewer.ID, domain.PermissionSecretsRead, tampered); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("tampered resolved ancestry err=%v, want not found", err)
	}
	_, _, err = store.CreateDeployment(ctx, viewer.ID, "viewer-deploy", "viewer-deploy", "request", domain.CreateDeployment{EnvironmentID: environment.Value.ID, ApplicationID: application.Value.ID, Image: "registry.test/api@sha256:" + strings.Repeat("b", 64), Replicas: 1, Port: 8080}, nil)
	if !errors.Is(err, base.ErrForbidden) {
		t.Fatalf("visible viewer mutation err=%v, want forbidden", err)
	}
	if err = store.AuthorizePromotion(ctx, viewer.ID, environment.Value.ID, application.Value.ID); !errors.Is(err, base.ErrForbidden) {
		t.Fatalf("promotion composite write err=%v, want forbidden", err)
	}
	outsider, _ := invitedUser(t, store, admin, "Outsider mutation", "outsider-mutation")
	_, _, err = store.CreateDeployment(ctx, outsider.ID, "outsider-deploy", "outsider-deploy", "request", domain.CreateDeployment{EnvironmentID: environment.Value.ID, ApplicationID: application.Value.ID, Image: "registry.test/api@sha256:" + strings.Repeat("c", 64), Replicas: 1, Port: 8080}, nil)
	if !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("inaccessible mutation err=%v, want not found", err)
	}
	if err = store.AuthorizePromotion(ctx, outsider.ID, environment.Value.ID, application.Value.ID); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("inaccessible promotion err=%v, want not found", err)
	}
	if err = store.AuthorizePromotion(ctx, admin.ID, environment.Value.ID, application.Value.ID); err != nil {
		t.Fatalf("platform admin promotion composite denied: %v", err)
	}
	currentViewerSession := issueSession(t, store, viewer.ID, "scoped-viewer-after-grant")
	auditsBeforeDelete := store.audits
	replay, err := store.DeleteProjectAccessGrant(ctx, admin.ID, project.Value.ID, grant.Value.ID, "delete-viewer", "delete-viewer", "request")
	if err != nil || replay {
		t.Fatalf("delete replay=%v err=%v", replay, err)
	}
	if _, sessionErr := store.UserBySession(ctx, currentViewerSession[:], time.Now()); !errors.Is(sessionErr, base.ErrNotFound) {
		t.Fatalf("grant removal retained an already-issued session: %v", sessionErr)
	}
	if store.audits != auditsBeforeDelete+1 {
		t.Fatalf("grant deletion was not audited exactly once: before=%d after=%d", auditsBeforeDelete, store.audits)
	}
	replay, err = store.DeleteProjectAccessGrant(ctx, admin.ID, project.Value.ID, grant.Value.ID, "delete-viewer", "delete-viewer", "request")
	if err != nil || !replay {
		t.Fatalf("idempotent delete replay=%v err=%v", replay, err)
	}
	if store.audits != auditsBeforeDelete+1 {
		t.Fatalf("idempotent grant deletion replay emitted another audit: %d", store.audits)
	}
}

func TestGrantDelegationCannotExceedManagerRoleOrScope(t *testing.T) {
	ctx := context.Background()
	store := New()
	admin := bootstrapAccessAdmin(t, store)
	manager, _ := invitedUser(t, store, admin, "Project manager", "project-manager")
	target, _ := invitedUser(t, store, admin, "Grant target", "grant-target")
	team, err := store.CreateTeam(ctx, admin.ID, "team-delegation", "team-delegation", "request", domain.CreateTeam{Name: "Delegation", Slug: "delegation"})
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(ctx, admin.ID, "project-delegation", "project-delegation", domain.CreateProject{Name: "Delegation", Slug: "delegation-project", TeamID: team.Value.ID})
	if err != nil {
		t.Fatal(err)
	}
	managerGrant := domain.CreateAccessGrant{ProjectID: project.Value.ID, SubjectUserID: manager.ID, Role: domain.RoleProjectAdmin, ScopeType: domain.ScopeProject, ScopeID: project.Value.ID}
	if _, err = store.CreateProjectAccessGrant(ctx, admin.ID, "manager-grant", "manager-grant", "request", managerGrant); err != nil {
		t.Fatal(err)
	}
	viewerGrant := domain.CreateAccessGrant{ProjectID: project.Value.ID, SubjectUserID: target.ID, Role: domain.RoleViewer, ScopeType: domain.ScopeProject, ScopeID: project.Value.ID}
	if _, err = store.CreateProjectAccessGrant(ctx, manager.ID, "viewer-grant", "viewer-grant", "request", viewerGrant); err != nil {
		t.Fatalf("project admin could not delegate viewer: %v", err)
	}
	broader := domain.CreateAccessGrant{ProjectID: project.Value.ID, SubjectUserID: target.ID, Role: domain.RoleOrganizationAdmin, ScopeType: domain.ScopeTeam, ScopeID: team.Value.ID}
	if _, err = store.CreateProjectAccessGrant(ctx, manager.ID, "broader-grant", "broader-grant", "request", broader); !errors.Is(err, base.ErrForbidden) {
		t.Fatalf("project admin delegated organization admin: %v", err)
	}
	organizationGrant, err := store.CreateProjectAccessGrant(ctx, admin.ID, "organization-grant", "organization-grant", "request", broader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.DeleteProjectAccessGrant(ctx, manager.ID, project.Value.ID, organizationGrant.Value.ID, "delete-organization-grant", "delete-organization-grant", "request"); !errors.Is(err, base.ErrForbidden) {
		t.Fatalf("project admin deleted team-scope organization admin: %v", err)
	}
}
