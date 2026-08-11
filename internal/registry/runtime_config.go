package registry

import (
	"errors"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

const (
	ManagedRegistryRuntimeEnabledEnv         = "KUBERPLOY_MANAGED_REGISTRY_RUNTIME_ENABLED"
	ManagedRegistryTargetIDEnv               = "KUBERPLOY_MANAGED_REGISTRY_TARGET_ID"
	ManagedRegistryTargetNameEnv             = "KUBERPLOY_MANAGED_REGISTRY_TARGET_NAME"
	ManagedRegistryEndpointEnv               = "KUBERPLOY_MANAGED_REGISTRY_ENDPOINT"
	ManagedRegistryRepositoryPrefixEnv       = "KUBERPLOY_MANAGED_REGISTRY_REPOSITORY_PREFIX"
	ManagedRegistryPullCredentialRefEnv      = "KUBERPLOY_MANAGED_REGISTRY_PULL_CREDENTIAL_REF"
	ManagedRegistryPushCredentialRefEnv      = "KUBERPLOY_MANAGED_REGISTRY_PUSH_CREDENTIAL_REF"
	ManagedRegistryCacheCredentialRefEnv     = "KUBERPLOY_MANAGED_REGISTRY_CACHE_CREDENTIAL_REF"
	ManagedRegistryLifecycleCredentialRefEnv = "KUBERPLOY_MANAGED_REGISTRY_LIFECYCLE_CREDENTIAL_REF"
	ManagedRegistryAllowPlainHTTPEnv         = "KUBERPLOY_MANAGED_REGISTRY_ALLOW_PLAIN_HTTP"
	ManagedRegistryNamespaceEnv              = "KUBERPLOY_MANAGED_REGISTRY_NAMESPACE"
	ManagedRegistryDeploymentEnv             = "KUBERPLOY_MANAGED_REGISTRY_DEPLOYMENT"
	ManagedRegistryPVCEnv                    = "KUBERPLOY_MANAGED_REGISTRY_PVC"
	ManagedRegistryConfigMapEnv              = "KUBERPLOY_MANAGED_REGISTRY_CONFIG_MAP"
	ManagedRegistryHelperServiceAccountEnv   = "KUBERPLOY_MANAGED_REGISTRY_HELPER_SERVICE_ACCOUNT"
	ManagedRegistryHelperImageEnv            = "KUBERPLOY_MANAGED_REGISTRY_HELPER_IMAGE"
	ManagedRegistryObservationSecondsEnv     = "KUBERPLOY_MANAGED_REGISTRY_OBSERVATION_SECONDS"

	managedRegistryCredentialRoot = "/var/run/secrets/kuberploy/managed-registry"
	managedRegistryUsernamePath   = managedRegistryCredentialRoot + "/username"
	managedRegistryPasswordPath   = managedRegistryCredentialRoot + "/password"
	managedRegistryStorageRoot    = "/var/lib/registry"
	managedRegistryConfigPath     = "/etc/distribution/config.yml"
)

var (
	errRegistryRuntimeConfig = errors.New("managed registry runtime configuration is invalid")
	registryUUIDRE           = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	registryKubeNameRE       = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9.]*[a-z0-9])?$`)
	registryImageDigestRE    = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)
	registryCredentialRefRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$`)
)

// RuntimeConfig is an operator-owned binding to one built-in managed
// Distribution instance. Persisted target metadata must match every identity
// field before credentials, Kubernetes mutation, or deletion become reachable.
type RuntimeConfig struct {
	Enabled            bool
	TargetID           string
	TargetName         string
	Endpoint           string
	RepositoryPrefix   string
	PullCredentialRef  string
	PushCredentialRef  string
	CacheCredentialRef string
	// CredentialRef identifies the operator-only lifecycle/maintenance
	// credential projection. It is deliberately independent from the target's
	// build-push, cache, and runtime-pull credential references.
	CredentialRef         string
	AllowPlainHTTP        bool
	Namespace             string
	Deployment            string
	PersistentVolumeClaim string
	RegistryConfigMap     string
	HelperServiceAccount  string
	HelperImage           string
	ObservationInterval   time.Duration
}

