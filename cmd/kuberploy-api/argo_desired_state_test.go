package main

import (
	"testing"

	"github.com/kuberploy/kuberploy/internal/argo"
)

func TestNewArgoDesiredStateAPIIsStrictlyDefaultOff(t *testing.T) {
	runtime, err := newArgoDesiredStateAPI(t.Context(), "not-a-database-url", argo.ProductionRuntimeConfig{})
	if err != nil || runtime != nil {
		t.Fatalf("runtime=%v err=%v", runtime, err)
	}
}
