package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/kuberploy/kuberploy/internal/registry"
	"github.com/kuberploy/kuberploy/internal/store/postgres"
)

type managedRegistryRuntime struct {
	controller             *registry.RuntimeController
	readiness              registry.RuntimeReadinessStore
	identity               registry.RuntimeIdentity
	workerID               string
	startedAt              time.Time
	prerequisitesValidated bool
}

func newManagedRegistryRuntime(ctx context.Context, host string, config registry.RuntimeConfig, database *postgres.Store) (*managedRegistryRuntime, error) {
	if !config.Enabled {
		return nil, nil
	}
	if database == nil || config.Validate() != nil {
		return nil, registry.ErrRegistryRuntimeUnavailable
	}
	target, err := database.RegistryTarget(ctx, config.TargetID)
	if err != nil {
		return nil, err
	}
	if err = config.ValidateTarget(target); err != nil {
		return nil, err
	}
	credentials, err := registry.NewProjectedCredentialSource(config.TargetID)
	if err != nil {
		return nil, err
	}
	if err = registry.ProbeDistributionCredentialSource(ctx, config.TargetID, credentials); err != nil {
		return nil, err
	}
	clientConfig := registry.DefaultDistributionClientConfig()
	clientConfig.ExpectedOrigin = config.Endpoint
	clientConfig.AllowPlainHTTP = config.AllowPlainHTTP
	deleter, err := registry.NewDistributionClient(target, clientConfig, credentials, nil)
	if err != nil {
		return nil, err
	}
	observerConfig := registry.DefaultDistributionObserverConfig()
	observerConfig.ExpectedOrigin = config.Endpoint
	observerConfig.AllowPlainHTTP = config.AllowPlainHTTP
	observerConfig.MaximumRepositories = 128
	if _, err = registry.NewDistributionObserver(target, observerConfig, credentials, nil); err != nil {
		return nil, err
	}
	maintenanceConfig := registry.DefaultKubernetesMaintenanceConfig()
	workloads, err := registry.NewInClusterRegistryMaintenanceWorkloads(config, maintenanceConfig.OperationTimeout)
	if err != nil {
		return nil, err
	}
	if _, err = workloads.Inspect(ctx, config); err != nil {
		return nil, err
	}
	maintenance, err := registry.NewKubernetesMaintenanceAdapter(database, database, workloads, config, maintenanceConfig)
	if err != nil {
		return nil, err
	}
	coordinator := registry.NewService(database)
	executor, err := registry.NewCleanupExecutor(coordinator, deleter, maintenance, maintenance, registry.DefaultCleanupExecutorConfig())
	if err != nil {
		return nil, err
	}
	owner := workerLeaseOwner(host+"/"+strconv.Itoa(os.Getpid()), "managed-registry")
	controller := &registry.RuntimeController{
		Store: database, Targets: database, Credentials: credentials, Cleanup: executor, Config: config, Owner: owner,
		LeaseDuration: 2 * time.Minute, HeartbeatInterval: 30 * time.Second, IdleDelay: time.Second,
		MinimumBackoff: 5 * time.Second, MaximumBackoff: 5 * time.Minute, JitterFraction: 0.2,
		ReportError: func(loop string, err error) {
			slog.Warn("managed registry worker iteration failed", "loop", loop, "error", err)
		},
	}
	identity, err := registry.RuntimeIdentityForConfig(config)
	if err != nil {
		return nil, err
	}
	return &managedRegistryRuntime{controller: controller, readiness: database, identity: identity,
		workerID: workerLeaseOwner(host+"/"+strconv.Itoa(os.Getpid()), "registry-ready"), startedAt: time.Now().UTC(),
		prerequisitesValidated: true}, nil
}

func (r *managedRegistryRuntime) Run(ctx context.Context) error {
	if r == nil || r.controller == nil || r.readiness == nil || r.workerID == "" || !r.prerequisitesValidated {
		return fmt.Errorf("managed registry runtime is not configured")
	}
	if err := r.controller.ValidateRuntime(); err != nil {
		return err
	}
	observedAt := time.Now().UTC()
	lease, err := r.readiness.AcquireManagedRegistryReadiness(ctx, registry.RuntimeWorkerObservation{
		WorkerID: r.workerID, RuntimeIdentity: r.identity, StartedAt: r.startedAt, ObservedAt: observedAt,
	}, registry.ManagedRegistryReadinessLease)
	if err != nil {
		return fmt.Errorf("managed registry readiness acquire: %w", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errorsChannel := make(chan error, 2)
	go func() { errorsChannel <- r.controller.Run(runCtx) }()
	go func() { errorsChannel <- r.heartbeat(runCtx, lease) }()
	first := <-errorsChannel
	cancel()
	second := <-errorsChannel
	if first != nil && !errors.Is(first, context.Canceled) {
		return first
	}
	if second != nil && !errors.Is(second, context.Canceled) {
		return second
	}
	return ctx.Err()
}

func (r *managedRegistryRuntime) heartbeat(ctx context.Context, lease registry.RuntimeReadinessLease) error {
	ticker := time.NewTicker(registry.ManagedRegistryHeartbeatInterval)
	defer ticker.Stop()
	current := lease
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case observedAt := <-ticker.C:
			updated, err := r.readiness.HeartbeatManagedRegistryReadiness(ctx, current, observedAt.UTC(), registry.ManagedRegistryReadinessLease)
			if err != nil {
				return fmt.Errorf("managed registry readiness heartbeat: %w", err)
			}
			current = updated
		}
	}
}

func (r *managedRegistryRuntime) Close() {}
