package api

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

func TestCertificateIssuerCatalogContractIsScopedSafeAndAgentReadable(t *testing.T) {
	var document struct {
		Paths map[string]map[string]struct {
			OperationID string   `json:"operationId"`
			Permission  string   `json:"x-kuberploy-permission"`
			Automation  string   `json:"x-kuberploy-automation-scope"`
			Audience    []string `json:"x-kuberploy-audience"`
			Parameters  []struct {
				Ref      string `json:"$ref"`
				Name     string `json:"name"`
				In       string `json:"in"`
				Required bool   `json:"required"`
			} `json:"parameters"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(OpenAPIJSON, &document); err != nil {
		t.Fatal(err)
	}
	operation := document.Paths["/v1/applications/{id}/certificate-issuers"]["get"]
	if operation.OperationID != "listApprovedCertificateIssuers" || operation.Permission != "resources.read" || operation.Automation != "app.read" ||
		!contains(operation.Audience, "human") || !contains(operation.Audience, "agent") {
		t.Fatalf("issuer catalog operation=%#v", operation)
	}
	foundID, foundEnvironment, foundHostname := false, false, false
	for _, parameter := range operation.Parameters {
		foundID = foundID || parameter.Ref == "#/components/parameters/ResourceId"
		foundEnvironment = foundEnvironment || parameter.Name == "environmentId" && parameter.In == "query" && parameter.Required
		foundHostname = foundHostname || parameter.Name == "hostname" && parameter.In == "query" && parameter.Required
	}
	if !foundID || !foundEnvironment || !foundHostname {
		t.Fatalf("issuer catalog parameters=%#v", operation.Parameters)
	}

	var item struct {
		Additional bool                       `json:"additionalProperties"`
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(document.Components.Schemas["CertificateIssuerCatalogItem"], &item); err != nil {
		t.Fatal(err)
	}
	fields := make([]string, 0, len(item.Properties))
	for field := range item.Properties {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	if item.Additional || strings.Join(fields, ",") != "environment,name,revision,solverTypes,source" {
		t.Fatalf("unsafe issuer fields=%#v additional=%t", fields, item.Additional)
	}
	for _, forbidden := range []string{"email", "server", "dnsZones", "secretRef", "apiTokenSecretName", "apiTokenSecretKey", "credentials"} {
		if _, leaked := item.Properties[forbidden]; leaked {
			t.Fatalf("issuer catalog exposes %q", forbidden)
		}
	}
}

func TestCertificateIssuerAdministrationIsHumanOnlyAndCannotAcceptAuthorityOrCredentialBytes(t *testing.T) {
	var document struct {
		Paths map[string]map[string]struct {
			OperationID string                       `json:"operationId"`
			Permission  string                       `json:"x-kuberploy-permission"`
			Audience    []string                     `json:"x-kuberploy-audience"`
			Security    []map[string]json.RawMessage `json:"security"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(OpenAPIJSON, &document); err != nil {
		t.Fatal(err)
	}
	operations := []struct{ path, method, id string }{
		{"/v1/platform/certificate-issuers", "get", "listPlatformCertificateIssuers"},
		{"/v1/platform/certificate-issuers", "post", "createPlatformCertificateIssuer"},
		{"/v1/platform/certificate-issuers/{id}", "put", "revisePlatformCertificateIssuer"},
		{"/v1/platform/certificate-issuers/{id}/deactivate", "post", "deactivatePlatformCertificateIssuer"},
	}
	for _, expected := range operations {
		operation := document.Paths[expected.path][expected.method]
		if operation.OperationID != expected.id || operation.Permission != "platform.admin" || strings.Join(operation.Audience, ",") != "human" || len(operation.Security) != 1 {
			t.Fatalf("unsafe issuer admin operation %s %s: %#v", expected.method, expected.path, operation)
		}
		if _, cookieOnly := operation.Security[0]["cookieAuth"]; !cookieOnly {
			t.Fatalf("issuer admin operation is not cookie-only: %#v", operation.Security)
		}
	}
	for _, schemaName := range []string{"CreateCertificateIssuer", "ReviseCertificateIssuer"} {
		var schema struct {
			Additional bool                       `json:"additionalProperties"`
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(document.Components.Schemas[schemaName], &schema); err != nil {
			t.Fatal(err)
		}
		if schema.Additional {
			t.Fatalf("%s accepts arbitrary fields", schemaName)
		}
		for _, forbidden := range []string{"server", "acmeServer", "apiToken", "credentials", "rawYaml", "webhook", "providerEndpoint"} {
			if _, present := schema.Properties[forbidden]; present {
				t.Fatalf("%s accepts caller authority %q", schemaName, forbidden)
			}
		}
	}
}
