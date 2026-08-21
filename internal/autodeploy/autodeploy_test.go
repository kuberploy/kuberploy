package autodeploy

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitops"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

const (
	policyID      = "11111111-1111-4111-8111-111111111111"
	definitionID  = "22222222-2222-4222-8222-222222222222"
	projectID     = "33333333-3333-4333-8333-333333333333"
	applicationID = "44444444-4444-4444-8444-444444444444"
	environmentID = "55555555-5555-4555-8555-555555555555"
	deploymentID  = "66666666-6666-4666-8666-666666666666"
	serviceActor  = "77777777-7777-4777-8777-777777777777"
	creatorID     = "88888888-8888-4888-8888-888888888888"
	attemptID     = "99999999-9999-4999-8999-999999999999"
	releaseID     = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	operationID   = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

var fixedNow = time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

func testRuntime() domain.WorkloadRuntime {
	return domain.NormalizeWorkloadRuntime(domain.WorkloadRuntime{Replicas: 2,
		Ports:        []domain.WorkloadPort{{Name: "http", ContainerPort: 8080}},
		NodeSelector: map[string]string{"kubernetes.io/arch": "amd64"}})
}

type policyCatalog struct {
	definition  BuildDefinitionIdentity
	application domain.Application
	environment domain.Environment
	deployment  domain.Deployment
	account     domain.ServiceAccount
}

func (c policyCatalog) BuildDefinitionIdentity(context.Context, string) (BuildDefinitionIdentity, error) {
	return c.definition, nil
}
func (c policyCatalog) GetApplication(context.Context, string) (domain.Application, error) {
	return c.application, nil
}
func (c policyCatalog) GetEnvironment(context.Context, string) (domain.Environment, error) {
	return c.environment, nil
}
func (c policyCatalog) GetDeployment(context.Context, string) (domain.Deployment, error) {
	return c.deployment, nil
}
func (c policyCatalog) GetServiceAccount(context.Context, string) (domain.ServiceAccount, error) {
	return c.account, nil
}

type policyProjection struct{ bundle gitprojection.Bundle }

func (p policyProjection) Bundle(context.Context, string, domain.Deployment, string, time.Duration) (gitprojection.Bundle, error) {
	return p.bundle, nil
}

func projectedPolicyBundle(raw []byte) gitprojection.Bundle {
	return gitprojection.Bundle{ETag: `"sha256:` + repeat("2", 64) + `"`, Documents: []gitprojection.Document{{ApplicationID: applicationID, Raw: raw, Valid: true}},
		Dependencies: []gitprojection.DependencyState{{Path: "variables/project.yaml"}, {Path: "variables/environment.yaml"}}}
}

type policyRecorder struct {
	policy   Policy
	revision Revision
}

func (s *policyRecorder) PolicyCommandReplay(_ context.Context, _, _, _, _ string) (Policy, Revision, bool, error) {
	return Policy{}, Revision{}, false, nil
}

func (s *policyRecorder) CreatePolicy(_ context.Context, policy Policy, revision Revision, _, _, _ string) (Policy, Revision, bool, error) {
	s.policy, s.revision = policy, revision
	return policy, revision, false, nil
}
func (s *policyRecorder) RevisePolicy(_ context.Context, _ Policy, revision Revision, _, _, _ string) (Policy, Revision, bool, error) {
	s.revision = revision
	return Policy{}, revision, false, nil
}

func TestPolicyCreationBindsExactResourcesAndStoresOnlyReusableInputs(t *testing.T) {
	deployment := domain.Deployment{ID: deploymentID, ApplicationID: applicationID, EnvironmentID: environmentID,
		Generation: 4, Image: "registry.example/app@sha256:" + repeat("1", 64), Runtime: testRuntime(),
		RegistryPull: &domain.RegistryPullReference{TargetID: policyID, ProfileName: "private", ProfileRevision: 1},
		Route:        &domain.Route{Hostname: "10-0-0-1.sslip.io", PathPrefix: "/", TLSMode: "httpOnly", DNSMode: "sslip"}}
	deployment.ConfigRaw = renderConfig(t, deployment)
	catalog := policyCatalog{
		definition:  BuildDefinitionIdentity{ID: definitionID, ProjectID: projectID, ApplicationID: applicationID},
		application: domain.Application{ID: applicationID, ProjectID: projectID},
		environment: domain.Environment{ID: environmentID, ProjectID: projectID},
		deployment:  deployment,
		account:     domain.ServiceAccount{ID: serviceActor, ProjectID: projectID},
	}
	store := &policyRecorder{}
	service := &PolicyService{Catalog: catalog, Projection: policyProjection{bundle: projectedPolicyBundle(deployment.ConfigRaw)}, Store: store, NewID: func() (string, error) { return policyID, nil }, Now: func() time.Time { return fixedNow }}
	policy, revision, replay, err := service.Create(t.Context(), creatorID, CreatePolicyInput{BuildDefinitionID: definitionID,
		ExpectedApplicationID: applicationID,
		EnvironmentID:         environmentID, TemplateDeploymentID: deploymentID, ServiceActorID: serviceActor,
		Enabled: true, IdempotencyKey: "create-1", RequestDigest: "sha256:" + repeat("c", 64), RequestID: "request-create-1"})
	if err != nil || replay || policy.ID != policyID {
		t.Fatalf("create policy=%#v revision=%#v replay=%v err=%v", policy, revision, replay, err)
	}
	if revision.TemplateDigest != TemplateDigest(revision.Template) {
		t.Fatalf("template digest mismatch: %#v", revision.Template)
	}
	var intent map[string]any
	if err = json.Unmarshal(revision.Template.ConfigIntent, &intent); err != nil {
		t.Fatal(err)
	}
	spec := intent["spec"].(map[string]any)
	delivery := spec["delivery"].(map[string]any)
	runtime := spec["runtime"].(map[string]any)
	if len(delivery) != 1 || delivery["mode"] != "image" || runtime["nodeSelector"] == nil || runtime["schedulingProfile"] != nil {
		t.Fatalf("direct scheduling intent was not retained canonically: %#v", intent)
	}
	// Release and RegistryPull are absent; the canonical image-only pipeline
	// resolves both against the current registry state on every run.
	if revision.Template.SourceDeploymentID != deploymentID || revision.Template.SourceDeploymentGeneration != 4 {
		t.Fatalf("template source mismatch: %#v", revision.Template)
	}
}

func TestPolicyCreationRejectsStaleProjectedDeploymentConfig(t *testing.T) {
	deployment := domain.Deployment{ID: deploymentID, ApplicationID: applicationID, EnvironmentID: environmentID,
		Generation: 5, Image: "registry.example/app@sha256:" + repeat("1", 64), Runtime: testRuntime()}
	deployment.ConfigRaw = renderConfig(t, deployment)
	stale := deployment
	stale.Runtime.Replicas = 1
	stale.Replicas = 1
	catalog := policyCatalog{
		definition:  BuildDefinitionIdentity{ID: definitionID, ProjectID: projectID, ApplicationID: applicationID},
		application: domain.Application{ID: applicationID, ProjectID: projectID},
		environment: domain.Environment{ID: environmentID, ProjectID: projectID},
		deployment:  deployment,
		account:     domain.ServiceAccount{ID: serviceActor, ProjectID: projectID},
	}
	store := &policyRecorder{}
	service := &PolicyService{Catalog: catalog, Projection: policyProjection{bundle: projectedPolicyBundle(renderConfig(t, stale))},
		Store: store, NewID: func() (string, error) { return policyID, nil }, Now: func() time.Time { return fixedNow }}
	_, _, _, err := service.Create(t.Context(), creatorID, CreatePolicyInput{BuildDefinitionID: definitionID,
		ExpectedApplicationID: applicationID, EnvironmentID: environmentID, TemplateDeploymentID: deploymentID,
		ServiceActorID: serviceActor, Enabled: true, IdempotencyKey: "create-stale", RequestDigest: "sha256:" + repeat("c", 64), RequestID: "request-create-stale"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stale projected AppConfig error=%v", err)
	}
	if store.policy.ID != "" || store.revision.PolicyID != "" {
		t.Fatalf("stale projected AppConfig persisted policy=%#v revision=%#v", store.policy, store.revision)
	}
}

func TestPolicyCreationRejectsCrossProjectAndDisabledServiceActor(t *testing.T) {
	base := policyCatalog{definition: BuildDefinitionIdentity{ID: definitionID, ProjectID: projectID, ApplicationID: applicationID},
		application: domain.Application{ID: applicationID, ProjectID: projectID}, environment: domain.Environment{ID: environmentID, ProjectID: projectID},
		deployment: domain.Deployment{ID: deploymentID, ApplicationID: applicationID, EnvironmentID: environmentID, Generation: 1,
			Image: "registry.example/app@sha256:" + repeat("1", 64), Runtime: testRuntime()},
		account: domain.ServiceAccount{ID: serviceActor, ProjectID: projectID}}
	base.deployment.ConfigRaw = renderConfig(t, base.deployment)
	service := &PolicyService{Catalog: base, Projection: policyProjection{bundle: projectedPolicyBundle(base.deployment.ConfigRaw)}, Store: &policyRecorder{}, NewID: func() (string, error) { return policyID, nil }, Now: func() time.Time { return fixedNow }}
	input := CreatePolicyInput{ExpectedApplicationID: applicationID, BuildDefinitionID: definitionID, EnvironmentID: environmentID, TemplateDeploymentID: deploymentID,
		ServiceActorID: serviceActor, Enabled: true, IdempotencyKey: "create-1", RequestDigest: "sha256:" + repeat("c", 64), RequestID: "request-create-1"}
	wrongPath := base
	wrongStore := &policyRecorder{}
	service.Catalog, service.Store = wrongPath, wrongStore
	wrongInput := input
	wrongInput.ExpectedApplicationID = creatorID
	if _, _, _, err := service.Create(t.Context(), creatorID, wrongInput); !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-application path was not rejected: %v", err)
	}
	if wrongStore.policy.ID != "" {
		t.Fatalf("cross-application request persisted policy: %#v", wrongStore.policy)
	}
	cross := base
	cross.environment.ProjectID = creatorID
	service.Catalog = cross
	if _, _, _, err := service.Create(t.Context(), creatorID, input); !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-project environment accepted: %v", err)
	}
	disabled := fixedNow
	base.account.DisabledAt = &disabled
	service.Catalog = base
	if _, _, _, err := service.Create(t.Context(), creatorID, input); !errors.Is(err, ErrConflict) {
		t.Fatalf("disabled service actor accepted: %v", err)
	}
}

