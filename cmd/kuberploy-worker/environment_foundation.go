package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/kuberploy/kuberploy/internal/environmentfoundation"
)

type environmentFoundationRuntime struct {
	store     *environmentfoundation.PostgresStore
	runtime   *environmentfoundation.Runtime
	readiness *environmentfoundation.RuntimeReadinessProbe
}

func newEnvironmentFoundationRuntime(ctx context.Context, databaseURL, host string,
	config environmentfoundation.RuntimeConfig, projection *gitProjectionRuntime) (*environmentFoundationRuntime, error) {
	if !config.Enabled {
		return nil, nil
	}
	if config.Validate() != nil || projection == nil || projection.store == nil || projection.writeManager == nil ||
		projection.headVerifier.Client == nil {
		return nil, environmentfoundation.ErrInvalid
	}
	store, err := environmentfoundation.OpenPostgresStore(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	now := func() time.Time { return time.Now().UTC() }
	publisher := &environmentfoundation.ProtectedGitPublisher{Store: store, Bindings: projection.store,
		Provider: projection.headVerifier, Manager: projection.writeManager, Publisher: config.Publisher, Now: now}
	workerID := workerLeaseOwner(host+"/"+strconv.Itoa(os.Getpid()), "environment-foundation")
	controller := &environmentfoundation.Controller{Store: store, Publisher: publisher, Profile: config.Profile,
		WorkerID: workerID, WorkerEpoch: 1, WorkLease: 2 * time.Minute,
		MinimumBackoff: 5 * time.Second, MaximumBackoff: 5 * time.Minute, Now: now}
	runtime := &environmentfoundation.Runtime{Store: store, Catalog: store, Controller: controller, Config: config,
		WorkerEpoch: 1, StartedAt: now(), Now: now}
	if runtime.Validate() != nil {
		store.Close()
		return nil, environmentfoundation.ErrInvalid
	}
	return &environmentFoundationRuntime{store: store, runtime: runtime,
		readiness: &environmentfoundation.RuntimeReadinessProbe{Store: store, Catalog: store, Config: config, Now: now}}, nil
}

func (r *environmentFoundationRuntime) Run(ctx context.Context) error {
	if r == nil || r.store == nil || r.runtime == nil {
		return fmt.Errorf("environment foundation runtime is not configured")
	}
	return r.runtime.Run(ctx)
}

func (r *environmentFoundationRuntime) Close() {
	if r != nil && r.store != nil {
		r.store.Close()
	}
}
