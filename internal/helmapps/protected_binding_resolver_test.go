package helmapps

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/id"
)

func protectedBindingResolverFixture(t *testing.T) (ReleaseTarget, ProtectedBindingResolverConfig,
	ProtectedBindingResolutionSnapshot) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	target := ReleaseTarget{ProjectID: id.New(), EnvironmentID: id.New(), ApplicationID: id.New()}
	clusterID, platformID := id.New(), id.New()
	repository := gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 10,
		RepositoryID: 20, Owner: "kuberploy", Name: "platform"}
	platform, err := gitprojection.NewGitHubPlatformBinding(platformID, clusterID, repository, "refs/heads/platform", now)
	if err != nil {
		t.Fatal(err)
	}
	platform.State, platform.TargetHeadRevision = gitprojection.BindingIndexing, strings.Repeat("a", 40)
	platform.TargetHeadObservedAt, platform.UpdatedAt = now, now
	environment, err := gitprojection.NewGitHubEnvironmentBinding(id.New(), target.ProjectID, target.EnvironmentID,
		gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 10,
			RepositoryID: 21, Owner: "kuberploy", Name: "environment"}, "refs/heads/main", now)
	if err != nil {
		t.Fatal(err)
	}
	environment.State, environment.TargetHeadRevision, environment.IndexedRevision = gitprojection.BindingReady,
		strings.Repeat("b", 40), strings.Repeat("b", 40)
	environment.ProjectionGeneration, environment.TargetHeadObservedAt, environment.IndexedAt, environment.UpdatedAt = 1, now, now, now
	activated := now
	generation := gitprojection.Generation{BindingID: environment.ID, Number: 1,
		HeadRevision: environment.IndexedRevision, ParserVersion: environment.ParserVersion,
		State: gitprojection.ProjectionActive, StartedAt: now, ActivatedAt: &activated}
	snapshot := ProtectedBindingResolutionSnapshot{Platform: platform, Environments: []gitprojection.Binding{environment},
		ActiveGenerations: []gitprojection.Generation{generation}, ApplicationProjects: map[string]string{target.ApplicationID: target.ProjectID},
		Catalog: []ApprovalDocument{releaseApprovalDocumentFixture(t)}}
	return target, ProtectedBindingResolverConfig{PlatformBindingID: platformID, ClusterID: clusterID}, snapshot
}

func TestMemoryProtectedBindingResolverDerivesOnlyExactReadySnapshot(t *testing.T) {
	target, config, snapshot := protectedBindingResolverFixture(t)
	resolver := &MemoryProtectedBindingResolver{Config: config, Snapshot: snapshot}
	resolved, err := resolver.ResolveProtectedBinding(t.Context(), target)
	if err != nil || resolved.Validate() != nil || resolved.PlatformBindingID != config.PlatformBindingID ||
		resolved.ClusterID != config.ClusterID || resolved.EnvironmentBindingID != snapshot.Environments[0].ID ||
		resolved.EnvironmentRevision != snapshot.Environments[0].IndexedRevision ||
		resolved.EnvironmentGeneration != snapshot.Environments[0].ProjectionGeneration ||
		resolved.PlannedBaseRevision != snapshot.Platform.TargetHeadRevision {
		t.Fatalf("resolved=%#v err=%v", resolved, err)
	}
	wantCatalog, err := protectedCatalogDigest(snapshot.Catalog)
	if err != nil || resolved.CatalogDigest != wantCatalog {
		t.Fatalf("catalog=%s want=%s err=%v", resolved.CatalogDigest, wantCatalog, err)
	}
	// Returned values remain stable even if caller-owned slices are reordered.
	second := releaseApprovalDocumentFixture(t)
	snapshot.Catalog = append(snapshot.Catalog, second)
	forward, err := protectedCatalogDigest(snapshot.Catalog)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Catalog[0], snapshot.Catalog[1] = snapshot.Catalog[1], snapshot.Catalog[0]
	reversed, err := protectedCatalogDigest(snapshot.Catalog)
	if err != nil || forward != reversed {
		t.Fatalf("catalog digest depended on row order: forward=%s reversed=%s err=%v", forward, reversed, err)
	}
}