type runStore struct {
	work             Work
	complete         int
	retries          int
	failures         int
	heartbeats       int
	heartbeatErrorAt int
}

func (s *runStore) ClaimNextRun(context.Context, string, time.Time, time.Duration) (Work, error) {
	return s.work, nil
}
func (s *runStore) HeartbeatRun(_ context.Context, lease Lease, now time.Time, duration time.Duration) (Lease, error) {
	s.heartbeats++
	if s.heartbeatErrorAt == s.heartbeats {
		return Lease{}, ErrLeaseLost
	}
	lease.Until = now.Add(duration)
	s.work.Lease = lease
	return lease, nil
}
func (s *runStore) RetryRun(context.Context, Lease, string, time.Time, time.Time) error {
	s.retries++
	return nil
}
func (s *runStore) FailRun(context.Context, Lease, string, time.Time) error { s.failures++; return nil }
func (s *runStore) CompleteRun(context.Context, Lease, SubmissionReceipt, time.Time) error {
	s.complete++
	return nil
}

type releaseVerifier struct{ release VerifiedRelease }

func (v releaseVerifier) ResolveVerifiedRelease(context.Context, string) (VerifiedRelease, error) {
	return v.release, nil
}

type authorizer struct {
	err   error
	calls int
}

func (a *authorizer) AuthorizeAutoDeploy(_ context.Context, actor string, scope domain.AutomationScope, project, environment, application string) error {
	a.calls++
	if actor != serviceActor || scope != domain.AutomationScopeAppEdit || project != projectID || environment != environmentID || application != applicationID {
		return ErrUnauthorized
	}
	return a.err
}

