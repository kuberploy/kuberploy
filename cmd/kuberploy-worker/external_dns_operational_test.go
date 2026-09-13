package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/edge"
	"github.com/kuberploy/kuberploy/internal/externaldns"
	"github.com/kuberploy/kuberploy/internal/githubapp"
)

type failingExternalDNSRuntimeSource struct {
	externalDNSRecoverySource
	err   error
	mu    sync.Mutex
	calls []time.Time
}

func (s *failingExternalDNSRuntimeSource) ListExternalDNSIntegrationsForRuntime(context.Context, int) ([]domain.ExternalDNSIntegration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, time.Now())
	return nil, s.err
}

func (s *failingExternalDNSRuntimeSource) attempts() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.calls...)
}

func TestExternalDNSRunHonorsProviderRetryAndCancellation(t *testing.T) {
	const poll = 5 * time.Second
	for _, test := range []struct {
		name  string
		class githubapp.APIErrorClass
		hint  time.Duration
		want  time.Duration
	}{
		{name: "primary rate reset", class: githubapp.APIErrorRateLimit, hint: 12 * time.Minute, want: 12 * time.Minute},
		{name: "transient retry after", class: githubapp.APIErrorTransient, hint: time.Minute, want: time.Minute},
		{name: "expired retry hint", class: githubapp.APIErrorRateLimit, hint: -time.Second, want: poll},
		{name: "minimum poll", class: githubapp.APIErrorRateLimit, hint: time.Second, want: poll},
		{name: "non retryable", class: githubapp.APIErrorForbidden, hint: time.Hour, want: poll},
		{name: "bounded retry hint", class: githubapp.APIErrorRateLimit, hint: 8 * 24 * time.Hour, want: 7 * 24 * time.Hour},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				started := time.Now()
				source := &failingExternalDNSRuntimeSource{err: fmt.Errorf("provider head: %w", &githubapp.APIError{Class: test.class, RetryAt: started.Add(test.hint)})}
				runtime := &externalDNSOperationalRuntime{source: source, config: externaldns.OperationalConfig{PollInterval: poll}}
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan error, 1)
				go func() { done <- runtime.Run(ctx) }()
				defer func() {
					cancel()
					if err := <-done; !errors.Is(err, context.Canceled) {
						t.Errorf("cancelled provider wait: %v", err)
					}
				}()
				synctest.Wait()
				if calls := source.attempts(); len(calls) != 1 {
					t.Fatalf("initial provider attempts=%d", len(calls))
				}
				firstWindow := min(test.want-time.Nanosecond, poll+time.Nanosecond)
				time.Sleep(firstWindow)
				synctest.Wait()
				if calls := source.attempts(); len(calls) != 1 {
					t.Fatalf("provider retried before permitted delay %s: attempts=%d", test.want, len(calls))
				}
				time.Sleep(test.want - time.Nanosecond - firstWindow)
				synctest.Wait()
				if calls := source.attempts(); len(calls) != 1 {
					t.Fatalf("provider retried before the exact retry time: attempts=%d", len(calls))
				}
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				if calls := source.attempts(); len(calls) != 2 || calls[1].Sub(started) != test.want {
					t.Fatalf("retry attempts=%v want second call after %s", calls, test.want)
				}
			})
		})
	}
}

type externalDNSRecoverySource struct {
	item     domain.ExternalDNSIntegration
	advances int
}

func (s *externalDNSRecoverySource) ListExternalDNSIntegrationsForRuntime(context.Context, int) ([]domain.ExternalDNSIntegration, error) {
	return []domain.ExternalDNSIntegration{s.item}, nil
}

func (s *externalDNSRecoverySource) AdvanceExternalDNSRuntimeRevision(_ context.Context, id string, revision int64, digest string, _ time.Time) error {
	if id != s.item.ID || revision != s.item.RuntimeRevision || digest != s.item.ProtectedGitContentDigest {
		return errors.New("unexpected runtime revision advance")
	}
	s.advances++
	return nil
}

func (*externalDNSRecoverySource) RecordExternalDNSPublication(context.Context, string, int64, bool, string, string, time.Time) error {
	return nil
}

