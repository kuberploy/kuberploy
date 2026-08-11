package imagepull

import (
	"bytes"
	"strings"
	"testing"
)

func TestDockerConfigAcceptsOnlyExactSingleRegistryCredential(t *testing.T) {
	profile := testRuntimeConfig().Profiles[0]
	for name, raw := range map[string]string{
		"auth":                    `{"auths":{"registry.example.test:5000":{"auth":"dXNlcjpwYXNz"}}}`,
		"user password":           `{"auths":{"registry.example.test:5000":{"username":"robot","password":"correct horse battery staple"}}}`,
		"kubectl docker-registry": `{"auths":{"registry.example.test:5000":{"auth":"cm9ib3Q6Y29ycmVjdCBob3JzZSBiYXR0ZXJ5IHN0YXBsZQ==","password":"correct horse battery staple","username":"robot"}}}`,
		"identity token":          `{"auths":{"registry.example.test:5000":{"identitytoken":"opaque-token"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateDockerConfig([]byte(raw), profile); err != nil {
				t.Fatal(err)
			}
		})
	}

	for name, raw := range map[string]string{
		"wrong registry":       `{"auths":{"evil.example.test":{"auth":"private"}}}`,
		"second registry":      `{"auths":{"registry.example.test:5000":{"auth":"private"},"evil.example.test":{"auth":"private"}}}`,
		"duplicate auths":      `{"auths":{"registry.example.test:5000":{"auth":"private"}},"auths":{"registry.example.test:5000":{"auth":"private"}}}`,
		"duplicate auth":       `{"auths":{"registry.example.test:5000":{"auth":"private","auth":"other"}}}`,
		"credential helper":    `{"auths":{"registry.example.test:5000":{"auth":"private"}},"credsStore":"desktop"}`,
		"unknown entry":        `{"auths":{"registry.example.test:5000":{"auth":"private","email":"person@example.test"}}}`,
		"empty entry":          `{"auths":{"registry.example.test:5000":{}}}`,
		"partial basic":        `{"auths":{"registry.example.test:5000":{"username":"robot"}}}`,
		"mixed auth modes":     `{"auths":{"registry.example.test:5000":{"auth":"dXNlcjpwYXNz","identitytoken":"opaque-token"}}}`,
		"mismatched auth copy": `{"auths":{"registry.example.test:5000":{"auth":"dXNlcjpwYXNz","password":"different","username":"user"}}}`,
		"invalid base64 auth":  `{"auths":{"registry.example.test:5000":{"auth":"private"}}}`,
		"empty basic user":     `{"auths":{"registry.example.test:5000":{"auth":"OnBhc3M="}}}`,
		"empty credential":     `{"auths":{"registry.example.test:5000":{"auth":""}}}`,
		"control credential":   `{"auths":{"registry.example.test:5000":{"password":"secret\nvalue","username":"robot"}}}`,
		"escaped tab":          `{"auths":{"registry.example.test:5000":{"identitytoken":"secret\tvalue"}}}`,
		"trailing document":    `{"auths":{"registry.example.test:5000":{"auth":"private"}}}{}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateDockerConfig([]byte(raw), profile); err == nil || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "secret") {
				t.Fatalf("unsafe Docker config result=%v", err)
			}
		})
	}
}

func TestDockerConfigBoundsAndCallerOwnedBytes(t *testing.T) {
	profile := testRuntimeConfig().Profiles[0]
	oversized := bytes.Repeat([]byte{'a'}, MaximumDockerConfigBytes+1)
	if err := ValidateDockerConfig(oversized, profile); err == nil {
		t.Fatal("oversized credential document accepted")
	}
	raw := []byte(`{"auths":{"registry.example.test:5000":{"auth":"dXNlcjpwYXNz"}}}`)
	want := append([]byte(nil), raw...)
	if err := ValidateDockerConfig(raw, profile); err != nil || !bytes.Equal(raw, want) {
		t.Fatalf("validation mutated caller-owned bytes: err=%v", err)
	}
}
