package main

import (
	"strings"
	"testing"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/externaldns"
)

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
