package builds

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/kuberploy/kuberploy/internal/builder"
	"github.com/kuberploy/kuberploy/internal/githubapp"
)

var (
	digestImageRE   = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)
	buildKitImageRE = regexp.MustCompile(`^[^\s@]+:v0\.32\.2$`)
)

const (
	GitHubBuildsEnabledEnv        = "KUBERPLOY_GITHUB_BUILDS_ENABLED"
	GitHubAppIDEnv                = "KUBERPLOY_GITHUB_APP_ID"
	GitHubAppClientIDEnv          = "KUBERPLOY_GITHUB_APP_CLIENT_ID"
	BuilderNamespaceEnv           = "KUBERPLOY_BUILDER_NAMESPACE"
	BuilderPodServiceAccountEnv   = "KUBERPLOY_BUILDER_POD_SERVICE_ACCOUNT"
	BuilderAgentImageEnv          = "KUBERPLOY_BUILDER_AGENT_IMAGE"
	BuilderBuildKitImageEnv       = "KUBERPLOY_BUILDER_BUILDKIT_IMAGE"
	BuilderSourceEgressCIDRsEnv   = "KUBERPLOY_BUILDER_SOURCE_EGRESS_CIDRS"
	BuilderRegistryEgressCIDRsEnv = "KUBERPLOY_BUILDER_REGISTRY_EGRESS_CIDRS"
)

// WorkerRuntimeConfig is deliberately disabled unless the operator supplies
// one exact opt-in flag and every non-secret identity setting. Credential
// bytes remain in the fixed projected Secret root owned by githubapp.
type WorkerRuntimeConfig struct {
	Enabled                  bool
	BuilderNamespace         string
	BuilderPodServiceAccount string
	BuilderAgentImage        string
	BuildKitImage            string
	SourceEgressCIDRs        []string
	RegistryEgressCIDRs      []string
	GitHub                   githubapp.Config
}

