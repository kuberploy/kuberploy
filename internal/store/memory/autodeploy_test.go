package memory

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/autodeploy"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitops"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func autoDeployMemoryTemplate(t *testing.T, project domain.Project, environment domain.Environment, application domain.Application, deployment domain.Deployment) (autodeploy.Template, string) {
	t.Helper()
	raw, err := gitops.RenderAppConfig(project, environment, application, deployment)
	if err != nil {
		t.Fatal(err)
	}
	intent, _, diagnostics := appconfig.AutoDeployIntentTemplate(raw)
	if len(diagnostics) != 0 {
		t.Fatalf("template diagnostics=%#v", diagnostics)
	}
	intent, digest, err := appconfig.BindAutoDeployDependencies(intent, []appconfig.AutoDeployDependencyIntent{{Path: "variables/project.yaml"}, {Path: "variables/environment.yaml"}})
	if err != nil {
		t.Fatal(err)
	}
	return autodeploy.Template{SourceDeploymentID: deployment.ID, SourceDeploymentGeneration: deployment.Generation,
		SourceConfigETag: `"sha256:` + strings.Repeat("a", 64) + `"`, ConfigIntent: intent}, digest
}

func TestAutoDeployPolicyStoreAuthorizesBeforeSideEffectsAndReplaysBeforeMutableAuthority(t *testing.T) {
	ctx := t.Context()
	store := New()
	admin := bootstrapAccessAdmin(t, store)
	projectResult, _ := store.CreateProject(ctx, admin.ID, "ad-project-00001", "ad-project-00001", domain.CreateProject{Name: "Auto", Slug: "auto"})
	environmentResult, _ := store.CreateEnvironment(ctx, admin.ID, "ad-environment-1", "ad-environment-1", domain.CreateEnvironment{ProjectID: projectResult.Value.ID, Name: "Dev", Slug: "dev"})
	applicationResult, _ := store.CreateApplication(ctx, admin.ID, "ad-application-1", "ad-application-1", domain.CreateApplication{ProjectID: projectResult.Value.ID, Name: "API", Slug: "api"})
	deployment, _, err := store.CreateDeployment(ctx, admin.ID, "ad-deployment-01", "ad-deployment-01", "request", domain.CreateDeployment{
		EnvironmentID: environmentResult.Value.ID, ApplicationID: applicationResult.Value.ID,
		Image: "registry.test/api@sha256:" + strings.Repeat("b", 64), Replicas: 1, Port: 8080}, nil)
	if err != nil {
		t.Fatal(err)
	}
	accountResult, err := store.CreateServiceAccount(ctx, admin.ID, "ad-account-00001", "ad-account-00001", "request",
		domain.CreateServiceAccount{ProjectID: projectResult.Value.ID, Name: "Auto deploy", Role: domain.RoleDeveloper})
	if err != nil {
		t.Fatal(err)
	}
	template, digest := autoDeployMemoryTemplate(t, projectResult.Value, environmentResult.Value, applicationResult.Value, deployment.Value)
	now := time.Now().UTC()
	policy := autodeploy.Policy{ID: "11111111-1111-4111-8111-111111111111", BuildDefinitionID: "22222222-2222-4222-8222-222222222222",
		ProjectID: projectResult.Value.ID, ApplicationID: applicationResult.Value.ID, EnvironmentID: environmentResult.Value.ID,
		CurrentRevision: 1, CreatedBy: admin.ID, CreatedAt: now}
	revision := autodeploy.Revision{PolicyID: policy.ID, Revision: 1, Enabled: true, Template: template, TemplateDigest: digest,
		ServiceActorID: accountResult.Value.ID, CreatedBy: admin.ID, CreatedAt: now}
	audits := store.AuditCount()
	created, _, replay, err := store.CreatePolicy(ctx, policy, revision, "accepted-create-0001", "sha256:"+strings.Repeat("c", 64), "request")
	if err != nil || replay || created.ID != policy.ID || store.AuditCount() != audits+1 {
		t.Fatalf("create policy=%#v replay=%v audits=%d err=%v", created, replay, store.AuditCount(), err)
	}
	if _, err = store.DisableServiceAccount(ctx, admin.ID, accountResult.Value.ID, "disable-ad-account", "disable-ad-account", "request"); err != nil {
		t.Fatal(err)
	}
	_, _, replay, err = store.CreatePolicy(ctx, policy, revision, "accepted-create-0001", "sha256:"+strings.Repeat("c", 64), "replay")
	if err != nil || !replay {
		t.Fatalf("accepted create did not replay with disabled account: replay=%v err=%v", replay, err)
	}
	if _, _, _, err = store.CreatePolicy(ctx, policy, revision, "accepted-create-0001", "sha256:"+strings.Repeat("d", 64), "conflict"); !errors.Is(err, base.ErrIdempotencyConflict) {
		t.Fatalf("changed replay digest err=%v", err)
	}

	unauthorized, _ := invitedUser(t, store, admin, "No policy authority", "no-policy-authority")
	denied := policy
	denied.ID, denied.CreatedBy = "33333333-3333-4333-8333-333333333333", unauthorized.ID
	deniedRevision := revision
	deniedRevision.PolicyID, deniedRevision.CreatedBy, deniedRevision.CreatedAt = denied.ID, unauthorized.ID, now.Add(time.Second)
	policiesBefore, commandsBefore, auditBefore := len(store.autoDeployPolicies), len(store.autoDeployCommands), store.AuditCount()
	if _, _, _, err = store.CreatePolicy(ctx, denied, deniedRevision, "denied-create-0001", "sha256:"+strings.Repeat("e", 64), "denied"); err == nil {
		t.Fatal("actor without exact build/composite authority created policy")
	}
	if len(store.autoDeployPolicies) != policiesBefore || len(store.autoDeployCommands) != commandsBefore || store.AuditCount() != auditBefore {
		t.Fatal("denied policy mutation wrote policy, command, or audit state")
	}

	next := revision
	next.Revision, next.CreatedAt = 2, now.Add(2*time.Second)
	if _, _, _, err = store.RevisePolicy(ctx, policy, next, "disabled-revise-01", "sha256:"+strings.Repeat("f", 64), "disabled"); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("disabled selected service account err=%v", err)
	}
}
