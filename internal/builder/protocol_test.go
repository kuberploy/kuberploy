package builder

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func validBuildRequest() BuildRequest {
	operationID := "11111111-1111-4111-8111-111111111111"
	projectID := "22222222-2222-4222-8222-222222222222"
	serviceID := "33333333-3333-4333-8333-333333333333"
	prefix := "kuberploy"
	server := "registry.example.test"
	buildDefinition := "sha256:" + strings.Repeat("a", 64)
	cacheRepository := server + "/" + prefix + "/projects/" + projectID + "/services/" + serviceID + "/cache/v1/trusted/amd64-arm64/" + strings.Repeat("a", 64)
	return BuildRequest{
		APIVersion:     ProtocolVersion,
		OperationID:    operationID,
		Generation:     2,
		ProjectID:      projectID,
		ServiceID:      serviceID,
		Commit:         strings.Repeat("b", 40),
		ContextPath:    ".",
		DockerfilePath: "Dockerfile",
		Platforms:      []string{"linux/amd64", "linux/arm64"},
		BuildKitImage:  DefaultBuildKitImage,
		Destination: Destination{
			Repository: server + "/" + prefix + "/projects/" + projectID + "/services/" + serviceID + "/image",
			Reference:  "candidate-11111111111141118111111111111111-g2-bbbbbbbbbbbb",
		},
		Registry: RegistryCredentials{
			Server:           server,
			RepositoryPrefix: prefix,
			UsernameFile:     RegistryPushSecretRoot + "/username",
			PasswordFile:     RegistryPushSecretRoot + "/password",
		},
		BuildArgs:   []BuildArg{{Name: "APP_ENV", Value: "production"}},
		SecretFiles: []FileReference{{ID: "npmrc", Path: BuildSecretRoot + "/npmrc"}},
		SSHFiles:    []FileReference{{ID: "default", Path: SSHSecretRoot + "/id_ed25519"}},
		Cache: CachePolicy{
			Schema:          "v1",
			TrustLane:       "trusted",
			BuildDefinition: buildDefinition,
			Imports:         []string{cacheRepository + ":generation-1"},
			CandidateExport: cacheRepository + ":candidate-11111111111141118111111111111111-g2",
			UsernameFile:    RegistryCacheSecretRoot + "/username",
			PasswordFile:    RegistryCacheSecretRoot + "/password",
		},
		Profile: BuildProfile{Resource: "standard", TimeoutSeconds: 900, Egress: "registry-and-source"},
	}
}

func TestDecodeBuildRequestClosedAndBounded(t *testing.T) {
	request := validBuildRequest()
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeBuildRequest(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	if decoded.Commit != request.Commit {
		t.Fatal("commit changed while decoding")
	}
	withUnknown := append(encoded[:len(encoded)-1], []byte(`,"unexpected":true}`)...)
	if _, err := DecodeBuildRequest(bytes.NewReader(withUnknown)); err == nil {
		t.Fatal("unknown field was accepted")
	}
	tooLarge := append(encoded, bytes.Repeat([]byte(" "), MaxRequestBytes)...)
	if _, err := DecodeBuildRequest(bytes.NewReader(tooLarge)); err == nil {
		t.Fatal("oversized request was accepted")
	}
}

func TestBuildRequestRejectsCommitAndPathEscapes(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*BuildRequest)
	}{
		{name: "short commit", mutate: func(r *BuildRequest) { r.Commit = "main" }},
		{name: "uppercase commit", mutate: func(r *BuildRequest) { r.Commit = strings.ToUpper(r.Commit) }},
		{name: "context traversal", mutate: func(r *BuildRequest) { r.ContextPath = "../secrets" }},
		{name: "absolute Dockerfile", mutate: func(r *BuildRequest) { r.DockerfilePath = "/etc/passwd" }},
		{name: "missing BuildKit image", mutate: func(r *BuildRequest) { r.BuildKitImage = "" }},
		{name: "mutable BuildKit image", mutate: func(r *BuildRequest) { r.BuildKitImage = "docker.io/moby/buildkit:latest" }},
		{name: "wrong BuildKit version", mutate: func(r *BuildRequest) { r.BuildKitImage = "docker.io/moby/buildkit:v0.32.1" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := validBuildRequest()
			test.mutate(&request)
			if err := request.Validate(); err == nil {
				t.Fatal("unsafe request was accepted")
			}
		})
	}
}

