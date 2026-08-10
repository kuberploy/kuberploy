package secrets

import (
	"os"
	"path/filepath"
	"syscall"
)

// openProjectedFile keeps kubelet's in-directory atomic symlinks usable while
// ensuring every component is resolved beneath an already-open, non-symlink
// projection directory. Callers deliberately collapse its errors so host
// filesystem details never cross the provider boundary.
func openProjectedFile(path string) (*os.File, error) {
	directory := filepath.Dir(path)
	info, err := os.Lstat(directory)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, os.ErrNotExist
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.Open(filepath.Base(path))
}

// secureProjectedRegularFile accepts a private owner-readable file and the
// Kubernetes non-root projection form where only a process group may also
// read it. Kubernetes applies fsGroup ownership to Secret volumes; rejecting
// 0440 would make a root-owned projection unreadable by the non-root API and
// worker. Write/execute permissions for the group and every permission for
// other users remain forbidden.
func secureProjectedRegularFile(info os.FileInfo) bool {
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
