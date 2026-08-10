package argo

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func productionConfigEnvironment() map[string]string {
	return map[string]string{
		ProductionEnabledEnv:              "true",
		productionGitHubAppIDEnv:          "12345",
		productionGitHubClientIDEnv:       "Iv1_client",
		ProductionPlatformBindingIDEnv:    "11111111-1111-4111-8111-111111111111",
		ProductionClusterIDEnv:            "22222222-2222-4222-8222-222222222222",
		ProductionNamespaceEnv:            "argocd",
		ProductionChartRepositoryEnv:      "oci://ghcr.io/kuberploy/charts",
		ProductionChartVersionEnv:         "1.2.3",
		ProductionChartDigestEnv:          "sha256:" + strings.Repeat("a", 64),
		ProductionRendererImageEnv:        "ghcr.io/kuberploy/kuberploy-worker@sha256:" + strings.Repeat("b", 64),
		ProductionPollIntervalSecondsEnv:  "2",
		ProductionCatalogMaxAgeSecondsEnv: "300",
	}
}

func lookupProduction(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) { value, found := values[name]; return value, found }
}

func TestProductionRuntimeConfigFromLookupBuildsExactDefaultOffIdentity(t *testing.T) {
	config, err := ProductionRuntimeConfigFromLookup(lookupProduction(productionConfigEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	if !config.Enabled || config.Validate() != nil || config.DesiredState.RootApplicationName != PlatformRootApplicationName ||
		config.DesiredState.RepositorySecretName != "kuberploy-repo-11111111111141118111111111111111" ||
		config.DesiredState.Runtime.ChartName != RuntimeChartName || config.DesiredState.DigestEnforcement != ChartDigestNativeOCI ||
		config.PollInterval != 2*time.Second || config.MaximumCatalogAge != 5*time.Minute {
		t.Fatalf("unexpected config: %+v", config)
	}
	if len(config.GitHub.MaximumTokenPermissions) != 3 || config.GitHub.MaximumTokenPermissions["administration"] != "read" ||
		config.GitHub.MaximumTokenPermissions["contents"] != "read" {
		t.Fatalf("unexpected provider cap: %+v", config.GitHub.MaximumTokenPermissions)
	}
	identity, err := DesiredStateRuntimeIdentityForConfig(config.DesiredState)
	if err != nil || identity.Validate() != nil {
		t.Fatalf("identity error=%v value=%+v", err, identity)
	}
}

func TestProductionRuntimeConfigFromLookupRejectsDormantAndMalformedSettings(t *testing.T) {
	if config, err := ProductionRuntimeConfigFromLookup(lookupProduction(map[string]string{})); err != nil || !reflect.DeepEqual(config, ProductionRuntimeConfig{}) {
		t.Fatalf("empty default-off config=%+v err=%v", config, err)
	}
	for name, mutate := range map[string]func(map[string]string){
		"dormant": func(values map[string]string) {
			values[ProductionEnabledEnv] = "false"
			values[ProductionNamespaceEnv] = "argocd"
		},
		"boolean":                func(values map[string]string) { values[ProductionEnabledEnv] = "TRUE" },
		"platform binding":       func(values map[string]string) { values[ProductionPlatformBindingIDEnv] = "not-a-uuid" },
		"cluster":                func(values map[string]string) { values[ProductionClusterIDEnv] = "not-a-uuid" },
		"floating chart":         func(values map[string]string) { values[ProductionChartDigestEnv] = "latest" },
		"floating renderer":      func(values map[string]string) { values[ProductionRendererImageEnv] = "ghcr.io/kuberploy/worker:latest" },
		"admin write impossible": func(values map[string]string) { values[productionGitHubAppIDEnv] = "01" },
		"poll":                   func(values map[string]string) { values[ProductionPollIntervalSecondsEnv] = "0" },
		"catalog":                func(values map[string]string) { values[ProductionCatalogMaxAgeSecondsEnv] = "3601" },
	} {
		t.Run(name, func(t *testing.T) {
			values := productionConfigEnvironment()
			mutate(values)
			if _, err := ProductionRuntimeConfigFromLookup(lookupProduction(values)); err == nil {
				t.Fatal("unsafe config accepted")
			}
		})
	}
}

func TestProductionRuntimeConfigDisabledAllowsSharedBootstrapClusterIdentity(t *testing.T) {
	config, err := ProductionRuntimeConfigFromLookup(lookupProduction(map[string]string{
		ProductionEnabledEnv:   "false",
		ProductionClusterIDEnv: "22222222-2222-4222-8222-222222222222",
	}))
	if err != nil || !reflect.DeepEqual(config, ProductionRuntimeConfig{}) {
		t.Fatalf("bootstrap cluster leaked into disabled Argo config=%+v err=%v", config, err)
	}
}
