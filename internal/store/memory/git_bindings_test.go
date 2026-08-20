package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/id"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func TestEnvironmentGitBindingIsAuthorizedCatalogBoundAndIdempotent(t *testing.T) {
	ctx := context.Background()
	store := New()
	admin := bootstrapAccessAdmin(t, store)
	outsider, _ := invitedUser(t, store, admin, "Outsider", "git-binding-outsider")
	project, err := store.CreateProject(ctx, admin.ID, "git-project", "git-project", domain.CreateProject{Name: "Git project", Slug: "git-project"})
	if err != nil {
		t.Fatal(err)
	}
	environment, err := store.CreateEnvironment(ctx, admin.ID, "git-environment", "git-environment", domain.CreateEnvironment{ProjectID: project.Value.ID, Name: "Production", Slug: "production"})
	if err != nil {
		t.Fatal(err)
	}
	installation, err := store.CreateGitHubInstallation(ctx, admin.ID, "git-installation", "git-installation", "request-installation", domain.CreateGitHubInstallation{
		GitHubInstallationID: 4242, AccountLogin: "Kuberploy", AccountType: "Organization", RepositorySelection: "selected", RepositoryCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	input := gitprojection.CreateEnvironmentBindingInput{
		EnvironmentID: environment.Value.ID, LinkedInstallationID: installation.Value.ID,
		LinkedRepositoryID: deterministicGitHubRepositoryID(installation.Value.ID, 9001), GitHubAppID: 77,
		Repository: gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 4242, RepositoryID: 9001, Owner: "kuberploy", Name: "platform-config"},
		TargetRef:  "refs/heads/main",
	}
	created, err := store.CreateEnvironmentGitBinding(ctx, admin.ID, "binding-key", "binding-fingerprint", "binding-request", input)
	if err != nil || created.Replay || created.Value.CredentialMode != gitprojection.CredentialGitHubApp || created.Value.CredentialSecretName != "" || created.Value.EnvironmentID != environment.Value.ID {
		t.Fatalf("created=%#v err=%v", created, err)
	}
	replay, err := store.CreateEnvironmentGitBinding(ctx, admin.ID, "binding-key", "binding-fingerprint", "binding-request-replay", input)
	if err != nil || !replay.Replay || replay.Value.ID != created.Value.ID {
		t.Fatalf("replay=%#v err=%v", replay, err)
	}
	if _, err = store.CreateEnvironmentGitBinding(ctx, admin.ID, "binding-key", "different-fingerprint", "binding-request-conflict", input); !errors.Is(err, base.ErrIdempotencyConflict) {
		t.Fatalf("different idempotency fingerprint err=%v", err)
	}
	if _, err = store.CreateEnvironmentGitBinding(ctx, admin.ID, "different-key", "different-key", "binding-request-second", input); !errors.Is(err, base.ErrConflict) {
		t.Fatalf("second environment authority err=%v", err)
	}
	read, err := store.GetEnvironmentGitBindingForActor(ctx, admin.ID, environment.Value.ID)
	if err != nil || read.ID != created.Value.ID || read.Repository.Owner != "kuberploy" {
		t.Fatalf("read=%#v err=%v", read, err)
	}
	if _, err = store.GetEnvironmentGitBindingForActor(ctx, outsider.ID, environment.Value.ID); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("outsider read disclosed binding: %v", err)
	}
}

func TestEnvironmentGitBindingRejectsUnresolvedRepositoryCatalogID(t *testing.T) {
	ctx := context.Background()
	store := New()
	admin := bootstrapAccessAdmin(t, store)
	project, _ := store.CreateProject(ctx, admin.ID, "git-project", "git-project", domain.CreateProject{Name: "Git project", Slug: "git-project"})
	environment, _ := store.CreateEnvironment(ctx, admin.ID, "git-environment", "git-environment", domain.CreateEnvironment{ProjectID: project.Value.ID, Name: "Production", Slug: "production"})
	installation, _ := store.CreateGitHubInstallation(ctx, admin.ID, "git-installation", "git-installation", "request-installation", domain.CreateGitHubInstallation{
		GitHubInstallationID: 4242, AccountLogin: "kuberploy", AccountType: "Organization", RepositorySelection: "selected", RepositoryCount: 1,
	})
	input := gitprojection.CreateEnvironmentBindingInput{
		EnvironmentID: environment.Value.ID, LinkedInstallationID: installation.Value.ID,
		LinkedRepositoryID: "99999999-9999-4999-8999-999999999999", GitHubAppID: 77,
		Repository: gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 4242, RepositoryID: 9001, Owner: "kuberploy", Name: "platform-config"},
		TargetRef:  "refs/heads/main",
	}
	if _, err := store.CreateEnvironmentGitBinding(ctx, admin.ID, "binding-key", "binding-fingerprint", "binding-request", input); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("unresolved repository catalog ID err=%v", err)
	}
}

