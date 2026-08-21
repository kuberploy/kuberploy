package deploymentrollback

import (
	"strings"
	"testing"
)

func TestMatchesRegistryArtifactNormalizesCanonicalRegistryOrigin(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	image := "registry.example.test:5443/team/api@" + digest
	for _, endpoint := range []string{
		"registry.example.test:5443",
		"https://registry.example.test:5443",
		"http://registry.example.test:5443",
		"https://registry.example.test:5443/",
	} {
		if !MatchesRegistryArtifact(image, endpoint, "/team/api/", digest) {
			t.Fatalf("canonical endpoint %q did not match OCI image", endpoint)
		}
	}
}

func TestMatchesRegistryArtifactRejectsEndpointOrIdentitySubstitution(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	image := "registry.example.test/team/api@" + digest
	for _, endpoint := range []string{
		"ftp://registry.example.test",
		"https://user:pass@registry.example.test",
		"https://registry.example.test/path",
		"registry.example.test/other",
	} {
		if MatchesRegistryArtifact(image, endpoint, "team/api", digest) {
			t.Fatalf("unsafe endpoint %q matched OCI image", endpoint)
		}
	}
	if MatchesRegistryArtifact(image, "https://registry.example.test", "team/other", digest) ||
		MatchesRegistryArtifact(image, "https://other.example.test", "team/api", digest) ||
		MatchesRegistryArtifact(image, "https://registry.example.test", "team/api", "sha256:"+strings.Repeat("c", 64)) {
		t.Fatal("registry artifact identity substitution matched")
	}
}
