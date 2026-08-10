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

	"github.com/kuberploy/kuberploy/internal/autodeploy"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	base "github.com/kuberploy/kuberploy/internal/store"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

type autoDeployReplayStore struct {
	policy        autodeploy.Policy
	revision      autodeploy.Revision
	key           string
	action        string
	digest        string
	replayCalls   int
	mutationCalls int
}

func (s *autoDeployReplayStore) PolicyCommandReplay(_ context.Context, _ string, key, action, digest string) (autodeploy.Policy, autodeploy.Revision, bool, error) {
	s.replayCalls++
	if s.key == "" {
		s.key, s.action, s.digest = key, action, digest
	}
	if key != s.key || action != s.action || digest != s.digest {
		return autodeploy.Policy{}, autodeploy.Revision{}, false, base.ErrIdempotencyConflict
	}
	return s.policy, s.revision, true, nil
}

func (s *autoDeployReplayStore) CreatePolicy(context.Context, autodeploy.Policy, autodeploy.Revision, string, string, string) (autodeploy.Policy, autodeploy.Revision, bool, error) {
	s.mutationCalls++
	return autodeploy.Policy{}, autodeploy.Revision{}, false, errors.New("unexpected create")
}

func (s *autoDeployReplayStore) RevisePolicy(context.Context, autodeploy.Policy, autodeploy.Revision, string, string, string) (autodeploy.Policy, autodeploy.Revision, bool, error) {
	s.mutationCalls++
	return autodeploy.Policy{}, autodeploy.Revision{}, false, errors.New("unexpected revise")
}

func (s *autoDeployReplayStore) PolicyForActor(context.Context, string, string) (autodeploy.PolicyStatus, error) {
	return autodeploy.PolicyStatus{}, errors.New("mutable policy lookup unavailable")
}
func (s *autoDeployReplayStore) PoliciesForApplication(context.Context, string, string) ([]autodeploy.PolicyStatus, error) {
	return nil, errors.New("mutable policy lookup unavailable")
}
func (s *autoDeployReplayStore) PolicyRevisionsForActor(context.Context, string, string, int) ([]autodeploy.Revision, error) {
	return nil, errors.New("mutable revision lookup unavailable")
}
func (s *autoDeployReplayStore) PolicyRunsForActor(context.Context, string, string, int) ([]autodeploy.Run, error) {
	return nil, errors.New("mutable run lookup unavailable")
}

type failingAutoDeployReadiness struct{ calls int }

func (p *failingAutoDeployReadiness) Probe(context.Context) error {
	p.calls++
	return errors.New("controller lease stale")
}

func newAutoDeployReplayAPI(t *testing.T) (*apiFixture, *autoDeployReplayStore, *failingAutoDeployReadiness) {
	t.Helper()
	st := memory.New()
	replays := &autoDeployReplayStore{}
	readiness := &failingAutoDeployReadiness{}
	service := &autodeploy.PolicyService{Store: replays}
	srv := httptest.NewServer(httpapi.New(httpapi.Options{Store: st, BootstrapToken: "one-time-secret", Version: "test",
		AutoDeployService: service, AutoDeployPolicies: replays, AutoDeployReadiness: readiness,
		HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000)}))
	jar, _ := cookiejar.New(nil)
	fixture := &apiFixture{t: t, server: srv, client: &http.Client{Jar: jar}, store: st}
	t.Cleanup(srv.Close)
	return fixture, replays, readiness
}

