package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/certissuers"
	"github.com/kuberploy/kuberploy/internal/httpapi"
)

type staticIssuerCatalog []httpapi.CertificateIssuerCatalogItem

func (c staticIssuerCatalog) ApprovedCertificateIssuers(context.Context, string, time.Time) ([]httpapi.CertificateIssuerCatalogItem, error) {
	return append([]httpapi.CertificateIssuerCatalogItem(nil), c...), nil
}

func TestCertificateIssuerAPIIsStrictlyDefaultOff(t *testing.T) {
	api, err := newCertificateIssuerAPI(t.Context(), "not-a-database-url", certissuers.ObserverConfig{}, nil)
	if err != nil || api != nil {
		t.Fatalf("api=%#v err=%v", api, err)
	}
	dormant := certissuers.ObserverConfig{PollInterval: time.Second}
	if api, err = newCertificateIssuerAPI(t.Context(), "not-a-database-url", dormant, nil); api != nil || !errors.Is(err, certissuers.ErrObservationUnavailable) {
		t.Fatalf("dormant api=%#v err=%v", api, err)
	}
}

func TestCombinedCertificateIssuerCatalogRejectsAmbiguousNames(t *testing.T) {
	catalog := combinedCertificateIssuerCatalog{bootstrap: staticIssuerCatalog{
		{Name: "z-staging", Environment: "staging", SolverTypes: []string{"http01"}, Source: "bootstrap"},
		{Name: "a-production", Environment: "production", SolverTypes: []string{"http01"}, Source: "bootstrap"},
	}}
	items, err := catalog.ApprovedCertificateIssuers(t.Context(), "app.example.com", time.Now().UTC())
	if err != nil || len(items) != 2 || items[0].Name != "a-production" || items[1].Name != "z-staging" {
		t.Fatalf("items=%#v err=%v", items, err)
	}
	catalog.bootstrap = staticIssuerCatalog{{Name: "duplicate"}, {Name: "duplicate"}}
	if _, err = catalog.ApprovedCertificateIssuers(t.Context(), "app.example.com", time.Now().UTC()); err == nil {
		t.Fatal("ambiguous issuer name accepted")
	}
}
