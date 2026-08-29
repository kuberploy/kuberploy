package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	"github.com/kuberploy/kuberploy/internal/registry"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

type registryHTTPExecutor struct {
	coordinator       *registry.Service
	calls             int
	failBlobSweepOnce bool
}

type registryHTTPReadiness struct{ err error }

func (p *registryHTTPReadiness) Probe(context.Context) error { return p.err }

func (e *registryHTTPExecutor) Execute(ctx context.Context, planID, owner string) error {
	e.calls++
	plan, _, err := e.coordinator.Claim(ctx, planID, owner, 10*time.Minute)
	if err != nil {
		return err
	}
	for _, blobPass := range []bool{false, true} {
		items := make([]domain.RegistryCleanupItem, 0)
		for _, item := range plan.Items {
			if item.Disposition != domain.RegistryCleanupDelete || (item.ResourceKind == "blob") != blobPass || item.State == "deleted" {
				continue
			}
			if item.State == "planned" {
				item, err = e.coordinator.AuthorizeItem(ctx, planID, item.Ordinal, owner)
				if err != nil {
					return err
				}
			}
			items = append(items, item)
		}
		if blobPass && e.failBlobSweepOnce {
			e.failBlobSweepOnce = false
			if err = e.coordinator.Finish(ctx, planID, owner, false, "managed registry cleanup execution failed"); err != nil {
				return err
			}
			return errors.New("simulated offline sweep acquisition failure")
		}
		for _, item := range items {
			if err = e.coordinator.RecordItemResult(ctx, planID, item.Ordinal, owner, domain.RegistryCleanupItemResult{State: "deleted", ProviderMessage: "test provider confirmed absence"}); err != nil {
				return err
			}
		}
	}
	return e.coordinator.Finish(ctx, planID, owner, true, "")
}

type registryAPIFixture struct {
	*apiFixture
	clock       time.Time
	executor    *registryHTTPExecutor
	readiness   *registryHTTPReadiness
	project     domain.Project
	application domain.Application
}

func newRegistryAPI(t *testing.T, configured bool) *registryAPIFixture {
	t.Helper()
	central := memory.New()
	clock := time.Date(2026, 8, 9, 7, 0, 0, 0, time.UTC)
	coordinator := registry.NewService(central, registry.WithClock(func() time.Time { return clock }), registry.WithMaxObservationAge(time.Hour))
	executor := &registryHTTPExecutor{coordinator: coordinator}
	var management httpapi.RegistryManagementService
	var readiness httpapi.ReadinessProbe
	probe := &registryHTTPReadiness{}
	if configured {
		management = registry.NewManagement(central, executor,
			registry.WithManagementClock(func() time.Time { return clock }),
			registry.WithManagementObservationAge(time.Hour))
		readiness = probe
	}
	server := httptest.NewServer(httpapi.New(httpapi.Options{
		Store: central, BootstrapToken: "one-time-secret", Version: "test",
		Registry: management, RegistryReadiness: readiness, HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000),
	}))
	jar, _ := cookiejar.New(nil)
	base := &apiFixture{t: t, server: server, client: &http.Client{Jar: jar}, store: central}
	t.Cleanup(server.Close)
	fixture := &registryAPIFixture{apiFixture: base, clock: clock, executor: executor, readiness: probe}
	fixture.bootstrap()
	if configured {
		response := fixture.request(http.MethodPost, "/v1/projects", "registry-project", map[string]string{"name": "Registry project", "slug": "registry-project"})
		fixture.project = decode[domain.Project](t, response)
		if response.StatusCode != http.StatusCreated {
			t.Fatalf("project status=%d", response.StatusCode)
		}
		response = fixture.request(http.MethodPost, "/v1/applications", "registry-application", map[string]string{"projectId": fixture.project.ID, "name": "API", "slug": "api"})
		fixture.application = decode[domain.Application](t, response)
		if response.StatusCode != http.StatusCreated {
			t.Fatalf("application status=%d", response.StatusCode)
		}
	}
	return fixture
}