type pipeline struct {
	submissions []Submission
	receipt     SubmissionReceipt
}

func (p *pipeline) SubmitAutoDeployment(_ context.Context, submission Submission) (SubmissionReceipt, error) {
	p.submissions = append(p.submissions, submission)
	return p.receipt, nil
}

type cancellationPipeline struct {
	submissions []Submission
	canceled    bool
}

func (p *cancellationPipeline) SubmitAutoDeployment(ctx context.Context, submission Submission) (SubmissionReceipt, error) {
	p.submissions = append(p.submissions, submission)
	<-ctx.Done()
	p.canceled = true
	return SubmissionReceipt{}, ctx.Err()
}

func validWork() Work {
	deployment := domain.Deployment{ID: deploymentID, ApplicationID: applicationID, EnvironmentID: environmentID, Generation: 2,
		Image: "registry.example/app@sha256:" + repeat("1", 64), Runtime: testRuntime()}
	raw, _ := gitops.RenderAppConfig(domain.Project{ID: projectID}, domain.Environment{ID: environmentID}, domain.Application{ID: applicationID, Slug: "app"}, deployment)
	intent, digest, _ := appconfig.AutoDeployIntentTemplate(raw)
	template := Template{SourceDeploymentID: deploymentID, SourceDeploymentGeneration: 2,
		SourceConfigETag: `"sha256:` + repeat("2", 64) + `"`, ConfigIntent: intent}
	policy := Policy{ID: policyID, BuildDefinitionID: definitionID, ProjectID: projectID, ApplicationID: applicationID,
		EnvironmentID: environmentID, CurrentRevision: 1, CreatedBy: creatorID, CreatedAt: fixedNow}
	revision := Revision{PolicyID: policyID, Revision: 1, Enabled: true, Template: template, TemplateDigest: digest,
		ServiceActorID: serviceActor, CreatedBy: creatorID, CreatedAt: fixedNow}
	run := Run{AttemptID: attemptID, PolicyID: policyID, PolicyRevision: 1, DefinitionID: definitionID,
		DefinitionDigest: "sha256:" + repeat("d", 64), ReleaseID: releaseID, TemplateDigest: revision.TemplateDigest,
		SourceDeploymentID: template.SourceDeploymentID, SourceDeploymentGeneration: template.SourceDeploymentGeneration,
		SourceConfigETag: template.SourceConfigETag,
		IdempotencyKey:   IdempotencyKey(policyID, 1, attemptID), State: RunProcessing, Attempts: 1,
		AvailableAt: fixedNow, CreatedAt: fixedNow, UpdatedAt: fixedNow}
	lease := Lease{AttemptID: attemptID, PolicyID: policyID, Owner: "worker-auto-deploy-01234567", Epoch: 1, Until: fixedNow.Add(time.Minute)}
	return Work{Policy: policy, Revision: revision, Run: run, Lease: lease}
}

