package memory

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/gitpublication"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func readyEnvironmentBinding(t *testing.T, projectID, environmentID string, now time.Time) gitprojection.Binding {
	t.Helper()
	binding, err := gitprojection.NewGitHubEnvironmentBinding("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", projectID, environmentID,
		gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 11, RepositoryID: 12, Owner: "kuberploy", Name: "desired-state"},
		"refs/heads/main", now)
	if err != nil {
		t.Fatal(err)
	}
	binding.State = gitprojection.BindingReady
	binding.TargetHeadRevision = strings.Repeat("c", 40)
	binding.IndexedRevision = binding.TargetHeadRevision
	binding.ProjectionGeneration = 1
	binding.TargetHeadObservedAt = now.Add(time.Second)
	binding.IndexedAt = now.Add(time.Second)
	binding.UpdatedAt = now.Add(time.Second)
	if err = binding.Validate(); err != nil {
		t.Fatal(err)
	}
	return binding
}

func TestProjectionAcceptancePersistsExactCreateUpdateAndConfigCommands(t *testing.T) {
	ctx := context.Background()
	st := New()
	admin := bootstrapAccessAdmin(t, st)
	project, err := st.CreateProject(ctx, admin.ID, "git-project", "git-project", domain.CreateProject{Name: "Git", Slug: "git"})
	if err != nil {
		t.Fatal(err)
	}
	environment, err := st.CreateEnvironment(ctx, admin.ID, "git-environment", "git-environment", domain.CreateEnvironment{ProjectID: project.Value.ID, Name: "Production", Slug: "production"})
	if err != nil {
		t.Fatal(err)
	}
	if environment.Value.ProtectionPolicy != domain.EnvironmentProtected {
		t.Fatalf("omitted environment policy was not fail-closed: %q", environment.Value.ProtectionPolicy)
	}
	application, err := st.CreateApplication(ctx, admin.ID, "git-application", "git-application", domain.CreateApplication{ProjectID: project.Value.ID, Name: "API", Slug: "api"})
	if err != nil {
		t.Fatal(err)
	}
	binding := readyEnvironmentBinding(t, project.Value.ID, environment.Value.ID, time.Now().UTC().Add(-time.Minute))
	if err = st.PutBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}
	chartDigest := "sha256:" + strings.Repeat("d", 64)
	createPlan := &gitprojection.WritePlan{BindingID: binding.ID, ProjectID: project.Value.ID, EnvironmentID: environment.Value.ID,
		ApplicationID: application.Value.ID, BaseRevision: binding.IndexedRevision, Precondition: gitprojection.MutationCreateIfAbsent,
		ChartDigest: chartDigest, PolicyVersion: "appconfig-v1alpha1"}
	create := domain.CreateDeployment{EnvironmentID: environment.Value.ID, ApplicationID: application.Value.ID,
		Image: "registry.example/api@sha256:" + strings.Repeat("a", 64), Runtime: domain.DefaultWorkloadRuntime(8080, nil)}
	created, createOperation, err := st.CreateDeployment(ctx, admin.ID, "projected-create", "projected-create", "request", create, createPlan)
	if err != nil {
		t.Fatal(err)
	}
	command, err := st.AcceptedGitWriteCommand(createOperation.ID)
	if err != nil || command.Plan.Precondition != gitprojection.MutationCreateIfAbsent || command.Plan.ExpectedETag != "" ||
		command.DeploymentID != created.Value.ID || command.ActorID != admin.ID || string(command.Content) != string(created.Value.ConfigRaw) {
		t.Fatalf("create command=%#v err=%v", command, err)
	}
	mode, err := st.AcceptedGitPublicationMode(createOperation.ID)
	publication, publicationErr := st.Publication(ctx, createOperation.ID)
	if err != nil || publicationErr != nil || mode != gitpublication.ModePullRequest ||
		publication.OperationID != createOperation.ID || publication.BindingID != binding.ID ||
		publication.Repository.InstallationID != binding.Repository.InstallationID ||
		publication.Repository.ID != binding.Repository.RepositoryID || publication.State != gitpublication.StatePendingCandidate {
		t.Fatalf("protected publication mode=%q publication=%#v modeErr=%v publicationErr=%v", mode, publication, err, publicationErr)
	}
	writeBase, err := publication.WithWriteBase(binding.TargetHeadRevision, publication.UpdatedAt.Add(time.Second))
	if err != nil || st.CompareAndSwapPublication(ctx, publication, writeBase) != nil {
		t.Fatalf("write base=%#v err=%v", writeBase, err)
	}
	publicationCandidate, err := writeBase.WithCandidate(strings.Repeat("8", 40), writeBase.UpdatedAt.Add(time.Second))
	if err != nil || st.CompareAndSwapPublication(ctx, writeBase, publicationCandidate) != nil {
		t.Fatalf("candidate=%#v err=%v", publicationCandidate, err)
	}
	prObservation := gitpublication.PullRequestObservation{Repository: publicationCandidate.Repository, Number: 17,
		URL: "https://github.com/kuberploy/desired-state/pull/17", TargetRef: publicationCandidate.TargetRef,
		HeadRef: publicationCandidate.CandidateRef, HeadRevision: publicationCandidate.CandidateRevision, State: gitpublication.PullRequestOpen,
		ObservedAt: publicationCandidate.UpdatedAt.Add(time.Second)}
	opened, err := publicationCandidate.WithPullRequest(prObservation, prObservation.ObservedAt)
	if err != nil || st.CompareAndSwapPublication(ctx, publicationCandidate, opened) != nil {
		t.Fatalf("opened=%#v err=%v", opened, err)
	}
	if _, err = st.LeasePendingOperations(ctx, "protected-completion-worker", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, execute, startErr := st.StartOperation(ctx, createOperation.ID, createOperation.Generation, "protected-completion-worker", time.Minute); startErr != nil || !execute {
		t.Fatalf("start execute=%v err=%v", execute, startErr)
	}
	completion := domain.GitPublicationResult{Mode: "pull-request", CandidateRevision: opened.CandidateRevision,
		PullRequestNumber: opened.PullRequestNumber, PullRequestURL: opened.PullRequestURL, PullRequestState: string(opened.PullRequestState)}
	forgedCompletion := completion
	forgedCompletion.PullRequestNumber++
	if err = st.CompleteGitOperation(ctx, createOperation.ID, createOperation.Generation, "protected-completion-worker", forgedCompletion); !errors.Is(err, base.ErrConflict) {
		t.Fatalf("substituted pull request completion accepted: %v", err)
	}
	if err = st.CompleteGitOperation(ctx, createOperation.ID, createOperation.Generation, "protected-completion-worker", completion); err != nil {
		t.Fatal(err)
	}
	protectedDeployment, err := st.GetDeployment(ctx, created.Value.ID)
	protectedOperation, operationErr := st.GetOperationForActor(ctx, admin.ID, createOperation.ID)
	if err != nil || operationErr != nil || protectedDeployment.State != "review-pending" || protectedDeployment.DesiredRevision != "" ||
		protectedOperation.GitRevision != "" || protectedOperation.PullRequest == nil || protectedOperation.PullRequest.Number != opened.PullRequestNumber {
		t.Fatalf("deployment=%#v operation=%#v deploymentErr=%v operationErr=%v", protectedDeployment, protectedOperation, err, operationErr)
	}

	document, err := gitprojection.NewDocument(binding, binding.ProjectionGeneration, application.Value.ID, binding.IndexedRevision,
		binding.IndexedRevision, strings.Repeat("e", 40), created.Value.ConfigRaw, nil, nil, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err = st.PutProjectionDocument(ctx, document); err != nil {
		t.Fatal(err)
	}
	etag, err := gitprojection.StrongETag(binding, []gitprojection.Document{document}, nil, chartDigest, "appconfig-v1alpha1")
	if err != nil {
		t.Fatal(err)
	}
	updatePlan := &gitprojection.WritePlan{BindingID: binding.ID, ProjectID: project.Value.ID, EnvironmentID: environment.Value.ID,
		ApplicationID: application.Value.ID, BaseRevision: binding.IndexedRevision, Precondition: gitprojection.MutationMatchETag,
		ExpectedETag: etag, ChartDigest: chartDigest, PolicyVersion: "appconfig-v1alpha1"}
	update := create
	update.Image = "registry.example/api@sha256:" + strings.Repeat("f", 64)
	updated, updateOperation, err := st.CreateDeployment(ctx, admin.ID, "projected-update", "projected-update", "request", update, updatePlan)
	if err != nil {
		t.Fatal(err)
	}
	command, err = st.AcceptedGitWriteCommand(updateOperation.ID)
	if err != nil || command.Plan.ExpectedETag != etag || command.Plan.Precondition != gitprojection.MutationMatchETag ||
		command.ContentSHA256 == "" || string(command.Content) != string(updated.Value.ConfigRaw) {
		t.Fatalf("update command=%#v err=%v", command, err)
	}

	forged := *updatePlan
	forged.ExpectedETag = `"sha256:` + strings.Repeat("0", 64) + `"`
	commandsBefore := len(st.gitWriteCommands)
	if _, _, err = st.CreateDeployment(ctx, admin.ID, "forged-etag", "forged-etag", "request", update, &forged); !errors.Is(err, base.ErrPreconditionFailed) {
		t.Fatalf("forged strong ETag accepted: %v", err)
	}
	if len(st.gitWriteCommands) != commandsBefore {
		t.Fatal("failed acceptance left a durable command")
	}

	candidate := appconfig.Apply(document.Raw, appconfig.Change{Mode: "jsonPatch", Patch: []appconfig.PatchOperation{{Op: "replace", Path: "/spec/runtime/replicas", Value: 2}}})
	if len(candidate.Diagnostics) != 0 {
		t.Fatalf("candidate diagnostics=%#v", candidate.Diagnostics)
	}
	tokenHash := sha256.Sum256([]byte("projection-preview"))
	if err = st.CreateDeploymentConfigPreview(ctx, admin.ID, domain.CreateConfigPreview{DeploymentID: updated.Value.ID, BaseETag: etag,
		TokenHash: tokenHash[:], CandidateHash: candidate.Hash, ExpiresAt: time.Now().Add(time.Hour)}, updatePlan); err != nil {
		t.Fatal(err)
	}
	saved, saveOperation, err := st.SaveDeploymentConfig(ctx, admin.ID, "projection-save", "projection-save", "request",
		domain.SaveDeploymentConfig{DeploymentID: updated.Value.ID, BaseETag: etag, TokenHash: tokenHash[:], CandidateHash: candidate.Hash,
			RawYAML: candidate.Raw, Runtime: candidate.Runtime}, updatePlan)
	if err != nil {
		t.Fatal(err)
	}
	command, err = st.AcceptedGitWriteCommand(saveOperation.ID)
	if err != nil || command.DeploymentID != saved.Value.ID || string(command.Content) != string(candidate.Raw) || command.Plan != *updatePlan {
		t.Fatalf("config-save command=%#v err=%v", command, err)
	}
}

func TestDevelopmentProjectionAcceptanceIsDirectAndCreatesNoPullRequestReceipt(t *testing.T) {
	ctx := t.Context()
	st := New()
	admin := bootstrapAccessAdmin(t, st)
	project, err := st.CreateProject(ctx, admin.ID, "direct-project", "direct-project", domain.CreateProject{Name: "Direct", Slug: "direct"})
	if err != nil {
		t.Fatal(err)
	}
	environment, err := st.CreateEnvironment(ctx, admin.ID, "direct-environment", "direct-environment", domain.CreateEnvironment{
		ProjectID: project.Value.ID, Name: "Development", Slug: "development", ProtectionPolicy: domain.EnvironmentDevelopment,
	})
	if err != nil {
		t.Fatal(err)
	}
	application, err := st.CreateApplication(ctx, admin.ID, "direct-application", "direct-application", domain.CreateApplication{ProjectID: project.Value.ID, Name: "API", Slug: "api"})
	if err != nil {
		t.Fatal(err)
	}
	binding := readyEnvironmentBinding(t, project.Value.ID, environment.Value.ID, time.Now().UTC().Add(-time.Minute))
	if err = st.PutBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}
	plan := &gitprojection.WritePlan{BindingID: binding.ID, ProjectID: project.Value.ID, EnvironmentID: environment.Value.ID,
		ApplicationID: application.Value.ID, BaseRevision: binding.IndexedRevision, Precondition: gitprojection.MutationCreateIfAbsent,
		ChartDigest: "sha256:" + strings.Repeat("d", 64), PolicyVersion: "appconfig-v1alpha1"}
	_, operation, err := st.CreateDeployment(ctx, admin.ID, "direct-deployment", "direct-deployment", "request",
		domain.CreateDeployment{EnvironmentID: environment.Value.ID, ApplicationID: application.Value.ID,
			Image: "registry.example/api@sha256:" + strings.Repeat("a", 64), Runtime: domain.DefaultWorkloadRuntime(8080, nil)}, plan)
	if err != nil {
		t.Fatal(err)
	}
	mode, err := st.AcceptedGitPublicationMode(operation.ID)
	if err != nil || mode != gitpublication.ModeDirect {
		t.Fatalf("mode=%q err=%v", mode, err)
	}
	if _, err = st.Publication(ctx, operation.ID); !errors.Is(err, gitpublication.ErrNotFound) {
		t.Fatalf("direct command created PR receipt: %v", err)
	}
}