func TestBuildRequestAcceptsBuildKitDigestMirror(t *testing.T) {
	request := validBuildRequest()
	request.BuildKitImage = "registry.example.test/mirror/buildkit@sha256:" + strings.Repeat("a", 64)
	if err := request.Validate(); err != nil {
		t.Fatalf("digest BuildKit mirror rejected: %v", err)
	}
}

func TestBuildRequestRejectsCacheExfiltration(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*BuildRequest)
	}{
		{name: "attacker destination", mutate: func(r *BuildRequest) { r.Destination.Repository = "attacker.test/stolen/image" }},
		{name: "latest destination", mutate: func(r *BuildRequest) { r.Destination.Reference = "latest" }},
		{name: "cross operation destination", mutate: func(r *BuildRequest) {
			r.Destination.Reference = "candidate-99999999999949998999999999999999-g2-bbbbbbbbbbbb"
		}},
		{name: "attacker cache registry", mutate: func(r *BuildRequest) { r.Cache.Imports[0] = "attacker.test/cache/stolen:generation-1" }},
		{name: "cross service cache", mutate: func(r *BuildRequest) {
			r.Cache.Imports[0] = strings.Replace(r.Cache.Imports[0], r.ServiceID, "44444444-4444-4444-8444-444444444444", 1)
		}},
		{name: "wrong trust lane", mutate: func(r *BuildRequest) {
			r.Cache.Imports[0] = strings.Replace(r.Cache.Imports[0], "/trusted/", "/untrusted/", 1)
		}},
		{name: "reused candidate", mutate: func(r *BuildRequest) {
			r.Cache.CandidateExport = strings.Replace(r.Cache.CandidateExport, "-g2", "-g1", 1)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := validBuildRequest()
			test.mutate(&request)
			if err := request.Validate(); err == nil {
				t.Fatal("cache exfiltration request was accepted")
			}
		})
	}
}

func TestBuildRequestRejectsCredentialAuthoritySubstitution(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*BuildRequest)
	}{
		{name: "push uses cache username", mutate: func(r *BuildRequest) { r.Registry.UsernameFile = r.Cache.UsernameFile }},
		{name: "push uses cache password", mutate: func(r *BuildRequest) { r.Registry.PasswordFile = r.Cache.PasswordFile }},
		{name: "cache uses push username", mutate: func(r *BuildRequest) { r.Cache.UsernameFile = r.Registry.UsernameFile }},
		{name: "cache uses push password", mutate: func(r *BuildRequest) { r.Cache.PasswordFile = r.Registry.PasswordFile }},
		{name: "cache path traverses to push root", mutate: func(r *BuildRequest) {
			r.Cache.UsernameFile = RegistryCacheSecretRoot + "/../registry-push/username"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := validBuildRequest()
			test.mutate(&request)
			if err := request.Validate(); err == nil {
				t.Fatal("credential authority substitution was accepted")
			}
		})
	}
}

func TestBuildRequestAcceptsValidBuildArgWithoutEnforcingNamingPolicy(t *testing.T) {
	request := validBuildRequest()
	request.BuildArgs = []BuildArg{{Name: "API_TOKEN", Value: "caller-selected-value"}}
	if err := request.Validate(); err != nil {
		t.Fatalf("valid Docker build argument was rejected: %v", err)
	}
}

func TestSensitiveBuildArgWarningUsesNamesOnlyAndNeverBlocks(t *testing.T) {
	secretValue := "caller-selected-secret-value"
	request := validBuildRequest()
	request.BuildArgs = []BuildArg{
		{Name: "API_TOKEN", Value: secretValue},
		{Name: "PUBLIC_VALUE", Value: "ordinary"},
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("sensitive-looking build argument was blocked: %v", err)
	}
	if !containsSensitiveBuildArgName(request.BuildArgs) {
		t.Fatal("sensitive-looking build argument did not produce a warning signal")
	}
	if containsSensitiveBuildArgName([]BuildArg{{Name: "PUBLIC_VALUE", Value: secretValue}}) {
		t.Fatal("ordinary build argument produced a sensitive warning")
	}
	if !containsSensitiveBuildArgName([]BuildArg{{Name: "DATABASE_PASSWORDS", Value: secretValue}}) {
		t.Fatal("plural sensitive build argument name did not produce a warning")
	}
}
