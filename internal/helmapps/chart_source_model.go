package helmapps

import (
	"net/url"
	"path"
	"regexp"
	"strings"
)

const (
	MaximumChartSourceRepositoryLength = 2048
	MaximumChartSourceVersionLength    = 128
	MaximumChartSourceChartNameLength  = 253
	MaximumChartSourceRevisionLength   = 64
	MaximumChartSourcePathLength       = 512
)

type ChartSourceKind string

const (
	ChartSourceKindOCI            ChartSourceKind = "oci"
	ChartSourceKindHelmRepository ChartSourceKind = "helm-repository"
	ChartSourceKindGit            ChartSourceKind = "git"
)

var (
	chartSourceNameRE    = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`)
	chartSourceSSHUserRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)

// ChartSource is a closed, provider-neutral union. Exactly one source payload
// must be present and must match Kind.
type ChartSource struct {
	Kind           ChartSourceKind            `json:"kind"`
	OCI            *OCIChartSource            `json:"oci,omitempty"`
	HelmRepository *HelmRepositoryChartSource `json:"helmRepository,omitempty"`
	Git            *GitChartSource            `json:"git,omitempty"`
}

type OCIChartSource struct {
	Repository string `json:"repository"`
	Version    string `json:"version"`
	Digest     string `json:"digest,omitempty"`
}

type HelmRepositoryChartSource struct {
	RepositoryURL string `json:"repositoryUrl"`
	ChartName     string `json:"chartName"`
	Version       string `json:"version"`
}

type GitChartSource struct {
	RepositoryURL string `json:"repositoryUrl"`
	Revision      string `json:"revision"`
	ChartPath     string `json:"chartPath"`
	ChartName     string `json:"chartName"`
	Version       string `json:"version"`
}

func (s ChartSource) Validate() error {
	if boolCount(s.OCI != nil, s.HelmRepository != nil, s.Git != nil) != 1 {
		return ErrInvalid
	}
	switch s.Kind {
	case ChartSourceKindOCI:
		if s.OCI == nil || s.HelmRepository != nil || s.Git != nil {
			return ErrInvalid
		}
		return s.OCI.Validate()
	case ChartSourceKindHelmRepository:
		if s.OCI != nil || s.HelmRepository == nil || s.Git != nil {
			return ErrInvalid
		}
		return s.HelmRepository.Validate()
	case ChartSourceKindGit:
		if s.OCI != nil || s.HelmRepository != nil || s.Git == nil {
			return ErrInvalid
		}
		return s.Git.Validate()
	default:
		return ErrInvalid
	}
}

func (s OCIChartSource) Validate() error {
	if !boundedExact(s.Repository, MaximumChartSourceRepositoryLength) ||
		!canonicalOCIRepository(s.Repository) || !validExactChartVersion(s.Version) ||
		(s.Digest != "" && !validDigest(s.Digest)) {
		return ErrInvalid
	}
	return nil
}

func (s HelmRepositoryChartSource) Validate() error {
	if !validChartSourceURL(s.RepositoryURL, false, "https") ||
		!boundedExact(s.ChartName, MaximumChartSourceChartNameLength) ||
		!chartSourceNameRE.MatchString(s.ChartName) || strings.Contains(s.ChartName, "..") ||
		!validExactChartVersion(s.Version) {
		return ErrInvalid
	}
	return nil
}

func (s GitChartSource) Validate() error {
	if !validChartSourceURL(s.RepositoryURL, true, "https", "ssh") ||
		len(s.Revision) == 0 || len(s.Revision) > MaximumChartSourceRevisionLength ||
		!gitCommitRE.MatchString(s.Revision) || !validRelativeChartPath(s.ChartPath) ||
		!boundedExact(s.ChartName, MaximumChartSourceChartNameLength) ||
		!chartSourceNameRE.MatchString(s.ChartName) || strings.Contains(s.ChartName, "..") ||
		!validExactChartVersion(s.Version) {
		return ErrInvalid
	}
	return nil
}

func (s ChartSource) ChartIdentity() (string, string, error) {
	if s.Validate() != nil {
		return "", "", ErrInvalid
	}
	switch s.Kind {
	case ChartSourceKindOCI:
		parts := strings.Split(s.OCI.Repository, "/")
		return parts[len(parts)-1], s.OCI.Version, nil
	case ChartSourceKindHelmRepository:
		return s.HelmRepository.ChartName, s.HelmRepository.Version, nil
	case ChartSourceKindGit:
		return s.Git.ChartName, s.Git.Version, nil
	default:
		return "", "", ErrInvalid
	}
}

func (s ChartSource) DisplayRepository() string {
	if s.Validate() != nil {
		return ""
	}
	switch s.Kind {
	case ChartSourceKindOCI:
		return s.OCI.Repository
	case ChartSourceKindHelmRepository:
		return s.HelmRepository.RepositoryURL
	case ChartSourceKindGit:
		return s.Git.RepositoryURL
	default:
		return ""
	}
}

func validExactChartVersion(value string) bool {
	return boundedExact(value, MaximumChartSourceVersionLength) && semverRE.MatchString(value)
}

func validChartSourceURL(value string, requirePath bool, schemes ...string) bool {
	if !boundedExact(value, MaximumChartSourceRepositoryLength) || containsControl(value) || strings.Contains(value, "\\") {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Opaque != "" || parsed.Host == "" || parsed.Hostname() == "" ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawPath != "" {
		return false
	}
	allowed := false
	for _, scheme := range schemes {
		if parsed.Scheme == scheme {
			allowed = true
			break
		}
	}
	if !allowed {
		return false
	}
	if parsed.Scheme == "ssh" {
		if parsed.User != nil {
			if _, hasPassword := parsed.User.Password(); hasPassword || !chartSourceSSHUserRE.MatchString(parsed.User.Username()) {
				return false
			}
		}
	} else if parsed.User != nil {
		return false
	}
	if requirePath && (parsed.Path == "" || parsed.Path == "/" || strings.HasSuffix(parsed.Path, "/")) {
		return false
	}
	return validURLPath(parsed.Path)
}

func validURLPath(value string) bool {
	if strings.Contains(value, "//") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func validRelativeChartPath(value string) bool {
	if !boundedExact(value, MaximumChartSourcePathLength) || containsControl(value) ||
		strings.Contains(value, "\\") || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") ||
		strings.Contains(value, "//") {
		return false
	}
	if value == "." {
		return true
	}
	if path.Clean(value) != value {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func boundedExact(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && strings.TrimSpace(value) == value
}

func boolCount(values ...bool) int {
	count := 0
	for _, value := range values {
		if value {
			count++
		}
	}
	return count
}
