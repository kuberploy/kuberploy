package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/imagepull"
)

type runtimePullStore struct {
	*imagepull.MemoryStore
	closed bool
}

func (s *runtimePullStore) Close() { s.closed = true }

type runtimePullReader struct{}

func (runtimePullReader) ReadDockerConfig(context.Context, imagepull.Profile) ([]byte, error) {
	return nil, imagepull.ErrUnavailable
}

type runtimePullSecretAPI struct{}

func (runtimePullSecretAPI) EnsureImagePullSecret(context.Context, imagepull.SecretRequest) (imagepull.SecretObservation, error) {
	return imagepull.SecretObservation{}, imagepull.ErrUnavailable
}

type runtimePullProjectionInvalidator struct{}

func (runtimePullProjectionInvalidator) InvalidateMatchingProfileMismatch(context.Context, string, string, string, int64, time.Time) (bool, error) {
	return false, nil
}

func workerRuntimePullConfig(t *testing.T) imagepull.RuntimeConfig {
	t.Helper()
	config := imagepull.DefaultRuntimeConfig()
	config.Enabled = true
	config.Namespaces = []string{"runtime-a"}
	config.Profiles = []imagepull.Profile{{Name: "registry-a", TargetID: "11111111-1111-4111-8111-111111111111",
		RegistryServer: "registry.example.test", CredentialRef: "pull/registry-a", Revision: 1,
		SourceSecretRef: "registry-a-pull", SourceSecretKey: ".dockerconfigjson"}}
	if config.Validate() != nil {
		t.Fatal("invalid test config")
	}
	return config
}

func TestRuntimeRegistryPullDefaultsOffWithoutDependencies(t *testing.T) {
	runtime, err := newRuntimeRegistryPullRuntime(t.Context(), "not-a-database-url", "bad host", imagepull.RuntimeConfig{}, nil)
	if err != nil || runtime != nil {
		t.Fatalf("runtime=%#v err=%v", runtime, err)
	}
}

func TestRuntimeRegistryPullConstructionIsExactAndClosesStore(t *testing.T) {
	config := workerRuntimePullConfig(t)
	store := &runtimePullStore{MemoryStore: imagepull.NewMemoryStore()}
	now := time.Date(2026, 8, 9, 4, 30, 0, 0, time.UTC)
	runtime, err := buildRuntimeRegistryPullRuntime(config, store, runtimePullReader{}, runtimePullSecretAPI{}, runtimePullProjectionInvalidator{}, "runtime-pull-worker:test", 1, now)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.controller.Config.Profiles[0].TargetID != config.Profiles[0].TargetID || runtime.controller.WorkerEpoch != 1 {
		t.Fatal("controller lost exact runtime identity")
	}
	runtime.Close()
	if !store.closed {
		t.Fatal("store was not closed")
	}

	if _, err = buildRuntimeRegistryPullRuntime(config, store, nil, runtimePullSecretAPI{}, runtimePullProjectionInvalidator{}, "runtime-pull-worker:test", 1, now); !errors.Is(err, imagepull.ErrUnavailable) {
		t.Fatalf("nil reader = %v", err)
	}
	if _, err = buildRuntimeRegistryPullRuntime(config, store, runtimePullReader{}, runtimePullSecretAPI{}, runtimePullProjectionInvalidator{}, "short", 1, now); !errors.Is(err, imagepull.ErrUnavailable) {
		t.Fatalf("short worker identity = %v", err)
	}
}
