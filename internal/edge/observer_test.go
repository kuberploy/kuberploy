package edge

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestKubernetesObserverExactProfiles(t *testing.T) {
	config := testRuntimeConfig()
	reader := newFakeKubernetesReader(config)
	observer := &KubernetesTargetObserver{Reader: reader, CallTimeout: time.Second}
	cases := []struct {
		name string
		call func() (ObservationReceipt, error)
		key  string
	}{
		{"traefik", func() (ObservationReceipt, error) {
			return observer.ObserveTraefik(context.Background(), *config.Profiles.Traefik)
		}, "traefik"},
		{"cert-manager", func() (ObservationReceipt, error) {
			return observer.ObserveCertManager(context.Background(), *config.Profiles.CertManager)
		}, "cert-manager"},
		{"external-dns", func() (ObservationReceipt, error) {
			return observer.ObserveExternalDNS(context.Background(), config.Profiles.ExternalDNS[0])
		}, "external-dns/" + testExternalDNSIntegrationID},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			receipt, err := item.call()
			if err != nil || receipt.TargetKey != item.key || !validDigest(receipt.IdentityDigest) || !validDigest(receipt.ResourceVersionDigest) {
				t.Fatalf("exact observation failed: %#v %v", receipt, err)
			}
		})
	}
}

func TestKubernetesObserverRejectsProfileArgumentsAndIssuerDrift(t *testing.T) {
	config := testRuntimeConfig()
	reader := newFakeKubernetesReader(config)
	observer := &KubernetesTargetObserver{Reader: reader, CallTimeout: time.Second}
	external := config.Profiles.ExternalDNS[0]
	deploymentKey := external.Namespace + "/" + external.Deployment.Name + "/" + external.Deployment.ContainerName
	value := reader.deployments[deploymentKey]
	value.ContainerArguments = append(value.ContainerArguments, "--policy=sync")
	reader.deployments[deploymentKey] = value
	if _, err := observer.ObserveExternalDNS(context.Background(), external); !errors.Is(err, ErrObservation) {
		t.Fatalf("duplicate TXT/policy argument accepted: %v", err)
	}

	issuerName := config.Profiles.CertManager.ProductionIssuer
	issuer := reader.issuers[issuerName]
	issuer.ObservedGeneration = 0
	reader.issuers[issuerName] = issuer
	if _, err := observer.ObserveCertManager(context.Background(), *config.Profiles.CertManager); !errors.Is(err, ErrObservation) {
		t.Fatalf("unobserved approved issuer accepted: %v", err)
	}

	traefik := config.Profiles.Traefik
	profileKey := traefik.Namespace + "/" + traefik.ProfileConfigMap
	profile := reader.configMaps[profileKey]
	profile.Data["management"] = "adopted"
	reader.configMaps[profileKey] = profile
	if _, err := observer.ObserveTraefik(context.Background(), *traefik); !errors.Is(err, ErrObservation) {
		t.Fatalf("profile ConfigMap drift accepted: %v", err)
	}
}

func TestControllerRecordsObservationMismatchWithoutClaimingReadiness(t *testing.T) {
	ctx := context.Background()
	config := testRuntimeConfig()
	digest, _ := config.Digest()
	targets, _ := config.DesiredTargets()
	now := time.Date(2026, 8, 9, 15, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	if err := store.SynchronizeTargets(ctx, digest, targets, now); err != nil {
		t.Fatal(err)
	}
	reader := newFakeKubernetesReader(config)
	reader.err = ErrNotFound
	controller := &RuntimeController{Store: store, Observer: &KubernetesTargetObserver{Reader: reader, CallTimeout: time.Second},
		Config: config, WorkerID: testWorkerID, WorkerEpoch: 1, Now: func() time.Time { return now }}
	worked, err := controller.Reconcile(ctx, digest)
	if err != nil || !worked {
		t.Fatalf("semantic observation mismatch was not durably retried: worked=%v err=%v", worked, err)
	}
	failed := 0
	for _, desired := range targets {
		target, getErr := store.Target(ctx, desired.Key, desired.Revision)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if target.ConsecutiveFailures == 1 && target.LastFailureCode == "resource-not-found" && target.State == StateAwaiting {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("expected exactly one fenced retry, got %d", failed)
	}
	if err = store.RuntimeReady(ctx, RuntimeContract, digest, len(targets), now, config.ReadinessMaxAge); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unobserved edge target became ready: %v", err)
	}
}

func TestControllerReconcilesEveryExactProfileBeforeReadiness(t *testing.T) {
	ctx := context.Background()
	config := testRuntimeConfig()
	digest, _ := config.Digest()
	targets, _ := config.DesiredTargets()
	now := time.Date(2026, 8, 9, 16, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	if err := store.SynchronizeTargets(ctx, digest, targets, now); err != nil {
		t.Fatal(err)
	}
	controller := &RuntimeController{Store: store,
		Observer: &KubernetesTargetObserver{Reader: newFakeKubernetesReader(config), CallTimeout: time.Second},
		Config:   config, WorkerID: testWorkerID, WorkerEpoch: 1, Now: func() time.Time { return now }}
	for range targets {
		worked, err := controller.Reconcile(ctx, digest)
		if err != nil || !worked {
			t.Fatalf("exact profile reconciliation failed: worked=%v err=%v", worked, err)
		}
	}
	if worked, err := controller.Reconcile(ctx, digest); err != nil || worked {
		t.Fatalf("unexpected extra due target: worked=%v err=%v", worked, err)
	}
	for _, desired := range targets {
		target, err := store.Target(ctx, desired.Key, desired.Revision)
		if err != nil || target.State != StateReady || target.LastObservedAt == nil || target.ObservedIdentityDigest == "" {
			t.Fatalf("target %s not exactly observed: %#v %v", desired.Key, target, err)
		}
	}
	readiness := Readiness{WorkerID: testWorkerID, WorkerEpoch: 1, Contract: RuntimeContract, ConfigDigest: digest,
		TargetCount: len(targets), StartedAt: now, ObservedAt: now, LeaseUntil: now.Add(config.ReadinessMaxAge)}
	if err := store.RecordReadiness(ctx, readiness); err != nil {
		t.Fatal(err)
	}
	probe := &RuntimeReadinessProbe{Store: store, Config: config, Now: func() time.Time { return now }}
	if err := probe.Probe(ctx); err != nil {
		t.Fatalf("exact fully observed runtime did not become ready: %v", err)
	}
}
