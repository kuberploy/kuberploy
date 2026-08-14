package httpapi_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/appconfigpreview"
	"github.com/kuberploy/kuberploy/internal/certificates"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/imageresolution"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	base "github.com/kuberploy/kuberploy/internal/store"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

type configBundleWire struct {
	ETag               string `json:"etag"`
	Freshness          string `json:"freshness"`
	TargetHeadRevision string `json:"targetHeadRevision"`
	IndexedRevision    string `json:"indexedRevision"`
	Documents          []struct {
		DocumentID       string         `json:"documentId"`
		RawYAML          string         `json:"rawYaml"`
		Document         map[string]any `json:"document"`
		EditablePointers []string       `json:"editablePointers"`
		LockedPointers   []string       `json:"lockedPointers"`
	} `json:"documents"`
}

type validationWire struct {
	Valid       bool                   `json:"valid"`
	Diagnostics []appconfig.Diagnostic `json:"diagnostics"`
}

type previewWire struct {
	PreviewToken         string                     `json:"previewToken"`
	GitDiff              string                     `json:"gitDiff"`
	RenderedDiff         string                     `json:"renderedDiff"`
	RenderIdentityDigest string                     `json:"renderIdentityDigest"`
	SemanticChanges      []appconfig.SemanticChange `json:"semanticChanges"`
	Warnings             []string                   `json:"warnings"`
}

type configPreviewFailureStore struct {
	base.Store
	err error
}

func (s *configPreviewFailureStore) AuthorizedImageSourcesForActor(ctx context.Context, actor, applicationID, environmentID string) ([]imageresolution.AuthorizedSource, error) {
	return s.Store.(imageresolution.Catalog).AuthorizedImageSourcesForActor(ctx, actor, applicationID, environmentID)
}

func (s *configPreviewFailureStore) CreateDeploymentConfigPreview(context.Context, string, domain.CreateConfigPreview, *gitprojection.WritePlan, ...*base.AppConfigReferencePlan) error {
	return s.err
}

func createConfigDeployment(t *testing.T, f *apiFixture) (domain.Operation, string) {
	t.Helper()
	f.bootstrap()
	r := f.request("POST", "/v1/projects", "config-project", map[string]string{"name": "Config"})
	project := decode[domain.Project](t, r)
	r = f.request("POST", "/v1/environments", "config-environment", map[string]string{"projectId": project.ID, "name": "Development"})
	environment := decode[domain.Environment](t, r)
	r = f.request("POST", "/v1/applications", "config-application", map[string]string{"projectId": project.ID, "name": "API"})
	application := decode[domain.Application](t, r)
	image := "registry.example/api@sha256:" + strings.Repeat("a", 64)
	r = f.request("POST", "/v1/deployments", "config-deployment", map[string]any{"environmentId": environment.ID, "applicationId": application.ID, "image": image, "runtime": domain.DefaultWorkloadRuntime(8080, nil)})
	operation := decode[domain.Operation](t, r)
	if r.StatusCode != http.StatusAccepted {
		t.Fatalf("create deployment status=%d", r.StatusCode)
	}
	return operation, image
}