func validRelease() VerifiedRelease {
	return VerifiedRelease{AttemptID: attemptID, DefinitionID: definitionID, DefinitionDigest: "sha256:" + repeat("d", 64),
		ProjectID: projectID, ApplicationID: applicationID, ReleaseID: releaseID,
		Image: "registry.example/team/app@sha256:" + repeat("e", 64), CommitSHA: repeat("f", 40), CompletedAt: fixedNow}
}

func TestControllerUsesVerifiedImageStableIdempotencyAndCanonicalPipeline(t *testing.T) {
	store := &runStore{work: validWork()}
	authorization := &authorizer{}
	deployments := &pipeline{receipt: SubmissionReceipt{OperationID: operationID, DeploymentID: deploymentID}}
	controller := &Controller{Store: store, Releases: releaseVerifier{release: validRelease()}, Authorization: authorization,
		Deployments: deployments, Owner: store.work.Lease.Owner, LeaseDuration: time.Minute, Now: func() time.Time { return fixedNow.Add(time.Second) }}
	worked, err := controller.ReconcileNext(t.Context())
	if err != nil || !worked || store.complete != 1 || len(deployments.submissions) != 1 || authorization.calls != 1 {
		t.Fatalf("worked=%v complete=%d submissions=%d auth=%d err=%v", worked, store.complete, len(deployments.submissions), authorization.calls, err)
	}
	submission := deployments.submissions[0]
	if submission.Image != validRelease().Image || submission.ActorID != serviceActor || submission.IdempotencyKey != store.work.Run.IdempotencyKey ||
		submission.TemplateDigest != store.work.Revision.TemplateDigest || submission.ProjectID != projectID || submission.EnvironmentID != environmentID ||
		submission.SourceDeploymentID != deploymentID || submission.SourceDeploymentGeneration != 2 || submission.SourceConfigETag != store.work.Revision.Template.SourceConfigETag {
		t.Fatalf("unsafe or substituted submission: %#v", submission)
	}
	// Publication mode is not represented in Submission. Development direct
	// and protected PR selection are necessarily re-resolved by the canonical
	// pipeline at execution time.
}

