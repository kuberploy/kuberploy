package imageresolution

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/kuberploy/kuberploy/internal/imagepull"
)

type authorization struct {
	scheme string
	value  []byte
}

func (a *authorization) destroy() {
	if a == nil {
		return
	}
	clear(a.value)
	a.value = nil
	a.scheme = ""
}

func (a authorization) header() (string, bool) {
	if (a.scheme != "Basic" && a.scheme != "Bearer") || len(a.value) < 1 || len(a.value) > 16<<10 {
		return "", false
	}
	for _, value := range a.value {
		if value < '!' || value > '~' {
			return "", false
		}
	}
	return a.scheme + " " + string(a.value), true
}

type CredentialSource interface {
	Authorization(context.Context, imagepull.Profile) (authorization, error)
}

type ProjectedCredentialSource struct {
	Reader imagepull.MaterialReader
}

func NewProjectedCredentialSource() *ProjectedCredentialSource {
	return &ProjectedCredentialSource{Reader: imagepull.NewProjectedMaterialReader()}
}

func (s *ProjectedCredentialSource) Authorization(ctx context.Context, profile imagepull.Profile) (authorization, error) {
	if s == nil || s.Reader == nil || profile.Validate() != nil {
		return authorization{}, ErrUnavailable
	}
	raw, err := s.Reader.ReadDockerConfig(ctx, profile)
	if err != nil {
		clear(raw)
		if ctx.Err() != nil {
			return authorization{}, ctx.Err()
		}
		return authorization{}, ErrUnavailable
	}
	defer clear(raw)
	if imagepull.ValidateDockerConfig(raw, profile) != nil {
		return authorization{}, ErrUnavailable
	}
	var config struct {
		Auths map[string]struct {
			Auth          string `json:"auth"`
			Username      string `json:"username"`
			Password      string `json:"password"`
			IdentityToken string `json:"identitytoken"`
		} `json:"auths"`
	}
	if err = json.Unmarshal(raw, &config); err != nil || len(config.Auths) != 1 {
		return authorization{}, ErrUnavailable
	}
	entry, ok := config.Auths[profile.RegistryServer]
	if !ok {
		return authorization{}, ErrUnavailable
	}
	result := authorization{}
	switch {
	case entry.IdentityToken != "":
		result.scheme, result.value = "Bearer", []byte(entry.IdentityToken)
	case entry.Auth != "":
		decoded, decodeErr := base64.StdEncoding.DecodeString(entry.Auth)
		if decodeErr != nil || base64.StdEncoding.EncodeToString(decoded) != entry.Auth || strings.IndexByte(string(decoded), ':') < 1 {
			clear(decoded)
			return authorization{}, ErrUnavailable
		}
		clear(decoded)
		result.scheme, result.value = "Basic", []byte(entry.Auth)
	case entry.Username != "" && entry.Password != "":
		pair := []byte(entry.Username + ":" + entry.Password)
		result.scheme = "Basic"
		result.value = make([]byte, base64.StdEncoding.EncodedLen(len(pair)))
		base64.StdEncoding.Encode(result.value, pair)
		clear(pair)
	default:
		return authorization{}, ErrUnavailable
	}
	if _, valid := result.header(); !valid {
		result.destroy()
		return authorization{}, ErrUnavailable
	}
	return result, nil
}
