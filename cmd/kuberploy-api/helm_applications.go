package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/helmdirect"
)

const (
	helmApplicationsEnabledEnv = "KUBERPLOY_HELM_APPLICATIONS_ENABLED"
	helmArgoNamespaceEnv       = "KUBERPLOY_ARGO_NAMESPACE"
)

type helmApplicationsAPI struct {
	pool    *pgxpool.Pool
	runtime *helmdirect.Service
}

func newHelmApplicationsAPI(ctx context.Context, databaseURL string, _ *gitProjectionAPI,
	_ *argoDesiredStateAPI) (*helmApplicationsAPI, error) {
	return newHelmApplicationsAPIFromLookup(ctx, databaseURL, nil, nil, os.LookupEnv)
}

func newHelmApplicationsAPIFromLookup(ctx context.Context, databaseURL string, _ *gitProjectionAPI,
	_ *argoDesiredStateAPI, lookup func(string) (string, bool)) (*helmApplicationsAPI, error) {
	value, present := lookup(helmApplicationsEnabledEnv)
	if !present || strings.TrimSpace(value) == "" || strings.EqualFold(strings.TrimSpace(value), "false") {
		return nil, nil
	}
	if !strings.EqualFold(strings.TrimSpace(value), "true") {
		return nil, errors.New("KUBERPLOY_HELM_APPLICATIONS_ENABLED must be true or false")
	}
	argoNamespace, present := lookup(helmArgoNamespaceEnv)
	argoNamespace = strings.TrimSpace(argoNamespace)
	if !present || argoNamespace == "" {
		return nil, errors.New("KUBERPLOY_ARGO_NAMESPACE is required when Helm Apps are enabled")
	}
	pool, err := openHelmAPIPool(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	store, err := helmdirect.NewPostgresStore(pool)
	if err != nil {
		pool.Close()
		return nil, err
	}
	api, err := helmdirect.NewInClusterApplicationAPI()
	if err != nil {
		pool.Close()
		return nil, err
	}
	service := &helmdirect.Service{Store: store, Reconciler: helmdirect.ArgoReconciler{API: api, Namespace: argoNamespace}}
	return &helmApplicationsAPI{pool: pool, runtime: service}, nil
}

func openHelmAPIPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, helmdirect.ErrInvalid
	}
	config.ConnConfig.RuntimeParams["application_name"] = "kuberploy-helm-applications-api"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

func (a *helmApplicationsAPI) Close() {
	if a != nil && a.pool != nil {
		a.pool.Close()
	}
}

func (a *helmApplicationsAPI) Run(ctx context.Context) error {
	if a == nil || a.runtime == nil {
		return helmdirect.ErrUnavailable
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if err := a.runtime.ReconcilePending(ctx, 25, time.Now().UTC()); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
