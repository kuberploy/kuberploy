package imagepull

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func runtimeLookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

func validRuntimeEnvironment(t *testing.T) map[string]string {
	t.Helper()
	encoded, err := json.Marshal(testRuntimeConfig().Profiles)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		RuntimeEnabledEnv:    "true",
		RuntimeNamespacesEnv: "tenant-a-dev,tenant-a-prod",
		RuntimeProfilesEnv:   string(encoded),
	}
}

func TestRuntimeConfigFromEnvironmentIsDefaultOffAndExact(t *testing.T) {
	config, err := RuntimeConfigFromLookup(runtimeLookup(nil))
	if err != nil || config.Enabled {
		t.Fatalf("absent config=%#v err=%v", config, err)
	}
	config, err = RuntimeConfigFromLookup(runtimeLookup(map[string]string{RuntimeEnabledEnv: "false"}))
	if err != nil || config.Enabled {
		t.Fatalf("disabled config=%#v err=%v", config, err)
	}
	for _, values := range []map[string]string{
		{RuntimeEnabledEnv: "yes"},
	} {
		if _, err = RuntimeConfigFromLookup(runtimeLookup(values)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("dormant/ambiguous config accepted: %#v err=%v", values, err)
		}
	}
	for _, values := range []map[string]string{
		{RuntimeProfilesEnv: "[]"},
		{RuntimeEnabledEnv: "false", RuntimeNamespacesEnv: "tenant-a-dev"},
	} {
		if config, parseErr := RuntimeConfigFromLookup(runtimeLookup(values)); parseErr != nil || config.Enabled {
			t.Fatalf("dormant config was not ignored: %#v err=%v", values, parseErr)
		}
	}

	config, err = RuntimeConfigFromLookup(runtimeLookup(validRuntimeEnvironment(t)))
	if err != nil || !config.Enabled || len(config.Profiles) != 1 || config.Profiles[0].SourceSecretRef != "registry-pull-main" {
		t.Fatalf("enabled config=%#v err=%v", config, err)
	}
}

func TestRuntimeProfileJSONRejectsDuplicatesUnknownFieldsAndTrailingData(t *testing.T) {
	valid := validRuntimeEnvironment(t)
	profile := strings.TrimSuffix(strings.TrimPrefix(valid[RuntimeProfilesEnv], "["), "]")
	for name, raw := range map[string]string{
		"duplicate": "[{\"name\":\"managed-main\",\"name\":\"other\",\"targetId\":\"" + testTargetID + "\",\"registryServer\":\"registry.example.test\",\"credentialRef\":\"runtime-pull/main\",\"revision\":3,\"sourceSecretRef\":\"registry-pull-main\",\"sourceSecretKey\":\".dockerconfigjson\"}]",
		"unknown":   "[" + strings.TrimSuffix(profile, "}") + ",\"password\":\"do-not-accept\"}]",
		"trailing":  valid[RuntimeProfilesEnv] + " {}",
		"object":    profile,
		"empty":     "[]",
	} {
		t.Run(name, func(t *testing.T) {
			values := validRuntimeEnvironment(t)
			values[RuntimeProfilesEnv] = raw
			if _, err := RuntimeConfigFromLookup(runtimeLookup(values)); !errors.Is(err, ErrInvalid) {
				t.Fatalf("unsafe profile JSON accepted: %s err=%v", raw, err)
			}
		})
	}
}

func TestRuntimeConfigNormalizesListsAndRejectsInvalidDurationForms(t *testing.T) {
	values := validRuntimeEnvironment(t)
	values[RuntimeNamespacesEnv] = "tenant-a-prod,tenant-a-dev,tenant-a-prod"
	config, err := RuntimeConfigFromLookup(runtimeLookup(values))
	if err != nil || strings.Join(config.Namespaces, ",") != "tenant-a-dev,tenant-a-prod" {
		t.Fatalf("namespace normalization failed: %#v err=%v", config.Namespaces, err)
	}
	for name, mutate := range map[string]func(map[string]string){
		"leading duration zero": func(values map[string]string) { values[RuntimePollSecondsEnv] = "030" },
		"short poll":            func(values map[string]string) { values[RuntimePollSecondsEnv] = "1" },
		"newline":               func(values map[string]string) { values[RuntimeNamespacesEnv] += "\n" },
	} {
		t.Run(name, func(t *testing.T) {
			values := validRuntimeEnvironment(t)
			mutate(values)
			if _, err := RuntimeConfigFromLookup(runtimeLookup(values)); !errors.Is(err, ErrInvalid) {
				t.Fatalf("non-canonical config accepted: %#v err=%v", values, err)
			}
		})
	}
}