func TestAutoDeployPolicyReplayPrecedesReadinessAndMutableCatalog(t *testing.T) {
	f, replays, readiness := newAutoDeployReplayAPI(t)
	admin := f.bootstrap()
	project, err := f.store.CreateProject(t.Context(), admin.ID, "auto-replay-project", "auto-replay-project", domain.CreateProject{Name: "Auto replay", Slug: "auto-replay"})
	if err != nil {
		t.Fatal(err)
	}
	application, err := f.store.CreateApplication(t.Context(), admin.ID, "auto-replay-application", "auto-replay-application",
		domain.CreateApplication{ProjectID: project.Value.ID, Name: "API", Slug: "api"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	replays.policy = autodeploy.Policy{ID: "11111111-1111-4111-8111-111111111111", BuildDefinitionID: "22222222-2222-4222-8222-222222222222",
		ProjectID: project.Value.ID, ApplicationID: application.Value.ID, EnvironmentID: "33333333-3333-4333-8333-333333333333",
		CurrentRevision: 1, CreatedBy: admin.ID, CreatedAt: now}
	replays.revision = autodeploy.Revision{PolicyID: replays.policy.ID, Revision: 1, Enabled: true,
		Template: autodeploy.Template{SourceDeploymentID: "44444444-4444-4444-8444-444444444444", SourceDeploymentGeneration: 1,
			SourceConfigETag: `"sha256:` + strings.Repeat("a", 64) + `"`}, TemplateDigest: "sha256:" + strings.Repeat("b", 64),
		ServiceActorID: "55555555-5555-4555-8555-555555555555", CreatedBy: admin.ID, CreatedAt: now}
	body := map[string]any{"buildDefinitionId": replays.policy.BuildDefinitionID, "environmentId": replays.policy.EnvironmentID,
		"templateDeploymentId": replays.revision.Template.SourceDeploymentID, "serviceActorId": replays.revision.ServiceActorID, "enabled": true}
	response := f.request(http.MethodPost, "/v1/applications/"+application.Value.ID+"/auto-deploy-policies", "accepted-create-0001", body)
	view := decode[map[string]any](t, response)
	if response.StatusCode != http.StatusCreated || response.Header.Get("Idempotent-Replay") != "true" || view["id"] != replays.policy.ID {
		t.Fatalf("create replay status=%d header=%q view=%#v", response.StatusCode, response.Header.Get("Idempotent-Replay"), view)
	}
	if readiness.calls != 0 || replays.mutationCalls != 0 {
		t.Fatalf("replay consulted readiness or mutation: readiness=%d mutations=%d", readiness.calls, replays.mutationCalls)
	}
	body["enabled"] = false
	response = f.request(http.MethodPost, "/v1/applications/"+application.Value.ID+"/auto-deploy-policies", "accepted-create-0001", body)
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusConflict || problem.Code != "IdempotencyConflict" {
		t.Fatalf("changed create replay status=%d problem=%#v", response.StatusCode, problem)
	}
	otherApplication, err := f.store.CreateApplication(t.Context(), admin.ID, "auto-replay-application-2", "auto-replay-application-2",
		domain.CreateApplication{ProjectID: project.Value.ID, Name: "Other", Slug: "other"})
	if err != nil {
		t.Fatal(err)
	}
	response = f.request(http.MethodPost, "/v1/applications/"+otherApplication.Value.ID+"/auto-deploy-policies", "accepted-create-0001", body)
	problem = decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusConflict || problem.Code != "IdempotencyConflict" {
		t.Fatalf("cross-route replay disclosed accepted policy: status=%d problem=%#v", response.StatusCode, problem)
	}

	replays.key, replays.action, replays.digest = "", "", ""
	revise := map[string]any{"templateDeploymentId": replays.revision.Template.SourceDeploymentID,
		"serviceActorId": replays.revision.ServiceActorID, "enabled": false, "expectedRevision": 1}
	response = f.request(http.MethodPut, "/v1/auto-deploy-policies/"+replays.policy.ID, "accepted-revise-0001", revise)
	view = decode[map[string]any](t, response)
	if response.StatusCode != http.StatusOK || response.Header.Get("Idempotent-Replay") != "true" || view["id"] != replays.policy.ID || readiness.calls != 0 {
		t.Fatalf("revise replay status=%d header=%q view=%#v readiness=%d", response.StatusCode, response.Header.Get("Idempotent-Replay"), view, readiness.calls)
	}
}

func TestAutoDeployPolicyStrictDTOsRejectIgnoredAuthorityFields(t *testing.T) {
	f, _, _ := newAutoDeployReplayAPI(t)
	admin := f.bootstrap()
	project, _ := f.store.CreateProject(t.Context(), admin.ID, "auto-dto-project", "auto-dto-project", domain.CreateProject{Name: "DTO", Slug: "dto"})
	application, _ := f.store.CreateApplication(t.Context(), admin.ID, "auto-dto-application", "auto-dto-application", domain.CreateApplication{ProjectID: project.Value.ID, Name: "DTO", Slug: "dto"})
	response := f.request(http.MethodPost, "/v1/applications/"+application.Value.ID+"/auto-deploy-policies", "dto-create-000001",
		map[string]any{"buildDefinitionId": "22222222-2222-4222-8222-222222222222", "environmentId": "33333333-3333-4333-8333-333333333333",
			"templateDeploymentId": "44444444-4444-4444-8444-444444444444", "serviceActorId": "55555555-5555-4555-8555-555555555555", "enabled": true, "expectedRevision": 1})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("create accepted ignored expectedRevision: %d", response.StatusCode)
	}
	response.Body.Close()
	response = f.request(http.MethodPut, "/v1/auto-deploy-policies/11111111-1111-4111-8111-111111111111", "dto-revise-00001",
		map[string]any{"templateDeploymentId": "44444444-4444-4444-8444-444444444444", "serviceActorId": "55555555-5555-4555-8555-555555555555",
			"enabled": true, "expectedRevision": 1, "environmentId": "33333333-3333-4333-8333-333333333333"})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("revise accepted ignored environmentId: %d", response.StatusCode)
	}
	response.Body.Close()
}
