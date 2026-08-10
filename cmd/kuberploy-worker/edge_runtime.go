package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/kuberploy/kuberploy/internal/edge"
)

var edgeWorkerHostPattern = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$`)

type edgeRuntimeStore interface {
	edge.Store
	Close()
}

type edgeRuntime struct {
	controller *edge.RuntimeController
	store      edgeRuntimeStore
}

func newEdgeRuntime(ctx context.Context, databaseURL, host string, config edge.RuntimeConfig) (*edgeRuntime, error) {
	if !config.Enabled {
		return nil, nil
	}
	if config.Validate() != nil {
		return nil, edge.ErrUnavailable
	}
	startedAt := time.Now().UTC()
	workerID, err := edgeWorkerIdentity(host, os.Getpid(), startedAt)
	if err != nil {
		return nil, err
	}
	store, err := edge.OpenPostgreSQLStore(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	reader, err := edge.NewInClusterKubernetesReader()
	if err != nil {
		store.Close()
		return nil, err
	}
	runtime, err := buildEdgeRuntime(config, store, &edge.KubernetesTargetObserver{Reader: reader, Resolver: net.DefaultResolver}, workerID, 1)
	if err != nil {
		store.Close()
		return nil, err
	}
	return runtime, nil
}

func buildEdgeRuntime(config edge.RuntimeConfig, store edgeRuntimeStore, observer edge.TargetObserver, workerID string, workerEpoch int64) (*edgeRuntime, error) {
	if config.Validate() != nil || !config.Enabled || store == nil || observer == nil || workerID == "" || workerEpoch <= 0 {
		return nil, edge.ErrUnavailable
	}
	controller := &edge.RuntimeController{
		Store: store, Observer: observer, Config: config, WorkerID: workerID, WorkerEpoch: workerEpoch,
		Now: func() time.Time { return time.Now().UTC() },
		ReportError: func(code string, err error) {
			slog.Warn("edge-runtime observation failed", "code", code, "error", err)
		},
	}
	if err := controller.Validate(); err != nil {
		return nil, err
	}
	return &edgeRuntime{controller: controller, store: store}, nil
}

func edgeWorkerIdentity(host string, pid int, startedAt time.Time) (string, error) {
	if !edgeWorkerHostPattern.MatchString(host) || pid <= 0 || startedAt.IsZero() {
		return "", edge.ErrUnavailable
	}
	value := "edge-worker:" + host + ":" + strconv.Itoa(pid) + ":" + strconv.FormatInt(startedAt.UnixNano(), 36)
	if len(value) < 16 || len(value) > 128 {
		return "", edge.ErrUnavailable
	}
	return value, nil
}

func (r *edgeRuntime) Run(ctx context.Context) error {
	if r == nil || r.controller == nil || r.store == nil {
		return fmt.Errorf("edge runtime is not configured")
	}
	return r.controller.Run(ctx)
}

func (r *edgeRuntime) Close() {
	if r != nil && r.store != nil {
		r.store.Close()
	}
}