func registryTargetPayload(name, mode string) map[string]any {
	return map[string]any{
		"name": name, "mode": mode, "endpoint": name + ".registry.test", "repositoryPrefix": "owned",
		"pullCredentialRef": "credentials/registry-pull", "pushCredentialRef": "credentials/registry-push",
		"cacheCredentialRef": "credentials/registry-cache",
	}
}

func TestProjectRegistryPullCredentialCatalogAndServiceSelection(t *testing.T) {
	fixture := newRegistryAPI(t, true)
	response := fixture.request(http.MethodPost, "/v1/registry-targets", "pull-target", registryTargetPayload("private", "external"))
	target := decode[domain.RegistryTarget](t, response)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("target status=%d", response.StatusCode)
	}
	path := "/v1/projects/" + fixture.project.ID + "/registry-pull-credentials"
	response = fixture.request(http.MethodPost, path, "pull-credential", map[string]any{"name": "Production", "registryTargetId": target.ID})
	credential := decode[domain.ProjectRegistryPullCredential](t, response)
	if response.StatusCode != http.StatusCreated || credential.RegistryServer != target.Endpoint || credential.Name != "Production" {
		t.Fatalf("credential=%#v status=%d", credential, response.StatusCode)
	}
	response = fixture.request(http.MethodGet, path, "", nil)
	var catalog struct {
		Items            []domain.ProjectRegistryPullCredential `json:"items"`
		AvailableTargets []map[string]any                       `json:"availableTargets"`
	}
	catalog = decode[struct {
		Items            []domain.ProjectRegistryPullCredential `json:"items"`
		AvailableTargets []map[string]any                       `json:"availableTargets"`
	}](t, response)
	if response.StatusCode != http.StatusOK || len(catalog.Items) != 1 || len(catalog.AvailableTargets) != 1 {
		t.Fatalf("catalog=%#v status=%d", catalog, response.StatusCode)
	}
	body, _ := json.Marshal(catalog)
	for _, forbidden := range []string{"pullCredentialRef", "pushCredentialRef", "cacheCredentialRef", "credentials/registry"} {
		if bytes.Contains(body, []byte(forbidden)) {
			t.Fatalf("catalog leaked %q: %s", forbidden, body)
		}
	}
	selectionPath := "/v1/applications/" + fixture.application.ID + "/registry-pull-selection"
	response = fixture.request(http.MethodPut, selectionPath, "select-pull", map[string]any{"type": "project-credential", "projectCredentialId": credential.ID})
	selection := decode[map[string]any](t, response)
	if response.StatusCode != http.StatusOK || selection["projectCredentialId"] != credential.ID {
		t.Fatalf("selection=%#v status=%d", selection, response.StatusCode)
	}
	response = fixture.request(http.MethodDelete, path+"/"+credential.ID, "delete-selected-pull-credential", nil)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("selected delete status=%d", response.StatusCode)
	}
	response = fixture.request(http.MethodPut, selectionPath, "select-public", map[string]any{"type": "public"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("public status=%d", response.StatusCode)
	}
	response = fixture.request(http.MethodDelete, path+"/"+credential.ID, "delete-pull-credential", nil)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status=%d", response.StatusCode)
	}
	response = fixture.request(http.MethodDelete, path+"/"+credential.ID, "delete-pull-credential", nil)
	if response.StatusCode != http.StatusNoContent || response.Header.Get("Idempotent-Replay") != "true" {
		t.Fatalf("delete replay status=%d replay=%q", response.StatusCode, response.Header.Get("Idempotent-Replay"))
	}
}

func readRegistryBody(t *testing.T, response *http.Response, status int, forbidden ...string) []byte {
	t.Helper()
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != status {
		t.Fatalf("status=%d want=%d body=%s", response.StatusCode, status, body)
	}
	if response.Header.Get("Cache-Control") != "private, no-store" || response.Header.Get("Pragma") != "no-cache" {
		t.Fatalf("registry response was cacheable: %#v", response.Header)
	}
	for _, value := range forbidden {
		if bytes.Contains(body, []byte(value)) {
			t.Fatalf("registry response disclosed forbidden value %q", value)
		}
	}
	return body
}

