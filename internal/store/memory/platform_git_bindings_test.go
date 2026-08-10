package memory

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/id"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func TestPlatformGitBindingIsPlatformAdminCatalogBoundIdempotentAndImmutable(t *testing.T) {
	ctx := context.Background()
	st := New()
	admin := bootstrapAccessAdmin(t, st)
	outsider, _ := invitedUser(t, st, admin, "Platform outsider", "platform-binding-outsider")
	installation, err := st.CreateGitHubInstallation(ctx, admin.ID, "platform-binding-installation", "platform-binding-installation", "request-installation", domain.CreateGitHubInstallation{
		GitHubInstallationID: 84242, AccountLogin: "Kuberploy", AccountType: "Organization", RepositorySelection: "selected", RepositoryCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	clusterID := id.New()
	input := gitprojection.CreatePlatformBindingInput{
		ClusterID: clusterID, LinkedInstallationID: installation.Value.ID,
		LinkedRepositoryID: deterministicGitHubRepositoryID(installation.Value.ID, 89001), GitHubAppID: 77,
		Repository: gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 84242,
			RepositoryID: 89001, Owner: "kuberploy", Name: "platform-gitops"},
		TargetRef: "refs/heads/platform",
	}
	auditsBefore := st.AuditCount()
	created, err := st.CreatePlatformGitBinding(ctx, admin.ID, "platform-binding-key", "platform-binding-fingerprint", "platform-binding-request", input)
	if err != nil || created.Replay || st.AuditCount() != auditsBefore+1 {
		t.Fatalf("created=%#v audits=%d err=%v", created, st.AuditCount(), err)
	}
	if created.Value.Kind != gitprojection.BindingPlatform || created.Value.ClusterID != clusterID ||
		created.Value.Prefix != gitprojection.PlatformPrefix(clusterID) || created.Value.CredentialMode != gitprojection.CredentialGitHubApp ||
		created.Value.CredentialSecretName != "" {
		t.Fatalf("platform authority was not derived: %#v", created.Value)
	}
	if remote, remoteErr := created.Value.Repository.CanonicalRemote(); remoteErr != nil || remote != "https://github.com/kuberploy/platform-gitops.git" {
		t.Fatalf("canonical remote=%q err=%v", remote, remoteErr)
	}
	replay, err := st.CreatePlatformGitBinding(ctx, admin.ID, "platform-binding-key", "platform-binding-fingerprint", "platform-binding-replay", input)
	if err != nil || !replay.Replay || replay.Value.ID != created.Value.ID || st.AuditCount() != auditsBefore+1 {
		t.Fatalf("replay=%#v audits=%d err=%v", replay, st.AuditCount(), err)
	}
	if _, err = st.CreatePlatformGitBinding(ctx, admin.ID, "platform-binding-key", "different", "platform-binding-idem-conflict", input); !errors.Is(err, base.ErrIdempotencyConflict) {
		t.Fatalf("changed idempotency fingerprint accepted: %v", err)
	}
	changedRef := input
	changedRef.TargetRef = "refs/heads/attacker"
	if _, err = st.CreatePlatformGitBinding(ctx, admin.ID, "platform-binding-second", "platform-binding-second", "platform-binding-second", changedRef); !errors.Is(err, base.ErrConflict) {
		t.Fatalf("second platform authority accepted: %v", err)
	}
	if _, err = st.CreatePlatformGitBinding(ctx, outsider.ID, "platform-binding-outsider", "platform-binding-outsider", "platform-binding-outsider", input); !errors.Is(err, base.ErrForbidden) {
		t.Fatalf("non-platform-admin create error=%v", err)
	}
	if _, err = st.GetPlatformGitBindingForActor(ctx, outsider.ID, clusterID); !errors.Is(err, base.ErrForbidden) {
		t.Fatalf("non-platform-admin read error=%v", err)
	}
	read, err := st.GetPlatformGitBindingForActor(ctx, admin.ID, clusterID)
	if err != nil || read.ID != created.Value.ID {
		t.Fatalf("read=%#v err=%v", read, err)
	}
	tampered := created.Value
	tampered.Repository.Name = "attacker"
	if err = st.PutBinding(ctx, tampered); !errors.Is(err, base.ErrConflict) {
		t.Fatalf("immutable platform authority changed: %v", err)
	}
}

func TestPlatformGitBindingConcurrentCreationHasOneAuthority(t *testing.T) {
	ctx := context.Background()
	st := New()
	admin := bootstrapAccessAdmin(t, st)
	installation, err := st.CreateGitHubInstallation(ctx, admin.ID, "platform-concurrent-installation", "platform-concurrent-installation", "platform-concurrent-installation", domain.CreateGitHubInstallation{
		GitHubInstallationID: 94242, AccountLogin: "kuberploy", AccountType: "Organization", RepositorySelection: "selected", RepositoryCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	input := gitprojection.CreatePlatformBindingInput{ClusterID: id.New(), LinkedInstallationID: installation.Value.ID,
		LinkedRepositoryID: deterministicGitHubRepositoryID(installation.Value.ID, 99001), GitHubAppID: 77,
		Repository: gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 94242, RepositoryID: 99001,
			Owner: "kuberploy", Name: "platform-concurrent"}, TargetRef: "refs/heads/platform"}
	results := make(chan error, 2)
	var start sync.WaitGroup
	start.Add(1)
	for index, key := range []string{"platform-concurrent-a", "platform-concurrent-b"} {
		go func(index int, key string) {
			start.Wait()
			candidate := input
			if index == 1 {
				candidate.TargetRef = "refs/heads/platform-other"
			}
			_, createErr := st.CreatePlatformGitBinding(ctx, admin.ID, key, key, key, candidate)
			results <- createErr
		}(index, key)
	}
	start.Done()
	succeeded, conflicted := 0, 0
	for range 2 {
		switch err := <-results; {
		case err == nil:
			succeeded++
		case errors.Is(err, base.ErrConflict):
			conflicted++
		default:
			t.Fatalf("unexpected concurrent result: %v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("succeeded=%d conflicted=%d", succeeded, conflicted)
	}
}
