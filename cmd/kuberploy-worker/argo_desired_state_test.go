package main

import (
	"context"
	"testing"

	"github.com/kuberploy/kuberploy/internal/argo"
	"github.com/kuberploy/kuberploy/internal/imagepull"
)

func TestNewArgoDesiredStateRuntimeIsStrictlyDefaultOff(t *testing.T) {
	runtime, err := newArgoDesiredStateRuntime(t.Context(), "not-a-database-url", "worker", argo.ProductionRuntimeConfig{}, imagepull.RuntimeConfig{}, nil, nil)
	if err != nil || runtime != nil {
		t.Fatalf("runtime=%v err=%v", runtime, err)
	}
}

func TestArgoDesiredStateRuntimeRejectsMissingProjectionBeforeExternalIO(t *testing.T) {
	values := productionConfigEnvironmentForWorker()
	config, err := argo.ProductionRuntimeConfigFromLookup(func(name string) (string, bool) { value, found := values[name]; return value, found })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = newArgoDesiredStateRuntime(context.Background(), "not-a-database-url", "worker", config, imagepull.RuntimeConfig{}, nil, nil); err == nil {
		t.Fatal("enabled runtime without projection was accepted")
	}
}

func productionConfigEnvironmentForWorker() map[string]string {
	return map[string]string{
		argo.ProductionEnabledEnv:              "true",
		"KUBERPLOY_GITHUB_APP_ID":              "12345",
		"KUBERPLOY_GITHUB_APP_CLIENT_ID":       "Iv1_client",
		argo.ProductionPlatformBindingIDEnv:    "11111111-1111-4111-8111-111111111111",
		argo.ProductionClusterIDEnv:            "22222222-2222-4222-8222-222222222222",
		argo.ProductionNamespaceEnv:            "argocd",
		argo.ProductionChartRepositoryEnv:      "oci://ghcr.io/kuberploy/charts",
		argo.ProductionChartVersionEnv:         "1.2.3",
		argo.ProductionChartDigestEnv:          "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		argo.ProductionRendererImageEnv:        "ghcr.io/kuberploy/kuberploy-worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		argo.ProductionPollIntervalSecondsEnv:  "2",
		argo.ProductionCatalogMaxAgeSecondsEnv: "300",
	}
}
