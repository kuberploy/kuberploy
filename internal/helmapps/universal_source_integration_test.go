package helmapps

import (
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"testing"
	"time"
)

// This opt-in test resolves and packages a real public Git chart without any
// ambient Git credential. Unit tests retain hermetic fake-process coverage;
// qualification supplies one exact public repository, commit, path, name, and
// version through environment variables.
func TestGitChartPackageSourceResolvesRealPublicRepository(t *testing.T) {
	repository := os.Getenv("KUBERPLOY_TEST_HELM_GIT_REPOSITORY")
	revision := os.Getenv("KUBERPLOY_TEST_HELM_GIT_REVISION")
	chartPath := os.Getenv("KUBERPLOY_TEST_HELM_GIT_CHART_PATH")
	chartName := os.Getenv("KUBERPLOY_TEST_HELM_GIT_CHART_NAME")
	chartVersion := os.Getenv("KUBERPLOY_TEST_HELM_GIT_CHART_VERSION")
	if repository == "" || revision == "" || chartPath == "" || chartName == "" || chartVersion == "" {
		t.Skip("set the KUBERPLOY_TEST_HELM_GIT_* variables for a real public Git chart test")
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	helmPath, err := exec.LookPath("helm")
	if err != nil {
		t.Fatal(err)
	}
	source := ChartSource{Kind: ChartSourceKindGit, Git: &GitChartSource{
		RepositoryURL: repository, Revision: revision, ChartPath: chartPath,
		ChartName: chartName, Version: chartVersion,
	}}
	approval := Approval{ApprovalKey: ApprovalKey{ID: "11111111-1111-4111-8111-111111111111", Revision: 1},
		Source: source, OCIRepository: syntheticChartRepository(ChartSourceKindGit, chartName), ChartVersion: chartVersion,
		ManifestDigest: unknownChartDigest, PackageDigest: unknownChartDigest, ValuesSchemaDigest: unknownChartDigest,
		RendererImage: RendererImage, RendererVersion: HelmVersion, PolicyVersion: PolicyVersion,
		CreatedBy: "22222222-2222-4222-8222-222222222222", IdempotencyKey: "real-public-git-chart-qualification",
		CreatedAt: time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)}
	artifact, err := (GitChartPackageSource{GitPath: gitPath, HelmPath: helmPath, Timeout: 2 * time.Minute}).Fetch(t.Context(), approval)
	if err != nil {
		t.Fatal(err)
	}
	approval.ManifestDigest, approval.PackageDigest = artifact.ManifestDigest, artifact.PackageDigest
	approval.ValuesSchemaDigest, err = chartPackageValuesSchemaDigest(artifact.PackageBytes, chartName)
	if err != nil || approval.Validate() != nil {
		t.Fatalf("resolved chart identity is invalid: %v", err)
	}
	if _, err = InspectChartPackage(approval, artifact.PackageBytes); err != nil {
		t.Fatalf("resolved real chart package failed admission inspection: %v", err)
	}
}

// This opt-in test downloads one exact chart version from a real classic HTTPS
// Helm repository, verifies the index digest, and applies the normal package
// admission inspection. It deliberately uses no repository credential.
func TestHelmRepositoryResolverResolvesRealPublicRepository(t *testing.T) {
	repository := os.Getenv("KUBERPLOY_TEST_HELM_REPOSITORY_URL")
	chartName := os.Getenv("KUBERPLOY_TEST_HELM_REPOSITORY_CHART_NAME")
	chartVersion := os.Getenv("KUBERPLOY_TEST_HELM_REPOSITORY_CHART_VERSION")
	if repository == "" || chartName == "" || chartVersion == "" {
		t.Skip("set the KUBERPLOY_TEST_HELM_REPOSITORY_* variables for a real public Helm repository test")
	}
	parsed, err := url.Parse(repository)
	if err != nil {
		t.Fatal(err)
	}
	source := ChartSource{Kind: ChartSourceKindHelmRepository, HelmRepository: &HelmRepositoryChartSource{
		RepositoryURL: repository, ChartName: chartName, Version: chartVersion,
	}}
	approval := Approval{ApprovalKey: ApprovalKey{ID: "11111111-1111-4111-8111-111111111111", Revision: 1},
		Source: source, OCIRepository: syntheticChartRepository(ChartSourceKindHelmRepository, chartName), ChartVersion: chartVersion,
		ManifestDigest: unknownChartDigest, PackageDigest: unknownChartDigest, ValuesSchemaDigest: unknownChartDigest,
		RendererImage: RendererImage, RendererVersion: HelmVersion, PolicyVersion: PolicyVersion,
		CreatedBy: "22222222-2222-4222-8222-222222222222", IdempotencyKey: "real-public-helm-repository-qualification",
		CreatedAt: time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)}
	resolved, err := (HelmRepositoryResolver{
		Client: &http.Client{Timeout: time.Minute}, AllowedHosts: []string{parsed.Host},
	}).Resolve(t.Context(), *source.HelmRepository)
	if err != nil {
		t.Fatal(err)
	}
	approval.ManifestDigest, approval.PackageDigest = resolved.Artifact.ManifestDigest, resolved.Artifact.PackageDigest
	approval.ValuesSchemaDigest, err = chartPackageValuesSchemaDigest(resolved.Artifact.PackageBytes, chartName)
	if err != nil || approval.Validate() != nil {
		t.Fatalf("resolved chart identity is invalid: %v", err)
	}
	if _, err = InspectChartPackage(approval, resolved.Artifact.PackageBytes); err != nil {
		t.Fatalf("resolved real chart package failed admission inspection: %v", err)
	}
}
