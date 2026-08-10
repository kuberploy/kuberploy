package imagepull

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

type ProjectedMaterialReader struct {
	root       string
	maximum    int64
	allowTests bool
}

func NewProjectedMaterialReader() *ProjectedMaterialReader {
	return &ProjectedMaterialReader{root: SourceRoot, maximum: MaximumDockerConfigBytes}
}

func newProjectedMaterialReaderAt(root string, maximum int64) (*ProjectedMaterialReader, error) {
	reader := &ProjectedMaterialReader{root: root, maximum: maximum, allowTests: true}
	if !reader.valid() {
		return nil, ErrInvalid
	}
	return reader, nil
}

func (r *ProjectedMaterialReader) valid() bool {
	return r != nil && r.root != "" && filepath.IsAbs(r.root) && filepath.Clean(r.root) == r.root && r.root != string(os.PathSeparator) &&
		r.maximum >= 16 && r.maximum <= MaximumDockerConfigBytes && (r.allowTests || r.root == SourceRoot)
}

func (r *ProjectedMaterialReader) ReadDockerConfig(ctx context.Context, profile Profile) ([]byte, error) {
	if !r.valid() || profile.Validate() != nil {
		return nil, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rootInfo, err := os.Lstat(r.root)
	if err != nil || rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return nil, ErrUnavailable
	}
	root, err := os.OpenRoot(r.root)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer root.Close()
	// Root.Open resolves kubelet's projected-volume symlink chain beneath the
	// already-open directory and rejects escapes. Keeping the traversal rooted
	// also closes the check/reopen race that EvalSymlinks followed by os.Open
	// would otherwise leave.
	file, err := root.Open(filepath.Join(profile.Name, "dockerconfigjson"))
	if err != nil {
		return nil, ErrUnavailable
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !secureProjectedCredentialFile(info) || info.Size() < 1 || info.Size() > r.maximum {
		return nil, ErrUnavailable
	}
	value, err := io.ReadAll(io.LimitReader(file, r.maximum+1))
	if err != nil || len(value) < 1 || int64(len(value)) > r.maximum {
		clearBytes(value)
		return nil, ErrUnavailable
	}
	if err = ctx.Err(); err != nil {
		clearBytes(value)
		return nil, err
	}
	return value, nil
}

func secureProjectedCredentialFile(info os.FileInfo) bool {
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
	if mode&0o040 == 0 {
		return true
	}
	fileGroup := int(stat.Gid)
	if fileGroup == os.Getegid() {
		return true
	}
	groups, err := os.Getgroups()
	if err != nil {
		return false
	}
	for _, group := range groups {
		if group == fileGroup {
			return true
		}
	}
	return false
}

var _ MaterialReader = (*ProjectedMaterialReader)(nil)
