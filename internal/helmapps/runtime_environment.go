package helmapps

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	RuntimeEnabledEnv                = "KUBERPLOY_HELM_APPLICATIONS_ENABLED"
	RuntimeRendererNamespaceEnv      = "KUBERPLOY_HELM_RENDERER_NAMESPACE"
	RuntimeRendererServiceAccountEnv = "KUBERPLOY_HELM_RENDERER_SERVICE_ACCOUNT"
	RuntimeRendererPollMillisEnv     = "KUBERPLOY_HELM_RENDERER_POLL_MILLISECONDS"
	RuntimeWorkPollMillisEnv         = "KUBERPLOY_HELM_WORK_POLL_MILLISECONDS"
	RuntimeRenderLeaseSecondsEnv     = "KUBERPLOY_HELM_RENDER_LEASE_SECONDS"
	RuntimePublishLeaseSecondsEnv    = "KUBERPLOY_HELM_PUBLISH_LEASE_SECONDS"
	RuntimeReadinessSecondsEnv       = "KUBERPLOY_HELM_READINESS_LEASE_SECONDS"
	RuntimeOCIRequestSecondsEnv      = "KUBERPLOY_HELM_OCI_REQUEST_SECONDS"
	RuntimeOCIRegistryHostsEnv       = "KUBERPLOY_HELM_OCI_REGISTRY_HOSTS"
	RuntimeOCIAuthHostsEnv           = "KUBERPLOY_HELM_OCI_AUTH_HOSTS"
	RuntimeOCIRedirectHostsEnv       = "KUBERPLOY_HELM_OCI_REDIRECT_HOSTS"
	RuntimeOCICredentialProfilesEnv  = "KUBERPLOY_HELM_OCI_CREDENTIAL_PROFILES_JSON"
	RuntimePackageCacheBytesEnv      = "KUBERPLOY_HELM_PACKAGE_CACHE_BYTES"
	RuntimeArgoNamespaceEnv          = "KUBERPLOY_HELM_ARGO_NAMESPACE"
)

func RuntimeConfigFromEnvironment(publisher ProtectedPublisherIdentity) (RuntimeConfig, error) {
	return RuntimeConfigFromLookup(os.LookupEnv, publisher)
}

