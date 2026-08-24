package api

import (
	"encoding/json"
	"testing"
)

func TestUserAndTeamDeletionAreHumanOnlyConfirmedCommands(t *testing.T) {
	type operation struct {
		OperationID string                `json:"operationId"`
		Security    []map[string][]string `json:"security"`
		Parameters  []struct {
			Ref string `json:"$ref"`
		} `json:"parameters"`
		RequestBody struct {
			Content map[string]struct {
				Schema struct {
					Ref string `json:"$ref"`
				} `json:"schema"`
			} `json:"content"`
		} `json:"requestBody"`
	}
	var document struct {
		Paths map[string]map[string]operation `json:"paths"`
	}
	if err := json.Unmarshal(OpenAPIJSON, &document); err != nil {
		t.Fatal(err)
	}
	for path, expected := range map[string]struct {
		operationID string
		schema      string
	}{
		"/v1/users/{id}": {"deleteUser", "#/components/schemas/DeleteUser"},
		"/v1/teams/{id}": {"deleteTeam", "#/components/schemas/DeleteTeam"},
	} {
		operation := document.Paths[path]["delete"]
		if operation.OperationID != expected.operationID || len(operation.Security) != 1 || len(operation.Security[0]) != 1 {
			t.Fatalf("%s deletion is not exact human-only operation: %#v", path, operation)
		}
		if _, ok := operation.Security[0]["cookieAuth"]; !ok {
			t.Fatalf("%s deletion allows non-session authentication: %#v", path, operation.Security)
		}
		refs := map[string]bool{}
		for _, parameter := range operation.Parameters {
			refs[parameter.Ref] = true
		}
		if !refs["#/components/parameters/SessionCSRFToken"] || !refs["#/components/parameters/IdempotencyKey"] {
			t.Fatalf("%s deletion omits command protections: %#v", path, refs)
		}
		if got := operation.RequestBody.Content["application/json"].Schema.Ref; got != expected.schema {
			t.Fatalf("%s confirmation schema=%q want=%q", path, got, expected.schema)
		}
	}

	profile, err := BuildAgentProfile(OpenAPIJSON)
	if err != nil {
		t.Fatal(err)
	}
	for _, operationID := range []string{"deleteUser", "deleteTeam"} {
		if json.Valid(profile) && containsJSONOperation(profile, operationID) {
			t.Fatalf("human-only operation %q leaked into agent allowlist", operationID)
		}
	}
}

func containsJSONOperation(profile []byte, operationID string) bool {
	var decoded struct {
		Operations []struct {
			OperationID string `json:"operationId"`
		} `json:"operations"`
	}
	if json.Unmarshal(profile, &decoded) != nil {
		return false
	}
	for _, operation := range decoded.Operations {
		if operation.OperationID == operationID {
			return true
		}
	}
	return false
}
