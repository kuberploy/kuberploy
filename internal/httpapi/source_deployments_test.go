package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/builds"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	"github.com/kuberploy/kuberploy/internal/store"
)

type sourceDeploymentAPIFake struct {
	command builds.SourceDeploymentCommand
}

func (f *sourceDeploymentAPIFake) AcceptSourceDeployment(_ context.Context, command builds.SourceDeploymentCommand) (builds.SourceDeploymentAcceptance, error) {
	f.command = command
	return builds.SourceDeploymentAcceptance{Attempt: builds.BuildAttempt{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		DefinitionID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", ProjectID: command.ProjectID, ServiceID: command.ApplicationID,
		CommitSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", GitRef: "refs/heads/main", Generation: 1,
		State: builds.AttemptQueued, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
		Intent: builds.SourceDeploymentIntent{ID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", Sequence: 1}}, nil
}

func TestSourceDeploymentEndpointAcceptsOneDurableCommand(t *testing.T) {
	f := newAPI(t)
	user := f.bootstrap()
	r := f.request(http.MethodPost, "/v1/projects", "source-api-project", map[string]string{"name": "Source API"})
	project := decode[struct {
		ID string `json:"id"`
	}](t, r)
	r = f.request(http.MethodPost, "/v1/environments", "source-api-environment", map[string]string{"projectId": project.ID, "name": "Production"})
	environment := decode[struct {
		ID string `json:"id"`
	}](t, r)
	r = f.request(http.MethodPost, "/v1/applications", "source-api-application", map[string]string{"projectId": project.ID, "name": "App"})
	application := decode[struct {
		ID string `json:"id"`
	}](t, r)
	r = f.request(http.MethodPost, "/v1/deployments", "source-api-deployment", map[string]any{"environmentId": environment.ID,
		"applicationId": application.ID, "image": "registry.example/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"runtime": domain.DefaultWorkloadRuntime(8080, nil)})
	operation := decode[struct {
		TargetID string `json:"targetId"`
	}](t, r)
	config, err := f.store.GetDeploymentConfigForActor(context.Background(), user.ID, operation.TargetID)
	if err != nil {
		t.Fatal(err)
	}
	fake := &sourceDeploymentAPIFake{}
	f.server.Close()
	_ = config
	f.server = httptest.NewServer(httpapi.New(httpapi.Options{Store: f.store, SourceDeployments: fake,
		HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000)}))
	r = f.request(http.MethodPost, "/v1/deployments/"+operation.TargetID+"/source-build", "source-api-command-0001", map[string]string{"mode": "deploy"})
	if r.StatusCode != http.StatusAccepted {
		problem := decode[httpapi.Problem](t, r)
		t.Fatalf("status=%d problem=%+v", r.StatusCode, problem)
	}
	if fake.command.DeploymentID != operation.TargetID || fake.command.ApplicationID != application.ID ||
		fake.command.EnvironmentID != environment.ID || fake.command.ProjectID != project.ID || len(fake.command.ConfigIntent) == 0 ||
		fake.command.SourceConfigETag != config.ETag || fake.command.SourceProjectionETag != config.ETag {
		t.Fatalf("status=%d command=%+v", r.StatusCode, fake.command)
	}

	binding := projectedHTTPBinding(t, project.ID, environment.ID, time.Now().UTC().Add(-time.Minute))
	document, err := gitprojection.NewDocument(binding, 1, application.ID, binding.IndexedRevision, binding.IndexedRevision,
		strings.Repeat("e", 40), config.RawYAML, nil, nil, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	gitETag := `"sha256:` + strings.Repeat("b", 64) + `"`
	backend := &projectionHTTPBackend{bundle: gitprojection.Bundle{ETag: gitETag, Documents: []gitprojection.Document{document},
		Dependencies: []gitprojection.DependencyState{{Path: "tenants/" + project.ID + "/variables.yaml"}, {Path: binding.Prefix + "/variables.yaml"}}}}
	f.server.Close()
	f.server = httptest.NewServer(httpapi.New(httpapi.Options{Store: f.store, SourceDeployments: fake, GitProjection: backend,
		GitProjectionReadiness: &projectionHTTPReadiness{}, HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000)}))
	r = f.request(http.MethodPost, "/v1/deployments/"+operation.TargetID+"/source-build", "source-api-command-git", map[string]string{"mode": "deploy"})
	if r.StatusCode != http.StatusAccepted || fake.command.SourceConfigETag != gitETag || fake.command.SourceProjectionETag != config.ETag {
		t.Fatalf("Git-backed acceptance status=%d Git ETag=%s projection ETag=%s", r.StatusCode, fake.command.SourceConfigETag, fake.command.SourceProjectionETag)
	}
}

func TestSourceDeploymentEndpointStartsNewSourceAppDraft(t *testing.T) {
	f := newAPI(t)
	user := f.bootstrap()
	r := f.request(http.MethodPost, "/v1/projects", "source-draft-project", map[string]string{"name": "Source Draft"})
	project := decode[struct {
		ID string `json:"id"`
	}](t, r)
	r = f.request(http.MethodPost, "/v1/environments", "source-draft-environment", map[string]string{"projectId": project.ID, "name": "Production"})
	environment := decode[struct {
		ID string `json:"id"`
	}](t, r)
	r = f.request(http.MethodPost, "/v1/applications", "source-draft-application", map[string]string{
		"projectId": project.ID, "environmentId": environment.ID, "name": "App", "sourceKind": "github",
	})
	application := decode[struct {
		ID string `json:"id"`
	}](t, r)
	deployments, err := f.store.ListDeployments(context.Background())
	if err != nil || len(deployments) != 1 || deployments[0].ApplicationID != application.ID || deployments[0].State != "stopped" {
		t.Fatalf("source draft=%+v err=%v", deployments, err)
	}
	if _, err = f.store.GetDeploymentConfigForActor(context.Background(), user.ID, deployments[0].ID); !errors.Is(err, store.ErrConfigProjectionMissing) {
		t.Fatalf("new source draft unexpectedly has runnable config: %v", err)
	}
	fake := &sourceDeploymentAPIFake{}
	f.server.Close()
	f.server = httptest.NewServer(httpapi.New(httpapi.Options{Store: f.store, SourceDeployments: fake,
		HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000)}))
	r = f.request(http.MethodPost, "/v1/deployments/"+deployments[0].ID+"/source-build", "source-draft-command-0001", map[string]string{"mode": "deploy"})
	if r.StatusCode != http.StatusAccepted {
		problem := decode[httpapi.Problem](t, r)
		t.Fatalf("status=%d problem=%+v", r.StatusCode, problem)
	}
	if !fake.command.StartDraft || fake.command.SourceConfigETag != "" || fake.command.SourceProjectionETag != "" || len(fake.command.ConfigIntent) != 0 ||
		fake.command.DeploymentID != deployments[0].ID || fake.command.EnvironmentID != environment.ID {
		t.Fatalf("draft command=%+v", fake.command)
	}
}
