package builds

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/buildpromotion"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitops"
)

const (
	sourceActorID      = "77777777-7777-4777-8777-777777777777"
	sourceEnvironment  = "88888888-8888-4888-8888-888888888888"
	sourceDeploymentID = "99999999-9999-4999-8999-999999999999"
)

func sourceCommand(t *testing.T, definition BuildDefinition, mode SourceDeploymentMode, sourceAttempt string, now time.Time) SourceDeploymentCommand {
	t.Helper()
	raw, err := gitops.RenderAppConfig(domain.Project{ID: testProjectID}, domain.Environment{ID: sourceEnvironment},
		domain.Application{ID: testServiceID, Slug: "app"}, domain.Deployment{ID: sourceDeploymentID,
			ApplicationID: testServiceID, EnvironmentID: sourceEnvironment, Image: "registry.test/app@sha256:" + strings.Repeat("1", 64),
			Generation: 1, Runtime: domain.NormalizeWorkloadRuntime(domain.WorkloadRuntime{Replicas: 1,
				Ports: []domain.WorkloadPort{{Name: "http", ContainerPort: 8080}}})})
	if err != nil {
		t.Fatal(err)
	}
	intent, digest, diagnostics := appconfig.AutoDeployIntentTemplate(raw)
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	return SourceDeploymentCommand{ActorID: sourceActorID, ProjectID: testProjectID, ApplicationID: testServiceID,
		EnvironmentID: sourceEnvironment, DeploymentID: sourceDeploymentID, Mode: mode, DefinitionID: definition.ID,
		ExpectedDefinitionDigest: definition.DefinitionDigest, SourceAttemptID: sourceAttempt, CommitSHA: strings.Repeat("a", 40),
		Execution: definition.Spec.Execution, SourceDeploymentGeneration: 1, SourceConfigETag: `"cfg-sha256-` + strings.Repeat("2", 64) + `"`,
		ConfigIntent: intent, TemplateDigest: digest, IdempotencyKey: "source-deploy-key-0001", Fingerprint: "sha256:" + strings.Repeat("3", 64),
		RequestID: "request-source-1", AcceptedAt: now}
}

func TestMemorySourceDeploymentAtomicReplayAndNewestWins(t *testing.T) {
	store, definition := seedMemory(t, RegistryManaged)
	command := sourceCommand(t, definition, SourceDeploymentDeploy, "", testNow)
	first, err := store.AcceptSourceDeployment(context.Background(), command)
	if err != nil || first.Replay || first.Intent.Sequence != 1 {
		t.Fatalf("first acceptance: %+v %v", first, err)
	}
	replay, err := store.AcceptSourceDeployment(context.Background(), command)
	if err != nil || !replay.Replay || replay.Attempt.ID != first.Attempt.ID || replay.Intent.ID != first.Intent.ID {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	conflict := command
	conflict.Fingerprint = "sha256:" + strings.Repeat("4", 64)
	if _, err = store.AcceptSourceDeployment(context.Background(), conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected fingerprint conflict, got %v", err)
	}
	second := command
	second.IdempotencyKey, second.Fingerprint, second.AcceptedAt = "source-deploy-key-0002", "sha256:"+strings.Repeat("5", 64), testNow.Add(time.Second)
	accepted, err := store.AcceptSourceDeployment(context.Background(), second)
	if err != nil || accepted.Intent.Sequence != 2 || store.sourceDeploymentIntents[first.Intent.ID].State != SourceDeploymentSuperseded {
		t.Fatalf("newest intent did not supersede old: %+v %v", accepted, err)
	}
}

func TestMemorySourceDeploymentBuildFailureLeavesIntentUnpublished(t *testing.T) {
	store, definition := seedMemory(t, RegistryManaged)
	accepted, err := store.AcceptSourceDeployment(context.Background(), sourceCommand(t, definition, SourceDeploymentDeploy, "", testNow))
	if err != nil {
		t.Fatal(err)
	}
	attempt := store.attempts[accepted.Attempt.ID]
	completed := testNow.Add(time.Minute)
	attempt.State, attempt.CompletedAt, attempt.UpdatedAt = AttemptFailed, &completed, completed
	store.attempts[attempt.ID] = attempt
	if _, err = store.ClaimNextSourceDeployment(context.Background(), "source-worker", completed, time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed build became publishable: %v", err)
	}
	if intent := store.sourceDeploymentIntents[accepted.Intent.ID]; intent.State != SourceDeploymentFailed || intent.OperationID != "" {
		t.Fatalf("unexpected failed intent: %+v", intent)
	}
}

type sourceReleaseResolver struct{ source buildpromotion.Source }

func (r sourceReleaseResolver) Resolve(context.Context, buildpromotion.Request) (buildpromotion.Source, error) {
	return r.source, nil
}

type sourceAuthorizer struct{ calls int }

func (a *sourceAuthorizer) Authorize(context.Context, string, domain.Permission, domain.AccessTarget) error {
	a.calls++
	return nil
}

type sourcePipeline struct{ calls int }

func (p *sourcePipeline) SubmitSourceDeployment(_ context.Context, s SourceDeploymentSubmission) (SourceDeploymentSubmissionReceipt, error) {
	p.calls++
	return SourceDeploymentSubmissionReceipt{OperationID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", DeploymentID: s.DeploymentID}, nil
}

func TestSourceDeploymentControllerPublishesExactProjectedBuildAfterRestart(t *testing.T) {
	store, definition := seedMemory(t, RegistryManaged)
	accepted, err := store.AcceptSourceDeployment(context.Background(), sourceCommand(t, definition, SourceDeploymentDeploy, "", testNow))
	if err != nil {
		t.Fatal(err)
	}
	attempt := store.attempts[accepted.Attempt.ID]
	completed := testNow.Add(time.Minute)
	attempt.State, attempt.CompletedAt, attempt.UpdatedAt = AttemptSucceeded, &completed, completed
	store.attempts[attempt.ID] = attempt
	store.releaseProjections[attempt.ID] = memoryReleaseProjection{state: ReleaseProjectionSucceeded, releaseID: attempt.ID}
	authorizer, pipeline := &sourceAuthorizer{}, &sourcePipeline{}
	controller := &SourceDeploymentController{Store: store, Releases: sourceReleaseResolver{source: buildpromotion.Source{
		ProjectedBuild: buildpromotion.ProjectedBuild{AttemptID: attempt.ID, DefinitionID: attempt.DefinitionID,
			DefinitionDigest: attempt.DefinitionDigest, ProjectID: attempt.ProjectID, ApplicationID: attempt.ServiceID,
			ImageReference: "registry.test/app@sha256:" + strings.Repeat("e", 64)}, EnvironmentID: sourceEnvironment}}, Authorization: authorizer,
		Deployments: pipeline, Owner: "source-worker-restarted", LeaseDuration: time.Minute, Now: func() time.Time { return completed }}
	processed, err := controller.ReconcileNext(context.Background())
	if err != nil || !processed || authorizer.calls != 1 || pipeline.calls != 1 {
		t.Fatalf("reconcile: processed=%v auth=%d pipeline=%d err=%v", processed, authorizer.calls, pipeline.calls, err)
	}
	if intent := store.sourceDeploymentIntents[accepted.Intent.ID]; intent.State != SourceDeploymentSubmitted {
		t.Fatalf("intent not submitted: %+v", intent)
	}
}
