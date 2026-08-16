package config

import (
	"fmt"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"
)

const (
	ProviderEgressCIDRsEnv        = "KUBERPLOY_EXTERNAL_EGRESS_CIDRS"
	KubeAPIServerCIDRsEnv         = "KUBERPLOY_KUBE_API_SERVER_CIDRS"
	BuilderSourceEgressCIDRsEnv   = "KUBERPLOY_BUILDER_SOURCE_EGRESS_CIDRS"
	BuilderRegistryEgressCIDRsEnv = "KUBERPLOY_BUILDER_REGISTRY_EGRESS_CIDRS"
	NetworkPolicyEnabledEnv       = "KUBERPLOY_NETWORK_POLICY_ENABLED"
	maximumProviderCIDRs          = 128
	maximumKubeAPICIDRs           = 32
)

// ValidateControlPlaneEgressFromEnvironment closes the gap between Helm's
// syntactic validation and Kubernetes' permissive CIDR parser. Both control
// plane binaries call it before opening a database or becoming ready.
func ValidateControlPlaneEgressFromEnvironment() error {
	// Helm intentionally treats CIDR lists as dormant configuration while the
	// NetworkPolicy feature is disabled. Keep startup behavior consistent with
	// that contract so stale optional values cannot crash an otherwise usable
	// control plane. An empty flag preserves backwards compatibility for older
	// manifests that predate this environment variable.
	if rawEnabled := os.Getenv(NetworkPolicyEnabledEnv); rawEnabled != "" {
		enabled, err := strconv.ParseBool(rawEnabled)
		if err != nil {
			return fmt.Errorf("%s must be a boolean: %w", NetworkPolicyEnabledEnv, err)
		}
		if !enabled {
			return nil
		}
	}
	providerCIDRs, err := parseProviderEgressCIDRs(ProviderEgressCIDRsEnv, os.Getenv(ProviderEgressCIDRsEnv))
	if err != nil {
		return err
	}
	builderCIDRs, err := parseProviderEgressCIDRs(BuilderSourceEgressCIDRsEnv, os.Getenv(BuilderSourceEgressCIDRsEnv))
	if err != nil {
		return err
	}
	registryCIDRs, err := parseExactHostCIDRs(BuilderRegistryEgressCIDRsEnv, os.Getenv(BuilderRegistryEgressCIDRsEnv))
	if err != nil {
		return err
	}
	kubeAPICIDRs, err := ParseKubeAPIServerCIDRs(os.Getenv(KubeAPIServerCIDRsEnv))
	if err != nil {
		return err
	}
	providerAndRegistryCIDRs := append(append(providerCIDRs, builderCIDRs...), registryCIDRs...)
	for _, provider := range providerAndRegistryCIDRs {
		providerPrefix := netip.MustParsePrefix(provider)
		for _, api := range kubeAPICIDRs {
			apiPrefix := netip.MustParsePrefix(api)
			if prefixesOverlap(providerPrefix, apiPrefix) {
				return fmt.Errorf("provider egress CIDR %q overlaps Kubernetes API CIDR %q", provider, api)
			}
		}
	}
	return nil
}

func parseExactHostCIDRs(environmentName, raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	values := strings.Split(raw, ",")
	if len(values) > 16 {
		return nil, fmt.Errorf("%s accepts at most 16 CIDRs", environmentName)
	}
	seen := make(map[string]struct{}, len(values))
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		prefix, err := netip.ParsePrefix(value)
		if err != nil || value == "" || strings.TrimSpace(value) != value || prefix.Masked().String() != value || prefix.Bits() != prefix.Addr().BitLen() {
			return nil, fmt.Errorf("%s contains a noncanonical host CIDR %q", environmentName, value)
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		normalized = append(normalized, value)
	}
	sort.Strings(normalized)
	return normalized, nil
}

func ValidateProviderEgressCIDRs(raw string) error {
	_, err := ParseProviderEgressCIDRs(raw)
	return err
}

func ParseProviderEgressCIDRs(raw string) ([]string, error) {
	return parseProviderEgressCIDRs(ProviderEgressCIDRsEnv, raw)
}

func parseProviderEgressCIDRs(environmentName, raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	values := strings.Split(raw, ",")
	if len(values) > maximumProviderCIDRs {
		return nil, fmt.Errorf("%s accepts at most %d CIDRs", environmentName, maximumProviderCIDRs)
	}
	seen := make(map[string]struct{}, len(values))
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || strings.TrimSpace(value) != value {
			return nil, fmt.Errorf("%s must contain nonempty canonical CIDRs", environmentName)
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		prefix, err := netip.ParsePrefix(value)
		if err != nil || prefix.Masked().String() != value {
			return nil, fmt.Errorf("%s contains a noncanonical CIDR %q", environmentName, value)
		}
		minimumBits := 16
		if prefix.Addr().Is4() {
			minimumBits = 8
		}
		if prefix.Bits() < minimumBits {
			return nil, fmt.Errorf("%s contains an excessively broad CIDR %q", environmentName, value)
		}
		seen[value] = struct{}{}
		normalized = append(normalized, value)
	}
	sort.Strings(normalized)
	return normalized, nil
}

// ParseKubeAPIServerCIDRs returns a canonical, sorted API exclusion list for
// NetworkPolicy rules assembled outside the control-plane chart.
func ParseKubeAPIServerCIDRs(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	values := strings.Split(raw, ",")
	if len(values) > maximumKubeAPICIDRs {
		return nil, fmt.Errorf("%s accepts at most %d CIDRs", KubeAPIServerCIDRsEnv, maximumKubeAPICIDRs)
	}
	seen := make(map[string]struct{}, len(values))
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		prefix, err := netip.ParsePrefix(value)
		if err != nil || value == "" || strings.TrimSpace(value) != value || prefix.Masked().String() != value || prefix.Bits() == 0 {
			return nil, fmt.Errorf("%s contains an invalid or noncanonical CIDR %q", KubeAPIServerCIDRsEnv, value)
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		normalized = append(normalized, value)
	}
	sort.Strings(normalized)
	return normalized, nil
}

func prefixesOverlap(left, right netip.Prefix) bool {
	return left.Addr().BitLen() == right.Addr().BitLen() &&
		(left.Contains(right.Addr()) || right.Contains(left.Addr()))
}
