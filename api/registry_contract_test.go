package api

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

type registryContractOperation struct {
	OperationID     string                `json:"operationId"`
	Description     string                `json:"description"`
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

func TestRegistryOpenAPIContractIsScopedBoundedAndMetadataOnly(t *testing.T) {
	var document struct {
		Paths      map[string]map[string]registryContractOperation `json:"paths"`
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(OpenAPIJSON, &document); err != nil {
		t.Fatal(err)
	}

	expected := map[string]struct {
		method, operationID, permission string
		mutation                        bool
		agentRead                       bool
	}{
		"/v1/registry-targets":                               {"get", "listRegistryTargets", "registry.targets.manage", false, false},
		"/v1/registry-targets#post":                          {"post", "createRegistryTarget", "registry.targets.manage", true, false},
		"/v1/registry-targets/{id}":                          {"put", "updateRegistryTarget", "registry.targets.manage", true, false},
		"/v1/applications/{id}/registry":                     {"get", "getApplicationRegistryInventory", "registry.read", false, true},
		"/v1/applications/{id}/registry/policies/{targetId}": {"put", "putApplicationRegistryPolicy", "registry.policy.write", true, false},
		"/v1/applications/{id}/registry/cleanup-previews":    {"post", "createRegistryCleanupPreview", "registry.cleanup.preview", true, false},
		"/v1/registry-cleanup-plans/{id}":                    {"get", "getRegistryCleanupPlan", "registry.read", false, true},
		"/v1/registry-cleanup-plans/{id}/executions":         {"post", "executeRegistryCleanupPlan", "registry.cleanup.execute", true, false},
	}
	operationIDs := map[string]bool{}
	for key, want := range expected {
		path := strings.TrimSuffix(key, "#post")
		operation, ok := document.Paths[path][want.method]
		if !ok || operation.OperationID != want.operationID || operation.Permission != want.permission || len(operation.Tags) != 1 || operation.Tags[0] != "Registry" {
			t.Fatalf("registry operation %s drifted: %#v", key, operation)
		}
		operationIDs[operation.OperationID] = true
		if want.mutation {
			if len(operation.Security) != 1 || len(operation.Security[0]) != 1 || operation.Security[0]["cookieAuth"] == nil || !containsString(operation.Audience, "human") || containsString(operation.Audience, "agent") {
				t.Fatalf("registry mutation %q is not human cookie-only: %#v", operation.OperationID, operation)
			}
			refs := map[string]bool{}
			for _, parameter := range operation.Parameters {
				refs[parameter.Ref] = true
			}
			if !refs["#/components/parameters/IdempotencyKey"] || !refs["#/components/parameters/SessionCSRFToken"] || operation.Responses["429"].Ref != "#/components/responses/RateLimitExceeded" {
				t.Fatalf("registry mutation %q lacks idempotency, CSRF, or stable limiter metadata: refs=%#v responses=%#v", operation.OperationID, refs, operation.Responses)
			}
		} else if want.agentRead {
			if operation.AutomationScope != "app.read" || !containsString(operation.Audience, "agent") || !containsString(operation.Audience, "human") {
				t.Fatalf("scoped registry read %q has unsafe automation metadata: %#v", operation.OperationID, operation)
			}
		} else if containsString(operation.Audience, "agent") || len(operation.Security) != 1 || operation.Security[0]["cookieAuth"] == nil {
			t.Fatalf("platform registry target read leaked outside human sessions: %#v", operation)
		}
	}
	if _, deletionExposed := document.Paths["/v1/registry-targets/{id}"]["delete"]; deletionExposed {
		t.Fatal("registry target deletion is exposed")
	}
	inventoryDescription := document.Paths["/v1/applications/{id}/registry"]["get"].Description
	for _, required := range []string{"External-target metadata remains available", "managed observer and cleanup-executor runtime is required", "fails closed with 503"} {
		if !strings.Contains(inventoryDescription, required) {
			t.Fatalf("application registry readiness boundary is not explicit in OpenAPI: missing %q in %q", required, inventoryDescription)
		}
	}

	allowedProperties := map[string][]string{
		"RegistryTarget":            {"cacheCredentialRef", "createdAt", "endpoint", "id", "mode", "name", "pullCredentialRef", "pushCredentialRef", "repositoryPrefix", "updatedAt"},
		"ApplicationRegistryTarget": {"cacheGenerations", "cacheGenerationsTruncated", "catalogObservations", "catalogTruncated", "inventory", "observedAt", "policy", "releases", "releasesTruncated", "target"},
		"RegistryCleanupPlan":       {"claimedAt", "completedAt", "createdAt", "failure", "id", "items", "itemsTruncated", "planDigest", "policy", "registryTargetId", "serviceId", "state", "summary"},
	}
	for schemaName, allowed := range allowedProperties {
		properties := registrySchemaProperties(t, document.Components.Schemas[schemaName])
		actual := make([]string, 0, len(properties))
		for name := range properties {
			actual = append(actual, name)
		}
		sort.Strings(actual)
		sort.Strings(allowed)
		if strings.Join(actual, ",") != strings.Join(allowed, ",") {
			t.Fatalf("safe registry schema %s fields=%#v, want %#v", schemaName, actual, allowed)
		}
	}
	for schemaName, forbidden := range map[string][]string{
		"RegistryTargetInput":       {"password", "token", "credentials", "secret", "credentialData"},
		"ApplicationRegistryTarget": {"manifests", "blobs", "references", "authorities", "snapshotToken", "authorityToken", "providerPayload"},
		"RegistryCleanupPlan":       {"snapshotToken", "authorityToken", "inventory", "catalogs", "authorities", "leases", "credentials", "providerRequest"},
	} {
		properties := registrySchemaProperties(t, document.Components.Schemas[schemaName])
		for _, name := range forbidden {
			if _, exposed := properties[name]; exposed {
				t.Fatalf("safe registry schema %s exposes %q", schemaName, name)
			}
		}
	}

	assertRegistryMaxItems(t, document.Components.Schemas["RegistryTargetList"], "items", 100)
	assertRegistryMaxItems(t, document.Components.Schemas["ApplicationRegistryInventory"], "items", 100)
	assertRegistryMaxItems(t, document.Components.Schemas["RegistryInventoryObservation"], "repositories", 100)
	assertRegistryMaxItems(t, document.Components.Schemas["ApplicationRegistryTarget"], "catalogObservations", 100)
	assertRegistryMaxItems(t, document.Components.Schemas["ApplicationRegistryTarget"], "releases", 100)
	assertRegistryMaxItems(t, document.Components.Schemas["ApplicationRegistryTarget"], "cacheGenerations", 100)
	assertRegistryMaxItems(t, document.Components.Schemas["RegistryCleanupPlan"], "items", 100)
	assertRegistryStringBounds(t, document.Components.Schemas["RegistryCredentialReference"], 1, 256, true)
	for _, field := range []struct {
		name       string
		minimum    int
		maximum    int
		patternReq bool
	}{{"name", 1, 100, true}, {"endpoint", 1, 2048, true}, {"repositoryPrefix", 1, 255, true}} {
		properties := registrySchemaProperties(t, document.Components.Schemas["RegistryTargetInput"])
		assertRegistryStringBounds(t, properties[field.name], field.minimum, field.maximum, field.patternReq)
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
	profileRegistry := map[string]bool{}
	for _, operation := range profile.Operations {
		if operationIDs[operation.OperationID] {
			profileRegistry[operation.OperationID] = true
		}
	}
	if !profileRegistry["getApplicationRegistryInventory"] || !profileRegistry["getRegistryCleanupPlan"] || len(profileRegistry) != 2 {
		t.Fatalf("registry agent profile projection=%#v", profileRegistry)
	}
}

func registrySchemaProperties(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	return schema.Properties
}

func assertRegistryMaxItems(t *testing.T, raw json.RawMessage, property string, maximum int) {
	t.Helper()
	properties := registrySchemaProperties(t, raw)
	var bounded struct {
		MaxItems int `json:"maxItems"`
	}
	if err := json.Unmarshal(properties[property], &bounded); err != nil {
		t.Fatal(err)
	}
	if bounded.MaxItems != maximum {
		t.Fatalf("registry schema property %s maxItems=%d, want %d", property, bounded.MaxItems, maximum)
	}
}

func assertRegistryStringBounds(t *testing.T, raw json.RawMessage, minimum, maximum int, patternRequired bool) {
	t.Helper()
	var bounded struct {
		MinLength int    `json:"minLength"`
		MaxLength int    `json:"maxLength"`
		Pattern   string `json:"pattern"`
	}
	if err := json.Unmarshal(raw, &bounded); err != nil {
		t.Fatal(err)
	}
	if bounded.MinLength != minimum || bounded.MaxLength != maximum || patternRequired && bounded.Pattern == "" {
		t.Fatalf("registry string bounds=%#v, want min=%d max=%d pattern=%t", bounded, minimum, maximum, patternRequired)
	}
}
