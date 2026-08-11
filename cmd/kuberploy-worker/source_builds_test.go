package main

import (
	"context"
	"strings"
	"testing"

	"github.com/kuberploy/kuberploy/internal/builds"
)

func TestNewSourceBuildRuntimeDisabledDoesNotOpenDependencies(t *testing.T) {
	runtime, err := newSourceBuildRuntime(context.Background(), "not-a-database-url", "", builds.WorkerRuntimeConfig{}, nil)
	if err != nil || runtime != nil {
		t.Fatalf("disabled runtime=%#v err=%v", runtime, err)
	}
}

func TestWorkerLeaseOwnersAreOpaqueDistinctAndDeterministic(t *testing.T) {
	deliveries := workerLeaseOwner("pod-name/123", "deliveries")
	builds := workerLeaseOwner("pod-name/123", "builds")
	projection := workerLeaseOwner("pod-name/123", "git-projection")
	releases := workerLeaseOwner("pod-name/123", "release-projection")
	if deliveries == builds || deliveries == projection || deliveries == releases || builds == projection || builds == releases || projection == releases || deliveries != workerLeaseOwner("pod-name/123", "deliveries") {
		t.Fatalf("owners are not distinct and deterministic: %q %q %q %q", deliveries, builds, projection, releases)
	}
	if strings.Contains(deliveries, "pod-name") || len(deliveries) > 128 || len(projection) > 128 || len(releases) > 128 || strings.ContainsAny(deliveries+builds+projection+releases, "\x00\r\n") {
		t.Fatalf("unsafe owner identities: %q %q %q %q", deliveries, builds, projection, releases)
	}
}

func TestWorkerLeaseOwnerSeparatesProcessStartsInSamePod(t *testing.T) {
	first := workerLeaseOwner("pod-name/1/2026-08-11T01:00:00Z", "environment-foundation")
	second := workerLeaseOwner("pod-name/1/2026-08-11T01:00:01Z", "environment-foundation")
	if first == second {
		t.Fatalf("process restart reused readiness identity %q", first)
	}
}
