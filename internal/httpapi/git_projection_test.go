package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/gitpublication"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

type projectionHTTPBackend struct {
	plan          gitprojection.WritePlan
	planErr       error
	bundle        gitprojection.Bundle
	bundleErr     error
	atLeast       string
	wait          time.Duration
	planCallCount int
	bundleCalls   int
	variableSets  []gitprojection.VariableSetSnapshot
	variablePlan  gitprojection.WritePlan
	variableErr   error
}

func (b *projectionHTTPBackend) PlanMutation(context.Context, string, string, string, string) (gitprojection.WritePlan, error) {
	b.planCallCount++
	return b.plan, b.planErr
}

func (b *projectionHTTPBackend) Bundle(_ context.Context, _ string, _ domain.Deployment, atLeast string, wait time.Duration) (gitprojection.Bundle, error) {
	b.bundleCalls++
	b.atLeast, b.wait = atLeast, wait
	return b.bundle, b.bundleErr
}

func (b *projectionHTTPBackend) VariableSets(context.Context, string, string) ([]gitprojection.VariableSetSnapshot, error) {
	return b.variableSets, b.variableErr
}

func (b *projectionHTTPBackend) PlanVariableMutation(context.Context, string, string, string, string) (gitprojection.WritePlan, error) {
	return b.variablePlan, b.variableErr
}

type projectionHTTPReadiness struct{ err error }

func (p *projectionHTTPReadiness) Probe(context.Context) error { return p.err }

func newProjectionAPI(t *testing.T, backend *projectionHTTPBackend, readiness *projectionHTTPReadiness, argoReadiness ...*projectionHTTPReadiness) *apiFixture {
	t.Helper()
	argo := &projectionHTTPReadiness{}
	if len(argoReadiness) == 1 && argoReadiness[0] != nil {
		argo = argoReadiness[0]
	}
	st := memory.New()
	srv := httptest.NewServer(httpapi.New(httpapi.Options{Store: st, BootstrapToken: "one-time-secret", Version: "test",
		GitProjection: backend, GitProjectionReadiness: readiness, ArgoReadiness: argo, AppConfigRenderedPreviews: staticAppConfigRenderer{}, HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000)}))
	jar, _ := cookiejar.New(nil)
	f := &apiFixture{t: t, server: srv, client: &http.Client{Jar: jar}, store: st}
	t.Cleanup(srv.Close)
	return f
}

