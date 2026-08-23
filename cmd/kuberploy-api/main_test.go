package main

import (
	"testing"

	"github.com/kuberploy/kuberploy/internal/builds"
)

func TestRunRejectsNoncanonicalProviderCIDRBeforeDependencies(t *testing.T) {
	t.Setenv("KUBERPLOY_EXTERNAL_EGRESS_CIDRS", "192.0.2.1/24")
	if err := run(); err == nil {
		t.Fatal("API accepted a noncanonical provider CIDR")
	}
}

func TestMonitoringClientConfigurationFailsClosed(t *testing.T) {
	tests := []struct {
		name      string
		mode      string
		endpoint  string
		token     string
		wantError bool
		wantNil   bool
	}{
		{name: "disabled", mode: "disabled", token: "false", wantNil: true},
		{name: "disabled with dormant endpoint", mode: "disabled", endpoint: "https://prometheus.example.test", token: "false", wantNil: true},
		{name: "disabled with dormant credential setting", mode: "disabled", endpoint: "https://prometheus.example.test", token: "true", wantNil: true},
		{name: "unknown mode", mode: "auto", token: "false", wantError: true},
		{name: "missing endpoint", mode: "managed", token: "false", wantError: true},
		{name: "managed cluster service outside Kubernetes", mode: "managed", endpoint: "http://prometheus-operated.kuberploy-monitoring.svc:9090", token: "false", wantError: true},
		{name: "managed alternate cluster service", mode: "managed", endpoint: "http://other.kuberploy-monitoring.svc:9090", token: "false", wantError: true},
		{name: "managed bearer token", mode: "managed", endpoint: "http://prometheus-operated.kuberploy-monitoring.svc:9090", token: "true", wantError: true},
		{name: "managed arbitrary HTTP", mode: "managed", endpoint: "http://prometheus.example.test", token: "false", wantError: true},
		{name: "existing HTTPS", mode: "existing", endpoint: "https://prometheus.example.test", token: "true"},
		{name: "existing HTTP", mode: "existing", endpoint: "http://prometheus.example.test", token: "false", wantError: true},
		{name: "invalid token setting", mode: "existing", endpoint: "https://prometheus.example.test", token: "yes", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("KUBERPLOY_PROMETHEUS_URL", test.endpoint)
			t.Setenv("KUBERPLOY_PROMETHEUS_BEARER_TOKEN_ENABLED", test.token)
			service, err := monitoringClient(test.mode, "0.1.0-rc.321")
			if (err != nil) != test.wantError {
				t.Fatalf("service=%T error=%v", service, err)
			}
			if !test.wantError && (service == nil) != test.wantNil {
				t.Fatalf("service=%T wantNil=%v", service, test.wantNil)
			}
		})
	}
}

func TestSourceBuildLogServiceConfigurationFailsClosed(t *testing.T) {
	t.Setenv("KUBERPLOY_BUILD_LOGS_ENABLED", "false")
	service, readiness, err := sourceBuildLogService(nil, nil)
	if err != nil || service != nil || readiness != nil {
		t.Fatalf("disabled service=%T readiness=%T err=%v", service, readiness, err)
	}
	t.Setenv("KUBERPLOY_BUILD_LOGS_ENABLED", "yes")
	if _, _, err = sourceBuildLogService(nil, nil); err == nil {
		t.Fatal("non-boolean build-log setting was accepted")
	}
	t.Setenv("KUBERPLOY_BUILD_LOGS_ENABLED", "true")
	if _, _, err = sourceBuildLogService(nil, nil); err == nil {
		t.Fatal("build logs started without the source-build runtime")
	}
}

func TestSourceBuildLogAttemptCatalogUsesExecutionStore(t *testing.T) {
	store := &builds.PostgreSQLStore{}
	source := &sourceBuildAPI{store: store}
	if got := sourceBuildAttemptCatalog(source); got != store {
		t.Fatalf("build log catalog=%T, want full execution store", got)
	}
}

func TestValkeyCredentialPreservesExactSecretBytesAndFallsBack(t *testing.T) {
	t.Setenv("KUBERPLOY_TEST_VALKEY_ROLE_PASSWORD", "  exact password bytes  ")
	if got := valkeyCredential("KUBERPLOY_TEST_VALKEY_ROLE_PASSWORD", "fallback"); got != "  exact password bytes  " {
		t.Fatal("role password bytes were normalized")
	}
	t.Setenv("KUBERPLOY_TEST_VALKEY_ROLE_PASSWORD", "")
	if got := valkeyCredential("KUBERPLOY_TEST_VALKEY_ROLE_PASSWORD", "fallback"); got != "fallback" {
		t.Fatal("empty role password did not use the explicit compatibility credential")
	}
}
