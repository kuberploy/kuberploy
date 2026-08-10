package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/helmapps"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

type helmHTTPBackend struct {
	capabilities               helmapps.Capabilities
	documents                  []helmapps.ApprovalDocument
	preview                    helmapps.ValuesPreview
	revision                   helmapps.ReleaseRevision
	status                     helmapps.ReleaseStatus
	upsert                     helmapps.UpsertReleaseRequest
	rollback                   helmapps.RollbackReleaseRequest
	upsertCalls, rollbackCalls int
	admitCalls, catalogCalls   int
	admission                  helmapps.ApprovalAdmissionRequest
	rendered                   helmapps.RenderedManifestPreview
}

func (b *helmHTTPBackend) Capabilities(context.Context) (helmapps.Capabilities, error) {
	return b.capabilities, nil
}
func (b *helmHTTPBackend) ApprovalCatalog(context.Context, int) ([]helmapps.ApprovalDocument, error) {
	return b.documents, nil
}
func (b *helmHTTPBackend) ApprovalDocument(context.Context, helmapps.ApprovalKey) (helmapps.ApprovalDocument, error) {
	if len(b.documents) == 0 {
		return helmapps.ApprovalDocument{}, helmapps.ErrNotFound
	}
	return b.documents[0], nil
}
func (b *helmHTTPBackend) PreviewValues(context.Context, helmapps.ReleaseTarget, helmapps.ApprovalKey, []byte) (helmapps.ValuesPreview, error) {
	return b.preview, nil
}
func (b *helmHTTPBackend) Upsert(_ context.Context, request helmapps.UpsertReleaseRequest, _ time.Time) (helmapps.ReleaseRevision, bool, error) {
	b.upsertCalls++
	b.upsert = request
	return b.revision, false, nil
}
func (b *helmHTTPBackend) Retry(context.Context, helmapps.RetryReleaseRequest, time.Time) (helmapps.ReleaseRevision, bool, error) {
	return b.revision, false, nil
}
func (b *helmHTTPBackend) Disable(context.Context, helmapps.DisableReleaseRequest, time.Time) (helmapps.ReleaseRevision, bool, error) {
	return b.revision, false, nil
}
func (b *helmHTTPBackend) Rollback(_ context.Context, request helmapps.RollbackReleaseRequest, _ time.Time) (helmapps.ReleaseRevision, bool, error) {
	b.rollbackCalls++
	b.rollback = request
	return b.revision, false, nil
}
func (b *helmHTTPBackend) Head(context.Context, helmapps.ReleaseTarget) (helmapps.ReleaseStatus, error) {
	return b.status, nil
}
func (b *helmHTTPBackend) History(context.Context, helmapps.ReleaseTarget, int) ([]helmapps.ReleaseStatus, error) {
	return []helmapps.ReleaseStatus{b.status}, nil
}
func (b *helmHTTPBackend) Catalog(_ context.Context, _ int) ([]helmapps.ApprovalDocument, error) {
	b.catalogCalls++
	return b.documents, nil
}
func (b *helmHTTPBackend) Admit(_ context.Context, request helmapps.ApprovalAdmissionRequest) (helmapps.ApprovalDocument, bool, error) {
	b.admitCalls++
	b.admission = request
	if len(b.documents) == 0 {
		return helmapps.ApprovalDocument{}, false, helmapps.ErrNotFound
	}
	return b.documents[0], false, nil
}
func (b *helmHTTPBackend) Preview(_ context.Context, _ helmapps.ReleaseTarget) (helmapps.RenderedManifestPreview, error) {
	return b.rendered, nil
}

type helmHTTPFixture struct {
	*apiFixture
	backend     *helmHTTPBackend
	project     domain.Project
	environment domain.Environment
	application domain.Application
}

