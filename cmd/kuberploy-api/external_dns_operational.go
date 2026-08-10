package main

import (
	"context"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/edge"
	"github.com/kuberploy/kuberploy/internal/externaldns"
	"github.com/kuberploy/kuberploy/internal/httpapi"
)

type externalDNSRuntimeCatalog interface {
	ListExternalDNSIntegrationsForRuntime(context.Context, int) ([]domain.ExternalDNSIntegration, error)
}

func externalDNSManagementForConfig(store externaldns.Store, config externaldns.OperationalConfig) httpapi.ExternalDNSManagementService {
	if !config.Enabled || config.Validate() != nil || store == nil {
		return nil
	}
	return externaldns.NewManagement(store)
}

type dynamicExternalDNSReadiness struct {
	catalog externalDNSRuntimeCatalog
	store   edge.Store
	base    edge.RuntimeConfig
	config  externaldns.OperationalConfig
	now     func() time.Time
}

func (p *dynamicExternalDNSReadiness) Probe(ctx context.Context) error {
	if p == nil || p.catalog == nil || p.store == nil || p.config.Validate() != nil || !p.config.Enabled {
		return externaldns.ErrRuntimeUnavailable
	}
	items, err := p.catalog.ListExternalDNSIntegrationsForRuntime(ctx, edge.MaximumExternalDNSProfiles)
	if err != nil {
		return externaldns.ErrRuntimeUnavailable
	}
	profiles := make([]edge.ExternalDNSProfile, 0, len(items))
	static := map[string]edge.ExternalDNSProfile{}
	for _, profile := range p.base.Profiles.ExternalDNS {
		static[profile.IntegrationID] = profile
	}
	for _, item := range items {
		if item.Lifecycle != "active" {
			continue
		}
		if item.Mode == externaldns.ModeManaged && (item.ProtectedGitState != "materialized" || item.ProtectedGitRevision != item.RuntimeRevision) {
			return externaldns.ErrRuntimeUnavailable
		}
		if item.Mode == externaldns.ModeManaged {
			profile, profileErr := externaldns.ManagedProfile(item, p.config.Template)
			if profileErr != nil {
				return externaldns.ErrRuntimeUnavailable
			}
			profiles = append(profiles, profile)
		} else {
			profile, ok := static[item.ID]
			if !ok || profile.Revision != item.RuntimeRevision {
				return externaldns.ErrRuntimeUnavailable
			}
			profiles = append(profiles, profile)
		}
	}
	runtime := p.base
	runtime.Enabled = true
	runtime.Profiles.ExternalDNS = profiles
	if runtime.Validate() != nil {
		return externaldns.ErrRuntimeUnavailable
	}
	digest, err := runtime.Digest()
	if err != nil {
		return externaldns.ErrRuntimeUnavailable
	}
	now := time.Now().UTC()
	if p.now != nil {
		now = p.now()
	}
	return p.store.RuntimeReady(ctx, edge.RuntimeContract, digest, runtime.TargetCount(), now, runtime.ReadinessMaxAge)
}

func enableDynamicExternalDNSAPI(api *edgeAPI, catalog externalDNSRuntimeCatalog, base edge.RuntimeConfig, config externaldns.OperationalConfig) error {
	if !config.Enabled {
		return nil
	}
	if api == nil || api.store == nil || catalog == nil {
		return externaldns.ErrRuntimeUnavailable
	}
	api.features.ExternalDNS = true
	api.readiness = &dynamicExternalDNSReadiness{catalog: catalog, store: api.store, base: base, config: config}
	return nil
}