func TestExternalDNSMetadataInvalidatesReadyGitProjection(t *testing.T) {
	ctx := context.Background()
	store := New()
	admin := bootstrapAccessAdmin(t, store)
	project, err := store.CreateProject(ctx, admin.ID, "dns-policy-project", "dns-policy-project", domain.CreateProject{Name: "DNS policy", Slug: "dns-policy"})
	if err != nil {
		t.Fatal(err)
	}
	environment, err := store.CreateEnvironment(ctx, admin.ID, "dns-policy-environment", "dns-policy-environment", domain.CreateEnvironment{ProjectID: project.Value.ID, Name: "Production", Slug: "production"})
	if err != nil {
		t.Fatal(err)
	}
	installation, err := store.CreateGitHubInstallation(ctx, admin.ID, "dns-policy-installation", "dns-policy-installation", "dns-policy-installation", domain.CreateGitHubInstallation{
		GitHubInstallationID: 5252, AccountLogin: "kuberploy", AccountType: "Organization", RepositorySelection: "selected", RepositoryCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateEnvironmentGitBinding(ctx, admin.ID, "dns-policy-binding", "dns-policy-binding", "dns-policy-binding", gitprojection.CreateEnvironmentBindingInput{
		EnvironmentID: environment.Value.ID, LinkedInstallationID: installation.Value.ID,
		LinkedRepositoryID: deterministicGitHubRepositoryID(installation.Value.ID, 5253), GitHubAppID: 77,
		Repository: gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 5252, RepositoryID: 5253, Owner: "kuberploy", Name: "platform-config"},
		TargetRef:  "refs/heads/main",
	})
	if err != nil {
		t.Fatal(err)
	}
	ready := created.Value
	indexedAt := ready.UpdatedAt.Add(time.Second)
	ready.TargetHeadRevision, ready.IndexedRevision = strings.Repeat("a", 40), strings.Repeat("a", 40)
	ready.TargetHeadObservedAt, ready.IndexedAt, ready.UpdatedAt = indexedAt, indexedAt, indexedAt
	ready.ProjectionGeneration, ready.State = 1, gitprojection.BindingReady
	if err = store.PutBinding(ctx, ready); err != nil {
		t.Fatal(err)
	}

	integration := domain.ExternalDNSIntegration{
		ID: id.New(), Slug: "public-dns", Name: "Public DNS", Mode: "managed", ProviderKind: "cloudflare", TXTOwnerID: "kuberploy.production",
		AllowedDomainSuffixes: []string{"example.com"}, SyncPolicy: "upsert-only", CredentialSecretRef: "dns-credentials", ProviderConfigRef: "cloudflare-provider",
		EgressConfigRef: "internet-egress", EnvironmentIDs: []string{environment.Value.ID},
	}
	if _, err = store.CreateExternalDNSIntegrationForActor(ctx, admin.ID, "dns-policy-create", "dns-policy-create", "dns-policy-create", integration); err != nil {
		t.Fatal(err)
	}
	invalidated, err := store.GetEnvironmentGitBindingForActor(ctx, admin.ID, environment.Value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if invalidated.State != gitprojection.BindingIndexing || !invalidated.UpdatedAt.After(ready.UpdatedAt) || invalidated.TargetHeadRevision != ready.TargetHeadRevision || invalidated.IndexedRevision != ready.IndexedRevision {
		t.Fatalf("external DNS metadata did not request exact-head policy revalidation: %#v", invalidated)
	}
}

func TestExternalDNSChangedUpdateClearsProtectedGitMetadata(t *testing.T) {
	ctx := context.Background()
	store := New()
	admin := bootstrapAccessAdmin(t, store)
	project, err := store.CreateProject(ctx, admin.ID, "dns-reset-project", "dns-reset-project", domain.CreateProject{Name: "DNS reset", Slug: "dns-reset"})
	if err != nil {
		t.Fatal(err)
	}
	environment, err := store.CreateEnvironment(ctx, admin.ID, "dns-reset-environment", "dns-reset-environment", domain.CreateEnvironment{ProjectID: project.Value.ID, Name: "Production", Slug: "production"})
	if err != nil {
		t.Fatal(err)
	}
	integration := domain.ExternalDNSIntegration{ID: id.New(), Slug: "public-dns", Name: "Public DNS", Mode: "managed", ProviderKind: "cloudflare", TXTOwnerID: "kuberploy.production",
		AllowedDomainSuffixes: []string{"example.com"}, SyncPolicy: "upsert-only", CredentialSecretRef: "dns-credentials", ProviderConfigRef: "cloudflare-provider",
		EgressConfigRef: "internet-egress", EnvironmentIDs: []string{environment.Value.ID}}
	created, err := store.CreateExternalDNSIntegrationForActor(ctx, admin.ID, "dns-reset-create", "dns-reset-create", "dns-reset-create", integration)
	if err != nil {
		t.Fatal(err)
	}
	observedAt := time.Now().UTC()
	if err = store.RecordExternalDNSPublication(ctx, created.Value.ID, created.Value.RuntimeRevision, false, "sha256:"+strings.Repeat("a", 64), "commit-a", observedAt); err != nil {
		t.Fatal(err)
	}
	updated := created.Value
	updated.Name = "Public DNS updated"
	updated.AllowedDomainSuffixes = []string{"example.org"}
	result, err := store.UpdateExternalDNSIntegrationForActor(ctx, admin.ID, "dns-reset-update", "dns-reset-update", "dns-reset-update", updated)
	if err != nil {
		t.Fatal(err)
	}
	if result.Value.ProtectedGitState != "pending" || result.Value.ProtectedGitRevision != 0 || result.Value.ProtectedGitContentDigest != "" || result.Value.ProtectedGitCommit != "" || result.Value.ProtectedGitObservedAt != nil {
		t.Fatalf("changed update retained protected Git metadata: %#v", result.Value)
	}
}

func TestExternalDNSManagedRuntimeRepublishAdvancesRevision(t *testing.T) {
	ctx := context.Background()
	store := New()
	admin := bootstrapAccessAdmin(t, store)
	project, err := store.CreateProject(ctx, admin.ID, "dns-republish-project", "dns-republish-project", domain.CreateProject{Name: "DNS republish", Slug: "dns-republish"})
	if err != nil {
		t.Fatal(err)
	}
	environment, err := store.CreateEnvironment(ctx, admin.ID, "dns-republish-environment", "dns-republish-environment", domain.CreateEnvironment{ProjectID: project.Value.ID, Name: "Production", Slug: "production"})
	if err != nil {
		t.Fatal(err)
	}
	integration := domain.ExternalDNSIntegration{ID: id.New(), Slug: "republish-dns", Name: "Republish DNS", Mode: "managed", ProviderKind: "cloudflare", TXTOwnerID: "kuberploy.republish",
		AllowedDomainSuffixes: []string{"example.com"}, SyncPolicy: "upsert-only", CredentialSecretRef: "dns-credentials", ProviderConfigRef: "cloudflare-provider",
		EgressConfigRef: "internet-egress", EnvironmentIDs: []string{environment.Value.ID}}
	created, err := store.CreateExternalDNSIntegrationForActor(ctx, admin.ID, "dns-republish-create", "dns-republish-create", "dns-republish-create", integration)
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	if err = store.RecordExternalDNSPublication(ctx, created.Value.ID, 1, false, digest, strings.Repeat("b", 40), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err = store.AdvanceExternalDNSRuntimeRevision(ctx, created.Value.ID, 1, digest, time.Now().UTC().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	items, err := store.ListExternalDNSIntegrationsForRuntime(ctx, 64)
	if err != nil || len(items) != 1 || items[0].RuntimeRevision != 2 || items[0].ProtectedGitState != "pending" || items[0].ProtectedGitRevision != 0 {
		t.Fatalf("runtime republish state=%#v err=%v", items, err)
	}
	if err = store.AdvanceExternalDNSRuntimeRevision(ctx, created.Value.ID, 1, digest, time.Now().UTC().Add(2*time.Second)); !errors.Is(err, base.ErrConflict) {
		t.Fatalf("stale runtime republish CAS accepted: %v", err)
	}
}