func (f *registryAPIFixture) createTarget(name, mode, key string) domain.RegistryTarget {
	f.t.Helper()
	response := f.request(http.MethodPost, "/v1/registry-targets", key, registryTargetPayload(name, mode))
	body := readRegistryBody(f.t, response, http.StatusCreated, `"password"`, `"deleteCredential"`, `"credentials"`+`: {`)
	var target domain.RegistryTarget
	if err := json.Unmarshal(body, &target); err != nil {
		f.t.Fatal(err)
	}
	return target
}

func (f *registryAPIFixture) putPolicy(targetID, key string, keep int) {
	f.t.Helper()
	body := map[string]any{"repository": "owned/service"}
	if keep != 0 {
		body["keepLastSuccessful"] = keep
	}
	response := f.request(http.MethodPut, "/v1/applications/"+f.application.ID+"/registry/policies/"+targetID, key, body)
	readRegistryBody(f.t, response, http.StatusOK)
}

func registryHTTPDigest(character string) string { return "sha256:" + strings.Repeat(character, 64) }

func (f *registryAPIFixture) seedManagedInventory(target domain.RegistryTarget) {
	f.t.Helper()
	old := f.clock.Add(-30 * 24 * time.Hour)
	repository := "owned/service"
	catalog := domain.RegistryCatalogSnapshot{
		Observation: domain.RegistryCatalogObservation{
			ID: "22222222-2222-4222-8222-222222222222", RegistryTargetID: target.ID,
			Repository: repository, Revision: 1, Complete: true, ObservedAt: f.clock,
			ManifestCount: 2, BlobCount: 2,
		},
		Manifests: []domain.RegistryManifest{
			{RegistryTargetID: target.ID, Repository: repository, Digest: registryHTTPDigest("a"), Kind: domain.RegistryManifestImage, MediaType: "application/vnd.oci.image.manifest.v1+json", SizeBytes: 10, FirstObservedAt: old},
			{RegistryTargetID: target.ID, Repository: repository, Digest: registryHTTPDigest("b"), Kind: domain.RegistryManifestImage, MediaType: "application/vnd.oci.image.manifest.v1+json", SizeBytes: 10, FirstObservedAt: old},
		},
		Blobs: []domain.RegistryBlob{
			{RegistryTargetID: target.ID, Repository: repository, Digest: registryHTTPDigest("c"), SizeBytes: 100, FirstObservedAt: old},
			{RegistryTargetID: target.ID, Repository: repository, Digest: registryHTTPDigest("d"), SizeBytes: 100, FirstObservedAt: old},
		},
		BlobLinks: []domain.RegistryManifestBlobLink{
			{Repository: repository, ManifestDigest: registryHTTPDigest("a"), BlobDigest: registryHTTPDigest("c")},
			{Repository: repository, ManifestDigest: registryHTTPDigest("b"), BlobDigest: registryHTTPDigest("d")},
		},
	}
	if err := f.store.ReplaceRegistryCatalog(context.Background(), catalog); err != nil {
		f.t.Fatal(err)
	}
	if err := f.store.RecordRegistryInventory(context.Background(), domain.RegistryInventoryObservation{
		RegistryTargetID: target.ID, Revision: "inventory-1", Complete: true,
		Repositories: []string{repository}, ObservedAt: f.clock,
	}); err != nil {
		f.t.Fatal(err)
	}
	for _, authority := range []domain.RegistryAuthority{domain.RegistryAuthorityGitIntent, domain.RegistryAuthorityRuntime, domain.RegistryAuthorityOperations} {
		if err := f.store.ReplaceRegistryProtectionSnapshot(context.Background(), domain.RegistryProtectionSnapshot{Observation: domain.RegistryAuthorityObservation{
			RegistryTargetID: target.ID, ServiceID: f.application.ID, Authority: authority,
			Revision: string(authority) + "-1", Complete: true, ObservedAt: f.clock,
		}}); err != nil {
			f.t.Fatal(err)
		}
	}
	newer, older := f.clock.Add(-24*time.Hour), f.clock.Add(-48*time.Hour)
	for _, release := range []domain.RegistryRelease{
		{ID: "33333333-3333-4333-8333-333333333333", RegistryTargetID: target.ID, ServiceID: f.application.ID, Repository: repository, RootDigest: registryHTTPDigest("a"), CreatedAt: old, SucceededAt: &newer, Availability: domain.RegistryArtifactPresent},
		{ID: "44444444-4444-4444-8444-444444444444", RegistryTargetID: target.ID, ServiceID: f.application.ID, Repository: repository, RootDigest: registryHTTPDigest("b"), CreatedAt: old, SucceededAt: &older, Availability: domain.RegistryArtifactPresent},
	} {
		if _, _, err := f.store.PutRegistryRelease(context.Background(), release); err != nil {
			f.t.Fatal(err)
		}
	}
}

