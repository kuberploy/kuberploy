package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/passwordauth"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func TestMigrationsAreIdempotent(t *testing.T) {
	url := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := context.Background()
	st, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err = Migrate(ctx, st.pool); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, st.pool); err != nil {
		t.Fatal(err)
	}
}

func TestTeamAccessSQLPaths(t *testing.T) {
	url := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := context.Background()
	st, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	adminSession := sha256.Sum256([]byte("integration-admin-session"))
	admin := domain.User{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Login: "Admin", Role: "platform-admin", Issuer: "integration", Subject: "admin", GrantRevision: 1, CreatedAt: time.Now().UTC()}
	adminPassword := "integration admin password 123"
	adminPasswordHash, err := passwordauth.Hash(adminPassword)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.BootstrapAdmin(ctx, admin, adminPasswordHash, adminSession[:], time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	invite := sha256.Sum256([]byte("integration-invitation"))
	if _, err = st.CreateUserInvitation(ctx, admin.ID, "Developer", invite[:], time.Now().Add(time.Hour), "request"); err != nil {
		t.Fatal(err)
	}
	developerSession := sha256.Sum256([]byte("integration-developer-session"))
	developerPassword := "integration developer password 123"
	developerPasswordHash, err := passwordauth.Hash(developerPassword)
	if err != nil {
		t.Fatal(err)
	}
	developer, err := st.AcceptUserInvitation(ctx, invite[:], "Developer", developerPasswordHash, developerSession[:], time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	t.Run("local credential lookup rehash and session CAS", func(t *testing.T) {
		credentialUser, storedHash, lookupErr := st.LocalCredential(ctx, "  dEvElOpEr  ")
		if lookupErr != nil || credentialUser.ID != developer.ID || storedHash != developerPasswordHash || storedHash == developerPassword {
			t.Fatalf("normalized credential user=%#v hashMatch=%t plaintext=%t err=%v", credentialUser, storedHash == developerPasswordHash, storedHash == developerPassword, lookupErr)
		}
		upgradedHash, hashErr := passwordauth.Hash(developerPassword)
		if hashErr != nil || upgradedHash == developerPasswordHash {
			t.Fatalf("upgraded hash was not independently salted: equal=%t err=%v", upgradedHash == developerPasswordHash, hashErr)
		}
		loginSession := sha256.Sum256([]byte("integration-recurring-login-session"))
		loggedIn, loginErr := st.CreateLoginSession(ctx, developer.ID, developerPasswordHash, upgradedHash, loginSession[:], time.Now().Add(time.Hour))
		if loginErr != nil || loggedIn.ID != developer.ID {
			t.Fatalf("login session user=%#v err=%v", loggedIn, loginErr)
		}
		if sessionUser, sessionErr := st.UserBySession(ctx, loginSession[:], time.Now()); sessionErr != nil || sessionUser.ID != developer.ID {
			t.Fatalf("login session lookup=%#v err=%v", sessionUser, sessionErr)
		}
		staleSession := sha256.Sum256([]byte("integration-stale-hash-session"))
		if _, staleErr := st.CreateLoginSession(ctx, developer.ID, developerPasswordHash, "", staleSession[:], time.Now().Add(time.Hour)); !errors.Is(staleErr, base.ErrNotFound) {
			t.Fatalf("stale credential CAS err=%v", staleErr)
		}
		var persistedHash string
		if queryErr := st.pool.QueryRow(ctx, `SELECT password_hash FROM user_password_credentials WHERE user_id=$1`, developer.ID).Scan(&persistedHash); queryErr != nil || persistedHash != upgradedHash || persistedHash == developerPassword {
			t.Fatalf("persisted hashMatch=%t plaintext=%t err=%v", persistedHash == upgradedHash, persistedHash == developerPassword, queryErr)
		}
		var plaintextColumns int
		if queryErr := st.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='user_password_credentials' AND column_name IN ('password','plaintext','password_plaintext')`).Scan(&plaintextColumns); queryErr != nil || plaintextColumns != 0 {
			t.Fatalf("plaintext credential columns=%d err=%v", plaintextColumns, queryErr)
		}
	})
	team, err := st.CreateTeam(ctx, admin.ID, "team", "team", "request", domain.CreateTeam{Name: "Platform", Slug: "platform"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.AddTeamMember(ctx, admin.ID, team.Value.ID, "member", "member", "request", domain.AddTeamMember{UserID: developer.ID, Role: "member"}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.UserBySession(ctx, developerSession[:], time.Now()); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("stale session survived grant revision change: %v", err)
	}
	project, err := st.CreateProject(ctx, admin.ID, "project", "project", domain.CreateProject{Name: "Team project", Slug: "team-project", TeamID: team.Value.ID})
	if err != nil {
		t.Fatal(err)
	}
	adminAudit, err := st.ListAuditEventsForActor(ctx, admin.ID, domain.AuditEventQuery{Action: "project.create", Limit: 20})
	if err != nil || len(adminAudit) == 0 {
		t.Fatalf("platform audit timeline=%#v err=%v", adminAudit, err)
	}
	scopedAudit, err := st.ListAuditEventsForActor(ctx, developer.ID, domain.AuditEventQuery{TargetType: "project", TargetID: project.Value.ID, Action: "project.create", Limit: 20})
	if err != nil || len(scopedAudit) != 1 || scopedAudit[0].TargetID != project.Value.ID || scopedAudit[0].Outcome != "recorded" {
		t.Fatalf("scoped audit timeline=%#v err=%v", scopedAudit, err)
	}
	if _, err = st.ListAuditEventsForActor(ctx, developer.ID, domain.AuditEventQuery{Limit: 20}); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("scoped global audit err=%v", err)
	}
	serviceAccount, err := st.CreateServiceAccount(ctx, admin.ID, "service-account", "service-account", "request", domain.CreateServiceAccount{ProjectID: project.Value.ID, Name: "Deploy agent", Role: domain.RoleDeveloper})
	if err != nil {
		t.Fatal(err)
	}
	serviceTokenRaw := []byte("integration-service-account-token")
	serviceTokenHash := sha256.Sum256(serviceTokenRaw)
	serviceToken, err := st.CreateServiceAccountToken(ctx, admin.ID, "service-token", "service-token", "request", domain.CreateServiceAccountToken{ServiceAccountID: serviceAccount.Value.ID, Name: "CI", Prefix: "kp_sa_integra0", TokenHash: serviceTokenHash[:], Scopes: []domain.AutomationScope{domain.AutomationScopeAppEdit, domain.AutomationScopeAppRead}, ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if replay, found, replayErr := st.ServiceAccountTokenReplay(ctx, admin.ID, serviceAccount.Value.ID, "service-token", "service-token"); replayErr != nil || !found || !replay.Replay || replay.Value.ID != serviceToken.Value.ID {
		t.Fatalf("service token replay lookup=%#v found=%t err=%v", replay, found, replayErr)
	}
	principal, err := st.ServiceAccountByToken(ctx, serviceTokenHash[:], time.Now())
	if err != nil || principal.User.ID != serviceAccount.Value.ID || principal.TokenID != serviceToken.Value.ID {
		t.Fatalf("service-account principal=%#v err=%v", principal, err)
	}
	users, err := st.ListUsersForActor(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range users {
		if user.ID == serviceAccount.Value.ID {
			t.Fatal("PostgreSQL service account leaked into the human user directory")
		}
	}
	if _, err = st.AddTeamMember(ctx, admin.ID, team.Value.ID, "service-account-member", "service-account-member", "request", domain.AddTeamMember{UserID: serviceAccount.Value.ID, Role: "member"}); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("PostgreSQL accepted service account as a team member: %v", err)
	}
	var storedHash []byte
	var storedPrefix string
	if err = st.pool.QueryRow(ctx, `SELECT token_hash,token_prefix FROM service_account_tokens WHERE id=$1`, serviceToken.Value.ID).Scan(&storedHash, &storedPrefix); err != nil || !strings.EqualFold(storedPrefix, "kp_sa_integra0") || string(storedHash) != string(serviceTokenHash[:]) || string(storedHash) == string(serviceTokenRaw) {
		t.Fatalf("stored service token prefix=%q hash=%x err=%v", storedPrefix, storedHash, err)
	}
	if replay, revokeErr := st.RevokeServiceAccountToken(ctx, admin.ID, serviceAccount.Value.ID, serviceToken.Value.ID, "revoke-service-token", "revoke-service-token", "request"); revokeErr != nil || replay {
		t.Fatalf("service token revoke replay=%t err=%v", replay, revokeErr)
	}
	if _, err = st.ServiceAccountByToken(ctx, serviceTokenHash[:], time.Now()); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("revoked PostgreSQL service token error=%v", err)
	}
	projects, err := st.ListProjectsForActor(ctx, developer.ID)
	if err != nil || len(projects) != 1 || projects[0].ID != project.Value.ID {
		t.Fatalf("developer projects=%#v err=%v", projects, err)
	}
	environment, err := st.CreateEnvironment(ctx, developer.ID, "environment", "environment", domain.CreateEnvironment{ProjectID: project.Value.ID, Name: "Dev", Slug: "dev", Namespace: "caller-controlled", ArgoProject: "caller-controlled"})
	if err != nil {
		t.Fatal(err)
	}
	wantNamespace, wantArgoProject := domain.DeriveEnvironmentDestination(project.Value, "dev")
	if environment.Value.Namespace != wantNamespace || environment.Value.ArgoProject != wantArgoProject {
		t.Fatalf("PostgreSQL accepted caller-owned destination: %#v", environment.Value)
	}
	application, err := st.CreateApplication(ctx, developer.ID, "application", "application", domain.CreateApplication{ProjectID: project.Value.ID, Name: "API", Slug: "api"})
	if err != nil {
		t.Fatal(err)
	}
	siblingApplication, err := st.CreateApplication(ctx, developer.ID, "sibling-application", "sibling-application", domain.CreateApplication{ProjectID: project.Value.ID, Name: "Worker", Slug: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	siblingProject, err := st.CreateProject(ctx, developer.ID, "sibling-project", "sibling-project", domain.CreateProject{Name: "Identity", Slug: "identity", TeamID: team.Value.ID})
	if err != nil {
		t.Fatal(err)
	}
	deployment, operation, err := st.CreateDeployment(ctx, developer.ID, "deployment", "deployment", "request", domain.CreateDeployment{EnvironmentID: environment.Value.ID, ApplicationID: application.Value.ID, Image: "registry.test/api@sha256:" + strings.Repeat("a", 64), Replicas: 1, Port: 8080}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rolloutRevision := strings.Repeat("9", 40)
	rolloutNow := time.Now().UTC().Truncate(time.Microsecond)
	if _, err = st.pool.Exec(ctx, `UPDATE deployments SET desired_revision=$2 WHERE id=$1`, deployment.Value.ID, rolloutRevision); err != nil {
		t.Fatal(err)
	}
	if _, err = st.pool.Exec(ctx, `INSERT INTO argo_application_observations(deployment_id,application_id,project_id,environment_id,argo_uid,argo_namespace,argo_name,destination_namespace,desired_revision,observed_revision,sync_status,health_status,operation_phase,message,resources,observed_at,updated_at)
		VALUES($1,$2,$3,$4,$5,'argocd','kp-test',$6,$7,$7,'synced','healthy','','','[]'::jsonb,$8,$8)`, deployment.Value.ID, application.Value.ID, project.Value.ID, environment.Value.ID, id.New(), environment.Value.Namespace, rolloutRevision, rolloutNow); err != nil {
		t.Fatal(err)
	}
	rolloutStatus, err := st.DeploymentStatusForActor(ctx, developer.ID, deployment.Value.ID)
	if err != nil || rolloutStatus.ArgoSyncStatus != "synced" || rolloutStatus.RolloutHealth != "healthy" || rolloutStatus.ArgoObservedRevision != rolloutRevision || rolloutStatus.ArgoObservedAt == nil {
		t.Fatalf("exact PostgreSQL rollout status=%#v err=%v", rolloutStatus, err)
	}
	if _, err = st.pool.Exec(ctx, `UPDATE argo_application_observations SET observed_revision=$2 WHERE deployment_id=$1`, deployment.Value.ID, strings.Repeat("7", 40)); err != nil {
		t.Fatal(err)
	}
	rolloutStatus, err = st.DeploymentStatusForActor(ctx, developer.ID, deployment.Value.ID)
	if err != nil || rolloutStatus.ArgoSyncStatus != "unknown" || rolloutStatus.RolloutHealth != "unknown" || rolloutStatus.ArgoObservedRevision != "" || rolloutStatus.ArgoObservedAt != nil {
		t.Fatalf("contradictory PostgreSQL synced/healthy revision leaked=%#v err=%v", rolloutStatus, err)
	}
	if _, err = st.pool.Exec(ctx, `UPDATE argo_application_observations SET observed_revision=$2,sync_status='out-of-sync',health_status='degraded',observed_at=$3,updated_at=$3 WHERE deployment_id=$1`, deployment.Value.ID, strings.Repeat("8", 40), rolloutNow.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	rolloutStatus, err = st.DeploymentStatus(ctx, deployment.Value.ID)
	if err != nil || rolloutStatus.ArgoSyncStatus != "out-of-sync" || rolloutStatus.RolloutHealth != "degraded" {
		t.Fatalf("degraded PostgreSQL rollout status=%#v err=%v", rolloutStatus, err)
	}
	if _, err = st.pool.Exec(ctx, `UPDATE argo_application_observations SET observed_at=$2,updated_at=$2 WHERE deployment_id=$1`, deployment.Value.ID, rolloutNow.Add(-21*time.Minute)); err != nil {
		t.Fatal(err)
	}
	rolloutStatus, err = st.DeploymentStatus(ctx, deployment.Value.ID)
	if err != nil || rolloutStatus.ArgoSyncStatus != "unknown" || rolloutStatus.RolloutHealth != "unknown" || rolloutStatus.ArgoObservedAt != nil {
		t.Fatalf("stale PostgreSQL rollout leaked status=%#v err=%v", rolloutStatus, err)
	}
	viewerInvite := sha256.Sum256([]byte("integration-scoped-viewer-invitation"))
	if _, err = st.CreateUserInvitation(ctx, admin.ID, "Scoped Viewer", viewerInvite[:], time.Now().Add(time.Hour), "request"); err != nil {
		t.Fatal(err)
	}
	viewerSession := sha256.Sum256([]byte("integration-scoped-viewer-session"))
	viewer, err := st.AcceptUserInvitation(ctx, viewerInvite[:], "Scoped Viewer", strings.Repeat("h", 64), viewerSession[:], time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, constraintErr := st.pool.Exec(ctx, `INSERT INTO access_grants(id,subject_user_id,role,scope_type,scope_id,permissions,source,created_by) VALUES($1,$2,'organization-admin','application',$3,ARRAY[]::text[],'explicit',$4)`, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", viewer.ID, application.Value.ID, admin.ID); constraintErr == nil {
		t.Fatal("migration accepted an organization administrator outside team scope")
	}
	viewerGrant := domain.CreateAccessGrant{ProjectID: project.Value.ID, SubjectUserID: viewer.ID, Role: domain.RoleViewer, ScopeType: domain.ScopeApplication, ScopeID: application.Value.ID, Permissions: []domain.Permission{domain.PermissionLogsRead}}
	createdGrant, err := st.CreateProjectAccessGrant(ctx, admin.ID, "scoped-viewer-grant", "scoped-viewer-grant", "request", viewerGrant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.UserBySession(ctx, viewerSession[:], time.Now()); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("scoped grant retained stale PostgreSQL session: %v", err)
	}
	if visibleProjects, listErr := st.ListProjectsForActor(ctx, viewer.ID); listErr != nil || len(visibleProjects) != 1 || visibleProjects[0].ID != project.Value.ID {
		t.Fatalf("scoped viewer project ancestry=%#v err=%v", visibleProjects, listErr)
	}
	if visibleApplications, listErr := st.ListApplicationsForActor(ctx, viewer.ID); listErr != nil || len(visibleApplications) != 1 || visibleApplications[0].ID != application.Value.ID {
		t.Fatalf("scoped viewer applications=%#v err=%v", visibleApplications, listErr)
	}
	if _, getErr := st.GetApplicationForActor(ctx, viewer.ID, siblingApplication.Value.ID); !errors.Is(getErr, base.ErrNotFound) {
		t.Fatalf("scoped PostgreSQL viewer saw sibling application detail: %v", getErr)
	}
	if _, getErr := st.GetProjectForActor(ctx, viewer.ID, siblingProject.Value.ID); !errors.Is(getErr, base.ErrNotFound) {
		t.Fatalf("scoped PostgreSQL viewer saw sibling project detail: %v", getErr)
	}
	if visibleEnvironments, listErr := st.ListEnvironmentsForActor(ctx, viewer.ID); listErr != nil || len(visibleEnvironments) != 0 {
		t.Fatalf("application grant leaked PostgreSQL environments=%#v err=%v", visibleEnvironments, listErr)
	}
	if visibleDeployments, listErr := st.ListDeploymentsForActor(ctx, viewer.ID); listErr != nil || len(visibleDeployments) != 1 || visibleDeployments[0].ID != deployment.Value.ID {
		t.Fatalf("scoped viewer deployments=%#v err=%v", visibleDeployments, listErr)
	}
	if _, _, mutationErr := st.CreateDeployment(ctx, viewer.ID, "viewer-release", "viewer-release", "request", domain.CreateDeployment{EnvironmentID: environment.Value.ID, ApplicationID: application.Value.ID, Image: "registry.test/api@sha256:" + strings.Repeat("d", 64), Replicas: 1, Port: 8080}, nil); !errors.Is(mutationErr, base.ErrForbidden) {
		t.Fatalf("scoped PostgreSQL viewer mutation err=%v", mutationErr)
	}
	var viewerRevision int64
	if err = st.pool.QueryRow(ctx, `SELECT grant_revision FROM users WHERE id=$1`, viewer.ID).Scan(&viewerRevision); err != nil {
		t.Fatal(err)
	}
	currentViewerSession := sha256.Sum256([]byte("integration-current-scoped-viewer-session"))
	if _, err = st.pool.Exec(ctx, `INSERT INTO sessions(token_hash,user_id,grant_revision,expires_at) VALUES($1,$2,$3,$4)`, currentViewerSession[:], viewer.ID, viewerRevision, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = st.UserBySession(ctx, currentViewerSession[:], time.Now()); err != nil {
		t.Fatalf("new scoped PostgreSQL viewer session was not current: %v", err)
	}
	if replay, deleteErr := st.DeleteProjectAccessGrant(ctx, admin.ID, project.Value.ID, createdGrant.Value.ID, "delete-scoped-viewer", "delete-scoped-viewer", "request"); deleteErr != nil || replay {
		t.Fatalf("delete scoped grant replay=%v err=%v", replay, deleteErr)
	}
	if _, err = st.UserBySession(ctx, currentViewerSession[:], time.Now()); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("removed PostgreSQL grant retained an already-issued session: %v", err)
	}
	if replay, deleteErr := st.DeleteProjectAccessGrant(ctx, admin.ID, project.Value.ID, createdGrant.Value.ID, "delete-scoped-viewer", "delete-scoped-viewer", "request"); deleteErr != nil || !replay {
		t.Fatalf("replay scoped grant delete=%v err=%v", replay, deleteErr)
	}
	var grantAuditCount int
	if err = st.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE target_type='access-grant' AND target_id=$1 AND action IN ('access-grant.create','access-grant.delete')`, createdGrant.Value.ID).Scan(&grantAuditCount); err != nil || grantAuditCount != 2 {
		t.Fatalf("PostgreSQL grant audit count=%d err=%v", grantAuditCount, err)
	}
	if _, err = st.GetOperationForActor(ctx, developer.ID, operation.ID); err != nil {
		t.Fatal(err)
	}
	initialOperationInput, err := st.GetDeploymentForOperation(ctx, operation.ID)
	if err != nil || len(initialOperationInput.ConfigRaw) == 0 {
		t.Fatalf("initial operation lacks exact accepted config: %q err=%v", initialOperationInput.ConfigRaw, err)
	}
	configState, err := st.GetDeploymentConfigForActor(ctx, developer.ID, deployment.Value.ID)
	if err != nil || configState.ETag == "" {
		t.Fatalf("initialize config state=%#v err=%v", configState, err)
	}
	if string(configState.RawYAML) != string(initialOperationInput.ConfigRaw) {
		t.Fatalf("current config differs from accepted operation snapshot")
	}
	candidate := appconfig.Apply(configState.RawYAML, appconfig.Change{Mode: "jsonPatch", Patch: []appconfig.PatchOperation{{Op: "replace", Path: "/spec/runtime/replicas", Value: 2}}})
	if len(candidate.Diagnostics) != 0 {
		t.Fatalf("candidate diagnostics=%#v", candidate.Diagnostics)
	}
	previewTokenHash := sha256.Sum256([]byte("integration-config-preview-token"))
	if err = st.CreateDeploymentConfigPreview(ctx, developer.ID, domain.CreateConfigPreview{DeploymentID: deployment.Value.ID, BaseETag: configState.ETag, TokenHash: previewTokenHash[:], CandidateHash: candidate.Hash, ExpiresAt: time.Now().Add(time.Hour)}, nil); err != nil {
		t.Fatal(err)
	}
	configInput := domain.SaveDeploymentConfig{DeploymentID: deployment.Value.ID, BaseETag: configState.ETag, TokenHash: previewTokenHash[:], CandidateHash: candidate.Hash, RawYAML: candidate.Raw, Runtime: candidate.Runtime}
	if _, _, err = st.SaveDeploymentConfig(ctx, admin.ID, "actor-bound-preview", "actor-bound-preview", "request", configInput, nil); !errors.Is(err, base.ErrPreviewInvalid) {
		t.Fatalf("preview token crossed actor boundary: %v", err)
	}
	saved, configOperation, err := st.SaveDeploymentConfig(ctx, developer.ID, "config-save", "config-save", "request", configInput, nil)
	if err != nil || saved.Replay {
		t.Fatalf("save config result=%#v op=%#v err=%v", saved, configOperation, err)
	}
	exact, err := st.GetDeploymentForOperation(ctx, configOperation.ID)
	if err != nil || string(exact.ConfigRaw) != string(candidate.Raw) || exact.Image != deployment.Value.Image {
		t.Fatalf("exact config snapshot=%q image=%q err=%v", exact.ConfigRaw, exact.Image, err)
	}
	replayedConfig, replayedOperation, err := st.SaveDeploymentConfig(ctx, developer.ID, "config-save", "config-save", "request", domain.SaveDeploymentConfig{DeploymentID: deployment.Value.ID, BaseETag: configState.ETag, TokenHash: previewTokenHash[:]}, nil)
	if err != nil || !replayedConfig.Replay || replayedOperation.ID != configOperation.ID {
		t.Fatalf("config replay after consume/ETag advance result=%#v op=%#v err=%v", replayedConfig, replayedOperation, err)
	}
	installation, err := st.CreateGitHubInstallation(ctx, admin.ID, "installation", "installation", "request", domain.CreateGitHubInstallation{GitHubInstallationID: 42, AccountLogin: "kuberploy", AccountType: "Organization", RepositorySelection: "selected", RepositoryCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.UpdateGitHubInstallationSharing(ctx, admin.ID, installation.Value.ID, "request", domain.UpdateGitHubInstallationSharing{Visibility: "team", TeamID: team.Value.ID}); err != nil {
		t.Fatal(err)
	}
	accessible, err := st.ListGitHubInstallationsForActor(ctx, developer.ID)
	if err != nil || len(accessible) != 1 {
		t.Fatalf("accessible installations=%#v err=%v", accessible, err)
	}
	verifiedAt := time.Now().UTC()
	if _, err = st.pool.Exec(ctx, `UPDATE github_installations
		SET github_app_id=4242,github_account_id=777,permissions='{"metadata":"read","contents":"write"}'::jsonb,
			lifecycle='active',last_verified_at=$2,updated_at=$2
		WHERE id=$1`, installation.Value.ID, verifiedAt); err != nil {
		t.Fatal(err)
	}
	repositoryCatalogID := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	if _, err = st.pool.Exec(ctx, `INSERT INTO github_repositories(
		id,installation_id,github_repository_id,github_owner_id,owner_login,name,lifecycle,last_verified_at,created_at,updated_at)
		VALUES($1,$2,42001,777,'kuberploy','platform-config','active',$3,$3,$3)`,
		repositoryCatalogID, installation.Value.ID, verifiedAt); err != nil {
		t.Fatal(err)
	}
	bindingInput := gitprojection.CreateEnvironmentBindingInput{
		EnvironmentID:        environment.Value.ID,
		LinkedInstallationID: installation.Value.ID,
		LinkedRepositoryID:   repositoryCatalogID,
		GitHubAppID:          4242,
		Repository: gitprojection.RepositoryIdentity{
			Provider:       "github",
			InstallationID: 42,
			RepositoryID:   42001,
			Owner:          "kuberploy",
			Name:           "platform-config",
		},
		TargetRef: "refs/heads/main",
	}
	createdBinding, err := st.CreateEnvironmentGitBinding(ctx, admin.ID, "git-binding", "git-binding", "request", bindingInput)
	if err != nil || createdBinding.Replay || createdBinding.Value.CredentialMode != gitprojection.CredentialGitHubApp || createdBinding.Value.CredentialSecretName != "" {
		t.Fatalf("created Git binding=%#v err=%v", createdBinding, err)
	}
	wantBindingPrefix := "tenants/" + project.Value.ID + "/environments/" + environment.Value.ID
	if createdBinding.Value.Prefix != wantBindingPrefix || createdBinding.Value.Repository.RepositoryID != 42001 {
		t.Fatalf("Git binding authority was not server-derived: %#v", createdBinding.Value)
	}
	replayedBinding, err := st.CreateEnvironmentGitBinding(ctx, admin.ID, "git-binding", "git-binding", "request-replay", bindingInput)
	if err != nil || !replayedBinding.Replay || replayedBinding.Value.ID != createdBinding.Value.ID {
		t.Fatalf("Git binding replay=%#v err=%v", replayedBinding, err)
	}
	if _, err = st.CreateEnvironmentGitBinding(ctx, admin.ID, "git-binding", "different-fingerprint", "request-conflict", bindingInput); !errors.Is(err, base.ErrIdempotencyConflict) {
		t.Fatalf("Git binding accepted changed idempotency fingerprint: %v", err)
	}
	if _, err = st.CreateEnvironmentGitBinding(ctx, developer.ID, "developer-git-binding", "developer-git-binding", "request", bindingInput); !errors.Is(err, base.ErrForbidden) {
		t.Fatalf("developer created Git authority without builds.manage: %v", err)
	}
	readBinding, err := st.GetEnvironmentGitBindingForActor(ctx, admin.ID, environment.Value.ID)
	if err != nil || readBinding.ID != createdBinding.Value.ID {
		t.Fatalf("read Git binding=%#v err=%v", readBinding, err)
	}
	if _, err = st.pool.Exec(ctx, `UPDATE git_repository_bindings SET credential_mode='legacy-secret' WHERE id=$1`, createdBinding.Value.ID); err == nil {
		t.Fatal("Git binding credential authority was mutable")
	}
	if _, err = st.AddTeamMember(ctx, admin.ID, team.Value.ID, "demote-sole-owner", "demote-sole-owner", "request", domain.AddTeamMember{UserID: admin.ID, Role: "member"}); !errors.Is(err, base.ErrConflict) {
		t.Fatalf("demoting sole PostgreSQL owner err=%v", err)
	}
	if _, err = st.AddTeamMember(ctx, admin.ID, team.Value.ID, "promote-developer", "promote-developer", "request", domain.AddTeamMember{UserID: developer.ID, Role: "owner"}); err != nil {
		t.Fatal(err)
	}
	mutationErrors := make(chan error, 2)
	go func() {
		_, mutationErr := st.AddTeamMember(ctx, admin.ID, team.Value.ID, "demote-admin-concurrently", "demote-admin-concurrently", "request", domain.AddTeamMember{UserID: admin.ID, Role: "member"})
		mutationErrors <- mutationErr
	}()
	go func() {
		_, mutationErr := st.AddTeamMember(ctx, admin.ID, team.Value.ID, "demote-developer-concurrently", "demote-developer-concurrently", "request", domain.AddTeamMember{UserID: developer.ID, Role: "member"})
		mutationErrors <- mutationErr
	}()
	succeeded, conflicted := 0, 0
	for range 2 {
		mutationErr := <-mutationErrors
		switch {
		case mutationErr == nil:
			succeeded++
		case errors.Is(mutationErr, base.ErrConflict):
			conflicted++
		default:
			t.Fatalf("concurrent membership mutation err=%v", mutationErr)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("serialized demotions succeeded=%d conflicted=%d", succeeded, conflicted)
	}
	var owners int
	if err = st.pool.QueryRow(ctx, `SELECT count(*) FROM team_memberships WHERE team_id=$1 AND role='owner'`, team.Value.ID).Scan(&owners); err != nil || owners != 1 {
		t.Fatalf("owners after concurrent demotions=%d err=%v", owners, err)
	}
	for key, input := range map[string]domain.AddTeamMember{
		"restore-admin-owner":     {UserID: admin.ID, Role: "owner"},
		"restore-developer-owner": {UserID: developer.ID, Role: "owner"},
	} {
		if _, err = st.AddTeamMember(ctx, admin.ID, team.Value.ID, key, key, "request", input); err != nil {
			t.Fatal(err)
		}
	}
	var developerRevision int64
	if err = st.pool.QueryRow(ctx, `SELECT grant_revision FROM users WHERE id=$1`, developer.ID).Scan(&developerRevision); err != nil {
		t.Fatal(err)
	}
	currentDeveloperSession := sha256.Sum256([]byte("integration-current-developer-session"))
	if _, err = st.pool.Exec(ctx, `INSERT INTO sessions(token_hash,user_id,grant_revision,expires_at) VALUES($1,$2,$3,$4)`, currentDeveloperSession[:], developer.ID, developerRevision, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err = st.RemoveTeamMember(ctx, admin.ID, team.Value.ID, developer.ID, "request"); err != nil {
		t.Fatal(err)
	}
	if _, err = st.UserBySession(ctx, currentDeveloperSession[:], time.Now()); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("removed PostgreSQL member retained session: %v", err)
	}
	if err = st.RemoveTeamMember(ctx, admin.ID, team.Value.ID, admin.ID, "request"); !errors.Is(err, base.ErrConflict) {
		t.Fatalf("removing final PostgreSQL owner err=%v", err)
	}
	if err = st.RemoveTeamMember(ctx, admin.ID, team.Value.ID, developer.ID, "request"); err != nil {
		t.Fatalf("repeated PostgreSQL removal should be idempotent: %v", err)
	}
}
