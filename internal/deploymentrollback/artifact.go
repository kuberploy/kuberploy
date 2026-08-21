package deploymentrollback

import (
	"net/url"
	"strings"
)

// MatchesRegistryArtifact compares one immutable deployment image with the
// server/repository/digest tuple stored by the registry release catalog.
func MatchesRegistryArtifact(image, endpoint, repository, digest string) bool {
	endpoint, ok := registryImageHost(endpoint)
	repository = strings.Trim(strings.TrimSpace(repository), "/")
	if !ok || !imageRE.MatchString(image) || repository == "" || !strings.HasPrefix(digest, "sha256:") {
		return false
	}
	return image == endpoint+"/"+repository+"@"+digest
}

// Registry targets are canonical HTTP origins, while OCI image references use
// only the registry host (and optional port). Older external targets may still
// contain the already scheme-free host form, so both representations remain
// valid comparison inputs.
func registryImageHost(endpoint string) (string, bool) {
	endpoint = strings.TrimSuffix(strings.TrimSpace(endpoint), "/")
	if endpoint == "" {
		return "", false
	}
	if !strings.Contains(endpoint, "://") {
		return endpoint, !strings.ContainsAny(endpoint, "/?#@")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil ||
		parsed.Opaque != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" {
		return "", false
	}
	return parsed.Host, true
}
