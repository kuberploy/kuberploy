package helmapps

import (
	"errors"
	"strings"
	"testing"
)

func TestChartSourceAcceptsExactProviderNeutralKinds(t *testing.T) {
	commit := strings.Repeat("a", 40)
	tests := []ChartSource{
		{Kind: ChartSourceKindOCI, OCI: &OCIChartSource{
			Repository: "oci://registry.example.com/platform/nginx", Version: "1.2.3",
		}},
		{Kind: ChartSourceKindOCI, OCI: &OCIChartSource{
			Repository: "oci://registry.example.com/platform/nginx", Version: "1.2.3-rc.1",
			Digest: "sha256:" + strings.Repeat("b", 64),
		}},
		{Kind: ChartSourceKindHelmRepository, HelmRepository: &HelmRepositoryChartSource{
			RepositoryURL: "https://charts.example.com/stable", ChartName: "ingress-nginx", Version: "4.13.2",
		}},
		{Kind: ChartSourceKindGit, Git: &GitChartSource{
			RepositoryURL: "https://git.example.com/platform/charts.git", Revision: commit, ChartPath: "charts/api",
			ChartName: "api", Version: "1.2.3",
		}},
		{Kind: ChartSourceKindGit, Git: &GitChartSource{
			RepositoryURL: "ssh://git@git.example.com/platform/charts.git", Revision: commit, ChartPath: ".",
			ChartName: "api", Version: "1.2.3",
		}},
	}

	for index, source := range tests {
		if err := source.Validate(); err != nil {
			t.Fatalf("source %d rejected: %v", index, err)
		}
	}
}

func TestChartSourceRejectsInvalidUnion(t *testing.T) {
	validOCI := &OCIChartSource{Repository: "oci://registry.example.com/platform/app", Version: "1.0.0"}
	validGit := &GitChartSource{
		RepositoryURL: "https://git.example.com/platform/charts.git",
		Revision:      strings.Repeat("a", 40),
		ChartPath:     "charts/app",
		ChartName:     "app",
		Version:       "1.0.0",
	}
	tests := []ChartSource{
		{},
		{Kind: "unknown", OCI: validOCI},
		{Kind: ChartSourceKindOCI, Git: validGit},
		{Kind: ChartSourceKindOCI, OCI: validOCI, Git: validGit},
	}
	for index, source := range tests {
		if err := source.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid union %d accepted: %v", index, err)
		}
	}
}

func TestOCIChartSourceRejectsFloatingOrUnsafeCoordinates(t *testing.T) {
	valid := OCIChartSource{Repository: "oci://registry.example.com/platform/app", Version: "1.2.3"}
	tests := []func(*OCIChartSource){
		func(source *OCIChartSource) { source.Repository = "https://registry.example.com/platform/app" },
		func(source *OCIChartSource) {
			source.Repository = "oci://user:secret@registry.example.com/platform/app"
		},
		func(source *OCIChartSource) { source.Repository = "oci://registry.example.com/platform/../app" },
		func(source *OCIChartSource) { source.Repository += ":latest" },
		func(source *OCIChartSource) { source.Version = "" },
		func(source *OCIChartSource) { source.Version = "latest" },
		func(source *OCIChartSource) { source.Version = ">=1.0.0" },
		func(source *OCIChartSource) { source.Version = strings.Repeat("1", MaximumChartSourceVersionLength+1) },
		func(source *OCIChartSource) { source.Digest = strings.Repeat("a", 64) },
	}
	for index, mutate := range tests {
		candidate := valid
		mutate(&candidate)
		if err := candidate.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid OCI source %d accepted: %#v", index, candidate)
		}
	}
}

func TestHelmRepositoryChartSourceRejectsCredentialsTraversalAndFloatingVersions(t *testing.T) {
	valid := HelmRepositoryChartSource{
		RepositoryURL: "https://charts.example.com/stable", ChartName: "sample-chart", Version: "1.2.3",
	}
	tests := []func(*HelmRepositoryChartSource){
		func(source *HelmRepositoryChartSource) { source.RepositoryURL = "http://charts.example.com" },
		func(source *HelmRepositoryChartSource) { source.RepositoryURL = "oci://charts.example.com/stable" },
		func(source *HelmRepositoryChartSource) {
			source.RepositoryURL = "https://user:secret@charts.example.com/stable"
		},
		func(source *HelmRepositoryChartSource) {
			source.RepositoryURL = "https://charts.example.com/a/../stable"
		},
		func(source *HelmRepositoryChartSource) {
			source.RepositoryURL = "https://charts.example.com/%2e%2e/stable"
		},
		func(source *HelmRepositoryChartSource) {
			source.RepositoryURL = "https://charts.example.com/stable?token=secret"
		},
		func(source *HelmRepositoryChartSource) { source.ChartName = "stable/sample" },
		func(source *HelmRepositoryChartSource) { source.ChartName = "../sample" },
		func(source *HelmRepositoryChartSource) { source.Version = "latest" },
		func(source *HelmRepositoryChartSource) { source.Version = "1.x" },
	}
	for index, mutate := range tests {
		candidate := valid
		mutate(&candidate)
		if err := candidate.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid Helm repository source %d accepted: %#v", index, candidate)
		}
	}
}

func TestGitChartSourceRejectsCredentialsFloatingRevisionsAndTraversal(t *testing.T) {
	valid := GitChartSource{
		RepositoryURL: "ssh://git@git.example.com/platform/charts.git",
		Revision:      strings.Repeat("a", 40),
		ChartPath:     "charts/sample",
		ChartName:     "sample",
		Version:       "1.2.3",
	}
	tests := []func(*GitChartSource){
		func(source *GitChartSource) { source.RepositoryURL = "git://git.example.com/platform/charts.git" },
		func(source *GitChartSource) { source.RepositoryURL = "git@git.example.com:platform/charts.git" },
		func(source *GitChartSource) {
			source.RepositoryURL = "ssh://git:secret@git.example.com/platform/charts.git"
		},
		func(source *GitChartSource) {
			source.RepositoryURL = "https://token@git.example.com/platform/charts.git"
		},
		func(source *GitChartSource) {
			source.RepositoryURL = "ssh://git@git.example.com/platform/../charts.git"
		},
		func(source *GitChartSource) {
			source.RepositoryURL = "ssh://git@git.example.com/platform/charts.git#main"
		},
		func(source *GitChartSource) { source.Revision = "" },
		func(source *GitChartSource) { source.Revision = "main" },
		func(source *GitChartSource) { source.Revision = "refs/heads/main" },
		func(source *GitChartSource) { source.ChartPath = "/charts/sample" },
		func(source *GitChartSource) { source.ChartPath = "../sample" },
		func(source *GitChartSource) { source.ChartPath = "charts/../sample" },
		func(source *GitChartSource) { source.ChartPath = "charts\\sample" },
		func(source *GitChartSource) { source.ChartPath = strings.Repeat("a", MaximumChartSourcePathLength+1) },
		func(source *GitChartSource) { source.ChartName = "../sample" },
		func(source *GitChartSource) { source.Version = "main" },
	}
	for index, mutate := range tests {
		candidate := valid
		mutate(&candidate)
		if err := candidate.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid Git source %d accepted: %#v", index, candidate)
		}
	}
}