func TestProjectionPlanIdentityCannotCrossBindingOrApplication(t *testing.T) {
	ctx := context.Background()
	st := New()
	admin := bootstrapAccessAdmin(t, st)
	project, _ := st.CreateProject(ctx, admin.ID, "identity-project", "identity-project", domain.CreateProject{Name: "Identity", Slug: "identity"})
	environment, _ := st.CreateEnvironment(ctx, admin.ID, "identity-environment", "identity-environment", domain.CreateEnvironment{ProjectID: project.Value.ID, Name: "Dev", Slug: "dev"})
	application, _ := st.CreateApplication(ctx, admin.ID, "identity-application", "identity-application", domain.CreateApplication{ProjectID: project.Value.ID, Name: "API", Slug: "api"})
	binding := readyEnvironmentBinding(t, project.Value.ID, environment.Value.ID, time.Now().UTC().Add(-time.Minute))
	if err := st.PutBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}
	plan := &gitprojection.WritePlan{BindingID: binding.ID, ProjectID: project.Value.ID, EnvironmentID: environment.Value.ID,
		ApplicationID: "99999999-9999-4999-8999-999999999999", BaseRevision: binding.IndexedRevision,
		Precondition: gitprojection.MutationCreateIfAbsent, ChartDigest: "sha256:" + strings.Repeat("d", 64), PolicyVersion: "policy-v1"}
	_, _, err := st.CreateDeployment(ctx, admin.ID, "cross-app", "cross-app", "request", domain.CreateDeployment{EnvironmentID: environment.Value.ID,
		ApplicationID: application.Value.ID, Image: "registry.example/api@sha256:" + strings.Repeat("a", 64), Runtime: domain.DefaultWorkloadRuntime(8080, nil)}, plan)
	if !errors.Is(err, base.ErrPreconditionFailed) {
		t.Fatalf("cross-application plan error=%v", err)
	}
}
