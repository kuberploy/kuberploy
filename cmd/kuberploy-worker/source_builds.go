package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/kuberploy/kuberploy/internal/builds"
	"github.com/kuberploy/kuberploy/internal/githubapp"
	"github.com/kuberploy/kuberploy/internal/gitssh"
)

type sourceBuildRuntime struct {
	store     *builds.PostgreSQLStore
	gitSSH    *gitssh.PostgresRepository
	runner    *builds.WorkerRunner
	capacity  builds.BuilderCapacityProbe
	settings  builds.BuilderPlatformSettingsReader
	identity  builds.SourceBuildRuntimeIdentity
	workerID  string
	startedAt time.Time
}

func newSourceBuildRuntime(ctx context.Context, databaseURL, host string, config builds.WorkerRuntimeConfig, registry builds.ReleaseProjectionRegistry) (*sourceBuildRuntime, error) {
	if !config.Enabled {
		return nil, nil
	}
	if registry == nil {
		return nil, builds.ErrInvalid
	}
	if err := githubapp.ProbeProjectedWorkerRuntime(ctx, config.GitHub); err != nil {
		return nil, err
	}
	provider, err := githubapp.NewProjectedClient(config.GitHub)
	if err != nil {
		return nil, err
	}
	kubernetes, err := builds.NewInClusterKubernetesAdapter(config.BuilderNamespace, config.NodeIsolation)
	if err != nil {
		return nil, err
	}
	store, err := builds.OpenPostgreSQLStore(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	settings := &builds.BuilderPlatformSettingsService{Store: store, Defaults: builds.DefaultBuilderPlatformSettings(config)}
	gitSSHEncryption, err := gitssh.EncryptionFromEnvironment()
	if err != nil {
		store.Close()
		return nil, err
	}
	var gitSSHRepository *gitssh.PostgresRepository
	var gitSSHService *gitssh.Service
	if gitSSHEncryption != nil {
		gitSSHRepository, err = gitssh.OpenPostgresRepository(ctx, databaseURL)
		if err != nil {
			store.Close()
			return nil, err
		}
		gitSSHService, err = gitssh.NewService(gitSSHRepository, gitSSHEncryption)
		if err != nil {
			gitSSHRepository.Close()
			store.Close()
			return nil, err
		}
	}
	processIdentity := host + "/" + strconv.Itoa(os.Getpid())
	runtimeIdentity, err := builds.RuntimeIdentity(config)
	if err != nil {
		store.Close()
		return nil, err
	}
	deliveryOwner := workerLeaseOwner(processIdentity, "deliveries")
	buildOwner := workerLeaseOwner(processIdentity, "builds")
	releaseOwner := workerLeaseOwner(processIdentity, "release-projection")
	const lease = 30 * time.Second
	deliveries := &builds.WebhookService{Provider: provider, Store: store, Owner: deliveryOwner, LeaseDuration: lease, Runtime: config, Settings: settings}
	controller := &builds.BuildController{Store: store, Provider: provider, GitSSH: gitSSHService, Kubernetes: kubernetes, Settings: settings, Owner: buildOwner, LeaseDuration: lease}
	releases := &builds.ReleaseProjector{Store: store, Registry: registry, Owner: releaseOwner, LeaseDuration: lease}
	runner := &builds.WorkerRunner{
		Store: store, Deliveries: deliveries, Builds: controller, Releases: releases,
		DeliveryOwner: deliveryOwner, BuildOwner: buildOwner, ReleaseOwner: releaseOwner, DeliveryBatch: 50,
		IdleDelay: time.Second, MinimumErrorDelay: time.Second, MaximumErrorDelay: 30 * time.Second,
		ReportError: func(loop string, err error) {
			slog.Warn("GitHub source build worker iteration failed", "loop", loop, "error", err)
		},
	}
	return &sourceBuildRuntime{store: store, gitSSH: gitSSHRepository, runner: runner, capacity: kubernetes, settings: settings, identity: runtimeIdentity,
		workerID: workerLeaseOwner(processIdentity, "runtime"), startedAt: time.Now().UTC()}, nil
}

func (r *sourceBuildRuntime) Run(ctx context.Context) error {
	if r == nil || r.runner == nil || r.store == nil || r.workerID == "" {
		return fmt.Errorf("source build runtime is not configured")
	}
	if err := r.runner.ValidateRuntime(); err != nil {
		return err
	}
	if err := r.observe(ctx, time.Now().UTC()); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errors := make(chan error, 2)
	go func() { errors <- r.runner.Run(runCtx) }()
	go func() { errors <- r.heartbeat(runCtx) }()
	first := <-errors
	cancel()
	second := <-errors
	if first != nil && first != context.Canceled {
		return first
	}
	if second != nil && second != context.Canceled {
		return second
	}
	return ctx.Err()
}

func (r *sourceBuildRuntime) heartbeat(ctx context.Context) error {
	ticker := time.NewTicker(builds.SourceBuildHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case observedAt := <-ticker.C:
			if err := r.observe(ctx, observedAt.UTC()); err != nil {
				return fmt.Errorf("source build readiness heartbeat: %w", err)
			}
		}
	}
}

func (r *sourceBuildRuntime) observe(ctx context.Context, observedAt time.Time) error {
	capacityReady := false
	if r.capacity != nil {
		capacityErr := r.capacity.BuilderCapacityReady(ctx)
		if r.settings != nil {
			settings, settingsErr := r.settings.Current(ctx)
			if settingsErr != nil {
				capacityErr = settingsErr
			} else {
				if scheduled, ok := r.capacity.(interface {
					BuilderCapacityReadyFor(context.Context, bool) error
				}); ok {
					capacityErr = scheduled.BuilderCapacityReadyFor(ctx, settings.NodeIsolation)
				}
			}
		}
		capacityReady = capacityErr == nil
	}
	return r.store.ObserveSourceBuildWorker(ctx, builds.SourceBuildWorkerObservation{WorkerID: r.workerID,
		SourceBuildRuntimeIdentity: r.identity, BuilderCapacityReady: capacityReady, StartedAt: r.startedAt, ObservedAt: observedAt})
}

func (r *sourceBuildRuntime) Close() {
	if r != nil && r.store != nil {
		r.store.Close()
	}
	if r != nil && r.gitSSH != nil {
		r.gitSSH.Close()
	}
}

func workerLeaseOwner(processIdentity, role string) string {
	digest := sha256.Sum256([]byte(processIdentity + "\x00" + role))
	return "worker-" + role + "-" + hex.EncodeToString(digest[:8])
}
