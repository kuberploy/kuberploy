package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/edge"
	"github.com/kuberploy/kuberploy/internal/externaldns"
)

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
