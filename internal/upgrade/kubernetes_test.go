package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/releases"
)

type fakeJobAPI struct {
	job           *KubernetesJob
	gets          int
	creates       int
	completeAtGet int
	getErrAt      int
}

func executableComponentCharts(version, digest string) []domain.ManifestChart {
	names := []string{"kuberploy-argocd", "kuberploy-installer", "kuberploy-builder", "kuberploy-cert-manager", "kuberploy-edge", "kuberploy-external-dns", "kuberploy-external-secrets", "kuberploy-monitoring", "kuberploy-postgresql", "kuberploy-registry", "kuberploy-runtime", "kuberploy-sealed-secrets", "kuberploy-valkey"}
	charts := make([]domain.ManifestChart, 0, len(names))
	for _, name := range names {
		charts = append(charts, domain.ManifestChart{Name: name, Version: version, OCIReference: "ghcr.io/kuberploy/charts/" + name + ":" + version, OCIDigest: digest, Package: name + "-" + version + ".tgz", PackageSHA256: digest})
	}
	return charts
}

func (f *fakeJobAPI) ServerVersion(context.Context) (string, error) { return "v1.35.3", nil }

func (f *fakeJobAPI) GetJob(_ context.Context, _, _ string) (KubernetesJob, error) {
	f.gets++
	if f.getErrAt > 0 && f.gets == f.getErrAt {
		return KubernetesJob{}, errors.New("temporary Kubernetes API outage")
	}
	if f.job == nil {
		return KubernetesJob{}, errJobNotFound
	}
	if f.completeAtGet > 0 && f.gets >= f.completeAtGet {
		f.job.Status = JobStatus{Succeeded: 1, CompletionTime: "2026-08-06T00:01:00Z", Conditions: []JobCondition{{Type: "Complete", Status: "True"}}}
	}
	return *f.job, nil
}
func (f *fakeJobAPI) CreateJob(_ context.Context, _ string, job KubernetesJob) (KubernetesJob, error) {
	f.creates++
	if f.job != nil {
		return KubernetesJob{}, errJobConflict
	}
	f.job = &job
	return job, nil
}

func validExecutableRequest() ExecutableRequest {
	version := "1.1.0"
	artifactDigest := "sha256:" + strings.Repeat("a", 64)
	commit := strings.Repeat("b", 40)
	manifest := domain.ReleaseManifest{
		Schema:        "https://raw.githubusercontent.com/kuberploy/kuberploy/" + commit + "/release/release-manifest.schema.json",
		SchemaVersion: "1.0.0",
		Release: domain.ManifestRelease{
			Tag:       "v" + version,
			Version:   version,
			CreatedAt: time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC),
			NotesURL:  "https://github.com/kuberploy/kuberploy/releases/tag/v" + version,
			Summary:   "Safe namespaced control-plane update",
		},
		Source:   domain.ManifestSource{Repository: "kuberploy/kuberploy", Commit: commit},
		Versions: domain.ManifestVersions{Kuberploy: version, API: version, Worker: version, Web: version, Migration: version, Upgrader: version, BuilderAgent: version, Chart: version},
		Compatibility: domain.ReleaseCompatibility{
			SupportedUpgradeFrom: ">=1.0.0 <1.1.0",
			Kubernetes:           domain.KubernetesCompatibility{Constraint: ">=1.34.0-0 <1.37.0-0", TestedMinors: []string{"1.34", "1.35", "1.36"}},
			Database: domain.DatabaseCompatibility{
				Engine: "postgresql", CurrentSchema: "002_platform_upgrades", MinimumUpgradeableSchema: "001_initial",
				MigrationSetSHA256: artifactDigest, Strategy: "prisma-migrate-deploy-with-advisory-lock",
				RollbackPolicy: "Only roll back to a schema-compatible control-plane release.",
			},
		},
		Artifacts: domain.ManifestArtifacts{
			Images: []domain.ManifestImage{
				{Component: "api", Reference: "ghcr.io/kuberploy/kuberploy-api", Digest: artifactDigest, Platforms: []string{"linux/amd64", "linux/arm64"}},
				{Component: "worker", Reference: "ghcr.io/kuberploy/kuberploy-worker", Digest: artifactDigest, Platforms: []string{"linux/amd64", "linux/arm64"}},
				{Component: "web", Reference: "ghcr.io/kuberploy/kuberploy-web", Digest: artifactDigest, Platforms: []string{"linux/amd64", "linux/arm64"}},
				{Component: "migration", Reference: "ghcr.io/kuberploy/kuberploy-migration", Digest: artifactDigest, Platforms: []string{"linux/amd64", "linux/arm64"}},
				{Component: "upgrader", Reference: "ghcr.io/kuberploy/kuberploy-upgrader", Digest: artifactDigest, Platforms: []string{"linux/amd64", "linux/arm64"}},
				{Component: "builder-agent", Reference: "ghcr.io/kuberploy/kuberploy-builder-agent", Digest: artifactDigest, Platforms: []string{"linux/amd64", "linux/arm64"}},
			},
			Chart:           domain.ManifestChart{Name: "kuberploy", Version: version, OCIReference: "ghcr.io/kuberploy/charts/kuberploy:" + version, OCIDigest: artifactDigest, Package: "kuberploy-" + version + ".tgz", PackageSHA256: artifactDigest},
			ComponentCharts: executableComponentCharts(version, artifactDigest),
		},
		DependencyLock: domain.ManifestDependencyLock{File: "DEPENDENCIES.md", SHA256: artifactDigest},
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(manifestBytes)
	manifestDigest := "sha256:" + hex.EncodeToString(sum[:])
	operationID := "11111111-2222-4333-8444-555555555555"
	return ExecutableRequest{OperationID: operationID, Generation: 2, JobName: JobName(operationID, 2), Namespace: "kuberploy-system", ReleaseName: "kuberploy-installer", TargetVersion: version, ManifestDigest: manifestDigest, ManifestBytes: manifestBytes}
}

