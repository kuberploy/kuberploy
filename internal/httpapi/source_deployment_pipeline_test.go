package httpapi

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/builds"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

type sourceDraftProjection struct{ plan gitprojection.WritePlan }
type sourceDraftReady struct{}

func (sourceDraftReady) Probe(context.Context) error { return nil }

func (p sourceDraftProjection) PlanMutation(context.Context, string, string, string, string) (gitprojection.WritePlan, error) {
	return p.plan, nil
}

func (sourceDraftProjection) Bundle(context.Context, string, domain.Deployment, string, time.Duration) (gitprojection.Bundle, error) {
	return gitprojection.Bundle{}, gitprojection.ErrNotFound
}

func TestSourceDeploymentStartsNewAppDraftWithDefaultRuntime(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	now := time.Now().UTC()
	admin := domain.User{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Email: "admin@example.test", DisplayName: "Admin",
		Role: "platform-admin", Issuer: "test", Subject: "admin", GrantRevision: 1, CreatedAt: now}
	sessionHash := sha256.Sum256([]byte("source-draft-session"))
	if err := st.BootstrapAdmin(ctx, admin, strings.Repeat("h", 64), sessionHash[:], now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	project, err := st.CreateProject(ctx, admin.ID, "source-draft-project", "source-draft-project", domain.CreateProject{Name: "Source", Slug: "source"})
	if err != nil {
		t.Fatal(err)
	}
	environment, err := st.CreateEnvironment(ctx, admin.ID, "source-draft-environment", "source-draft-environment", domain.CreateEnvironment{
		ProjectID: project.Value.ID, Name: "Production", Slug: "production", ProtectionPolicy: domain.EnvironmentDevelopment,
	})
	if err != nil {
		t.Fatal(err)
	}
	application, err := st.CreateApplication(ctx, admin.ID, "source-draft-application", "source-draft-application", domain.CreateApplication{
		ProjectID: project.Value.ID, EnvironmentID: environment.Value.ID, Name: "API", Slug: "api", SourceKind: domain.ApplicationSourceGitHub,
	})
	if err != nil {
		t.Fatal(err)
	}
	deployments, err := st.ListDeployments(ctx)
	if err != nil || len(deployments) != 1 {
		t.Fatalf("draft deployments=%+v err=%v", deployments, err)
	}
	draft := deployments[0]
	binding, err := gitprojection.NewGitHubEnvironmentBinding("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", project.Value.ID, environment.Value.ID,
		gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 11, RepositoryID: 12, Owner: "kuberploy", Name: "desired-state"},
		"refs/heads/main", now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	binding.State, binding.TargetHeadRevision, binding.IndexedRevision = gitprojection.BindingReady, strings.Repeat("c", 40), strings.Repeat("c", 40)
	binding.ProjectionGeneration, binding.TargetHeadObservedAt, binding.IndexedAt, binding.UpdatedAt = 1, now, now, now
	if err = st.PutBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}
	plan := gitprojection.WritePlan{BindingID: binding.ID, ProjectID: project.Value.ID, EnvironmentID: environment.Value.ID,
		ApplicationID: application.Value.ID, BaseRevision: binding.IndexedRevision, Precondition: gitprojection.MutationCreateIfAbsent,
		ChartDigest: "sha256:" + strings.Repeat("d", 64), PolicyVersion: gitprojection.DefaultParser}
	server := &Server{store: st, gitProjection: sourceDraftProjection{plan: plan}, gitReadiness: sourceDraftReady{}}
	submission := builds.SourceDeploymentSubmission{IntentID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", ActorID: admin.ID,
		RequestID: "source-draft-request", AttemptID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", ProjectID: project.Value.ID,
		ApplicationID: application.Value.ID, EnvironmentID: environment.Value.ID, DeploymentID: draft.ID, Sequence: 1,
		SourceDeploymentGeneration: draft.Generation, Image: "registry.example/api@sha256:" + strings.Repeat("e", 64), StartDraft: true}
	receipt, err := server.SubmitSourceDeployment(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	started, err := st.GetDeployment(ctx, receipt.DeploymentID)
	if err != nil || started.ID != draft.ID || started.State != "pending-git" || started.Image != submission.Image || started.Port != 3000 {
		t.Fatalf("started=%+v receipt=%+v err=%v", started, receipt, err)
	}
	if len(started.ConfigRaw) == 0 {
		t.Fatal("started source draft has no AppConfig")
	}
}