func TestRegistryHTTPManagedLifecycleIsMetadataOnlyBoundedAndReplaySafe(t *testing.T) {
	f := newRegistryAPI(t, true)
	auditsBefore := f.store.AuditCount()
	targetPayload := registryTargetPayload("managed", "managed")
	response := f.request(http.MethodPost, "/v1/registry-targets", "registry-target-create", targetPayload)
	body := readRegistryBody(t, response, http.StatusCreated, `"password"`, `"deleteCredential"`)
	var target domain.RegistryTarget
	if err := json.Unmarshal(body, &target); err != nil {
		t.Fatal(err)
	}
	if target.ID == "" || f.store.AuditCount() != auditsBefore+1 {
		t.Fatalf("target=%#v audits=%d", target, f.store.AuditCount())
	}
	response = f.request(http.MethodPost, "/v1/registry-targets", "registry-target-create", targetPayload)
	replayBody := readRegistryBody(t, response, http.StatusCreated)
	if response.Header.Get("Idempotent-Replay") != "true" || !bytes.Contains(replayBody, []byte(target.ID)) || f.store.AuditCount() != auditsBefore+1 {
		t.Fatalf("target replay header=%q audits=%d", response.Header.Get("Idempotent-Replay"), f.store.AuditCount())
	}
	invalid := registryTargetPayload("invalid", "managed")
	invalid["password"] = "raw registry password"
	response = f.request(http.MethodPost, "/v1/registry-targets", "registry-invalid-secret", invalid)
	readRegistryBody(t, response, http.StatusBadRequest, "raw registry password")
	sharedPull := registryTargetPayload("shared-pull", "managed")
	sharedPull["pullCredentialRef"] = sharedPull["pushCredentialRef"]
	response = f.request(http.MethodPost, "/v1/registry-targets", "registry-shared-pull", sharedPull)
	readRegistryBody(t, response, http.StatusBadRequest)

	response = f.request(http.MethodPut, "/v1/applications/"+f.application.ID+"/registry/policies/"+target.ID, "registry-policy-outside", map[string]any{"repository": "other/service"})
	readRegistryBody(t, response, http.StatusBadRequest)

	response = f.request(http.MethodPut, "/v1/applications/"+f.application.ID+"/registry/policies/"+target.ID, "registry-policy-default", map[string]any{"repository": "owned/service"})
	policyBody := readRegistryBody(t, response, http.StatusOK)
	var defaultPolicy struct {
		KeepLastSuccessful       int   `json:"keepLastSuccessful"`
		MinimumSafetyAgeSeconds  int64 `json:"minimumSafetyAgeSeconds"`
		CacheUnusedExpirySeconds int64 `json:"cacheUnusedExpirySeconds"`
	}
	if err := json.Unmarshal(policyBody, &defaultPolicy); err != nil {
		t.Fatal(err)
	}
	if defaultPolicy.KeepLastSuccessful != 10 || defaultPolicy.MinimumSafetyAgeSeconds != 86400 || defaultPolicy.CacheUnusedExpirySeconds != 604800 {
		t.Fatalf("defaults=%#v", defaultPolicy)
	}
	f.putPolicy(target.ID, "registry-policy-retain-one", 1)
	f.seedManagedInventory(target)

	response = f.request(http.MethodGet, "/v1/applications/"+f.application.ID+"/registry?limit=1", "", nil)
	inventoryBody := readRegistryBody(t, response, http.StatusOK,
		`"manifests"`, `"blobs"`, `"references"`, `"snapshotToken"`, `"authorityToken"`,
		`"pullCredentialRef"`, `"pushCredentialRef"`, `"cacheCredentialRef"`, `credentials/registry-`)
	var inventory struct {
		Items []struct {
			Releases          []json.RawMessage `json:"releases"`
			ReleasesTruncated bool              `json:"releasesTruncated"`
		} `json:"items"`
	}
	if err := json.Unmarshal(inventoryBody, &inventory); err != nil {
		t.Fatal(err)
	}
	if len(inventory.Items) != 1 || len(inventory.Items[0].Releases) != 1 || !inventory.Items[0].ReleasesTruncated {
		t.Fatalf("bounded inventory=%s", inventoryBody)
	}

	response = f.request(http.MethodPost, "/v1/applications/"+f.application.ID+"/registry/cleanup-previews", "registry-preview", map[string]string{"targetId": target.ID})
	previewBody := readRegistryBody(t, response, http.StatusCreated, `"snapshotToken"`, `"authorityToken"`, `"pullCredentialRef"`, `"pushCredentialRef"`)
	var preview struct {
		ID    string `json:"id"`
		State string `json:"state"`
		Items []struct {
			Disposition string `json:"disposition"`
		} `json:"items"`
	}
	if err := json.Unmarshal(previewBody, &preview); err != nil {
		t.Fatal(err)
	}
	if preview.ID == "" || preview.State != "preview" || len(preview.Items) == 0 {
		t.Fatalf("preview=%#v", preview)
	}
	response = f.request(http.MethodGet, "/v1/registry-cleanup-plans/"+preview.ID, "", nil)
	readRegistryBody(t, response, http.StatusOK, `"snapshotToken"`, `"authorityToken"`)

	response = f.request(http.MethodPost, "/v1/registry-cleanup-plans/"+preview.ID+"/executions", "registry-execute-wrong", map[string]string{"confirmation": "wrong-plan-id"})
	readRegistryBody(t, response, http.StatusBadRequest)
	if f.executor.calls != 0 {
		t.Fatalf("wrong confirmation called executor %d times", f.executor.calls)
	}
	auditsBeforeExecute := f.store.AuditCount()
	response = f.request(http.MethodPost, "/v1/registry-cleanup-plans/"+preview.ID+"/executions", "registry-execute", map[string]string{"confirmation": preview.ID})
	executionBody := readRegistryBody(t, response, http.StatusOK)
	if !bytes.Contains(executionBody, []byte(`"state":"succeeded"`)) || f.executor.calls != 1 || f.store.AuditCount() != auditsBeforeExecute+1 {
		t.Fatalf("execution=%s calls=%d audits=%d", executionBody, f.executor.calls, f.store.AuditCount())
	}
	response = f.request(http.MethodPost, "/v1/registry-cleanup-plans/"+preview.ID+"/executions", "registry-execute", map[string]string{"confirmation": preview.ID})
	readRegistryBody(t, response, http.StatusOK)
	if response.Header.Get("Idempotent-Replay") != "true" || f.executor.calls != 1 || f.store.AuditCount() != auditsBeforeExecute+1 {
		t.Fatalf("execute replay header=%q calls=%d audits=%d", response.Header.Get("Idempotent-Replay"), f.executor.calls, f.store.AuditCount())
	}
}

