package api

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

func TestCertificateManagementContractIsHumanOnlyWriteOnlyAndClosed(t *testing.T) {
	type parameter struct {
		Ref string `json:"$ref"`
	}
	type operation struct {
		OperationID string                `json:"operationId"`
		Tags        []string              `json:"tags"`
		Security    []map[string][]string `json:"security"`
		Audience    []string              `json:"x-kuberploy-audience"`
		Permission  string                `json:"x-kuberploy-permission"`
		Automation  string                `json:"x-kuberploy-automation-scope"`
		Parameters  []parameter           `json:"parameters"`
	}
	var document struct {
		Paths      map[string]map[string]operation `json:"paths"`
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
	}{
		"/v1/applications/{id}/certificate-bindings#get":  {"get", "listCertificateBindings", "certificates.read", false},
		"/v1/applications/{id}/certificate-bindings#post": {"post", "createCertificateBinding", "certificates.create", true},
		"/v1/certificate-bindings/{id}#get":               {"get", "getCertificateBinding", "certificates.read", false},
		"/v1/certificate-bindings/{id}#delete":            {"delete", "deleteCertificateBinding", "certificates.delete", true},
		"/v1/certificate-bindings/{id}/versions#post":     {"post", "rotateCertificateBinding", "certificates.rotate", true},
	}
	operationIDs := map[string]struct{}{}
	for key, want := range expected {
		path := strings.Split(key, "#")[0]
		actual, ok := document.Paths[path][want.method]
		if !ok || actual.OperationID != want.operationID || actual.Permission != want.permission || actual.Automation != "none" || len(actual.Tags) != 1 || actual.Tags[0] != "TLS Certificates" {
			t.Fatalf("operation %s: %#v", key, actual)
		}
		operationIDs[actual.OperationID] = struct{}{}
		if len(actual.Security) != 1 || len(actual.Security[0]) != 1 || actual.Security[0]["cookieAuth"] == nil || contains(actual.Audience, "agent") || !contains(actual.Audience, "human") {
			t.Fatalf("certificate operation is not exact human cookie-only: %#v", actual)
		}
		refs := map[string]bool{}
		for _, item := range actual.Parameters {
			refs[item.Ref] = true
		}
		if want.mutation && (!refs["#/components/parameters/SecretIdempotencyKey"] || !refs["#/components/parameters/SessionCSRFToken"]) {
			t.Fatalf("mutation %q lacks strict idempotency/CSRF: %#v", actual.OperationID, refs)
		}
	}

	for _, schemaName := range []string{"CreateCertificateBinding", "RotateCertificateBinding"} {
		var schema struct {
			Additional bool     `json:"additionalProperties"`
			Required   []string `json:"required"`
			Properties map[string]struct {
				Type      string `json:"type"`
				Format    string `json:"format"`
				WriteOnly bool   `json:"writeOnly"`
				MinLength int    `json:"minLength"`
				MaxLength int    `json:"maxLength"`
				MaxBytes  int    `json:"x-kuberploy-max-utf8-bytes"`
				Minimum   int64  `json:"minimum"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(document.Components.Schemas[schemaName], &schema); err != nil {
			t.Fatal(err)
		}
		if schema.Additional || !schema.Properties["certificatePem"].WriteOnly || schema.Properties["certificatePem"].MinLength != 1 || schema.Properties["certificatePem"].MaxLength != 65536 || schema.Properties["certificatePem"].MaxBytes != 65536 ||
			!schema.Properties["privateKeyPem"].WriteOnly || schema.Properties["privateKeyPem"].MinLength != 1 || schema.Properties["privateKeyPem"].MaxLength != 32768 || schema.Properties["privateKeyPem"].MaxBytes != 32768 {
			t.Fatalf("%s PEM contract drifted: %#v", schemaName, schema.Properties)
		}
		for _, forbidden := range []string{"organizationId", "projectId", "namespace", "provider", "targetSecretName", "secretName", "secretRef", "artifact", "ciphertext", "fingerprint"} {
			if _, accepted := schema.Properties[forbidden]; accepted {
				t.Fatalf("%s accepts forbidden caller field %q", schemaName, forbidden)
			}
		}
	}

	allowedResponseFields := map[string][]string{
		"CertificateBindingMetadata": {"activeVersion", "applicationId", "createdAt", "createdBy", "deleteStartedAt", "deletedAt", "environmentId", "id", "name", "state", "updatedAt"},
		"CertificateVersionMetadata": {"createdAt", "createdBy", "dnsNames", "ipAddresses", "leafFingerprint", "notAfter", "notBefore", "number", "publicKeyFingerprint"},
		"CertificateBindingDetail":   {"activeVersion", "applicationId", "createdAt", "createdBy", "deleteStartedAt", "deletedAt", "environmentId", "id", "name", "state", "updatedAt", "versions"},
	}
	for schemaName, want := range allowedResponseFields {
		var schema struct {
			Additional bool                       `json:"additionalProperties"`
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
		if schema.Additional || strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s fields=%#v want=%#v additional=%t", schemaName, got, want, schema.Additional)
		}
		for _, forbidden := range []string{"certificatePem", "privateKeyPem", "secretVersionId", "targetSecretName", "provider", "artifact", "ciphertext", "namespace", "organizationId", "projectId"} {
			if _, leaked := schema.Properties[forbidden]; leaked {
				t.Fatalf("%s returns forbidden field %q", schemaName, forbidden)
			}
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
		if _, leaked := operationIDs[operation.OperationID]; leaked {
			t.Fatalf("certificate management leaked into agent profile: %#v", operation)
		}
	}
}
