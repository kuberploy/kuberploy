package observability

import (
	"context"
	"errors"
	"io"
	"os"
)

const projectedBearerTokenPath = "/var/run/secrets/kuberploy/monitoring/token"

// ProjectedBearerToken reads the fixed key mounted by the control-plane chart.
// Kubernetes projected Secret symlinks are intentionally supported. The path
// is not configurable through an API or environment variable, so a caller
// cannot turn this reader into an arbitrary-file primitive.
type ProjectedBearerToken struct {
	path string
}

func NewProjectedBearerToken() ProjectedBearerToken {
	return ProjectedBearerToken{path: projectedBearerTokenPath}
}

func (source ProjectedBearerToken) ReadToken(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, errors.New("monitoring credential unavailable")
	}
	path := source.path
	if path == "" {
		path = projectedBearerTokenPath
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("monitoring credential unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 8 || info.Size() > 8192 {
		return nil, errors.New("monitoring credential unavailable")
	}
	raw, err := io.ReadAll(io.LimitReader(file, 8193))
	if err != nil || len(raw) < 8 || len(raw) > 8192 {
		erase(raw)
		return nil, errors.New("monitoring credential unavailable")
	}
	if err = ctx.Err(); err != nil {
		erase(raw)
		return nil, errors.New("monitoring credential unavailable")
	}
	return raw, nil
}
