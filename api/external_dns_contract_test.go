package api

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

type externalDNSContractOperation struct {
	OperationID     string                `json:"operationId"`
	Tags            []string              `json:"tags"`
	Security        []map[string][]string `json:"security"`
	Audience        []string              `json:"x-kuberploy-audience"`
	Permission      string                `json:"x-kuberploy-permission"`
	AutomationScope string                `json:"x-kuberploy-automation-scope"`
	Parameters      []struct {
		Ref string `json:"$ref"`
	} `json:"parameters"`
	Responses map[string]struct {
		Ref string `json:"$ref"`
	} `json:"responses"`
}

func TestExternalDNSOpenAPIContractIsScopedMetadataOnlyAndFailClosed(t *testing.T) {
	var document struct {
		Paths      map[string]map[string]externalDNSContractOperation `json:"paths"`
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(OpenAPIJSON, &document); err != nil {
		t.Fatal(err)
	}

	expected := map[string]struct {
		path, method, operationID, permission string
		platform, mutation                    bool
	}{
		"platform-list":       {"/v1/external-dns/integrations", "get", "listExternalDNSIntegrations", "external-dns.manage", true, false},
		"platform-create":     {"/v1/external-dns/integrations", "post", "createExternalDNSIntegration", "external-dns.manage", true, true},
		"platform-update":     {"/v1/external-dns/integrations/{id}", "put", "updateExternalDNSIntegration", "external-dns.manage", true, true},
		"platform-deactivate": {"/v1/external-dns/integrations/{id}", "delete", "deactivateExternalDNSIntegration", "external-dns.manage", true, true},
		"platform-status":     {"/v1/external-dns/status", "get", "getExternalDNSConfigurationStatus", "external-dns.manage", true, false},
		"environment":         {"/v1/environments/{id}/external-dns-integrations", "get", "listEnvironmentExternalDNSIntegrations", "external-dns.read", false, false},
		"application":         {"/v1/applications/{id}/external-dns-integrations", "get", "listApplicationExternalDNSIntegrations", "external-dns.read", false, false},
	}
	operationIDs := map[string]bool{}
	for name, want := range expected {
		operation, ok := document.Paths[want.path][want.method]
		if !ok || operation.OperationID != want.operationID || operation.Permission != want.permission ||
			len(operation.Tags) != 1 || operation.Tags[0] != "External DNS" {
			t.Fatalf("%s operation drifted: %#v", name, operation)
		}
		operationIDs[operation.OperationID] = true
		if want.platform {
			if len(operation.Security) != 1 || operation.Security[0]["cookieAuth"] == nil ||
				!externalDNSContains(operation.Audience, "human") || externalDNSContains(operation.Audience, "agent") {
				t.Fatalf("%s must remain human cookie-only: %#v", name, operation)
			}
		} else if operation.AutomationScope != "app.read" ||
			!externalDNSContains(operation.Audience, "human") || !externalDNSContains(operation.Audience, "agent") {
			t.Fatalf("%s catalog read lost scoped automation metadata: %#v", name, operation)
		}
		if want.mutation {
			refs := map[string]bool{}
			for _, parameter := range operation.Parameters {
				refs[parameter.Ref] = true
			}
			if !refs["#/components/parameters/SecretIdempotencyKey"] ||
				!refs["#/components/parameters/SessionCSRFToken"] ||
				operation.Responses["429"].Ref != "#/components/responses/RateLimitExceeded" {
				t.Fatalf("%s lacks strict mutation controls: refs=%#v responses=%#v", name, refs, operation.Responses)
			}
		}
	}

	allowedProperties := map[string][]string{
		"ExternalDNSIntegrationInput": {"allowedDomainSuffixes", "credentialSecretRef", "destructiveSyncConfirmed", "egressConfigRef", "environmentIds", "mode", "name", "operatorProfileRef", "providerConfigRef", "providerKind", "slug", "syncPolicy", "txtOwnerId"},
		"ExternalDNSIntegration":      {"allowedDomainSuffixes", "createdAt", "createdBy", "credentialSecretRef", "deactivatedAt", "destructiveSyncConfirmed", "egressConfigRef", "environmentIds", "id", "lifecycle", "mode", "name", "operatorProfileRef", "protectedGitObservedAt", "protectedGitRevision", "protectedGitState", "providerConfigRef", "providerKind", "runtimeRevision", "slug", "syncPolicy", "txtOwnerId", "updatedAt"},
		"ExternalDNSCatalogItem":      {"allowedDomainSuffixes", "id", "mode", "name", "providerKind", "runtimeAvailable", "runtimeRevision", "slug"},
	}
	for schemaName, want := range allowedProperties {
		properties := externalDNSSchemaProperties(t, document.Components.Schemas[schemaName])
		got := make([]string, 0, len(properties))
		for name := range properties {
			got = append(got, name)
		}
		sort.Strings(got)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s fields=%#v, want %#v", schemaName, got, want)
		}
	}
	for _, schemaName := range []string{"ExternalDNSIntegrationInput", "ExternalDNSIntegration", "ExternalDNSCatalogItem"} {
		properties := externalDNSSchemaProperties(t, document.Components.Schemas[schemaName])
		for _, forbidden := range []string{"credential", "credentialValue", "secretData", "token", "password", "endpoint", "providerOptions", "providerJson", "webhook"} {
			if _, exposed := properties[forbidden]; exposed {
				t.Fatalf("%s exposes forbidden field %q", schemaName, forbidden)
			}
		}
	}
	catalogProperties := externalDNSSchemaProperties(t, document.Components.Schemas["ExternalDNSCatalogItem"])
	var itemRuntimeAvailable struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(catalogProperties["runtimeAvailable"], &itemRuntimeAvailable); err != nil || itemRuntimeAvailable.Type != "boolean" {
		t.Fatalf("tenant catalog lacks exact per-item readiness: %#v err=%v", itemRuntimeAvailable, err)
	}
	for _, platformOnly := range []string{"credentialSecretRef", "providerConfigRef", "egressConfigRef", "operatorProfileRef", "txtOwnerId", "createdBy"} {
		if _, exposed := catalogProperties[platformOnly]; exposed {
			t.Fatalf("tenant catalog exposes platform-only field %q", platformOnly)
		}
	}
	assertExternalDNSMaxItems(t, document.Components.Schemas["ExternalDNSIntegrationList"], "items", 100)
	assertExternalDNSMaxItems(t, document.Components.Schemas["ExternalDNSCatalog"], "items", 100)

	for _, schemaName := range []string{"ExternalDNSCatalog", "ExternalDNSStatus"} {
		properties := externalDNSSchemaProperties(t, document.Components.Schemas[schemaName])
		var readiness struct {
			Type string   `json:"type"`
			Enum []string `json:"enum"`
		}
		var runtimeAvailable struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(properties["controllerReadiness"], &readiness); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(properties["runtimeAvailable"], &runtimeAvailable); err != nil {
			t.Fatal(err)
		}
		if readiness.Type != "string" || len(readiness.Enum) != 2 || readiness.Enum[0] != "unobserved" || readiness.Enum[1] != "ready" || runtimeAvailable.Type != "boolean" {
			t.Fatalf("%s does not expose the closed observed readiness contract: readiness=%#v runtime=%#v", schemaName, readiness, runtimeAvailable)
		}
	}

	profileBytes, err := BuildAgentProfile(OpenAPIJSON)
	if err != nil {
		t.Fatal(err)
	}
	var profile struct {
		Operations []AgentOperation `json:"operations"`
	}
	if err = json.Unmarshal(profileBytes, &profile); err != nil {
		t.Fatal(err)
	}
	projected := map[string]bool{}
	for _, operation := range profile.Operations {
		if operationIDs[operation.OperationID] {
			projected[operation.OperationID] = true
		}
	}
	if !projected["listEnvironmentExternalDNSIntegrations"] ||
		!projected["listApplicationExternalDNSIntegrations"] || len(projected) != 2 {
		t.Fatalf("External DNS agent projection=%#v", projected)
	}
}

func externalDNSSchemaProperties(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	return schema.Properties
}

func assertExternalDNSMaxItems(t *testing.T, raw json.RawMessage, property string, maximum int) {
	t.Helper()
	properties := externalDNSSchemaProperties(t, raw)
	var bounded struct {
		MaxItems int `json:"maxItems"`
	}
	if err := json.Unmarshal(properties[property], &bounded); err != nil {
		t.Fatal(err)
	}
	if bounded.MaxItems != maximum {
		t.Fatalf("%s maxItems=%d, want %d", property, bounded.MaxItems, maximum)
	}
}

func externalDNSContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
