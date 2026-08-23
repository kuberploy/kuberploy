package main

import (
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/edge"
	"github.com/kuberploy/kuberploy/internal/externaldns"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

func validExternalDNSOperationalConfig() externaldns.OperationalConfig {
	return externaldns.OperationalConfig{
		Enabled:   true,
		BindingID: "11111111-1111-4111-8111-111111111111",
		Template: externaldns.ManagedRuntimeTemplate{
			Namespace: "external-dns", Version: "v0.18.0",
			Image:          "registry.k8s.io/external-dns/external-dns@sha256:" + strings.Repeat("a", 64),
			ServiceAccount: "external-dns",
		},
		PollInterval: 5 * time.Second,
	}
}

func TestExternalDNSManagementDefaultsOffAndRequiresValidOperationalConfig(t *testing.T) {
	store := memory.New()
	if service := externalDNSManagementForConfig(store, externaldns.OperationalConfig{}); service != nil {
		t.Fatalf("disabled operational config exposed management: %T", service)
	}
	partial := externaldns.OperationalConfig{Enabled: true}
	if service := externalDNSManagementForConfig(store, partial); service != nil {
		t.Fatalf("invalid operational config exposed management: %T", service)
	}
	if service := externalDNSManagementForConfig(store, validExternalDNSOperationalConfig()); service == nil {
		t.Fatal("valid enabled operational config did not expose management")
	}
}

func TestExternalDNSProfilesSortByIntegrationID(t *testing.T) {
	profiles := []edge.ExternalDNSProfile{
		{IntegrationID: "c09f5b43-5a6f-4bbe-b9c7-eb08a102c92a"},
		{IntegrationID: "8ca71fce-a2c1-4c47-b2f8-dcf88b7683e0"},
	}

	sortExternalDNSProfiles(profiles)
	if profiles[0].IntegrationID != "8ca71fce-a2c1-4c47-b2f8-dcf88b7683e0" || profiles[1].IntegrationID != "c09f5b43-5a6f-4bbe-b9c7-eb08a102c92a" {
		t.Fatalf("profiles not sorted by integration ID: %#v", profiles)
	}
}
