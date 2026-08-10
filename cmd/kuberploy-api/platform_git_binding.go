package main

import (
	"errors"
	"os"

	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/httpapi"
)

const platformClusterIDEnv = "KUBERPLOY_CLUSTER_ID"

func platformGitBindingConfigFromEnvironment(runtime gitprojection.RuntimeConfig) (httpapi.PlatformGitBindingConfig, error) {
	return platformGitBindingConfigFromLookup(os.LookupEnv, runtime)
}

func platformGitBindingConfigFromLookup(lookup func(string) (string, bool), runtime gitprojection.RuntimeConfig) (httpapi.PlatformGitBindingConfig, error) {
	if lookup == nil {
		return httpapi.PlatformGitBindingConfig{}, errors.New("platform Git binding configuration lookup is unavailable")
	}
	clusterID, present := lookup(platformClusterIDEnv)
	if !present {
		return httpapi.PlatformGitBindingConfig{}, nil
	}
	// Validate the cluster independently even when Git projection is disabled.
	// This makes a present-but-empty, noncanonical, or whitespace value an
	// operator error rather than silently treating it as an absent setting.
	clusterProbe := httpapi.PlatformGitBindingConfig{Enabled: true, ClusterID: clusterID, GitHubAppID: 1}
	if clusterProbe.Validate() != nil {
		return httpapi.PlatformGitBindingConfig{}, errors.New(platformClusterIDEnv + " must be an exact canonical UUID")
	}
	if !runtime.Enabled {
		return httpapi.PlatformGitBindingConfig{}, nil
	}
	if runtime.Validate() != nil {
		return httpapi.PlatformGitBindingConfig{}, errors.New("platform Git binding requires a valid enabled Git projection runtime")
	}
	result := httpapi.PlatformGitBindingConfig{Enabled: true, ClusterID: clusterID, GitHubAppID: runtime.GitHub.AppID}
	if result.Validate() != nil {
		return httpapi.PlatformGitBindingConfig{}, errors.New("platform Git binding operator identity is invalid")
	}
	return result, nil
}
