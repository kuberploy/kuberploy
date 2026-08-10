package gitops

import "github.com/kuberploy/kuberploy/internal/appconfig"

// PreviewAppConfig is the pure seam used by the synchronous API. It performs
// no repository access; the worker remains the only component that touches Git.
func PreviewAppConfig(path string, current, candidate []byte) string {
	return appconfig.GitDiff(path, current, candidate)
}