func TestRegistryHTTPRetriesExactFailedOfflineSweepPlan(t *testing.T) {
	f := newRegistryAPI(t, true)
	target := f.createTarget("managed-recovery", "managed", "registry-recovery-target")
	f.putPolicy(target.ID, "registry-recovery-policy", 1)
	f.seedManagedInventory(target)

	response := f.request(http.MethodPost, "/v1/applications/"+f.application.ID+"/registry/cleanup-previews", "registry-recovery-preview", map[string]string{"targetId": target.ID})
	previewBody := readRegistryBody(t, response, http.StatusCreated)
	var preview struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(previewBody, &preview); err != nil || preview.ID == "" {
		t.Fatalf("preview=%s err=%v", previewBody, err)
	}
	f.executor.failBlobSweepOnce = true
	response = f.request(http.MethodPost, "/v1/registry-cleanup-plans/"+preview.ID+"/executions", "registry-recovery-first", map[string]string{"confirmation": preview.ID})
	readRegistryBody(t, response, http.StatusInternalServerError)

	response = f.request(http.MethodGet, "/v1/registry-cleanup-plans/"+preview.ID, "", nil)
	failedBody := readRegistryBody(t, response, http.StatusOK)
	if !bytes.Contains(failedBody, []byte(`"state":"failed"`)) || !bytes.Contains(failedBody, []byte(`"resourceKind":"blob"`)) || !bytes.Contains(failedBody, []byte(`"state":"deleting"`)) {
		t.Fatalf("failed sweep was not durably recoverable: %s", failedBody)
	}

	response = f.request(http.MethodPost, "/v1/registry-cleanup-plans/"+preview.ID+"/executions", "registry-recovery-second", map[string]string{"confirmation": preview.ID})
	recoveredBody := readRegistryBody(t, response, http.StatusOK)
	if !bytes.Contains(recoveredBody, []byte(`"state":"succeeded"`)) || f.executor.calls != 2 {
		t.Fatalf("recovery=%s calls=%d", recoveredBody, f.executor.calls)
	}
}

