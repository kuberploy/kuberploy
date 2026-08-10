package argo

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/githubapp"
)

const (
	ProductionEnabledEnv              = "KUBERPLOY_ARGO_DESIRED_STATE_ENABLED"
	ProductionPlatformBindingIDEnv    = "KUBERPLOY_ARGO_PLATFORM_BINDING_ID"
	ProductionClusterIDEnv            = "KUBERPLOY_CLUSTER_ID"
	ProductionNamespaceEnv            = "KUBERPLOY_ARGO_NAMESPACE"
	ProductionChartRepositoryEnv      = "KUBERPLOY_ARGO_RUNTIME_CHART_REPOSITORY"
	ProductionChartVersionEnv         = "KUBERPLOY_ARGO_RUNTIME_CHART_VERSION"
	ProductionChartDigestEnv          = "KUBERPLOY_ARGO_RUNTIME_CHART_DIGEST"
	ProductionRendererImageEnv        = "KUBERPLOY_ARGO_RENDERER_IMAGE"
	ProductionPollIntervalSecondsEnv  = "KUBERPLOY_ARGO_DESIRED_STATE_POLL_INTERVAL_SECONDS"
	ProductionCatalogMaxAgeSecondsEnv = "KUBERPLOY_ARGO_CATALOG_MAX_AGE_SECONDS"

	productionGitHubAppIDEnv    = "KUBERPLOY_GITHUB_APP_ID"
	productionGitHubClientIDEnv = "KUBERPLOY_GITHUB_APP_CLIENT_ID"
	RuntimeChartName            = "kuberploy-runtime"
)

var productionEnvironmentNames = []string{
	ProductionPlatformBindingIDEnv,
	ProductionNamespaceEnv,
	ProductionChartRepositoryEnv,
	ProductionChartVersionEnv,
	ProductionChartDigestEnv,
	ProductionRendererImageEnv,
	ProductionPollIntervalSecondsEnv,
	ProductionCatalogMaxAgeSecondsEnv,
}

// ProductionRuntimeConfig is the exact default-off contract shared by the API
// readiness probe and the sole production desired-state worker. The GitHub
// client is capped at one-repository read operations plus administration:read
// solely for branch/ruleset observation; it cannot change repository policy.
type ProductionRuntimeConfig struct {
	Enabled           bool
	DesiredState      DesiredStateRuntimeConfig
	GitHub            githubapp.Config
	PollInterval      time.Duration
	MaximumCatalogAge time.Duration
}

func ProductionRuntimeConfigFromEnvironment() (ProductionRuntimeConfig, error) {
	return ProductionRuntimeConfigFromLookup(os.LookupEnv)
}

func ProductionRuntimeConfigFromLookup(lookup func(string) (string, bool)) (ProductionRuntimeConfig, error) {
	if lookup == nil {
		return ProductionRuntimeConfig{}, ErrInvalid
	}
	enabled, present := lookup(ProductionEnabledEnv)
	if !present || enabled == "" || enabled == "false" {
		for _, name := range productionEnvironmentNames {
			if value, found := lookup(name); found && value != "" {
				return ProductionRuntimeConfig{}, errors.New(name + " cannot be configured while protected Argo desired state is disabled")
			}
		}
		return ProductionRuntimeConfig{}, nil
	}
	if enabled != "true" {
		return ProductionRuntimeConfig{}, errors.New(ProductionEnabledEnv + " must be exactly true or false")
	}
	value := func(name string) string {
		raw, found := lookup(name)
		if !found || raw == "" || strings.TrimSpace(raw) != raw {
			return ""
		}
		return raw
	}
	appIDRaw := value(productionGitHubAppIDEnv)
	appID, err := strconv.ParseInt(appIDRaw, 10, 64)
	if err != nil || appID <= 0 || strconv.FormatInt(appID, 10) != appIDRaw {
		return ProductionRuntimeConfig{}, errors.New(productionGitHubAppIDEnv + " must be a canonical positive integer")
	}
	github, err := githubapp.NewProjectedConfig(appID, value(productionGitHubClientIDEnv), githubapp.Permissions{
		"metadata":       githubapp.PermissionRead,
		"contents":       githubapp.PermissionRead,
		"administration": githubapp.PermissionRead,
	})
	if err != nil {
		return ProductionRuntimeConfig{}, err
	}
	platformBindingID := value(ProductionPlatformBindingIDEnv)
	repositorySecretName, err := RepositoryCredentialName(platformBindingID)
	if err != nil {
		return ProductionRuntimeConfig{}, errors.New(ProductionPlatformBindingIDEnv + " must be a canonical UUID")
	}
	poll, err := productionSeconds(value(ProductionPollIntervalSecondsEnv), 1, 60, ProductionPollIntervalSecondsEnv)
	if err != nil {
		return ProductionRuntimeConfig{}, err
	}
	catalogAge, err := productionSeconds(value(ProductionCatalogMaxAgeSecondsEnv), 60, 3600, ProductionCatalogMaxAgeSecondsEnv)
	if err != nil {
		return ProductionRuntimeConfig{}, err
	}
	desired := DesiredStateRuntimeConfig{
		Enabled:              true,
		GitHubAppID:          appID,
		PlatformBindingID:    platformBindingID,
		ClusterID:            value(ProductionClusterIDEnv),
		ArgoNamespace:        value(ProductionNamespaceEnv),
		RootApplicationName:  PlatformRootApplicationName,
		RepositorySecretName: repositorySecretName,
		Runtime: RuntimeLock{
			ChartRepository: value(ProductionChartRepositoryEnv),
			ChartName:       RuntimeChartName,
			ChartVersion:    value(ProductionChartVersionEnv),
			ChartDigest:     value(ProductionChartDigestEnv),
			RendererImage:   value(ProductionRendererImageEnv),
		},
		DigestEnforcement: ChartDigestNativeOCI,
	}
	config := ProductionRuntimeConfig{Enabled: true, DesiredState: desired, GitHub: github, PollInterval: poll, MaximumCatalogAge: catalogAge}
	if config.Validate() != nil {
		return ProductionRuntimeConfig{}, ErrInvalid
	}
	return config, nil
}

func productionSeconds(raw string, minimum, maximum int64, name string) (time.Duration, error) {
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < minimum || value > maximum || strconv.FormatInt(value, 10) != raw {
		return 0, errors.New(name + " must be a canonical bounded integer")
	}
	return time.Duration(value) * time.Second, nil
}

func (c ProductionRuntimeConfig) Validate() error {
	if !c.Enabled {
		if c.DesiredState != (DesiredStateRuntimeConfig{}) || c.GitHub.AppID != 0 || c.PollInterval != 0 || c.MaximumCatalogAge != 0 {
			return ErrInvalid
		}
		return nil
	}
	if c.DesiredState.Validate() != nil || c.DesiredState.RootApplicationName != PlatformRootApplicationName ||
		c.DesiredState.Runtime.ChartName != RuntimeChartName || c.GitHub.Validate() != nil ||
		c.GitHub.AppID != c.DesiredState.GitHubAppID || c.GitHub.MaximumTokenPermissions["metadata"] != githubapp.PermissionRead ||
		c.GitHub.MaximumTokenPermissions["contents"] != githubapp.PermissionRead ||
		c.GitHub.MaximumTokenPermissions["administration"] != githubapp.PermissionRead ||
		len(c.GitHub.MaximumTokenPermissions) != 3 || c.PollInterval < time.Second || c.PollInterval > time.Minute ||
		c.MaximumCatalogAge < time.Minute || c.MaximumCatalogAge > time.Hour {
		return ErrInvalid
	}
	return nil
}
