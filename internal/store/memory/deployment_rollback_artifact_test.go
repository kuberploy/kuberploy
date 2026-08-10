package memory

import (
	"errors"
	"testing"

	"github.com/kuberploy/kuberploy/internal/deploymentrollback"
	"github.com/kuberploy/kuberploy/internal/domain"
)

func TestVerifyRetainedDeploymentImageDistinguishesExternalPresentAndExpired(t *testing.T) {
	seed := seedManagedRegistry(t)
	managed, err := seed.store.VerifyRetainedDeploymentImage(t.Context(), seed.serviceID,
		"registry.test/"+seed.repository+"@"+seed.oldRelease.RootDigest)
	if err != nil || !managed {
		t.Fatalf("present managed release: managed=%v err=%v", managed, err)
	}
	managed, err = seed.store.VerifyRetainedDeploymentImage(t.Context(), seed.serviceID,
		"external.test/other/api@"+seed.oldRelease.RootDigest)
	if err != nil || managed {
		t.Fatalf("external image: managed=%v err=%v", managed, err)
	}
	seed.store.mu.Lock()
	release := seed.store.registryReleases[seed.oldRelease.ID]
	release.Availability = domain.RegistryArtifactExpired
	now := seed.now
	release.AvailabilityObservedAt = &now
	seed.store.registryReleases[release.ID] = release
	seed.store.mu.Unlock()
	managed, err = seed.store.VerifyRetainedDeploymentImage(t.Context(), seed.serviceID,
		"registry.test/"+seed.repository+"@"+seed.oldRelease.RootDigest)
	if !managed || !errors.Is(err, deploymentrollback.ErrArtifactUnavailable) {
		t.Fatalf("expired release: managed=%v err=%v", managed, err)
	}
}