func (c RuntimeConfig) Validate() error {
	if !c.Enabled {
		if c != (RuntimeConfig{}) {
			return errRegistryRuntimeConfig
		}
		return nil
	}
	parsed, err := url.Parse(c.Endpoint)
	if err != nil || parsed.String() != c.Endpoint || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil || parsed.Path != "" || parsed.Host == "" ||
		(parsed.Scheme != "https" && !(c.AllowPlainHTTP && parsed.Scheme == "http")) {
		return errRegistryRuntimeConfig
	}
	if !registryUUIDRE.MatchString(c.TargetID) || strings.TrimSpace(c.TargetName) != c.TargetName || c.TargetName == "" ||
		len(c.TargetName) > 100 || !validRepository(c.RepositoryPrefix) ||
		!registryCredentialRefRE.MatchString(c.PullCredentialRef) ||
		!registryCredentialRefRE.MatchString(c.PushCredentialRef) ||
		!registryCredentialRefRE.MatchString(c.CacheCredentialRef) ||
		c.PullCredentialRef == c.PushCredentialRef || c.PullCredentialRef == c.CacheCredentialRef ||
		c.PushCredentialRef == c.CacheCredentialRef || c.CredentialRef == c.PullCredentialRef ||
		c.CredentialRef == c.PushCredentialRef || c.CredentialRef == c.CacheCredentialRef ||
		!registryCredentialRefRE.MatchString(c.CredentialRef) ||
		!validRegistryKubeName(c.Namespace) || !validRegistryKubeName(c.Deployment) ||
		!validRegistryKubeName(c.PersistentVolumeClaim) || !validRegistryKubeName(c.RegistryConfigMap) ||
		!validRegistryKubeName(c.HelperServiceAccount) || !registryImageDigestRE.MatchString(c.HelperImage) ||
		c.ObservationInterval < 15*time.Second || c.ObservationInterval > 24*time.Hour {
		return errRegistryRuntimeConfig
	}
	return nil
}

// ManagedTarget materializes the exact operator-owned catalog row. Both API
// and worker call this before serving or reconciling so install order cannot
// turn the built-in registry into caller-managed metadata.
func (c RuntimeConfig) ManagedTarget() (domain.RegistryTarget, error) {
	target := domain.RegistryTarget{
		ID: c.TargetID, Name: c.TargetName, Mode: domain.RegistryTargetManaged,
		Endpoint: c.Endpoint, RepositoryPrefix: c.RepositoryPrefix,
		PullCredentialRef: c.PullCredentialRef, PushCredentialRef: c.PushCredentialRef,
		CacheCredentialRef: c.CacheCredentialRef,
	}
	if c.Validate() != nil || ValidateTarget(target) != nil ||
		c.CredentialRef == target.PullCredentialRef || c.CredentialRef == target.PushCredentialRef ||
		c.CredentialRef == target.CacheCredentialRef {
		return domain.RegistryTarget{}, errRegistryRuntimeConfig
	}
	return target, nil
}

// ValidateTarget rejects any persisted target mutation before a credential is
// read or a destructive runtime path is entered.
func (c RuntimeConfig) ValidateTarget(target domain.RegistryTarget) error {
	if c.Validate() != nil || !c.Enabled || target.ID != c.TargetID || target.Mode != domain.RegistryTargetManaged ||
		target.Name != c.TargetName || target.Endpoint != c.Endpoint || target.RepositoryPrefix != c.RepositoryPrefix ||
		target.PullCredentialRef != c.PullCredentialRef || target.PushCredentialRef != c.PushCredentialRef ||
		target.CacheCredentialRef != c.CacheCredentialRef ||
		ValidateTarget(target) != nil {
		return ErrDistributionScopeMismatch
	}
	return nil
}

func validRegistryKubeName(value string) bool {
	return len(value) <= 253 && registryKubeNameRE.MatchString(value)
}

func RuntimeConfigFromEnvironment() (RuntimeConfig, error) {
	return RuntimeConfigFromLookup(os.LookupEnv)
}

