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

	"github.com/kuberploy/kuberploy/internal/buildpromotion"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

type promotionProjectionCatalog struct {
	projected buildpromotion.ProjectedBuild
	err       error
}

func (c *promotionProjectionCatalog) SuccessfulReleaseProjection(context.Context, string) (buildpromotion.ProjectedBuild, error) {
	return c.projected, c.err
}

type promotionReleaseCatalog struct {
	release domain.RegistryRelease
	err     error
}

func (c *promotionReleaseCatalog) RegistryRelease(context.Context, string) (domain.RegistryRelease, error) {
	return c.release, c.err
}

func newPromotionAPI(t *testing.T, gitBackends ...*projectionHTTPBackend) (*apiFixture, *promotionProjectionCatalog, *promotionReleaseCatalog) {
	t.Helper()
	st := memory.New()
	projections := &promotionProjectionCatalog{}
	releases := &promotionReleaseCatalog{}
	resolver := &buildpromotion.Resolver{Projections: projections, Releases: releases, Resources: st, Access: st}
	options := httpapi.Options{Store: st, BootstrapToken: "one-time-secret", Version: "test", BuildPromotions: resolver, HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000)}
	if len(gitBackends) == 1 && gitBackends[0] != nil {
		options.GitProjection = gitBackends[0]
		options.GitProjectionReadiness = &projectionHTTPReadiness{}
		options.ArgoReadiness = &projectionHTTPReadiness{}
	}
	srv := httptest.NewServer(httpapi.New(options))
	jar, _ := cookiejar.New(nil)
	fixture := &apiFixture{t: t, server: srv, client: &http.Client{Jar: jar}, store: st}
	t.Cleanup(srv.Close)
	return fixture, projections, releases
}

func seedPromotion(t *testing.T, f *apiFixture, projections *promotionProjectionCatalog, releases *promotionReleaseCatalog) (domain.Environment, domain.Application) {
	t.Helper()
	admin := f.bootstrap()
	project, err := f.store.CreateProject(t.Context(), admin.ID, "promotion-project", "promotion-project", domain.CreateProject{Name: "Promotion", Slug: "promotion"})
	if err != nil {
		t.Fatal(err)
	}
	environment, err := f.store.CreateEnvironment(t.Context(), admin.ID, "promotion-environment", "promotion-environment", domain.CreateEnvironment{ProjectID: project.Value.ID, Name: "Production", Slug: "production"})
	if err != nil {
		t.Fatal(err)
	}
	application, err := f.store.CreateApplication(t.Context(), admin.ID, "promotion-application", "promotion-application", domain.CreateApplication{ProjectID: project.Value.ID, Name: "API", Slug: "api"})
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 8, 9, 5, 0, 0, 0, time.UTC)
	completed := created.Add(time.Minute)
	digest := "sha256:" + strings.Repeat("a", 64)
	repository := "kp/projects/" + project.Value.ID + "/services/" + application.Value.ID + "/image"
	projections.projected = buildpromotion.ProjectedBuild{
		AttemptID: "22222222-2222-4222-8222-222222222222", DefinitionID: "33333333-3333-4333-8333-333333333333",
		ProjectID: project.Value.ID, ApplicationID: application.Value.ID, Generation: 1, CommitSHA: strings.Repeat("b", 40), DefinitionDigest: "sha256:" + strings.Repeat("c", 64),
		RegistryTargetID: "77777777-7777-4777-8777-777777777777", RegistryServer: "registry.example.test", Repository: repository,
		ImageReference: "registry.example.test/" + repository + "@" + digest, ImageDigest: digest, ReleaseID: "22222222-2222-4222-8222-222222222222",
		CreatedAt: created, CompletedAt: completed, ProjectionCompletedAt: completed.Add(time.Second),
	}
	releases.release = domain.RegistryRelease{ID: projections.projected.ReleaseID, RegistryTargetID: projections.projected.RegistryTargetID, ServiceID: application.Value.ID, Repository: repository, RootDigest: digest, CreatedAt: created, SucceededAt: &completed, Availability: domain.RegistryArtifactPresent}
	return environment.Value, application.Value
}

