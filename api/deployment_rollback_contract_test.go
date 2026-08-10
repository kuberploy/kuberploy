package api

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

func TestDeploymentRollbackContractIsClosedGitOnlyAndTruthful(t *testing.T) {
	var document struct {
		Paths map[string]map[string]struct {
			OperationID string   `json:"operationId"`
			Summary     string   `json:"summary"`
			Description string   `json:"description"`
			Permission  string   `json:"x-kuberploy-permission"`
			Effect      string   `json:"x-kuberploy-effect"`
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
	list := document.Paths["/v1/deployments/{id}/rollback-sources"]["get"]
	rollback := document.Paths["/v1/deployments/{id}/rollback"]["post"]
	if list.OperationID != "listDeploymentRollbackSources" || list.Permission != "resources.read" || list.Automation != "app.read" ||
		rollback.OperationID != "rollbackDeployment" || rollback.Permission != "resources.write" || rollback.Effect != "git-write" || rollback.Automation != "app.edit" ||
		!contains(list.Audience, "agent") || !contains(rollback.Audience, "agent") {
		t.Fatalf("rollback safety metadata drifted: list=%#v rollback=%#v", list, rollback)
	}
	for _, phrase := range []string{"sourceOperationId", "never rewritten", "no imperative Kubernetes or Argo rollback"} {
		if !strings.Contains(rollback.Description, phrase) {
			t.Fatalf("rollback description omits %q: %s", phrase, rollback.Description)
		}
	}
	var input struct {
		AdditionalProperties bool                       `json:"additionalProperties"`
		Properties           map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(document.Components.Schemas["DeploymentRollbackInput"], &input); err != nil {
		t.Fatal(err)
	}
	fields := make([]string, 0, len(input.Properties))
	for field := range input.Properties {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	if input.AdditionalProperties || strings.Join(fields, ",") != "sourceOperationId" {
		t.Fatalf("rollback caller authority expanded: fields=%v additional=%v", fields, input.AdditionalProperties)
	}
	var candidate struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(document.Components.Schemas["DeploymentRollbackCandidate"], &candidate); err != nil {
		t.Fatal(err)
	}
	if _, ok := candidate.Properties["managedReleaseVerified"]; !ok {
		t.Fatal("safe rollback history omits managed release verification truth")
	}
	for _, forbidden := range []string{"runtime", "environment", "route", "config", "rawYaml", "registryPull", "credentials", "pullRequest"} {
		if _, exposed := candidate.Properties[forbidden]; exposed {
			t.Fatalf("rollback history exposes %q", forbidden)
		}
	}
}
