package main

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/kuberploy/kuberploy/internal/certissuers"
	"github.com/kuberploy/kuberploy/internal/httpapi"
)

type certificateIssuerAPI struct {
	store     *certissuers.PostgresStore
	admin     *certificateIssuerAdminStore
	catalog   httpapi.CertificateIssuerCatalog
	readiness *certissuers.ObserverReadinessProbe
}

type certificateIssuerAdminStore struct {
	store    *certissuers.PostgresStore
	reserved map[string]struct{}
}

func (s *certificateIssuerAdminStore) Create(ctx context.Context, command certissuers.Command, name string, spec certissuers.Spec) (certissuers.MutationResult, error) {
	if s == nil || s.store == nil {
		return certissuers.MutationResult{}, certissuers.ErrObservationUnavailable
	}
	if _, reserved := s.reserved[name]; reserved {
		return certissuers.MutationResult{}, certissuers.ErrConflict
	}
	return s.store.Create(ctx, command, name, spec)
}
func (s *certificateIssuerAdminStore) Revise(ctx context.Context, command certissuers.Command, ref certissuers.Ref, spec certissuers.Spec) (certissuers.MutationResult, error) {
	if s == nil || s.store == nil {
		return certissuers.MutationResult{}, certissuers.ErrObservationUnavailable
	}
	return s.store.Revise(ctx, command, ref, spec)
}
func (s *certificateIssuerAdminStore) Deactivate(ctx context.Context, command certissuers.Command, ref certissuers.Ref) (certissuers.MutationResult, error) {
	if s == nil || s.store == nil {
		return certissuers.MutationResult{}, certissuers.ErrObservationUnavailable
	}
	return s.store.Deactivate(ctx, command, ref)
}
func (s *certificateIssuerAdminStore) ReplayCreate(ctx context.Context, command certissuers.Command, name string, spec certissuers.Spec) (certissuers.MutationResult, bool, error) {
	if s == nil || s.store == nil {
		return certissuers.MutationResult{}, false, certissuers.ErrObservationUnavailable
	}
	if _, reserved := s.reserved[name]; reserved {
		return certissuers.MutationResult{}, false, certissuers.ErrConflict
	}
	return s.store.ReplayCreate(ctx, command, name, spec)
}
func (s *certificateIssuerAdminStore) ReplayRevise(ctx context.Context, command certissuers.Command, ref certissuers.Ref, spec certissuers.Spec) (certissuers.MutationResult, bool, error) {
	if s == nil || s.store == nil {
		return certissuers.MutationResult{}, false, certissuers.ErrObservationUnavailable
	}
	return s.store.ReplayRevise(ctx, command, ref, spec)
}
func (s *certificateIssuerAdminStore) ReplayDeactivate(ctx context.Context, command certissuers.Command, ref certissuers.Ref) (certissuers.MutationResult, bool, error) {
	if s == nil || s.store == nil {
		return certissuers.MutationResult{}, false, certissuers.ErrObservationUnavailable
	}
	return s.store.ReplayDeactivate(ctx, command, ref)
}
func (s *certificateIssuerAdminStore) List(ctx context.Context, limit int) ([]certissuers.Entry, error) {
	if s == nil || s.store == nil {
		return nil, certissuers.ErrObservationUnavailable
	}
	return s.store.List(ctx, limit)
}
func (s *certificateIssuerAdminStore) Observation(ctx context.Context, profileID string, revision int64) (certissuers.Observation, error) {
	if s == nil || s.store == nil {
		return certissuers.Observation{}, certissuers.ErrObservationUnavailable
	}
	return s.store.Observation(ctx, profileID, revision)
}

type combinedCertificateIssuerCatalog struct {
	bootstrap httpapi.CertificateIssuerCatalog
	managed   *certissuers.Catalog
	maxAge    time.Duration
}

func (c combinedCertificateIssuerCatalog) ApprovedCertificateIssuers(ctx context.Context, hostname string, now time.Time) ([]httpapi.CertificateIssuerCatalogItem, error) {
	items := []httpapi.CertificateIssuerCatalogItem{}
	if c.bootstrap != nil {
		bootstrap, err := c.bootstrap.ApprovedCertificateIssuers(ctx, hostname, now)
		if err != nil {
			return nil, err
		}
		items = append(items, bootstrap...)
	}
	if c.managed != nil {
		managed, err := c.managed.ForHostname(ctx, hostname, now, c.maxAge, 100)
		if err != nil {
			return nil, err
		}
		for _, item := range managed {
			items = append(items, httpapi.CertificateIssuerCatalogItem{Name: item.Name, Environment: string(item.Environment),
				SolverTypes: []string{string(item.Solver)}, Source: "managed", Revision: item.Revision})
		}
	}
	seen := map[string]struct{}{}
	for _, item := range items {
		if _, duplicate := seen[item.Name]; duplicate {
			return nil, errors.New("ambiguous certificate issuer name")
		}
		seen[item.Name] = struct{}{}
	}
	sort.Slice(items, func(left, right int) bool { return items[left].Name < items[right].Name })
	return items, nil
}

func newCertificateIssuerAPI(ctx context.Context, databaseURL string, config certissuers.ObserverConfig, bootstrap httpapi.CertificateIssuerCatalog) (*certificateIssuerAPI, error) {
	if config.Validate() != nil {
		return nil, certissuers.ErrObservationUnavailable
	}
	if !config.Enabled {
		return nil, nil
	}
	store, err := certissuers.OpenPostgresStore(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	identity, err := certissuers.ObserverIdentityForConfig(config)
	if err != nil {
		store.Close()
		return nil, err
	}
	managed, err := certissuers.NewCatalog(store)
	if err != nil {
		store.Close()
		return nil, err
	}
	reserved := map[string]struct{}{}
	if bootstrap != nil {
		// Bootstrap issuers are hostname-independent identities. A public
		// hostname is used only to enumerate their reserved object names.
		entries, catalogErr := bootstrap.ApprovedCertificateIssuers(ctx, "reserved.kuberploy.dev", time.Now().UTC())
		if catalogErr != nil {
			store.Close()
			return nil, catalogErr
		}
		for _, entry := range entries {
			reserved[entry.Name] = struct{}{}
		}
	}
	admin := &certificateIssuerAdminStore{store: store, reserved: reserved}
	return &certificateIssuerAPI{store: store, admin: admin,
		catalog: combinedCertificateIssuerCatalog{bootstrap: bootstrap, managed: managed, maxAge: config.MaximumAge},
		readiness: &certissuers.ObserverReadinessProbe{Store: store, ReadinessStore: store, Config: config, Identity: identity,
			Now: func() time.Time { return time.Now().UTC() }}}, nil
}

func (a *certificateIssuerAPI) Close() {
	if a != nil && a.store != nil {
		a.store.Close()
	}
}
