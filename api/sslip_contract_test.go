package api

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

func TestSSLIPPreviewContractIsServerDerivedScopedAndAgentReadable(t *testing.T) {
	var document struct {
		Paths map[string]map[string]struct {
			OperationID string   `json:"operationId"`
			Permission  string   `json:"x-kuberploy-permission"`
			Effect      string   `json:"x-kuberploy-effect"`
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
	operation := document.Paths["/v1/applications/{id}/sslip-hostname"]["get"]
	if operation.OperationID != "previewApplicationSSLIPHostname" || operation.Permission != "resources.read" ||
		operation.Effect != "sensitive-metadata-read" || operation.Automation != "app.read" ||
		!contains(operation.Audience, "human") || !contains(operation.Audience, "agent") {
		t.Fatalf("sslip operation=%#v", operation)
	}
	foundResourceID, foundEnvironment := false, false
	for _, parameter := range operation.Parameters {
		if parameter.Ref == "#/components/parameters/ResourceId" {
			foundResourceID = true
		}
		if parameter.Name == "environmentId" && parameter.In == "query" && parameter.Required {
			foundEnvironment = true
		}
		for _, forbidden := range []string{"ip", "hostname", "namespace", "integrationRef", "ttl"} {
			if strings.EqualFold(parameter.Name, forbidden) {
				t.Fatalf("sslip accepts caller-controlled %q", parameter.Name)
			}
		}
	}
	if !foundResourceID || !foundEnvironment {
		t.Fatalf("sslip parameters=%#v", operation.Parameters)
	}

	var schema struct {
		Additional bool                       `json:"additionalProperties"`
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(document.Components.Schemas["SSLIPHostnamePreview"], &schema); err != nil {
		t.Fatal(err)
	}
	fields := make([]string, 0, len(schema.Properties))
	for field := range schema.Properties {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	sort.Strings(schema.Required)
	if schema.Additional || strings.Join(fields, ",") != "hostname,mode,observedAt,source" || strings.Join(schema.Required, ",") != "hostname,mode,observedAt,source" {
		t.Fatalf("sslip schema fields=%#v required=%#v additional=%t", fields, schema.Required, schema.Additional)
	}
	for _, forbidden := range []string{"ip", "address", "namespace", "applicationId", "environmentId", "projectId", "integrationRef", "ttl"} {
		if _, leaked := schema.Properties[forbidden]; leaked {
			t.Fatalf("sslip response exposes %q", forbidden)
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
	found := false
	for _, candidate := range profile.Operations {
		if candidate.OperationID == operation.OperationID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("safe scoped sslip preview was omitted from the agent profile")
	}
}

func TestCreateDeploymentRouteContractSeparatesManualAndServerDerivedSSLIP(t *testing.T) {
	var document struct {
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(OpenAPIJSON, &document); err != nil {
		t.Fatal(err)
	}

	var createDeployment struct {
		OneOf []struct {
			Ref string `json:"$ref"`
		} `json:"oneOf"`
		Properties map[string]struct {
			Ref string `json:"$ref"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(document.Components.Schemas["CreateDeployment"], &createDeployment); err != nil {
		t.Fatal(err)
	}
	deploymentSchemas := []string{"CreateDeployment"}
	if len(createDeployment.OneOf) > 0 {
		deploymentSchemas = deploymentSchemas[:0]
		for _, member := range createDeployment.OneOf {
			deploymentSchemas = append(deploymentSchemas, strings.TrimPrefix(member.Ref, "#/components/schemas/"))
		}
	}
	for _, schemaName := range deploymentSchemas {
		var member struct {
			Properties map[string]struct {
				Ref string `json:"$ref"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(document.Components.Schemas[schemaName], &member); err != nil {
			t.Fatal(err)
		}
		if got := member.Properties["route"].Ref; got != "#/components/schemas/CreateRoute" {
			t.Fatalf("create deployment member %s route ref=%q", schemaName, got)
		}
	}

	var createRoute struct {
		OneOf []struct {
			Ref string `json:"$ref"`
		} `json:"oneOf"`
	}
	if err := json.Unmarshal(document.Components.Schemas["CreateRoute"], &createRoute); err != nil {
		t.Fatal(err)
	}
	refs := make([]string, 0, len(createRoute.OneOf))
	for _, candidate := range createRoute.OneOf {
		refs = append(refs, candidate.Ref)
	}
	sort.Strings(refs)
	if strings.Join(refs, ",") != "#/components/schemas/ManualCreateRoute,#/components/schemas/SSLIPCreateRoute" {
		t.Fatalf("create route variants=%#v", refs)
	}

	type property struct {
		Const string `json:"const"`
	}
	type objectSchema struct {
		Additional bool                `json:"additionalProperties"`
		Required   []string            `json:"required"`
		Properties map[string]property `json:"properties"`
	}
	decodeSchema := func(name string) objectSchema {
		t.Helper()
		var schema objectSchema
		if err := json.Unmarshal(document.Components.Schemas[name], &schema); err != nil {
			t.Fatal(err)
		}
		return schema
	}
	fieldNames := func(schema objectSchema) []string {
		fields := make([]string, 0, len(schema.Properties))
		for field := range schema.Properties {
			fields = append(fields, field)
		}
		sort.Strings(fields)
		return fields
	}

	manual := decodeSchema("ManualCreateRoute")
	sort.Strings(manual.Required)
	if manual.Additional || strings.Join(fieldNames(manual), ",") != "dnsMode,hostname,pathPrefix,tlsMode" || strings.Join(manual.Required, ",") != "hostname" || manual.Properties["dnsMode"].Const != "manual" {
		t.Fatalf("manual create route=%#v", manual)
	}

	sslip := decodeSchema("SSLIPCreateRoute")
	sort.Strings(sslip.Required)
	if sslip.Additional || strings.Join(fieldNames(sslip), ",") != "dnsMode,pathPrefix,tlsMode" || strings.Join(sslip.Required, ",") != "dnsMode,pathPrefix,tlsMode" ||
		sslip.Properties["dnsMode"].Const != "sslip" || sslip.Properties["pathPrefix"].Const != "/" || sslip.Properties["tlsMode"].Const != "httpOnly" {
		t.Fatalf("sslip create route=%#v", sslip)
	}
	for _, forbidden := range []string{"hostname", "ip", "address", "namespace", "integrationRef"} {
		if _, accepted := sslip.Properties[forbidden]; accepted {
			t.Fatalf("sslip create route accepts caller-controlled %q", forbidden)
		}
	}

	stored := decodeSchema("Route")
	if _, ok := stored.Properties["hostname"]; !ok {
		t.Fatal("stored route omitted the resolved hostname")
	}
}