func TestKubernetesRunnerRejectsLegacyChildAndUnavailableInstallerMutation(t *testing.T) {
	request := validExecutableRequest()
	jobs := &fakeJobAPI{}

	legacy := request
	legacy.ReleaseName = "kuberploy"
	legacyRunner := KubernetesRunner{Jobs: jobs, Namespace: legacy.Namespace, ServiceAccount: "kuberploy-upgrade", ReleaseName: legacy.ReleaseName}
	if _, err := legacyRunner.Run(context.Background(), legacy); err == nil || !strings.Contains(err.Error(), "legacy child Helm upgrade is disabled") {
		t.Fatalf("legacy child rejection err=%v", err)
	}

	installerRunner := KubernetesRunner{Jobs: jobs, Namespace: request.Namespace, ServiceAccount: "kuberploy-upgrade", ReleaseName: request.ReleaseName}
	if _, err := installerRunner.Run(context.Background(), request); err == nil || !strings.Contains(err.Error(), "durable installer desired-state contract") {
		t.Fatalf("installer mutation rejection err=%v", err)
	}
	if jobs.creates != 0 || jobs.gets != 0 {
		t.Fatalf("disabled platform lifecycle touched Jobs: gets=%d creates=%d", jobs.gets, jobs.creates)
	}
}

func TestInstallerChartSelectsOnlyExactManifestArtifact(t *testing.T) {
	request := validExecutableRequest()
	manifest, err := releases.ParseExactManifest(request.ManifestBytes, request.ManifestDigest)
	if err != nil {
		t.Fatal(err)
	}
	chart, err := installerChart(manifest)
	if err != nil || chart.Name != "kuberploy-installer" || !strings.Contains(chart.OCIReference, "/kuberploy-installer:") {
		t.Fatalf("chart=%#v err=%v", chart, err)
	}
	manifest.Artifacts.ComponentCharts = manifest.Artifacts.ComponentCharts[:1]
	if _, err = installerChart(manifest); err == nil {
		t.Fatal("missing exact installer artifact was accepted")
	}
}

func TestKubernetesRunnerRejectsTamperedExactManifestBytes(t *testing.T) {
	request := validExecutableRequest()
	request.ManifestBytes = append([]byte(nil), request.ManifestBytes...)
	request.ManifestBytes[len(request.ManifestBytes)-1] ^= 1
	jobs := &fakeJobAPI{}
	runner := KubernetesRunner{Jobs: jobs, Namespace: request.Namespace, ServiceAccount: "kuberploy-upgrade", ReleaseName: request.ReleaseName}
	if _, err := runner.Run(context.Background(), request); err == nil || !strings.Contains(err.Error(), "exact release manifest bytes") {
		t.Fatalf("expected exact-byte rejection, got %v", err)
	}
	if jobs.creates != 0 {
		t.Fatalf("tampered manifest created %d Jobs", jobs.creates)
	}
}

func TestSupportedKubernetesVersionIsBoundedByManifestContract(t *testing.T) {
	for _, version := range []string{"v1.34.0", "v1.35.7+k3s1", "1.36.1"} {
		if !supportedKubernetesVersion(version) {
			t.Errorf("expected %s to be supported", version)
		}
	}
	for _, version := range []string{"v1.33.9", "v1.37.0", "v2.34.0", "garbage"} {
		if supportedKubernetesVersion(version) {
			t.Errorf("expected %s to be rejected", version)
		}
	}
}
