package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/builds"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	"github.com/kuberploy/kuberploy/internal/store"
	"github.com/kuberploy/kuberploy/internal/variablecompiler"
)

type sourceDeploymentAPIFake struct {
	command      builds.SourceDeploymentCommand
	receipt      *builds.SourceDeploymentAcceptance
	receiptCalls int
}

func (f *sourceDeploymentAPIFake) SourceDeploymentReceipt(context.Context, string, string, string) (builds.SourceDeploymentAcceptance, error) {
	f.receiptCalls++
	if f.receipt != nil {
		return *f.receipt, nil
	}
	return builds.SourceDeploymentAcceptance{}, builds.ErrNotFound
}

type sourceReplayDeniedStore struct{ store.Store }

func (s sourceReplayDeniedStore) Authorize(ctx context.Context, actor string, permission domain.Permission, target domain.AccessTarget) error {
	if permission == domain.PermissionBuildsManage {
		return store.ErrForbidden
	}
	return s.Store.Authorize(ctx, actor, permission, target)
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
	assertSourceDeploymentDependencySnapshot(t, fake.command.ConfigIntent, fake.command.TemplateDigest, config.RawYAML, backend.bundle)

	// A present parent must be bound by exact content identity without copying
	// its inherited values into the immutable application intent.
	parentPath := backend.bundle.Dependencies[0].Path
	parentRaw := []byte("apiVersion: variables.kuberploy.io/v1alpha1\nkind: VariableSet\nvalues:\n  INHERITED: ordinary-parent-value\n")
	parent, err := gitprojection.NewDependencyDocument(binding, 1, parentPath, binding.IndexedRevision, binding.IndexedRevision,
		strings.Repeat("f", 40), parentRaw, map[string]any{"apiVersion": "variables.kuberploy.io/v1alpha1", "kind": "VariableSet",
			"values": map[string]any{"INHERITED": "ordinary-parent-value"}}, nil, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	backend.bundle.Documents = append(backend.bundle.Documents, parent)
	backend.bundle.Dependencies[0].Present, backend.bundle.Dependencies[0].BlobID = true, parent.BlobID
	for _, mode := range []string{"deploy", "rebuild"} {
		input := map[string]string{"mode": mode}
		if mode == "rebuild" {
			input["sourceAttemptId"] = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
		}
		r = f.request(http.MethodPost, "/v1/deployments/"+operation.TargetID+"/source-build", "source-parent-"+mode, input)
		if r.StatusCode != http.StatusAccepted {
			t.Fatalf("parent snapshot %s status=%d problem=%+v", mode, r.StatusCode, decode[httpapi.Problem](t, r))
		}
		assertSourceDeploymentDependencySnapshot(t, fake.command.ConfigIntent, fake.command.TemplateDigest, config.RawYAML, backend.bundle)
		if strings.Contains(string(fake.command.ConfigIntent), "ordinary-parent-value") {
			t.Fatal("parent value was copied into the immutable application intent")
		}
	}
	previousFingerprint := fake.command.Fingerprint
	r = f.request(http.MethodPost, "/v1/deployments/"+operation.TargetID+"/source-build", "source-parent-other-attempt", map[string]string{
		"mode": "rebuild", "sourceAttemptId": "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee",
	})
	if r.StatusCode != http.StatusAccepted || fake.command.Fingerprint == previousFingerprint {
		t.Fatal("changing the selected rebuild attempt did not change request identity")
	}
	stored := builds.SourceDeploymentAcceptance{Intent: builds.SourceDeploymentIntent{ActorID: user.ID, ProjectID: project.ID,
		ApplicationID: application.ID, EnvironmentID: environment.ID, DeploymentID: operation.TargetID, Mode: builds.SourceDeploymentRebuild,
		SourceAttemptID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd"}}
	for _, field := range []string{"actor", "project", "application", "environment", "deployment", "mode", "attempt"} {
		receipt := stored
		otherID := "ffffffff-ffff-4fff-8fff-ffffffffffff"
		switch field {
		case "actor":
			receipt.Intent.ActorID = otherID
		case "project":
			receipt.Intent.ProjectID = otherID
		case "application":
			receipt.Intent.ApplicationID = otherID
		case "environment":
			receipt.Intent.EnvironmentID = otherID
		case "deployment":
			receipt.Intent.DeploymentID = otherID
		case "mode":
			receipt.Intent.Mode, receipt.Intent.SourceAttemptID = builds.SourceDeploymentDeploy, ""
		case "attempt":
			receipt.Intent.SourceAttemptID = otherID
		}
		fake.receipt = &receipt
		r = f.request(http.MethodPost, "/v1/deployments/"+operation.TargetID+"/source-build", "source-receipt-scope", map[string]string{
			"mode": "rebuild", "sourceAttemptId": stored.Intent.SourceAttemptID,
		})
		if r.StatusCode != http.StatusConflict {
			t.Fatalf("mismatched stored %s escaped receipt scope: status=%d", field, r.StatusCode)
		}
	}
	fake.receipt, fake.receiptCalls = &stored, 0
	r = f.request(http.MethodPost, "/v1/deployments/"+operation.TargetID+"/source-build", "source-receipt-scope", map[string]string{
		"mode": "rebuild", "sourceAttemptId": stored.Intent.SourceAttemptID, "unknown": "field",
	})
	if r.StatusCode != http.StatusBadRequest || fake.receiptCalls != 0 {
		t.Fatal("unknown request fields reached receipt lookup")
	}
	f.server.Close()
	f.server = httptest.NewServer(httpapi.New(httpapi.Options{Store: sourceReplayDeniedStore{f.store}, SourceDeployments: fake,
		HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000)}))
	r = f.request(http.MethodPost, "/v1/deployments/"+operation.TargetID+"/source-build", "source-receipt-scope", map[string]string{
		"mode": "rebuild", "sourceAttemptId": stored.Intent.SourceAttemptID,
	})
	if r.StatusCode != http.StatusForbidden || fake.receiptCalls != 0 {
		t.Fatal("revoked BuildsManage permission did not reject before receipt lookup")
	}
}

func assertSourceDeploymentDependencySnapshot(t *testing.T, intent []byte, digest string, current []byte, bundle gitprojection.Bundle) {
	t.Helper()
	dependencies, _, err := variablecompiler.CanonicalDependencyIntent(bundle.Dependencies, bundle.Documents)
	if err != nil {
		t.Fatal(err)
	}
	bound := make([]appconfig.AutoDeployDependencyIntent, len(dependencies))
	for index, dependency := range dependencies {
		bound[index] = appconfig.AutoDeployDependencyIntent{Path: dependency.Path, Present: dependency.Present,
			BlobID: dependency.BlobID, ContentSHA256: dependency.ContentSHA256}
	}
	image := "registry.example/app@sha256:" + strings.Repeat("9", 64)
	if candidate := appconfig.ApplyAutoDeployImageWithDependencies(current, intent, digest, image, bound); len(candidate.Diagnostics) != 0 {
		t.Fatalf("accepted source intent cannot publish with unchanged parent dependencies: %+v", candidate.Diagnostics)
	}
	// Parent changes after acceptance must still fail the publication fence.
	bound[0].Present, bound[0].BlobID, bound[0].ContentSHA256 = true, strings.Repeat("c", 40), "sha256:"+strings.Repeat("c", 64)
	if candidate := appconfig.ApplyAutoDeployImageWithDependencies(current, intent, digest, image, bound); len(candidate.Diagnostics) != 1 || candidate.Diagnostics[0].Code != "AutoDeployTemplateConflict" {
		t.Fatalf("changed parent bypassed immutable intent: %+v", candidate.Diagnostics)
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