func workerExternalDNSRuntimeTemplate() externaldns.ManagedRuntimeTemplate {
	return externaldns.ManagedRuntimeTemplate{
		Namespace: "kuberploy-dns", Version: "v0.18.0",
		Image:          "registry.k8s.io/external-dns/external-dns@sha256:" + strings.Repeat("a", 64),
		ServiceAccount: "external-dns-managed",
	}
}

func workerExternalDNSIntegration() domain.ExternalDNSIntegration {
	return domain.ExternalDNSIntegration{
		ID: "11111111-1111-4111-8111-111111111111", Slug: "primary", Name: "Primary",
		Mode: externaldns.ModeManaged, ProviderKind: "cloudflare", TXTOwnerID: "kuberploy.primary",
		AllowedDomainSuffixes: []string{"example.com"}, SyncPolicy: externaldns.SyncPolicyUpsert,
		CredentialSecretRef: "cloudflare-credentials", ProviderConfigRef: "cloudflare-provider",
		EgressConfigRef: "cloudflare-egress", EnvironmentIDs: []string{"22222222-2222-4222-8222-222222222222"},
		RuntimeRevision: 3, Lifecycle: "active",
	}
}

func TestExternalDNSPublicationNeededOnlyForUnmaterializedOrChangedContent(t *testing.T) {
	template := workerExternalDNSRuntimeTemplate()
	item := workerExternalDNSIntegration()
	item.ProtectedGitState = "materialized"
	item.ProtectedGitRevision = item.RuntimeRevision
	item.ProtectedGitCommit = strings.Repeat("c", 40)
	digest, err := externaldns.ManagedBundleDigest(item, template)
	if err != nil {
		t.Fatal(err)
	}
	item.ProtectedGitContentDigest = digest
	if externalDNSPublicationNeeded(item, template) {
		t.Fatal("unchanged materialized integration should not republish")
	}
	if externalDNSRuntimeRevisionAdvanceNeeded(item, template) {
		t.Fatal("unchanged materialized integration should not advance runtime revision")
	}
	item.ProtectedGitContentDigest = "sha256:" + strings.Repeat("d", 64)
	if !externalDNSRuntimeRevisionAdvanceNeeded(item, template) {
		t.Fatal("changed managed bundle must advance runtime revision before republishing")
	}

	item.RuntimeRevision++
	if !externalDNSPublicationNeeded(item, template) {
		t.Fatal("runtime revision change must republish")
	}
	item = workerExternalDNSIntegration()
	item.ProtectedGitState = "pending"
	if !externalDNSPublicationNeeded(item, template) {
		t.Fatal("pending integration must publish")
	}
}

