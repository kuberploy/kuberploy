package api

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

func TestPlatformArgoGitBindingContractIsHumanOnlyAndAuthorityClosed(t *testing.T) {
	var document struct {
		Paths map[string]map[string]struct {
			OperationID  string                `json:"operationId"`
			Permission   string                `json:"x-kuberploy-permission"`
			Effect       string                `json:"x-kuberploy-effect"`
			Risk         string                `json:"x-kuberploy-risk"`
			Idempotency  string                `json:"x-kuberploy-idempotency"`
			Confirmation string                `json:"x-kuberploy-confirmation"`
			Audience     []string              `json:"x-kuberploy-audience"`
			Security     []map[string][]string `json:"security"`
			Parameters   []struct {
				Ref string `json:"$ref"`
			} `json:"parameters"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(OpenAPIJSON, &document); err != nil {
		t.Fatal(err)
	}

	path := document.Paths["/v1/platform/argo/git-binding"]
	for method, operation := range path {
		if operation.Permission != "platform.admin" || len(operation.Security) != 1 || len(operation.Security[0]) != 1 || operation.Security[0]["cookieAuth"] == nil || !equalStrings(operation.Audience, []string{"human"}) {
			t.Fatalf("%s operation is not exact platform-admin cookie-only human: %#v", method, operation)
		}
	}
	if get := path["get"]; get.OperationID != "getPlatformArgoGitBinding" || get.Effect != "sensitive-metadata-read" || get.Risk != "medium" || get.Idempotency != "safe" || get.Confirmation != "none" {
		t.Fatalf("read safety metadata drifted: %#v", get)
	}
	post := path["post"]
	if post.OperationID != "createPlatformArgoGitBinding" || post.Effect != "git-authority" || post.Risk != "high" || post.Idempotency != "required" || post.Confirmation != "request-body" {
		t.Fatalf("mutation safety metadata drifted: %#v", post)
	}
	refs := make([]string, 0, len(post.Parameters))
	for _, parameter := range post.Parameters {
		refs = append(refs, parameter.Ref)
	}
	sort.Strings(refs)
	if strings.Join(refs, ",") != "#/components/parameters/CommandIdempotencyKey,#/components/parameters/SessionCSRFToken" {
		t.Fatalf("mutation guards=%#v", refs)
	}

	input := decodeSchemaProperties(t, document.Components.Schemas["CreatePlatformArgoGitBinding"])
	actualInput := make([]string, 0, len(input))
	for name := range input {
		actualInput = append(actualInput, name)
	}
	sort.Strings(actualInput)
	if strings.Join(actualInput, ",") != "installationId,repositoryId,targetRef" {
		t.Fatalf("caller can select authority fields: %#v", actualInput)
	}
	output := decodeSchemaProperties(t, document.Components.Schemas["PlatformArgoGitBinding"])
	actualOutput := make([]string, 0, len(output))
	for name := range output {
		actualOutput = append(actualOutput, name)
	}
	sort.Strings(actualOutput)
	if strings.Join(actualOutput, ",") != "clusterId,createdAt,id,pathPrefix,repository,state,targetHeadObservedAt,targetHeadRevision,targetRef,updatedAt" {
		t.Fatalf("safe response fields drifted: %#v", actualOutput)
	}
	for _, forbidden := range []string{"remote", "cloneUrl", "githubAppId", "credentialMode", "credentialSecret", "secretName", "token", "privateKey"} {
		if _, leaked := output[forbidden]; leaked {
			t.Fatalf("response exposes %q", forbidden)
		}
	}

	profile, err := BuildAgentProfile(OpenAPIJSON)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"getPlatformArgoGitBinding", "createPlatformArgoGitBinding"} {
		if strings.Contains(string(profile), forbidden) {
			t.Fatalf("human-only platform authority leaked into agent profile: %s", forbidden)
		}
	}
}
