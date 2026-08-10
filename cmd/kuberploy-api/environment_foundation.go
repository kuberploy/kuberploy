package main

import (
	"context"

	"github.com/kuberploy/kuberploy/internal/environmentfoundation"
	"github.com/kuberploy/kuberploy/internal/httpapi"
)

type environmentFoundationAPI struct {
	store     *environmentfoundation.PostgresStore
	readiness *environmentfoundation.RuntimeReadinessProbe
}

func newEnvironmentFoundationAPI(ctx context.Context, databaseURL string, config environmentfoundation.RuntimeConfig) (*environmentFoundationAPI, error) {
	if !config.Enabled {
		return nil, nil
	}
	if config.Validate() != nil {
		return nil, environmentfoundation.ErrInvalid
	}
	store, err := environmentfoundation.OpenPostgresStore(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	return &environmentFoundationAPI{store: store,
		readiness: &environmentfoundation.RuntimeReadinessProbe{Store: store, Catalog: store, Config: config}}, nil
}

func (a *environmentFoundationAPI) Close() {
	if a != nil && a.store != nil {
		a.store.Close()
	}
}

type combinedReadiness []httpapi.ReadinessProbe

func (p combinedReadiness) Probe(ctx context.Context) error {
	for _, probe := range p {
		if probe == nil {
			return environmentfoundation.ErrUnavailable
		}
		if err := probe.Probe(ctx); err != nil {
			return err
		}
	}
	return nil
}