func configRequest(t *testing.T, f *apiFixture, method, path, key string, body any, headers map[string]string) *http.Response {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(method, f.server.URL+path, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if f.csrf != "" {
		req.Header.Set("X-CSRF-Token", f.csrf)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	response, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestDeploymentConfigPreviewSaveCASAndExactSnapshot(t *testing.T) {
	f := newAPI(t)
	created, image := createConfigDeployment(t, f)
	acceptedSnapshot, err := f.store.GetDeploymentForOperation(t.Context(), created.ID)
	if err != nil || len(acceptedSnapshot.ConfigRaw) == 0 {
		t.Fatalf("create did not snapshot exact AppConfig before GET: %q err=%v", acceptedSnapshot.ConfigRaw, err)
	}
	path := "/v1/deployments/" + created.TargetID + "/config"
	r := f.request("GET", path, "", nil)
	bundle := decode[configBundleWire](t, r)
	if r.StatusCode != 200 || r.Header.Get("ETag") != bundle.ETag || !strings.HasPrefix(bundle.ETag, `"cfg-sha256-`) || len(bundle.Documents) != 1 {
		t.Fatalf("config bundle status=%d etag=%q body=%#v", r.StatusCode, r.Header.Get("ETag"), bundle)
	}
	if bundle.Freshness != "projection-only" || bundle.TargetHeadRevision != "" || bundle.IndexedRevision != "" {
		t.Fatalf("bundle overstated Git freshness: %#v", bundle)
	}
	if len(bundle.Documents[0].EditablePointers) == 0 || len(bundle.Documents[0].LockedPointers) == 0 || strings.Contains(bundle.Documents[0].RawYAML, "plaintextSecret") {
		t.Fatalf("incomplete config contract: %#v", bundle.Documents[0])
	}
	if bundle.Documents[0].RawYAML != string(acceptedSnapshot.ConfigRaw) {
		t.Fatalf("GET config did not return the exact accepted operation snapshot")
	}
	lockedChange := appconfig.Change{Mode: "jsonPatch", Patch: []appconfig.PatchOperation{{Op: "replace", Path: "/spec/delivery/release/digest", Value: "sha256:" + strings.Repeat("b", 64)}}}
	r = configRequest(t, f, "POST", path+"/validate", "", lockedChange, nil)
	validation := decode[validationWire](t, r)
	if r.StatusCode != 200 || validation.Valid || !hasConfigDiagnostic(validation.Diagnostics, "LockedField", "/spec/delivery/release/digest") {
		t.Fatalf("locked validation status=%d body=%#v", r.StatusCode, validation)
	}
	change := appconfig.Change{Mode: "jsonPatch", Patch: []appconfig.PatchOperation{
		{Op: "replace", Path: "/spec/runtime/replicas", Value: 2},
		{Op: "add", Path: "/spec/routes", Value: []any{map[string]any{"id": "public", "host": "api.example.test", "path": "/", "port": "http", "ingressClassName": "traefik", "tls": map[string]any{"mode": "httpOnly"}}}},
	}}
	r = configRequest(t, f, "POST", path+"/preview", "", change, map[string]string{"If-Match": bundle.ETag})
	preview := decode[previewWire](t, r)
	if r.StatusCode != 200 || preview.PreviewToken == "" || !strings.Contains(preview.GitDiff, "api.example.test") || len(preview.SemanticChanges) != 2 {
		t.Fatalf("preview status=%d body=%#v", r.StatusCode, preview)
	}
	if r.Header.Get("Cache-Control") != "no-store" || preview.RenderedDiff == "" || preview.RenderIdentityDigest == "" {
		t.Fatalf("preview bearer token/cache or rendered-diff status is ambiguous: cache=%q warnings=%#v", r.Header.Get("Cache-Control"), preview.Warnings)
	}
	tampered := appconfig.Change{Mode: "jsonPatch", Patch: []appconfig.PatchOperation{{Op: "replace", Path: "/spec/runtime/replicas", Value: 3}}}
	r = configRequest(t, f, "PUT", path, "tampered-save", tampered, map[string]string{"If-Match": bundle.ETag, "Preview-Token": preview.PreviewToken})
	problem := decode[httpapi.Problem](t, r)
	if r.StatusCode != 409 || problem.Code != "PreviewInvalid" {
		t.Fatalf("tampered token accepted: status=%d %#v", r.StatusCode, problem)
	}
	r = configRequest(t, f, "PUT", path, "config-save", change, map[string]string{"If-Match": bundle.ETag, "Preview-Token": preview.PreviewToken})
	saved := decode[domain.Operation](t, r)
	if r.StatusCode != http.StatusAccepted || saved.ID == created.ID || saved.Generation != created.Generation+1 {
		t.Fatalf("save status=%d op=%#v", r.StatusCode, saved)
	}
	snapshot, err := f.store.GetDeploymentForOperation(t.Context(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Image != image || !strings.Contains(string(snapshot.ConfigRaw), `"replicas": 2`) || !strings.Contains(string(snapshot.ConfigRaw), "api.example.test") || !strings.Contains(string(snapshot.ConfigRaw), strings.Repeat("a", 64)) {
		t.Fatalf("operation did not snapshot exact candidate while preserving release image: image=%q config=%s", snapshot.Image, snapshot.ConfigRaw)
	}
	if _, accessErr := f.store.GetDeploymentConfigForActor(t.Context(), "99999999-9999-4999-8999-999999999999", saved.TargetID); !errors.Is(accessErr, base.ErrNotFound) {
		t.Fatalf("deployment config leaked across actor scope: %v", accessErr)
	}
	r = f.request("GET", path, "", nil)
	updatedBundle := decode[configBundleWire](t, r)
	if updatedBundle.ETag == bundle.ETag || !strings.Contains(updatedBundle.Documents[0].RawYAML, "api.example.test") {
		t.Fatalf("accepted projection did not advance exact ETag/config: %#v", updatedBundle)
	}
	r = f.request("GET", "/v1/me", "", nil)
	actor := decode[domain.User](t, r)
	expiredChange := appconfig.Change{Mode: "jsonPatch", Patch: []appconfig.PatchOperation{{Op: "replace", Path: "/spec/runtime/replicas", Value: 3}}}
	expiredCandidate := appconfig.Apply([]byte(updatedBundle.Documents[0].RawYAML), expiredChange)
	rawExpiredToken := bytes.Repeat([]byte{0x42}, 32)
	_, renderIdentityDigest, _ := (staticAppConfigRenderer{}).Identity()
	expiredTokenHash, hashErr := appconfigpreview.PreviewTokenHash(rawExpiredToken, renderIdentityDigest)
	if hashErr != nil {
		t.Fatal(hashErr)
	}
	if err = f.store.CreateDeploymentConfigPreview(t.Context(), actor.ID, domain.CreateConfigPreview{DeploymentID: saved.TargetID, BaseETag: updatedBundle.ETag, TokenHash: expiredTokenHash[:], CandidateHash: expiredCandidate.Hash, ExpiresAt: time.Now().Add(-time.Minute)}, nil); err != nil {
		t.Fatal(err)
	}
	r = configRequest(t, f, "PUT", path, "expired-preview", expiredChange, map[string]string{"If-Match": updatedBundle.ETag, "Preview-Token": base64.RawURLEncoding.EncodeToString(rawExpiredToken)})
	problem = decode[httpapi.Problem](t, r)
	if r.StatusCode != 409 || problem.Code != "PreviewExpired" {
		t.Fatalf("expired preview accepted: status=%d %#v", r.StatusCode, problem)
	}
	// A lost successful response can be replayed after both the ETag advanced
	// and the one-time token was consumed.
	r = configRequest(t, f, "PUT", path, "config-save", change, map[string]string{"If-Match": bundle.ETag, "Preview-Token": preview.PreviewToken})
	replayed := decode[domain.Operation](t, r)
	if r.StatusCode != http.StatusAccepted || replayed.ID != saved.ID || r.Header.Get("Idempotent-Replay") != "true" {
		t.Fatalf("save replay was not recoverable: status=%d op=%#v", r.StatusCode, replayed)
	}
	// A different idempotency key cannot replay the consumed/stale preview.
	r = configRequest(t, f, "PUT", path, "token-reuse", change, map[string]string{"If-Match": bundle.ETag, "Preview-Token": preview.PreviewToken})
	problem = decode[httpapi.Problem](t, r)
	if r.StatusCode != 412 || problem.Code != "PreconditionFailed" {
		t.Fatalf("stale token reuse status=%d %#v", r.StatusCode, problem)
	}
}

func TestDeploymentConfigPreviewFailsClosedWithoutPinnedRenderer(t *testing.T) {
	st := memory.New()
	server := httptest.NewServer(httpapi.New(httpapi.Options{Store: st, BootstrapToken: "one-time-secret", Version: "test", HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000)}))
	jar, _ := cookiejar.New(nil)
	fixture := &apiFixture{t: t, server: server, client: &http.Client{Jar: jar}, store: st}
	t.Cleanup(server.Close)
	created, _ := createConfigDeployment(t, fixture)
	path := "/v1/deployments/" + created.TargetID + "/config"
	response := fixture.request(http.MethodGet, path, "", nil)
	bundle := decode[configBundleWire](t, response)
	change := appconfig.Change{Mode: "jsonPatch", Patch: []appconfig.PatchOperation{{Op: "replace", Path: "/spec/runtime/replicas", Value: 2}}}
	response = configRequest(t, fixture, http.MethodPost, path+"/preview", "", change, map[string]string{"If-Match": bundle.ETag})
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusServiceUnavailable || problem.Code != "AppConfigRendererUnavailable" {
		t.Fatalf("unconfigured renderer issued preview authority: status=%d problem=%#v", response.StatusCode, problem)
	}
}

func TestDeploymentConfigPreviewMapsTransactionalCertificateOutageTo503(t *testing.T) {
	underlying := memory.New()
	wrapped := &configPreviewFailureStore{Store: underlying, err: certificates.ErrObservationUnavailable}
	server := httptest.NewServer(httpapi.New(httpapi.Options{Store: wrapped, BootstrapToken: "one-time-secret", Version: "test", AppConfigRenderedPreviews: staticAppConfigRenderer{}, HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000)}))
	jar, _ := cookiejar.New(nil)
	fixture := &apiFixture{t: t, server: server, client: &http.Client{Jar: jar}, store: underlying}
	t.Cleanup(server.Close)
	created, _ := createConfigDeployment(t, fixture)
	path := "/v1/deployments/" + created.TargetID + "/config"
	response := fixture.request(http.MethodGet, path, "", nil)
	bundle := decode[configBundleWire](t, response)
	change := appconfig.Change{Mode: "jsonPatch", Patch: []appconfig.PatchOperation{{Op: "replace", Path: "/spec/runtime/replicas", Value: 2}}}
	response = configRequest(t, fixture, http.MethodPost, path+"/preview", "", change, map[string]string{"If-Match": bundle.ETag})
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusServiceUnavailable || problem.Code != "CertificateReferenceRuntimeUnavailable" {
		t.Fatalf("transactional certificate outage status=%d problem=%#v", response.StatusCode, problem)
	}
}

func TestDeploymentConfigPreviewRequiresStrongETag(t *testing.T) {
	f := newAPI(t)
	created, _ := createConfigDeployment(t, f)
	change := appconfig.Change{Mode: "jsonPatch", Patch: []appconfig.PatchOperation{{Op: "replace", Path: "/spec/runtime/replicas", Value: 2}}}
	r := configRequest(t, f, "POST", "/v1/deployments/"+created.TargetID+"/config/preview", "", change, nil)
	problem := decode[httpapi.Problem](t, r)
	if r.StatusCode != 428 || problem.Code != "PreconditionRequired" {
		t.Fatalf("missing precondition status=%d %#v", r.StatusCode, problem)
	}
	r = configRequest(t, f, "POST", "/v1/deployments/"+created.TargetID+"/config/preview", "", change, map[string]string{"If-Match": `"cfg-sha256-not-64-hex"`})
	problem = decode[httpapi.Problem](t, r)
	if r.StatusCode != 400 || problem.Code != "InvalidPrecondition" {
		t.Fatalf("malformed strong ETag status=%d %#v", r.StatusCode, problem)
	}
}

func hasConfigDiagnostic(diagnostics []appconfig.Diagnostic, code, pointer string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code && diagnostic.Pointer == pointer {
			return true
		}
	}
	return false
}
