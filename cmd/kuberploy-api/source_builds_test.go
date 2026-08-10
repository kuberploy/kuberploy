package main

import (
	"context"
	"testing"

	"github.com/kuberploy/kuberploy/internal/builds"
)

func TestSourceBuildAPIDefaultsOffWithoutOpeningDependencies(t *testing.T) {
	runtime, err := newSourceBuildAPI(context.Background(), "not-a-database-url", "http://local.invalid", "", builds.WorkerRuntimeConfig{}, nil)
	if err != nil || runtime != nil {
		t.Fatalf("runtime=%#v err=%v", runtime, err)
	}
}

func TestSourceBuildPublicURLRequiresExactHTTPSOrigin(t *testing.T) {
	for _, valid := range []string{"https://kuberploy.example.test", "https://kuberploy.example.test:8443/"} {
		if value, err := sourceBuildPublicURL(valid); err != nil || value == "" {
			t.Fatalf("valid origin %q rejected: %q %v", valid, value, err)
		}
	}
	for _, invalid := range []string{"", "http://kuberploy.example.test", "https://user@kuberploy.example.test", "https://kuberploy.example.test/base", "https://kuberploy.example.test?x=1", " https://kuberploy.example.test", "https://:443", "https://kuberploy.example.test:0", "https://kuberploy.example.test:99999"} {
		if value, err := sourceBuildPublicURL(invalid); err == nil || value != "" {
			t.Fatalf("invalid origin %q accepted: %q %v", invalid, value, err)
		}
	}
}
