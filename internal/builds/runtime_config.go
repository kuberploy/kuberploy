package builds

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/kuberploy/kuberploy/internal/builder"
	platformconfig "github.com/kuberploy/kuberploy/internal/config"
	"github.com/kuberploy/kuberploy/internal/githubapp"
)

const (
	maximumBuilderEgressCIDRs     = 128
	maximumRegistryEgressCIDRs    = 16
	maximumSecretProfileAppIDs    = 256
	GitHubBuildsEnabledEnv        = "KUBERPLOY_GITHUB_BUILDS_ENABLED"
	GitHubAppIDEnv                = "KUBERPLOY_GITHUB_APP_ID"
	GitHubAppClientIDEnv          = "KUBERPLOY_GITHUB_APP_CLIENT_ID"
	BuilderNamespaceEnv           = "KUBERPLOY_BUILDER_NAMESPACE"
	BuilderPodServiceAccountEnv   = "KUBERPLOY_BUILDER_POD_SERVICE_ACCOUNT"
	BuilderAgentImageEnv          = "KUBERPLOY_BUILDER_AGENT_IMAGE"
	BuilderBuildKitImageEnv       = "KUBERPLOY_BUILDER_BUILDKIT_IMAGE"
	BuilderDinDImageEnv           = "KUBERPLOY_BUILDER_DIND_IMAGE"
	BuilderNodeIsolationEnv       = "KUBERPLOY_BUILDER_NODE_ISOLATION"
	BuilderBuildSecretEnv         = "KUBERPLOY_BUILDER_BUILD_SECRET"
	BuilderSSHSecretEnv           = "KUBERPLOY_BUILDER_SSH_SECRET"
	BuilderBuildSecretProfilesEnv = "KUBERPLOY_BUILDER_BUILD_SECRET_PROFILES"
	BuilderSSHSecretProfilesEnv   = "KUBERPLOY_BUILDER_SSH_SECRET_PROFILES"
	BuilderSourceEgressCIDRsEnv   = "KUBERPLOY_BUILDER_SOURCE_EGRESS_CIDRS"
	BuilderRegistryEgressCIDRsEnv = "KUBERPLOY_BUILDER_REGISTRY_EGRESS_CIDRS"
)