func TestBuildPromotionDerivesImageAndReplaysBeforeReleaseReadiness(t *testing.T) {
	f, projections, releases := newPromotionAPI(t)
	environment, application := seedPromotion(t, f, projections, releases)
	body := map[string]any{"environmentId": environment.ID, "runtime": domain.DefaultWorkloadRuntime(8080, nil)}
	r := f.request(http.MethodPost, "/v1/builds/"+projections.projected.AttemptID+"/promote", "promote-build-1", body)
	operation := decode[domain.Operation](t, r)
	if r.StatusCode != http.StatusAccepted || operation.TargetID == "" {
		t.Fatalf("promotion status=%d operation=%#v", r.StatusCode, operation)
	}
	deployment, err := f.store.GetDeployment(t.Context(), operation.TargetID)
	if err != nil || deployment.ApplicationID != application.ID || deployment.EnvironmentID != environment.ID || deployment.Image != projections.projected.ImageReference {
		t.Fatalf("derived deployment=%#v err=%v", deployment, err)
	}
	releases.err = errors.New("registry observation unavailable")
	r = f.request(http.MethodPost, "/v1/builds/"+projections.projected.AttemptID+"/promote", "promote-build-1", body)
	replayed := decode[domain.Operation](t, r)
	if r.StatusCode != http.StatusAccepted || replayed.ID != operation.ID || r.Header.Get("Idempotent-Replay") != "true" || f.store.OutboxCount() != 1 {
		t.Fatalf("lost-response replay status=%d operation=%#v", r.StatusCode, replayed)
	}
	body["runtime"] = domain.DefaultWorkloadRuntime(9090, nil)
	r = f.request(http.MethodPost, "/v1/builds/"+projections.projected.AttemptID+"/promote", "promote-build-1", body)
	problem := decode[httpapi.Problem](t, r)
	if r.StatusCode != http.StatusConflict || problem.Code != "IdempotencyConflict" || f.store.OutboxCount() != 1 {
		t.Fatalf("promotion idempotency conflict status=%d problem=%#v", r.StatusCode, problem)
	}
}

func TestBuildPromotionMapsProtectedGitCASConflictTo409(t *testing.T) {
	backend := &projectionHTTPBackend{planErr: gitprojection.ErrConflict}
	f, projections, releases := newPromotionAPI(t, backend)
	environment, _ := seedPromotion(t, f, projections, releases)
	body := map[string]any{"environmentId": environment.ID, "runtime": domain.DefaultWorkloadRuntime(8080, nil)}
	r := f.request(http.MethodPost, "/v1/builds/"+projections.projected.AttemptID+"/promote", "promote-cas", body)
	problem := decode[httpapi.Problem](t, r)
	if r.StatusCode != http.StatusConflict || problem.Code != "PromotionCASConflict" || f.store.OutboxCount() != 0 {
		t.Fatalf("promotion CAS status=%d problem=%#v", r.StatusCode, problem)
	}
}

func TestBuildPromotionFailsClosedForPendingProjectionAndCallerAuthorityFields(t *testing.T) {
	f, projections, releases := newPromotionAPI(t)
	environment, _ := seedPromotion(t, f, projections, releases)
	projections.err = buildpromotion.ErrNotReady
	body := map[string]any{"environmentId": environment.ID, "runtime": domain.DefaultWorkloadRuntime(8080, nil)}
	r := f.request(http.MethodPost, "/v1/builds/"+projections.projected.AttemptID+"/promote", "promote-pending", body)
	problem := decode[httpapi.Problem](t, r)
	if r.StatusCode != http.StatusServiceUnavailable || problem.Code != "BuildReleaseProjectionUnavailable" || f.store.OutboxCount() != 0 {
		t.Fatalf("pending promotion status=%d problem=%#v", r.StatusCode, problem)
	}
	projections.err = nil
	body["image"] = "attacker.test/image@sha256:" + strings.Repeat("d", 64)
	r = f.request(http.MethodPost, "/v1/builds/"+projections.projected.AttemptID+"/promote", "promote-forged", body)
	problem = decode[httpapi.Problem](t, r)
	if r.StatusCode != http.StatusBadRequest || problem.Code != "InvalidJSON" || f.store.OutboxCount() != 0 {
		t.Fatalf("caller image accepted status=%d problem=%#v", r.StatusCode, problem)
	}
}