func TestControllerFailsSubstitutionAndRetriesRevokedGrantWithoutSubmitting(t *testing.T) {
	work := validWork()
	work.Run.DefinitionDigest = "sha256:" + repeat("0", 64)
	store := &runStore{work: work}
	deployments := &pipeline{}
	controller := &Controller{Store: store, Releases: releaseVerifier{release: validRelease()}, Authorization: &authorizer{},
		Deployments: deployments, Owner: work.Lease.Owner, LeaseDuration: time.Minute, Now: func() time.Time { return fixedNow.Add(time.Second) }}
	if worked, err := controller.ReconcileNext(t.Context()); err != nil || !worked || store.failures != 1 || len(deployments.submissions) != 0 {
		t.Fatalf("substitution worked=%v failures=%d submissions=%d err=%v", worked, store.failures, len(deployments.submissions), err)
	}
	for name, substitute := range map[string]func(*Work){
		"source deployment": func(work *Work) { work.Run.SourceDeploymentID = creatorID },
		"source generation": func(work *Work) { work.Run.SourceDeploymentGeneration++ },
		"source etag":       func(work *Work) { work.Run.SourceConfigETag = `"sha256:` + repeat("9", 64) + `"` },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := validWork()
			substitute(&candidate)
			candidateStore := &runStore{work: candidate}
			candidatePipeline := &pipeline{}
			candidateController := &Controller{Store: candidateStore, Releases: releaseVerifier{release: validRelease()}, Authorization: &authorizer{},
				Deployments: candidatePipeline, Owner: candidate.Lease.Owner, LeaseDuration: time.Minute, Now: func() time.Time { return fixedNow.Add(time.Second) }}
			if _, err := candidateController.ReconcileNext(t.Context()); err != nil || candidateStore.failures != 1 || len(candidatePipeline.submissions) != 0 {
				t.Fatalf("provenance substitution was not rejected: failures=%d submissions=%d err=%v", candidateStore.failures, len(candidatePipeline.submissions), err)
			}
		})
	}

	work = validWork()
	store = &runStore{work: work}
	revoked := &authorizer{err: ErrUnauthorized}
	controller.Store, controller.Authorization = store, revoked
	if worked, err := controller.ReconcileNext(t.Context()); err != nil || !worked || store.retries != 1 || len(deployments.submissions) != 0 {
		t.Fatalf("revoked worked=%v retries=%d submissions=%d err=%v", worked, store.retries, len(deployments.submissions), err)
	}
}

