package main

import (
	"context"
	"fmt"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/registry"
)

type managedRegistryAPIStore interface {
	registry.ManagementStore
	registry.ProtectionRefresher
	registry.RuntimeReadinessStore
	registry.RuntimeReadinessTargetCatalog
	PutRegistryTarget(context.Context, domain.RegistryTarget) (domain.RegistryTarget, error)
}

type managedRegistryAPI struct {
	management httpapi.RegistryManagementService
	readiness  httpapi.ReadinessProbe
}

func newManagedRegistryAPI(config registry.RuntimeConfig, database managedRegistryAPIStore) (*managedRegistryAPI, error) {
	if database == nil || config.Validate() != nil {
		return nil, fmt.Errorf("invalid managed registry API configuration")
	}
	options := []registry.ManagementOption{registry.WithManagementProtectionRefresher(database)}
	if config.Enabled {
		target, err := config.ManagedTarget()
		if err != nil {
			return nil, err
		}
		persisted, err := database.PutRegistryTarget(context.Background(), target)
		if err != nil {
			return nil, fmt.Errorf("persist managed registry target: %w", err)
		}
		if err = config.ValidateTarget(persisted); err != nil {
			return nil, fmt.Errorf("validate managed registry target: %w", err)
		}
		options = append(options, registry.WithManagedTarget(target))
	}
	result := &managedRegistryAPI{management: registry.NewManagement(database, registry.DurableCleanupDispatcher{}, options...)}
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