// RuntimeDigest binds the API and worker to the exact immutable execution
// boundary. It intentionally contains no Secret bytes or Secret references.
func (c WorkerRuntimeConfig) RuntimeDigest() (string, error) {
	if err := c.validateEnabled(); err != nil {
		return "", err
	}
	view := struct {
		Version                  int
		WorkerContract           string
		GitHubAppID              int64
		GitHubClientID           string
		BuilderNamespace         string
		BuilderPodServiceAccount string
		BuilderAgentImage        string
		BuildKitImage            string
		SourceEgressCIDRs        []string
		RegistryEgressCIDRs      []string
		Execution                ExecutionSettings
	}{1, "deliveries+builds+release-projection", c.GitHub.AppID, c.GitHub.ClientID, c.BuilderNamespace, c.BuilderPodServiceAccount, c.BuilderAgentImage, c.BuildKitImage,
		slices.Clone(c.SourceEgressCIDRs), slices.Clone(c.RegistryEgressCIDRs), c.executionTemplate()}
	encoded, err := json.Marshal(view)
	if err != nil {
		return "", ErrInvalid
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// ExecutionSettings returns the operator-owned portion copied into an
// immutable build definition. registryPort is derived from the verified
// registry target, never from the public build-definition request.
func (c WorkerRuntimeConfig) ExecutionSettings(registryPort int) (ExecutionSettings, error) {
	if err := c.validateEnabled(); err != nil || registryPort < 1 || registryPort > 65535 {
		return ExecutionSettings{}, ErrInvalid
	}
	settings := c.executionTemplate()
	portsByCIDR := make(map[string]map[int]struct{}, len(c.SourceEgressCIDRs)+len(c.RegistryEgressCIDRs))
	for _, cidr := range c.SourceEgressCIDRs {
		portsByCIDR[cidr] = map[int]struct{}{443: {}}
	}
	for _, cidr := range c.RegistryEgressCIDRs {
		ports := portsByCIDR[cidr]
		if ports == nil {
			ports = map[int]struct{}{}
			portsByCIDR[cidr] = ports
		}
		ports[registryPort] = struct{}{}
	}
	settings.Egress = make([]builder.EgressEndpoint, 0, len(portsByCIDR))
	for cidr, portSet := range portsByCIDR {
		ports := make([]int, 0, len(portSet))
		for port := range portSet {
			ports = append(ports, port)
		}
		slices.Sort(ports)
		settings.Egress = append(settings.Egress, builder.EgressEndpoint{CIDR: cidr, Ports: ports})
	}
	slices.SortFunc(settings.Egress, func(a, b builder.EgressEndpoint) int { return strings.Compare(a.CIDR, b.CIDR) })
	return settings, nil
}

func (c WorkerRuntimeConfig) executionTemplate() ExecutionSettings {
	return ExecutionSettings{
		Namespace: c.BuilderNamespace, PodServiceAccount: c.BuilderPodServiceAccount, BuilderAgentImage: c.BuilderAgentImage, BuildKitImage: c.BuildKitImage,
		NodeSelector:       map[string]string{"kuberploy.io/node-class": "dind-builder"},
		Toleration:         builder.TaintToleration{Key: "kuberploy.io/dind-builder", Value: "true", Effect: "NoSchedule"},
		CheckoutResources:  builder.ContainerResources{CPURequest: "100m", MemoryRequest: "128Mi", EphemeralStorageRequest: "1Gi", CPULimit: "1", MemoryLimit: "512Mi", EphemeralStorageLimit: "2Gi"},
		DinDResources:      builder.ContainerResources{CPURequest: "500m", MemoryRequest: "1Gi", EphemeralStorageRequest: "10Gi", CPULimit: "4", MemoryLimit: "8Gi", EphemeralStorageLimit: "50Gi"},
		AgentResources:     builder.ContainerResources{CPURequest: "250m", MemoryRequest: "256Mi", EphemeralStorageRequest: "1Gi", CPULimit: "4", MemoryLimit: "4Gi", EphemeralStorageLimit: "10Gi"},
		WorkspaceSizeLimit: "20Gi", SocketSizeLimit: "64Mi", ResultSizeLimit: "1Mi", DockerDataSizeLimit: "50Gi",
		ActiveDeadlineSeconds: 7860, TTLSecondsAfterFinished: 3600,
	}
}

func (c WorkerRuntimeConfig) validateEnabled() error {
	if !c.Enabled || c.GitHub.Validate() != nil || !kubeNameRE.MatchString(c.BuilderNamespace) ||
		!kubeNameRE.MatchString(c.BuilderPodServiceAccount) || !validRuntimeAgentImage(c.BuilderAgentImage) || !validRuntimeBuildKitImage(c.BuildKitImage) ||
		len(c.SourceEgressCIDRs) == 0 || len(c.RegistryEgressCIDRs) == 0 {
		return ErrInvalid
	}
	if validateHostCIDRs(c.SourceEgressCIDRs) != nil || validateHostCIDRs(c.RegistryEgressCIDRs) != nil {
		return ErrInvalid
	}
	return nil
}

func WorkerRuntimeConfigFromEnvironment() (WorkerRuntimeConfig, error) {
	return WorkerRuntimeConfigFromLookup(os.LookupEnv)
}

func WorkerRuntimeConfigFromLookup(lookup func(string) (string, bool)) (WorkerRuntimeConfig, error) {
	if lookup == nil {
		return WorkerRuntimeConfig{}, ErrInvalid
	}
	enabledValue, present := lookup(GitHubBuildsEnabledEnv)
	if !present || enabledValue == "" || enabledValue == "false" {
		return WorkerRuntimeConfig{}, nil
	}
	if enabledValue != "true" {
		return WorkerRuntimeConfig{}, errors.New("KUBERPLOY_GITHUB_BUILDS_ENABLED must be exactly true or false")
	}

	appIDValue := lookupExact(lookup, GitHubAppIDEnv)
	clientID := lookupExact(lookup, GitHubAppClientIDEnv)
	namespace := lookupExact(lookup, BuilderNamespaceEnv)
	podServiceAccount := lookupExact(lookup, BuilderPodServiceAccountEnv)
	agentImage := lookupExact(lookup, BuilderAgentImageEnv)
	buildKitImage := lookupExact(lookup, BuilderBuildKitImageEnv)
	sourceCIDRs, sourceErr := parseHostCIDRs(lookupExact(lookup, BuilderSourceEgressCIDRsEnv))
	registryCIDRs, registryErr := parseHostCIDRs(lookupExact(lookup, BuilderRegistryEgressCIDRsEnv))
	if appIDValue == "" || clientID == "" || namespace == "" || !kubeNameRE.MatchString(namespace) ||
		!kubeNameRE.MatchString(podServiceAccount) || !validRuntimeAgentImage(agentImage) || !validRuntimeBuildKitImage(buildKitImage) || sourceErr != nil || registryErr != nil {
		return WorkerRuntimeConfig{}, errors.New("enabled GitHub builds require exact provider identity, immutable builder runtime, and host egress CIDRs")
	}
	appID, err := strconv.ParseInt(appIDValue, 10, 64)
	if err != nil || appID < 1 || strconv.FormatInt(appID, 10) != appIDValue {
		return WorkerRuntimeConfig{}, errors.New("KUBERPLOY_GITHUB_APP_ID must be a canonical positive integer")
	}

	githubConfig, err := githubapp.NewProjectedConfig(appID, clientID, githubapp.Permissions{
		"metadata": githubapp.PermissionRead,
		"contents": githubapp.PermissionRead,
	})
	if err != nil {
		return WorkerRuntimeConfig{}, err
	}
	config := WorkerRuntimeConfig{Enabled: true, BuilderNamespace: namespace, BuilderPodServiceAccount: podServiceAccount,
		BuilderAgentImage: agentImage, BuildKitImage: buildKitImage, SourceEgressCIDRs: sourceCIDRs, RegistryEgressCIDRs: registryCIDRs, GitHub: githubConfig}
	if err = config.validateEnabled(); err != nil {
		return WorkerRuntimeConfig{}, err
	}
	return config, nil
}

func validRuntimeAgentImage(value string) bool {
	return len(value) >= 80 && len(value) <= 512 && digestImageRE.MatchString(value)
}

func validRuntimeBuildKitImage(value string) bool {
	return len(value) >= len("a/b:v0.32.2") && len(value) <= 512 && buildKitImageRE.MatchString(value)
}

func parseHostCIDRs(raw string) ([]string, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return nil, ErrInvalid
	}
	values := strings.Split(raw, ",")
	if err := validateHostCIDRs(values); err != nil {
		return nil, err
	}
	values = slices.Clone(values)
	slices.Sort(values)
	return values, nil
}

func validateHostCIDRs(values []string) error {
	if len(values) < 1 || len(values) > 16 {
		return ErrInvalid
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		_, network, err := net.ParseCIDR(value)
		prefix, bits := 0, 0
		if network != nil {
			prefix, bits = network.Mask.Size()
		}
		if err != nil || network.String() != value || !((bits == 32 && prefix == 32) || (bits == 128 && prefix == 128)) {
			return ErrInvalid
		}
		if _, duplicate := seen[value]; duplicate {
			return ErrInvalid
		}
		seen[value] = struct{}{}
	}
	return nil
}

func lookupExact(lookup func(string) (string, bool), name string) string {
	value, present := lookup(name)
	if !present || value == "" || strings.TrimSpace(value) != value {
		return ""
	}
	return value
}
