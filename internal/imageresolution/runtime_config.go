package imageresolution

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/kuberploy/kuberploy/internal/imagepull"
)

const (
	AnonymousTargetsEnv = "KUBERPLOY_IMAGE_TAG_RESOLUTION_ANONYMOUS_TARGET_IDS"
	TokenAuthoritiesEnv = "KUBERPLOY_IMAGE_TAG_RESOLUTION_TOKEN_AUTHORITIES"
	PlatformEnv         = "KUBERPLOY_IMAGE_TAG_RESOLUTION_PLATFORM"
)

func RuntimeConfigFromEnvironment(pullConfig imagepull.RuntimeConfig) (RuntimeConfig, error) {
	return RuntimeConfigFromLookup(pullConfig, os.LookupEnv)
}

func RuntimeConfigFromLookup(pullConfig imagepull.RuntimeConfig, lookup func(string) (string, bool)) (RuntimeConfig, error) {
	if lookup == nil {
		return RuntimeConfig{}, ErrInvalid
	}
	anonymous := []string(nil)
	if value, present := lookup(AnonymousTargetsEnv); present {
		if value == "" || strings.TrimSpace(value) != value {
			return RuntimeConfig{}, ErrInvalid
		}
		anonymous = strings.Split(value, ",")
		if !sort.StringsAreSorted(anonymous) {
			return RuntimeConfig{}, ErrInvalid
		}
	}
	authorities := []TokenAuthority(nil)
	if value, present := lookup(TokenAuthoritiesEnv); present {
		if value == "" || len(value) > 64<<10 || strings.TrimSpace(value) != value || decodeTokenAuthorities([]byte(value), &authorities) != nil {
			return RuntimeConfig{}, ErrInvalid
		}
	}
	platform := DefaultPlatform()
	if value, present := lookup(PlatformEnv); present {
		parts := strings.Split(value, "/")
		if len(parts) < 2 || len(parts) > 3 {
			return RuntimeConfig{}, ErrInvalid
		}
		platform = Platform{OS: parts[0], Architecture: parts[1]}
		if len(parts) == 3 {
			platform.Variant = parts[2]
		}
	}
	return ConfigFromPullRuntime(pullConfig, anonymous, authorities, platform)
}

func decodeTokenAuthorities(raw []byte, result *[]TokenAuthority) error {
	if rejectDuplicateJSONKeys(raw, 4) != nil {
		return ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(result); err != nil || len(*result) > imagepull.MaximumProfiles {
		return ErrInvalid
	}
	if token, err := decoder.Token(); err != io.EOF || token != nil {
		return ErrInvalid
	}
	for index := range *result {
		if index > 0 && (*result)[index-1].TargetID >= (*result)[index].TargetID {
			return ErrInvalid
		}
	}
	return nil
}