func TestDeterministicSubmissionRecoversCrashByPipelineReplay(t *testing.T) {
	work := validWork()
	store := &runStore{work: work}
	deployments := &pipeline{receipt: SubmissionReceipt{OperationID: operationID, DeploymentID: deploymentID, Replay: true}}
	controller := &Controller{Store: store, Releases: releaseVerifier{release: validRelease()}, Authorization: &authorizer{}, Deployments: deployments,
		Owner: work.Lease.Owner, LeaseDuration: time.Minute, Now: func() time.Time { return fixedNow.Add(time.Second) }}
	if _, err := controller.ReconcileNext(t.Context()); err != nil || store.complete != 1 {
		t.Fatalf("replay was not completed: complete=%d err=%v", store.complete, err)
	}
	if deployments.submissions[0].IdempotencyKey != IdempotencyKey(policyID, 1, attemptID) {
		t.Fatalf("idempotency key changed: %s", deployments.submissions[0].IdempotencyKey)
	}
}

func TestControllerCancelsSubmissionAndStopsMutatingAfterLeaseLoss(t *testing.T) {
	work := validWork()
	store := &runStore{work: work, heartbeatErrorAt: 2}
	deployments := &cancellationPipeline{}
	controller := &Controller{Store: store, Releases: releaseVerifier{release: validRelease()}, Authorization: &authorizer{}, Deployments: deployments,
		Owner: work.Lease.Owner, LeaseDuration: 15 * time.Second, Now: func() time.Time { return fixedNow.Add(time.Second) },
		heartbeatEvery: func(time.Duration) time.Duration { return time.Millisecond }}
	worked, err := controller.ReconcileNext(t.Context())
	if !worked || !errors.Is(err, ErrLeaseLost) || len(deployments.submissions) != 1 || !deployments.canceled {
		t.Fatalf("worked=%v submissions=%d canceled=%v err=%v", worked, len(deployments.submissions), deployments.canceled, err)
	}
	if store.complete != 0 || store.retries != 0 || store.failures != 0 {
		t.Fatalf("stale owner mutated run: complete=%d retries=%d failures=%d", store.complete, store.retries, store.failures)
	}
}

func TestControllerConvergesWhenSubmissionWasAcceptedBeforeLeaseLoss(t *testing.T) {
	work := validWork()
	store := &runStore{work: work, heartbeatErrorAt: 2}
	deployments := &pipeline{receipt: SubmissionReceipt{OperationID: operationID, DeploymentID: deploymentID}}
	controller := &Controller{Store: store, Releases: releaseVerifier{release: validRelease()}, Authorization: &authorizer{}, Deployments: deployments,
		Owner: work.Lease.Owner, LeaseDuration: 15 * time.Second, Now: func() time.Time { return fixedNow.Add(time.Second) }}
	worked, err := controller.ReconcileNext(t.Context())
	if !worked || !errors.Is(err, ErrLeaseLost) || len(deployments.submissions) != 1 || store.complete != 0 || store.retries != 0 || store.failures != 0 {
		t.Fatalf("accepted-before-loss worked=%v submissions=%d complete=%d retries=%d failures=%d err=%v", worked, len(deployments.submissions), store.complete, store.retries, store.failures, err)
	}

	store.heartbeats = 0
	store.heartbeatErrorAt = 0
	deployments.receipt.Replay = true
	if worked, err = controller.ReconcileNext(t.Context()); err != nil || !worked || len(deployments.submissions) != 2 || store.complete != 1 {
		t.Fatalf("replay convergence worked=%v submissions=%d complete=%d err=%v", worked, len(deployments.submissions), store.complete, err)
	}
	if deployments.submissions[0].IdempotencyKey != deployments.submissions[1].IdempotencyKey {
		t.Fatalf("replay changed idempotency key: %q != %q", deployments.submissions[0].IdempotencyKey, deployments.submissions[1].IdempotencyKey)
	}
}

func repeat(value string, count int) string {
	result := ""
	for range count {
		result += value
	}
	return result
}

func renderConfig(t *testing.T, deployment domain.Deployment) []byte {
	t.Helper()
	raw, err := gitops.RenderAppConfig(domain.Project{ID: projectID}, domain.Environment{ID: environmentID},
		domain.Application{ID: applicationID, Slug: "app"}, deployment)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
