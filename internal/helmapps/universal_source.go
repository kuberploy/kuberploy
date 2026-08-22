package helmapps

import (
	"bytes"
	"context"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// UniversalChartPackageSource keeps source acquisition outside renderer Jobs.
// The admitted package is persisted immutably and later rendering stays
// network-off regardless of whether the source was OCI, a Helm repository, or Git.
type UniversalChartPackageSource struct {
	OCI      ChartPackageSource
	HTTP     *http.Client
	GitPath  string
	HelmPath string
	Timeout  time.Duration
}

func (s UniversalChartPackageSource) Fetch(ctx context.Context, approval Approval) (ChartArtifact, error) {
	if ctx == nil || approval.Validate() != nil {
		return ChartArtifact{}, ErrInvalid
	}
	source, err := approval.CanonicalSource()
	if err != nil {
		return ChartArtifact{}, err
	}
	switch source.Kind {
	case ChartSourceKindOCI:
		if s.OCI == nil {
			return ChartArtifact{}, ErrUnavailable
		}
		return s.OCI.Fetch(ctx, approval)
	case ChartSourceKindHelmRepository:
		if s.HTTP == nil {
			return ChartArtifact{}, ErrUnavailable
		}
		parsed, parseErr := url.Parse(source.HelmRepository.RepositoryURL)
		if parseErr != nil {
			return ChartArtifact{}, ErrInvalid
		}
		resolved, resolveErr := (HelmRepositoryResolver{
			Client: s.HTTP, AllowedHosts: []string{parsed.Host},
		}).Resolve(ctx, *source.HelmRepository)
		if resolveErr != nil {
			return ChartArtifact{}, resolveErr
		}
		if approval.ManifestDigest != unknownChartDigest && resolved.Artifact.ManifestDigest != approval.ManifestDigest ||
			approval.PackageDigest != unknownChartDigest && resolved.Artifact.PackageDigest != approval.PackageDigest {
			clear(resolved.Artifact.PackageBytes)
			return ChartArtifact{}, ErrUnsafeChart
		}
		return resolved.Artifact, nil
	case ChartSourceKindGit:
		return (GitChartPackageSource{GitPath: s.GitPath, HelmPath: s.HelmPath, Timeout: s.Timeout}).Fetch(ctx, approval)
	default:
		return ChartArtifact{}, ErrInvalid
	}
}

type GitChartPackageSource struct {
	GitPath  string
	HelmPath string
	Timeout  time.Duration
}

func (s GitChartPackageSource) Fetch(ctx context.Context, approval Approval) (ChartArtifact, error) {
	source, err := approval.CanonicalSource()
	if ctx == nil || err != nil || source.Kind != ChartSourceKindGit || source.Git == nil ||
		source.Git.Validate() != nil || !strings.HasPrefix(source.Git.RepositoryURL, "https://") ||
		!filepath.IsAbs(s.GitPath) || !filepath.IsAbs(s.HelmPath) ||
		s.Timeout < time.Second || s.Timeout > 2*time.Minute {
		return ChartArtifact{}, ErrInvalid
	}
	work, err := os.MkdirTemp("", "kuberploy-helm-git-")
	if err != nil {
		return ChartArtifact{}, ErrUnavailable
	}
	defer os.RemoveAll(work) //nolint:errcheck
	if err = os.Chmod(work, 0o700); err != nil {
		return ChartArtifact{}, ErrUnavailable
	}
	repository := filepath.Join(work, "repository")
	home := filepath.Join(work, "home")
	out := filepath.Join(work, "package")
	for _, directory := range []string{repository, home, out} {
		if err = os.Mkdir(directory, 0o700); err != nil {
			return ChartArtifact{}, ErrUnavailable
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	environment := []string{
		"HOME=" + home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"HELM_CACHE_HOME=" + filepath.Join(home, "helm-cache"),
		"HELM_CONFIG_HOME=" + filepath.Join(home, "helm-config"),
		"HELM_DATA_HOME=" + filepath.Join(home, "helm-data"),
		"LANG=C",
		"LC_ALL=C",
	}
	run := func(path string, directory string, arguments ...string) error {
		command := exec.CommandContext(runCtx, path, arguments...)
		command.Dir = directory
		command.Env = environment
		var output boundedCommandOutput
		command.Stdout, command.Stderr = &output, &output
		if command.Run() != nil {
			return ErrUnavailable
		}
		return nil
	}
	gitArgs := []string{"-c", "credential.helper=", "-c", "core.hooksPath=/dev/null", "-c", "http.followRedirects=false"}
	if err = run(s.GitPath, repository, append(gitArgs, "init", "--quiet")...); err != nil ||
		run(s.GitPath, repository, append(gitArgs, "remote", "add", "origin", source.Git.RepositoryURL)...) != nil ||
		run(s.GitPath, repository, append(gitArgs, "fetch", "--quiet", "--depth=1", "--no-tags", "origin", source.Git.Revision)...) != nil ||
		run(s.GitPath, repository, append(gitArgs, "checkout", "--quiet", "--detach", "FETCH_HEAD")...) != nil {
		return ChartArtifact{}, ErrUnavailable
	}
	resolved, err := gitOutput(runCtx, s.GitPath, repository, environment, append(gitArgs, "rev-parse", "HEAD"))
	if err != nil || strings.TrimSpace(resolved) != source.Git.Revision {
		return ChartArtifact{}, ErrUnsafeChart
	}
	chartPath := filepath.Join(repository, filepath.FromSlash(source.Git.ChartPath))
	if !pathWithin(repository, chartPath) || rejectChartSymlinks(chartPath) != nil {
		return ChartArtifact{}, ErrUnsafeChart
	}
	if err = run(s.HelmPath, repository, "package", chartPath, "--destination", out); err != nil {
		return ChartArtifact{}, ErrUnsafeChart
	}
	matches, err := filepath.Glob(filepath.Join(out, "*.tgz"))
	if err != nil || len(matches) != 1 {
		return ChartArtifact{}, ErrUnsafeChart
	}
	packageBytes, err := os.ReadFile(matches[0])
	if err != nil || len(packageBytes) == 0 || len(packageBytes) > MaximumChartSize {
		clear(packageBytes)
		return ChartArtifact{}, ErrUnsafeChart
	}
	manifestDigest, err := digestJSON(source)
	if err != nil || approval.ManifestDigest != unknownChartDigest && manifestDigest != approval.ManifestDigest ||
		approval.PackageDigest != unknownChartDigest && digestBytes(packageBytes) != approval.PackageDigest {
		clear(packageBytes)
		return ChartArtifact{}, ErrUnsafeChart
	}
	return ChartArtifact{ManifestDigest: manifestDigest, PackageDigest: digestBytes(packageBytes), PackageBytes: packageBytes}, nil
}

type boundedCommandOutput struct {
	bytes.Buffer
}

func (b *boundedCommandOutput) Write(value []byte) (int, error) {
	original := len(value)
	remaining := 32<<10 - b.Len()
	if remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
		}
		_, _ = b.Buffer.Write(value)
	}
	return original, nil
}

func gitOutput(ctx context.Context, gitPath, directory string, environment, arguments []string) (string, error) {
	command := exec.CommandContext(ctx, gitPath, arguments...)
	command.Dir, command.Env = directory, environment
	var output boundedCommandOutput
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		return "", ErrUnavailable
	}
	return output.String(), nil
}

func rejectChartSymlinks(root string) error {
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrUnsafeChart
	}
	return filepath.WalkDir(root, func(_ string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.Type()&os.ModeSymlink != 0 {
			return ErrUnsafeChart
		}
		return nil
	})
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

var _ ChartPackageSource = UniversalChartPackageSource{}
var _ ChartPackageSource = GitChartPackageSource{}
