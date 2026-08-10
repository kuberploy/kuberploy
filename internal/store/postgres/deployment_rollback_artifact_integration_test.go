package postgres

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/deploymentrollback"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/registry"
)

func TestPostgreSQLDeploymentRollbackArtifactVerification(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	st, err := Open(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := databaseTime(time.Now())
	targetID, applicationID := id.New(), id.New()
	repository := "rollback/" + applicationID
	target, err := st.PutRegistryTarget(t.Context(), domain.RegistryTarget{ID: targetID, Name: "rollback-" + targetID,
		Mode: domain.RegistryTargetManaged, Endpoint: "registry.rollback.test", RepositoryPrefix: "rollback", CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.PutServiceRegistryPolicy(t.Context(), registry.DefaultPolicy(target.ID, applicationID, repository, now)); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("9", 64)
	release := domain.RegistryRelease{ID: id.New(), RegistryTargetID: target.ID, ServiceID: applicationID,
		Repository: repository, RootDigest: digest, CreatedAt: now, SucceededAt: &now, Availability: domain.RegistryArtifactPresent}
	if _, _, err = st.PutRegistryRelease(t.Context(), release); err != nil {
		t.Fatal(err)
	}
	image := target.Endpoint + "/" + repository + "@" + digest
	managed, err := st.VerifyRetainedDeploymentImage(t.Context(), applicationID, image)
	if err != nil || !managed {
		t.Fatalf("present release managed=%v err=%v", managed, err)
	}
	if managed, err = st.VerifyRetainedDeploymentImage(t.Context(), applicationID,
		"external.test/ordinary/api@"+digest); err != nil || managed {
		t.Fatalf("external image managed=%v err=%v", managed, err)
	}
	if _, err = st.pool.Exec(t.Context(), `UPDATE registry_releases SET availability='missing',availability_observed_at=$2 WHERE id=$1`, release.ID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	managed, err = st.VerifyRetainedDeploymentImage(t.Context(), applicationID, image)
	if !managed || !errors.Is(err, deploymentrollback.ErrArtifactUnavailable) {
		t.Fatalf("missing release managed=%v err=%v", managed, err)
	}
}
