package httpapi_test

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"

	"github.com/kuberploy/kuberploy/internal/builder"
	"github.com/kuberploy/kuberploy/internal/builds"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	centralmemory "github.com/kuberploy/kuberploy/internal/store/memory"
)

func TestBuilderPlatformSettingsAdminLifecycle(t *testing.T) {
	central := centralmemory.New()
	buildStore := builds.NewMemoryStore()
	defaults := builds.DefaultBuilderPlatformSettings(builds.WorkerRuntimeConfig{})
	settings := &builds.BuilderPlatformSettingsService{Store: buildStore, Defaults: defaults}
	server := httptest.NewServer(httpapi.New(httpapi.Options{Store: central, BootstrapToken: "one-time-secret", Version: "test",
		BuilderSettings: settings, HighRiskLimiter: ratelimit.NewMemoryLimiter(100)}))
	jar, _ := cookiejar.New(nil)
	fixture := &apiFixture{t: t, server: server, client: &http.Client{Jar: jar}, store: central}
	t.Cleanup(server.Close)
	fixture.bootstrap()

	response := fixture.request(http.MethodGet, "/v1/platform/builder-settings", "", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("get defaults status=%d", response.StatusCode)
	}
	current := decode[builds.BuilderPlatformSettings](t, response)
	if current.Revision != 0 || current.NodeIsolation || current.MaxConcurrentBuilders != 1 {
		t.Fatalf("defaults=%+v", current)
	}
	input := builds.BuilderPlatformSettingsInput{NodeIsolation: true, MaxConcurrentBuilders: 3,
		CheckoutResources: resources("150m", "192Mi", "2Gi", "1", "768Mi", "4Gi"),
		DinDResources:     resources("750m", "2Gi", "20Gi", "6", "12Gi", "80Gi"),
		AgentResources:    resources("300m", "384Mi", "2Gi", "5", "6Gi", "12Gi")}
	body := struct {
		Revision int64 `json:"revision"`
		builds.BuilderPlatformSettingsInput
	}{Revision: current.Revision, BuilderPlatformSettingsInput: input}
	response = fixture.request(http.MethodPut, "/v1/platform/builder-settings", "builder-settings-1", body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("update status=%d problem=%+v", response.StatusCode, decode[map[string]any](t, response))
	}
	updated := decode[builds.BuilderPlatformSettings](t, response)
	if updated.Revision != 1 || !updated.NodeIsolation || updated.MaxConcurrentBuilders != 3 || updated.DinDResources.CPULimit != "6" {
		t.Fatalf("updated=%+v", updated)
	}
	response = fixture.request(http.MethodPut, "/v1/platform/builder-settings", "builder-settings-1", body)
	if response.StatusCode != http.StatusOK || response.Header.Get("Idempotent-Replay") != "true" {
		t.Fatalf("replay status=%d header=%q", response.StatusCode, response.Header.Get("Idempotent-Replay"))
	}
	response.Body.Close()
	body.MaxConcurrentBuilders = 4
	response = fixture.request(http.MethodPut, "/v1/platform/builder-settings", "builder-settings-stale", body)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("stale update status=%d", response.StatusCode)
	}
	response.Body.Close()
}

func resources(cpuRequest, memoryRequest, storageRequest, cpuLimit, memoryLimit, storageLimit string) builder.ContainerResources {
	return builder.ContainerResources{CPURequest: cpuRequest, MemoryRequest: memoryRequest, EphemeralStorageRequest: storageRequest,
		CPULimit: cpuLimit, MemoryLimit: memoryLimit, EphemeralStorageLimit: storageLimit}
}
