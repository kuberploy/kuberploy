package main

import (
	"context"
	"testing"

	"github.com/kuberploy/kuberploy/internal/argo"
)

func TestArgoObservationRuntimeDefaultsOffWithoutOpeningDependencies(t *testing.T) {
	runtime, err := newArgoObservationRuntime(context.Background(), "not-a-database-url", "test-host", argo.ObservationRuntimeConfig{})
	if err != nil || runtime != nil {
		t.Fatalf("runtime=%#v err=%v", runtime, err)
	}
}
