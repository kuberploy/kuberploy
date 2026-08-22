package helmapps

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestGitChartPackageSourcePackagesExactPinnedPublicCommit(t *testing.T) {
	root := t.TempDir()
	files := testChartFiles()
	packageBytes := packageChart(t, files)
	packagePath := filepath.Join(root, "sample-1.2.3.tgz")
	if err := os.WriteFile(packagePath, packageBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("a", 40)
	gitPath := filepath.Join(root, "git")
	helmPath := filepath.Join(root, "helm")
	gitScript := "#!/bin/sh\ncase \"$*\" in\n  *\"rev-parse HEAD\"*) echo " + commit + ";;\n  *checkout*) /bin/mkdir -p charts/sample;;\nesac\n"
	helmScript := "#!/bin/sh\n/bin/cp " + strconv.Quote(packagePath) + " \"$4/sample-1.2.3.tgz\"\n"
	for path, content := range map[string]string{gitPath: gitScript, helmPath: helmScript} {
		if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	source := ChartSource{Kind: ChartSourceKindGit, Git: &GitChartSource{
		RepositoryURL: "https://git.example.test/platform/charts.git",
		Revision:      commit, ChartPath: "charts/sample", ChartName: "sample", Version: "1.2.3",
	}}
	manifestDigest, err := digestJSON(source)
	if err != nil {
		t.Fatal(err)
	}
	approval := testApproval(t, packageBytes, files)
	approval.Source = source
	approval.OCIRepository = syntheticChartRepository(source.Kind, "sample")
	approval.ManifestDigest = manifestDigest
	if err = approval.Validate(); err != nil {
		t.Fatalf("Git approval: %v", err)
	}
	artifact, err := (GitChartPackageSource{GitPath: gitPath, HelmPath: helmPath, Timeout: 10 * time.Second}).Fetch(t.Context(), approval)
	if err != nil || artifact.ManifestDigest != approval.ManifestDigest ||
		artifact.PackageDigest != approval.PackageDigest || !equalBytes(artifact.PackageBytes, packageBytes) {
		t.Fatalf("Git artifact=%#v err=%v", artifact, err)
	}
}

func TestGitChartPackageSourceRejectsSSHWithoutCredentialBinding(t *testing.T) {
	files := testChartFiles()
	packageBytes := packageChart(t, files)
	approval := testApproval(t, packageBytes, files)
	approval.Source = ChartSource{Kind: ChartSourceKindGit, Git: &GitChartSource{
		RepositoryURL: "ssh://git@git.example.test/platform/charts.git",
		Revision:      strings.Repeat("a", 40), ChartPath: "charts/sample", ChartName: "sample", Version: "1.2.3",
	}}
	approval.OCIRepository = syntheticChartRepository(ChartSourceKindGit, "sample")
	approval.ManifestDigest, _ = digestJSON(approval.Source)
	if _, err := (GitChartPackageSource{GitPath: "/usr/bin/git", HelmPath: "/usr/bin/helm", Timeout: time.Minute}).Fetch(t.Context(), approval); err != ErrInvalid {
		t.Fatalf("SSH source without an exact credential binding accepted: %v", err)
	}
}
