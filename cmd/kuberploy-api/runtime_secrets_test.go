package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/secrets"
)

type apiRuntimeSecretStore struct {
	*secrets.MemoryStore
	closed bool
}

func (s *apiRuntimeSecretStore) Close() { s.closed = true }

type apiRuntimeSecretKeys struct{}

func (apiRuntimeSecretKeys) ActiveKey(context.Context) (secrets.FingerprintKey, error) {
	return secrets.FingerprintKey{ID: secrets.DefaultFingerprintKeyID, Bytes: []byte("0123456789abcdef0123456789abcdef")}, nil
}

type apiRuntimeSecretProvider struct{}

func (apiRuntimeSecretProvider) StageStrictSealedSecret(context.Context, secrets.StageRequest, *secrets.Material) (secrets.Artifact, error) {
	return secrets.Artifact{}, secrets.ErrProviderOperation
}
func (apiRuntimeSecretProvider) ObserveStrictSealedSecret(context.Context, secrets.Artifact) (secrets.ReadinessObservation, error) {
	return secrets.ReadinessObservation{}, secrets.ErrProviderOperation
}
func (apiRuntimeSecretProvider) DeleteStrictSealedSecret(context.Context, secrets.Artifact) (secrets.DeleteObservation, error) {
	return secrets.DeleteObservation{}, secrets.ErrProviderOperation
}

func apiRuntimeSecretConfig(t *testing.T) secrets.RuntimeConfig {
	t.Helper()
	config := secrets.DefaultRuntimeConfig()
	config.Enabled = true
	config.Namespaces = []string{"apps-production"}
	config.FingerprintSecretRef = "kuberploy-runtime-secret-fingerprint"
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	return config
}

func TestRuntimeSecretAPIDefaultsOffWithoutOpeningDependencies(t *testing.T) {
	api, err := newRuntimeSecretAPI(t.Context(), "not-a-database-url", secrets.RuntimeConfig{})
	if err != nil || api != nil {
		t.Fatalf("api=%#v err=%v", api, err)
	}
}

func TestRuntimeSecretAPIRequiresBothProjectedFilesAndFreshExactWorker(t *testing.T) {
	config := apiRuntimeSecretConfig(t)
	store := &apiRuntimeSecretStore{MemoryStore: secrets.NewMemoryStore()}
	identity, err := secrets.RuntimeIdentityForConfig(config, "sha256:"+strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	resolve := func(context.Context, secrets.RuntimeConfig, time.Time) (secrets.RuntimeIdentity, error) {
		return identity, nil
	}
	api, err := buildRuntimeSecretAPI(t.Context(), config, store, apiRuntimeSecretProvider{}, apiRuntimeSecretKeys{}, resolve, time.Now().UTC())
	if err != nil || api.backend == nil || api.readiness == nil {
		t.Fatalf("api=%#v err=%v", api, err)
	}
	if err = api.readiness.Probe(t.Context()); !errors.Is(err, secrets.ErrRuntimeUnavailable) {
		t.Fatalf("unobserved worker probe=%v", err)
	}
	now := time.Now().UTC()
	if _, err = store.AcquireRuntimeSecretReadiness(t.Context(), secrets.RuntimeWorkerObservation{
		WorkerID: "runtime-secrets-worker:test-api", Identity: identity, StartedAt: now, ObservedAt: now,
	}, secrets.RuntimeSecretReadinessLease); err != nil {
		t.Fatal(err)
	}
	if err = api.readiness.Probe(t.Context()); err != nil {
		t.Fatalf("fresh worker probe=%v", err)
	}
	api.Close()
	if !store.closed {
		t.Fatal("runtime-secret API store was not closed")
	}
}

func TestRuntimeSecretAPIRejectsProjectionFailureBeforeBackendConstruction(t *testing.T) {
	config := apiRuntimeSecretConfig(t)
	store := &apiRuntimeSecretStore{MemoryStore: secrets.NewMemoryStore()}
	resolve := func(context.Context, secrets.RuntimeConfig, time.Time) (secrets.RuntimeIdentity, error) {
		return secrets.RuntimeIdentity{}, errors.New("certificate unavailable")
	}
	api, err := buildRuntimeSecretAPI(t.Context(), config, store, apiRuntimeSecretProvider{}, apiRuntimeSecretKeys{}, resolve, time.Now().UTC())
	if api != nil || !errors.Is(err, secrets.ErrRuntimeUnavailable) {
		t.Fatalf("api=%#v err=%v", api, err)
	}
}
