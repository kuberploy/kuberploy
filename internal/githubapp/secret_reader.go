package githubapp

import (
	"context"
	"io"
	"os"
	"path/filepath"
)

const (
	// DefaultProjectedSecretRoot is the read-only volume root used by the API
	// and worker. The chart projects only explicitly selected GitHub App keys
	// below <secret-name>/<key>; the reader never accepts an arbitrary path.
	DefaultProjectedSecretRoot = "/var/run/secrets/kuberploy/github-app"
	defaultSecretReadLimit     = int64(128 << 10)
)

// ProjectedSecretReader reads Kubernetes projected Secret keys from one
// immutable volume boundary. It accepts kubelet's in-root atomic symlinks but
// rejects a root symlink, an escaped resolution, directories, devices, and
// oversized values. Values are never cached; the caller owns and must erase
// each returned byte slice after use.
type ProjectedSecretReader struct {
	root       string
	maximum    int64
	allowTests bool
}

// NewProjectedSecretReader constructs the production reader at the fixed
// release-image mount. A fixed root keeps operator configuration from becoming
// an arbitrary file-read primitive.
func NewProjectedSecretReader() *ProjectedSecretReader {
	return &ProjectedSecretReader{root: DefaultProjectedSecretRoot, maximum: defaultSecretReadLimit}
}

// newProjectedSecretReaderAt exists only for hermetic package tests.
func newProjectedSecretReaderAt(root string, maximum int64) (*ProjectedSecretReader, error) {
	reader := &ProjectedSecretReader{root: root, maximum: maximum, allowTests: true}
	if !reader.validConfig() {
		return nil, ErrInvalidConfig
	}
	return reader, nil
}

func (r *ProjectedSecretReader) validConfig() bool {
	if r == nil || r.maximum < 16 || r.maximum > 1<<20 || r.root == "" || !filepath.IsAbs(r.root) || filepath.Clean(r.root) != r.root || r.root == string(os.PathSeparator) {
		return false
	}
	return r.allowTests || r.root == DefaultProjectedSecretRoot
}

func (r *ProjectedSecretReader) ReadSecret(ctx context.Context, ref SecretRef) ([]byte, error) {
	if !r.validConfig() || ref.validate("projected") != nil {
		return nil, ErrSecretUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rootInfo, err := os.Lstat(r.root)
	if err != nil || rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return nil, ErrSecretUnavailable
	}
	root, err := os.OpenRoot(r.root)
	if err != nil {
		return nil, ErrSecretUnavailable
	}
	defer root.Close()
	file, err := root.Open(filepath.Join(ref.Name, ref.Key))
	if err != nil {
		return nil, ErrSecretUnavailable
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > r.maximum {
		return nil, ErrSecretUnavailable
	}
	value, err := io.ReadAll(io.LimitReader(file, r.maximum+1))
	if err != nil || len(value) < 1 || int64(len(value)) > r.maximum {
		zeroBytes(value)
		return nil, ErrSecretUnavailable
	}
	if err = ctx.Err(); err != nil {
		zeroBytes(value)
		return nil, err
	}
	return value, nil
}

var _ SecretReader = (*ProjectedSecretReader)(nil)