var (
	buildProfileIDRE = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,62}$`)
	secretKeyRE      = regexp.MustCompile(`^[A-Za-z0-9._-]{1,253}$`)
)

// BuildSecretProfile is safe catalog metadata. It contains only an opaque
// selectable profile ID, display label, and fixed mounted path; it never
// contains Secret data or the Kubernetes Secret object name.
type BuildSecretProfile struct {
	ID             string                `json:"id"`
	Label          string                `json:"label"`
	ApplicationIDs []string              `json:"applicationIds"`
	File           builder.FileReference `json:"file"`
}

type BuildSecretProfileCatalog struct {
	Build []BuildSecretProfile `json:"build"`
	SSH   []BuildSecretProfile `json:"ssh"`
}

type BuildSecretSelection struct {
	BuildSecret string
	SSHSecret   string
	SecretFiles []builder.FileReference
	SSHFiles    []builder.FileReference
}

// WorkerRuntimeConfig is deliberately disabled unless the operator supplies
// one exact opt-in flag and every non-secret identity setting. Credential
// bytes remain in the fixed projected Secret root owned by githubapp.
type WorkerRuntimeConfig struct {
	Enabled                  bool
	BuilderNamespace         string
	BuilderPodServiceAccount string
	BuilderAgentImage        string
	BuildKitImage            string
	DinDImage                string
	NodeIsolation            bool
	BuildSecret              string
	SSHSecret                string
	BuildSecretProfiles      []BuildSecretProfile
	SSHSecretProfiles        []BuildSecretProfile
	SourceEgressCIDRs        []string
	RegistryEgressCIDRs      []string
	KubeAPIServerCIDRs       []string
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
		DinDImage                string
		NodeIsolation            bool
		SourceEgressCIDRs        []string
		RegistryEgressCIDRs      []string
		KubeAPIServerCIDRs       []string
		BuildSecretProfiles      []BuildSecretProfile
		SSHSecretProfiles        []BuildSecretProfile
		Execution                ExecutionSettings
	}{2, "deliveries+builds+release-projection", c.GitHub.AppID, c.GitHub.ClientID, c.BuilderNamespace, c.BuilderPodServiceAccount, c.BuilderAgentImage, c.BuildKitImage, effectiveDinDImage(c.DinDImage), c.NodeIsolation,
		slices.Clone(c.SourceEgressCIDRs), slices.Clone(c.RegistryEgressCIDRs), slices.Clone(c.KubeAPIServerCIDRs), slices.Clone(c.BuildSecretProfiles), slices.Clone(c.SSHSecretProfiles), c.executionTemplate()}
	encoded, err := json.Marshal(view)
	if err != nil {
		return "", ErrInvalid
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (c WorkerRuntimeConfig) SecretProfileCatalog(applicationID string) (BuildSecretProfileCatalog, error) {
	if !uuidRE.MatchString(applicationID) {
		return BuildSecretProfileCatalog{}, ErrInvalid
	}
	return BuildSecretProfileCatalog{
		Build: profilesForApplication(c.BuildSecretProfiles, applicationID),
		SSH:   profilesForApplication(c.SSHSecretProfiles, applicationID),
	}, nil
}

func (c WorkerRuntimeConfig) ResolveSecretProfiles(applicationID string, buildIDs, sshIDs []string) (BuildSecretSelection, error) {
	if err := c.validateEnabled(); err != nil || !uuidRE.MatchString(applicationID) {
		return BuildSecretSelection{}, ErrInvalid
	}
	buildProfiles := profilesForApplication(c.BuildSecretProfiles, applicationID)
	sshProfiles := profilesForApplication(c.SSHSecretProfiles, applicationID)
	selection := BuildSecretSelection{}
	selection.SecretFiles, selection.BuildSecret = selectSecretProfiles(c.BuildSecret, buildProfiles, buildIDs, builder.BuildSecretRoot)
	selection.SSHFiles, selection.SSHSecret = selectSecretProfiles(c.SSHSecret, sshProfiles, sshIDs, builder.SSHSecretRoot)
	if (len(buildIDs) > 0 && len(selection.SecretFiles) == 0) || (len(sshIDs) > 0 && len(selection.SSHFiles) == 0) {
		return BuildSecretSelection{}, ErrInvalid
	}
	return selection, nil
}

func (c WorkerRuntimeConfig) ResolveSecretFiles(applicationID string, secretFiles, sshFiles []builder.FileReference) (BuildSecretSelection, error) {
	if !uuidRE.MatchString(applicationID) {
		return BuildSecretSelection{}, ErrInvalid
	}
	buildIDs, err := profileIDsForFiles(profilesForApplication(c.BuildSecretProfiles, applicationID), secretFiles)
	if err != nil {
		return BuildSecretSelection{}, ErrInvalid
	}
	sshIDs, err := profileIDsForFiles(profilesForApplication(c.SSHSecretProfiles, applicationID), sshFiles)
	if err != nil {
		return BuildSecretSelection{}, ErrInvalid
	}
	return c.ResolveSecretProfiles(applicationID, buildIDs, sshIDs)
}

func profilesForApplication(profiles []BuildSecretProfile, applicationID string) []BuildSecretProfile {
	filtered := make([]BuildSecretProfile, 0, len(profiles))
	for _, profile := range profiles {
		if slices.Contains(profile.ApplicationIDs, applicationID) {
			filtered = append(filtered, profile)
		}
	}
	return filtered
}

func selectSecretProfiles(secretName string, profiles []BuildSecretProfile, ids []string, root string) ([]builder.FileReference, string) {
	if len(ids) == 0 {
		return nil, ""
	}
	byID := make(map[string]BuildSecretProfile, len(profiles))
	for _, profile := range profiles {
		byID[profile.ID] = profile
	}
	selected := make([]builder.FileReference, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		profile, ok := byID[id]
		if !ok || profile.File.Path == "" {
			return nil, ""
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, ""
		}
		seen[id] = struct{}{}
		selected = append(selected, builder.FileReference{ID: profile.File.ID, Path: root + "/" + strings.TrimPrefix(profile.File.Path, root+"/")})
	}
	return selected, secretName
}

func profileIDsForFiles(profiles []BuildSecretProfile, files []builder.FileReference) ([]string, error) {
	if len(files) == 0 {
		return nil, nil
	}
	byFile := make(map[string]string, len(profiles))
	for _, profile := range profiles {
		byFile[profile.File.ID+"\x00"+profile.File.Path] = profile.ID
	}
	ids := make([]string, 0, len(files))
	for _, file := range files {
		id, ok := byFile[file.ID+"\x00"+file.Path]
		if !ok {
			return nil, ErrInvalid
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// ExecutionSettings returns the operator-owned portion copied into an
// immutable build definition. registryPort is derived from the verified
// registry target, never from the public build-definition request.
func (c WorkerRuntimeConfig) ExecutionSettings(registryPort int) (ExecutionSettings, error) {
	return c.ExecutionSettingsForPlatform(registryPort, DefaultBuilderPlatformSettings(c))
}

// ExecutionSettingsForPlatform combines immutable install-time identities and
// provider boundaries with database-backed operator settings for new builds.
func (c WorkerRuntimeConfig) ExecutionSettingsForPlatform(registryPort int, platform BuilderPlatformSettings) (ExecutionSettings, error) {
	if err := c.validateEnabled(); err != nil || registryPort < 1 || registryPort > 65535 {
		return ExecutionSettings{}, ErrInvalid
	}
	if err := platform.Validate(); err != nil {
		return ExecutionSettings{}, ErrInvalid
	}
	settings := c.executionTemplate()
	settings.CheckoutResources = platform.CheckoutResources
	settings.DinDResources = platform.DinDResources
	settings.AgentResources = platform.AgentResources
	settings.NodeSelector = nil
	settings.Toleration = builder.TaintToleration{}
	if platform.NodeIsolation {
		settings.NodeSelector = map[string]string{"kuberploy.io/node-class": "dind-builder"}
		settings.Toleration = builder.TaintToleration{Key: "kuberploy.io/dind-builder", Value: "true", Effect: "NoSchedule"}
	}
	sourceCIDRs := c.SourceEgressCIDRs
	if len(sourceCIDRs) == 0 {
		sourceCIDRs = []string{"0.0.0.0/0", "::/0"}
	}
	registryCIDRs := c.RegistryEgressCIDRs
	if len(registryCIDRs) == 0 {
		registryCIDRs = []string{"0.0.0.0/0", "::/0"}
	}
	portsByCIDR := make(map[string]map[int]struct{}, len(sourceCIDRs)+len(registryCIDRs))
	for _, cidr := range sourceCIDRs {
		portsByCIDR[cidr] = map[int]struct{}{443: {}}
	}
	for _, cidr := range registryCIDRs {
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
		except := []string(nil)
		if cidr == "0.0.0.0/0" || cidr == "::/0" {
			for _, apiCIDR := range c.KubeAPIServerCIDRs {
				if strings.Contains(cidr, ".") == strings.Contains(apiCIDR, ".") {
					except = append(except, apiCIDR)
				}
			}
		}
		settings.Egress = append(settings.Egress, builder.EgressEndpoint{CIDR: cidr, Ports: ports, Except: except})
	}
	slices.SortFunc(settings.Egress, func(a, b builder.EgressEndpoint) int { return strings.Compare(a.CIDR, b.CIDR) })
	return settings, nil
}

func (c WorkerRuntimeConfig) executionTemplate() ExecutionSettings {
	settings := ExecutionSettings{
		Namespace: c.BuilderNamespace, PodServiceAccount: c.BuilderPodServiceAccount, BuilderAgentImage: c.BuilderAgentImage, BuildKitImage: c.BuildKitImage, DinDImage: effectiveDinDImage(c.DinDImage),
		CheckoutResources:  builder.ContainerResources{CPURequest: "100m", MemoryRequest: "128Mi", EphemeralStorageRequest: "1Gi", CPULimit: "1", MemoryLimit: "512Mi", EphemeralStorageLimit: "2Gi"},
		DinDResources:      builder.ContainerResources{CPURequest: "500m", MemoryRequest: "1Gi", EphemeralStorageRequest: "10Gi", CPULimit: "4", MemoryLimit: "8Gi", EphemeralStorageLimit: "50Gi"},
		AgentResources:     builder.ContainerResources{CPURequest: "250m", MemoryRequest: "256Mi", EphemeralStorageRequest: "1Gi", CPULimit: "4", MemoryLimit: "4Gi", EphemeralStorageLimit: "10Gi"},
		WorkspaceSizeLimit: "20Gi", SocketSizeLimit: "64Mi", ResultSizeLimit: "1Mi", DockerDataSizeLimit: "50Gi",
		ActiveDeadlineSeconds: 7860, TTLSecondsAfterFinished: 3600,
	}
	if c.NodeIsolation {
		settings.NodeSelector = map[string]string{"kuberploy.io/node-class": "dind-builder"}
		settings.Toleration = builder.TaintToleration{Key: "kuberploy.io/dind-builder", Value: "true", Effect: "NoSchedule"}
	}
	return settings
}

func (c WorkerRuntimeConfig) validateEnabled() error {
	dindImage := effectiveDinDImage(c.DinDImage)
	if !c.Enabled || c.GitHub.Validate() != nil || !kubeNameRE.MatchString(c.BuilderNamespace) ||
		!kubeNameRE.MatchString(c.BuilderPodServiceAccount) || !validRuntimeAgentImage(c.BuilderAgentImage) || !validRuntimeBuildKitImage(c.BuildKitImage) || !validRuntimeDinDImage(dindImage) ||
		validateKubeAPIServerCIDRs(c.KubeAPIServerCIDRs) != nil {
		return ErrInvalid
	}
	if (len(c.SourceEgressCIDRs) > 0 && validateProviderCIDRs(c.SourceEgressCIDRs, maximumBuilderEgressCIDRs) != nil) ||
		(len(c.RegistryEgressCIDRs) > 0 && validateHostCIDRs(c.RegistryEgressCIDRs) != nil) || len(c.SourceEgressCIDRs)+len(c.RegistryEgressCIDRs) > maximumBuilderEgressCIDRs {
		return ErrInvalid
	}
	if (len(c.BuildSecretProfiles) > 0 && !kubeNameRE.MatchString(c.BuildSecret)) ||
		(len(c.SSHSecretProfiles) > 0 && !kubeNameRE.MatchString(c.SSHSecret)) {
		return ErrInvalid
	}
	for _, providerCIDR := range append(slices.Clone(c.SourceEgressCIDRs), c.RegistryEgressCIDRs...) {
		_, provider, _ := net.ParseCIDR(providerCIDR)
		for _, apiCIDR := range c.KubeAPIServerCIDRs {
			_, api, _ := net.ParseCIDR(apiCIDR)
			if provider != nil && api != nil && len(provider.IP) == len(api.IP) && (provider.Contains(api.IP) || api.Contains(provider.IP)) {
				return ErrInvalid
			}
		}
	}
	return nil
}

func validateKubeAPIServerCIDRs(values []string) error {
	if len(values) == 0 {
		return nil
	}
	if len(values) > 16 {
		return ErrInvalid
	}
	canonical, err := platformconfig.ParseKubeAPIServerCIDRs(strings.Join(values, ","))
	if err != nil || !slices.Equal(canonical, values) {
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
	dindImage := lookupExact(lookup, BuilderDinDImageEnv)
	nodeIsolationValue := lookupExact(lookup, BuilderNodeIsolationEnv)
	buildSecret := lookupExact(lookup, BuilderBuildSecretEnv)
	sshSecret := lookupExact(lookup, BuilderSSHSecretEnv)
	if dindImage == "" {
		dindImage = builder.DefaultDinDImage
	}
	buildProfiles, buildProfilesErr := parseSecretProfiles(lookupExact(lookup, BuilderBuildSecretProfilesEnv), builder.BuildSecretRoot)
	sshProfiles, sshProfilesErr := parseSecretProfiles(lookupExact(lookup, BuilderSSHSecretProfilesEnv), builder.SSHSecretRoot)
	sourceCIDRs, sourceErr := parseProviderCIDRs(lookupExact(lookup, BuilderSourceEgressCIDRsEnv))
	registryCIDRs, registryErr := parseHostCIDRs(lookupExact(lookup, BuilderRegistryEgressCIDRsEnv))
	kubeAPICIDRs, kubeAPIErr := platformconfig.ParseKubeAPIServerCIDRs(lookupExact(lookup, platformconfig.KubeAPIServerCIDRsEnv))
	if nodeIsolationValue == "" {
		nodeIsolationValue = "false"
	}
	if nodeIsolationValue != "true" && nodeIsolationValue != "false" {
		return WorkerRuntimeConfig{}, errors.New("KUBERPLOY_BUILDER_NODE_ISOLATION must be exactly true or false")
	}
	if appIDValue == "" || clientID == "" || namespace == "" || !kubeNameRE.MatchString(namespace) ||
		!kubeNameRE.MatchString(podServiceAccount) || !validRuntimeAgentImage(agentImage) || !validRuntimeBuildKitImage(buildKitImage) || !validRuntimeDinDImage(dindImage) ||
		(buildSecret != "" && !kubeNameRE.MatchString(buildSecret)) || (sshSecret != "" && !kubeNameRE.MatchString(sshSecret)) ||
		buildProfilesErr != nil || sshProfilesErr != nil || sourceErr != nil || registryErr != nil || kubeAPIErr != nil {
		return WorkerRuntimeConfig{}, errors.New("enabled GitHub builds require exact provider identity, immutable builder runtime, and canonical Kubernetes API exclusions")
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
		BuilderAgentImage: agentImage, BuildKitImage: buildKitImage, DinDImage: dindImage, NodeIsolation: nodeIsolationValue == "true", BuildSecret: buildSecret, SSHSecret: sshSecret,
		BuildSecretProfiles: buildProfiles, SSHSecretProfiles: sshProfiles, SourceEgressCIDRs: sourceCIDRs, RegistryEgressCIDRs: registryCIDRs, KubeAPIServerCIDRs: kubeAPICIDRs, GitHub: githubConfig}
	if err = config.validateEnabled(); err != nil {
		return WorkerRuntimeConfig{}, err
	}
	return config, nil
}

func validRuntimeImage(value string) bool {
	return len(value) >= 3 && len(value) <= 512 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, " \t\r\n")
}

func validRuntimeAgentImage(value string) bool {
	return validRuntimeImage(value) && builder.ValidateExecutionImage(value) == nil
}

func validRuntimeBuildKitImage(value string) bool {
	return builder.ValidateBuildKitImage(value) == nil
}

func validRuntimeDinDImage(value string) bool {
	return validRuntimeImage(value) && builder.ValidateExecutionImage(value) == nil
}

func effectiveDinDImage(value string) string {
	if value == "" {
		return builder.DefaultDinDImage
	}
	return value
}

func parseHostCIDRs(raw string) ([]string, error) {
	return parseCIDRs(raw, func(values []string) error { return validateHostCIDRsUnordered(values) })
}

func parseProviderCIDRs(raw string) ([]string, error) {
	return parseCIDRs(raw, func(values []string) error { return validateProviderCIDRsUnordered(values, maximumBuilderEgressCIDRs) })
}

func parseCIDRs(raw string, validate func([]string) error) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	if strings.TrimSpace(raw) != raw {
		return nil, ErrInvalid
	}
	values := strings.Split(raw, ",")
	if len(values) > maximumBuilderEgressCIDRs {
		return nil, ErrInvalid
	}
	values = slices.Clone(values)
	slices.Sort(values)
	values = slices.Compact(values)
	if err := validate(values); err != nil {
		return nil, err
	}
	return values, nil
}

func parseSecretProfiles(raw, root string) ([]BuildSecretProfile, error) {
	if raw == "" {
		return nil, nil
	}
	var values []struct {
		ID             string   `json:"id"`
		Label          string   `json:"label"`
		Key            string   `json:"key"`
		ApplicationIDs []string `json:"applicationIds"`
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&values); err != nil || len(values) == 0 {
		return nil, ErrInvalid
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, ErrInvalid
	}
	maximum := 32
	if root == builder.SSHSecretRoot {
		maximum = 8
	}
	if len(values) > maximum {
		return nil, ErrInvalid
	}
	profiles := make([]BuildSecretProfile, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !buildProfileIDRE.MatchString(value.ID) || len(value.Label) < 1 || len(value.Label) > 128 || strings.TrimSpace(value.Label) != value.Label ||
			strings.ContainsAny(value.Label, "\x00\r\n") || !secretKeyRE.MatchString(value.Key) || len(value.ApplicationIDs) < 1 || len(value.ApplicationIDs) > maximumSecretProfileAppIDs {
			return nil, ErrInvalid
		}
		applicationIDs := slices.Clone(value.ApplicationIDs)
		slices.Sort(applicationIDs)
		if len(slices.Compact(applicationIDs)) != len(applicationIDs) {
			return nil, ErrInvalid
		}
		for _, applicationID := range applicationIDs {
			if !uuidRE.MatchString(applicationID) {
				return nil, ErrInvalid
			}
		}
		if _, duplicate := seen[value.ID]; duplicate {
			return nil, ErrInvalid
		}
		seen[value.ID] = struct{}{}
		profiles = append(profiles, BuildSecretProfile{ID: value.ID, Label: value.Label, ApplicationIDs: applicationIDs, File: builder.FileReference{ID: value.ID, Path: root + "/" + value.Key}})
	}
	slices.SortFunc(profiles, func(a, b BuildSecretProfile) int { return strings.Compare(a.ID, b.ID) })
	return profiles, nil
}

func validateHostCIDRs(values []string) error {
	if !slices.IsSorted(values) {
		return ErrInvalid
	}
	return validateHostCIDRsUnordered(values)
}

func validateHostCIDRsUnordered(values []string) error {
	if len(values) < 1 || len(values) > maximumRegistryEgressCIDRs {
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

func validateProviderCIDRs(values []string, maximum int) error {
	if !slices.IsSorted(values) {
		return ErrInvalid
	}
	return validateProviderCIDRsUnordered(values, maximum)
}

func validateProviderCIDRsUnordered(values []string, maximum int) error {
	if len(values) < 1 || len(values) > maximum || maximum < 1 || maximum > maximumBuilderEgressCIDRs {
		return ErrInvalid
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		_, network, err := net.ParseCIDR(value)
		prefix, bits := 0, 0
		if network != nil {
			prefix, bits = network.Mask.Size()
		}
		if err != nil || network.String() != value || !((bits == 32 && prefix >= 8 && prefix <= 32) || (bits == 128 && prefix >= 16 && prefix <= 128)) {
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
