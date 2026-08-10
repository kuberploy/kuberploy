package secrets

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
)

const (
	// DefaultFingerprintKeyPath is the immutable Kubernetes Secret projection
	// location used by the API. The projection must contain 32 to 128 raw,
	// random bytes and be readable only by its owner or the Pod's exact fsGroup.
	DefaultFingerprintSecretKey = "runtime-secret-fingerprint.key"
	DefaultFingerprintKeyPath   = "/var/run/secrets/kuberploy-system/" + DefaultFingerprintSecretKey
	DefaultFingerprintKeyID     = "runtime-secrets-hmac-v1"
)

// ProjectedFingerprintKeyProvider reads an operator-selected, immutable file
// path on every call so Kubernetes' atomic Secret projection updates are
// observed. The path and key identity cannot be selected by an API caller.
type ProjectedFingerprintKeyProvider struct {
	path  string
	keyID string
}

func NewDefaultProjectedFingerprintKeyProvider() *ProjectedFingerprintKeyProvider {
	provider, _ := NewProjectedFingerprintKeyProvider(DefaultFingerprintKeyPath, DefaultFingerprintKeyID)
	return provider
}

// NewProjectedFingerprintKeyProvider accepts configuration-time values only.
// Requiring an absolute canonical path prevents working-directory changes or
// request data from redirecting reads.
func NewProjectedFingerprintKeyProvider(path, keyID string) (*ProjectedFingerprintKeyProvider, error) {
	if path == "" || len(path) > 4096 || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) ||
		!keyIDRE.MatchString(keyID) {
		return nil, ErrInvalid
	}
	return &ProjectedFingerprintKeyProvider{path: path, keyID: keyID}, nil
}

func (p *ProjectedFingerprintKeyProvider) ActiveKey(ctx context.Context) (FingerprintKey, error) {
	if p == nil {
		return FingerprintKey{}, ErrFingerprintKeyUnavailable
	}
	if err := ctx.Err(); err != nil {
		return FingerprintKey{}, errors.Join(ErrFingerprintKeyUnavailable, err)
	}
	file, err := openProjectedFile(p.path)
	if err != nil {
		return FingerprintKey{}, ErrFingerprintKeyUnavailable
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !secureProjectedRegularFile(info) {
		return FingerprintKey{}, ErrFingerprintKeyUnavailable
	}
	value, err := io.ReadAll(io.LimitReader(file, 129))
	if err != nil || len(value) < 32 || len(value) > 128 {
		clear(value)
		return FingerprintKey{}, ErrFingerprintKeyUnavailable
	}
	if err := ctx.Err(); err != nil {
		clear(value)
		return FingerprintKey{}, errors.Join(ErrFingerprintKeyUnavailable, err)
	}
	return FingerprintKey{ID: p.keyID, Bytes: value}, nil
}

var _ FingerprintKeyProvider = (*ProjectedFingerprintKeyProvider)(nil)
