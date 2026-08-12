package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/kuberploy/kuberploy/internal/imagepull"
)

type runtimeRegistryPullStore interface {
	imagepull.Store
	Close()
}

type runtimeRegistryPullRuntime struct {
	controller *imagepull.RuntimeController
	store      runtimeRegistryPullStore
}

func newRuntimeRegistryPullRuntime(ctx context.Context, databaseURL, host string, config imagepull.RuntimeConfig, projection *gitProjectionRuntime) (*runtimeRegistryPullRuntime, error) {
	if !config.Enabled {
		return nil, nil
	}
	if config.Validate() != nil || projection == nil || projection.store == nil {
		return nil, imagepull.ErrUnavailable
	}
	store, err := imagepull.OpenPostgreSQLStore(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	secretAPI, err := imagepull.NewInClusterKubernetesSecretAPI()
	if err != nil {
		store.Close()
		return nil, err
	}
	startedAt := time.Now().UTC()
	workerID := "runtime-pull-worker:" + host + ":" + strconv.Itoa(os.Getpid()) + ":" + strconv.FormatInt(startedAt.UnixNano(), 36)
	runtime, err := buildRuntimeRegistryPullRuntime(config, store, imagepull.NewProjectedMaterialReader(), secretAPI,
		projection.store, workerID, 1, startedAt)
	if err != nil {
		store.Close()
		return nil, err
	}
	return runtime, nil
}

func buildRuntimeRegistryPullRuntime(
	config imagepull.RuntimeConfig,
	store runtimeRegistryPullStore,
	reader imagepull.MaterialReader,
	secretAPI imagepull.SecretAPI,
	projections imagepull.ProjectionInvalidator,
	workerID string,
	workerEpoch int64,
	now time.Time,
) (*runtimeRegistryPullRuntime, error) {
	if config.Validate() != nil || !config.Enabled || store == nil || reader == nil || secretAPI == nil || projections == nil || now.IsZero() {
		return nil, imagepull.ErrUnavailable
	}
	controller := &imagepull.RuntimeController{Store: store, Reader: reader, Secrets: secretAPI, Projections: projections, Config: config,
		WorkerID: workerID, WorkerEpoch: workerEpoch, Now: func() time.Time { return time.Now().UTC() },
		ReportError: func(loop string, err error) {
			slog.Warn("runtime registry-pull worker iteration failed", "loop", loop, "error", err)
		},
	}
	if err := controller.Validate(); err != nil {
		return nil, err
	}
	return &runtimeRegistryPullRuntime{controller: controller, store: store}, nil
}

func (r *runtimeRegistryPullRuntime) Run(ctx context.Context) error {
	if r == nil || r.controller == nil || r.store == nil {
		return fmt.Errorf("runtime registry-pull controller is not configured")
	}
	return r.controller.Run(ctx)
}

func (r *runtimeRegistryPullRuntime) Close() {
	if r != nil && r.store != nil {
		r.store.Close()
	}
}