func projectedHTTPBinding(t *testing.T, projectID, environmentID string, now time.Time) gitprojection.Binding {
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

func TestProjectionHTTPCreateReplayBundleAndCapabilityAreExact(t *testing.T) {
	backend := &projectionHTTPBackend{}
	readiness := &projectionHTTPReadiness{}
	argoReadiness := &projectionHTTPReadiness{}
	f := newProjectionAPI(t, backend, readiness, argoReadiness)
	admin := f.bootstrap()
	project, err := f.store.CreateProject(t.Context(), admin.ID, "projected-project", "projected-project", domain.CreateProject{Name: "Projected", Slug: "projected"})
	if err != nil {
		t.Fatal(err)
	}
	environment, err := f.store.CreateEnvironment(t.Context(), admin.ID, "projected-environment", "projected-environment", domain.CreateEnvironment{ProjectID: project.Value.ID, Name: "Production", Slug: "production"})
	if err != nil {
		t.Fatal(err)
	}
	application, err := f.store.CreateApplication(t.Context(), admin.ID, "projected-application", "projected-application", domain.CreateApplication{ProjectID: project.Value.ID, Name: "API", Slug: "api"})
	if err != nil {
		t.Fatal(err)
	}
	binding := projectedHTTPBinding(t, project.Value.ID, environment.Value.ID, time.Now().UTC().Add(-time.Minute))
	if err = f.store.PutBinding(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	chartDigest := "sha256:" + strings.Repeat("d", 64)
	backend.plan = gitprojection.WritePlan{BindingID: binding.ID, ProjectID: project.Value.ID, EnvironmentID: environment.Value.ID,
		ApplicationID: application.Value.ID, BaseRevision: binding.IndexedRevision, Precondition: gitprojection.MutationCreateIfAbsent,
		ChartDigest: chartDigest, PolicyVersion: "appconfig-v1alpha1"}
	body := map[string]any{"environmentId": environment.Value.ID, "applicationId": application.Value.ID,
		"image": "registry.example/api@sha256:" + strings.Repeat("a", 64), "runtime": domain.DefaultWorkloadRuntime(8080, nil)}
	r := f.request("POST", "/v1/deployments", "projected-create", body)
	operation := decode[domain.Operation](t, r)
	if r.StatusCode != http.StatusAccepted {
		t.Fatalf("projected create status=%d operation=%#v", r.StatusCode, operation)
	}
	command, err := f.store.AcceptedGitWriteCommand(operation.ID)
	if err != nil || command.Plan != backend.plan || command.State != gitprojection.WriteCommandPending {
		t.Fatalf("accepted command=%#v err=%v", command, err)
	}
	publication, err := f.store.Publication(t.Context(), operation.ID)
	writeBase, writeBaseErr := publication.WithWriteBase(binding.IndexedRevision, publication.UpdatedAt.Add(time.Second))
	if err != nil || writeBaseErr != nil || f.store.CompareAndSwapPublication(t.Context(), publication, writeBase) != nil {
		t.Fatalf("publication=%#v writeBase=%#v err=%v writeBaseErr=%v", publication, writeBase, err, writeBaseErr)
	}
	candidate, err := writeBase.WithCandidate(strings.Repeat("7", 40), writeBase.UpdatedAt.Add(time.Second))
	if err != nil || f.store.CompareAndSwapPublication(t.Context(), writeBase, candidate) != nil {
		t.Fatalf("candidate=%#v err=%v", candidate, err)
	}
	observation := gitpublication.PullRequestObservation{Repository: candidate.Repository, Number: 23,
		URL: "https://github.com/kuberploy/desired-state/pull/23", TargetRef: candidate.TargetRef,
		HeadRef: candidate.CandidateRef, HeadRevision: candidate.CandidateRevision, State: gitpublication.PullRequestOpen,
		ObservedAt: candidate.UpdatedAt.Add(time.Second)}
	opened, err := candidate.WithPullRequest(observation, observation.ObservedAt)
	if err != nil || f.store.CompareAndSwapPublication(t.Context(), candidate, opened) != nil {
		t.Fatalf("opened=%#v err=%v", opened, err)
	}
	r = f.request("GET", "/v1/operations/"+operation.ID, "", nil)
	projectedOperation := decode[domain.Operation](t, r)
	if r.StatusCode != http.StatusOK || projectedOperation.PullRequest == nil || projectedOperation.PullRequest.Number != 23 ||
		projectedOperation.PullRequest.CandidateRevision != candidate.CandidateRevision || projectedOperation.GitRevision != "" {
		t.Fatalf("projected operation status=%d %#v", r.StatusCode, projectedOperation)
	}
	argoReadiness.err = errors.New("protected rollout stale")
	r = f.request("POST", "/v1/deployments", "projected-create", body)
	replayedWhileStale := decode[domain.Operation](t, r)
	if r.StatusCode != http.StatusAccepted || replayedWhileStale.ID != operation.ID || r.Header.Get("Idempotent-Replay") != "true" {
		t.Fatalf("stale Argo lost-response replay status=%d operation=%#v", r.StatusCode, replayedWhileStale)
	}
	conflictingBody := map[string]any{"environmentId": environment.Value.ID, "applicationId": application.Value.ID,
		"image": "registry.example/api@sha256:" + strings.Repeat("b", 64), "runtime": domain.DefaultWorkloadRuntime(8080, nil)}
	r = f.request("POST", "/v1/deployments", "projected-create", conflictingBody)
	problem := decode[httpapi.Problem](t, r)
	if r.StatusCode != http.StatusConflict || problem.Code != "IdempotencyConflict" {
		t.Fatalf("stale Argo masked idempotency conflict status=%d problem=%#v", r.StatusCode, problem)
	}
	r = f.request("POST", "/v1/deployments", "stale-argo-create", body)
	problem = decode[httpapi.Problem](t, r)
	if r.StatusCode != http.StatusServiceUnavailable || problem.Code != "ArgoDesiredStateRuntimeUnavailable" {
		t.Fatalf("stale Argo accepted new deployment status=%d problem=%#v", r.StatusCode, problem)
	}
	argoReadiness.err = nil
	backend.planErr = gitprojection.ErrProtectionUnavailable
	r = f.request("POST", "/v1/deployments", "unattested-protected-create", body)
	problem = decode[httpapi.Problem](t, r)
	if r.StatusCode != http.StatusServiceUnavailable || problem.Code != "ProtectedGitPolicyUnavailable" {
		t.Fatalf("unattested protected mutation status=%d problem=%#v", r.StatusCode, problem)
	}

	backend.planErr = gitprojection.ErrPreconditionRequired
	r = f.request("POST", "/v1/deployments", "projected-create", body)
	replayed := decode[domain.Operation](t, r)
	if r.StatusCode != http.StatusAccepted || replayed.ID != operation.ID || r.Header.Get("Idempotent-Replay") != "true" {
		t.Fatalf("lost create response replay status=%d operation=%#v", r.StatusCode, replayed)
	}
	r = f.request("POST", "/v1/deployments", "different-create", body)
	problem = decode[httpapi.Problem](t, r)
	if r.StatusCode != http.StatusPreconditionRequired || problem.Code != "PreconditionRequired" {
		t.Fatalf("new update without Git ETag status=%d problem=%#v", r.StatusCode, problem)
	}

	snapshot, err := f.store.GetDeploymentForOperation(t.Context(), operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	document, err := gitprojection.NewDocument(binding, 1, application.Value.ID, binding.IndexedRevision, binding.IndexedRevision,
		strings.Repeat("e", 40), snapshot.ConfigRaw, nil, nil, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err = f.store.PutProjectionDocument(t.Context(), document); err != nil {
		t.Fatal(err)
	}
	etag, err := gitprojection.StrongETag(binding, []gitprojection.Document{document}, nil, chartDigest, "appconfig-v1alpha1")
	if err != nil {
		t.Fatal(err)
	}
	backend.bundle = gitprojection.Bundle{BindingID: binding.ID, TargetRef: binding.TargetRef,
		TargetHeadRevision: binding.TargetHeadRevision, IndexedRevision: binding.IndexedRevision, ConfigRevision: document.ConfigRevision,
		ETag: etag, Documents: []gitprojection.Document{document}, Dependencies: []gitprojection.DependencyState{
			{Path: "tenants/" + binding.ProjectID + "/variables.yaml"},
			{Path: binding.Prefix + "/variables.yaml"},
		}, IndexedAt: binding.IndexedAt}
	backend.bundleErr = nil
	path := "/v1/deployments/" + operation.TargetID + "/config?atLeastRevision=" + binding.IndexedRevision + "&waitSeconds=2"
	r = f.request("GET", path, "", nil)
	bundle := decode[configBundleWire](t, r)
	if r.StatusCode != http.StatusOK || r.Header.Get("ETag") != etag || bundle.ETag != etag || bundle.Freshness != "fresh" ||
		bundle.TargetHeadRevision != binding.TargetHeadRevision || bundle.IndexedRevision != binding.IndexedRevision || backend.atLeast != binding.IndexedRevision || backend.wait != 2*time.Second {
		t.Fatalf("projected bundle status=%d wire=%#v backend=%#v", r.StatusCode, bundle, backend)
	}
	backend.planErr = nil
	backend.plan.Precondition = gitprojection.MutationMatchETag
	backend.plan.ExpectedETag = etag
	change := appconfig.Change{Mode: "jsonPatch", Patch: []appconfig.PatchOperation{{Op: "replace", Path: "/spec/runtime/replicas", Value: 2}}}
	configPath := "/v1/deployments/" + operation.TargetID + "/config"
	r = configRequest(t, f, http.MethodPost, configPath+"/preview", "", change, map[string]string{"If-Match": etag})
	preview := decode[previewWire](t, r)
	if r.StatusCode != http.StatusOK || preview.PreviewToken == "" {
		t.Fatalf("projected preview status=%d body=%#v", r.StatusCode, preview)
	}
	r = configRequest(t, f, http.MethodPut, configPath, "projected-config-save", change, map[string]string{"If-Match": etag, "Preview-Token": preview.PreviewToken})
	saved := decode[domain.Operation](t, r)
	if r.StatusCode != http.StatusAccepted {
		t.Fatalf("projected config save status=%d operation=%#v", r.StatusCode, saved)
	}
	argoReadiness.err = errors.New("protected rollout stale")
	r = configRequest(t, f, http.MethodPut, configPath, "projected-config-save", change, map[string]string{"If-Match": etag, "Preview-Token": preview.PreviewToken})
	replayedSave := decode[domain.Operation](t, r)
	if r.StatusCode != http.StatusAccepted || replayedSave.ID != saved.ID || r.Header.Get("Idempotent-Replay") != "true" {
		t.Fatalf("stale Argo config replay status=%d operation=%#v", r.StatusCode, replayedSave)
	}
	conflictingChange := appconfig.Change{Mode: "jsonPatch", Patch: []appconfig.PatchOperation{{Op: "replace", Path: "/spec/runtime/replicas", Value: 3}}}
	r = configRequest(t, f, http.MethodPut, configPath, "projected-config-save", conflictingChange, map[string]string{"If-Match": etag, "Preview-Token": preview.PreviewToken})
	problem = decode[httpapi.Problem](t, r)
	if r.StatusCode != http.StatusConflict || problem.Code != "IdempotencyConflict" {
		t.Fatalf("stale Argo masked config idempotency conflict status=%d problem=%#v", r.StatusCode, problem)
	}
	r = configRequest(t, f, http.MethodPut, configPath, "stale-argo-config-save", change, map[string]string{"If-Match": etag, "Preview-Token": preview.PreviewToken})
	problem = decode[httpapi.Problem](t, r)
	if r.StatusCode != http.StatusServiceUnavailable || problem.Code != "ArgoDesiredStateRuntimeUnavailable" {
		t.Fatalf("stale Argo accepted new config save status=%d problem=%#v", r.StatusCode, problem)
	}
	argoReadiness.err = nil

	r = f.request("GET", "/v1/capabilities", "", nil)
	capabilities := decode[struct {
		Features map[string]bool `json:"features"`
	}](t, r)
	if !capabilities.Features["git"] || !capabilities.Features["gitops"] || !capabilities.Features["argo"] || !capabilities.Features["argoCD"] {
		t.Fatalf("projection/Argo capabilities=%#v", capabilities.Features)
	}
	readiness.err = errors.New("stale exact runtime")
	r = f.request("GET", "/v1/capabilities", "", nil)
	capabilities = decode[struct {
		Features map[string]bool `json:"features"`
	}](t, r)
	if capabilities.Features["git"] || capabilities.Features["gitops"] {
		t.Fatalf("stale projection runtime was advertised: %#v", capabilities.Features)
	}
	r = f.request("GET", "/readyz", "", nil)
	problem = decode[httpapi.Problem](t, r)
	if r.StatusCode != http.StatusServiceUnavailable || problem.Code != "GitProjectionRuntimeUnavailable" {
		t.Fatalf("stale runtime readiness status=%d problem=%#v", r.StatusCode, problem)
	}
}

func TestArgoCapabilityRequiresBothGitAndProductionReadiness(t *testing.T) {
	backend := &projectionHTTPBackend{}
	gitReadiness := &projectionHTTPReadiness{}
	argoReadiness := &projectionHTTPReadiness{}
	st := memory.New()
	srv := httptest.NewServer(httpapi.New(httpapi.Options{
		Store: st, BootstrapToken: "one-time-secret", Version: "test",
		GitProjection: backend, GitProjectionReadiness: gitReadiness, ArgoReadiness: argoReadiness,
		HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000),
	}))
	jar, _ := cookiejar.New(nil)
	f := &apiFixture{t: t, server: srv, client: &http.Client{Jar: jar}, store: st}
	t.Cleanup(srv.Close)
	f.bootstrap()

	assert := func(gitErr, argoErr error, want bool) {
		t.Helper()
		gitReadiness.err, argoReadiness.err = gitErr, argoErr
		response := f.request("GET", "/v1/capabilities", "", nil)
		capabilities := decode[struct {
			Features map[string]bool `json:"features"`
		}](t, response)
		if capabilities.Features["argo"] != want || capabilities.Features["argoCD"] != want ||
			capabilities.Features["gitops"] != want || capabilities.Features["git"] != (gitErr == nil) {
			t.Fatalf("gitErr=%v argoErr=%v features=%#v", gitErr, argoErr, capabilities.Features)
		}
	}
	assert(nil, nil, true)
	assert(errors.New("git stale"), nil, false)
	assert(nil, errors.New("Argo stale"), false)

	argoReadiness.err = errors.New("Argo stale")
	response := f.request("GET", "/readyz", "", nil)
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusServiceUnavailable || problem.Code != "ArgoDesiredStateRuntimeUnavailable" {
		t.Fatalf("Argo readiness status=%d problem=%#v", response.StatusCode, problem)
	}
}

func TestProjectionHTTPRevisionFenceValidationIsBounded(t *testing.T) {
	backend := &projectionHTTPBackend{}
	f := newProjectionAPI(t, backend, &projectionHTTPReadiness{})
	f.bootstrap()
	for _, query := range []string{"?waitSeconds=1", "?atLeastRevision=HEAD", "?atLeastRevision=" + strings.Repeat("a", 40) + "&waitSeconds=11"} {
		r := f.request("GET", "/v1/deployments/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/config"+query, "", nil)
		problem := decode[httpapi.Problem](t, r)
		if r.StatusCode != http.StatusUnprocessableEntity || problem.Code != "InvalidRevisionFence" {
			t.Fatalf("query %q status=%d problem=%#v", query, r.StatusCode, problem)
		}
	}
	if backend.bundleCalls != 0 {
		t.Fatalf("invalid revision fence reached backend %d times", backend.bundleCalls)
	}
}
