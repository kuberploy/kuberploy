package helmapps

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"syscall"
	"time"
)

const (
	ProjectedOCICredentialRoot = "/var/run/secrets/kuberploy/helm-oci"
	OCICredentialModeBasic     = "basic"
	OCICredentialModeBearer    = "bearer"
	maximumOCIUsernameBytes    = 1024
)

var (
	ociCredentialProfileNameRE  = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$`)
	ErrOCICredentialUnavailable = errors.New("operator OCI credential is unavailable")
)

// OCIRegistryCredentialProfile is operator configuration. RegistryHost selects
// the profile; AuthHost is the only host that may receive its credential.
// Neither value is accepted from an approval request or persisted chart data.
type OCIRegistryCredentialProfile struct {
	RegistryHost     string `json:"registryHost"`
	AuthHost         string `json:"authHost"`
	Name             string `json:"name"`
	Mode             string `json:"mode"`
	ProjectionDigest string `json:"projectionDigest"`
}

func (p OCIRegistryCredentialProfile) Validate() error {
	if !validOCIHost(p.RegistryHost) || !validOCIHost(p.AuthHost) ||
		!ociCredentialProfileNameRE.MatchString(p.Name) ||
		(p.Mode != OCICredentialModeBasic && p.Mode != OCICredentialModeBearer) || !validDigest(p.ProjectionDigest) {
		return ErrInvalid
	}
	return nil
}

func validateOCICredentialProfiles(profiles []OCIRegistryCredentialProfile, registries, authHosts []string) error {
	if len(profiles) > maximumOCIHostCount || !sort.SliceIsSorted(profiles, func(i, j int) bool {
		return profiles[i].RegistryHost < profiles[j].RegistryHost
	}) {
		return ErrInvalid
	}
	names := make(map[string]struct{}, len(profiles))
	for index, profile := range profiles {
		if profile.Validate() != nil || !containsExactHost(registries, profile.RegistryHost) ||
			!containsExactHost(authHosts, profile.AuthHost) ||
			index > 0 && profile.RegistryHost == profiles[index-1].RegistryHost {
			return ErrInvalid
		}
		if _, duplicate := names[profile.Name]; duplicate {
			return ErrInvalid
		}
		names[profile.Name] = struct{}{}
	}
	return nil
}

// ProjectedOCIRegistryCredentialProvider reads only fixed, chart-projected
// profile paths. A registry selected by a caller can never select a Secret,
// key, absolute path, or another registry's credential.
type ProjectedOCIRegistryCredentialProvider struct {
	root     string
	profiles map[string]OCIRegistryCredentialProfile
	ordered  []OCIRegistryCredentialProfile
	testRoot bool
}

func NewProjectedOCIRegistryCredentialProvider(profiles []OCIRegistryCredentialProfile) (*ProjectedOCIRegistryCredentialProvider, error) {
	return newProjectedOCIRegistryCredentialProviderAt(ProjectedOCICredentialRoot, profiles, false)
}

// RuntimeOCICredentialProvider constructs no credential dependency for public
// registries. Private profiles must all be readable before API or worker
// production composition succeeds.
func RuntimeOCICredentialProvider(ctx context.Context, config RuntimeConfig) (OCIRegistryCredentialProvider, error) {
	if ctx == nil || config.Validate() != nil || !config.Enabled {
		return nil, ErrInvalid
	}
	if len(config.OCICredentialProfiles) == 0 {
		return nil, nil
	}
	provider, err := NewProjectedOCIRegistryCredentialProvider(config.OCICredentialProfiles)
	if err != nil || provider.Probe(ctx) != nil {
		return nil, ErrOCICredentialUnavailable
	}
	return provider, nil
}

func newProjectedOCIRegistryCredentialProviderAt(root string, profiles []OCIRegistryCredentialProfile, testRoot bool) (*ProjectedOCIRegistryCredentialProvider, error) {
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root || root == string(os.PathSeparator) ||
		(!testRoot && root != ProjectedOCICredentialRoot) || len(profiles) < 1 || len(profiles) > maximumOCIHostCount {
		return nil, ErrInvalid
	}
	provider := &ProjectedOCIRegistryCredentialProvider{root: root,
		profiles: make(map[string]OCIRegistryCredentialProfile, len(profiles)),
		ordered:  append([]OCIRegistryCredentialProfile(nil), profiles...), testRoot: testRoot}
	names := make(map[string]struct{}, len(profiles))
	for _, profile := range profiles {
		if profile.Validate() != nil {
			return nil, ErrInvalid
		}
		if _, duplicate := provider.profiles[profile.RegistryHost]; duplicate {
			return nil, ErrInvalid
		}
		if _, duplicate := names[profile.Name]; duplicate {
			return nil, ErrInvalid
		}
		provider.profiles[profile.RegistryHost] = profile
		names[profile.Name] = struct{}{}
	}
	return provider, nil
}

func (p *ProjectedOCIRegistryCredentialProvider) AcquireOCIRegistryCredential(ctx context.Context, host, repository string) (*OCIRegistryCredential, error) {
	if !p.valid() || ctx == nil || ctx.Err() != nil || !validOCIHost(host) ||
		!canonicalOCIRepository("oci://"+host+"/"+repository) {
		return nil, ErrOCICredentialUnavailable
	}
	profile, configured := p.profiles[host]
	if !configured {
		return nil, nil
	}
	credential := &OCIRegistryCredential{AuthHost: profile.AuthHost}
	var err error
	switch profile.Mode {
	case OCICredentialModeBasic:
		credential.Username, err = p.read(ctx, profile.Name, "username", maximumOCIUsernameBytes)
		if err == nil {
			credential.Password, err = p.read(ctx, profile.Name, "password", maximumOCIToken)
		}
	case OCICredentialModeBearer:
		credential.BearerToken, err = p.read(ctx, profile.Name, "token", maximumOCIToken)
	default:
		err = ErrOCICredentialUnavailable
	}
	if err != nil || credential.validate(time.Now().UTC()) != nil {
		credential.Destroy()
		return nil, ErrOCICredentialUnavailable
	}
	return credential, nil
}

// Probe proves that every configured projected profile is readable and
// well-formed without making a provider request. Runtime readiness calls it
// before publishing a fresh renderer lease.
func (p *ProjectedOCIRegistryCredentialProvider) Probe(ctx context.Context) error {
	if !p.valid() || ctx == nil || ctx.Err() != nil {
		return ErrOCICredentialUnavailable
	}
	for _, profile := range p.ordered {
		credential, err := p.AcquireOCIRegistryCredential(ctx, profile.RegistryHost, "kuberploy/readiness-probe")
		if err != nil || credential == nil {
			if credential != nil {
				credential.Destroy()
			}
			return ErrOCICredentialUnavailable
		}
		credential.Destroy()
	}
	return nil
}

func (p *ProjectedOCIRegistryCredentialProvider) valid() bool {
	return p != nil && p.root != "" && filepath.IsAbs(p.root) && filepath.Clean(p.root) == p.root &&
		p.root != string(os.PathSeparator) && (p.testRoot || p.root == ProjectedOCICredentialRoot) &&
		len(p.profiles) > 0 && len(p.profiles) == len(p.ordered)
}

func (p *ProjectedOCIRegistryCredentialProvider) read(ctx context.Context, profile, key string, maximum int64) ([]byte, error) {
	if ctx.Err() != nil || !ociCredentialProfileNameRE.MatchString(profile) ||
		(key != "username" && key != "password" && key != "token") || maximum < 1 || maximum > maximumOCIToken {
		return nil, ErrOCICredentialUnavailable
	}
	info, err := os.Lstat(p.root)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, ErrOCICredentialUnavailable
	}
	root, err := os.OpenRoot(p.root)
	if err != nil {
		return nil, ErrOCICredentialUnavailable
	}
	defer root.Close()
	file, err := root.Open(filepath.Join(profile, key))
	if err != nil {
		return nil, ErrOCICredentialUnavailable
	}
	defer file.Close()
	fileInfo, err := file.Stat()
	if err != nil || !secureOCIProjectedFile(fileInfo) || fileInfo.Size() < 1 || fileInfo.Size() > maximum {
		return nil, ErrOCICredentialUnavailable
	}
	value, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || len(value) < 1 || int64(len(value)) > maximum || ctx.Err() != nil {
		clear(value)
		return nil, ErrOCICredentialUnavailable
	}
	return value, nil
}

func secureOCIProjectedFile(info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() {
		return false
	}
	mode := info.Mode().Perm()
	if mode&0o400 == 0 || mode&0o137 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != 0 && int(stat.Uid) != os.Geteuid() {
		return false
	}
	if mode&0o040 == 0 || int(stat.Gid) == os.Getegid() {
		return true
	}
	groups, err := os.Getgroups()
	if err != nil {
		return false
	}
	for _, group := range groups {
		if group == int(stat.Gid) {
			return true
		}
	}
	return false
}

var _ OCIRegistryCredentialProvider = (*ProjectedOCIRegistryCredentialProvider)(nil)
