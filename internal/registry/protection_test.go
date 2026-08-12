package registry

import (
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

func TestBuildProtectionSnapshotClosesTargetBoundary(t *testing.T) {
	now := time.Date(2026, 8, 13, 1, 2, 3, 0, time.UTC)
	target := domain.RegistryTarget{ID: "11111111-1111-4111-8111-111111111111", Name: "managed", Mode: domain.RegistryTargetManaged,
		Endpoint: "https://registry.example.test:5443", RepositoryPrefix: "tenant"}
	policy := DefaultPolicy(target.ID, "22222222-2222-4222-8222-222222222222", "tenant/service/image", now)
	image := "registry.example.test:5443/tenant/service/image@sha256:" + strings.Repeat("a", 64)
	snapshot := BuildProtectionSnapshot(target, policy, domain.RegistryAuthorityGitIntent, []ProtectionInput{{
		ReferenceKey: "deployment/one", Image: image, SourceRevision: strings.Repeat("b", 40), CreatedAt: now.Add(-time.Minute),
	}}, true, now)
	if !snapshot.Observation.Complete || len(snapshot.References) != 1 || snapshot.References[0].Repository != policy.Repository ||
		snapshot.References[0].Digest != "sha256:"+strings.Repeat("a", 64) || snapshot.References[0].Kind != domain.RegistryReferenceCurrentGitIntent {
		t.Fatalf("snapshot=%#v", snapshot)
	}

	external := BuildProtectionSnapshot(target, policy, domain.RegistryAuthorityRuntime, []ProtectionInput{{
		ReferenceKey: "deployment/two", Image: "ghcr.io/other/service@sha256:" + strings.Repeat("c", 64), SourceRevision: strings.Repeat("e", 40),
	}}, true, now)
	if !external.Observation.Complete || len(external.References) != 0 {
		t.Fatalf("external snapshot=%#v", external)
	}

	wrongRepository := BuildProtectionSnapshot(target, policy, domain.RegistryAuthorityOperations, []ProtectionInput{{
		ReferenceKey: "operation/one", Image: "registry.example.test:5443/tenant/other/image@sha256:" + strings.Repeat("d", 64), SourceRevision: "1/2026-08-13T01:02:03Z",
	}}, true, now)
	if wrongRepository.Observation.Complete || len(wrongRepository.References) != 0 {
		t.Fatalf("wrong repository snapshot=%#v", wrongRepository)
	}
}

func TestBuildProtectionSnapshotRejectsMalformedDuplicateAndIncompleteInputs(t *testing.T) {
	now := time.Date(2026, 8, 13, 1, 2, 3, 0, time.UTC)
	target := domain.RegistryTarget{ID: "11111111-1111-4111-8111-111111111111", Name: "managed", Mode: domain.RegistryTargetManaged,
		Endpoint: "https://registry.example.test", RepositoryPrefix: "tenant"}
	policy := DefaultPolicy(target.ID, "22222222-2222-4222-8222-222222222222", "tenant/service/image", now)
	valid := "registry.example.test/tenant/service/image@sha256:" + strings.Repeat("a", 64)
	for name, inputs := range map[string][]ProtectionInput{
		"malformed": {{ReferenceKey: "one", Image: "registry.example.test/tenant/service/image:latest"}},
		"duplicate": {{ReferenceKey: "one", Image: valid}, {ReferenceKey: "one", Image: valid}},
		"empty key": {{ReferenceKey: "", Image: valid}},
	} {
		t.Run(name, func(t *testing.T) {
			snapshot := BuildProtectionSnapshot(target, policy, domain.RegistryAuthorityRuntime, inputs, true, now)
			if snapshot.Observation.Complete || len(snapshot.References) != 0 {
				t.Fatalf("snapshot=%#v", snapshot)
			}
		})
	}
	snapshot := BuildProtectionSnapshot(target, policy, domain.RegistryAuthorityRuntime, nil, false, now)
	if snapshot.Observation.Complete || snapshot.Observation.Revision == "" {
		t.Fatalf("incomplete snapshot=%#v", snapshot)
	}
}
