package api

import (
	"encoding/json"
	"testing"
)

func TestSchedulingProfileContractSeparatesAdminAuthorityFromWorkloadSelection(t *testing.T) {
	var document struct {
		Paths map[string]map[string]struct {
			OperationID string   `json:"operationId"`
			Permission  string   `json:"x-kuberploy-permission"`
			Automation  string   `json:"x-kuberploy-automation-scope"`
			Audience    []string `json:"x-kuberploy-audience"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(OpenAPIJSON, &document); err != nil {
		t.Fatal(err)
	}
	assigned := document.Paths["/v1/environments/{id}/scheduling-profiles"]["get"]
	if assigned.OperationID != "listAssignedSchedulingProfiles" || assigned.Automation != "app.read" || !contains(assigned.Audience, "agent") {
		t.Fatalf("assigned catalog boundary drifted: %#v", assigned)
	}
	for path, method := range map[string]string{
		"/v1/platform/scheduling-profiles":                 "post",
		"/v1/platform/scheduling-profiles/{id}":            "put",
		"/v1/platform/scheduling-profiles/{id}/deactivate": "post",
	} {
		operation := document.Paths[path][method]
		if operation.Permission != "platform.admin" || contains(operation.Audience, "agent") || operation.Automation != "" {
			t.Fatalf("admin boundary drifted for %s %s: %#v", method, path, operation)
		}
	}
	for _, name := range []string{"SchedulingProfileRef", "SchedulingProfilePod", "SchedulingProfileAssignment", "CreateSchedulingProfile", "ReviseSchedulingProfile", "DeactivateSchedulingProfile"} {
		var schema struct {
			Additional bool `json:"additionalProperties"`
		}
		if err := json.Unmarshal(document.Components.Schemas[name], &schema); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if schema.Additional {
			t.Fatalf("%s permits unknown authority fields", name)
		}
	}
	var assignedView struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(document.Components.Schemas["AssignedSchedulingProfile"], &assignedView); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"assignments", "createdBy", "deactivatedBy", "lifecycle"} {
		if _, exposed := assignedView.Properties[forbidden]; exposed {
			t.Fatalf("tenant assigned catalog exposes %q", forbidden)
		}
	}
	var runtime struct {
		Properties map[string]struct {
			ReadOnly bool `json:"readOnly"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(document.Components.Schemas["WorkloadRuntime"], &runtime); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"nodeSelector", "affinity", "topologySpreadConstraints", "tolerations", "priorityClassName"} {
		if !runtime.Properties[field].ReadOnly {
			t.Fatalf("effective scheduling field %q is not read-only", field)
		}
	}
}
