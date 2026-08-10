package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/kuberploy/kuberploy/internal/argo"
)

type argoObservationRuntime struct {
	store       *argo.PostgreSQLStore
	coordinator *argo.ObservationCoordinator
}

func newArgoObservationRuntime(ctx context.Context, databaseURL, host string, config argo.ObservationRuntimeConfig) (*argoObservationRuntime, error) {
	if !config.Enabled {
		return nil, nil
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	client, err := argo.NewInClusterApplicationClient()
	if err != nil {
		return nil, err
	}
	store, err := argo.OpenPostgreSQLStore(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	resolver, err := argo.NewPostgreSQLObservationTargetResolverFromStore(store)
	if err != nil {
		store.Close()
		return nil, err
	}
	owner := workerLeaseOwner(host+"/"+strconv.Itoa(os.Getpid()), "argo-observer")
	coordinator := &argo.ObservationCoordinator{
		Store: store, Source: client, Resolver: resolver, Namespace: config.Namespace, Owner: owner,
		LeaseDuration: time.Minute, HeartbeatInterval: 15 * time.Second, WorkTimeout: 5 * time.Minute,
		PollInterval: config.PollInterval, MinimumBackoff: 5 * time.Second, MaximumBackoff: 5 * time.Minute, IdleDelay: time.Second,
		ReportError: func(err error) { slog.Warn("Argo observation iteration failed", "error", err) },
	}
	if err = coordinator.Validate(); err != nil {
		store.Close()
		return nil, err
	}
	return &argoObservationRuntime{store: store, coordinator: coordinator}, nil
}

func (r *argoObservationRuntime) Run(ctx context.Context) error {
	if r == nil || r.coordinator == nil || r.store == nil {
		return fmt.Errorf("Argo observation runtime is not configured")
	}
	return r.coordinator.Run(ctx)
}

func (r *argoObservationRuntime) Close() {
	if r != nil && r.store != nil {
		r.store.Close()
	}
}
