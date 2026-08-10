package buildpromotion

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	platformstore "github.com/kuberploy/kuberploy/internal/store"
)

const (
	actorID       = "11111111-1111-4111-8111-111111111111"
	attemptID     = "22222222-2222-4222-8222-222222222222"
	definitionID  = "33333333-3333-4333-8333-333333333333"
	projectID     = "44444444-4444-4444-8444-444444444444"
	applicationID = "55555555-5555-4555-8555-555555555555"
	environmentID = "66666666-6666-4666-8666-666666666666"
	registryID    = "77777777-7777-4777-8777-777777777777"
)

type projectionCatalog struct {
	source ProjectedBuild
	err    error
}

func (c projectionCatalog) SuccessfulReleaseProjection(context.Context, string) (ProjectedBuild, error) {
	return c.source, c.err
}

type releaseCatalog struct {
	release domain.RegistryRelease
	err     error
}

func (c releaseCatalog) RegistryRelease(context.Context, string) (domain.RegistryRelease, error) {
	return c.release, c.err
}

type resourceCatalog struct {
	environment domain.Environment
	application domain.Application
	err         error
}

func (c resourceCatalog) GetEnvironment(context.Context, string) (domain.Environment, error) {
	return c.environment, c.err
}
func (c resourceCatalog) GetApplication(context.Context, string) (domain.Application, error) {
	return c.application, c.err
}

type accessRecorder struct {
	buildPermission                            domain.Permission
	buildTarget                                domain.AccessTarget
	promotionEnvironment, promotionApplication string
	buildErr, promotionErr                     error
}

func (a *accessRecorder) Authorize(_ context.Context, _ string, p domain.Permission, t domain.AccessTarget) error {
	a.buildPermission, a.buildTarget = p, t
	return a.buildErr
}
func (a *accessRecorder) AuthorizePromotion(_ context.Context, _ string, e, app string) error {
	a.promotionEnvironment, a.promotionApplication = e, app
	return a.promotionErr
}

func fixture() (*Resolver, Request, ProjectedBuild, domain.RegistryRelease, *accessRecorder) {
	created := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)
	completed := created.Add(time.Minute)
	digest := "sha256:" + strings.Repeat("a", 64)
	repository := "kp/projects/" + projectID + "/services/" + applicationID + "/image"
	projected := ProjectedBuild{AttemptID: attemptID, DefinitionID: definitionID, ProjectID: projectID, ApplicationID: applicationID, Generation: 9, CommitSHA: strings.Repeat("b", 40), DefinitionDigest: "sha256:" + strings.Repeat("c", 64), RegistryTargetID: registryID, RegistryServer: "registry.example.test", Repository: repository, ImageReference: "registry.example.test/" + repository + "@" + digest, ImageDigest: digest, ReleaseID: attemptID, CreatedAt: created, CompletedAt: completed, ProjectionCompletedAt: completed.Add(time.Second)}
	release := domain.RegistryRelease{ID: attemptID, RegistryTargetID: registryID, ServiceID: applicationID, Repository: repository, RootDigest: digest, CreatedAt: created, SucceededAt: &completed, Availability: domain.RegistryArtifactPresent}
	access := &accessRecorder{}
	resolver := &Resolver{Projections: projectionCatalog{source: projected}, Releases: releaseCatalog{release: release}, Resources: resourceCatalog{environment: domain.Environment{ID: environmentID, ProjectID: projectID, Namespace: "kp-demo-prod"}, application: domain.Application{ID: applicationID, ProjectID: projectID}}, Access: access}
	return resolver, Request{actorID, attemptID, environmentID}, projected, release, access
}

func TestResolveDerivesEveryDeploymentAuthorityFromSuccessfulBuild(t *testing.T) {
	resolver, request, projected, _, access := fixture()
	source, err := resolver.Resolve(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if source.ImageReference != projected.ImageReference || source.ApplicationID != applicationID || source.ProjectID != projectID || source.EnvironmentID != environmentID || source.Namespace != "kp-demo-prod" {
		t.Fatalf("source was not derived exactly: %#v", source)
	}
	if access.buildPermission != domain.PermissionBuildsRead || access.buildTarget.Type != "application" || access.buildTarget.ID != applicationID || access.promotionEnvironment != environmentID || access.promotionApplication != applicationID {
		t.Fatalf("authorization proof was incomplete: %#v", access)
	}
}

func TestResolveFailsClosedUntilExactProjectionAndRelease(t *testing.T) {
	t.Run("projection pending", func(t *testing.T) {
		resolver, request, _, _, _ := fixture()
		resolver.Projections = projectionCatalog{err: ErrNotReady}
		if _, err := resolver.Resolve(context.Background(), request); !errors.Is(err, ErrNotReady) {
			t.Fatalf("pending projection: %v", err)
		}
	})
	t.Run("release digest mismatch", func(t *testing.T) {
		resolver, request, _, release, _ := fixture()
		release.RootDigest = "sha256:" + strings.Repeat("d", 64)
		resolver.Releases = releaseCatalog{release: release}
		if _, err := resolver.Resolve(context.Background(), request); !errors.Is(err, ErrConflict) {
			t.Fatalf("mismatched release: %v", err)
		}
	})
	t.Run("release row missing", func(t *testing.T) {
		resolver, request, _, _, _ := fixture()
		resolver.Releases = releaseCatalog{err: platformstore.ErrNotFound}
		if _, err := resolver.Resolve(context.Background(), request); !errors.Is(err, ErrNotReady) {
			t.Fatalf("missing exact release was exposed as resource absence: %v", err)
		}
	})
	t.Run("artifact unavailable", func(t *testing.T) {
		resolver, request, _, release, _ := fixture()
		observed := release.CreatedAt.Add(time.Hour)
		release.Availability = domain.RegistryArtifactMissing
		release.AvailabilityObservedAt = &observed
		resolver.Releases = releaseCatalog{release: release}
		if _, err := resolver.Resolve(context.Background(), request); !errors.Is(err, ErrArtifactUnavailable) {
			t.Fatalf("missing release: %v", err)
		}
	})
	t.Run("cross project environment", func(t *testing.T) {
		resolver, request, _, _, _ := fixture()
		resolver.Resources = resourceCatalog{environment: domain.Environment{ID: environmentID, ProjectID: "88888888-8888-4888-8888-888888888888", Namespace: "attacker"}, application: domain.Application{ID: applicationID, ProjectID: projectID}}
		if _, err := resolver.Resolve(context.Background(), request); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-project environment: %v", err)
		}
	})
}

func TestResolveRequiresBothBuildReadAndCompositeResourceWrite(t *testing.T) {
	resolver, request, _, _, access := fixture()
	access.buildErr = platformstore.ErrForbidden
	if _, err := resolver.Resolve(context.Background(), request); !errors.Is(err, platformstore.ErrForbidden) {
		t.Fatalf("build read bypassed: %v", err)
	}
	if access.promotionEnvironment != "" {
		t.Fatal("resource authority evaluated after denied build")
	}
	resolver, request, _, _, access = fixture()
	access.promotionErr = platformstore.ErrForbidden
	if _, err := resolver.Resolve(context.Background(), request); !errors.Is(err, platformstore.ErrForbidden) {
		t.Fatalf("resource write bypassed: %v", err)
	}
}
