package registry

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maximumProjectedRegistryCredentialBytes = 16 << 10

// ProjectedCredentialSource is fixed to one operator-configured target and
// one projected username/password pair. Neither a DB row nor a request can
// select a filesystem path.
type ProjectedCredentialSource struct {
	targetID     string
	usernamePath string
	passwordPath string
}

func NewProjectedCredentialSource(targetID string) (*ProjectedCredentialSource, error) {
	if !registryUUIDRE.MatchString(targetID) {
		return nil, ErrDistributionInvalidConfig
	}
	return &ProjectedCredentialSource{targetID: targetID, usernamePath: managedRegistryUsernamePath, passwordPath: managedRegistryPasswordPath}, nil
}

func (s *ProjectedCredentialSource) Authorization(ctx context.Context, targetID string) (DistributionAuthorization, error) {
	if s == nil || ctx == nil || targetID != s.targetID {
		return DistributionAuthorization{}, ErrDistributionScopeMismatch
	}
	username, err := readBoundedCredentialFile(ctx, s.usernamePath)
	if err != nil {
		return DistributionAuthorization{}, ErrDistributionCredentialUnavailable
	}
	defer zeroBytes(username)
	password, err := readBoundedCredentialFile(ctx, s.passwordPath)
	if err != nil {
		return DistributionAuthorization{}, ErrDistributionCredentialUnavailable
	}
	defer zeroBytes(password)
	return NewDistributionBasicAuthorization(string(username), password)
}

// ProbeDistributionCredentialSource verifies that the runtime can read and
// construct a bounded authorization value, then erases it without making a
// provider request.
func ProbeDistributionCredentialSource(ctx context.Context, targetID string, source DistributionCredentialSource) error {
	if ctx == nil || ctx.Err() != nil || targetID == "" || source == nil {
		return ErrDistributionCredentialUnavailable
	}
	authorization, err := source.Authorization(ctx, targetID)
	if err != nil {
		return ErrDistributionCredentialUnavailable
	}
	defer authorization.destroy()
	if !authorization.valid() {
		return ErrDistributionCredentialUnavailable
	}
	return nil
}

func readBoundedCredentialFile(ctx context.Context, path string) ([]byte, error) {
	if ctx.Err() != nil || (path != managedRegistryUsernamePath && path != managedRegistryPasswordPath) {
		return nil, errors.New("projected registry credential unavailable")
	}
	rootInfo, err := os.Lstat(managedRegistryCredentialRoot)
	if err != nil || rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return nil, errors.New("projected registry credential unavailable")
	}
	root, err := os.OpenRoot(managedRegistryCredentialRoot)
	if err != nil {
		return nil, errors.New("projected registry credential unavailable")
	}
	defer root.Close()
	file, err := root.Open(filepath.Base(path))
	if err != nil {
		return nil, errors.New("projected registry credential unavailable")
	}
	info, statErr := file.Stat()
	if statErr != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > maximumProjectedRegistryCredentialBytes {
		file.Close() //nolint:errcheck
		return nil, errors.New("projected registry credential unavailable")
	}
	value, readErr := io.ReadAll(io.LimitReader(file, maximumProjectedRegistryCredentialBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(value) == 0 || len(value) > maximumProjectedRegistryCredentialBytes {
		zeroBytes(value)
		return nil, errors.New("projected registry credential unavailable")
	}
	trimmed := strings.TrimSpace(string(value))
	if trimmed == "" || len(trimmed) != len(value) || strings.IndexFunc(trimmed, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		zeroBytes(value)
		return nil, errors.New("projected registry credential unavailable")
	}
	return value, nil
}

var _ DistributionCredentialSource = (*ProjectedCredentialSource)(nil)