func TestExternalDNSProfilesSortByIntegrationID(t *testing.T) {
	profiles := []edge.ExternalDNSProfile{
		{IntegrationID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc"},
		{IntegrationID: "88888888-8888-4888-8888-888888888888"},
	}
	sortExternalDNSProfiles(profiles)
	if profiles[0].IntegrationID >= profiles[1].IntegrationID {
		t.Fatalf("profiles not sorted by integration ID: %#v", profiles)
	}
}

func TestExternalDNSTargetConflictAdvancesOnlyCurrentMaterializedManagedRevision(t *testing.T) {
	item := workerExternalDNSIntegration()
	item.ProtectedGitState = "materialized"
	item.ProtectedGitRevision = item.RuntimeRevision
	item.ProtectedGitCommit = strings.Repeat("c", 40)
	item.ProtectedGitContentDigest = "sha256:" + strings.Repeat("d", 64)
	profile, err := externaldns.ManagedProfile(item, workerExternalDNSRuntimeTemplate())
	if err != nil {
		t.Fatal(err)
	}
	config := edge.DefaultRuntimeConfig()
	config.Enabled = true
	config.Profiles.ExternalDNS = []edge.ExternalDNSProfile{profile}
	targets, err := config.DesiredTargets()
	if err != nil || len(targets) != 1 {
		t.Fatalf("desired targets: %#v err=%v", targets, err)
	}
	desired := targets[0]
	current := desired
	if externalDNSTargetRevisionAdvanceNeeded(item, current, desired) {
		t.Fatal("identical target must not advance runtime revision")
	}
	current.RuntimeConfigDigest = "sha256:" + strings.Repeat("e", 64)
	if externalDNSTargetRevisionAdvanceNeeded(item, current, desired) {
		t.Fatal("runtime-wide config digest change must not advance profile revision")
	}
	current.DesiredDigest = "sha256:" + strings.Repeat("f", 64)
	if !externalDNSTargetRevisionAdvanceNeeded(item, current, desired) {
		t.Fatal("changed exact ExternalDNS target identity must advance runtime revision")
	}
	item.ProtectedGitState = "pending"
	if externalDNSTargetRevisionAdvanceNeeded(item, current, desired) {
		t.Fatal("pending publication must not advance runtime revision")
	}
}

func TestExternalDNSRuntimeAdvancesExactConflictingDurableTarget(t *testing.T) {
	item := workerExternalDNSIntegration()
	item.ProtectedGitState = "materialized"
	item.ProtectedGitRevision = item.RuntimeRevision
	item.ProtectedGitCommit = strings.Repeat("c", 40)
	item.ProtectedGitContentDigest = "sha256:" + strings.Repeat("d", 64)
	profile, err := externaldns.ManagedProfile(item, workerExternalDNSRuntimeTemplate())
	if err != nil {
		t.Fatal(err)
	}
	config := edge.DefaultRuntimeConfig()
	config.Enabled = true
	config.Profiles.ExternalDNS = []edge.ExternalDNSProfile{profile}
	targets, err := config.DesiredTargets()
	if err != nil || len(targets) != 1 {
		t.Fatalf("desired targets: %#v err=%v", targets, err)
	}
	current := targets[0]
	current.DesiredDigest = "sha256:" + strings.Repeat("e", 64)
	current.RuntimeConfigDigest = "sha256:" + strings.Repeat("f", 64)
	edgeStore := &workerEdgeStore{MemoryStore: edge.NewMemoryStore()}
	if err = edgeStore.SynchronizeTargets(t.Context(), current.RuntimeConfigDigest, []edge.DesiredTarget{current}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	source := &externalDNSRecoverySource{item: item}
	runtime := &externalDNSOperationalRuntime{source: source, edgeStore: edgeStore}
	advanced, err := runtime.advanceConflictingExternalDNSRuntimeRevisions(t.Context(), targets)
	if err != nil || !advanced || source.advances != 1 {
		t.Fatalf("advanced=%v calls=%d err=%v", advanced, source.advances, err)
	}
	current = targets[0]
	current.RuntimeConfigDigest = "sha256:" + strings.Repeat("f", 64)
	edgeStore = &workerEdgeStore{MemoryStore: edge.NewMemoryStore()}
	if err = edgeStore.SynchronizeTargets(t.Context(), current.RuntimeConfigDigest, []edge.DesiredTarget{current}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	source.advances = 0
	runtime.edgeStore = edgeStore
	advanced, err = runtime.advanceConflictingExternalDNSRuntimeRevisions(t.Context(), targets)
	if err != nil || advanced || source.advances != 0 {
		t.Fatalf("runtime config-only change advanced=%v calls=%d err=%v", advanced, source.advances, err)
	}
}

func TestExternalDNSReadinessEpochAdvancesOnlyAfterConfigAdmission(t *testing.T) {
	runtime := &externalDNSOperationalRuntime{workerEpoch: 1}
	first := "sha256:" + strings.Repeat("a", 64)
	second := "sha256:" + strings.Repeat("b", 64)
	if epoch := runtime.nextReadinessEpoch(first); epoch != 1 {
		t.Fatalf("initial epoch=%d", epoch)
	}
	runtime.readinessConfigDigest = first
	if epoch := runtime.nextReadinessEpoch(first); epoch != 1 {
		t.Fatalf("unchanged config epoch=%d", epoch)
	}
	if epoch := runtime.nextReadinessEpoch(second); epoch != 2 {
		t.Fatalf("changed config epoch=%d", epoch)
	}
	if epoch := runtime.nextReadinessEpoch(second); epoch != 2 {
		t.Fatalf("unadmitted config consumed another epoch=%d", epoch)
	}
	runtime.workerEpoch, runtime.readinessConfigDigest = 2, second
	if epoch := runtime.nextReadinessEpoch(second); epoch != 2 {
		t.Fatalf("admitted config epoch=%d", epoch)
	}
}
