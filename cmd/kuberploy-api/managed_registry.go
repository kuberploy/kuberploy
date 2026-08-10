package main

import (
	"fmt"

	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/registry"
)

type managedRegistryAPIStore interface {
	registry.ManagementStore
	registry.RuntimeReadinessStore
	registry.RuntimeReadinessTargetCatalog
}

type managedRegistryAPI struct {
	management httpapi.RegistryManagementService
	readiness  httpapi.ReadinessProbe
}

func newManagedRegistryAPI(config registry.RuntimeConfig, database managedRegistryAPIStore) (*managedRegistryAPI, error) {
	if database == nil || config.Validate() != nil {
		return nil, fmt.Errorf("invalid managed registry API configuration")
	}
	result := &managedRegistryAPI{management: registry.NewManagement(database, registry.DurableCleanupDispatcher{})}
	if !config.Enabled {
		return result, nil
	}
	if _, err := registry.RuntimeIdentityForConfig(config); err != nil {
		return nil, err
	}
	result.readiness = &registry.RuntimeReadinessProbe{Store: database, Targets: database, Config: config,
		MaxAge: registry.ManagedRegistryHeartbeatMaxAge}
	return result, nil
}
