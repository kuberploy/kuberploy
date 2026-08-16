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

func TestArgoRuntimeNamespaceSeparationAndRollingCompatibility(t *testing.T) {
	lookup := func(values map[string]string) func(string) (string, bool) {
		return func(name string) (string, bool) { value, found := values[name]; return value, found }
	}

	observationOnly := map[string]string{
		ObservationEnabledEnv: "true", ObservationNamespaceEnv: "observed-argocd", ObservationPollSecondsEnv: "30",
		ProductionEnabledEnv: "false",
	}
	observation, err := ObservationRuntimeConfigFromLookup(lookup(observationOnly))
	if err != nil || observation.Namespace != "observed-argocd" {
		t.Fatalf("observation-only=%+v err=%v", observation, err)
	}
	if production, productionErr := ProductionRuntimeConfigFromLookup(lookup(observationOnly)); productionErr != nil || production.Enabled {
		t.Fatalf("observation-only production=%+v err=%v", production, productionErr)
	}

	both := productionConfigEnvironment()
	both[ObservationEnabledEnv] = "true"
	both[ObservationNamespaceEnv] = "argocd"
	both[ObservationPollSecondsEnv] = "30"
	observation, err = ObservationRuntimeConfigFromLookup(lookup(both))
	production, productionErr := ProductionRuntimeConfigFromLookup(lookup(both))
	if err != nil || productionErr != nil || observation.Namespace != "argocd" ||
		production.DesiredState.ArgoNamespace != "argocd" {
		t.Fatalf("split observation=%+v production=%+v errors=%v/%v", observation, production, err, productionErr)
	}
	both[ObservationNamespaceEnv] = "other-argocd"
	observation, err = ObservationRuntimeConfigFromLookup(lookup(both))
	production, productionErr = ProductionRuntimeConfigFromLookup(lookup(both))
	if err != nil || productionErr != nil || observation.Namespace == production.DesiredState.ArgoNamespace {
		t.Fatalf("differing namespace parser fixture is not distinct: observation=%+v production=%+v errors=%v/%v", observation, production, err, productionErr)
	}

	legacy := map[string]string{
		ObservationEnabledEnv: "true", ProductionEnabledEnv: "false", ProductionNamespaceEnv: "legacy-argocd",
		ObservationPollSecondsEnv: "30",
	}
	observation, err = ObservationRuntimeConfigFromLookup(lookup(legacy))
	production, productionErr = ProductionRuntimeConfigFromLookup(lookup(legacy))
	if err != nil || productionErr != nil || observation.Namespace != "legacy-argocd" || production.Enabled {
		t.Fatalf("legacy observation=%+v production=%+v errors=%v/%v", observation, production, err, productionErr)
	}
	legacyDisabled := map[string]string{
		ObservationEnabledEnv: "false", ObservationPollSecondsEnv: "45", ProductionEnabledEnv: "false",
	}
	observation, err = ObservationRuntimeConfigFromLookup(lookup(legacyDisabled))
	production, productionErr = ProductionRuntimeConfigFromLookup(lookup(legacyDisabled))
	if err != nil || productionErr != nil || observation.Enabled || production.Enabled {
		t.Fatalf("legacy disabled observation=%+v production=%+v errors=%v/%v", observation, production, err, productionErr)
	}
	legacyBoth := productionConfigEnvironment()
	legacyBoth[ObservationEnabledEnv] = "true"
	legacyBoth[ObservationPollSecondsEnv] = "30"
	observation, err = ObservationRuntimeConfigFromLookup(lookup(legacyBoth))
	production, productionErr = ProductionRuntimeConfigFromLookup(lookup(legacyBoth))
	if err != nil || productionErr != nil || observation.Namespace != production.DesiredState.ArgoNamespace {
		t.Fatalf("legacy combined observation=%+v production=%+v errors=%v/%v", observation, production, err, productionErr)
	}

	for name, testCase := range map[string]struct {
		values            map[string]string
		observationReject bool
		productionReject  bool
	}{
		"dormant generic": {
			values: map[string]string{ProductionEnabledEnv: "false", ProductionNamespaceEnv: "argocd"},
		},
		"dormant dedicated": {
			values: map[string]string{ObservationEnabledEnv: "false", ObservationNamespaceEnv: "argocd"}, observationReject: true,
		},
		"poll without enabled": {
			values: map[string]string{ObservationPollSecondsEnv: "30"}, observationReject: true,
		},
		"empty enabled with poll": {
			values: map[string]string{ObservationEnabledEnv: "", ObservationPollSecondsEnv: "30"}, observationReject: true,
		},
		"dedicated empty cannot fall back": {
			values: map[string]string{ObservationEnabledEnv: "true", ObservationNamespaceEnv: "", ProductionNamespaceEnv: "legacy-argocd", ObservationPollSecondsEnv: "30"}, observationReject: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, observationErr := ObservationRuntimeConfigFromLookup(lookup(testCase.values)); (observationErr != nil) != testCase.observationReject {
				t.Fatal("observation parser accepted invalid namespace state")
			}
			if _, productionErr := ProductionRuntimeConfigFromLookup(lookup(testCase.values)); (productionErr != nil) != testCase.productionReject {
				t.Fatal("production parser accepted dormant generic namespace")
			}
		})
	}
}
