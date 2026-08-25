package main

import (
	"context"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/argo"
	"github.com/kuberploy/kuberploy/internal/imagepull"
)

func TestArgoDesiredStateWorkerIDChangesAcrossSamePodRestart(t *testing.T) {
	firstStartedAt := time.Date(2026, time.August, 25, 1, 2, 3, 4, time.UTC)
	secondStartedAt := firstStartedAt.Add(time.Nanosecond)
	first := argoDesiredStateWorkerID("worker-pod", 1, firstStartedAt)
	second := argoDesiredStateWorkerID("worker-pod", 1, secondStartedAt)
	if first == second {
		t.Fatalf("same-pod restarts must have distinct Argo desired-state worker IDs: %q", first)
	}
}

func TestNewArgoDesiredStateRuntimeIsStrictlyDefaultOff(t *testing.T) {
	runtime, err := newArgoDesiredStateRuntime(t.Context(), "not-a-database-url", "worker", argo.ProductionRuntimeConfig{}, imagepull.RuntimeConfig{}, nil, nil, nil)
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
	if _, err = newArgoDesiredStateRuntime(context.Background(), "not-a-database-url", "worker", config, imagepull.RuntimeConfig{}, nil, nil, nil); err == nil {
		t.Fatal("enabled runtime without projection was accepted")
	}
}

func productionConfigEnvironmentForWorker() map[string]string {
	return map[string]string{
		argo.ProductionEnabledEnv:              "true",
		"KUBERPLOY_GITHUB_APP_ID":              "12345",
		"KUBERPLOY_GITHUB_APP_CLIENT_ID":       "Iv1_client",
		argo.ProductionPlatformBindingIDEnv:    "11111111-1111-4111-8111-111111111111",
		argo.ProductionNamespaceEnv:            "argocd",
		argo.ProductionChartRepositoryEnv:      "oci://ghcr.io/kuberploy/charts",
		argo.ProductionChartVersionEnv:         "1.2.3",
		argo.ProductionChartDigestEnv:          "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		argo.ProductionRendererImageEnv:        "ghcr.io/kuberploy/kuberploy-worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		argo.ProductionPollIntervalSecondsEnv:  "2",
		argo.ProductionCatalogMaxAgeSecondsEnv: "300",
	}
}