func RuntimeConfigFromLookup(lookup func(string) (string, bool)) (RuntimeConfig, error) {
	if lookup == nil {
		return RuntimeConfig{}, errRegistryRuntimeConfig
	}
	enabled, present := lookup(ManagedRegistryRuntimeEnabledEnv)
	if !present || enabled == "" || enabled == "false" {
		for _, name := range managedRegistryRuntimeIdentityEnvs() {
			if value, ok := lookup(name); ok && value != "" {
				return RuntimeConfig{}, errRegistryRuntimeConfig
			}
		}
		return RuntimeConfig{}, nil
	}
	if enabled != "true" {
		return RuntimeConfig{}, errRegistryRuntimeConfig
	}
	plainValue := exactRegistryEnv(lookup, ManagedRegistryAllowPlainHTTPEnv)
	allowPlain, err := strconv.ParseBool(plainValue)
	if err != nil || strconv.FormatBool(allowPlain) != plainValue {
		return RuntimeConfig{}, errRegistryRuntimeConfig
	}
	secondsValue := exactRegistryEnv(lookup, ManagedRegistryObservationSecondsEnv)
	seconds, err := strconv.ParseInt(secondsValue, 10, 32)
	if err != nil || seconds < 1 || strconv.FormatInt(seconds, 10) != secondsValue {
		return RuntimeConfig{}, errRegistryRuntimeConfig
	}
	config := RuntimeConfig{
		Enabled: true, TargetID: exactRegistryEnv(lookup, ManagedRegistryTargetIDEnv),
		TargetName:         exactRegistryEnv(lookup, ManagedRegistryTargetNameEnv),
		Endpoint:           exactRegistryEnv(lookup, ManagedRegistryEndpointEnv),
		RepositoryPrefix:   exactRegistryEnv(lookup, ManagedRegistryRepositoryPrefixEnv),
		PullCredentialRef:  exactRegistryEnv(lookup, ManagedRegistryPullCredentialRefEnv),
		PushCredentialRef:  exactRegistryEnv(lookup, ManagedRegistryPushCredentialRefEnv),
		CacheCredentialRef: exactRegistryEnv(lookup, ManagedRegistryCacheCredentialRefEnv),
		CredentialRef:      exactRegistryEnv(lookup, ManagedRegistryLifecycleCredentialRefEnv),
		AllowPlainHTTP:     allowPlain, Namespace: exactRegistryEnv(lookup, ManagedRegistryNamespaceEnv),
		Deployment:            exactRegistryEnv(lookup, ManagedRegistryDeploymentEnv),
		PersistentVolumeClaim: exactRegistryEnv(lookup, ManagedRegistryPVCEnv),
		RegistryConfigMap:     exactRegistryEnv(lookup, ManagedRegistryConfigMapEnv),
		HelperServiceAccount:  exactRegistryEnv(lookup, ManagedRegistryHelperServiceAccountEnv),
		HelperImage:           exactRegistryEnv(lookup, ManagedRegistryHelperImageEnv),
		ObservationInterval:   time.Duration(seconds) * time.Second,
	}
	if err = config.Validate(); err != nil {
		return RuntimeConfig{}, err
	}
	return config, nil
}

func managedRegistryRuntimeIdentityEnvs() []string {
	return []string{
		ManagedRegistryTargetIDEnv, ManagedRegistryTargetNameEnv, ManagedRegistryEndpointEnv, ManagedRegistryRepositoryPrefixEnv,
		ManagedRegistryPullCredentialRefEnv, ManagedRegistryPushCredentialRefEnv, ManagedRegistryCacheCredentialRefEnv,
		ManagedRegistryLifecycleCredentialRefEnv, ManagedRegistryAllowPlainHTTPEnv, ManagedRegistryNamespaceEnv,
		ManagedRegistryDeploymentEnv, ManagedRegistryPVCEnv, ManagedRegistryConfigMapEnv,
		ManagedRegistryHelperServiceAccountEnv, ManagedRegistryHelperImageEnv,
		ManagedRegistryObservationSecondsEnv,
	}
}

func exactRegistryEnv(lookup func(string) (string, bool), name string) string {
	value, ok := lookup(name)
	if !ok || value == "" || strings.TrimSpace(value) != value {
		return ""
	}
	return value
}
