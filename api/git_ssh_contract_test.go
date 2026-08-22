package api

import (
	"encoding/json"
	"testing"
)

func TestGitSSHOpenAPIExposesOnlyPublicHumanKeyLifecycle(t *testing.T) {
	var document struct {
		Paths map[string]map[string]struct {
			OperationID  string         `json:"operationId"`
			Permission   string         `json:"x-kuberploy-permission"`
			Idempotency  string         `json:"x-kuberploy-idempotency"`
			Confirmation string         `json:"x-kuberploy-confirmation"`
			Audience     []string       `json:"x-kuberploy-audience"`
			RequestBody  map[string]any `json:"requestBody"`
			Responses    map[string]struct {
				Content map[string]struct {
					Schema map[string]any `json:"schema"`
				} `json:"content"`
			} `json:"responses"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]struct {
				Required   []string                  `json:"required"`
				Properties map[string]map[string]any `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(OpenAPIJSON, &document); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []struct {
		base, prefix string
	}{
		{"/v1/projects/{id}/git-ssh-keys", "Project"},
		{"/v1/applications/{id}/git-ssh-keys", "Application"},
	} {
		list := document.Paths[scope.base]["get"]
		create := document.Paths[scope.base]["post"]
		rotate := document.Paths[scope.base+"/rotate"]["post"]
		revoke := document.Paths[scope.base+"/active"]["delete"]
		if list.OperationID != "list"+scope.prefix+"GitSSHKeys" || list.Permission != "resources.read" ||
			list.Responses["200"].Content["application/json"].Schema["$ref"] != "#/components/schemas/GitSSHKeyList" {
			t.Fatalf("%s list=%#v", scope.prefix, list)
		}
		for name, operation := range map[string]struct {
			value        string
			confirmation string
		}{
			"create": {create.OperationID, create.Confirmation},
			"rotate": {rotate.OperationID, rotate.Confirmation},
			"revoke": {revoke.OperationID, revoke.Confirmation},
		} {
			expectedID := name + scope.prefix + "GitSSHKey"
			expectedConfirmation := "dialog"
			if name == "create" {
				expectedConfirmation = "none"
			}
			if operation.value != expectedID || operation.confirmation != expectedConfirmation {
				t.Fatalf("%s %s=%#v", scope.prefix, name, operation)
			}
		}
		for _, operation := range []struct {
			Permission, Idempotency string
			Audience                []string
			RequestBody             map[string]any
		}{
			{create.Permission, create.Idempotency, create.Audience, create.RequestBody},
			{rotate.Permission, rotate.Idempotency, rotate.Audience, rotate.RequestBody},
			{revoke.Permission, revoke.Idempotency, revoke.Audience, revoke.RequestBody},
		} {
			if operation.Permission != "resources.write" || operation.Idempotency != "required" ||
				len(operation.Audience) != 1 || operation.Audience[0] != "human" || operation.RequestBody != nil {
				t.Fatalf("unsafe %s mutation metadata=%#v", scope.prefix, operation)
			}
		}
	}

	schema := document.Components.Schemas["GitSSHKey"]
	for _, field := range []string{"scope", "ownerId", "revision", "status", "publicKey", "fingerprint"} {
		if schema.Properties[field] == nil {
			t.Fatalf("public key schema omits %s", field)
		}
	}
	for _, forbidden := range []string{"privateKey", "privateKeyCiphertext", "ciphertext", "encryptionKey", "encryptionKeyVersion", "knownHosts"} {
		if schema.Properties[forbidden] != nil {
			t.Fatalf("public key schema exposes %s", forbidden)
		}
	}
}
