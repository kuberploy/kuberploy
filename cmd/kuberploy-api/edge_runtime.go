package main

import (
	"context"
	"time"

	"github.com/kuberploy/kuberploy/internal/edge"
	"github.com/kuberploy/kuberploy/internal/httpapi"
)

type edgeAPIStore interface {
	edge.Store
	Close()
}

type edgeAPI struct {
	readiness httpapi.ReadinessProbe
	features  httpapi.EdgeRuntimeFeatures
	store     edgeAPIStore
	sslip     httpapi.SSLIPHostnameResolver
	issuers   httpapi.CertificateIssuerCatalog
}

type edgeCertificateIssuerCatalog struct{ profile edge.CertManagerProfile }

func (c edgeCertificateIssuerCatalog) ApprovedCertificateIssuers(_ context.Context, hostname string, _ time.Time) ([]httpapi.CertificateIssuerCatalogItem, error) {
	if hostname == "" {
		return nil, edge.ErrInvalid
	}
	items := c.profile.ApprovedIssuerCatalog()
	if len(items) == 0 {
		return nil, edge.ErrUnavailable
	}
	result := make([]httpapi.CertificateIssuerCatalogItem, 0, len(items))
	for _, item := range items {
		result = append(result, httpapi.CertificateIssuerCatalogItem{Name: item.Name, Environment: item.Environment,
			SolverTypes: append([]string(nil), item.SolverTypes...), Source: "bootstrap"})
	}
	return result, nil
}

type sslipHTTPResolver struct{ resolver *edge.PostgreSQLSSLIPResolver }

func (r sslipHTTPResolver) ResolveSSLIPHostname(ctx context.Context, request httpapi.SSLIPHostnameRequest) (httpapi.SSLIPHostnamePreview, error) {
	if r.resolver == nil {
		return httpapi.SSLIPHostnamePreview{}, edge.ErrUnavailable
	}
	resolved, err := r.resolver.ResolveHostname(ctx, edge.SSLIPHostnameRequest{
		ApplicationID: request.ApplicationID,
		EnvironmentID: request.EnvironmentID,
		ProjectID:     request.ProjectID,
		Namespace:     request.Namespace,
	})
	if err != nil {
		return httpapi.SSLIPHostnamePreview{}, err
	}
	return httpapi.SSLIPHostnamePreview{Mode: "sslip", Hostname: resolved.Hostname,
		Source: httpapi.SSLIPHostnameSource(resolved.Source), ObservedAt: resolved.ObservedAt}, nil
}

func newEdgeAPI(ctx context.Context, databaseURL string, config edge.RuntimeConfig) (*edgeAPI, error) {
	if !config.Enabled {
		return nil, nil
	}
	if config.Validate() != nil {
		return nil, edge.ErrUnavailable
	}
	store, err := edge.OpenPostgreSQLStore(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	api, err := buildEdgeAPI(config, store)
	if err != nil {
		store.Close()
		return nil, err
	}
	return api, nil
}

func buildEdgeAPI(config edge.RuntimeConfig, store edgeAPIStore) (*edgeAPI, error) {
	if config.Validate() != nil || !config.Enabled || store == nil {
		return nil, edge.ErrUnavailable
	}
	features := httpapi.EdgeRuntimeFeatures{
		Traefik:     config.Profiles.Traefik != nil,
		CertManager: config.Profiles.CertManager != nil,
		ExternalDNS: len(config.Profiles.ExternalDNS) != 0,
	}
	if !features.Traefik && !features.CertManager && !features.ExternalDNS {
		return nil, edge.ErrUnavailable
	}
	result := &edgeAPI{readiness: &edge.RuntimeReadinessProbe{Store: store, Config: config}, features: features, store: store}
	if config.Profiles.CertManager != nil {
		result.issuers = edgeCertificateIssuerCatalog{profile: *config.Profiles.CertManager}
	}
	if config.Profiles.Traefik != nil && config.Profiles.Traefik.SSLIP != nil {
		postgresStore, ok := store.(*edge.PostgreSQLStore)
		if !ok {
			return nil, edge.ErrUnavailable
		}
		resolver, err := edge.NewPostgreSQLSSLIPResolverFromStore(postgresStore, config)
		if err != nil {
			return nil, err
		}
		result.sslip = sslipHTTPResolver{resolver: resolver}
	}
	return result, nil
}

func (a *edgeAPI) Close() {
	if a != nil && a.store != nil {
		a.store.Close()
	}
}

func edgeHTTPRuntime(runtime *edgeAPI) (httpapi.ReadinessProbe, httpapi.EdgeRuntimeFeatures) {
	if runtime == nil {
		return nil, httpapi.EdgeRuntimeFeatures{}
	}
	return runtime.readiness, runtime.features
}

func edgeHTTPSSLIP(runtime *edgeAPI) httpapi.SSLIPHostnameResolver {
	if runtime == nil {
		return nil
	}
	return runtime.sslip
}

func edgeHTTPCertificateIssuers(runtime *edgeAPI) httpapi.CertificateIssuerCatalog {
	if runtime == nil {
		return nil
	}
	return runtime.issuers
}