func newHelmHTTPFixture(t *testing.T, backend *helmHTTPBackend) *helmHTTPFixture {
	t.Helper()
	central := memory.New()
	server := httptest.NewServer(httpapi.New(httpapi.Options{Store: central, BootstrapToken: "one-time-secret",
		Version: "test", HelmApplications: backend, HelmApprovals: backend, HelmRenderedPreviews: backend,
		HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000)}))
	jar, _ := cookiejar.New(nil)
	base := &apiFixture{t: t, server: server, client: &http.Client{Jar: jar}, store: central}
	t.Cleanup(server.Close)
	fixture := &helmHTTPFixture{apiFixture: base, backend: backend}
	fixture.bootstrap()
	team := decode[domain.Team](t, fixture.request(http.MethodPost, "/v1/teams", "helm-team-create", map[string]string{"name": "Helm", "slug": "helm"}))
	fixture.project = decode[domain.Project](t, fixture.request(http.MethodPost, "/v1/projects", "helm-project-create", map[string]string{"name": "Helm", "slug": "helm", "teamId": team.ID}))
	fixture.environment = decode[domain.Environment](t, fixture.request(http.MethodPost, "/v1/environments", "helm-environment-create", map[string]string{"projectId": fixture.project.ID, "name": "Production", "slug": "production"}))
	fixture.application = decode[domain.Application](t, fixture.request(http.MethodPost, "/v1/applications", "helm-application-create", map[string]string{"projectId": fixture.project.ID, "name": "API", "slug": "api"}))
	backend.revision = helmapps.ReleaseRevision{ID: "66666666-6666-4666-8666-666666666666", Generation: 1,
		Target:      helmapps.ReleaseTarget{ProjectID: fixture.project.ID, EnvironmentID: fixture.environment.ID, ApplicationID: fixture.application.ID},
		ReleaseName: "api", Action: helmapps.ReleaseInitial, DesiredEnabled: true,
		Approval:        helmapps.ApprovalKey{ID: "22222222-2222-4222-8222-222222222222", Revision: 1},
		RenderCommandID: "77777777-7777-4777-8777-777777777777", ValuesDigest: "sha256:" + string(bytes.Repeat([]byte{'a'}, 64)),
		IntentDigest: "sha256:" + string(bytes.Repeat([]byte{'b'}, 64)), RequestID: "request", CreatedAt: time.Now().UTC()}
	backend.status = helmapps.ReleaseStatus{Revision: backend.revision, Phase: helmapps.ReleasePhaseRendering, RenderState: "queued"}
	return fixture
}

func (f *helmHTTPFixture) basePath() string {
	return "/v1/applications/" + f.application.ID + "/environments/" + f.environment.ID + "/helm"
}

func TestHelmMutationsRemainClosedUntilAllExactRuntimeFencesAreReady(t *testing.T) {
	backend := &helmHTTPBackend{}
	fixture := newHelmHTTPFixture(t, backend)
	response := fixture.request(http.MethodPut, fixture.basePath()+"/release", "helm-release-upsert-0001",
		map[string]any{"approvalId": "22222222-2222-4222-8222-222222222222", "approvalRevision": 1, "valuesYaml": "replicas: 2\n"})
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusServiceUnavailable || problem.Code != "HelmRuntimeNotReady" || backend.upsertCalls != 0 {
		t.Fatalf("status=%d problem=%#v calls=%d", response.StatusCode, problem, backend.upsertCalls)
	}
	capabilityResponse := fixture.request(http.MethodGet, "/v1/capabilities", "", nil)
	capabilities := decode[struct {
		Features map[string]bool `json:"features"`
	}](t, capabilityResponse)
	if capabilities.Features["helmDeployments"] || capabilities.Features["helmRollbacks"] || capabilities.Features["rollbacks"] {
		t.Fatalf("stale Helm runtime advertised: %#v", capabilities.Features)
	}
}

func TestHelmUpsertUsesOnlyAuthorizedPathScopeAndCanonicalActor(t *testing.T) {
	backend := &helmHTTPBackend{capabilities: helmapps.Capabilities{HelmDeployments: true, HelmRollbacks: true}}
	fixture := newHelmHTTPFixture(t, backend)
	response := fixture.request(http.MethodPut, fixture.basePath()+"/release", "helm-release-upsert-0001",
		map[string]any{"approvalId": "22222222-2222-4222-8222-222222222222", "approvalRevision": 1, "valuesYaml": "replicas: 2\n"})
	view := decode[struct {
		ID       string `json:"id"`
		Approval struct {
			ID       string `json:"id"`
			Revision int64  `json:"revision"`
		} `json:"approval"`
	}](t, response)
	if response.StatusCode != http.StatusAccepted || view.ID != backend.revision.ID || view.Approval.ID != backend.revision.Approval.ID ||
		backend.upsertCalls != 1 || backend.upsert.Target != backend.revision.Target || string(backend.upsert.ValuesYAML) != "replicas: 2\n" ||
		backend.upsert.Actor.IdempotencyKey != "helm-release-upsert-0001" || backend.upsert.Actor.ID == "" || backend.upsert.Actor.RequestID == "" {
		t.Fatalf("status=%d view=%#v request=%#v", response.StatusCode, view, backend.upsert)
	}
	capabilityResponse := fixture.request(http.MethodGet, "/v1/capabilities", "", nil)
	capabilities := decode[struct {
		Features map[string]bool `json:"features"`
	}](t, capabilityResponse)
	if !capabilities.Features["helmDeployments"] || !capabilities.Features["helmRollbacks"] || !capabilities.Features["rollbacks"] {
		t.Fatalf("ready Helm runtime omitted: %#v", capabilities.Features)
	}
}

