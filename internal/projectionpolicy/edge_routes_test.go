package projectionpolicy

import (
	"testing"

	"github.com/kuberploy/kuberploy/internal/edge"
)

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