func TestMemoryProtectedBindingResolverRejectsEveryStaleOrSubstitutedAuthority(t *testing.T) {
	target, config, valid := protectedBindingResolverFixture(t)
	otherEnvironment := valid.Environments[0]
	otherEnvironment.ID = id.New()
	otherEnvironment.Repository.RepositoryID++
	otherEnvironment.CreatedAt, otherEnvironment.UpdatedAt = valid.Environments[0].CreatedAt, valid.Environments[0].UpdatedAt
	tests := []struct {
		name   string
		mutate func(*ProtectedBindingResolverConfig, *ReleaseTarget, *ProtectedBindingResolutionSnapshot)
	}{
		{name: "wrong platform", mutate: func(c *ProtectedBindingResolverConfig, _ *ReleaseTarget, _ *ProtectedBindingResolutionSnapshot) {
			c.PlatformBindingID = id.New()
		}},
		{name: "platform legacy credential", mutate: func(_ *ProtectedBindingResolverConfig, _ *ReleaseTarget, s *ProtectedBindingResolutionSnapshot) {
			s.Platform.CredentialMode, s.Platform.CredentialSecretName = gitprojection.CredentialLegacySecret, "legacy-secret"
		}},
		{name: "platform unobserved", mutate: func(_ *ProtectedBindingResolverConfig, _ *ReleaseTarget, s *ProtectedBindingResolutionSnapshot) {
			s.Platform.State, s.Platform.TargetHeadRevision, s.Platform.TargetHeadObservedAt = gitprojection.BindingWaiting, "", time.Time{}
		}},
		{name: "provider identity substituted", mutate: func(_ *ProtectedBindingResolverConfig, _ *ReleaseTarget, s *ProtectedBindingResolutionSnapshot) {
			s.Platform.Repository.Provider = "gitlab"
		}},
		{name: "target ref substituted", mutate: func(_ *ProtectedBindingResolverConfig, _ *ReleaseTarget, s *ProtectedBindingResolutionSnapshot) {
			s.Environments[0].TargetRef = "refs/heads/../substituted"
		}},
		{name: "ambiguous environment", mutate: func(_ *ProtectedBindingResolverConfig, _ *ReleaseTarget, s *ProtectedBindingResolutionSnapshot) {
			s.Environments = append(s.Environments, otherEnvironment)
		}},
		{name: "wrong environment project", mutate: func(_ *ProtectedBindingResolverConfig, _ *ReleaseTarget, s *ProtectedBindingResolutionSnapshot) {
			s.Environments[0].ProjectID = id.New()
		}},
		{name: "environment target index lag", mutate: func(_ *ProtectedBindingResolverConfig, _ *ReleaseTarget, s *ProtectedBindingResolutionSnapshot) {
			s.Environments[0].State, s.Environments[0].TargetHeadRevision = gitprojection.BindingIndexing, strings.Repeat("c", 40)
		}},
		{name: "generation substitution", mutate: func(_ *ProtectedBindingResolverConfig, _ *ReleaseTarget, s *ProtectedBindingResolutionSnapshot) {
			s.ActiveGenerations[0].HeadRevision = strings.Repeat("c", 40)
		}},
		{name: "multiple active generations", mutate: func(_ *ProtectedBindingResolverConfig, _ *ReleaseTarget, s *ProtectedBindingResolutionSnapshot) {
			s.ActiveGenerations = append(s.ActiveGenerations, s.ActiveGenerations[0])
		}},
		{name: "invalid projected document", mutate: func(_ *ProtectedBindingResolverConfig, _ *ReleaseTarget, s *ProtectedBindingResolutionSnapshot) {
			s.InvalidDocumentCount = 1
		}},
		{name: "application escaped project", mutate: func(_ *ProtectedBindingResolverConfig, _ *ReleaseTarget, s *ProtectedBindingResolutionSnapshot) {
			s.ApplicationProjects[target.ApplicationID] = id.New()
		}},
		{name: "catalog absent", mutate: func(_ *ProtectedBindingResolverConfig, _ *ReleaseTarget, s *ProtectedBindingResolutionSnapshot) {
			s.Catalog = nil
		}},
		{name: "catalog bytes substituted", mutate: func(_ *ProtectedBindingResolverConfig, _ *ReleaseTarget, s *ProtectedBindingResolutionSnapshot) {
			s.Catalog[0].DefaultValuesYAML = []byte("substituted: true\n")
		}},
		{name: "caller target substituted", mutate: func(_ *ProtectedBindingResolverConfig, target *ReleaseTarget, _ *ProtectedBindingResolutionSnapshot) {
			target.ProjectID = id.New()
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidateConfig, candidateTarget := config, target
			candidate := cloneProtectedResolutionSnapshot(valid)
			test.mutate(&candidateConfig, &candidateTarget, &candidate)
			resolver := &MemoryProtectedBindingResolver{Config: candidateConfig, Snapshot: candidate}
			if resolved, err := resolver.ResolveProtectedBinding(t.Context(), candidateTarget); err == nil || resolved != (ProtectedBindingSnapshot{}) {
				t.Fatalf("substituted authority resolved=%#v err=%v", resolved, err)
			}
		})
	}
}

func TestProtectedCatalogDigestRejectsDuplicateOrInvalidDocuments(t *testing.T) {
	document := releaseApprovalDocumentFixture(t)
	if _, err := protectedCatalogDigest([]ApprovalDocument{document, document}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate catalog entry error=%v", err)
	}
	document.DocumentsDigest = digestBytes([]byte("substituted"))
	if _, err := protectedCatalogDigest([]ApprovalDocument{document}); !errors.Is(err, ErrConflict) {
		t.Fatalf("substituted catalog document error=%v", err)
	}
}