func TestHelmRequestRejectsDuplicateJSONFieldsBeforeBackend(t *testing.T) {
	backend := &helmHTTPBackend{capabilities: helmapps.Capabilities{HelmDeployments: true, HelmRollbacks: true}}
	fixture := newHelmHTTPFixture(t, backend)
	raw := `{"approvalId":"22222222-2222-4222-8222-222222222222","approvalRevision":1,"valuesYaml":"{}\n","valuesYaml":"caller\n"}`
	request, _ := http.NewRequest(http.MethodPut, fixture.server.URL+fixture.basePath()+"/release", bytes.NewBufferString(raw))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "helm-release-duplicate-0001")
	request.Header.Set("X-CSRF-Token", fixture.csrf)
	response, err := fixture.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusBadRequest || problem.Code != "InvalidJSON" || backend.upsertCalls != 0 {
		t.Fatalf("status=%d problem=%#v calls=%d", response.StatusCode, problem, backend.upsertCalls)
	}
}

func TestHelmRollbackIsIndependentPermissionedTransition(t *testing.T) {
	backend := &helmHTTPBackend{capabilities: helmapps.Capabilities{HelmDeployments: true, HelmRollbacks: true}}
	fixture := newHelmHTTPFixture(t, backend)
	source := "88888888-8888-4888-8888-888888888888"
	response := fixture.request(http.MethodPost, fixture.basePath()+"/release/rollback", "helm-release-rollback-0001", map[string]string{"sourceRevisionId": source})
	if response.StatusCode != http.StatusAccepted {
		body, _ := json.Marshal(decode[httpapi.Problem](t, response))
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
	response.Body.Close()
	if backend.rollbackCalls != 1 || backend.rollback.Target != backend.revision.Target || backend.rollback.SourceRevisionID != source || backend.rollback.Actor.IdempotencyKey != "helm-release-rollback-0001" {
		t.Fatalf("rollback=%#v calls=%d", backend.rollback, backend.rollbackCalls)
	}
}

func TestPlatformHelmApprovalAdmissionAcceptsOnlyPinnedCoordinates(t *testing.T) {
	backend := &helmHTTPBackend{documents: []helmapps.ApprovalDocument{{Approval: helmapps.Approval{
		ApprovalKey:   helmapps.ApprovalKey{ID: "22222222-2222-4222-8222-222222222222", Revision: 1},
		OCIRepository: "oci://registry.example.test/charts/demo", ChartVersion: "1.2.3",
		ManifestDigest: "sha256:" + string(bytes.Repeat([]byte{'a'}, 64)), PackageDigest: "sha256:" + string(bytes.Repeat([]byte{'b'}, 64)),
		ValuesSchemaDigest: "sha256:" + string(bytes.Repeat([]byte{'c'}, 64)), RendererImage: helmapps.RendererImage,
		RendererVersion: helmapps.HelmVersion, PolicyVersion: helmapps.PolicyVersion},
		DocumentsDigest: "sha256:" + string(bytes.Repeat([]byte{'d'}, 64)), ValuesSchemaJSON: []byte(`{"type":"object"}`),
		DefaultValuesYAML: []byte("replicas: 1\n"), CreatedAt: time.Now().UTC()}}}
	fixture := newHelmHTTPFixture(t, backend)
	response := fixture.request(http.MethodPost, "/v1/platform/helm/approvals", "helm-approval-admit-0001", map[string]any{
		"repository": "oci://registry.example.test/charts/demo", "version": "1.2.3",
		"manifestDigest":     "sha256:" + string(bytes.Repeat([]byte{'a'}, 64)),
		"packageDigest":      "sha256:" + string(bytes.Repeat([]byte{'b'}, 64)),
		"valuesSchemaDigest": "sha256:" + string(bytes.Repeat([]byte{'c'}, 64)),
	})
	if response.StatusCode != http.StatusCreated || backend.admitCalls != 1 ||
		backend.admission.ActorID == "" || backend.admission.IdempotencyKey != "helm-approval-admit-0001" ||
		backend.admission.OCIRepository != "oci://registry.example.test/charts/demo" {
		t.Fatalf("status=%d calls=%d admission=%#v", response.StatusCode, backend.admitCalls, backend.admission)
	}
	var view map[string]any
	view = decode[map[string]any](t, response)
	for _, forbidden := range []string{"createdBy", "idempotencyKey", "credentials", "packageBytes", "rawManifests"} {
		if _, exists := view[forbidden]; exists {
			t.Fatalf("approval response exposed %q: %#v", forbidden, view)
		}
	}

	response = fixture.request(http.MethodPost, "/v1/platform/helm/approvals", "helm-approval-admit-0002", map[string]any{
		"repository": "oci://registry.example.test/charts/demo", "version": "1.2.3",
		"manifestDigest":     "sha256:" + string(bytes.Repeat([]byte{'a'}, 64)),
		"packageDigest":      "sha256:" + string(bytes.Repeat([]byte{'b'}, 64)),
		"valuesSchemaDigest": "sha256:" + string(bytes.Repeat([]byte{'c'}, 64)), "valuesSchema": map[string]any{},
	})
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusBadRequest || problem.Code != "InvalidJSON" || backend.admitCalls != 1 {
		t.Fatalf("unknown admission field status=%d problem=%#v calls=%d", response.StatusCode, problem, backend.admitCalls)
	}
}

func TestHelmRenderedPreviewReturnsBoundedSanitizedYAML(t *testing.T) {
	backend := &helmHTTPBackend{}
	fixture := newHelmHTTPFixture(t, backend)
	backend.rendered = helmapps.RenderedManifestPreview{ReleaseRevisionID: backend.revision.ID, Generation: 1,
		Target: backend.revision.Target, ManifestDigest: "sha256:" + string(bytes.Repeat([]byte{'a'}, 64)),
		InventoryDigest: "sha256:" + string(bytes.Repeat([]byte{'b'}, 64)), ResourceCount: 1,
		PreviewBytes: len("apiVersion: apps/v1\nkind: Deployment\n"),
		Resources: []helmapps.RenderedResourcePreview{{APIVersion: "apps/v1", Kind: "Deployment", Namespace: fixture.environment.Namespace, Name: "api",
			SanitizedYAML: "apiVersion: apps/v1\nkind: Deployment\n", PreviewOmitted: false}}}
	response := fixture.request(http.MethodGet, fixture.basePath()+"/rendered-preview", "", nil)
	view := decode[map[string]any](t, response)
	if response.StatusCode != http.StatusOK || view["resourceCount"] != float64(1) {
		t.Fatalf("status=%d view=%#v", response.StatusCode, view)
	}
	for _, forbidden := range []string{"target", "raw", "manifests", "job", "uid"} {
		if _, exists := view[forbidden]; exists {
			t.Fatalf("rendered preview exposed %q: %#v", forbidden, view)
		}
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"sanitizedYaml":"apiVersion: apps/v1`)) ||
		bytes.Contains(encoded, []byte("caller-controlled-secret")) {
		t.Fatalf("unexpected sanitized response: %s", encoded)
	}
}

func TestHelmRenderedPreviewRejectsUnboundedOrInconsistentBackendProjection(t *testing.T) {
	for _, test := range []struct {
		name     string
		resource helmapps.RenderedResourcePreview
		bytes    int
	}{
		{name: "oversized YAML", resource: helmapps.RenderedResourcePreview{APIVersion: "v1", Kind: "ConfigMap",
			Namespace: "preview", Name: "oversized", SanitizedYAML: string(bytes.Repeat([]byte{'x'}, helmapps.MaximumSanitizedResourcePreviewBytes+1))},
			bytes: helmapps.MaximumSanitizedResourcePreviewBytes + 1},
		{name: "omitted with content", resource: helmapps.RenderedResourcePreview{APIVersion: "v1", Kind: "ConfigMap",
			Namespace: "preview", Name: "omitted", SanitizedYAML: "raw-secret", PreviewOmitted: true}, bytes: 0},
		{name: "incorrect aggregate", resource: helmapps.RenderedResourcePreview{APIVersion: "v1", Kind: "ConfigMap",
			Namespace: "preview", Name: "wrong", SanitizedYAML: "apiVersion: v1\n"}, bytes: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &helmHTTPBackend{}
			fixture := newHelmHTTPFixture(t, backend)
			backend.rendered = helmapps.RenderedManifestPreview{ReleaseRevisionID: backend.revision.ID, Generation: 1,
				Target: backend.revision.Target, ManifestDigest: "sha256:" + string(bytes.Repeat([]byte{'a'}, 64)),
				InventoryDigest: "sha256:" + string(bytes.Repeat([]byte{'b'}, 64)), ResourceCount: 1,
				PreviewBytes: test.bytes, Resources: []helmapps.RenderedResourcePreview{test.resource}}
			response := fixture.request(http.MethodGet, fixture.basePath()+"/rendered-preview", "", nil)
			problem := decode[httpapi.Problem](t, response)
			if response.StatusCode != http.StatusConflict || problem.Code != "HelmReleaseConflict" {
				t.Fatalf("status=%d problem=%+v", response.StatusCode, problem)
			}
		})
	}
}
