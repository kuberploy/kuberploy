package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kuberploy/kuberploy/internal/argo"
	"github.com/kuberploy/kuberploy/internal/environmentfoundation"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

const testPlatformBindingID = "11111111-1111-4111-8111-111111111111"

func platformBindingRuntime(t *testing.T) gitprojection.RuntimeConfig {
	t.Helper()
	values := map[string]string{
		gitprojection.ProjectionEnabledEnv:       "true",
		gitprojection.ProjectionCacheMaxBytesEnv: "536870912",
		gitprojection.ProjectionPollSecondsEnv:   "300",
		gitprojection.ProjectionWebhookWakeEnv:   "true",
		gitprojection.ProjectionGitAuthModeEnv:   "github-app",
		gitprojection.ProjectionGitHubAppIDEnv:   "12345",
		gitprojection.ProjectionGitHubClientEnv:  "Iv1_KuberployClient",
		gitprojection.ProjectionChartVersionEnv:  "0.1.0-rc.425",
		gitprojection.ProjectionPolicyVersionEnv: "runtime-policy-v1",
	}
	runtime, err := gitprojection.RuntimeConfigFromLookup(func(name string) (string, bool) {
		value, found := values[name]
		return value, found
	})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func TestPlatformGitBindingConfigDefaultsOffAndUsesOnlyValidatedRuntimeIdentity(t *testing.T) {
	runtime := platformBindingRuntime(t)
	absent, err := platformGitBindingConfigFromLookup(func(string) (string, bool) { return "", false }, runtime)
	if err != nil || absent.Enabled || absent.BindingID != "" || absent.GitHubAppID != 0 {
		t.Fatalf("absent config=%#v err=%v", absent, err)
	}

	lookup := func(name string) (string, bool) {
		if name == platformBindingIDEnv {
			return testPlatformBindingID, true
		}
		return "", false
	}
	enabled, err := platformGitBindingConfigFromLookup(lookup, runtime)
	if err != nil || !enabled.Enabled || enabled.BindingID != testPlatformBindingID || enabled.GitHubAppID != runtime.GitHub.AppID || enabled.Validate() != nil {
		t.Fatalf("enabled config=%#v err=%v", enabled, err)
	}

	disabled, err := platformGitBindingConfigFromLookup(lookup, gitprojection.RuntimeConfig{})
	if err != nil || disabled.Enabled || disabled.BindingID != "" || disabled.GitHubAppID != 0 {
		t.Fatalf("disabled runtime config=%#v err=%v", disabled, err)
	}

	tampered := runtime
	tampered.GitHub.AppID = 0
	if _, err = platformGitBindingConfigFromLookup(lookup, tampered); err == nil {
		t.Fatal("invalid enabled runtime supplied platform GitHub App identity")
	}
}

func TestPlatformGitBindingConfigRejectsEveryPresentNoncanonicalBinding(t *testing.T) {
	for name, value := range map[string]string{
		"empty":       "",
		"whitespace":  " " + testPlatformBindingID,
		"uppercase":   "01900000-0000-7000-8000-00000000000A",
		"nil version": "01900000-0000-0000-8000-000000000001",
		"bad variant": "01900000-0000-7000-7000-000000000001",
		"not uuid":    "cluster-production",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := platformGitBindingConfigFromLookup(func(string) (string, bool) { return value, true }, gitprojection.RuntimeConfig{}); err == nil {
				t.Fatalf("unsafe binding ID %q accepted", value)
			}
		})
	}
}

func TestPlatformGitBindingIdentityIsSharedAcrossBootstrapAndRuntimes(t *testing.T) {
	runtime := platformBindingRuntime(t)
	stageOne := map[string]string{
		platformBindingIDEnv:      testPlatformBindingID,
		argo.ProductionEnabledEnv: "false",
	}
	lookupStageOne := func(name string) (string, bool) { value, found := stageOne[name]; return value, found }
	argoOff, err := argo.ProductionRuntimeConfigFromLookup(lookupStageOne)
	if err != nil || !reflect.DeepEqual(argoOff, argo.ProductionRuntimeConfig{}) {
		t.Fatalf("stage-one Argo config=%+v err=%v", argoOff, err)
	}
	bootstrap, err := platformGitBindingConfigFromLookup(lookupStageOne, runtime)
	if err != nil || !bootstrap.Enabled || bootstrap.BindingID != testPlatformBindingID || bootstrap.GitHubAppID != runtime.GitHub.AppID {
		t.Fatalf("stage-one platform binding config=%+v err=%v", bootstrap, err)
	}

	createdBindingID := testPlatformBindingID
	stageTwo := map[string]string{
		platformBindingIDEnv:                                   createdBindingID,
		argo.ProductionEnabledEnv:                              "true",
		argo.ProductionPlatformBindingIDEnv:                    createdBindingID,
		argo.ProductionNamespaceEnv:                            "argocd",
		argo.ProductionChartRepositoryEnv:                      "oci://ghcr.io/kuberploy/charts",
		argo.ProductionChartVersionEnv:                         "1.2.3",
		argo.ProductionChartDigestEnv:                          "sha256:" + strings.Repeat("c", 64),
		argo.ProductionRendererImageEnv:                        "ghcr.io/kuberploy/worker@sha256:" + strings.Repeat("d", 64),
		argo.ProductionPollIntervalSecondsEnv:                  "2",
		argo.ProductionCatalogMaxAgeSecondsEnv:                 "300",
		"KUBERPLOY_GITHUB_APP_ID":                              "12345",
		"KUBERPLOY_GITHUB_APP_CLIENT_ID":                       "Iv1_KuberployClient",
		environmentfoundation.RuntimeEnabledEnv:                "true",
		environmentfoundation.RuntimePlatformBindingIDEnv:      createdBindingID,
		environmentfoundation.RuntimePSAVersionEnv:             "v1.31",
		environmentfoundation.RuntimePollSecondsEnv:            "2",
		environmentfoundation.RuntimeControlPlaneNamespaceEnv:  "kuberploy-system",
		environmentfoundation.RuntimeObserverServiceAccountEnv: "kuberploy-api",
	}
	lookupStageTwo := func(name string) (string, bool) { value, found := stageTwo[name]; return value, found }
	argoOn, err := argo.ProductionRuntimeConfigFromLookup(lookupStageTwo)
	if err != nil || !argoOn.Enabled || argoOn.DesiredState.PlatformBindingID != createdBindingID {
		t.Fatalf("stage-two Argo config=%+v err=%v", argoOn, err)
	}
	foundation, err := environmentfoundation.RuntimeConfigFromLookup(lookupStageTwo)
	if err != nil || !foundation.Enabled || foundation.PlatformBindingID != createdBindingID {
		t.Fatalf("stage-two foundation config=%+v err=%v", foundation, err)
	}
}
