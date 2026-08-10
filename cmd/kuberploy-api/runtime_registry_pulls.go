package main

import (
	"context"
	"time"

	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/imagepull"
)

type runtimeRegistryPullAPIStore interface {
	imagepull.Store
	Close()
}

type runtimeRegistryPullAPI struct {
	readiness httpapi.ReadinessProbe
	store     runtimeRegistryPullAPIStore
}

func newRuntimeRegistryPullAPI(ctx context.Context, databaseURL string, config imagepull.RuntimeConfig) (*runtimeRegistryPullAPI, error) {
	if !config.Enabled {
		return nil, nil
	}
	if config.Validate() != nil {
		return nil, imagepull.ErrUnavailable
	}
	store, err := imagepull.OpenPostgreSQLStore(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	api, err := buildRuntimeRegistryPullAPI(config, store, time.Now().UTC())
	if err != nil {
		store.Close()
		return nil, err
	}
	return api, nil
}

func buildRuntimeRegistryPullAPI(config imagepull.RuntimeConfig, store runtimeRegistryPullAPIStore, now time.Time) (*runtimeRegistryPullAPI, error) {
	if config.Validate() != nil || !config.Enabled || store == nil || now.IsZero() {
		return nil, imagepull.ErrUnavailable
	}
	probe := &imagepull.ReadinessProbe{Store: store, Config: config}
	return &runtimeRegistryPullAPI{readiness: probe, store: store}, nil
}

func (a *runtimeRegistryPullAPI) Close() {
	if a != nil && a.store != nil {
		a.store.Close()
	}
}

func runtimeRegistryPullReadiness(runtime *runtimeRegistryPullAPI) httpapi.ReadinessProbe {
	if runtime == nil {
		return nil
	}
	return runtime.readiness
}
