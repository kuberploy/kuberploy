package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/secrets"
)

type workerRuntimeSecretStore struct {
	*secrets.MemoryStore
	closed bool
}

func (s *workerRuntimeSecretStore) Close() { s.closed = true }

type workerRuntimeSecretObserver struct{}

func (workerRuntimeSecretObserver) ObserveStrictSealedSecret(context.Context, secrets.Artifact) (secrets.ReadinessObservation, error) {
	return secrets.ReadinessObservation{}, secrets.ErrProviderOperation
}

func workerRuntimeSecretConfig(t *testing.T) secrets.RuntimeConfig {
	t.Helper()
	config := secrets.DefaultRuntimeConfig()
	config.Enabled = true
	config.Namespaces = []string{"apps-production"}
	config.FingerprintSecretRef = "kuberploy-runtime-secret-fingerprint"
	config.FingerprintSecretKey = secrets.DefaultFingerprintSecretKey
	config.FingerprintKeyID = secrets.DefaultFingerprintKeyID
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	return config
}

func TestRuntimeSecretWorkerDefaultsOffWithoutOpeningDependencies(t *testing.T) {
	runtime, err := newRuntimeSecretRuntime(t.Context(), "not-a-database-url", "bad host", secrets.RuntimeConfig{})
	if err != nil || runtime != nil {
		t.Fatalf("runtime=%#v err=%v", runtime, err)
	}
}

func TestRuntimeSecretWorkerValidatesExactIdentityBeforeReadiness(t *testing.T) {
	config := workerRuntimeSecretConfig(t)
	store := &workerRuntimeSecretStore{MemoryStore: secrets.NewMemoryStore()}
	fingerprint := "sha256:" + strings.Repeat("a", 64)
	identity, err := secrets.RuntimeIdentityForConfig(config, fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	resolveCalls := 0
	resolve := func(context.Context, secrets.RuntimeConfig, time.Time) (secrets.RuntimeIdentity, error) {
		resolveCalls++
		if resolveCalls > 1 {
			return secrets.RuntimeIdentity{}, errors.New("projected certificate changed")
		}
		return identity, nil
	}
	runtime, err := buildRuntimeSecretRuntime(t.Context(), "worker-pod-0", config, store, workerRuntimeSecretObserver{}, resolve, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err = runtime.Run(t.Context()); !errors.Is(err, secrets.ErrRuntimeUnavailable) {
		t.Fatalf("run error=%v", err)
	}
	if resolveCalls != 2 {
		t.Fatalf("identity resolutions=%d, want startup and pre-readiness", resolveCalls)
	}
	if err = store.RuntimeSecretReady(t.Context(), identity, time.Now().UTC(), secrets.RuntimeSecretHeartbeatMaxAge); !errors.Is(err, secrets.ErrRuntimeUnavailable) {
		t.Fatalf("invalid worker published readiness: %v", err)
	}
	runtime.Close()
	if !store.closed {
		t.Fatal("runtime store was not closed")
	}
}