func TestRegistryHTTPExternalAndIncompleteTargetsFailClosed(t *testing.T) {
	f := newRegistryAPI(t, true)
	external := f.createTarget("external", "external", "registry-external-target")
	f.putPolicy(external.ID, "registry-external-policy", 0)
	auditsBefore := f.store.AuditCount()
	response := f.request(http.MethodPost, "/v1/applications/"+f.application.ID+"/registry/cleanup-previews", "registry-external-preview", map[string]string{"targetId": external.ID})
	body := readRegistryBody(t, response, http.StatusConflict, `"items"`, `"actions"`)
	if !bytes.Contains(body, []byte(`"code":"RegistryExternalLifecycle"`)) || f.executor.calls != 0 || f.store.AuditCount() != auditsBefore {
		t.Fatalf("external response=%s calls=%d audits=%d", body, f.executor.calls, f.store.AuditCount())
	}
	response = f.request(http.MethodGet, "/v1/applications/"+f.application.ID+"/registry", "", nil)
	body = readRegistryBody(t, response, http.StatusOK, `"cleanup"`, `"delete"`, `"garbageCollect"`)
	if !bytes.Contains(body, []byte(`"mode":"external"`)) {
		t.Fatalf("external metadata absent: %s", body)
	}

	managed := f.createTarget("incomplete", "managed", "registry-incomplete-target")
	f.putPolicy(managed.ID, "registry-incomplete-policy", 1)
	response = f.request(http.MethodPost, "/v1/applications/"+f.application.ID+"/registry/cleanup-previews", "registry-incomplete-preview", map[string]string{"targetId": managed.ID})
	body = readRegistryBody(t, response, http.StatusConflict)
	if !bytes.Contains(body, []byte(`"code":"RegistryObservationIncomplete"`)) || f.executor.calls != 0 {
		t.Fatalf("incomplete response=%s calls=%d", body, f.executor.calls)
	}
}

