package main

import (
	"context"

	"github.com/kuberploy/kuberploy/internal/argo"
)

type argoDesiredStateAPI struct {
	store     *argo.PostgreSQLStore
	readiness *argo.ProductionDesiredStateReadinessProbe
}

func newArgoDesiredStateAPI(ctx context.Context, databaseURL string, config argo.ProductionRuntimeConfig) (*argoDesiredStateAPI, error) {
	if !config.Enabled {
		return nil, nil
	}
	if config.Validate() != nil {
		return nil, argo.ErrInvalid
	}
	identity, err := argo.DesiredStateRuntimeIdentityForConfig(config.DesiredState)
	if err != nil {
		return nil, err
	}
	store, err := argo.OpenPostgreSQLStore(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	return &argoDesiredStateAPI{store: store, readiness: &argo.ProductionDesiredStateReadinessProbe{
		Store: store, Identity: identity,
	}}, nil
}

func (a *argoDesiredStateAPI) Close() {
	if a != nil && a.store != nil {
		a.store.Close()
	}
}
