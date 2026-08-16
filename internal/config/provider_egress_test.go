package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestValidateProviderEgressCIDRs(t *testing.T) {
	t.Parallel()
	valid := []string{
		"",
		"192.0.0.0/20",
		"192.0.0.0/20,2001:db8::/29",
		"2001:db8::/29,192.0.0.0/20",
		"10.0.0.0/8,2001::/16",
		"192.0.0.0/20,192.0.0.0/20",
	}
	for _, value := range valid {
		value := value
		t.Run("valid_"+value, func(t *testing.T) {
			t.Parallel()
			if err := ValidateProviderEgressCIDRs(value); err != nil {
				t.Fatalf("ValidateProviderEgressCIDRs(%q): %v", value, err)
			}
		})
	}

	tooMany := make([]string, 129)
	for index := range tooMany {
		tooMany[index] = fmt.Sprintf("10.%d.0.0/16", index)
	}
	invalid := []string{
		"0.0.0.0/0",
		"10.0.0.0/7",
		"2001:db8::/15",
		"192.0.2.1/24",
		"2001:0db8::/29",
		"2001:db8::1/29",
		"192.0.0.0/20, 2001:db8::/29",
		"192.0.0.0/20,",
		strings.Join(tooMany, ","),
	}
	for _, value := range invalid {
		value := value
		t.Run("invalid_"+value, func(t *testing.T) {
			t.Parallel()
			if err := ValidateProviderEgressCIDRs(value); err == nil {
				t.Fatalf("ValidateProviderEgressCIDRs(%q) unexpectedly succeeded", value)
			}
		})
	}
}

func TestValidateControlPlaneEgressFromEnvironment(t *testing.T) {
	t.Setenv(NetworkPolicyEnabledEnv, "true")
	t.Setenv(ProviderEgressCIDRsEnv, "192.0.2.1/24")
	if err := ValidateControlPlaneEgressFromEnvironment(); err == nil {
		t.Fatal("noncanonical environment CIDR unexpectedly succeeded")
	}
}

func TestValidateControlPlaneEgressIgnoresDormantValuesWhenDisabled(t *testing.T) {
	t.Setenv(NetworkPolicyEnabledEnv, "false")
	t.Setenv(ProviderEgressCIDRsEnv, "10.0.0.0/7")
	t.Setenv(BuilderSourceEgressCIDRsEnv, "10.0.0.0/8")
	t.Setenv(BuilderRegistryEgressCIDRsEnv, "10.0.0.1/32")
	t.Setenv(KubeAPIServerCIDRsEnv, "10.0.0.1/32")
	if err := ValidateControlPlaneEgressFromEnvironment(); err != nil {
		t.Fatalf("disabled NetworkPolicy rejected dormant values: %v", err)
	}
}

func TestValidateControlPlaneEgressRejectsMalformedFeatureFlag(t *testing.T) {
	t.Setenv(NetworkPolicyEnabledEnv, "sometimes")
	if err := ValidateControlPlaneEgressFromEnvironment(); err == nil {
		t.Fatal("malformed NetworkPolicy feature flag unexpectedly succeeded")
	}
}

func TestValidateControlPlaneEgressRejectsKubeAPIOverlap(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		builder  string
		registry string
		api      string
	}{
		{name: "provider contains API", provider: "10.43.0.0/16", api: "10.43.0.1/32"},
		{name: "API contains provider", provider: "10.43.0.1/32", api: "10.43.0.0/16"},
		{name: "builder contains API", builder: "10.43.0.0/16", api: "10.43.0.1/32"},
		{name: "API contains builder", builder: "10.43.0.1/32", api: "10.43.0.0/16"},
		{name: "IPv6 provider contains API", provider: "2001:db8::/29", api: "2001:db8::1/128"},
		{name: "registry equals API", registry: "10.43.0.1/32", api: "10.43.0.1/32"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(ProviderEgressCIDRsEnv, test.provider)
			t.Setenv(BuilderSourceEgressCIDRsEnv, test.builder)
			t.Setenv(BuilderRegistryEgressCIDRsEnv, test.registry)
			t.Setenv(KubeAPIServerCIDRsEnv, test.api)
			if err := ValidateControlPlaneEgressFromEnvironment(); err == nil {
				t.Fatal("overlapping provider and Kubernetes API CIDRs unexpectedly succeeded")
			}
		})
	}
}

func TestValidateControlPlaneEgressAllowsDisjointAndEmpty(t *testing.T) {
	t.Setenv(ProviderEgressCIDRsEnv, "2001:db8::/29,192.0.0.0/20")
	t.Setenv(BuilderSourceEgressCIDRsEnv, "192.0.32.0/20")
	t.Setenv(KubeAPIServerCIDRsEnv, "10.43.0.1/32")
	if err := ValidateControlPlaneEgressFromEnvironment(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateControlPlaneEgressAllowsDefaultPublicRoutes(t *testing.T) {
	t.Setenv(ProviderEgressCIDRsEnv, "")
	t.Setenv(BuilderSourceEgressCIDRsEnv, "")
	t.Setenv(BuilderRegistryEgressCIDRsEnv, "")
	t.Setenv(KubeAPIServerCIDRsEnv, "10.43.0.1/32,fd00::1/128")
	if err := ValidateControlPlaneEgressFromEnvironment(); err != nil {
		t.Fatal(err)
	}
}

func TestParseProviderEgressCIDRsNormalizesOrder(t *testing.T) {
	got, err := ParseProviderEgressCIDRs("2001:db8::/29,192.0.0.0/20,2001:db8::/29")
	if err != nil {
		t.Fatal(err)
	}
	if want := "192.0.0.0/20,2001:db8::/29"; strings.Join(got, ",") != want {
		t.Fatalf("normalized CIDRs = %q, want %q", strings.Join(got, ","), want)
	}
}
