// Package imageresolution turns an authorized, bounded registry tag into the
// exact immutable image manifest that the deployment pipeline may persist.
package imageresolution

import (
	"errors"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/imagepull"
)

const MaximumAuthorizedSources = 64

var (
	ErrInvalid     = errors.New("image reference is invalid")
	ErrNotFound    = errors.New("authorized image source was not found")
	ErrForbidden   = errors.New("image source is not authorized")
	ErrUnavailable = errors.New("image tag resolution is unavailable")
	ErrConflict    = errors.New("image tag resolution conflicts with server policy")

	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	tagPattern    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	uuidPattern   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	hostPattern   = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?::[1-9][0-9]{0,4})?$`)
)

type Platform struct {
	OS           string
	Architecture string
	Variant      string
}

func DefaultPlatform() Platform { return Platform{OS: "linux", Architecture: "amd64"} }

func (p Platform) valid() bool {
	if p.OS != "linux" || (p.Architecture != "amd64" && p.Architecture != "arm64") {
		return false
	}
	return p.Variant == "" || p.Architecture == "arm64" && (p.Variant == "v8" || p.Variant == "v9")
}

// RuntimeConfig contains only operator-owned authority. A caller can name a
// tag, but cannot select a target, credential profile, or anonymous policy.
type RuntimeConfig struct {
	Profiles           []imagepull.Profile
	AnonymousTargetIDs []string
	TokenAuthorities   []TokenAuthority
	Platform           Platform
}

// TokenAuthority is an exact operator-owned Bearer challenge binding. Registry
// responses cannot select an arbitrary credential endpoint or token audience.
type TokenAuthority struct {
	TargetID string `json:"targetId"`
	RealmURL string `json:"realmUrl"`
	Service  string `json:"service"`
}

func (c RuntimeConfig) Validate() error {
	if !c.Platform.valid() || len(c.Profiles) > imagepull.MaximumProfiles || len(c.AnonymousTargetIDs) > imagepull.MaximumProfiles || len(c.TokenAuthorities) > imagepull.MaximumProfiles {
		return ErrInvalid
	}
	profiles := append([]imagepull.Profile(nil), c.Profiles...)
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].TargetID < profiles[j].TargetID })
	for index, profile := range profiles {
		if profile.Validate() != nil || index > 0 && profiles[index-1].TargetID == profile.TargetID {
			return ErrInvalid
		}
	}
	for index, targetID := range c.AnonymousTargetIDs {
		if !uuidPattern.MatchString(targetID) || index > 0 && c.AnonymousTargetIDs[index-1] >= targetID {
			return ErrInvalid
		}
		for _, profile := range profiles {
			if profile.TargetID == targetID {
				return ErrInvalid
			}
		}
	}
	for index, authority := range c.TokenAuthorities {
		if !uuidPattern.MatchString(authority.TargetID) || index > 0 && c.TokenAuthorities[index-1].TargetID >= authority.TargetID ||
			authority.Service == "" || len(authority.Service) > 253 || strings.TrimSpace(authority.Service) != authority.Service || strings.ContainsAny(authority.Service, "\x00\r\n") {
			return ErrInvalid
		}
		realm, err := url.Parse(authority.RealmURL)
		if err != nil || realm.Scheme != "https" || realm.Host == "" || realm.Host != strings.ToLower(realm.Host) || realm.User != nil ||
			realm.Path == "" || realm.RawQuery != "" || realm.Fragment != "" || !hostPattern.MatchString(realm.Host) {
			return ErrInvalid
		}
		configured := false
		for _, profile := range profiles {
			configured = configured || profile.TargetID == authority.TargetID
		}
		anonymousIndex := sort.SearchStrings(c.AnonymousTargetIDs, authority.TargetID)
		configured = configured || anonymousIndex < len(c.AnonymousTargetIDs) && c.AnonymousTargetIDs[anonymousIndex] == authority.TargetID
		if !configured {
			return ErrInvalid
		}
	}
	return nil
}

func ConfigFromPullRuntime(config imagepull.RuntimeConfig, anonymousTargetIDs []string, tokenAuthorities []TokenAuthority, platform Platform) (RuntimeConfig, error) {
	if platform == (Platform{}) {
		platform = DefaultPlatform()
	}
	result := RuntimeConfig{AnonymousTargetIDs: append([]string(nil), anonymousTargetIDs...), TokenAuthorities: append([]TokenAuthority(nil), tokenAuthorities...), Platform: platform}
	if config.Enabled {
		if config.Validate() != nil {
			return RuntimeConfig{}, ErrInvalid
		}
		result.Profiles = append([]imagepull.Profile(nil), config.Profiles...)
	} else if config.Validate() != nil {
		return RuntimeConfig{}, ErrInvalid
	}
	sort.Strings(result.AnonymousTargetIDs)
	sort.Slice(result.TokenAuthorities, func(i, j int) bool { return result.TokenAuthorities[i].TargetID < result.TokenAuthorities[j].TargetID })
	if result.Validate() != nil {
		return RuntimeConfig{}, ErrInvalid
	}
	return result, nil
}

func (c RuntimeConfig) tokenAuthority(targetID string) *TokenAuthority {
	index := sort.Search(len(c.TokenAuthorities), func(index int) bool { return c.TokenAuthorities[index].TargetID >= targetID })
	if index == len(c.TokenAuthorities) || c.TokenAuthorities[index].TargetID != targetID {
		return nil
	}
	value := c.TokenAuthorities[index]
	return &value
}

func (c RuntimeConfig) authority(targetID string) (*imagepull.Profile, bool, bool) {
	if c.Validate() != nil {
		return nil, false, false
	}
	for index := range c.Profiles {
		if c.Profiles[index].TargetID == targetID {
			profile := c.Profiles[index]
			return &profile, false, true
		}
	}
	index := sort.SearchStrings(c.AnonymousTargetIDs, targetID)
	return nil, index < len(c.AnonymousTargetIDs) && c.AnonymousTargetIDs[index] == targetID, true
}

type AuthorizedSource struct {
	Target domain.RegistryTarget
	Policy domain.ServiceRegistryPolicy
}

func (s AuthorizedSource) Validate(applicationID string) error {
	server, err := canonicalRegistryServer(s.Target.Endpoint)
	if err != nil || server == "" || s.Policy.RegistryTargetID != s.Target.ID || s.Policy.ServiceID != applicationID ||
		s.Policy.Repository == "" || !validRepository(s.Policy.Repository) ||
		!(s.Policy.Repository == s.Target.RepositoryPrefix || strings.HasPrefix(s.Policy.Repository, strings.TrimSuffix(s.Target.RepositoryPrefix, "/")+"/")) {
		return ErrConflict
	}
	return nil
}

type TagReference struct {
	Server     string
	Repository string
	Tag        string
}

func ParseTagReference(image string) (TagReference, error) {
	if image == "" || len(image) > 512 || strings.TrimSpace(image) != image || strings.ContainsAny(image, "@\x00\r\n\t ") {
		return TagReference{}, ErrInvalid
	}
	slash := strings.IndexByte(image, '/')
	colon := strings.LastIndexByte(image, ':')
	if slash < 1 || colon <= slash+1 || colon == len(image)-1 {
		return TagReference{}, ErrInvalid
	}
	server, repository, tag := image[:slash], image[slash+1:colon], image[colon+1:]
	if !hostPattern.MatchString(server) || server != strings.ToLower(server) || !validRepository(repository) || !tagPattern.MatchString(tag) {
		return TagReference{}, ErrInvalid
	}
	if portSeparator := strings.LastIndexByte(server, ':'); portSeparator >= 0 {
		port, err := strconv.ParseUint(server[portSeparator+1:], 10, 16)
		if err != nil || port == 0 {
			return TagReference{}, ErrInvalid
		}
	}
	return TagReference{Server: server, Repository: repository, Tag: tag}, nil
}

func IsImmutableImage(image string) bool {
	separator := strings.LastIndexByte(image, '@')
	if separator < 1 || separator == len(image)-1 || strings.ContainsAny(image, "\x00\r\n\t ") {
		return false
	}
	reference, err := ParseRepository(image[:separator])
	return err == nil && reference.Server != "" && digestPattern.MatchString(image[separator+1:])
}

func ParseRepository(value string) (TagReference, error) {
	if value == "" || len(value) > 383 || strings.ContainsAny(value, "@:\x00\r\n\t ") {
		// A colon is allowed only in the server port, handled below.
		if slash := strings.IndexByte(value, '/'); slash < 1 || strings.Contains(value[slash+1:], ":") {
			return TagReference{}, ErrInvalid
		}
	}
	slash := strings.IndexByte(value, '/')
	if slash < 1 {
		return TagReference{}, ErrInvalid
	}
	server, repository := value[:slash], value[slash+1:]
	if !hostPattern.MatchString(server) || server != strings.ToLower(server) || !validRepository(repository) {
		return TagReference{}, ErrInvalid
	}
	return TagReference{Server: server, Repository: repository}, nil
}

func canonicalRegistryServer(endpoint string) (string, error) {
	if endpoint == "" || strings.TrimSpace(endpoint) != endpoint || strings.ContainsAny(endpoint, "\x00\r\n") {
		return "", ErrConflict
	}
	raw := endpoint
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Host != strings.ToLower(u.Host) || u.User != nil ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" || !hostPattern.MatchString(u.Host) {
		return "", ErrConflict
	}
	return u.Host, nil
}

func validRepository(repository string) bool {
	if len(repository) < 1 || len(repository) > 255 || repository != strings.ToLower(repository) || strings.HasPrefix(repository, "/") || strings.HasSuffix(repository, "/") {
		return false
	}
	for _, component := range strings.Split(repository, "/") {
		if !validRepositoryComponent(component) {
			return false
		}
	}
	return true
}

func validRepositoryComponent(component string) bool {
	if component == "" {
		return false
	}
	isAlphaNumeric := func(value byte) bool { return value >= 'a' && value <= 'z' || value >= '0' && value <= '9' }
	for index := 0; index < len(component); {
		if !isAlphaNumeric(component[index]) {
			return false
		}
		for index < len(component) && isAlphaNumeric(component[index]) {
			index++
		}
		if index == len(component) {
			return true
		}
		switch component[index] {
		case '.':
			index++
		case '_':
			index++
			if index < len(component) && component[index] == '_' {
				index++
			}
		case '-':
			for index < len(component) && component[index] == '-' {
				index++
			}
		default:
			return false
		}
		if index == len(component) || !isAlphaNumeric(component[index]) {
			return false
		}
	}
	return true
}