func TestRegistryHTTPFeatureFlagsRequireFreshRuntimeAndUnavailableStatesAreSafe(t *testing.T) {
	f := newRegistryAPI(t, true)
	response := f.request(http.MethodGet, "/v1/capabilities", "", nil)
	var capabilities struct {
		Features map[string]bool `json:"features"`
	}
	capabilities = decode[struct {
		Features map[string]bool `json:"features"`
	}](t, response)
	if !capabilities.Features["registry"] || !capabilities.Features["managedRegistry"] {
		t.Fatalf("healthy exact registry runtime not advertised: %#v", capabilities.Features)
	}
	response = f.request(http.MethodGet, "/v1/applications/"+f.application.ID+"/registry", "", nil)
	body := readRegistryBody(t, response, http.StatusOK)
	if string(body) != "{\"items\":[],\"truncated\":false}\n" {
		t.Fatalf("empty registry response=%s", body)
	}
	managed := f.createTarget("readiness-managed", "managed", "registry-readiness-managed-target")
	f.putPolicy(managed.ID, "registry-readiness-managed-policy", 1)
	f.readiness.err = errors.New("stale managed registry worker")
	response = f.request(http.MethodGet, "/v1/capabilities", "", nil)
	capabilities = decode[struct {
		Features map[string]bool `json:"features"`
	}](t, response)
	if !capabilities.Features["registry"] || capabilities.Features["managedRegistry"] {
		t.Fatalf("registry metadata and managed runtime capabilities were not split: %#v", capabilities.Features)
	}
	response = f.request(http.MethodGet, "/v1/applications/"+f.application.ID+"/registry", "", nil)
	body = readRegistryBody(t, response, http.StatusServiceUnavailable)
	if !bytes.Contains(body, []byte(`"code":"RegistryUnavailable"`)) {
		t.Fatalf("stale runtime response=%s", body)
	}
	response = f.request(http.MethodGet, "/readyz", "", nil)
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("stale optional registry runtime removed API readiness: status=%d body=%s", response.StatusCode, body)
	}
	response = f.request(http.MethodGet, "/v1/registry-targets", "", nil)
	readRegistryBody(t, response, http.StatusOK)

	externalOnly := newRegistryAPI(t, true)
	external := externalOnly.createTarget("readiness-external", "external", "registry-readiness-external-target")
	externalOnly.putPolicy(external.ID, "registry-readiness-external-policy", 0)
	externalOnly.readiness.err = errors.New("managed runtime intentionally absent")
	response = externalOnly.request(http.MethodGet, "/v1/applications/"+externalOnly.application.ID+"/registry", "", nil)
	body = readRegistryBody(t, response, http.StatusOK)
	if !bytes.Contains(body, []byte(`"mode":"external"`)) {
		t.Fatalf("external registry metadata was coupled to managed readiness: %s", body)
	}
	response = externalOnly.request(http.MethodGet, "/v1/capabilities", "", nil)
	capabilities = decode[struct {
		Features map[string]bool `json:"features"`
	}](t, response)
	if !capabilities.Features["registry"] || capabilities.Features["managedRegistry"] {
		t.Fatalf("external-only registry capability mismatch: %#v", capabilities.Features)
	}

	unavailable := newRegistryAPI(t, false)
	response = unavailable.request(http.MethodGet, "/v1/registry-targets", "", nil)
	body = readRegistryBody(t, response, http.StatusServiceUnavailable)
	if !bytes.Contains(body, []byte(`"code":"RegistryUnavailable"`)) {
		t.Fatalf("unavailable response=%s", body)
	}
}

func TestRegistryHTTPMutationsRequireHumanSession(t *testing.T) {
	f := newRegistryAPI(t, true)
	target := f.createTarget("human-only", "external", "registry-human-target")
	response := f.request(http.MethodPost, "/v1/projects/"+f.project.ID+"/service-accounts", "registry-service-account", map[string]any{"name": "registry-bot", "role": "project-admin"})
	var account domain.ServiceAccount
	account = decode[domain.ServiceAccount](t, response)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("service account status=%d", response.StatusCode)
	}
	response = f.request(http.MethodPost, "/v1/service-accounts/"+account.ID+"/tokens", "registry-service-token", map[string]any{
		"name": "registry-token", "scopes": []string{"app.read", "app.edit"}, "expiresAt": time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
	})
	var issued struct {
		Token string `json:"token"`
	}
	issued = decode[struct {
		Token string `json:"token"`
	}](t, response)
	if response.StatusCode != http.StatusCreated || issued.Token == "" {
		t.Fatalf("token status=%d token=%t", response.StatusCode, issued.Token != "")
	}
	payload, _ := json.Marshal(map[string]any{"repository": "owned/service"})
	request, _ := http.NewRequest(http.MethodPut, f.server.URL+"/v1/applications/"+f.application.ID+"/registry/policies/"+target.ID, bytes.NewReader(payload))
	request.Header.Set("Authorization", "Bearer "+issued.Token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "registry-service-policy")
	response, err := (&http.Client{}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body := readRegistryBody(t, response, http.StatusForbidden)
	if !bytes.Contains(body, []byte(`"code":"HumanSessionRequired"`)) {
		t.Fatalf("service-account mutation response=%s", body)
	}
}
