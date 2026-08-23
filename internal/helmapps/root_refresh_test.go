package helmapps

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/argo"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

type platformRootRefresherFunc func(context.Context, argo.PlatformRootApplicationExpectation, time.Time) error

func (f platformRootRefresherFunc) RefreshPlatformRootApplication(ctx context.Context,
	expectation argo.PlatformRootApplicationExpectation, now time.Time) error {
	return f(ctx, expectation, now)
}

func TestProductionProtectedRootRefresherUsesExactVerifiedPlatformHead(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	_, _, _, observation := protectedArgoReadinessFixture(t, now)
	identity := observation.DesiredStateRuntimeIdentity
	binding, err := gitprojection.NewGitHubPlatformBinding(identity.PlatformBindingID, identity.ClusterID,
		gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 7, RepositoryID: 8,
			Owner: "kuberploy", Name: "platform"}, "refs/heads/main", now)
	if err != nil {
		t.Fatal(err)
	}
	revision := strings.Repeat("a", 40)
	binding.State, binding.TargetHeadRevision, binding.TargetHeadObservedAt = gitprojection.BindingIndexing, revision, now
	binding.UpdatedAt = now
	head := gitprojection.VerifiedHead{BindingID: binding.ID, Repository: binding.Repository,
		TargetRef: binding.TargetRef, Commit: revision, Source: gitprojection.ObservationWrite,
		ProviderRequest: "helm-root-refresh", ObservedAt: now}
	calls := 0
	adapter := &ProductionProtectedRootRefresher{Identity: identity,
		Refresher: platformRootRefresherFunc(func(_ context.Context,
			expectation argo.PlatformRootApplicationExpectation, refreshedAt time.Time) error {
			calls++
			if expectation.Namespace != identity.ArgoNamespace || expectation.Name != identity.RootApplicationName ||
				expectation.ExpectedGitRevision != revision || expectation.TargetRevision != binding.TargetRef ||
				expectation.RepositoryCredentialName != identity.RepositorySecretName || refreshedAt != now {
				return errors.New("substituted root refresh expectation")
			}
			return nil
		})}
	if err = adapter.RefreshProtectedRoot(t.Context(), binding, head, now); err != nil || calls != 1 {
		t.Fatalf("refresh calls=%d err=%v", calls, err)
	}
	substituted := binding
	substituted.ClusterID = strings.Repeat("0", 8) + binding.ClusterID[8:]
	if err = adapter.RefreshProtectedRoot(t.Context(), substituted, head, now); !errors.Is(err, ErrInvalid) || calls != 1 {
		t.Fatalf("substituted binding refresh calls=%d err=%v", calls, err)
	}
}
