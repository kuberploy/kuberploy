package main

import (
	"errors"
	"os"

	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/httpapi"
)

const (
	platformBindingIDEnv = "KUBERPLOY_PLATFORM_GIT_BINDING_ID"
)

func platformGitBindingConfigFromEnvironment(runtime gitprojection.RuntimeConfig) (httpapi.PlatformGitBindingConfig, error) {
	return platformGitBindingConfigFromLookup(os.LookupEnv, runtime)
}

func platformGitBindingConfigFromLookup(lookup func(string) (string, bool), runtime gitprojection.RuntimeConfig) (httpapi.PlatformGitBindingConfig, error) {
	if lookup == nil {
		return httpapi.PlatformGitBindingConfig{}, errors.New("platform Git binding configuration lookup is unavailable")
	}
	bindingID, bindingPresent := lookup(platformBindingIDEnv)
	if !bindingPresent {
		return httpapi.PlatformGitBindingConfig{}, nil
	}
	identityProbe := httpapi.PlatformGitBindingConfig{Enabled: true, BindingID: bindingID, GitHubAppID: 1}
	if identityProbe.Validate() != nil {
		return httpapi.PlatformGitBindingConfig{}, errors.New("platform Git binding ID must be an exact canonical UUID")
	}
	if !runtime.Enabled {
		return httpapi.PlatformGitBindingConfig{}, nil
	}
	if runtime.Validate() != nil {
		return httpapi.PlatformGitBindingConfig{}, errors.New("platform Git binding requires a valid enabled Git projection runtime")
	}
	result := httpapi.PlatformGitBindingConfig{Enabled: true, BindingID: bindingID, GitHubAppID: runtime.GitHub.AppID}
	if result.Validate() != nil {
		return httpapi.PlatformGitBindingConfig{}, errors.New("platform Git binding operator identity is invalid")
	}
	return result, nil
}