func RuntimeConfigFromLookup(lookup func(string) (string, bool), publisher ProtectedPublisherIdentity) (RuntimeConfig, error) {
	if lookup == nil {
		return RuntimeConfig{}, ErrInvalid
	}
	enabled, present := lookup(RuntimeEnabledEnv)
	if !present || enabled == "" || enabled == "false" {
		return RuntimeConfig{}, nil
	}
	if enabled != "true" || publisher.Validate() != nil {
		return RuntimeConfig{}, ErrInvalid
	}
	rendererNamespace, namespaceOK := exactRuntimeEnvironment(lookup, RuntimeRendererNamespaceEnv)
	serviceAccount, accountOK := exactRuntimeEnvironment(lookup, RuntimeRendererServiceAccountEnv)
	registryValue, registryOK := exactRuntimeEnvironment(lookup, RuntimeOCIRegistryHostsEnv)
	argoNamespace, argoOK := exactRuntimeEnvironment(lookup, RuntimeArgoNamespaceEnv)
	if !namespaceOK || !accountOK || !registryOK || !argoOK {
		return RuntimeConfig{}, ErrInvalid
	}
	config := RuntimeConfig{Enabled: true,
		Renderer:         KubernetesRendererConfig{Namespace: rendererNamespace, ServiceAccount: serviceAccount},
		OCIRegistryHosts: strings.Split(registryValue, ","), Application: ProtectedApplicationRuntime{ArgoNamespace: argoNamespace},
		Publisher: publisher}
	if authValue, configured := lookup(RuntimeOCIAuthHostsEnv); configured {
		if authValue == "" || strings.TrimSpace(authValue) != authValue || strings.ContainsAny(authValue, "\x00\r\n") {
			return RuntimeConfig{}, ErrInvalid
		}
		config.OCIAuthHosts = strings.Split(authValue, ",")
	}
	if redirectValue, configured := lookup(RuntimeOCIRedirectHostsEnv); configured {
		if redirectValue == "" || strings.TrimSpace(redirectValue) != redirectValue || strings.ContainsAny(redirectValue, "\x00\r\n") {
			return RuntimeConfig{}, ErrInvalid
		}
		config.OCIRedirectHosts = strings.Split(redirectValue, ",")
	}
	if profilesValue, configured := lookup(RuntimeOCICredentialProfilesEnv); configured {
		if len(profilesValue) > 32<<10 || strings.TrimSpace(profilesValue) != profilesValue || strings.ContainsAny(profilesValue, "\x00\r\n") {
			return RuntimeConfig{}, ErrInvalid
		}
		if _, err := decodeStrictJSON([]byte(profilesValue)); err != nil {
			return RuntimeConfig{}, ErrInvalid
		}
		decoder := json.NewDecoder(bytes.NewBufferString(profilesValue))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&config.OCICredentialProfiles); err != nil || len(config.OCICredentialProfiles) < 1 {
			return RuntimeConfig{}, ErrInvalid
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			return RuntimeConfig{}, ErrInvalid
		}
	}
	var err error
	if config.Renderer.PollInterval, err = runtimeDuration(lookup, RuntimeRendererPollMillisEnv, time.Millisecond); err != nil {
		return RuntimeConfig{}, err
	}
	if config.WorkPollInterval, err = runtimeDuration(lookup, RuntimeWorkPollMillisEnv, time.Millisecond); err != nil {
		return RuntimeConfig{}, err
	}
	if config.RenderLeaseDuration, err = runtimeDuration(lookup, RuntimeRenderLeaseSecondsEnv, time.Second); err != nil {
		return RuntimeConfig{}, err
	}
	if config.PublishLeaseDuration, err = runtimeDuration(lookup, RuntimePublishLeaseSecondsEnv, time.Second); err != nil {
		return RuntimeConfig{}, err
	}
	if config.ReadinessLeaseDuration, err = runtimeDuration(lookup, RuntimeReadinessSecondsEnv, time.Second); err != nil {
		return RuntimeConfig{}, err
	}
	if config.OCIRequestTimeout, err = runtimeDuration(lookup, RuntimeOCIRequestSecondsEnv, time.Second); err != nil {
		return RuntimeConfig{}, err
	}
	cacheValue, cacheOK := exactRuntimeEnvironment(lookup, RuntimePackageCacheBytesEnv)
	cacheBytes, parseErr := strconv.ParseInt(cacheValue, 10, 32)
	if !cacheOK || parseErr != nil || strconv.FormatInt(cacheBytes, 10) != cacheValue {
		return RuntimeConfig{}, ErrInvalid
	}
	config.PackageCacheBytes = int(cacheBytes)
	config.OCIRegistryHosts = normalizeRuntimeHosts(config.OCIRegistryHosts)
	config.OCIAuthHosts = normalizeRuntimeHosts(config.OCIAuthHosts)
	config.OCIRedirectHosts = normalizeRuntimeHosts(config.OCIRedirectHosts)
	sort.Slice(config.OCICredentialProfiles, func(left, right int) bool {
		return config.OCICredentialProfiles[left].RegistryHost < config.OCICredentialProfiles[right].RegistryHost
	})
	config.OCICredentialProfiles = slices.Compact(config.OCICredentialProfiles)
	if config.Validate() != nil {
		return RuntimeConfig{}, ErrInvalid
	}
	return config, nil
}

func normalizeRuntimeHosts(values []string) []string {
	values = slices.Clone(values)
	slices.Sort(values)
	return slices.Compact(values)
}

func runtimeDuration(lookup func(string) (string, bool), name string, unit time.Duration) (time.Duration, error) {
	value, ok := exactRuntimeEnvironment(lookup, name)
	parsed, err := strconv.ParseInt(value, 10, 32)
	if !ok || err != nil || parsed < 1 || strconv.FormatInt(parsed, 10) != value {
		return 0, ErrInvalid
	}
	return time.Duration(parsed) * unit, nil
}

func exactRuntimeEnvironment(lookup func(string) (string, bool), name string) (string, bool) {
	value, present := lookup(name)
	return value, present && value != "" && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}
