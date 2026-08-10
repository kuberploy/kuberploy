package api

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

func TestRuntimeSecretOpenAPIContractIsWriteOnlyAndAgentExcluded(t *testing.T) {
	type parameter struct {
		Ref string `json:"$ref"`
	}
	type operation struct {
		OperationID string                `json:"operationId"`
		Tags        []string              `json:"tags"`
		Security    []map[string][]string `json:"security"`
		Audience    []string              `json:"x-kuberploy-audience"`
		Permission  string                `json:"x-kuberploy-permission"`
		Parameters  []parameter           `json:"parameters"`
	}
	var document struct {
		OpenAPI    string                          `json:"openapi"`
		Paths      map[string]map[string]operation `json:"paths"`
		Components struct {
			Parameters map[string]json.RawMessage `json:"parameters"`
			Schemas    map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(OpenAPIJSON, &document); err != nil {
		t.Fatal(err)
	}
	if document.OpenAPI != "3.2.0" {
		t.Fatalf("OpenAPI version=%q", document.OpenAPI)
	}

	expected := map[string]struct {
		method, operationID, permission string
		mutation                        bool
	}{
		"/v1/applications/{id}/secret-bindings#get":  {"get", "listRuntimeSecretBindings", "secrets.read", false},
		"/v1/applications/{id}/secret-bindings#post": {"post", "createRuntimeSecretBinding", "secrets.create", true},
		"/v1/secret-bindings/{id}#get":               {"get", "getRuntimeSecretBinding", "secrets.read", false},
		"/v1/secret-bindings/{id}#delete":            {"delete", "deleteRuntimeSecretBinding", "secrets.delete", true},
		"/v1/secret-bindings/{id}/versions#post":     {"post", "rotateRuntimeSecretBinding", "secrets.rotate", true},
	}
	secretOperationIDs := map[string]struct{}{}
	for key, want := range expected {
		path := strings.Split(key, "#")[0]
		actual, ok := document.Paths[path][want.method]
		if !ok || actual.OperationID != want.operationID || actual.Permission != want.permission || len(actual.Tags) != 1 || actual.Tags[0] != "Runtime Secrets" {
			t.Fatalf("operation %s: %#v", key, actual)
		}
		secretOperationIDs[actual.OperationID] = struct{}{}
		if contains(actual.Audience, "agent") || !contains(actual.Audience, "human") {
			t.Fatalf("secret operation leaked into agent audience: %#v", actual)
		}
		refs := map[string]bool{}
		for _, item := range actual.Parameters {
			refs[item.Ref] = true
		}
		if want.mutation {
			if len(actual.Security) != 1 || len(actual.Security[0]) != 1 || actual.Security[0]["cookieAuth"] == nil {
				t.Fatalf("mutation %q is not cookie-only: %#v", actual.OperationID, actual.Security)
			}
			if !refs["#/components/parameters/SecretIdempotencyKey"] || !refs["#/components/parameters/SessionCSRFToken"] {
				t.Fatalf("mutation %q lacks strict idempotency/CSRF parameters: %#v", actual.OperationID, refs)
			}
		}
	}
	for path, pathItem := range document.Paths {
		if strings.Contains(strings.ToLower(path), "reveal") {
			t.Fatalf("secret reveal path exists: %s", path)
		}
		for _, operation := range pathItem {
			if _, secretOperation := secretOperationIDs[operation.OperationID]; secretOperation && contains(operation.Audience, "agent") {
				t.Fatalf("secret operation %q is agent visible", operation.OperationID)
			}
		}
	}

	var idempotency struct {
		Required bool `json:"required"`
		Schema   struct {
			MinLength int    `json:"minLength"`
			MaxLength int    `json:"maxLength"`
			Pattern   string `json:"pattern"`
		} `json:"schema"`
	}
	if err := json.Unmarshal(document.Components.Parameters["SecretIdempotencyKey"], &idempotency); err != nil {
		t.Fatal(err)
	}
	if !idempotency.Required || idempotency.Schema.MinLength != 16 || idempotency.Schema.MaxLength != 128 || idempotency.Schema.Pattern == "" {
		t.Fatalf("secret idempotency parameter is not closed: %#v", idempotency)
	}

	var values struct {
		Type          string `json:"type"`
		WriteOnly     bool   `json:"writeOnly"`
		MinProperties int    `json:"minProperties"`
		MaxProperties int    `json:"maxProperties"`
		Additional    struct {
			Type      string `json:"type"`
			MinLength int    `json:"minLength"`
			MaxLength int    `json:"maxLength"`
			MaxBytes  int    `json:"x-kuberploy-max-utf8-bytes"`
		} `json:"additionalProperties"`
		MaxTotal int `json:"x-kuberploy-max-total-utf8-bytes"`
	}
	if err := json.Unmarshal(document.Components.Schemas["RuntimeSecretValues"], &values); err != nil {
		t.Fatal(err)
	}
	if values.Type != "object" || !values.WriteOnly || values.MinProperties != 1 || values.MaxProperties != 64 ||
		values.Additional.Type != "string" || values.Additional.MinLength != 1 || values.Additional.MaxBytes != 64<<10 || values.MaxTotal != 256<<10 {
		t.Fatalf("write-only value bounds drifted: %#v", values)
	}

	var reference struct {
		Type       string   `json:"type"`
		Required   []string `json:"required"`
		Additional bool     `json:"additionalProperties"`
		Properties map[string]struct {
			Type      string `json:"type"`
			Format    string `json:"format"`
			Minimum   int64  `json:"minimum"`
			MinLength int    `json:"minLength"`
			MaxLength int    `json:"maxLength"`
			Pattern   string `json:"pattern"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(document.Components.Schemas["SecretBindingRef"], &reference); err != nil {
		t.Fatal(err)
	}
	sort.Strings(reference.Required)
	if reference.Type != "object" || reference.Additional || strings.Join(reference.Required, ",") != "bindingId,key,name,version" ||
		len(reference.Properties) != 4 || reference.Properties["bindingId"].Type != "string" || reference.Properties["bindingId"].Format != "uuid" ||
		reference.Properties["name"].Type != "string" || reference.Properties["name"].MinLength != 1 || reference.Properties["name"].MaxLength != 63 || reference.Properties["name"].Pattern == "" ||
		reference.Properties["key"].Type != "string" || reference.Properties["key"].MinLength != 1 || reference.Properties["key"].MaxLength != 253 || reference.Properties["key"].Pattern == "" ||
		reference.Properties["version"].Type != "integer" || reference.Properties["version"].Format != "int64" || reference.Properties["version"].Minimum != 1 {
		t.Fatalf("immutable secret binding reference drifted: %#v", reference)
	}

	for _, schemaName := range []string{"CreateRuntimeSecretBinding", "RotateRuntimeSecretBinding"} {
		var schema struct {
			Properties map[string]struct {
				WriteOnly bool   `json:"writeOnly"`
				Ref       string `json:"$ref"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(document.Components.Schemas[schemaName], &schema); err != nil {
			t.Fatal(err)
		}
		if !schema.Properties["values"].WriteOnly || schema.Properties["values"].Ref != "#/components/schemas/RuntimeSecretValues" {
			t.Fatalf("%s values is not an explicit write-only reference: %#v", schemaName, schema.Properties["values"])
		}
		for _, forbidden := range []string{"organizationId", "projectId", "namespace", "targetSecretName", "fingerprint", "ciphertext"} {
			if _, accepted := schema.Properties[forbidden]; accepted {
				t.Fatalf("%s accepts server/provider field %q", schemaName, forbidden)
			}
		}
	}

	allowedResponseFields := map[string][]string{
		"RuntimeSecretBindingMetadata": {"activeVersion", "applicationId", "createdAt", "createdBy", "deleteStartedAt", "deletedAt", "environmentId", "id", "name", "provider", "state", "updatedAt"},
		"RuntimeSecretVersionMetadata": {"activatedAt", "createdAt", "deliveries", "failureCode", "id", "number", "readinessObservedAt", "retainedAt", "stagedAt", "state", "updatedAt"},
		"RuntimeSecretBindingDetail":   {"activeVersion", "applicationId", "createdAt", "createdBy", "deleteStartedAt", "deletedAt", "environmentId", "id", "name", "provider", "state", "updatedAt", "versions"},
	}
	for schemaName, want := range allowedResponseFields {
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(document.Components.Schemas[schemaName], &schema); err != nil {
			t.Fatal(err)
		}
		got := make([]string, 0, len(schema.Properties))
		for field := range schema.Properties {
			got = append(got, field)
		}
		sort.Strings(got)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s response fields=%#v, want %#v", schemaName, got, want)
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
	for _, operation := range profile.Operations {
		if _, secretOperation := secretOperationIDs[operation.OperationID]; secretOperation {
			t.Fatalf("runtime-secret operation leaked into agent profile: %#v", operation)
		}
	}
}
