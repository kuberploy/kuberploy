package main

import (
	"errors"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/imagepull"
)

type apiRuntimePullStore struct {
	*imagepull.MemoryStore
	closed bool
}

func (s *apiRuntimePullStore) Close() { s.closed = true }

func apiRuntimePullConfig(t *testing.T) imagepull.RuntimeConfig {
	t.Helper()
	config := imagepull.DefaultRuntimeConfig()
	config.Enabled = true
	config.Namespaces = []string{"runtime-a"}
	config.Profiles = []imagepull.Profile{{Name: "registry-a", TargetID: "11111111-1111-4111-8111-111111111111",
		RegistryServer: "registry.example.test", CredentialRef: "pull/registry-a", Revision: 1,
		SourceSecretRef: "registry-a-pull", SourceSecretKey: ".dockerconfigjson"}}
	if config.Validate() != nil {
		t.Fatal("invalid test configuration")
	}
	return config
}

func TestRuntimeRegistryPullAPIDefaultsOffWithoutDependencies(t *testing.T) {
	api, err := newRuntimeRegistryPullAPI(t.Context(), "not-a-database-url", imagepull.RuntimeConfig{})
	if err != nil || api != nil {
		t.Fatalf("api=%#v err=%v", api, err)
	}
}

func TestRuntimeRegistryPullAPIUsesExactReadinessAndClosesStore(t *testing.T) {
	config := apiRuntimePullConfig(t)
	store := &apiRuntimePullStore{MemoryStore: imagepull.NewMemoryStore()}
	now := time.Date(2026, 8, 9, 5, 0, 0, 0, time.UTC)
	api, err := buildRuntimeRegistryPullAPI(config, store, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = api.readiness.Probe(t.Context()); !errors.Is(err, imagepull.ErrUnavailable) {
		t.Fatalf("readiness without worker = %v", err)
	}
	api.Close()
	if !store.closed {
		t.Fatal("store was not closed")
	}
	if _, err = buildRuntimeRegistryPullAPI(config, nil, now); !errors.Is(err, imagepull.ErrUnavailable) {
		t.Fatalf("nil store = %v", err)
	}
}
