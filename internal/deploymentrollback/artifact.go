package deploymentrollback

import "strings"

// MatchesRegistryArtifact compares one immutable deployment image with the
// server/repository/digest tuple stored by the registry release catalog.
func MatchesRegistryArtifact(image, endpoint, repository, digest string) bool {
	endpoint = strings.TrimSuffix(strings.TrimSpace(endpoint), "/")
	repository = strings.Trim(strings.TrimSpace(repository), "/")
	if !imageRE.MatchString(image) || endpoint == "" || repository == "" || !strings.HasPrefix(digest, "sha256:") {
		return false
	}
	return image == endpoint+"/"+repository+"@"+digest
}
