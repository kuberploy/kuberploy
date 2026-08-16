package registry

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func validRuntimeEnvironment() map[string]string {
	return map[string]string{
		ManagedRegistryRuntimeEnabledEnv: "true", ManagedRegistryTargetIDEnv: "11111111-1111-4111-8111-111111111111",
		ManagedRegistryTargetNameEnv:       "Managed registry",
		ManagedRegistryEndpointEnv:         "http://kuberploy-registry.kuberploy-registry.svc.cluster.local:5000",
		ManagedRegistryRepositoryPrefixEnv: "kuberploy", ManagedRegistryLifecycleCredentialRefEnv: "operator/managed-registry",
		ManagedRegistryPullCredentialRefEnv: "registry-pull", ManagedRegistryPushCredentialRefEnv: "registry-push",
		ManagedRegistryCacheCredentialRefEnv: "registry-cache",
		ManagedRegistryAllowPlainHTTPEnv:     "true", ManagedRegistryNamespaceEnv: "kuberploy-registry",
		ManagedRegistryDeploymentEnv: "kuberploy-registry", ManagedRegistryPVCEnv: "kuberploy-registry",
		ManagedRegistryConfigMapEnv: "kuberploy-registry-config-abc123", ManagedRegistryHelperServiceAccountEnv: "kuberploy-registry-maintenance",
		ManagedRegistryHelperImageEnv:        "ghcr.io/kuberploy/kuberploy-worker@sha256:" + repeatHex("a", 64),
		ManagedRegistryObservationSecondsEnv: "300",
	}
}

func TestRuntimeConfigDefaultOffAndIgnoresDormantConfiguration(t *testing.T) {
	lookup := func(string) (string, bool) { return "", false }
	config, err := RuntimeConfigFromLookup(lookup)
	if err != nil || config.Enabled {
		t.Fatalf("default config = %#v, %v", config, err)
	}
	for _, values := range []map[string]string{
		{ManagedRegistryRuntimeEnabledEnv: "false", ManagedRegistryEndpointEnv: "https://registry.example.test"},
		{ManagedRegistryRuntimeEnabledEnv: "", ManagedRegistryTargetIDEnv: "stale-value"},
	} {
		config, parseErr := RuntimeConfigFromLookup(func(name string) (string, bool) { value, ok := values[name]; return value, ok })
		if parseErr != nil || config != (RuntimeConfig{}) {
			t.Fatalf("dormant config was not ignored: %#v config=%#v err=%v", values, config, parseErr)
		}
	}
}

func TestRuntimeConfigEnabledIsExactAndImmutable(t *testing.T) {
	values := validRuntimeEnvironment()
	lookup := func(name string) (string, bool) { value, ok := values[name]; return value, ok }
	config, err := RuntimeConfigFromLookup(lookup)
	if err != nil {
		t.Fatal(err)
	}
	if !config.Enabled || config.ObservationInterval != 5*time.Minute || !config.AllowPlainHTTP {
		t.Fatalf("config = %#v", config)
	}
	for name, mutation := range map[string]string{
		ManagedRegistryTargetIDEnv: "../../other", ManagedRegistryEndpointEnv: "http://registry.invalid/path",
		ManagedRegistryTargetNameEnv:       " managed ",
		ManagedRegistryRepositoryPrefixEnv: "../other", ManagedRegistryLifecycleCredentialRefEnv: " secret ",
		ManagedRegistryPullCredentialRefEnv: "registry-push",
		ManagedRegistryNamespaceEnv:         "other/namespace", ManagedRegistryHelperImageEnv: "ghcr.io/kuberploy/worker:latest",
		ManagedRegistryObservationSecondsEnv: "014", ManagedRegistryAllowPlainHTTPEnv: "TRUE",
	} {
		bad := validRuntimeEnvironment()
		bad[name] = mutation
		_, err = RuntimeConfigFromLookup(func(key string) (string, bool) { value, ok := bad[key]; return value, ok })
		if !errors.Is(err, errRegistryRuntimeConfig) {
			t.Fatalf("%s mutation accepted: %v", name, err)
		}
	}
}

func TestProjectedCredentialSourceIsTargetAndFixedPathBound(t *testing.T) {
	targetID := "11111111-1111-4111-8111-111111111111"
	source, err := NewProjectedCredentialSource(targetID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = source.Authorization(t.Context(), "22222222-2222-4222-8222-222222222222"); !errors.Is(err, ErrDistributionScopeMismatch) {
		t.Fatalf("cross-target error = %v", err)
	}
	dir := t.TempDir()
	usernamePath, passwordPath := filepath.Join(dir, "username"), filepath.Join(dir, "password")
	if err = os.WriteFile(usernamePath, []byte("registry-controller"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(passwordPath, []byte("not-logged-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	source.usernamePath, source.passwordPath = usernamePath, passwordPath
	// Tests cannot replace production fixed paths: the reader rejects any
	// caller-controlled path even when it contains valid-looking bytes.
	if _, err = source.Authorization(t.Context(), targetID); !errors.Is(err, ErrDistributionCredentialUnavailable) {
		t.Fatalf("mutable path accepted: %v", err)
	}
}

func repeatHex(value string, count int) string {
	out := ""
	for range count {
		out += value
	}
	return out
}
